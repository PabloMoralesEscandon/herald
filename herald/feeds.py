from __future__ import annotations

import hashlib
import html
import re
from dataclasses import dataclass
from datetime import UTC, datetime
from email.utils import parsedate_to_datetime
from html.parser import HTMLParser
from urllib.parse import parse_qsl, urlencode, urljoin, urlsplit, urlunsplit
from xml.etree import ElementTree

from .markdown import html_to_markdown


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
    content_markdown: str = ""


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


class _FeedLinkExtractor(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.feed_links: list[str] = []

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        if tag.lower() != "link":
            return
        values = {key.lower(): (value or "") for key, value in attrs}
        relations = {item.casefold() for item in values.get("rel", "").split()}
        media_type = values.get("type", "").split(";", 1)[0].strip().casefold()
        if (
            "alternate" in relations
            and media_type
            in {
                "application/rss+xml",
                "application/atom+xml",
                "application/rdf+xml",
            }
            and values.get("href", "").strip()
        ):
            self.feed_links.append(values["href"].strip())

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


def _element_markup(element: ElementTree.Element | None) -> str:
    """Keep embedded feed markup, including Atom's namespaced XHTML form."""
    if element is None:
        return ""
    if not list(element):
        return (element.text or "").strip()

    def serialize(current: ElementTree.Element) -> str:
        name = _local_name(current.tag)
        attributes = "".join(
            f' {key.rsplit("}", 1)[-1]}="{html.escape(value, quote=True)}"'
            for key, value in current.attrib.items()
        )
        inner = html.escape(current.text or "", quote=False)
        for child in current:
            inner += serialize(child)
            inner += html.escape(child.tail or "", quote=False)
        return f"<{name}{attributes}>{inner}</{name}>"

    prefix = html.escape(element.text or "", quote=False)
    return prefix + "".join(
        serialize(child) + html.escape(child.tail or "", quote=False)
        for child in element
    )


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


_TRACKING_PARAMETERS = {
    "fbclid",
    "gclid",
    "dclid",
    "msclkid",
    "mc_cid",
    "mc_eid",
    "vero_conv",
    "vero_id",
    "oly_anon_id",
    "oly_enc_id",
}


def canonicalize_url(value: str) -> str:
    """Normalize an article URL and remove only recognized tracking parameters."""
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
    query = [
        (key, item)
        for key, item in parse_qsl(parts.query, keep_blank_values=True)
        if not key.casefold().startswith("utm_")
        and key.casefold() not in _TRACKING_PARAMETERS
    ]
    query.sort(key=lambda pair: (pair[0].casefold(), pair[1]))
    return urlunsplit(
        (
            parts.scheme.lower(),
            hostname,
            parts.path or "/",
            urlencode(query, doseq=True),
            "",
        )
    )


def discover_feed_url(document: bytes | str, page_url: str) -> str | None:
    """Return the first RSS/Atom autodiscovery link from an HTML document.

    This deliberately reads only standard ``<link rel=alternate>`` metadata; it
    is not a page/article scraper.
    """
    if isinstance(document, bytes):
        try:
            text = document.decode("utf-8", errors="replace")
        except (UnicodeDecodeError, AttributeError):
            return None
    else:
        text = document
    parser = _FeedLinkExtractor()
    try:
        parser.feed(text)
    except ValueError:
        return None
    if not parser.feed_links:
        return None
    discovered = urljoin(page_url, parser.feed_links[0])
    parts = urlsplit(discovered)
    if parts.scheme.casefold() not in {"http", "https"} or not parts.netloc:
        return None
    return canonicalize_url(discovered)


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
        url = canonicalize_url(_atom_link(item))
        published = _published_at(
            _element_text(_child(item, "published", "updated"))
        )
        guid = _element_text(_child(item, "id")) or url
        content_markup = _element_markup(_child(item, "content", "summary"))
        content = _plain_text(content_markup)
        content_markdown = html_to_markdown(content_markup, base_url=url)
        parsed.append(
            FeedEntry(
                guid=guid or _generated_guid(title, url, published),
                url=url,
                title=title,
                author=_atom_author(item),
                published_at=published,
                content=content,
                content_markdown=content_markdown,
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
        url = canonicalize_url(_rss_link(item))
        published = _published_at(
            _element_text(_child(item, "pubdate", "published", "date", "updated"))
        )
        guid = _element_text(_child(item, "guid", "id")) or url
        content_markup = _element_markup(
            _child(item, "encoded", "content", "description", "summary")
        )
        content = _plain_text(content_markup)
        content_markdown = html_to_markdown(content_markup, base_url=url)
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
                content_markdown=content_markdown,
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
