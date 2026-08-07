from __future__ import annotations

import os
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


if __name__ == "__main__":
    unittest.main()
