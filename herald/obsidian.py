from __future__ import annotations

import json
import os
import re
import tempfile
from pathlib import Path
from typing import Any


def _yaml_string(value: object) -> str:
    return json.dumps(str(value or ""), ensure_ascii=False)


def _safe_component(value: str, fallback: str, maximum: int = 100) -> str:
    value = re.sub(r'[<>:"/\\|?*#\[\]\x00-\x1f]', " ", value)
    value = re.sub(r"\s+", " ", value).strip(" .")
    return (value[:maximum].rstrip(" .") or fallback)


def _tag(value: str) -> str:
    value = value.casefold().replace("&", " and ")
    value = re.sub(r"[^a-z0-9]+", "-", value).strip("-")
    return value or "unsorted"


def _blockquote(value: str) -> str:
    lines = value.splitlines() or [""]
    return "\n".join(f"> {line}" if line else ">" for line in lines)


def render_markdown(entry: dict[str, Any]) -> str:
    tags = ["herald", _tag(str(entry.get("source_category", "Unsorted")))]
    frontmatter = [
        "---",
        f"herald_id: {int(entry['id'])}",
        f"title: {_yaml_string(entry.get('title'))}",
        f"source: {_yaml_string(entry.get('url'))}",
        f"source_title: {_yaml_string(entry.get('source_title'))}",
        f"category: {_yaml_string(entry.get('source_category'))}",
        f"author: {_yaml_string(entry.get('author'))}",
        f"published_at: {_yaml_string(entry.get('published_at'))}",
        f"discovered_at: {_yaml_string(entry.get('discovered_at'))}",
        f"status: {_yaml_string(entry.get('status'))}",
        "tags:",
        *(f"  - {_yaml_string(tag)}" for tag in tags),
        "---",
    ]
    title = str(entry.get("title") or "Untitled").replace("\n", " ").strip()
    summary = str(entry.get("summary") or "No summary is available.").strip()
    content = str(entry.get("content") or "No article text was supplied by the feed.")
    source_url = str(entry.get("url") or "")
    source_title = str(entry.get("source_title") or "Unknown source")
    author = str(entry.get("author") or "Unknown")
    published = str(entry.get("published_at") or "Unknown")
    body = [
        *frontmatter,
        "",
        f"# {title}",
        "",
        "> [!abstract] Summary",
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
        "",
        "## Notes",
        "",
        "",
    ]
    return "\n".join(body)


class ObsidianExporter:
    def __init__(self, vault_path: str | Path):
        self.vault_path = Path(vault_path)

    def _destination(self, entry: dict[str, Any]) -> Path:
        existing = str(entry.get("exported_path") or "")
        if existing:
            candidate = (self.vault_path / existing).resolve()
            try:
                candidate.relative_to(self.vault_path.resolve())
            except ValueError:
                pass
            else:
                return candidate

        category = _safe_component(
            str(entry.get("source_category") or "Unsorted"), "Unsorted"
        )
        title = _safe_component(str(entry.get("title") or ""), "Untitled")
        filename = f"{int(entry['id']):06d} - {title}.md"
        return self.vault_path / "Herald" / category / filename

    def export(self, entry: dict[str, Any]) -> Path:
        if entry.get("status") != "kept":
            raise ValueError("Only kept entries can be exported")
        destination = self._destination(entry)
        destination.parent.mkdir(parents=True, exist_ok=True)
        document = render_markdown(entry)
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{destination.name}.", suffix=".tmp", dir=destination.parent
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as file:
                file.write(document)
            os.replace(temporary, destination)
        finally:
            temporary.unlink(missing_ok=True)
        return destination

    def relative_path(self, path: Path) -> str:
        return path.resolve().relative_to(self.vault_path.resolve()).as_posix()
