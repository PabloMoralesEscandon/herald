from __future__ import annotations

import os
import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from herald.config import Settings
from herald.demo import load_demo
from herald.storage import Database


class FoundationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.database = Database(Path(self.temporary_directory.name) / "herald.db")
        self.database.initialize()

    def test_demo_is_idempotent(self) -> None:
        self.assertEqual(load_demo(self.database), 2)
        self.assertEqual(load_demo(self.database), 0)
        self.assertEqual(len(self.database.list_entries()), 2)

    def test_entry_status_transitions(self) -> None:
        load_demo(self.database)
        entry = self.database.list_entries()[0]
        self.assertTrue(self.database.set_status(entry["id"], "kept"))
        self.assertEqual(self.database.get_entry(entry["id"])["status"], "kept")
        with self.assertRaises(ValueError):
            self.database.set_status(entry["id"], "unknown")

    def test_settings_can_be_overridden(self) -> None:
        root = Path(self.temporary_directory.name)
        with patch.dict(
            os.environ,
            {
                "HERALD_DATA_DIR": str(root),
                "HERALD_PORT": "9000",
                "HERALD_VAULT": str(root / "notes"),
            },
        ):
            settings = Settings.from_env()
        self.assertEqual(settings.port, 9000)
        self.assertEqual(settings.vault_path, root / "notes")

    def test_entry_counts_cover_complete_database(self) -> None:
        load_demo(self.database)
        first = self.database.list_entries()[0]
        self.database.set_status(first["id"], "kept")

        counts = self.database.entry_counts()

        self.assertEqual(counts["total"], 2)
        self.assertEqual(counts["statuses"]["kept"], 1)
        self.assertEqual(counts["statuses"]["unread"], 1)
        self.assertEqual(counts["categories"]["Demo"], 2)

    def test_existing_database_gains_summary_provenance_columns(self) -> None:
        path = Path(self.temporary_directory.name) / "legacy.db"
        connection = sqlite3.connect(path)
        connection.executescript(
            """
            CREATE TABLE sources (
                id INTEGER PRIMARY KEY, title TEXT NOT NULL, url TEXT NOT NULL UNIQUE,
                category TEXT NOT NULL DEFAULT 'Unsorted', enabled INTEGER NOT NULL DEFAULT 1,
                created_at TEXT NOT NULL
            );
            CREATE TABLE entries (
                id INTEGER PRIMARY KEY, source_id INTEGER NOT NULL REFERENCES sources(id),
                guid TEXT NOT NULL, url TEXT NOT NULL, title TEXT NOT NULL,
                author TEXT NOT NULL DEFAULT '', published_at TEXT, discovered_at TEXT NOT NULL,
                content TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
                status TEXT NOT NULL DEFAULT 'unread', exported_path TEXT,
                UNIQUE(source_id, guid)
            );
            INSERT INTO sources VALUES (1, 'Legacy', 'https://example.org/feed', 'Test', 1, '2026-01-01');
            INSERT INTO entries VALUES (
                1, 1, 'legacy', 'https://example.org/article', 'Legacy article', '', NULL,
                '2026-01-01', 'Legacy content.', 'Legacy summary.', 'unread',
                'Herald/Legacy.md'
            );
            """
        )
        connection.commit()
        connection.close()

        database = Database(path)
        database.initialize()
        entry = database.get_entry(1)

        self.assertEqual(entry["summary_provider"], "unknown")
        self.assertEqual(entry["summary_model"], "")
        self.assertIsNone(entry["summary_generated_at"])
        self.assertEqual(entry["content_kind"], "paper")
        self.assertEqual(entry["canonical_url"], "https://example.org/article")
        self.assertEqual(entry["status"], "unread")
        self.assertEqual(entry["summary"], "Legacy summary.")
        self.assertEqual(entry["exported_path"], "Herald/Legacy.md")
        self.assertEqual(database.get_obsidian_export(1)["state"], "synced")
        with database.connect() as migrated:
            tables = {
                str(row["name"])
                for row in migrated.execute(
                    "SELECT name FROM sqlite_master WHERE type = 'table'"
                )
            }
            version = int(migrated.execute("PRAGMA user_version").fetchone()[0])
        self.assertEqual(version, 3)
        self.assertTrue(
            {
                "paper_identifiers",
                "paper_references",
                "entry_keywords",
                "relevance_profiles",
                "entry_rankings",
                "entry_feedback",
                "embedding_cache",
                "application_settings",
                "provider_cache",
                "obsidian_exports",
                "obsidian_archives",
            }.issubset(tables)
        )

    def test_entry_pages_have_no_hidden_five_hundred_item_ceiling(self) -> None:
        source_id = self.database.add_source(
            "Large source", "https://example.org/large", "Test"
        )
        for index in range(620):
            self.database.upsert_entry(
                source_id=source_id,
                guid=f"paper-{index}",
                url=f"https://example.org/paper/{index}",
                title=f"Paper {index}",
            )

        self.assertEqual(len(self.database.list_entries(limit=600)), 600)
        self.assertEqual(len(self.database.list_entries(limit=None)), 620)

        seen: list[int] = []
        cursor = None
        while True:
            page = self.database.list_entries_page(limit=137, cursor=cursor)
            seen.extend(int(entry["id"]) for entry in page["entries"])
            cursor = page["next_cursor"]
            if cursor is None:
                break
        self.assertEqual(len(seen), 620)
        self.assertEqual(len(set(seen)), 620)
        with self.assertRaises(ValueError):
            self.database.list_entries(cursor="not-a-valid-cursor")

    def test_enrichment_ranking_and_obsidian_storage_apis(self) -> None:
        source_id = self.database.add_source(
            "Official News",
            "https://example.org/news.xml",
            "Announcements",
            content_kind="news",
            adapter="atom",
        )
        entry_id, created = self.database.upsert_entry(
            source_id=source_id,
            guid="launch",
            url="https://example.org/launch?tracking=yes",
            canonical_url="https://example.org/launch",
            canonical_key="doi-10.1-example",
            title="Example launch",
        )
        self.assertTrue(created)
        entry = self.database.get_entry(entry_id)
        self.assertEqual(entry["content_kind"], "news")
        self.assertEqual(entry["source_adapter"], "atom")
        self.assertEqual(entry["canonical_url"], "https://example.org/launch")

        self.assertTrue(
            self.database.update_source_refresh_state(
                source_id,
                succeeded=True,
                etag='"revision-1"',
                last_modified="Fri, 08 Aug 2026 12:00:00 GMT",
            )
        )
        source = self.database.list_sources()[0]
        self.assertEqual(source["etag"], '"revision-1"')
        self.assertEqual(source["refresh_error"], "")

        self.assertTrue(
            self.database.set_enrichment_state(
                entry_id,
                "enriched",
                provider="semantic-scholar",
                canonical_key="doi-10.1-example",
            )
        )
        self.database.add_paper_identifier(
            entry_id, "DOI", "10.1/EXAMPLE", is_primary=True
        )
        self.assertEqual(
            self.database.find_entry_by_identifier("doi", "10.1/example")["id"],
            entry_id,
        )
        self.assertTrue(self.database.list_paper_identifiers(entry_id)[0]["is_primary"])

        cited_id, _ = self.database.upsert_entry(
            source_id=source_id,
            guid="cited",
            url="https://example.org/cited",
            title="Cited work",
        )
        reference_id = self.database.upsert_paper_reference(
            entry_id,
            "doi:10.2/cited",
            cited_entry_id=cited_id,
            external_scheme="doi",
            external_id="10.2/CITED",
            cited_title="Cited work",
            position=1,
            provider="semantic-scholar",
        )
        references = self.database.list_paper_references(entry_id)
        self.assertEqual(references[0]["id"], reference_id)
        self.assertEqual(references[0]["cited_entry_id"], cited_id)
        self.assertEqual(references[0]["external_id"], "10.2/cited")

        self.database.replace_entry_keywords(
            entry_id,
            [
                {"keyword": "chip design", "score": 0.9, "provider": "tfidf"},
                {"keyword": "Hardware", "kind": "topic", "provider": "s2"},
            ],
        )
        self.assertEqual(len(self.database.list_entry_keywords(entry_id)), 2)

        profile = self.database.upsert_relevance_profile(
            "news",
            interests=["chip launches"],
            exclusions=["financial results"],
            include_phrases=["new architecture"],
        )
        ranking = self.database.upsert_entry_ranking(
            entry_id,
            profile["id"],
            score=88.5,
            bucket="relevant",
            components={"lexical": 20.0},
            explanation={"matched": ["chip launches"]},
            model="tfidf",
        )
        self.assertEqual(ranking["components"], {"lexical": 20.0})
        self.assertEqual(
            [entry["id"] for entry in self.database.list_entries(
                content_kind="news",
                relevance_bucket="relevant",
                profile_id=profile["id"],
            )],
            [entry_id],
        )
        self.database.record_relevance_feedback(entry_id, profile["id"], "keep")
        self.assertEqual(
            self.database.list_relevance_feedback(profile["id"])[0]["label"],
            "keep",
        )

        self.database.put_embedding("content-hash", "embeddinggemma", 3, b"vector")
        self.assertEqual(
            self.database.get_embedding("content-hash", "embeddinggemma")["vector"],
            b"vector",
        )
        self.database.set_setting("obsidian", {"vault_path": "/notes"})
        self.assertEqual(
            self.database.get_setting("obsidian"), {"vault_path": "/notes"}
        )

        export = self.database.upsert_obsidian_export(
            entry_id,
            state="synced",
            vault_path="/notes",
            relative_path="Herald/News/example.md",
            content_hash="markdown-hash",
        )
        self.assertEqual(export["state"], "synced")
        self.assertEqual(
            self.database.get_entry(entry_id)["exported_path"],
            "Herald/News/example.md",
        )
        archive_id = self.database.add_obsidian_archive(
            entry_id,
            original_relative_path="Herald/News/example.md",
            archive_path="2026-08-08/example.md",
        )
        self.assertEqual(
            self.database.list_obsidian_archives(entry_id)[0]["id"], archive_id
        )


if __name__ == "__main__":
    unittest.main()
