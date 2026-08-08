from __future__ import annotations

import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path
from unittest.mock import Mock, patch

from herald.feeds import (
    FeedParseError,
    canonicalize_url,
    discover_feed_url,
    parse_feed,
)
from herald.service import FeedResponse, HeraldService
from herald.sources import CURATED_SOURCES, NEWS_SOURCES, RESEARCH_SOURCES
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


NEWS_FEED = b"""<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
  <channel>
    <title>Official Company Newsroom</title>
    <item>
      <guid>launch-recent</guid>
      <title>Company launches a new accelerator</title>
      <link>https://example.com/news/accelerator?utm_source=mail&amp;edition=global#top</link>
      <pubDate>Fri, 07 Aug 2026 09:00:00 GMT</pubDate>
      <description><![CDATA[<p>A faster accelerator is now available.</p>]]></description>
      <content:encoded><![CDATA[<p>Official launch details for developers.</p>]]></content:encoded>
    </item>
    <item>
      <guid>launch-old</guid>
      <title>Company launches an older product</title>
      <link>https://example.com/news/older</link>
      <pubDate>Thu, 01 Jan 2026 09:00:00 GMT</pubDate>
      <description>Older official announcement.</description>
    </item>
  </channel>
</rss>"""


class RecordingRelevance:
    def __init__(self) -> None:
        self.started: list[str] = []

    def start(self, content_kind: str) -> dict[str, str]:
        self.started.append(content_kind)
        return {"content_kind": content_kind}


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

    def test_canonicalizes_tracking_without_losing_meaningful_query_data(self) -> None:
        self.assertEqual(
            canonicalize_url(
                "HTTPS://Example.COM:443/news?id=7&utm_medium=email&lang=en#top"
            ),
            "https://example.com/news?id=7&lang=en",
        )

    def test_discovers_standard_relative_atom_link_without_scraping(self) -> None:
        document = b"""<html><head>
          <link rel="alternate" type="application/atom+xml" href="/updates.atom">
        </head><body><a href="/not-a-feed">News</a></body></html>"""
        self.assertEqual(
            discover_feed_url(document, "https://example.com/news"),
            "https://example.com/updates.atom",
        )


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
        self.assertEqual(len(sources), 15)
        self.assertEqual(
            {source["category"] for source in sources},
            {
                "Chip Design & Digital Circuits",
                "Machine Learning",
                "Operating Systems",
                "Reinforcement Learning",
                "NVIDIA",
                "OpenAI",
                "AMD",
                "Intel",
            },
        )
        self.assertEqual(
            sum(source["content_kind"] == "paper" for source in sources),
            len(RESEARCH_SOURCES),
        )
        self.assertEqual(
            sum(source["content_kind"] == "news" for source in sources),
            len(NEWS_SOURCES),
        )

    def test_local_backfill_preserves_status_and_is_idempotent(self) -> None:
        paper_source = self.database.add_source(
            "Legacy arXiv", "https://example.org/papers.xml", "Machine Learning"
        )
        paper_id, _ = self.database.upsert_entry(
            source_id=paper_source,
            guid="legacy-paper",
            url="https://arxiv.org/abs/2608.01234v2",
            title="Reinforcement Learning Accelerator Design",
            content="A digital circuit for efficient policy optimization.",
        )
        self.database.set_status(paper_id, "kept")
        news_source = self.database.add_source(
            "Official News",
            "https://example.org/news.xml",
            "Company",
            content_kind="news",
        )
        news_id, _ = self.database.upsert_entry(
            source_id=news_source,
            guid="legacy-news",
            url="https://example.org/launch",
            title="Company launches an inference chip",
            content_kind="news",
        )
        self.database.set_status(news_id, "discarded")
        service = HeraldService(self.database)

        first = service.backfill_existing(sync_obsidian=False)
        second = service.backfill_existing(sync_obsidian=False)

        self.assertEqual(self.database.get_entry(paper_id)["status"], "kept")
        self.assertEqual(self.database.get_entry(news_id)["status"], "discarded")
        self.assertEqual(self.database.get_entry(paper_id)["canonical_key"], "arxiv:2608.01234")
        self.assertEqual(
            self.database.list_paper_identifiers(paper_id)[0]["value"],
            "2608.01234",
        )
        self.assertTrue(self.database.list_entry_keywords(paper_id))
        self.assertTrue(self.database.list_entry_keywords(news_id))
        self.assertEqual(first["identifiers_added"], 1)
        self.assertEqual(second["identifiers_added"], 0)
        self.assertEqual(second["canonical_keys_added"], 0)
        self.assertEqual(second["keywords_added"], 0)
        self.assertEqual(first["rankings"]["paper"]["scored"], 1)
        self.assertEqual(first["rankings"]["news"]["scored"], 1)

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
        self.assertTrue(self.database.list_entries()[0]["summary"])

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
        bad = next(source for source in self.database.list_sources() if source["title"] == "Bad")
        good = next(source for source in self.database.list_sources() if source["title"] == "Good")
        self.assertTrue(bad["refresh_attempted_at"])
        self.assertTrue(bad["refresh_error"])
        self.assertTrue(good["refresh_succeeded_at"])
        self.assertEqual(good["refresh_error"], "")

    def test_news_bootstraps_thirty_days_then_accepts_every_observed_item(self) -> None:
        source_id = self.database.add_source(
            "Official News",
            "https://example.com/feed.xml",
            "Company",
            content_kind="news",
        )
        ranking = RecordingRelevance()
        delayed_feed = NEWS_FEED.replace(b"launch-old", b"delayed-old")
        documents = iter((NEWS_FEED, NEWS_FEED, delayed_feed))
        service = HeraldService(
            self.database,
            fetcher=lambda _: next(documents),
            relevance=ranking,  # type: ignore[arg-type]
            now=lambda: datetime(2026, 8, 8, tzinfo=UTC),
        )
        source = self.database.get_source(source_id)
        assert source is not None

        first = service.refresh_source(source)
        second = service.refresh_source(source)
        third = service.refresh_source(source)

        self.assertEqual((first.fetched, first.created), (2, 1))
        self.assertEqual((second.created, second.updated), (0, 1))
        self.assertEqual((third.created, third.updated), (1, 1))
        self.assertEqual(len(self.database.list_entries(content_kind="news")), 2)
        self.assertEqual(ranking.started, ["news", "news"])
        self.assertEqual(
            self.database.list_source_bootstrap_skips(source_id), {"launch-old"}
        )
        recent = next(
            entry
            for entry in self.database.list_entries()
            if entry["guid"] == "launch-recent"
        )
        self.assertEqual(
            recent["canonical_url"],
            "https://example.com/news/accelerator?edition=global",
        )

    def test_autodiscovery_is_cached_and_feed_validators_are_persisted(self) -> None:
        page = b"""<html><head><link rel="alternate" type="application/rss+xml"
                    href="/official.xml"></head></html>"""
        requested: list[str] = []

        def fetcher(url: str) -> bytes | FeedResponse:
            requested.append(url)
            if url.endswith("/news"):
                return page
            return FeedResponse(
                NEWS_FEED,
                "https://example.com/official.xml",
                etag='"news-v1"',
                last_modified="Fri, 07 Aug 2026 12:00:00 GMT",
            )

        source_id = self.database.add_source(
            "Official News",
            "https://example.com/news",
            "Company",
            content_kind="news",
        )
        service = HeraldService(
            self.database,
            fetcher=fetcher,
            relevance=RecordingRelevance(),  # type: ignore[arg-type]
            now=lambda: datetime(2026, 8, 8, tzinfo=UTC),
        )
        source = self.database.get_source(source_id)
        assert source is not None

        service.refresh_source(source)
        refreshed = self.database.get_source(source_id)
        assert refreshed is not None
        self.assertEqual(refreshed["resolved_url"], "https://example.com/official.xml")
        self.assertEqual(refreshed["etag"], '"news-v1"')

        service.refresh_source(source)
        self.assertEqual(
            requested,
            [
                "https://example.com/news",
                "https://example.com/official.xml",
                "https://example.com/official.xml",
            ],
        )

    def test_conditional_not_modified_is_a_successful_no_op(self) -> None:
        source_id = self.database.add_source(
            "Official News",
            "https://example.com/official.xml",
            "Company",
            content_kind="news",
        )
        responses = [
            FeedResponse(NEWS_FEED, "https://example.com/official.xml", etag='"v1"'),
            FeedResponse(
                b"",
                "https://example.com/official.xml",
                etag='"v1"',
                not_modified=True,
            ),
        ]
        with patch("herald.service.fetch_feed", new=Mock(side_effect=responses)) as mocked:
            service = HeraldService(
                self.database,
                relevance=RecordingRelevance(),  # type: ignore[arg-type]
                now=lambda: datetime(2026, 8, 8, tzinfo=UTC),
            )
            source = self.database.get_source(source_id)
            assert source is not None
            service.refresh_source(source)
            result = service.refresh_source(source)

        self.assertTrue(result.not_modified)
        self.assertEqual((result.fetched, result.created, result.updated), (0, 0, 0))
        self.assertEqual(mocked.call_args_list[1].kwargs["etag"], '"v1"')
        refreshed = self.database.get_source(source_id)
        assert refreshed is not None
        self.assertEqual(refreshed["refresh_error"], "")

    def test_canonical_url_deduplicates_feeds_and_preserves_triage(self) -> None:
        first_id = self.database.add_source(
            "News one", "https://example.com/one.xml", "Company", content_kind="news"
        )
        second_id = self.database.add_source(
            "News two", "https://example.com/two.xml", "Company", content_kind="news"
        )
        duplicate_feed = NEWS_FEED.replace(
            b"launch-recent", b"other-guid"
        ).replace(
            b"utm_source=mail&amp;edition=global",
            b"edition=global&amp;fbclid=tracker",
        ).replace(
            b"<item>\n      <guid>launch-old</guid>",
            b"<!-- omitted old item <item>\n      <guid>launch-old</guid>",
        ).replace(b"</channel>", b"-->\n  </channel>")
        feeds = {
            "https://example.com/one.xml": NEWS_FEED,
            "https://example.com/two.xml": duplicate_feed,
        }
        service = HeraldService(
            self.database,
            fetcher=lambda url: feeds[url],
            relevance=RecordingRelevance(),  # type: ignore[arg-type]
            now=lambda: datetime(2026, 8, 8, tzinfo=UTC),
        )
        first = self.database.get_source(first_id)
        second = self.database.get_source(second_id)
        assert first is not None and second is not None
        service.refresh_source(first)
        entry = self.database.list_entries()[0]
        self.database.set_status(int(entry["id"]), "discarded")

        result = service.refresh_source(second)

        self.assertEqual((result.created, result.updated), (0, 1))
        entries = self.database.list_entries()
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0]["status"], "discarded")

    def test_add_source_validates_user_input(self) -> None:
        service = HeraldService(self.database)
        with self.assertRaises(ValueError):
            service.add_source("", "https://example.org/feed")
        with self.assertRaises(ValueError):
            service.add_source("Feed", "file:///tmp/feed.xml")


if __name__ == "__main__":
    unittest.main()
