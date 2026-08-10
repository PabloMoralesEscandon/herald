from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

from herald.storage import Database
from herald.sources import CURATED_SOURCES


PROJECT_ROOT = Path(__file__).resolve().parents[1]


def _available_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


class EndToEndWorkflowTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.data_dir = Path(self.temporary_directory.name) / "herald-data"
        self.port = _available_port()
        self.base_url = f"http://127.0.0.1:{self.port}"
        self.environment = os.environ.copy()
        self.environment.update(
            {
                "HERALD_DATA_DIR": str(self.data_dir),
                "HERALD_PORT": str(self.port),
                "HERALD_OLLAMA_URL": "http://127.0.0.1:1",
                "PYTHONUNBUFFERED": "1",
            }
        )
        self.server: subprocess.Popen[str] | None = None
        self.addCleanup(self._stop_server)

    def _run_cli(self, *command: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, "-m", "herald.cli", *command],
            cwd=PROJECT_ROOT,
            env=self.environment,
            text=True,
            capture_output=True,
            check=True,
            timeout=10,
        )

    def _start_server(self) -> None:
        self.server = subprocess.Popen(
            [sys.executable, "-m", "herald.cli", "serve"],
            cwd=PROJECT_ROOT,
            env=self.environment,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if self.server.poll() is not None:
                output = self.server.stdout.read() if self.server.stdout else ""
                self.fail(f"Herald server stopped during startup:\n{output}")
            try:
                with urlopen(self.base_url + "/", timeout=0.25) as response:
                    if response.status == 200:
                        return
            except URLError:
                time.sleep(0.05)
        self.fail("Herald server did not become ready within five seconds")

    def _stop_server(self) -> None:
        if self.server is None:
            return
        if self.server.poll() is None:
            self.server.terminate()
            try:
                self.server.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait(timeout=2)
        if self.server.stdout is not None:
            self.server.stdout.close()

    def _request(
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
            headers={"Content-Type": "application/json"} if body is not None else {},
        )
        try:
            with urlopen(request, timeout=2) as response:
                return response.status, json.loads(response.read())
        except HTTPError as error:
            try:
                return error.code, json.loads(error.read())
            finally:
                error.close()

    def test_clean_cli_to_api_to_markdown_workflow(self) -> None:
        initialized = self._run_cli("init")
        demonstrated = self._run_cli("demo")

        self.assertIn(f"{len(CURATED_SOURCES)} sources added", initialized.stdout)
        self.assertIn("Loaded 2 demo entries", demonstrated.stdout)
        self.assertTrue((self.data_dir / "herald.db").is_file())

        self._start_server()
        status, entries = self._request("/api/entries")
        self.assertEqual(status, 200)
        self.assertIsInstance(entries, list)
        self.assertEqual(len(entries), 2)
        entry_id = entries[0]["id"]

        status, summarized = self._request(
            f"/api/entries/{entry_id}/summarize", method="POST", payload={}
        )
        self.assertEqual(status, 200)
        self.assertEqual(summarized["provider"], "fallback")
        self.assertTrue(summarized["entry"]["summary"])

        status, kept = self._request(
            f"/api/entries/{entry_id}/action",
            method="POST",
            payload={"action": "keep"},
        )
        self.assertEqual(status, 200)
        self.assertEqual(kept["status"], "kept")
        self.assertEqual(kept["obsidian_export"]["state"], "synced")
        self.assertTrue((self.data_dir / "vault" / kept["exported_path"]).is_file())

        status, exported = self._request(
            f"/api/entries/{entry_id}/export", method="POST", payload={}
        )
        self.assertEqual(status, 200)
        markdown_path = Path(exported["path"])
        self.assertTrue(markdown_path.is_relative_to(self.data_dir / "vault"))
        document = markdown_path.read_text(encoding="utf-8")
        self.assertIn("---\n# herald:managed:start\nherald_id:", document)
        self.assertIn("> [!abstract] Summary", document)
        self.assertIn("## Article text", document)
        self.assertIn("## Source", document)
        self.assertIn("## My Notes", document)
        self.assertIn("status: \"kept\"", document)
        self.assertEqual(
            exported["entry"]["exported_path"],
            markdown_path.relative_to(self.data_dir / "vault").as_posix(),
        )

    def test_source_manifest_cli_round_trip(self) -> None:
        self._run_cli("init")
        destination = self.data_dir / "portable-sources.json"

        exported = self._run_cli("sources", "export", str(destination))
        manifest = json.loads(destination.read_text(encoding="utf-8"))
        imported = self._run_cli("sources", "import", str(destination))
        result = json.loads(imported.stdout)

        self.assertIn(f"Exported {len(CURATED_SOURCES)} sources", exported.stdout)
        self.assertEqual(manifest["format"], "herald.sources")
        self.assertEqual(result["imported"], len(CURATED_SOURCES))
        self.assertEqual(result["unchanged"], len(CURATED_SOURCES))
        self.assertEqual(result["created"], 0)

    def test_news_workspace_runs_from_clean_install(self) -> None:
        self._run_cli("init")
        self._start_server()
        database = Database(self.data_dir / "herald.db")
        source_id = database.add_source(
            "Example Official News",
            "https://example.com/news.xml",
            "Example",
            content_kind="news",
        )
        entry_id, _created = database.upsert_entry(
            source_id=source_id,
            guid="example-launch",
            url="https://example.com/news/launch",
            title="Example launches a new inference accelerator",
            content="The official announcement describes a new accelerator.",
            content_kind="news",
        )
        profile = database.get_relevance_profile("news")
        self.assertIsNotNone(profile)
        assert profile is not None
        database.upsert_entry_ranking(
            entry_id,
            profile["id"],
            score=88,
            bucket="relevant",
            explanation={"matched_interests": ["hardware announcements"]},
            model="tfidf-v1",
        )

        status, page = self._request(
            "/api/entries?kind=news&bucket=relevant&status=unread&q=accelerator"
        )
        self.assertEqual(status, 200)
        self.assertEqual(page["entries"][0]["id"], entry_id)
        self.assertEqual(page["entries"][0]["content_kind"], "news")
        with urlopen(self.base_url + "/") as response:
            dashboard = response.read().decode()
        self.assertIn('data-kind="news"', dashboard)
        self.assertIn('id="profile-dialog"', dashboard)


if __name__ == "__main__":
    unittest.main()
