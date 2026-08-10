from __future__ import annotations

import json
from dataclasses import dataclass
from importlib.resources import files
from typing import Any, Iterable, Mapping
from urllib.parse import urlsplit

from .feeds import canonicalize_url


SOURCE_MANIFEST_FORMAT = "herald.sources"
SOURCE_MANIFEST_VERSION = 1
MAX_SOURCES = 1_000
_SOURCE_FIELDS = {"title", "url", "category", "content_kind", "enabled"}


class SourceManifestError(ValueError):
    """Raised when a portable source manifest is invalid or unsafe."""


@dataclass(frozen=True, slots=True)
class CuratedSource:
    title: str
    url: str
    category: str
    content_kind: str = "paper"
    enabled: bool = True

    def to_dict(self) -> dict[str, str | bool]:
        return {
            "title": self.title,
            "url": self.url,
            "category": self.category,
            "content_kind": self.content_kind,
            "enabled": self.enabled,
        }


def _required_text(item: Mapping[str, Any], key: str, index: int) -> str:
    value = item.get(key)
    if not isinstance(value, str) or not value.strip():
        raise SourceManifestError(f"sources[{index}].{key} must be a non-empty string")
    value = value.strip()
    limit = 2_048 if key == "url" else 200
    if len(value) > limit:
        raise SourceManifestError(f"sources[{index}].{key} is too long")
    return value


def validate_source_manifest(payload: Any) -> tuple[CuratedSource, ...]:
    if not isinstance(payload, dict):
        raise SourceManifestError("Source manifest must be a JSON object")
    if payload.get("format") != SOURCE_MANIFEST_FORMAT:
        raise SourceManifestError(f"format must be {SOURCE_MANIFEST_FORMAT!r}")
    if payload.get("version") != SOURCE_MANIFEST_VERSION:
        raise SourceManifestError(
            f"Unsupported source manifest version: {payload.get('version')!r}"
        )
    raw_sources = payload.get("sources")
    if not isinstance(raw_sources, list):
        raise SourceManifestError("sources must be a JSON array")
    if len(raw_sources) > MAX_SOURCES:
        raise SourceManifestError(f"A manifest may contain at most {MAX_SOURCES} sources")

    result: list[CuratedSource] = []
    seen_urls: set[str] = set()
    for index, raw in enumerate(raw_sources):
        if not isinstance(raw, dict):
            raise SourceManifestError(f"sources[{index}] must be a JSON object")
        unexpected = set(raw) - _SOURCE_FIELDS
        if unexpected:
            names = ", ".join(sorted(unexpected))
            raise SourceManifestError(f"sources[{index}] has unsupported fields: {names}")
        title = _required_text(raw, "title", index)
        raw_url = _required_text(raw, "url", index)
        raw_parts = urlsplit(raw_url)
        try:
            raw_port = raw_parts.port
        except ValueError as error:
            raise SourceManifestError(f"sources[{index}].url has an invalid port") from error
        if raw_parts.scheme not in {"http", "https"} or not raw_parts.hostname:
            raise SourceManifestError(f"sources[{index}].url must be an HTTP(S) URL")
        if raw_parts.username or raw_parts.password:
            raise SourceManifestError(f"sources[{index}].url must not contain credentials")
        if raw_port is not None and raw_port not in {80, 443}:
            raise SourceManifestError(f"sources[{index}].url must use a standard HTTP(S) port")
        url = canonicalize_url(raw_url)
        category = _required_text(raw, "category", index)
        content_kind = raw.get("content_kind")
        if content_kind not in {"paper", "news"}:
            raise SourceManifestError(
                f"sources[{index}].content_kind must be paper or news"
            )
        enabled = raw.get("enabled", True)
        if not isinstance(enabled, bool):
            raise SourceManifestError(f"sources[{index}].enabled must be a boolean")
        if url in seen_urls:
            raise SourceManifestError(f"Duplicate source URL: {url}")
        seen_urls.add(url)
        result.append(CuratedSource(title, url, category, content_kind, enabled))
    return tuple(result)


def load_source_catalog() -> tuple[CuratedSource, ...]:
    resource = files("herald").joinpath("data/sources.json")
    try:
        payload = json.loads(resource.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise SourceManifestError(f"Could not load packaged source catalog: {error}") from error
    return validate_source_manifest(payload)


def create_source_manifest(sources: Iterable[Mapping[str, Any]]) -> dict[str, Any]:
    portable = [
        {
            "title": source["title"],
            "url": source["url"],
            "category": source["category"],
            "content_kind": source.get("content_kind", "paper"),
            "enabled": bool(source.get("enabled", True)),
        }
        for source in sources
    ]
    portable.sort(
        key=lambda source: (
            str(source["content_kind"]),
            str(source["category"]).casefold(),
            str(source["title"]).casefold(),
            str(source["url"]),
        )
    )
    validated = validate_source_manifest(
        {"format": SOURCE_MANIFEST_FORMAT, "version": SOURCE_MANIFEST_VERSION, "sources": portable}
    )
    return {
        "format": SOURCE_MANIFEST_FORMAT,
        "version": SOURCE_MANIFEST_VERSION,
        "sources": [source.to_dict() for source in validated],
    }


CURATED_SOURCES = load_source_catalog()
RESEARCH_SOURCES = tuple(source for source in CURATED_SOURCES if source.content_kind == "paper")
NEWS_SOURCES = tuple(source for source in CURATED_SOURCES if source.content_kind == "news")
