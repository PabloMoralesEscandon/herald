from __future__ import annotations

import html
import http.client
import ipaddress
import json
import re
import socket
import time
import urllib.error
import urllib.request
from collections import Counter
from dataclasses import dataclass, field
from datetime import UTC, datetime
from html.parser import HTMLParser
from typing import Any, Callable, Mapping
from urllib.parse import quote, unquote, urljoin, urlsplit, urlunsplit

from .storage import Database
from .summaries import deterministic_summary


SEMANTIC_SCHOLAR_API = "https://api.semanticscholar.org/graph/v1/paper/"
CROSSREF_API = "https://api.crossref.org/works/"
MAX_PROVIDER_BYTES = 5 * 1024 * 1024
MAX_PAPER_PAGE_BYTES = 2 * 1024 * 1024
PROVIDER_CACHE_SECONDS = 7 * 24 * 60 * 60
MANUAL_SOURCE_URL = "herald://manual-imports"

DOI_RE = re.compile(r"10\.\d{4,9}/[-._;()/:a-z0-9]+", re.IGNORECASE)
ARXIV_RE = re.compile(
    r"(?:arxiv\s*:\s*)?((?:\d{4}\.\d{4,5}|[a-z-]+(?:\.[A-Z]{2})?/\d{7})(?:v\d+)?)",
    re.IGNORECASE,
)
S2_ID_RE = re.compile(r"[0-9a-f]{40}", re.IGNORECASE)
WORD_RE = re.compile(r"[a-z](?:[a-z0-9+.-]*[a-z0-9+])?", re.IGNORECASE)

STOPWORDS = {
    "about", "after", "against", "also", "among", "and", "are", "based",
    "been", "before", "being", "between", "both", "but", "can", "could",
    "does", "each", "for", "from", "have", "into", "its", "more", "most", "not",
    "our", "over", "paper", "results", "show", "such", "than", "that", "the",
    "their", "these", "they", "this", "through", "using", "was", "were", "which",
    "while", "with", "within", "without", "would", "you",
}


class PaperImportError(RuntimeError):
    """A user-facing manual import failure."""


class PaperNotFoundError(PaperImportError):
    """No usable metadata exists for the requested paper."""


class PaperFetchError(PaperImportError):
    def __init__(self, message: str, *, transient: bool = False):
        super().__init__(message)
        self.transient = transient


class UnsafePaperUrlError(PaperImportError):
    """A URL failed Herald's public HTTPS checks."""


FetchDocument = Callable[[str, Mapping[str, str], int, float], bytes]


def _validate_public_https_url(url: str, *, resolve_dns: bool) -> str:
    parts = urlsplit(url.strip())
    if parts.scheme.lower() != "https" or not parts.hostname:
        raise UnsafePaperUrlError("Paper URLs must use public HTTPS")
    if parts.username is not None or parts.password is not None:
        raise UnsafePaperUrlError("Paper URLs cannot contain credentials")
    try:
        port = parts.port
    except ValueError as error:
        raise UnsafePaperUrlError("Paper URL has an invalid port") from error
    if port not in {None, 443}:
        raise UnsafePaperUrlError("Paper URLs must use the standard HTTPS port")

    addresses: set[str] = set()
    try:
        addresses.add(str(ipaddress.ip_address(parts.hostname)))
    except ValueError:
        if resolve_dns:
            try:
                addresses.update(
                    item[4][0]
                    for item in socket.getaddrinfo(parts.hostname, 443, type=socket.SOCK_STREAM)
                )
            except OSError as error:
                raise PaperFetchError(f"Could not resolve paper host: {error}") from error
    for value in addresses:
        address = ipaddress.ip_address(value)
        if not address.is_global:
            raise UnsafePaperUrlError("Paper URLs cannot target private or local networks")
    return urlunsplit(("https", parts.netloc, parts.path or "/", parts.query, ""))


class _SafeRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(
        self,
        req: urllib.request.Request,
        fp: Any,
        code: int,
        msg: str,
        headers: Any,
        newurl: str,
    ) -> urllib.request.Request | None:
        safe_url = _validate_public_https_url(
            urljoin(req.full_url, newurl), resolve_dns=True
        )
        return super().redirect_request(req, fp, code, msg, headers, safe_url)


def fetch_public_document(
    url: str,
    headers: Mapping[str, str],
    max_bytes: int,
    timeout: float,
) -> bytes:
    safe_url = _validate_public_https_url(url, resolve_dns=True)
    request_headers = {
        "User-Agent": "Herald/0.1 (+local research reader; polite metadata client)",
        "Accept-Encoding": "identity",
        **headers,
    }
    request = urllib.request.Request(safe_url, headers=request_headers)
    opener = urllib.request.build_opener(_SafeRedirectHandler())
    try:
        with opener.open(request, timeout=timeout) as response:
            _validate_public_https_url(response.geturl(), resolve_dns=True)
            declared = response.headers.get("Content-Length")
            if declared and int(declared) > max_bytes:
                raise PaperFetchError("Remote metadata exceeds Herald's size limit")
            document = response.read(max_bytes + 1)
    except urllib.error.HTTPError as error:
        if error.code == 404:
            raise PaperNotFoundError("Paper metadata was not found") from error
        raise PaperFetchError(
            f"Metadata provider returned HTTP {error.code}",
            transient=error.code == 429 or error.code >= 500,
        ) from error
    except (urllib.error.URLError, http.client.HTTPException, TimeoutError, OSError) as error:
        raise PaperFetchError(f"Could not fetch paper metadata: {error}", transient=True) from error
    except ValueError as error:
        raise PaperFetchError("Remote metadata has an invalid size") from error
    if len(document) > max_bytes:
        raise PaperFetchError("Remote metadata exceeds Herald's size limit")
    return document


@dataclass(frozen=True, slots=True)
class PaperLocator:
    scheme: str
    value: str
    page_url: str = ""


@dataclass(slots=True)
class PaperReferenceMetadata:
    title: str = ""
    url: str = ""
    identifiers: dict[str, str] = field(default_factory=dict)


@dataclass(slots=True)
class PaperMetadata:
    title: str = ""
    authors: list[str] = field(default_factory=list)
    abstract: str = ""
    published_at: str | None = None
    url: str = ""
    identifiers: dict[str, str] = field(default_factory=dict)
    topics: list[str] = field(default_factory=list)
    supplied_keywords: list[str] = field(default_factory=list)
    references: list[PaperReferenceMetadata] = field(default_factory=list)
    provider: str = ""


@dataclass(frozen=True, slots=True)
class PaperImportResult:
    entry: dict[str, Any]
    created: bool
    identifiers: list[dict[str, Any]]
    keywords: list[dict[str, Any]]
    references: list[dict[str, Any]]

    def to_dict(self) -> dict[str, Any]:
        return {
            "entry": self.entry,
            "created": self.created,
            "identifiers": self.identifiers,
            "keywords": self.keywords,
            "references": self.references,
        }


def normalize_doi(value: str) -> str:
    decoded = unquote(value.strip())
    decoded = re.sub(r"^(?:doi\s*:\s*|https?://(?:dx\.)?doi\.org/)", "", decoded, flags=re.I)
    match = DOI_RE.search(decoded)
    if not match:
        raise ValueError("Invalid DOI")
    doi = match.group(0).rstrip(".,;").lower()
    if match.start() != 0 or decoded[match.end():].strip(" .,)];}"):
        raise ValueError("Invalid DOI")
    return doi


def normalize_arxiv_id(value: str) -> str:
    decoded = unquote(value.strip())
    decoded = re.sub(r"^https?://(?:www\.)?arxiv\.org/(?:abs|pdf)/", "", decoded, flags=re.I)
    decoded = re.sub(r"\.pdf$", "", decoded, flags=re.I)
    match = ARXIV_RE.fullmatch(decoded.strip())
    if not match:
        raise ValueError("Invalid arXiv identifier")
    return re.sub(r"v\d+$", "", match.group(1), flags=re.I).lower()


def parse_paper_locator(value: str) -> PaperLocator:
    raw = value.strip()
    if not raw or len(raw) > 2_048:
        raise ValueError("Paper identifier or URL is required")
    try:
        return PaperLocator("doi", normalize_doi(raw))
    except ValueError:
        pass
    try:
        return PaperLocator("arxiv", normalize_arxiv_id(raw))
    except ValueError:
        pass

    parts = urlsplit(raw)
    if parts.scheme.lower() != "https" or not parts.hostname:
        raise ValueError("Use a DOI, arXiv ID, Semantic Scholar URL, or public HTTPS paper URL")
    hostname = parts.hostname.lower().removeprefix("www.")
    if hostname == "doi.org":
        return PaperLocator("doi", normalize_doi(raw))
    if hostname == "arxiv.org":
        return PaperLocator("arxiv", normalize_arxiv_id(raw))
    if hostname.endswith("semanticscholar.org"):
        segments = [segment for segment in parts.path.split("/") if segment]
        candidate = segments[-1] if segments else ""
        if not S2_ID_RE.fullmatch(candidate):
            raise ValueError("Semantic Scholar URL does not contain a paper ID")
        return PaperLocator("s2", candidate.lower())
    safe_url = _validate_public_https_url(raw, resolve_dns=False)
    return PaperLocator("url", safe_url, safe_url)


class _CitationMetaParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.values: dict[str, list[str]] = {}
        self.canonical_url = ""
        self._title_parts: list[str] = []
        self._inside_title = False

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        values = {name.lower(): value or "" for name, value in attrs}
        if tag.lower() == "meta":
            name = (values.get("name") or values.get("property") or "").lower()
            content = html.unescape(values.get("content", "")).strip()
            if name and content:
                self.values.setdefault(name, []).append(content)
        elif tag.lower() == "link" and values.get("rel", "").lower() == "canonical":
            self.canonical_url = values.get("href", "").strip()
        elif tag.lower() == "title":
            self._inside_title = True

    def handle_endtag(self, tag: str) -> None:
        if tag.lower() == "title":
            self._inside_title = False

    def handle_data(self, data: str) -> None:
        if self._inside_title:
            self._title_parts.append(data)

    @property
    def page_title(self) -> str:
        return " ".join("".join(self._title_parts).split())


def _first(values: dict[str, list[str]], *names: str) -> str:
    for name in names:
        items = values.get(name, [])
        if items:
            return items[0].strip()
    return ""


def _normalize_date(value: Any, *, year: Any = None) -> str | None:
    text = str(value or "").strip()
    if not text and year:
        text = str(year)
    if not text:
        return None
    if re.fullmatch(r"\d{4}", text):
        return f"{text}-01-01T00:00:00+00:00"
    candidate = text.replace("Z", "+00:00")
    try:
        parsed = datetime.fromisoformat(candidate)
    except ValueError:
        return text[:64]
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=UTC)
    return parsed.astimezone(UTC).replace(microsecond=0).isoformat()


def parse_citation_metadata(document: bytes, page_url: str) -> PaperMetadata:
    try:
        text = document.decode("utf-8")
    except UnicodeDecodeError:
        text = document.decode("utf-8", errors="replace")
    parser = _CitationMetaParser()
    try:
        parser.feed(text)
    except Exception as error:  # HTMLParser can expose malformed char references.
        raise PaperImportError(f"Paper page metadata could not be parsed: {error}") from error
    values = parser.values
    identifiers: dict[str, str] = {}
    doi_value = _first(values, "citation_doi", "dc.identifier", "prism.doi")
    if doi_value:
        try:
            identifiers["doi"] = normalize_doi(doi_value)
        except ValueError:
            pass
    arxiv_value = _first(values, "citation_arxiv_id", "arxiv_id")
    if arxiv_value:
        try:
            identifiers["arxiv"] = normalize_arxiv_id(arxiv_value)
        except ValueError:
            pass
    keyword_values = values.get("citation_keywords", []) + values.get("keywords", [])
    keywords = [
        item.strip()
        for value in keyword_values
        for item in re.split(r"[,;]", value)
        if item.strip()
    ]
    canonical = urljoin(page_url, parser.canonical_url) if parser.canonical_url else page_url
    if urlsplit(canonical).scheme != "https":
        canonical = page_url
    return PaperMetadata(
        title=_first(values, "citation_title", "dc.title", "og:title") or parser.page_title,
        authors=[
            item.strip()
            for item in values.get("citation_author", []) + values.get("dc.creator", [])
            if item.strip()
        ],
        abstract=_first(values, "citation_abstract", "dc.description", "description", "og:description"),
        published_at=_normalize_date(_first(values, "citation_publication_date", "citation_date", "dc.date")),
        url=canonical,
        identifiers=identifiers,
        supplied_keywords=keywords,
        provider="page-meta",
    )


def _clean_text(value: Any) -> str:
    if not isinstance(value, str):
        return ""
    return " ".join(html.unescape(value).split())


def extract_keywords(title: str, abstract: str, *, limit: int = 8) -> list[dict[str, Any]]:
    """Rank deterministic uni/bi/tri-grams with a title and phrase boost."""
    title_tokens = [token.lower() for token in WORD_RE.findall(title)]
    body_tokens = [token.lower() for token in WORD_RE.findall(abstract)]
    scores: Counter[str] = Counter()
    for tokens, weight in ((title_tokens, 4.0), (body_tokens, 1.0)):
        for size in (1, 2, 3):
            for index in range(len(tokens) - size + 1):
                phrase_tokens = tokens[index:index + size]
                if any(token in STOPWORDS for token in phrase_tokens):
                    continue
                phrase = " ".join(phrase_tokens)
                scores[phrase] += weight * (1.0 + 0.55 * (size - 1))
    ranked: list[tuple[str, float]] = []
    for phrase, score in scores.items():
        if len(phrase) < 4:
            continue
        ranked.append((phrase, round(score, 3)))
    ranked.sort(key=lambda item: (-item[1], -item[0].count(" "), item[0]))

    selected: list[tuple[str, float]] = []
    for phrase, score in ranked:
        phrase_words = set(phrase.split())
        if any(phrase_words < set(existing.split()) for existing, _ in selected):
            continue
        selected.append((phrase, score))
        if len(selected) == limit:
            break
    return [
        {"keyword": phrase, "kind": "keyword", "score": score, "provider": "deterministic-tfidf"}
        for phrase, score in selected
    ]


class PaperImporter:
    def __init__(
        self,
        database: Database,
        *,
        fetcher: FetchDocument = fetch_public_document,
        sleep: Callable[[float], None] = time.sleep,
        request_delay: float | None = None,
        retries: int = 2,
        timeout: float = 10.0,
    ) -> None:
        self.database = database
        self.fetcher = fetcher
        self.sleep = sleep
        self.request_delay = None if request_delay is None else max(0.0, request_delay)
        self.retries = max(0, retries)
        self.timeout = min(max(timeout, 1.0), 30.0)
        self._last_request_at: dict[str, float] = {}

    def import_paper(
        self,
        value: str,
        *,
        inspect_page: bool = False,
    ) -> PaperImportResult:
        locator = parse_paper_locator(value)
        page_metadata: PaperMetadata | None = None
        provider_locator = locator
        provider_errors: list[PaperImportError] = []
        requested_url = ""
        raw_parts = urlsplit(value.strip())
        if raw_parts.scheme.lower() == "https" and raw_parts.hostname:
            requested_url = _validate_public_https_url(value.strip(), resolve_dns=False)
        page_url = locator.page_url or (requested_url if inspect_page else "")
        if page_url:
            try:
                document = self.fetcher(
                    page_url,
                    {"Accept": "text/html, application/xhtml+xml"},
                    MAX_PAPER_PAGE_BYTES,
                    self.timeout,
                )
                page_metadata = parse_citation_metadata(document, page_url)
            except PaperImportError as error:
                # A DOI/arXiv/S2 identifier can still be enriched by a metadata
                # provider when the publication page is temporarily unavailable.
                # For a generic page URL there is no other identifier to query.
                if locator.scheme == "url":
                    raise
                provider_errors.append(error)
        if page_metadata is not None:
            if "doi" in page_metadata.identifiers:
                provider_locator = PaperLocator("doi", page_metadata.identifiers["doi"], locator.page_url)
            elif "arxiv" in page_metadata.identifiers:
                provider_locator = PaperLocator("arxiv", page_metadata.identifiers["arxiv"], locator.page_url)

        metadata: PaperMetadata | None = None
        if provider_locator.scheme in {"doi", "arxiv", "s2"}:
            try:
                metadata = self._semantic_scholar(provider_locator)
            except PaperImportError as error:
                provider_errors.append(error)
            if metadata is None and provider_locator.scheme == "doi":
                try:
                    metadata = self._crossref(provider_locator.value)
                except PaperImportError as error:
                    provider_errors.append(error)

        if metadata is None:
            metadata = page_metadata
        elif page_metadata is not None:
            metadata = self._merge_metadata(metadata, page_metadata)
        if metadata is None or not metadata.title:
            fetch_errors = [error for error in provider_errors if isinstance(error, PaperFetchError)]
            if fetch_errors:
                raise fetch_errors[-1]
            detail = "; ".join(str(error) for error in provider_errors) or "No citation metadata was found"
            raise PaperNotFoundError(detail)
        if provider_locator.scheme != "url":
            metadata.identifiers.setdefault(provider_locator.scheme, provider_locator.value)
        if locator.scheme == "s2":
            metadata.identifiers.setdefault("s2", locator.value)
        return self._persist(metadata, original_locator=locator)

    def _request_json(self, provider: str, cache_key: str, url: str) -> dict[str, Any]:
        cached = self.database.get_provider_cache(
            provider, cache_key, max_age_seconds=PROVIDER_CACHE_SECONDS
        )
        if cached is not None:
            return cached
        configured_delay = self.request_delay
        if configured_delay is None:
            configured_delay = 1.0 if provider == "semantic-scholar" else 0.1
        delay = configured_delay
        last = self._last_request_at.get(provider)
        if last is not None:
            delay = max(0.0, configured_delay - (time.monotonic() - last))
        if delay:
            self.sleep(delay)
        error: PaperImportError | None = None
        for attempt in range(self.retries + 1):
            try:
                document = self.fetcher(
                    url, {"Accept": "application/json"}, MAX_PROVIDER_BYTES, self.timeout
                )
                self._last_request_at[provider] = time.monotonic()
                payload = json.loads(document)
                if not isinstance(payload, dict):
                    raise PaperFetchError(f"{provider} returned an invalid response")
                return payload
            except (json.JSONDecodeError, UnicodeDecodeError) as caught:
                error = PaperFetchError(f"{provider} returned invalid JSON")
                break
            except PaperNotFoundError as caught:
                raise caught
            except PaperFetchError as caught:
                error = caught
                if not caught.transient or attempt == self.retries:
                    break
                self.sleep(min(2 ** attempt, 4))
        assert error is not None
        raise error

    def _semantic_scholar(self, locator: PaperLocator) -> PaperMetadata:
        prefix = {"doi": "DOI:", "arxiv": "ARXIV:", "s2": ""}[locator.scheme]
        paper_id = prefix + locator.value
        fields = (
            "paperId,externalIds,url,title,abstract,authors,publicationDate,year,"
            "fieldsOfStudy,references.paperId,references.externalIds,"
            "references.url,references.title"
        )
        url = f"{SEMANTIC_SCHOLAR_API}{quote(paper_id, safe='')}?fields={fields}"
        # Version the cache key because older Herald responses were requested
        # without references and would otherwise suppress citation enrichment.
        cache_key = f"{paper_id.lower()}:references-v1"
        payload = self._request_json("semantic-scholar", cache_key, url)
        title = _clean_text(payload.get("title"))
        if not title:
            raise PaperNotFoundError("Semantic Scholar did not return paper metadata")
        self.database.put_provider_cache("semantic-scholar", cache_key, payload)
        identifiers: dict[str, str] = {}
        external = payload.get("externalIds")
        if isinstance(external, dict):
            if external.get("DOI"):
                try:
                    identifiers["doi"] = normalize_doi(str(external["DOI"]))
                except ValueError:
                    pass
            if external.get("ArXiv"):
                try:
                    identifiers["arxiv"] = normalize_arxiv_id(str(external["ArXiv"]))
                except ValueError:
                    pass
        paper_id_value = str(payload.get("paperId") or "").strip().lower()
        if paper_id_value:
            identifiers["s2"] = paper_id_value
        authors = payload.get("authors")
        author_names = [
            _clean_text(item.get("name"))
            for item in authors if isinstance(item, dict) and _clean_text(item.get("name"))
        ] if isinstance(authors, list) else []
        topics = payload.get("fieldsOfStudy")
        topic_names = [_clean_text(item) for item in topics if _clean_text(item)] if isinstance(topics, list) else []
        references = self._semantic_scholar_references(payload.get("references"))
        return PaperMetadata(
            title=title,
            authors=author_names,
            abstract=_clean_text(payload.get("abstract")),
            published_at=_normalize_date(payload.get("publicationDate"), year=payload.get("year")),
            url=_clean_text(payload.get("url")),
            identifiers=identifiers,
            topics=topic_names,
            references=references,
            provider="semantic-scholar",
        )

    @staticmethod
    def _semantic_scholar_references(value: Any) -> list[PaperReferenceMetadata]:
        results: list[PaperReferenceMetadata] = []
        for raw in value if isinstance(value, list) else []:
            if not isinstance(raw, dict):
                continue
            item = raw.get("citedPaper") if isinstance(raw.get("citedPaper"), dict) else raw
            identifiers: dict[str, str] = {}
            external = item.get("externalIds")
            if isinstance(external, dict):
                if external.get("DOI"):
                    try:
                        identifiers["doi"] = normalize_doi(str(external["DOI"]))
                    except ValueError:
                        pass
                if external.get("ArXiv"):
                    try:
                        identifiers["arxiv"] = normalize_arxiv_id(str(external["ArXiv"]))
                    except ValueError:
                        pass
            paper_id = str(item.get("paperId") or "").strip().lower()
            if S2_ID_RE.fullmatch(paper_id):
                identifiers["s2"] = paper_id
            title = _clean_text(item.get("title"))
            url = _clean_text(item.get("url"))
            if not url and identifiers.get("doi"):
                url = f"https://doi.org/{identifiers['doi']}"
            elif not url and identifiers.get("arxiv"):
                url = f"https://arxiv.org/abs/{identifiers['arxiv']}"
            if identifiers or url:
                results.append(PaperReferenceMetadata(title, url, identifiers))
        return results

    def _crossref(self, doi: str) -> PaperMetadata:
        url = CROSSREF_API + quote(doi, safe="")
        payload = self._request_json("crossref", doi, url)
        message = payload.get("message")
        if not isinstance(message, dict):
            raise PaperNotFoundError("Crossref did not return paper metadata")
        titles = message.get("title")
        title = _clean_text(titles[0]) if isinstance(titles, list) and titles else ""
        if not title:
            raise PaperNotFoundError("Crossref did not return a paper title")
        self.database.put_provider_cache("crossref", doi, payload)
        authors: list[str] = []
        author_items = message.get("author")
        for author in author_items if isinstance(author_items, list) else []:
            if not isinstance(author, dict):
                continue
            name = _clean_text(" ".join(filter(None, [str(author.get("given", "")), str(author.get("family", ""))])))
            if name:
                authors.append(name)
        published_at: str | None = None
        date_source = message.get("published") or message.get("published-print") or message.get("published-online")
        if isinstance(date_source, dict):
            parts = date_source.get("date-parts")
            if isinstance(parts, list) and parts and isinstance(parts[0], list) and parts[0]:
                try:
                    numbers = [int(value) for value in parts[0][:3]]
                    while len(numbers) < 3:
                        numbers.append(1)
                    published_at = datetime(*numbers, tzinfo=UTC).isoformat()
                except (TypeError, ValueError):
                    pass
        subjects = message.get("subject")
        references: list[PaperReferenceMetadata] = []
        raw_references = message.get("reference")
        for raw_reference in raw_references if isinstance(raw_references, list) else []:
            if not isinstance(raw_reference, dict):
                continue
            identifiers: dict[str, str] = {}
            doi_value = raw_reference.get("DOI") or raw_reference.get("doi")
            if doi_value:
                try:
                    identifiers["doi"] = normalize_doi(str(doi_value))
                except ValueError:
                    pass
            title_value = _clean_text(
                raw_reference.get("article-title") or raw_reference.get("volume-title")
            )
            if identifiers:
                references.append(PaperReferenceMetadata(
                    title=title_value,
                    url=f"https://doi.org/{identifiers['doi']}",
                    identifiers=identifiers,
                ))
        return PaperMetadata(
            title=title,
            authors=authors,
            abstract=_clean_text(re.sub(r"<[^>]+>", " ", str(message.get("abstract") or ""))),
            published_at=published_at,
            url=_clean_text(message.get("URL")) or f"https://doi.org/{doi}",
            identifiers={"doi": doi},
            topics=[_clean_text(item) for item in subjects if _clean_text(item)]
            if isinstance(subjects, list) else [],
            references=references,
            provider="crossref",
        )

    @staticmethod
    def _merge_metadata(primary: PaperMetadata, fallback: PaperMetadata) -> PaperMetadata:
        return PaperMetadata(
            title=primary.title or fallback.title,
            authors=primary.authors or fallback.authors,
            abstract=primary.abstract or fallback.abstract,
            published_at=primary.published_at or fallback.published_at,
            url=primary.url or fallback.url,
            identifiers={**fallback.identifiers, **primary.identifiers},
            topics=list(dict.fromkeys(primary.topics + fallback.topics)),
            supplied_keywords=list(dict.fromkeys(primary.supplied_keywords + fallback.supplied_keywords)),
            references=primary.references or fallback.references,
            provider=primary.provider,
        )

    @staticmethod
    def _reference_identity(
        reference: PaperReferenceMetadata,
    ) -> tuple[str, str, str]:
        for scheme in ("doi", "arxiv", "s2"):
            value = reference.identifiers.get(scheme, "").strip()
            if value:
                return f"{scheme}:{value}", scheme, value
        if reference.url.strip():
            return f"url:{reference.url.strip()}", "", ""
        raise ValueError("A paper reference needs an identifier or URL")

    @staticmethod
    def _identity(metadata: PaperMetadata, original_locator: PaperLocator) -> tuple[str, str, str]:
        if metadata.identifiers.get("doi"):
            value = metadata.identifiers["doi"]
            return f"doi:{value}", f"https://doi.org/{value}", f"doi:{value}"
        if metadata.identifiers.get("arxiv"):
            value = metadata.identifiers["arxiv"]
            return f"arxiv:{value}", f"https://arxiv.org/abs/{value}", f"arxiv:{value}"
        if metadata.identifiers.get("s2"):
            value = metadata.identifiers["s2"]
            return f"semantic-scholar:{value}", metadata.url, f"s2:{value}"
        page_url = metadata.url or original_locator.page_url
        return f"url:{page_url}", page_url, f"url:{page_url}"

    def _persist(self, metadata: PaperMetadata, *, original_locator: PaperLocator) -> PaperImportResult:
        guid, canonical_url, canonical_key = self._identity(metadata, original_locator)
        matches: dict[int, dict[str, Any]] = {}
        for scheme, value in metadata.identifiers.items():
            match = self.database.find_entry_by_identifier(scheme, value)
            if match is not None:
                matches[int(match["id"])] = match
        canonical_match = self.database.find_entry_by_canonical_key(canonical_key)
        if canonical_match is not None:
            matches[int(canonical_match["id"])] = canonical_match
        candidate_urls = {canonical_url, original_locator.page_url}
        arxiv_id = metadata.identifiers.get("arxiv")
        if arxiv_id:
            candidate_urls.update({
                f"https://arxiv.org/abs/{arxiv_id}",
                f"http://arxiv.org/abs/{arxiv_id}",
            })
        for candidate_url in candidate_urls:
            match = self.database.find_entry_by_url(candidate_url)
            if match is not None:
                matches[int(match["id"])] = match
        if len(matches) > 1:
            raise PaperImportError(
                "Paper identifiers match multiple Herald entries; resolve the duplicate before importing"
            )
        existing = next(iter(matches.values()), None)
        created = False
        if existing is None:
            source_id = self.database.add_source(
                "Manual Imports", MANUAL_SOURCE_URL, "Manual Imports",
                content_kind="paper", adapter="manual",
            )
            entry_id, created = self.database.upsert_entry(
                source_id=source_id,
                guid=guid,
                url=canonical_url,
                canonical_url=canonical_url,
                canonical_key=canonical_key,
                title=metadata.title,
                author=", ".join(metadata.authors),
                published_at=metadata.published_at,
                content=metadata.abstract,
                summary=deterministic_summary(metadata.title, metadata.abstract),
                summary_provider="extractive",
                content_kind="paper",
            )
        else:
            entry_id = int(existing["id"])
            self.database.update_entry_metadata(
                entry_id,
                title=metadata.title,
                author=", ".join(metadata.authors),
                published_at=metadata.published_at,
                content=metadata.abstract,
                canonical_url=canonical_url,
                canonical_key=canonical_key,
            )

        primary_scheme = canonical_key.split(":", 1)[0]
        if primary_scheme == "s2":
            primary_scheme = "s2"
        for scheme, identifier in metadata.identifiers.items():
            self.database.add_paper_identifier(
                entry_id, scheme, identifier, is_primary=scheme == primary_scheme
            )
        for position, reference in enumerate(metadata.references, start=1):
            try:
                reference_key, external_scheme, external_id = self._reference_identity(
                    reference
                )
            except ValueError:
                continue
            self.database.upsert_paper_reference(
                entry_id,
                reference_key,
                external_scheme=external_scheme,
                external_id=external_id,
                cited_title=reference.title,
                cited_url=reference.url,
                position=position,
                provider=metadata.provider,
            )
        self.database.reconcile_paper_references()
        keyword_rows: list[dict[str, Any]] = []
        existing_values: set[str] = set()
        for keyword in metadata.supplied_keywords:
            normalized_keyword = keyword.lower()
            if normalized_keyword not in existing_values and len(keyword_rows) < 8:
                keyword_rows.append({
                    "keyword": keyword, "kind": "keyword", "score": None,
                    "provider": "page-meta",
                })
                existing_values.add(normalized_keyword)
        for keyword in extract_keywords(metadata.title, metadata.abstract):
            normalized_keyword = str(keyword["keyword"]).lower()
            if normalized_keyword not in existing_values and len(keyword_rows) < 8:
                keyword_rows.append(keyword)
                existing_values.add(normalized_keyword)
        for topic in metadata.topics:
            keyword_rows.append({
                "keyword": topic, "kind": "topic", "score": None,
                "provider": metadata.provider,
            })
        self.database.replace_entry_keywords(entry_id, keyword_rows)
        self.database.set_enrichment_state(
            entry_id,
            "enriched",
            provider=metadata.provider,
            canonical_url=canonical_url,
            canonical_key=canonical_key,
        )
        entry = self.database.get_entry(entry_id)
        assert entry is not None
        return PaperImportResult(
            entry=entry,
            created=created,
            identifiers=self.database.list_paper_identifiers(entry_id),
            keywords=self.database.list_entry_keywords(entry_id),
            references=self.database.list_paper_references(entry_id),
        )
