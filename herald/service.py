from __future__ import annotations

import http.client
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Callable
from urllib.parse import urlsplit

from .feeds import FeedParseError, parse_feed
from .obsidian import ObsidianExporter
from .papers import PaperImporter, PaperImportResult
from .sources import CURATED_SOURCES
from .storage import Database
from .summaries import (
    LocalSummarizer,
    SummaryProvider,
    SummaryResult,
    deterministic_summary,
)


Fetcher = Callable[[str], bytes]


class FeedFetchError(RuntimeError):
    """Raised when a remote feed cannot be retrieved."""


def fetch_feed(url: str, *, timeout: float = 20.0) -> bytes:
    request = urllib.request.Request(
        url,
        headers={
            "User-Agent": "Herald/0.1 (+local research reader)",
            "Accept": (
                "application/atom+xml, application/rss+xml, "
                "application/xml, text/xml"
            ),
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            document = response.read(10 * 1024 * 1024 + 1)
    except (
        urllib.error.URLError,
        http.client.HTTPException,
        TimeoutError,
        OSError,
    ) as error:
        raise FeedFetchError(f"Could not fetch {url}: {error}") from error
    if len(document) > 10 * 1024 * 1024:
        raise FeedFetchError(f"Feed exceeds the 10 MiB limit: {url}")
    return document


@dataclass(frozen=True, slots=True)
class RefreshResult:
    source_id: int
    source_title: str
    fetched: int
    created: int
    updated: int
    error: str | None = None

    def to_dict(self) -> dict[str, int | str | None]:
        return asdict(self)


class HeraldService:
    def __init__(
        self,
        database: Database,
        fetcher: Fetcher | None = None,
        *,
        summarizer: SummaryProvider | None = None,
        exporter: ObsidianExporter | None = None,
        paper_importer: PaperImporter | None = None,
    ):
        self.database = database
        self.fetcher = fetcher or fetch_feed
        self.summarizer = summarizer or LocalSummarizer()
        self.exporter = exporter or ObsidianExporter(database.path.parent / "vault")
        self.paper_importer = paper_importer or PaperImporter(database)

    def seed_curated_sources(self) -> int:
        existing = {source["url"] for source in self.database.list_sources()}
        for source in CURATED_SOURCES:
            self.database.add_source(source.title, source.url, source.category)
        return sum(source.url not in existing for source in CURATED_SOURCES)

    def add_source(self, title: str, url: str, category: str = "Unsorted") -> int:
        if not title.strip():
            raise ValueError("Source title cannot be empty")
        parts = urlsplit(url.strip())
        if parts.scheme not in {"http", "https"} or not parts.netloc:
            raise ValueError("Source URL must be an HTTP or HTTPS URL")
        return self.database.add_source(title, url, category or "Unsorted")

    def list_sources(self, enabled_only: bool = False) -> list[dict[str, object]]:
        return self.database.list_sources(enabled_only=enabled_only)

    def refresh_source(self, source: dict[str, object]) -> RefreshResult:
        source_id = int(source["id"])
        source_title = str(source["title"])
        document = self.fetcher(str(source["url"]))
        entries = parse_feed(document)
        created = 0
        updated = 0
        for entry in entries:
            _, was_created = self.database.upsert_entry(
                source_id=source_id,
                guid=entry.guid,
                url=entry.url,
                title=entry.title,
                author=entry.author,
                published_at=entry.published_at,
                content=entry.content,
                summary=deterministic_summary(entry.title, entry.content),
                summary_provider="extractive",
            )
            created += int(was_created)
            updated += int(not was_created)
        return RefreshResult(
            source_id=source_id,
            source_title=source_title,
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
                results.append(
                    RefreshResult(
                        source_id=int(source["id"]),
                        source_title=str(source["title"]),
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
        return result

    def export_entry(self, entry_id: int) -> Path:
        entry = self.database.get_entry(entry_id)
        if entry is None:
            raise KeyError(f"Entry {entry_id} does not exist")
        destination = self.exporter.export(entry)
        self.database.set_exported_path(
            entry_id, self.exporter.relative_path(destination)
        )
        return destination

    def export_kept(self) -> list[Path]:
        kept = self.database.list_entries(status="kept", limit=500)
        return [self.export_entry(entry["id"]) for entry in kept]

    def import_paper(self, value: str) -> PaperImportResult:
        return self.paper_importer.import_paper(value)
