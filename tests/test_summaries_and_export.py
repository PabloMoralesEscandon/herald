from __future__ import annotations

import tempfile
import unittest
from contextlib import AbstractContextManager
from datetime import UTC, datetime
from json import loads
from pathlib import Path
from unittest.mock import patch

from herald.obsidian import ObsidianConflictError, ObsidianExporter
from herald.service import HeraldService
from herald.storage import Database
from herald.summaries import (
    LocalSummarizer,
    OllamaClient,
    OllamaError,
    SummaryResult,
    deterministic_summary,
)


ARTICLE = (
    "arXiv:2608.00001 Announce Type: new Abstract: "
    "The authors study scheduling for mixed CPU cores. "
    "Their method learns from offline traces. "
    "It reduces tail latency by twelve percent. "
    "A fourth sentence should not be selected."
)


class SummaryTests(unittest.TestCase):
    def test_deterministic_fallback_extracts_three_abstract_sentences(self) -> None:
        expected = (
            "The authors study scheduling for mixed CPU cores. "
            "Their method learns from offline traces. "
            "It reduces tail latency by twelve percent."
        )
        self.assertEqual(deterministic_summary("Scheduler", ARTICLE), expected)
        self.assertEqual(deterministic_summary("Scheduler", ARTICLE), expected)

    def test_ollama_summary_is_used_when_generation_succeeds(self) -> None:
        prompts: list[str] = []

        def generate(prompt: str) -> str:
            prompts.append(prompt)
            return "  A factual generated summary.  "

        result = LocalSummarizer(
            ollama_model="test-model", generator=generate
        ).summarize("Scheduler", ARTICLE)

        self.assertEqual(result.text, "A factual generated summary.")
        self.assertEqual(result.provider, "ollama")
        self.assertEqual(result.model, "test-model")
        self.assertIn("Title: Scheduler", prompts[0])

    def test_ollama_failure_reliably_uses_fallback(self) -> None:
        def unavailable(_: str) -> str:
            raise OllamaError("connection refused")

        summarizer = LocalSummarizer(generator=unavailable)
        first = summarizer.summarize("Scheduler", ARTICLE)
        second = summarizer.summarize("Scheduler", ARTICLE)

        self.assertEqual(first, second)
        self.assertEqual(first.provider, "fallback")
        self.assertIn("connection refused", first.fallback_reason or "")
        self.assertEqual(first.text, deterministic_summary("Scheduler", ARTICLE))

    def test_empty_input_has_a_stable_fallback(self) -> None:
        result = LocalSummarizer(generator=lambda _: "unused").summarize("", "")
        self.assertEqual(result.text, "No summary is available.")
        self.assertEqual(result.provider, "fallback")

    def test_ollama_client_sends_non_streaming_deterministic_request(self) -> None:
        class Response(AbstractContextManager["Response"]):
            def read(self, _: int) -> bytes:
                return b'{"response":"Local model summary."}'

            def __exit__(self, *args: object) -> None:
                return None

        with patch("urllib.request.urlopen", return_value=Response()) as urlopen:
            result = OllamaClient(
                "http://127.0.0.1:11434", "tiny-model"
            ).generate("Summarize this")

        request = urlopen.call_args.args[0]
        payload = loads(request.data)
        self.assertEqual(result, "Local model summary.")
        self.assertEqual(request.full_url, "http://127.0.0.1:11434/api/generate")
        self.assertEqual(payload["model"], "tiny-model")
        self.assertFalse(payload["stream"])
        self.assertEqual(payload["options"]["temperature"], 0)


class _FixedSummarizer:
    def summarize(self, title: str, content: str) -> SummaryResult:
        return SummaryResult(
            f"Summary of {title}: {content[:10]}", "test", "test-model"
        )


class ExportTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        root = Path(self.temporary_directory.name)
        self.database = Database(root / "herald.db")
        self.database.initialize()
        self.vault = root / "Obsidian Vault"
        self.service = HeraldService(
            self.database,
            summarizer=_FixedSummarizer(),
            exporter=ObsidianExporter(self.vault),
        )
        source_id = self.database.add_source(
            'Source: "Lab"',
            "https://example.org/feed",
            "ML: RL #research",
        )
        self.entry_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="unsafe-entry",
            url="https://example.org/article?one=1&two=2",
            title='A: "quoted" / unsafe\n# title',
            author='Ada "A"\nResearcher',
            published_at="2026-08-07T10:00:00+00:00",
            content="Line one.\n\nLine two with **Markdown**.",
        )

    def test_service_persists_generated_summary(self) -> None:
        result = self.service.summarize_entry(self.entry_id)
        entry = self.database.get_entry(self.entry_id)

        self.assertEqual(result.provider, "test")
        self.assertEqual(entry["summary"], result.text)
        self.assertEqual(entry["summary_provider"], "test")
        self.assertEqual(entry["summary_model"], "test-model")
        self.assertTrue(entry["summary_generated_at"])

    def test_only_kept_entries_can_be_exported(self) -> None:
        with self.assertRaisesRegex(ValueError, "Only kept"):
            self.service.export_entry(self.entry_id)
        self.assertFalse(self.vault.exists())

    def test_markdown_export_escapes_metadata_and_unsafe_filename(self) -> None:
        self.database.set_status(self.entry_id, "kept")
        self.service.summarize_entry(self.entry_id)

        path = self.service.export_entry(self.entry_id)
        document = path.read_text(encoding="utf-8")

        self.assertTrue(path.is_relative_to(self.vault))
        self.assertNotIn(":", path.name)
        self.assertNotIn("/", path.name)
        self.assertNotIn("#", path.name)
        self.assertIn('title: "A: \\"quoted\\" / unsafe\\n# title"', document)
        self.assertIn('author: "Ada \\"A\\"\\nResearcher"', document)
        self.assertIn('category: "ML: RL #research"', document)
        self.assertIn('summary_provider: "test"', document)
        self.assertIn('summary_model: "test-model"', document)
        self.assertIn("# A: \"quoted\" / unsafe # title", document)
        self.assertIn("> Method: test (test-model)", document)
        self.assertIn("> Summary of A:", document)
        self.assertIn("Line two with **Markdown**.", document)
        self.assertEqual(
            self.database.get_entry(self.entry_id)["exported_path"],
            path.relative_to(self.vault).as_posix(),
        )

    def test_export_automatically_converts_legacy_html_content(self) -> None:
        html_entry_id, _ = self.database.upsert_entry(
            source_id=self.database.list_sources()[0]["id"],
            guid="legacy-html",
            url="https://example.org/articles/rich",
            title="Rich article",
            content=(
                '<p>An <strong>important</strong> result. '
                '<a href="/methods">Methods</a>.</p>'
                '<ul><li>Fast</li><li>Local</li></ul>'
            ),
        )
        self.database.set_status(html_entry_id, "kept")

        path = self.service.export_entry(html_entry_id)
        document = path.read_text(encoding="utf-8")

        self.assertIn("An **important** result.", document)
        self.assertIn("[Methods](<https://example.org/methods>)", document)
        self.assertIn("- Fast\n- Local", document)
        self.assertNotIn("<strong>", document)

    def test_export_prefers_rich_markdown_over_plain_reader_content(self) -> None:
        rich_entry_id, _ = self.database.upsert_entry(
            source_id=self.database.list_sources()[0]["id"],
            guid="rich-markdown",
            url="https://example.org/articles/structured",
            title="Structured article",
            content="Plain reader text.",
            content_markdown="A **structured** excerpt.\n\n- First\n- Second",
        )
        self.database.set_status(rich_entry_id, "kept")

        path = self.service.export_entry(rich_entry_id)
        document = path.read_text(encoding="utf-8")

        self.assertIn("A **structured** excerpt.\n\n- First\n- Second", document)
        self.assertNotIn("Plain reader text.", document)

    def test_reexport_is_path_and_content_idempotent(self) -> None:
        self.database.set_status(self.entry_id, "kept")
        self.service.summarize_entry(self.entry_id)

        first_path = self.service.export_entry(self.entry_id)
        first_document = first_path.read_bytes()
        second_path = self.service.export_entry(self.entry_id)

        self.assertEqual(second_path, first_path)
        self.assertEqual(second_path.read_bytes(), first_document)
        self.assertEqual(len(list(self.vault.rglob("*.md"))), 1)

    def test_untrusted_existing_export_path_cannot_escape_vault(self) -> None:
        self.database.set_status(self.entry_id, "kept")
        self.database.set_exported_path(self.entry_id, "../../outside.md")

        path = self.service.export_entry(self.entry_id)

        self.assertTrue(path.is_relative_to(self.vault))
        self.assertFalse((self.vault.parent / "outside.md").exists())

    def test_export_kept_skips_entries_not_kept(self) -> None:
        self.database.set_status(self.entry_id, "kept")
        paths = self.service.export_kept()
        self.assertEqual(len(paths), 1)

    def test_keep_automatically_exports_and_reports_sync_state(self) -> None:
        entry = self.service.change_status(self.entry_id, "kept")

        self.assertEqual(entry["status"], "kept")
        self.assertEqual(entry["obsidian_export"]["state"], "synced")
        self.assertTrue((self.vault / entry["exported_path"]).is_file())
        self.assertEqual(
            entry["exported_path"], f"Herald/Papers/herald-{self.entry_id:06d}.md"
        )

    def test_news_uses_a_publisher_specific_stable_path(self) -> None:
        source_id = self.database.add_source(
            "Vendor: News",
            "https://example.org/news.xml",
            "Announcements",
            content_kind="news",
        )
        news_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="launch",
            url="https://example.org/launch",
            title="Product launch",
        )

        entry = self.service.change_status(news_id, "kept")

        self.assertEqual(
            entry["exported_path"],
            f"Herald/News/Vendor News/herald-{news_id:06d}.md",
        )
        self.assertTrue((self.vault / entry["exported_path"]).is_file())

    def test_news_note_contains_managed_metadata_and_survives_archive_restore(self) -> None:
        source_id = self.database.add_source(
            "NVIDIA Newsroom",
            "https://nvidia.example/news.xml",
            "NVIDIA",
            content_kind="news",
        )
        news_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="accelerator-launch",
            url="https://nvidia.example/launch?utm_source=email",
            canonical_url="https://nvidia.example/launch",
            title="NVIDIA launches an accelerator",
            author="NVIDIA Newsroom",
            published_at="2026-08-07T09:00:00+00:00",
            content="The accelerator is available to developers.",
            summary="A new accelerator has launched.",
            summary_provider="extractive",
            content_kind="news",
        )
        self.database.replace_entry_keywords(
            news_id,
            [
                {"keyword": "AI accelerator", "kind": "keyword", "score": 0.9},
                {"keyword": "Chip launches", "kind": "topic", "score": 0.8},
            ],
        )
        profile = self.database.get_relevance_profile("news")
        assert profile is not None
        self.database.upsert_entry_ranking(
            news_id,
            int(profile["id"]),
            score=84.25,
            bucket="relevant",
            explanation={
                "matched_interests": ["new processors GPUs accelerators"],
                "include_matches": ["launches"],
                "decision": "score meets threshold",
            },
            model="tfidf-v1",
        )

        kept = self.service.change_status(news_id, "kept")
        path = self.vault / kept["exported_path"]
        document = path.read_text(encoding="utf-8")

        self.assertEqual(
            kept["exported_path"],
            f"Herald/News/NVIDIA Newsroom/herald-{news_id:06d}.md",
        )
        self.assertIn('type: "news"', document)
        self.assertIn('publisher: "NVIDIA Newsroom"', document)
        self.assertIn('canonical_source: "https://nvidia.example/launch"', document)
        self.assertIn('keywords:\n  - "AI accelerator"', document)
        self.assertIn('topics:\n  - "Chip launches"', document)
        self.assertIn("relevance_score: 84.25", document)
        self.assertIn('relevance_bucket: "relevant"', document)
        self.assertIn('relevance_model: "tfidf-v1"', document)
        self.assertIn('  - "Matched interest: new processors GPUs accelerators"', document)
        self.assertIn('  - "Included phrase: launches"', document)
        self.assertIn('status: "kept"', document)
        self.assertIn('summary_provider: "extractive"', document)
        self.assertIn("## Announcement", document)
        self.assertIn("- Publisher: NVIDIA Newsroom", document)
        self.assertIn("- Original: <https://nvidia.example/launch>", document)
        self.assertNotIn("## References", document)
        self.assertNotIn("identifiers:", document)

        path.write_text(
            document.replace("## My Notes\n\n", "## My Notes\n\nTrack benchmarks.\n"),
            encoding="utf-8",
        )
        archived = self.service.change_status(news_id, "read")
        self.assertEqual(archived["obsidian_export"]["state"], "archived")
        self.assertFalse(path.exists())

        restored = self.service.change_status(news_id, "kept")
        restored_document = (self.vault / restored["exported_path"]).read_text(
            encoding="utf-8"
        )
        self.assertIn("Track benchmarks.", restored_document)
        self.assertNotIn("## References", restored_document)

    def test_failed_news_export_can_be_retried_without_changing_keep(self) -> None:
        source_id = self.database.add_source(
            "OpenAI News",
            "https://openai.example/news.xml",
            "OpenAI",
            content_kind="news",
        )
        news_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="model-launch",
            url="https://openai.example/model",
            title="A model launch",
            content_kind="news",
        )
        self.vault.write_text("temporarily occupied", encoding="utf-8")

        kept = self.service.change_status(news_id, "kept")

        self.assertEqual(kept["status"], "kept")
        self.assertEqual(kept["obsidian_export"]["state"], "failed")
        self.vault.unlink()
        retried = self.service.retry_obsidian(news_id)
        self.assertIsNotNone(retried)
        self.assertEqual(
            self.database.get_obsidian_export(news_id)["state"], "synced"
        )
        self.assertTrue(retried.is_file())

    def test_news_refresh_and_cross_feed_dedupe_update_one_preserved_note(self) -> None:
        initial = b"""<?xml version="1.0"?><rss version="2.0"><channel>
          <title>Vendor News</title><item><guid>launch-one</guid>
          <title>First launch title</title>
          <link>https://vendor.example/launch?utm_source=email</link>
          <pubDate>Fri, 07 Aug 2026 09:00:00 GMT</pubDate>
          <description>Initial launch details.</description>
          </item></channel></rss>"""
        refreshed = initial.replace(
            b"First launch title", b"Updated launch title"
        ).replace(b"Initial launch details.", b"Updated launch details.")
        duplicate = refreshed.replace(b"launch-one", b"different-guid").replace(
            b"?utm_source=email", b"?fbclid=tracker"
        ).replace(b"Updated launch title", b"Canonical duplicate title")
        documents = {
            "https://vendor.example/first.xml": [initial, refreshed],
            "https://vendor.example/second.xml": [duplicate],
        }

        def fetcher(url: str) -> bytes:
            return documents[url].pop(0)

        first_source = self.database.add_source(
            "Vendor News",
            "https://vendor.example/first.xml",
            "Vendor",
            content_kind="news",
        )
        second_source = self.database.add_source(
            "Vendor Blog",
            "https://vendor.example/second.xml",
            "Vendor",
            content_kind="news",
        )
        service = HeraldService(
            self.database,
            fetcher=fetcher,
            exporter=ObsidianExporter(self.vault),
            now=lambda: datetime(2026, 8, 8, tzinfo=UTC),
        )
        service.refresh_source(self.database.get_source(first_source))
        entry = self.database.list_entries(content_kind="news")[0]
        kept = service.change_status(int(entry["id"]), "kept")
        path = self.vault / kept["exported_path"]
        path.write_text(
            path.read_text(encoding="utf-8").replace(
                "## My Notes\n\n", "## My Notes\n\nCompare launch claims.\n"
            ),
            encoding="utf-8",
        )

        service.refresh_source(self.database.get_source(first_source))
        service.refresh_source(self.database.get_source(second_source))

        entries = self.database.list_entries(content_kind="news")
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0]["source_id"], first_source)
        self.assertEqual(entries[0]["status"], "kept")
        self.assertEqual(entries[0]["exported_path"], kept["exported_path"])
        self.assertEqual(len(list(self.vault.rglob("*.md"))), 1)
        updated = path.read_text(encoding="utf-8")
        self.assertIn("# Canonical duplicate title", updated)
        self.assertIn("Updated launch details.", updated)
        self.assertIn("Compare launch claims.", updated)
        self.assertIn('publisher: "Vendor News"', updated)
        self.assertNotIn("## References", updated)

    def test_keep_succeeds_when_export_fails(self) -> None:
        invalid_vault = self.vault.parent / "not-a-directory"
        invalid_vault.write_text("occupied", encoding="utf-8")
        service = HeraldService(
            self.database,
            exporter=ObsidianExporter(invalid_vault),
        )

        entry = service.change_status(self.entry_id, "kept")

        self.assertEqual(entry["status"], "kept")
        self.assertEqual(entry["obsidian_export"]["state"], "failed")
        self.assertTrue(entry["obsidian_export"]["error"])

    def test_reexport_preserves_custom_frontmatter_and_notes(self) -> None:
        self.service.change_status(self.entry_id, "kept")
        path = self.vault / self.database.get_entry(self.entry_id)["exported_path"]
        document = path.read_text(encoding="utf-8")
        document = document.replace(
            "# herald:managed:end\n---",
            "# herald:managed:end\nmy_rating: 5\n---",
        ).replace("## My Notes\n\n", "## My Notes\n\nExact **annotation**.\n")
        path.write_text(document, encoding="utf-8")
        self.database.set_summary(
            self.entry_id, "A newer summary.", provider="test", model="new-model"
        )

        self.service.export_entry(self.entry_id)
        updated = path.read_text(encoding="utf-8")

        self.assertIn("my_rating: 5", updated)
        self.assertIn("Exact **annotation**.", updated)
        self.assertIn("A newer summary.", updated)
        self.assertEqual(updated.count("my_rating: 5"), 1)
        self.assertEqual(updated.count("Exact **annotation**."), 1)

    def test_summary_update_resynchronizes_a_kept_note(self) -> None:
        kept = self.service.change_status(self.entry_id, "kept")
        path = self.vault / kept["exported_path"]

        self.service.summarize_entry(self.entry_id)

        self.assertIn("Summary of A:", path.read_text(encoding="utf-8"))

    def test_atomic_failure_leaves_existing_note_unchanged(self) -> None:
        kept = self.service.change_status(self.entry_id, "kept")
        path = self.vault / kept["exported_path"]
        original = path.read_bytes()
        self.database.set_summary(
            self.entry_id, "A replacement that must not be partial.", provider="test"
        )

        with patch("herald.obsidian.os.replace", side_effect=OSError("disk full")):
            with self.assertRaisesRegex(OSError, "disk full"):
                self.service.export_entry(self.entry_id)

        self.assertEqual(path.read_bytes(), original)
        self.assertEqual(
            self.database.get_obsidian_export(self.entry_id)["state"], "failed"
        )

    def test_malformed_managed_note_is_a_non_destructive_conflict(self) -> None:
        self.service.change_status(self.entry_id, "kept")
        path = self.vault / self.database.get_entry(self.entry_id)["exported_path"]
        damaged = path.read_text(encoding="utf-8").replace(
            "<!-- herald:managed:end -->", ""
        )
        path.write_text(damaged, encoding="utf-8")

        with self.assertRaises(ObsidianConflictError):
            self.service.export_entry(self.entry_id)

        self.assertEqual(path.read_text(encoding="utf-8"), damaged)
        self.assertEqual(
            self.database.get_obsidian_export(self.entry_id)["state"], "conflict"
        )

    def test_unkeep_archives_and_rekeep_restores_annotations(self) -> None:
        kept = self.service.change_status(self.entry_id, "kept")
        path = self.vault / kept["exported_path"]
        path.write_text(
            path.read_text(encoding="utf-8") + "Permanent annotation.\n",
            encoding="utf-8",
        )

        read = self.service.change_status(self.entry_id, "read")

        self.assertEqual(read["status"], "read")
        self.assertEqual(read["obsidian_export"]["state"], "archived")
        self.assertIsNone(read["exported_path"])
        self.assertFalse(path.exists())
        archive = self.database.list_obsidian_archives(self.entry_id)[0]
        archived_path = self.service.exporter.archive_root / archive["archive_path"]
        self.assertTrue(archived_path.is_file())
        self.assertFalse(archived_path.is_relative_to(self.vault))

        restored = self.service.change_status(self.entry_id, "kept")
        restored_path = self.vault / restored["exported_path"]
        self.assertIn(
            "Permanent annotation.", restored_path.read_text(encoding="utf-8")
        )
        self.assertEqual(restored["obsidian_export"]["state"], "synced")
        self.assertIsNotNone(
            self.database.list_obsidian_archives(self.entry_id)[0]["restored_at"]
        )

    def test_stable_path_does_not_change_when_title_changes(self) -> None:
        self.service.change_status(self.entry_id, "kept")
        first = self.database.get_entry(self.entry_id)["exported_path"]
        with self.database.connect() as connection:
            connection.execute(
                "UPDATE entries SET title = ? WHERE id = ?",
                ("A completely different title", self.entry_id),
            )

        self.service.export_entry(self.entry_id)

        self.assertEqual(self.database.get_entry(self.entry_id)["exported_path"], first)
        self.assertIn(
            "# A completely different title",
            (self.vault / first).read_text(encoding="utf-8"),
        )

    def test_stable_path_does_not_change_when_identity_is_enriched_later(self) -> None:
        self.service.change_status(self.entry_id, "kept")
        first = self.database.get_entry(self.entry_id)["exported_path"]
        self.database.set_enrichment_state(
            self.entry_id,
            "enriched",
            provider="semantic-scholar",
            canonical_key="doi-10.1000/example",
        )

        self.service.export_entry(self.entry_id)

        self.assertEqual(self.database.get_entry(self.entry_id)["exported_path"], first)
        self.assertTrue((self.vault / first).is_file())

    def test_legacy_note_moves_to_stable_path_and_preserves_user_fields(self) -> None:
        self.database.set_status(self.entry_id, "kept")
        legacy_relative = "Herald/Old/legacy-note.md"
        legacy = self.vault / legacy_relative
        legacy.parent.mkdir(parents=True)
        legacy.write_text(
            "---\n"
            f"herald_id: {self.entry_id}\n"
            "title: \"Old generated title\"\n"
            "tags:\n  - \"herald\"\n"
            "my_rating: 4\n"
            "---\n\n"
            "# Old generated title\n\n## Article text\n\nOld text\n\n"
            "## Source\n\n- Original: unavailable\n\n"
            "## Notes\n\nLegacy annotation.\n",
            encoding="utf-8",
        )
        self.database.upsert_obsidian_export(
            self.entry_id,
            state="synced",
            vault_path=str(self.vault),
            relative_path=legacy_relative,
        )

        stable = self.service.export_entry(self.entry_id)
        document = stable.read_text(encoding="utf-8")

        self.assertFalse(legacy.exists())
        self.assertEqual(stable.name, f"herald-{self.entry_id:06d}.md")
        self.assertIn("my_rating: 4", document)
        self.assertIn("Legacy annotation.", document)
        self.assertIn("# herald:managed:start", document)

    def test_symlink_escape_is_rejected_without_writing_outside(self) -> None:
        outside = self.vault.parent / "outside"
        outside.mkdir()
        self.vault.mkdir()
        (self.vault / "Herald").symlink_to(outside, target_is_directory=True)

        entry = self.service.change_status(self.entry_id, "kept")

        self.assertEqual(entry["status"], "kept")
        self.assertEqual(entry["obsidian_export"]["state"], "failed")
        self.assertEqual(list(outside.rglob("*.md")), [])

    def test_changing_vault_preserves_annotations(self) -> None:
        kept = self.service.change_status(self.entry_id, "kept")
        old_path = self.vault / kept["exported_path"]
        old_path.write_text(
            old_path.read_text(encoding="utf-8") + "Move this annotation.\n",
            encoding="utf-8",
        )
        new_vault = self.vault.parent / "Existing Vault"
        new_vault.mkdir()

        settings = self.service.configure_obsidian(str(new_vault))

        entry = self.service.entry_with_obsidian_state(self.entry_id)
        new_path = new_vault / entry["exported_path"]
        self.assertEqual(settings["vault_path"], str(new_vault.resolve()))
        self.assertFalse(old_path.exists())
        self.assertIn("Move this annotation.", new_path.read_text(encoding="utf-8"))
        self.assertEqual(entry["obsidian_export"]["state"], "synced")

    def test_paper_metadata_and_directed_citation_link_follow_target_sync(self) -> None:
        self.database.add_paper_identifier(
            self.entry_id, "doi", "10.1000/citing", is_primary=True
        )
        self.database.replace_entry_keywords(
            self.entry_id,
            [
                {"keyword": "heterogeneous scheduling", "kind": "keyword", "score": 0.9},
                {"keyword": "Computer Science", "kind": "topic", "score": 1.0},
            ],
        )
        source_id = self.database.add_source(
            "Citation source", "https://example.org/citations", "Research"
        )
        target_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="cited-target",
            url="https://example.org/target",
            title="The cited target",
        )
        self.database.add_paper_identifier(target_id, "doi", "10.1000/target")
        self.database.upsert_paper_reference(
            self.entry_id,
            "doi:10.1000/target",
            external_scheme="doi",
            external_id="10.1000/target",
            cited_title="The cited target",
            cited_url="https://example.org/target",
        )

        citing = self.service.change_status(self.entry_id, "kept")
        citing_path = self.vault / citing["exported_path"]
        external = citing_path.read_text(encoding="utf-8")
        self.assertIn('identifiers:\n  - "doi:10.1000/citing"', external)
        self.assertIn('keywords:\n  - "heterogeneous scheduling"', external)
        self.assertIn('topics:\n  - "Computer Science"', external)
        self.assertIn("- [The cited target](https://doi.org/10.1000/target)", external)

        annotated = external.replace(
            "# herald:managed:end\n---",
            "# herald:managed:end\nreview_score: 5\n---",
        ).replace("## My Notes\n\n", "## My Notes\n\nDo not lose this.\n")
        citing_path.write_text(annotated, encoding="utf-8")

        target = self.service.change_status(target_id, "kept")
        promoted = citing_path.read_text(encoding="utf-8")
        target_link = target["exported_path"].removesuffix(".md")
        self.assertIn(f"- [[{target_link}|The cited target]]", promoted)
        self.assertNotIn("https://doi.org/10.1000/target", promoted)
        self.assertIn("review_score: 5", promoted)
        self.assertIn("Do not lose this.", promoted)
        target_note = (self.vault / target["exported_path"]).read_text(encoding="utf-8")
        self.assertNotIn("quoted unsafe", target_note)
        self.assertNotIn("10.1000/citing", target_note)

        self.service.change_status(target_id, "read")
        demoted = citing_path.read_text(encoding="utf-8")
        self.assertIn("- [The cited target](https://doi.org/10.1000/target)", demoted)
        self.assertNotIn(f"[[{target_link}", demoted)
        self.assertIn("Do not lose this.", demoted)

    def test_kept_but_unsynced_target_stays_an_external_reference(self) -> None:
        source_id = self.database.add_source(
            "Citation source", "https://example.org/unsynced", "Research"
        )
        target_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="unsynced-target",
            url="https://example.org/unsynced-target",
            title="Unsynced target",
        )
        self.database.add_paper_identifier(target_id, "arxiv", "2608.01234")
        self.database.upsert_paper_reference(
            self.entry_id,
            "arxiv:2608.01234",
            external_scheme="arxiv",
            external_id="2608.01234",
            cited_title="Unsynced target",
        )
        self.database.set_status(target_id, "kept")
        self.database.upsert_obsidian_export(
            target_id,
            state="failed",
            vault_path=str(self.vault),
            relative_path=f"Herald/Papers/herald-{target_id:06d}.md",
            error="disk unavailable",
        )

        citing = self.service.change_status(self.entry_id, "kept")
        document = (self.vault / citing["exported_path"]).read_text(encoding="utf-8")

        self.assertIn("[Unsynced target](https://arxiv.org/abs/2608.01234)", document)
        self.assertNotIn("[[Herald/Papers/", document)


if __name__ == "__main__":
    unittest.main()
