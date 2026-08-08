from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import tempfile
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


FRONTMATTER_START = "# herald:managed:start"
FRONTMATTER_END = "# herald:managed:end"
BODY_START = "<!-- herald:managed:start -->"
BODY_END = "<!-- herald:managed:end -->"


class ObsidianConflictError(RuntimeError):
    """Raised when Herald cannot safely update a note it does not own."""


@dataclass(frozen=True, slots=True)
class ExportResult:
    path: Path
    content_hash: str
    migrated_legacy: bool = False


@dataclass(frozen=True, slots=True)
class ArchiveResult:
    original_path: Path
    archive_path: Path


def _yaml_string(value: object) -> str:
    return json.dumps(str(value or ""), ensure_ascii=False)


def _safe_component(value: str, fallback: str, maximum: int = 100) -> str:
    value = re.sub(r'[<>:"/\\|?*#\[\]\x00-\x1f]', " ", value)
    value = re.sub(r"\s+", " ", value).strip(" .")
    return value[:maximum].rstrip(" .") or fallback


def _stable_key(entry: dict[str, Any]) -> str:
    canonical = str(entry.get("canonical_key") or "").strip()
    if canonical:
        return _safe_component(canonical, f"herald-{int(entry['id']):06d}", 140)
    return f"herald-{int(entry['id']):06d}"


def _tag(value: str) -> str:
    value = value.casefold().replace("&", " and ")
    value = re.sub(r"[^a-z0-9]+", "-", value).strip("-")
    return value or "unsorted"


def _blockquote(value: str) -> str:
    lines = value.splitlines() or [""]
    return "\n".join(f"> {line}" if line else ">" for line in lines)


def _managed_frontmatter(entry: dict[str, Any]) -> str:
    tags = ["herald", _tag(str(entry.get("source_category", "Unsorted")))]
    content_kind = str(entry.get("content_kind") or "paper")
    if content_kind not in tags:
        tags.append(content_kind)
    return "\n".join(
        [
            FRONTMATTER_START,
            f"herald_id: {int(entry['id'])}",
            f"herald_key: {_yaml_string(_stable_key(entry))}",
            f"title: {_yaml_string(entry.get('title'))}",
            f"source: {_yaml_string(entry.get('canonical_url') or entry.get('url'))}",
            f"source_title: {_yaml_string(entry.get('source_title'))}",
            f"category: {_yaml_string(entry.get('source_category'))}",
            f"content_kind: {_yaml_string(content_kind)}",
            f"author: {_yaml_string(entry.get('author'))}",
            f"published_at: {_yaml_string(entry.get('published_at'))}",
            f"discovered_at: {_yaml_string(entry.get('discovered_at'))}",
            f"status: {_yaml_string(entry.get('status'))}",
            f"enrichment_status: {_yaml_string(entry.get('enrichment_status'))}",
            f"summary_provider: {_yaml_string(entry.get('summary_provider'))}",
            f"summary_model: {_yaml_string(entry.get('summary_model'))}",
            f"summary_generated_at: {_yaml_string(entry.get('summary_generated_at'))}",
            "tags:",
            *(f"  - {_yaml_string(tag)}" for tag in tags),
            FRONTMATTER_END,
        ]
    )


def _managed_body(entry: dict[str, Any]) -> str:
    title = str(entry.get("title") or "Untitled").replace("\n", " ").strip()
    summary = str(entry.get("summary") or "No summary is available.").strip()
    provider = str(entry.get("summary_provider") or "unknown")
    model = str(entry.get("summary_model") or "")
    provenance = f"{provider} ({model})" if model else provider
    content = str(entry.get("content") or "No article text was supplied by the feed.")
    source_url = str(entry.get("canonical_url") or entry.get("url") or "")
    source_title = str(entry.get("source_title") or "Unknown source")
    author = str(entry.get("author") or "Unknown")
    published = str(entry.get("published_at") or "Unknown")
    return "\n".join(
        [
            BODY_START,
            f"# {title}",
            "",
            "> [!abstract] Summary",
            f"> Method: {provenance}",
            ">",
            _blockquote(summary),
            "",
            "## Article text",
            "",
            content.strip(),
            "",
            "## Source",
            "",
            f"- Original: <{source_url}>" if source_url else "- Original: unavailable",
            f"- Publication: {source_title}",
            f"- Author: {author}",
            f"- Published: {published}",
            BODY_END,
        ]
    )


def render_markdown(entry: dict[str, Any]) -> str:
    return "\n".join(
        [
            "---",
            _managed_frontmatter(entry),
            "---",
            "",
            _managed_body(entry),
            "",
            "## My Notes",
            "",
            "",
        ]
    )


def _replace_managed_block(
    document: str, start: str, end: str, replacement: str
) -> str:
    if document.count(start) != 1 or document.count(end) != 1:
        raise ObsidianConflictError(
            "The note's Herald management markers are missing or ambiguous"
        )
    start_at = document.index(start)
    end_at = document.index(end, start_at) + len(end)
    if end_at <= start_at:
        raise ObsidianConflictError("The note's Herald management markers are invalid")
    return document[:start_at] + replacement + document[end_at:]


def _merge_managed(document: str, entry: dict[str, Any]) -> str:
    merged = _replace_managed_block(
        document, FRONTMATTER_START, FRONTMATTER_END, _managed_frontmatter(entry)
    )
    return _replace_managed_block(merged, BODY_START, BODY_END, _managed_body(entry))


_LEGACY_KEYS = {
    "herald_id",
    "title",
    "source",
    "source_title",
    "category",
    "author",
    "published_at",
    "discovered_at",
    "status",
    "summary_provider",
    "summary_model",
    "summary_generated_at",
    "tags",
}


def _migrate_legacy(document: str, entry: dict[str, Any]) -> str:
    """Migrate Herald's original note format without touching user additions."""
    if not document.startswith("---\n"):
        raise ObsidianConflictError("Existing file is not a Herald-managed note")
    frontmatter_end = document.find("\n---\n", 4)
    if frontmatter_end < 0:
        raise ObsidianConflictError("Legacy note has malformed frontmatter")
    old_frontmatter = document[4:frontmatter_end]
    id_match = re.search(r"(?m)^herald_id:\s*(\d+)\s*$", old_frontmatter)
    if id_match is None or int(id_match.group(1)) != int(entry["id"]):
        raise ObsidianConflictError("Existing file belongs to a different entry")

    preserved: list[str] = []
    skipping_tags = False
    for line in old_frontmatter.splitlines():
        key_match = re.match(r"^([A-Za-z0-9_-]+):", line)
        if key_match:
            key = key_match.group(1)
            skipping_tags = key == "tags"
            if key in _LEGACY_KEYS:
                continue
        elif skipping_tags and re.match(r"^\s+-\s+", line):
            continue
        else:
            skipping_tags = False
        preserved.append(line)

    body = document[frontmatter_end + len("\n---\n") :]
    notes_match = re.search(r"(?m)^## (?:My )?Notes\s*$", body)
    if notes_match is None:
        raise ObsidianConflictError(
            "Legacy Herald note has no Notes section; refusing to overwrite it"
        )
    notes = body[notes_match.end() :]
    custom_frontmatter = "\n".join(line for line in preserved if line.strip())
    frontmatter = _managed_frontmatter(entry)
    if custom_frontmatter:
        frontmatter += "\n" + custom_frontmatter
    return (
        f"---\n{frontmatter}\n---\n\n{_managed_body(entry)}"
        f"\n\n## My Notes{notes}"
    )


def _atomic_write(destination: Path, document: str) -> None:
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{destination.name}.", suffix=".tmp", dir=destination.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as file:
            file.write(document)
            file.flush()
            os.fsync(file.fileno())
        os.replace(temporary, destination)
    finally:
        temporary.unlink(missing_ok=True)


class ObsidianExporter:
    def __init__(self, vault_path: str | Path, archive_root: str | Path | None = None):
        self.vault_path = Path(vault_path).expanduser().resolve()
        self.archive_root = Path(
            archive_root or self.vault_path.parent / "obsidian-archive"
        ).expanduser().resolve()
        if self.vault_path == self.archive_root:
            raise ValueError("The vault and archive paths must be different")
        if (
            self.vault_path in self.archive_root.parents
            or self.archive_root in self.vault_path.parents
        ):
            raise ValueError("The archive must be outside the Obsidian vault")

    def _assert_safe(self, path: Path, root: Path) -> Path:
        root_resolved = root.resolve()
        candidate = path.resolve(strict=False)
        try:
            candidate.relative_to(root_resolved)
        except ValueError as error:
            raise ValueError(f"Path escapes managed root: {path}") from error
        current = root_resolved
        relative = path.absolute().relative_to(root.absolute())
        for part in relative.parts:
            current = current / part
            if current.is_symlink():
                raise ValueError(f"Symbolic links are not allowed in managed paths: {path}")
        return path

    def _destination(self, entry: dict[str, Any]) -> Path:
        existing = str(entry.get("obsidian_relative_path") or "")
        if existing:
            relative_existing = Path(existing)
            parts = relative_existing.parts
            content_kind = str(entry.get("content_kind") or "paper")
            is_stable_paper = (
                content_kind == "paper"
                and len(parts) == 3
                and parts[:2] == ("Herald", "Papers")
            )
            is_stable_news = (
                content_kind == "news"
                and len(parts) == 4
                and parts[:2] == ("Herald", "News")
            )
            if (
                not relative_existing.is_absolute()
                and relative_existing.suffix == ".md"
                and (is_stable_paper or is_stable_news)
            ):
                return self._assert_safe(
                    self.vault_path / relative_existing, self.vault_path
                )
        key = _stable_key(entry)
        if str(entry.get("content_kind") or "paper") == "news":
            publisher = _safe_component(
                str(entry.get("source_title") or "Unknown Publisher"),
                "Unknown Publisher",
            )
            relative = Path("Herald") / "News" / publisher / f"{key}.md"
        else:
            relative = Path("Herald") / "Papers" / f"{key}.md"
        return self._assert_safe(self.vault_path / relative, self.vault_path)

    def destination(self, entry: dict[str, Any]) -> Path:
        return self._destination(entry)

    def export(self, entry: dict[str, Any]) -> ExportResult:
        if entry.get("status") != "kept":
            raise ValueError("Only kept entries can be exported")
        destination = self._destination(entry)
        destination.parent.mkdir(parents=True, exist_ok=True)
        self._assert_safe(destination, self.vault_path)
        migrated = False
        if destination.exists():
            if not destination.is_file():
                raise ObsidianConflictError("The Obsidian destination is not a file")
            existing = destination.read_text(encoding="utf-8")
            if FRONTMATTER_START in existing or BODY_START in existing:
                document = _merge_managed(existing, entry)
            else:
                document = _migrate_legacy(existing, entry)
                migrated = True
        else:
            document = render_markdown(entry)
        _atomic_write(destination, document)
        return ExportResult(
            destination,
            hashlib.sha256(document.encode("utf-8")).hexdigest(),
            migrated,
        )

    def archive(self, entry: dict[str, Any], relative_path: str) -> ArchiveResult | None:
        if not relative_path:
            return None
        source = self._assert_safe(self.vault_path / relative_path, self.vault_path)
        if not source.exists():
            return None
        if not source.is_file():
            raise ObsidianConflictError("The Obsidian note is not a regular file")
        stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%S.%fZ")
        destination = self._assert_safe(
            self.archive_root / stamp / relative_path, self.archive_root
        )
        destination.parent.mkdir(parents=True, exist_ok=True)
        self._assert_safe(destination, self.archive_root)
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{destination.name}.", suffix=".tmp", dir=destination.parent
        )
        os.close(descriptor)
        temporary = Path(temporary_name)
        try:
            shutil.copy2(source, temporary)
            with temporary.open("rb") as file:
                os.fsync(file.fileno())
            os.replace(temporary, destination)
            source.unlink()
        finally:
            temporary.unlink(missing_ok=True)
        return ArchiveResult(source, destination)

    def restore(self, entry: dict[str, Any], archive_path: str) -> Path:
        source = self._assert_safe(self.archive_root / archive_path, self.archive_root)
        if not source.is_file():
            raise FileNotFoundError(f"Archived note does not exist: {archive_path}")
        destination = self._destination(entry)
        destination.parent.mkdir(parents=True, exist_ok=True)
        self._assert_safe(destination, self.vault_path)
        if destination.exists():
            raise ObsidianConflictError(
                "Cannot restore annotations because the destination already exists"
            )
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{destination.name}.", suffix=".tmp", dir=destination.parent
        )
        os.close(descriptor)
        temporary = Path(temporary_name)
        try:
            shutil.copy2(source, temporary)
            os.replace(temporary, destination)
        finally:
            temporary.unlink(missing_ok=True)
        return destination

    def relative_path(self, path: Path) -> str:
        return path.resolve().relative_to(self.vault_path).as_posix()

    def archive_relative_path(self, path: Path) -> str:
        return path.resolve().relative_to(self.archive_root).as_posix()
