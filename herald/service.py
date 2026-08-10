from __future__ import annotations

import http.client
import threading
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Callable
from urllib.parse import urlsplit

from .feeds import (
    FeedParseError,
    canonicalize_url,
    discover_feed_url,
    parse_feed,
)
from .obsidian import ObsidianConflictError, ObsidianExporter
from .papers import (
    PaperImporter,
    PaperImportError,
    PaperImportResult,
    extract_keywords,
    normalize_arxiv_id,
    normalize_doi,
)
from .relevance import RelevanceCoordinator, RelevanceEngine
from .sources import (
    create_source_manifest,
    load_source_catalog,
    validate_source_manifest,
)
from .storage import Database
from .summaries import (
    LocalSummarizer,
    SummaryProvider,
    SummaryResult,
    deterministic_summary,
)


class FeedFetchError(RuntimeError):
    """Raised when a remote feed cannot be retrieved."""


@dataclass(frozen=True, slots=True)
class FeedResponse:
    document: bytes
    url: str
    etag: str = ""
    last_modified: str = ""
    not_modified: bool = False


Fetcher = Callable[..., bytes | FeedResponse]


def fetch_feed(
    url: str,
    *,
    timeout: float = 20.0,
    etag: str = "",
    last_modified: str = "",
) -> FeedResponse:
    headers = {
        "User-Agent": "Herald/0.1 (+local research reader)",
        "Accept": (
            "application/atom+xml, application/rss+xml, "
            "application/xml, text/xml, text/html"
        ),
    }
    if etag:
        headers["If-None-Match"] = etag
    if last_modified:
        headers["If-Modified-Since"] = last_modified
    request = urllib.request.Request(
        url,
        headers=headers,
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            document = response.read(10 * 1024 * 1024 + 1)
            response_url = response.geturl()
            response_etag = response.headers.get("ETag", "")
            response_modified = response.headers.get("Last-Modified", "")
    except urllib.error.HTTPError as error:
        if error.code == 304:
            return FeedResponse(
                document=b"",
                url=error.geturl() or url,
                etag=error.headers.get("ETag", etag),
                last_modified=error.headers.get("Last-Modified", last_modified),
                not_modified=True,
            )
        raise FeedFetchError(f"Could not fetch {url}: {error}") from error
    except (
        urllib.error.URLError,
        http.client.HTTPException,
        TimeoutError,
        OSError,
    ) as error:
        raise FeedFetchError(f"Could not fetch {url}: {error}") from error
    if len(document) > 10 * 1024 * 1024:
        raise FeedFetchError(f"Feed exceeds the 10 MiB limit: {url}")
    return FeedResponse(
        document=document,
        url=response_url,
        etag=response_etag,
        last_modified=response_modified,
    )


@dataclass(frozen=True, slots=True)
class RefreshResult:
    source_id: int
    source_title: str
    content_kind: str
    fetched: int
    created: int
    updated: int
    error: str | None = None
    not_modified: bool = False

    def to_dict(self) -> dict[str, bool | int | str | None]:
        return asdict(self)


class HeraldService:
    def __init__(
        self,
        database: Database,
        fetcher: Fetcher | None = None,
        *,
        summarizer: SummaryProvider | None = None,
        exporter: ObsidianExporter | None = None,
        relevance: RelevanceCoordinator | None = None,
        paper_importer: PaperImporter | None = None,
        default_vault_path: str | Path | None = None,
        archive_root: str | Path | None = None,
        auto_enrich_kept: bool = False,
        now: Callable[[], datetime] | None = None,
    ):
        self.database = database
        self._uses_default_fetcher = fetcher is None
        self.fetcher = fetcher or fetch_feed
        self.summarizer = summarizer or LocalSummarizer()
        self.relevance = relevance or RelevanceCoordinator(RelevanceEngine(database))
        self.paper_importer = paper_importer or PaperImporter(database)
        self._exporter_override = exporter
        self.default_vault_path = Path(
            default_vault_path or database.path.parent / "vault"
        ).expanduser().resolve()
        self.archive_root = Path(
            archive_root or database.path.parent / "obsidian-archive"
        ).expanduser().resolve()
        self.auto_enrich_kept = auto_enrich_kept
        self._enrichment_lock = threading.Lock()
        self._enriching_entries: set[int] = set()
        self.now = now or (lambda: datetime.now(UTC))

    @property
    def exporter(self) -> ObsidianExporter:
        if self._exporter_override is not None:
            return self._exporter_override
        configured = self.database.get_setting("obsidian", {})
        configured_path = (
            configured.get("vault_path") if isinstance(configured, dict) else None
        )
        return ObsidianExporter(
            configured_path or self.default_vault_path,
            self.archive_root,
        )

    def seed_curated_sources(self) -> int:
        existing = {source["url"] for source in self.database.list_sources()}
        catalog = load_source_catalog()
        for source in catalog:
            if source.url in existing:
                continue
            self.database.add_source(
                source.title,
                source.url,
                source.category,
                content_kind=source.content_kind,
                enabled=source.enabled,
            )
        return sum(source.url not in existing for source in catalog)

    def backfill_existing(self, *, sync_obsidian: bool = True) -> dict[str, object]:
        """Upgrade existing rows locally without changing any reading status."""
        entries = self.database.list_entries(limit=None)
        identifiers_added = 0
        canonical_keys_added = 0
        keywords_added = 0
        for entry in entries:
            entry_id = int(entry["id"])
            content_kind = str(entry.get("content_kind") or "paper")
            if not self.database.list_entry_keywords(entry_id):
                keywords = extract_keywords(
                    str(entry.get("title") or ""), str(entry.get("content") or "")
                )
                self.database.replace_entry_keywords(entry_id, keywords)
                keywords_added += len(keywords)
            if content_kind != "paper":
                continue
            scheme = ""
            value = ""
            url = str(entry.get("canonical_url") or entry.get("url") or "")
            parts = urlsplit(url)
            try:
                if parts.hostname in {"arxiv.org", "www.arxiv.org", "export.arxiv.org"}:
                    path_value = parts.path
                    for prefix in ("/abs/", "/pdf/"):
                        if path_value.startswith(prefix):
                            path_value = path_value[len(prefix):]
                            break
                    value = normalize_arxiv_id(path_value)
                    scheme = "arxiv"
                elif parts.hostname in {"doi.org", "dx.doi.org"}:
                    value = normalize_doi(parts.path.lstrip("/"))
                    scheme = "doi"
            except ValueError:
                scheme = ""
                value = ""
            if not scheme:
                continue
            canonical_key = f"{scheme}:{value}"
            if not entry.get("canonical_key"):
                self.database.update_entry_metadata(
                    entry_id,
                    title=str(entry.get("title") or ""),
                    canonical_key=canonical_key,
                )
                canonical_keys_added += 1
            known = {
                (str(item["scheme"]), str(item["value"]))
                for item in self.database.list_paper_identifiers(entry_id)
            }
            if (scheme, value) not in known:
                self.database.add_paper_identifier(
                    entry_id, scheme, value, is_primary=True
                )
                identifiers_added += 1

        references_linked = self.database.reconcile_paper_references()
        rankings = {
            kind: self.relevance.engine.rescore(kind) for kind in ("paper", "news")
        }
        if sync_obsidian:
            self.reconcile_obsidian()
        return {
            "entries": len(entries),
            "identifiers_added": identifiers_added,
            "canonical_keys_added": canonical_keys_added,
            "keywords_added": keywords_added,
            "references_linked": references_linked,
            "rankings": rankings,
            "obsidian_reconciled": sync_obsidian,
        }

    def add_source(
        self,
        title: str,
        url: str,
        category: str = "Unsorted",
        *,
        content_kind: str = "paper",
    ) -> int:
        if not title.strip():
            raise ValueError("Source title cannot be empty")
        parts = urlsplit(url.strip())
        if parts.scheme not in {"http", "https"} or not parts.netloc:
            raise ValueError("Source URL must be an HTTP or HTTPS URL")
        return self.database.add_source(
            title,
            canonicalize_url(url),
            category or "Unsorted",
            content_kind=content_kind,
        )

    def list_sources(self, enabled_only: bool = False) -> list[dict[str, object]]:
        return self.database.list_sources(enabled_only=enabled_only)

    def export_sources(self) -> dict[str, object]:
        """Return portable configuration, deliberately excluding local source state."""
        return create_source_manifest(self.database.list_sources())

    def import_sources(self, payload: object) -> dict[str, int]:
        """Validate and idempotently merge portable source configuration."""
        sources = validate_source_manifest(payload)
        return self.database.import_source_configs(
            [source.to_dict() for source in sources]
        )

    def _fetch_source_url(
        self,
        url: str,
        *,
        etag: str = "",
        last_modified: str = "",
    ) -> FeedResponse:
        if self._uses_default_fetcher:
            result = self.fetcher(url, etag=etag, last_modified=last_modified)
            if isinstance(result, FeedResponse):
                return result
            return FeedResponse(document=result, url=url)
        result = self.fetcher(url)
        if isinstance(result, FeedResponse):
            return result
        return FeedResponse(document=result, url=url)

    @staticmethod
    def _parse_timestamp(value: str | None) -> datetime | None:
        if not value:
            return None
        try:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            return None
        if parsed.tzinfo is None:
            parsed = parsed.replace(tzinfo=UTC)
        return parsed.astimezone(UTC)

    def _include_feed_entry(
        self,
        source: dict[str, object],
        guid: str,
        published_at: str | None,
        bootstrap_skips: set[str],
    ) -> bool:
        if str(source.get("content_kind") or "paper") != "news":
            return True
        if source.get("refresh_succeeded_at"):
            return guid not in bootstrap_skips
        published = self._parse_timestamp(published_at)
        if published is None:
            return False
        return published >= self.now().astimezone(UTC) - timedelta(days=30)

    def refresh_source(self, source: dict[str, object]) -> RefreshResult:
        source_id = int(source["id"])
        current_source = self.database.get_source(source_id)
        if current_source is None:
            raise KeyError(f"Source {source_id} does not exist")
        source = current_source
        source_title = str(source["title"])
        fetch_url = str(source.get("resolved_url") or source["url"])
        try:
            response = self._fetch_source_url(
                fetch_url,
                etag=str(source.get("etag") or ""),
                last_modified=str(source.get("last_modified") or ""),
            )
            if response.not_modified:
                self.database.update_source_refresh_state(
                    source_id,
                    succeeded=True,
                    etag=response.etag or None,
                    last_modified=response.last_modified or None,
                )
                return RefreshResult(
                    source_id=source_id,
                    source_title=source_title,
                    content_kind=str(source.get("content_kind") or "paper"),
                    fetched=0,
                    created=0,
                    updated=0,
                    not_modified=True,
                )
            resolved_url = str(source.get("resolved_url") or "")
            if not resolved_url and canonicalize_url(response.url) != canonicalize_url(
                str(source["url"])
            ):
                resolved_url = canonicalize_url(response.url)
            try:
                entries = parse_feed(response.document)
            except FeedParseError:
                if resolved_url:
                    raise
                discovered = discover_feed_url(response.document, response.url)
                if discovered is None:
                    raise
                feed_response = self._fetch_source_url(discovered)
                if feed_response.not_modified:
                    entries = []
                else:
                    entries = parse_feed(feed_response.document)
                response = feed_response
                resolved_url = discovered
        except (FeedFetchError, FeedParseError, ValueError) as error:
            self.database.update_source_refresh_state(
                source_id, succeeded=False, error=str(error)
            )
            raise
        created = 0
        updated = 0
        bootstrap_skips = self.database.list_source_bootstrap_skips(source_id)
        new_bootstrap_skips: list[str] = []
        for entry in entries:
            if not self._include_feed_entry(
                source,
                entry.guid,
                entry.published_at,
                bootstrap_skips,
            ):
                if not source.get("refresh_succeeded_at"):
                    new_bootstrap_skips.append(entry.guid)
                continue
            canonical_url = canonicalize_url(entry.url)
            entry_id, was_created = self.database.upsert_entry(
                source_id=source_id,
                guid=entry.guid,
                url=entry.url,
                title=entry.title,
                author=entry.author,
                published_at=entry.published_at,
                content=entry.content,
                summary=deterministic_summary(entry.title, entry.content),
                summary_provider="extractive",
                content_kind=str(source.get("content_kind") or "paper"),
                canonical_url=canonical_url,
            )
            created += int(was_created)
            updated += int(not was_created)
            if str(source.get("content_kind") or "paper") == "news":
                self.database.set_enrichment_state(entry_id, "not_applicable")
            current = self.database.get_entry(entry_id)
            if current is not None and current["status"] == "kept":
                try:
                    self.export_entry(entry_id)
                except (OSError, ValueError, ObsidianConflictError):
                    pass
        self.database.add_source_bootstrap_skips(source_id, new_bootstrap_skips)
        self.database.update_source_refresh_state(
            source_id,
            succeeded=True,
            etag=response.etag,
            last_modified=response.last_modified,
            resolved_url=resolved_url or None,
        )
        content_kind = str(source.get("content_kind") or "paper")
        if created and content_kind == "news":
            self.relevance.start("news")
        return RefreshResult(
            source_id=source_id,
            source_title=source_title,
            content_kind=content_kind,
            fetched=len(entries),
            created=created,
            updated=updated,
        )

    def refresh_all(self) -> list[RefreshResult]:
        results: list[RefreshResult] = []
        for source in self.database.list_sources(enabled_only=True):
            if source.get("adapter") == "manual":
                continue
            try:
                results.append(self.refresh_source(source))
            except (FeedFetchError, FeedParseError, ValueError) as error:
                # refresh_source records source health before propagating; keep the
                # remaining feeds isolated from this failure.
                results.append(
                    RefreshResult(
                        source_id=int(source["id"]),
                        source_title=str(source["title"]),
                        content_kind=str(source.get("content_kind") or "paper"),
                        fetched=0,
                        created=0,
                        updated=0,
                        error=str(error),
                    )
                )
        return results

    def summarize_entry(self, entry_id: int) -> SummaryResult:
        entry = self.database.get_entry(entry_id)
        if entry is None:
            raise KeyError(f"Entry {entry_id} does not exist")
        result = self.summarizer.summarize(entry["title"], entry["content"])
        self.database.set_summary(
            entry_id,
            result.text,
            provider=result.provider,
            model=result.model,
        )
        entry = self.database.get_entry(entry_id)
        if entry is not None and entry["status"] == "kept":
            try:
                self.export_entry(entry_id)
            except (OSError, ValueError, ObsidianConflictError):
                pass
        return result

    def _entry_for_export(self, entry_id: int) -> dict[str, object]:
        entry = self.database.get_entry(entry_id)
        if entry is None:
            raise KeyError(f"Entry {entry_id} does not exist")
        entry["keywords"] = self.database.list_entry_keywords(entry_id)
        profile = self.database.get_relevance_profile(str(entry["content_kind"]))
        if profile is not None:
            entry["relevance"] = self.database.get_entry_ranking(
                entry_id, int(profile["id"])
            )
        if entry["content_kind"] == "paper":
            entry["identifiers"] = self.database.list_paper_identifiers(entry_id)
            entry["references"] = self.database.list_paper_references(entry_id)
        export = self.database.get_obsidian_export(entry_id)
        if export is not None and export["relative_path"]:
            entry["obsidian_relative_path"] = export["relative_path"]
        return entry

    def entry_with_obsidian_state(self, entry_id: int) -> dict[str, object] | None:
        entry = self.database.get_entry(entry_id)
        if entry is None:
            return None
        entry["obsidian_export"] = self.database.get_obsidian_export(entry_id)
        return entry

    def get_obsidian_settings(self) -> dict[str, object]:
        exporter = self.exporter
        return {
            "vault_path": str(exporter.vault_path),
            "default_vault_path": str(self.default_vault_path),
            "archive_path": str(exporter.archive_root),
            "configured": self.database.get_setting("obsidian") is not None,
        }

    def configure_obsidian(self, vault_path: str) -> dict[str, object]:
        requested = Path(vault_path).expanduser()
        if not requested.is_absolute():
            raise ValueError("Obsidian vault path must be absolute")
        requested = requested.resolve()
        if not requested.is_dir():
            raise ValueError("Obsidian vault path must be an existing directory")
        next_exporter = ObsidianExporter(requested, self.archive_root)
        previous_exporter = self.exporter
        if requested == previous_exporter.vault_path:
            return self.get_obsidian_settings()

        # Archive first so annotations can be restored into the new vault. A partial
        # failure leaves the old configuration active and every completed move
        # recoverable through the normal retry path.
        for export in self.database.list_obsidian_exports():
            if export["state"] != "synced" or not export["relative_path"]:
                continue
            entry = self.database.get_entry(int(export["entry_id"]))
            if entry is None:
                continue
            self._archive_entry(entry, exporter=previous_exporter)

        self.database.set_setting("obsidian", {"vault_path": str(requested)})
        self._exporter_override = None
        for entry in self.database.list_entries(status="kept", limit=None):
            try:
                self.export_entry(int(entry["id"]))
            except (OSError, ValueError, ObsidianConflictError):
                # The selected setting remains valid. Individual failures are
                # persisted and can be retried without losing the kept status.
                continue
        return {
            **self.get_obsidian_settings(),
            "vault_path": str(next_exporter.vault_path),
        }

    def export_entry(self, entry_id: int) -> Path:
        return self._export_entry(entry_id, resync_citing=True)

    def _export_entry(self, entry_id: int, *, resync_citing: bool) -> Path:
        entry = self._entry_for_export(entry_id)
        exporter = self.exporter
        try:
            export_state = self.database.get_obsidian_export(entry_id)
            destination_relative = exporter.relative_path(exporter.destination(entry))
            if (
                export_state is not None
                and export_state["state"] == "synced"
                and export_state["relative_path"]
                and export_state["relative_path"] != destination_relative
            ):
                # Notes written by older Herald versions used mutable, title-based
                # paths. Archive and restore them through the same safe lifecycle so
                # annotations survive the one-time move to a stable key.
                try:
                    self._archive_entry(entry, exporter=exporter)
                except ValueError as error:
                    if "escapes managed root" not in str(error):
                        raise
                    # A tampered legacy path is untrusted input, not a reason to
                    # prevent a new safe note from being written.
                export_state = self.database.get_obsidian_export(entry_id)

            archives = self.database.list_obsidian_archives(entry_id)
            should_restore = (
                export_state is not None and export_state["state"] == "archived"
            )
            if should_restore:
                for archive in archives:
                    if archive["restored_at"] is not None:
                        continue
                    try:
                        exporter.restore(entry, str(archive["archive_path"]))
                    except FileNotFoundError:
                        continue
                    self.database.mark_obsidian_archive_restored(int(archive["id"]))
                    break

            result = exporter.export(entry)
        except ObsidianConflictError as error:
            self.database.upsert_obsidian_export(
                entry_id,
                state="conflict",
                vault_path=str(exporter.vault_path),
                error=str(error),
            )
            if resync_citing:
                self._resync_citing_notes(entry_id)
            raise
        except (OSError, ValueError) as error:
            self.database.upsert_obsidian_export(
                entry_id,
                state="failed",
                vault_path=str(exporter.vault_path),
                error=str(error),
            )
            if resync_citing:
                self._resync_citing_notes(entry_id)
            raise
        relative_path = exporter.relative_path(result.path)
        self.database.upsert_obsidian_export(
            entry_id,
            state="synced",
            vault_path=str(exporter.vault_path),
            relative_path=relative_path,
            content_hash=result.content_hash,
        )
        if resync_citing:
            self._resync_citing_notes(entry_id)
        return result.path

    def _resync_citing_notes(self, cited_entry_id: int) -> None:
        """Promote or demote direct citation links after target sync changes."""
        for citing_entry_id in self.database.list_citing_entry_ids(cited_entry_id):
            citing = self.database.get_entry(citing_entry_id)
            export = self.database.get_obsidian_export(citing_entry_id)
            if (
                citing is None
                or citing["status"] != "kept"
                or export is None
                or export["state"] != "synced"
            ):
                continue
            try:
                self._export_entry(citing_entry_id, resync_citing=False)
            except (OSError, ValueError, ObsidianConflictError):
                continue

    def _archive_entry(
        self,
        entry: dict[str, object],
        *,
        exporter: ObsidianExporter | None = None,
    ) -> Path | None:
        entry_id = int(entry["id"])
        export = self.database.get_obsidian_export(entry_id)
        if export is None or export["state"] not in {"synced", "archive_pending"}:
            return None
        relative_path = str(export["relative_path"] or entry.get("exported_path") or "")
        active_exporter = exporter or self.exporter
        self.database.upsert_obsidian_export(
            entry_id,
            state="archive_pending",
            vault_path=str(active_exporter.vault_path),
            relative_path=relative_path,
        )
        try:
            result = active_exporter.archive(entry, relative_path)
        except (OSError, ValueError, ObsidianConflictError) as error:
            self.database.upsert_obsidian_export(
                entry_id,
                state="archive_pending",
                vault_path=str(active_exporter.vault_path),
                relative_path=relative_path,
                error=str(error),
            )
            self._resync_citing_notes(entry_id)
            raise
        if result is not None:
            archive_path = active_exporter.archive_relative_path(result.archive_path)
            self.database.add_obsidian_archive(
                entry_id,
                original_relative_path=relative_path,
                archive_path=archive_path,
            )
            destination: Path | None = result.archive_path
        else:
            destination = None
        self.database.upsert_obsidian_export(
            entry_id,
            state="archived",
            vault_path=str(active_exporter.vault_path),
            relative_path=relative_path,
        )
        self._resync_citing_notes(entry_id)
        return destination

    def change_status(self, entry_id: int, status: str) -> dict[str, object]:
        previous = self.database.get_entry(entry_id)
        if previous is None:
            raise KeyError(f"Entry {entry_id} does not exist")
        if not self.database.set_status(entry_id, status):
            raise KeyError(f"Entry {entry_id} does not exist")
        profile = self.database.get_relevance_profile(str(previous["content_kind"]))
        if profile is not None:
            profile_id = int(profile["id"])
            if status in {"kept", "discarded"}:
                self.database.record_relevance_feedback(
                    entry_id,
                    profile_id,
                    "keep" if status == "kept" else "discard",
                )
            else:
                self.database.clear_relevance_feedback(entry_id, profile_id)
            self.relevance.start(str(previous["content_kind"]))
        updated = self.database.get_entry(entry_id)
        assert updated is not None
        if status == "kept":
            try:
                self.export_entry(entry_id)
            except (OSError, ValueError, ObsidianConflictError):
                pass
            self._start_kept_enrichment(updated)
        elif previous["status"] == "kept":
            try:
                self._archive_entry(updated)
            except (OSError, ValueError, ObsidianConflictError):
                pass
        result = self.entry_with_obsidian_state(entry_id)
        assert result is not None
        return result

    def retry_obsidian(self, entry_id: int) -> Path | None:
        entry = self.database.get_entry(entry_id)
        if entry is None:
            raise KeyError(f"Entry {entry_id} does not exist")
        if entry["status"] == "kept":
            return self.export_entry(entry_id)
        return self._archive_entry(entry)

    def reconcile_obsidian(self) -> None:
        for entry in self.database.list_entries(limit=None):
            export = self.database.get_obsidian_export(int(entry["id"]))
            if entry["status"] == "kept":
                exporter = self.exporter
                export_entry = self._entry_for_export(int(entry["id"]))
                expected = exporter.relative_path(exporter.destination(export_entry))
                recorded_path = str(export["relative_path"] or "") if export else ""
                recorded_note = exporter.vault_path / recorded_path
                needs_sync = export is None or export["state"] in {
                    "pending",
                    "failed",
                    "archived",
                    "archive_pending",
                }
                needs_sync = needs_sync or (
                    export is not None
                    and export["state"] == "synced"
                    and (
                        str(export["vault_path"] or "") != str(exporter.vault_path)
                        or recorded_path != expected
                        or not recorded_note.is_file()
                    )
                )
                if needs_sync:
                    try:
                        self.export_entry(int(entry["id"]))
                    except (OSError, ValueError, ObsidianConflictError):
                        continue
                self._start_kept_enrichment(entry)
            elif export is not None and export["state"] in {"synced", "archive_pending"}:
                try:
                    self._archive_entry(entry)
                except (OSError, ValueError, ObsidianConflictError):
                    continue

    def _start_kept_enrichment(self, entry: dict[str, object]) -> None:
        if not self.auto_enrich_kept or entry.get("content_kind") != "paper":
            return
        if entry.get("enrichment_status") == "enriched":
            return
        url = str(entry.get("canonical_url") or entry.get("url") or "")
        host = (urlsplit(url).hostname or "").lower()
        if host not in {
            "arxiv.org",
            "www.arxiv.org",
            "export.arxiv.org",
            "doi.org",
            "dx.doi.org",
            "semanticscholar.org",
            "www.semanticscholar.org",
        }:
            return
        entry_id = int(entry["id"])
        with self._enrichment_lock:
            if entry_id in self._enriching_entries:
                return
            self._enriching_entries.add(entry_id)

        def enrich() -> None:
            try:
                try:
                    self.paper_importer.import_paper(url)
                except PaperImportError as error:
                    self.database.set_enrichment_state(
                        entry_id, "failed", error=str(error)
                    )
                    return
                current = self.database.get_entry(entry_id)
                if current is None or current["status"] != "kept":
                    return
                try:
                    self.export_entry(entry_id)
                except (OSError, ValueError, ObsidianConflictError):
                    pass
            finally:
                with self._enrichment_lock:
                    self._enriching_entries.discard(entry_id)

        threading.Thread(
            target=enrich,
            name=f"herald-enrich-{entry_id}",
            daemon=True,
        ).start()

    def export_kept(self) -> list[Path]:
        kept = self.database.list_entries(status="kept", limit=None)
        return [self.export_entry(entry["id"]) for entry in kept]

    def import_paper(self, value: str) -> PaperImportResult:
        return self.paper_importer.import_paper(value)

    def list_paper_references(self, entry_id: int) -> list[dict[str, object]]:
        entry = self.database.get_entry(entry_id)
        if entry is None:
            raise KeyError(f"Entry {entry_id} does not exist")
        if entry["content_kind"] != "paper":
            raise ValueError("Only papers have citation references")
        return self.database.list_paper_references(entry_id)

    def add_paper_reference(self, reference_id: int) -> dict[str, object]:
        """Import exactly one user-selected reference without changing triage."""
        reference = self.database.get_paper_reference(reference_id)
        if reference is None:
            raise KeyError(f"Reference {reference_id} does not exist")

        cited_entry_id = reference.get("cited_entry_id")
        if cited_entry_id is not None:
            entry = self.database.get_entry(int(cited_entry_id))
            assert entry is not None
            return {
                "reference": reference,
                "import": {
                    "entry": entry,
                    "created": False,
                    "identifiers": self.database.list_paper_identifiers(int(cited_entry_id)),
                    "keywords": self.database.list_entry_keywords(int(cited_entry_id)),
                    "references": self.database.list_paper_references(int(cited_entry_id)),
                },
            }

        scheme = str(reference.get("external_scheme") or "")
        identifier = str(reference.get("external_id") or "")
        if scheme == "doi":
            locator = f"doi:{identifier}"
        elif scheme == "arxiv":
            locator = f"arXiv:{identifier}"
        elif scheme == "s2":
            locator = f"https://www.semanticscholar.org/paper/{identifier}"
        else:
            locator = str(reference.get("cited_url") or "")
        if not locator:
            raise PaperImportError(
                "This reference has no DOI, arXiv ID, Semantic Scholar ID, or paper URL"
            )

        result = self.import_paper(locator)
        # The selected edge is authoritative even when its provider URL redirects
        # to a different canonical identifier during import.
        self.database.upsert_paper_reference(
            int(reference["citing_entry_id"]),
            str(reference["reference_key"]),
            cited_entry_id=int(result.entry["id"]),
            external_scheme=scheme,
            external_id=identifier,
            cited_title=str(reference.get("cited_title") or ""),
            cited_url=str(reference.get("cited_url") or ""),
            position=reference.get("position"),
            provider=str(reference.get("provider") or ""),
        )
        updated_reference = self.database.get_paper_reference(reference_id)
        assert updated_reference is not None
        return {"reference": updated_reference, "import": result.to_dict()}
