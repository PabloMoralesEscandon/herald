from __future__ import annotations

import json
import tempfile
import threading
import unittest
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from herald.config import Settings
from herald.demo import load_demo
from herald.storage import Database
from herald.web import make_server


class WebTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        root = Path(self.temporary_directory.name)
        self.database = Database(root / "herald.db")
        self.database.initialize()
        load_demo(self.database)
        self.settings = Settings(
            data_dir=root,
            database_path=root / "herald.db",
            vault_path=root / "vault",
            host="127.0.0.1",
            port=0,
            ollama_url="http://127.0.0.1:11434",
            ollama_model="demo",
        )
        self.server = make_server(self.settings, self.database, port=0)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self._stop_server)
        self.base_url = f"http://127.0.0.1:{self.server.server_address[1]}"

    def _stop_server(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def request(
        self,
        path: str,
        *,
        method: str = "GET",
        payload: dict[str, object] | None = None,
    ) -> tuple[int, object]:
        body = json.dumps(payload).encode() if payload is not None else None
        request = Request(
            self.base_url + path,
            data=body,
            method=method,
            headers={"Content-Type": "application/json"} if body else {},
        )
        try:
            with urlopen(request) as response:
                return response.status, json.loads(response.read())
        except HTTPError as error:
            try:
                return error.code, json.loads(error.read())
            finally:
                error.close()

    def test_dashboard_assets_are_served(self) -> None:
        with urlopen(self.base_url + "/") as response:
            html = response.read().decode()
        self.assertEqual(response.status, 200)
        self.assertIn("Herald", html)
        self.assertIn("/static/app.js", html)

        with urlopen(self.base_url + "/static/styles.css") as response:
            self.assertIn("text/css", response.headers["Content-Type"])
            self.assertIn("--green", response.read().decode())

    def test_lists_and_filters_entries(self) -> None:
        status, entries = self.request("/api/entries?status=unread&category=Demo")
        self.assertEqual(status, 200)
        self.assertEqual(len(entries), 2)
        self.assertTrue(all(entry["source_category"] == "Demo" for entry in entries))

    def test_gets_an_entry(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        status, entry = self.request(f"/api/entries/{entry_id}")
        self.assertEqual(status, 200)
        self.assertEqual(entry["id"], entry_id)
        self.assertIn("source_title", entry)

    def test_triage_action_uses_contract_verbs(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        status, entry = self.request(
            f"/api/entries/{entry_id}/action",
            method="POST",
            payload={"action": "keep"},
        )
        self.assertEqual(status, 200)
        self.assertEqual(entry["status"], "kept")
        self.assertEqual(self.database.get_entry(entry_id)["status"], "kept")

    def test_rejects_unknown_action(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        status, payload = self.request(
            f"/api/entries/{entry_id}/action",
            method="POST",
            payload={"action": "archive"},
        )
        self.assertEqual(status, 400)
        self.assertIn("error", payload)

    def test_adds_and_lists_source(self) -> None:
        status, source = self.request(
            "/api/sources",
            method="POST",
            payload={
                "title": "arXiv Hardware Architecture",
                "url": "https://rss.arxiv.org/rss/cs.AR",
                "category": "Chip Design",
            },
        )
        self.assertEqual(status, 201)
        self.assertEqual(source["category"], "Chip Design")
        _, sources = self.request("/api/sources")
        self.assertEqual(len(sources), 2)

    def test_future_service_routes_fail_explicitly(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        status, payload = self.request(
            f"/api/entries/{entry_id}/summarize",
            method="POST",
            payload={},
        )
        self.assertEqual(status, 501)
        self.assertIn("error", payload)


if __name__ == "__main__":
    unittest.main()
