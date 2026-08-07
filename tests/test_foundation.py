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
                '2026-01-01', 'Legacy content.', 'Legacy summary.', 'unread', NULL
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


if __name__ == "__main__":
    unittest.main()
