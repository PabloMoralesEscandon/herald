from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from herald.papers import (
    MAX_PAPER_PAGE_BYTES,
    PaperFetchError,
    PaperImporter,
    PaperNotFoundError,
    UnsafePaperUrlError,
    extract_keywords,
    fetch_public_document,
    parse_paper_locator,
)
from herald.service import HeraldService
from herald.storage import Database


S2_PAPER = {
    "paperId": "0123456789abcdef0123456789abcdef01234567",
    "externalIds": {"DOI": "10.1145/Example.1", "ArXiv": "2607.07570v2"},
    "url": "https://www.semanticscholar.org/paper/example",
    "title": "A Reliable Chiplet Interconnect",
    "abstract": "We present a reliable chiplet interconnect for digital systems. The design reduces latency.",
    "authors": [{"name": "Ada Researcher"}, {"name": "Lin Scientist"}],
    "publicationDate": "2026-07-15",
    "year": 2026,
    "fieldsOfStudy": ["Computer Science", "Engineering"],
}

CROSSREF_PAPER = {
    "status": "ok",
    "message": {
        "DOI": "10.5555/fallback",
        "title": ["Fallback Metadata for a Processor"],
        "abstract": "<jats:p>A processor design abstract.</jats:p>",
        "author": [{"given": "Grace", "family": "Hopper"}],
        "published": {"date-parts": [[2025, 3, 2]]},
        "subject": ["Computer architecture"],
        "URL": "https://doi.org/10.5555/fallback",
    },
}

PAPER_PAGE = b"""<!doctype html><html><head>
<meta name="citation_title" content="Page Metadata Title">
<meta name="citation_author" content="First Author">
<meta name="citation_author" content="Second Author">
<meta name="citation_doi" content="10.1145/example.1">
<meta name="citation_abstract" content="Page abstract used as a fallback.">
<meta name="citation_keywords" content="chiplets; interconnects">
<link rel="canonical" href="https://papers.example.org/work/1">
</head></html>"""


class FixtureFetcher:
    def __init__(self, *, s2: object = S2_PAPER, crossref: object = CROSSREF_PAPER):
        self.s2 = s2
        self.crossref = crossref
        self.calls: list[tuple[str, int, float]] = []

    def __call__(self, url: str, headers: object, max_bytes: int, timeout: float) -> bytes:
        self.calls.append((url, max_bytes, timeout))
        if url in {
            "https://papers.example.org/work/1",
            "https://doi.org/10.1145/EXAMPLE.1",
        }:
            return PAPER_PAGE
        response = self.crossref if "api.crossref.org" in url else self.s2
        if isinstance(response, Exception):
            raise response
        return json.dumps(response).encode()


class PaperImportTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.database = Database(Path(self.temporary_directory.name) / "herald.db")
        self.database.initialize()

    def importer(self, fetcher: FixtureFetcher) -> PaperImporter:
        return PaperImporter(
            self.database,
            fetcher=fetcher,
            sleep=lambda _: None,
            request_delay=0,
            retries=1,
        )

    def test_resolves_supported_identifiers_and_rejects_unsafe_urls(self) -> None:
        self.assertEqual(parse_paper_locator("DOI: 10.1145/EXAMPLE.1").value, "10.1145/example.1")
        self.assertEqual(parse_paper_locator("https://arxiv.org/pdf/2607.07570v3.pdf").value, "2607.07570")
        semantic = parse_paper_locator(
            "https://www.semanticscholar.org/paper/title/0123456789abcdef0123456789abcdef01234567"
        )
        self.assertEqual((semantic.scheme, semantic.value), ("s2", "0123456789abcdef0123456789abcdef01234567"))
        with self.assertRaises(UnsafePaperUrlError):
            parse_paper_locator("https://127.0.0.1/paper")
        with self.assertRaises(ValueError):
            parse_paper_locator("http://example.org/paper")
        with self.assertRaises(ValueError):
            parse_paper_locator("not a paper")

    def test_imports_doi_with_s2_metadata_keywords_topics_and_unread_status(self) -> None:
        fetcher = FixtureFetcher()
        result = self.importer(fetcher).import_paper("10.1145/example.1")

        self.assertTrue(result.created)
        self.assertEqual(result.entry["status"], "unread")
        self.assertEqual(result.entry["source_title"], "Manual Imports")
        self.assertEqual(result.entry["source_adapter"], "manual")
        self.assertEqual(result.entry["title"], S2_PAPER["title"])
        self.assertEqual(result.entry["author"], "Ada Researcher, Lin Scientist")
        self.assertEqual(result.entry["canonical_key"], "doi:10.1145/example.1")
        self.assertEqual(result.entry["enrichment_provider"], "semantic-scholar")
        self.assertEqual(
            {(item["scheme"], item["value"]) for item in result.identifiers},
            {
                ("doi", "10.1145/example.1"),
                ("arxiv", "2607.07570"),
                ("s2", "0123456789abcdef0123456789abcdef01234567"),
            },
        )
        self.assertIn("topic", {item["kind"] for item in result.keywords})
        self.assertLessEqual(
            len([item for item in result.keywords if item["kind"] == "keyword"]), 8
        )

    def test_repeat_import_uses_persistent_cache_and_preserves_triage(self) -> None:
        first_fetcher = FixtureFetcher()
        first = self.importer(first_fetcher).import_paper("10.1145/example.1")
        self.database.set_status(first.entry["id"], "kept")

        second_fetcher = FixtureFetcher(s2=AssertionError("provider should be cached"))
        second = self.importer(second_fetcher).import_paper("https://doi.org/10.1145/EXAMPLE.1")

        self.assertFalse(second.created)
        self.assertEqual(second.entry["id"], first.entry["id"])
        self.assertEqual(second.entry["status"], "kept")
        self.assertEqual(second_fetcher.calls, [])
        self.assertEqual(len(self.database.list_entries(limit=None)), 1)

    def test_manual_arxiv_import_enriches_existing_feed_entry_without_duplication(self) -> None:
        source_id = self.database.add_source(
            "arXiv", "https://rss.arxiv.org/rss/cs.AR", "Chip Design"
        )
        entry_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="http://arxiv.org/abs/2607.07570",
            url="http://arxiv.org/abs/2607.07570",
            title="Feed title",
        )
        self.database.set_status(entry_id, "read")

        result = self.importer(FixtureFetcher()).import_paper("2607.07570v4")

        self.assertFalse(result.created)
        self.assertEqual(result.entry["id"], entry_id)
        self.assertEqual(result.entry["source_title"], "arXiv")
        self.assertEqual(result.entry["status"], "read")
        self.assertEqual(result.entry["enrichment_status"], "enriched")
        self.assertEqual(len(self.database.list_entries(limit=None)), 1)

    def test_manual_import_source_is_not_treated_as_a_refreshable_feed(self) -> None:
        self.importer(FixtureFetcher()).import_paper("10.1145/example.1")
        attempted: list[str] = []

        def fetch_feed(url: str) -> bytes:
            attempted.append(url)
            raise AssertionError("manual sources are not feeds")

        self.assertEqual(HeraldService(self.database, fetcher=fetch_feed).refresh_all(), [])
        self.assertEqual(attempted, [])

    def test_crossref_is_free_fallback_when_semantic_scholar_has_no_record(self) -> None:
        fetcher = FixtureFetcher(s2=PaperNotFoundError("not found"))
        result = self.importer(fetcher).import_paper("10.5555/fallback")

        self.assertEqual(result.entry["title"], "Fallback Metadata for a Processor")
        self.assertEqual(result.entry["enrichment_provider"], "crossref")
        self.assertEqual(result.entry["published_at"], "2025-03-02T00:00:00+00:00")
        self.assertEqual(sum("api.crossref.org" in call[0] for call in fetcher.calls), 1)

    def test_public_page_discovers_doi_then_merges_citation_keywords(self) -> None:
        fetcher = FixtureFetcher()
        result = self.importer(fetcher).import_paper("https://papers.example.org/work/1")

        self.assertEqual(result.entry["title"], S2_PAPER["title"])
        self.assertEqual(result.entry["canonical_url"], "https://doi.org/10.1145/example.1")
        self.assertIn("chiplets", {item["keyword"] for item in result.keywords})
        self.assertLessEqual(
            len([item for item in result.keywords if item["kind"] == "keyword"]), 8
        )
        page_call = fetcher.calls[0]
        self.assertEqual(page_call[1], MAX_PAPER_PAGE_BYTES)
        self.assertLessEqual(page_call[2], 30)

    def test_inspection_reads_a_recognized_paper_url_before_provider_lookup(self) -> None:
        fetcher = FixtureFetcher()

        result = self.importer(fetcher).import_paper(
            "https://doi.org/10.1145/EXAMPLE.1",
            inspect_page=True,
        )

        self.assertEqual(fetcher.calls[0][0], "https://doi.org/10.1145/EXAMPLE.1")
        self.assertEqual(fetcher.calls[0][1], MAX_PAPER_PAGE_BYTES)
        self.assertIn("chiplets", {item["keyword"] for item in result.keywords})

    def test_provider_failure_retries_then_leaves_database_unchanged(self) -> None:
        fetcher = FixtureFetcher(s2=PaperFetchError("offline", transient=True))
        with self.assertRaises(PaperFetchError):
            self.importer(fetcher).import_paper("arXiv:2607.07570")
        self.assertEqual(len(fetcher.calls), 2)
        self.assertEqual(self.database.list_entries(limit=None), [])

    def test_keyword_extraction_is_deterministic_and_bounded(self) -> None:
        first = extract_keywords("Chip design accelerator", "Chip design improves accelerator latency.")
        self.assertEqual(first, extract_keywords("Chip design accelerator", "Chip design improves accelerator latency."))
        self.assertLessEqual(len(first), 8)
        self.assertTrue(all(item["provider"] == "deterministic-tfidf" for item in first))


class SafeFetcherTests(unittest.TestCase):
    def test_dns_resolution_to_private_address_is_rejected_before_open(self) -> None:
        with patch("herald.papers.socket.getaddrinfo", return_value=[(2, 1, 6, "", ("127.0.0.1", 443))]):
            with self.assertRaises(UnsafePaperUrlError):
                fetch_public_document("https://papers.invalid/work", {}, 100, 1)

    def test_declared_oversized_response_is_rejected(self) -> None:
        class Response:
            headers = {"Content-Length": "101"}
            def __enter__(self) -> "Response": return self
            def __exit__(self, *args: object) -> None: return None
            def geturl(self) -> str: return "https://example.org/paper"
            def read(self, amount: int) -> bytes: return b"unused"

        class Opener:
            def open(self, request: object, timeout: float) -> Response: return Response()

        with (
            patch("herald.papers.socket.getaddrinfo", return_value=[(2, 1, 6, "", ("93.184.216.34", 443))]),
            patch("herald.papers.urllib.request.build_opener", return_value=Opener()),
        ):
            with self.assertRaises(PaperFetchError):
                fetch_public_document("https://example.org/paper", {}, 100, 1)


if __name__ == "__main__":
    unittest.main()
