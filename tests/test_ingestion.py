from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from herald.feeds import FeedParseError, parse_feed
from herald.service import HeraldService
from herald.sources import CURATED_SOURCES
from herald.storage import Database


RSS_FEED = b"""<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/"
     xmlns:dc="http://purl.org/dc/elements/1.1/">
  <channel>
    <title>Systems Lab</title>
    <item>
      <guid>paper-1</guid>
      <title>A &amp; B Scheduling</title>
      <link>https://example.org/papers/1#abstract</link>
      <dc:creator>Example Author</dc:creator>
      <pubDate>Thu, 06 Aug 2026 10:00:00 GMT</pubDate>
      <description><![CDATA[<p>Short version.</p>]]></description>
      <content:encoded><![CDATA[<p>Full <strong>paper</strong> abstract.</p>]]></content:encoded>
    </item>
  </channel>
</rss>"""


ATOM_FEED = b"""<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>ML Papers</title>
  <entry>
    <id>https://example.org/papers/2</id>
    <title>Learning a Controller</title>
    <updated>2026-08-07T08:30:00Z</updated>
    <author><name>Ada Researcher</name></author>
    <author><name>Lin Scientist</name></author>
    <link rel="related" href="https://example.org/papers/2.pdf" />
    <link rel="alternate" href="https://example.org/papers/2" />
    <summary type="html">&lt;p&gt;A short summary.&lt;/p&gt;</summary>
  </entry>
</feed>"""


RDF_FEED = b"""<?xml version="1.0"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
         xmlns="http://purl.org/rss/1.0/">
  <channel rdf:about="https://example.org/feed"><title>RDF feed</title></channel>
  <item rdf:about="https://example.org/rdf-item">
    <title>RDF Item</title>
    <link>https://example.org/rdf-item</link>
    <description>RDF description</description>
  </item>
</rdf:RDF>"""


class FeedParserTests(unittest.TestCase):
    def test_parses_rss_namespaces_dates_and_full_content(self) -> None:
        entries = parse_feed(RSS_FEED)

        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].guid, "paper-1")
        self.assertEqual(entries[0].title, "A & B Scheduling")
        self.assertEqual(entries[0].url, "https://example.org/papers/1")
        self.assertEqual(entries[0].author, "Example Author")
        self.assertEqual(entries[0].published_at, "2026-08-06T10:00:00+00:00")
        self.assertEqual(entries[0].content, "Full paper abstract.")

    def test_parses_atom_links_authors_and_iso_dates(self) -> None:
        entries = parse_feed(ATOM_FEED)

        self.assertEqual(entries[0].guid, "https://example.org/papers/2")
        self.assertEqual(entries[0].url, "https://example.org/papers/2")
        self.assertEqual(entries[0].author, "Ada Researcher, Lin Scientist")
        self.assertEqual(entries[0].published_at, "2026-08-07T08:30:00+00:00")
        self.assertEqual(entries[0].content, "A short summary.")

    def test_parses_rss_one_rdf_items(self) -> None:
        entries = parse_feed(RDF_FEED)
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].title, "RDF Item")

    def test_rejects_invalid_and_unsupported_documents(self) -> None:
        with self.assertRaises(FeedParseError):
            parse_feed(b"<rss>")
        with self.assertRaises(FeedParseError):
            parse_feed(b"<html></html>")


class IngestionServiceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.database = Database(Path(self.temporary_directory.name) / "herald.db")
        self.database.initialize()

    def test_curated_sources_are_complete_and_idempotent(self) -> None:
        service = HeraldService(self.database, fetcher=lambda _: ATOM_FEED)

        self.assertEqual(service.seed_curated_sources(), len(CURATED_SOURCES))
        self.assertEqual(service.seed_curated_sources(), 0)
        sources = service.list_sources()
        self.assertEqual(len(sources), 11)
        self.assertEqual(
            {source["category"] for source in sources},
            {
                "Chip Design & Digital Circuits",
                "Machine Learning",
                "Operating Systems",
                "Reinforcement Learning",
            },
        )

    def test_refresh_is_idempotent(self) -> None:
        source_id = self.database.add_source(
            "Systems Lab", "https://example.org/rss", "Operating Systems"
        )
        service = HeraldService(self.database, fetcher=lambda _: RSS_FEED)
        source = next(
            source for source in service.list_sources() if source["id"] == source_id
        )

        first = service.refresh_source(source)
        second = service.refresh_source(source)

        self.assertEqual((first.fetched, first.created, first.updated), (1, 1, 0))
        self.assertEqual((second.fetched, second.created, second.updated), (1, 0, 1))
        self.assertEqual(len(self.database.list_entries()), 1)

    def test_cross_listed_url_is_deduplicated_between_sources(self) -> None:
        source_one = self.database.add_source(
            "ML", "https://example.org/ml", "Machine Learning"
        )
        source_two = self.database.add_source(
            "RL", "https://example.org/rl", "Reinforcement Learning"
        )
        service = HeraldService(self.database, fetcher=lambda _: ATOM_FEED)
        sources = {source["id"]: source for source in service.list_sources()}

        self.assertEqual(service.refresh_source(sources[source_one]).created, 1)
        second = service.refresh_source(sources[source_two])

        self.assertEqual((second.created, second.updated), (0, 1))
        self.assertEqual(len(self.database.list_entries()), 1)

    def test_refresh_all_isolates_a_bad_feed(self) -> None:
        self.database.add_source("Bad", "https://example.org/bad", "Test")
        self.database.add_source("Good", "https://example.org/good", "Test")

        def fetcher(url: str) -> bytes:
            return b"not XML" if url.endswith("/bad") else ATOM_FEED

        results = HeraldService(self.database, fetcher=fetcher).refresh_all()

        self.assertEqual(len(results), 2)
        self.assertEqual(sum(result.created for result in results), 1)
        self.assertEqual(sum(result.error is not None for result in results), 1)

    def test_add_source_validates_user_input(self) -> None:
        service = HeraldService(self.database)
        with self.assertRaises(ValueError):
            service.add_source("", "https://example.org/feed")
        with self.assertRaises(ValueError):
            service.add_source("Feed", "file:///tmp/feed.xml")


if __name__ == "__main__":
    unittest.main()
