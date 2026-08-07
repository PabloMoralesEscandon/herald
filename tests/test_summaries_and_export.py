from __future__ import annotations

import tempfile
import unittest
from contextlib import AbstractContextManager
from json import loads
from pathlib import Path
from unittest.mock import patch

from herald.obsidian import ObsidianExporter
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


if __name__ == "__main__":
    unittest.main()
