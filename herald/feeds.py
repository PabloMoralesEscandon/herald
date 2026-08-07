from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass
from datetime import UTC, datetime
from email.utils import parsedate_to_datetime
from html.parser import HTMLParser
from urllib.parse import urlsplit, urlunsplit
from xml.etree import ElementTree


class FeedParseError(ValueError):
    """Raised when a document is not a supported or well-formed feed."""


@dataclass(frozen=True, slots=True)
class FeedEntry:
    guid: str
    url: str
    title: str
    author: str = ""
    published_at: str | None = None
    content: str = ""


class _TextExtractor(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.parts: list[str] = []

    def handle_data(self, data: str) -> None:
        self.parts.append(data)

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        if tag in {"br", "p", "div", "li", "h1", "h2", "h3", "tr"}:
            self.parts.append("\n")

    def handle_endtag(self, tag: str) -> None:
        if tag in {"p", "div", "li", "h1", "h2", "h3", "tr"}:
            self.parts.append("\n")


def _local_name(tag: str) -> str:
    return tag.rsplit("}", 1)[-1].lower()


def _children(element: ElementTree.Element, name: str) -> list[ElementTree.Element]:
    wanted = name.lower()
    return [child for child in element if _local_name(child.tag) == wanted]


def _child(element: ElementTree.Element, *names: str) -> ElementTree.Element | None:
    for name in names:
        wanted = name.lower()
        match = next(
            (child for child in element if _local_name(child.tag) == wanted), None
        )
        if match is not None:
            return match
    return None


def _element_text(element: ElementTree.Element | None) -> str:
    if element is None:
        return ""
    return "".join(element.itertext()).strip()


def _plain_text(value: str) -> str:
    if not value:
        return ""
    if "<" in value and ">" in value:
        extractor = _TextExtractor()
        try:
            extractor.feed(value)
            value = " ".join(extractor.parts)
        except ValueError:
            pass
    return re.sub(r"\s+", " ", value).strip()


def _normalized_url(value: str) -> str:
    value = value.strip()
    if not value:
        return ""
    parts = urlsplit(value)
    if parts.scheme.lower() not in {"http", "https"}:
        return value
    hostname = (parts.hostname or "").lower()
    port = parts.port
    if port and not (
        (parts.scheme.lower() == "http" and port == 80)
        or (parts.scheme.lower() == "https" and port == 443)
    ):
        hostname = f"{hostname}:{port}"
    return urlunsplit(
        (parts.scheme.lower(), hostname, parts.path or "/", parts.query, "")
    )


def _published_at(value: str) -> str | None:
    value = value.strip()
    if not value:
        return None
    try:
        parsed = parsedate_to_datetime(value)
    except (TypeError, ValueError, OverflowError):
        try:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            return value
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=UTC)
    return parsed.astimezone(UTC).replace(microsecond=0).isoformat()


def _generated_guid(title: str, url: str, published_at: str | None) -> str:
    material = "\x1f".join((title, url, published_at or ""))
    return "herald:" + hashlib.sha256(material.encode("utf-8")).hexdigest()


def _atom_link(element: ElementTree.Element) -> str:
    fallback = ""
    for link in _children(element, "link"):
        href = link.attrib.get("href", "").strip()
        if not href:
            continue
        relation = link.attrib.get("rel", "alternate").lower()
        if relation == "alternate":
            return href
        if not fallback:
            fallback = href
    return fallback


def _atom_author(element: ElementTree.Element) -> str:
    authors: list[str] = []
    for author in _children(element, "author"):
        name = _element_text(_child(author, "name")) or _element_text(author)
        if name:
            authors.append(name)
    return ", ".join(authors)


def _parse_atom(root: ElementTree.Element) -> list[FeedEntry]:
    parsed: list[FeedEntry] = []
    for item in _children(root, "entry"):
        title = _plain_text(_element_text(_child(item, "title"))) or "Untitled"
        url = _normalized_url(_atom_link(item))
        published = _published_at(
            _element_text(_child(item, "published", "updated"))
        )
        guid = _element_text(_child(item, "id")) or url
        content = _plain_text(
            _element_text(_child(item, "content", "summary"))
        )
        parsed.append(
            FeedEntry(
                guid=guid or _generated_guid(title, url, published),
                url=url,
                title=title,
                author=_atom_author(item),
                published_at=published,
                content=content,
            )
        )
    return parsed


def _rss_link(element: ElementTree.Element) -> str:
    link = _child(element, "link")
    if link is None:
        return ""
    return link.attrib.get("href", "").strip() or _element_text(link)


def _parse_rss(root: ElementTree.Element) -> list[FeedEntry]:
    channel = _child(root, "channel")
    container = root if _local_name(root.tag) == "rdf" or channel is None else channel
    parsed: list[FeedEntry] = []
    for item in _children(container, "item"):
        title = _plain_text(_element_text(_child(item, "title"))) or "Untitled"
        url = _normalized_url(_rss_link(item))
        published = _published_at(
            _element_text(_child(item, "pubdate", "published", "date", "updated"))
        )
        guid = _element_text(_child(item, "guid", "id")) or url
        content = _plain_text(
            _element_text(_child(item, "encoded", "content", "description", "summary"))
        )
        parsed.append(
            FeedEntry(
                guid=guid or _generated_guid(title, url, published),
                url=url,
                title=title,
                author=_plain_text(
                    _element_text(_child(item, "creator", "author"))
                ),
                published_at=published,
                content=content,
            )
        )
    return parsed


def parse_feed(document: bytes | str) -> list[FeedEntry]:
    """Parse RSS 2.0, RSS 1.0/RDF, or Atom into normalized entries."""

    try:
        root = ElementTree.fromstring(document)
    except (ElementTree.ParseError, ValueError) as error:
        raise FeedParseError(f"Invalid XML feed: {error}") from error

    kind = _local_name(root.tag)
    if kind == "feed":
        return _parse_atom(root)
    if kind in {"rss", "rdf"}:
        return _parse_rss(root)
    raise FeedParseError(f"Unsupported feed root element: {kind}")
