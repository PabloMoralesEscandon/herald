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
from herald.papers import PaperImporter
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
        self.assertIn("/static/app.js?v=22", html)
        self.assertIn("/static/styles.css?v=20", html)
        self.assertIn('id="reader-pdf"', html)
        self.assertIn('id="reader-content" class="reader-content" hidden', html)
        self.assertNotIn('id="reader-placeholder"', html)
        self.assertIn('class="nav-item active" data-status="unread"', html)
        self.assertLess(html.index('data-status="unread"'), html.index('data-status="all"'))
        self.assertIn('<h1 id="inbox-title">Unread</h1>', html)
        self.assertIn('id="clear-filters" class="text-button" type="button">Show unread', html)
        self.assertIn('id="relevance-nav"', html)
        self.assertIn('data-bucket="relevant"', html)
        self.assertIn('data-bucket="filtered"', html)
        self.assertIn('id="profile-dialog"', html)
        self.assertIn('id="import-dialog"', html)
        self.assertIn('id="references-section"', html)
        self.assertIn('id="load-more"', html)
        self.assertIn('class="workspace-switch" id="kind-nav"', html)
        self.assertIn('data-kind="paper" role="tab" aria-selected="true"', html)
        self.assertIn('data-kind="news" role="tab" aria-selected="false"', html)
        self.assertIn('id="source-health-list"', html)

        with urlopen(self.base_url + "/static/styles.css") as response:
            styles = response.read().decode()
            self.assertIn("text/css", response.headers["Content-Type"])
            self.assertIn("[hidden] { display: none !important; }", styles)
            self.assertIn("height: 100dvh", styles)
            self.assertIn("overscroll-behavior: contain", styles)
            self.assertEqual(response.headers["Cache-Control"], "no-store, max-age=0")
            self.assertIn("--green", styles)
            self.assertIn(".workspace-switch", styles)
            self.assertIn(".news-card", styles)
            self.assertIn(".source-health", styles)

        with urlopen(self.base_url + "/static/app.js") as response:
            script = response.read().decode()
        self.assertIn('elements.list.addEventListener("click"', script)
        self.assertIn('elements.list.addEventListener("dblclick"', script)
        self.assertIn('window.open(arxivPdfUrl(entry.url) || entry.url', script)
        self.assertNotIn('href="/entry/${entry.id}"', script)
        self.assertNotIn('addEventListener("pointerover"', script)
        self.assertNotIn("selectEntry(state.entries[0].id)", script)
        self.assertIn("Preview · double-click", script)
        self.assertIn('status: "unread"', script)
        self.assertIn('state.status = "unread"', script)
        self.assertIn('kind: state.activeKind', script)
        self.assertIn('params.set("cursor", cursor)', script)
        self.assertIn('api(`/api/profiles/${state.activeKind}`)', script)
        self.assertIn('api("/api/import/paper"', script)
        self.assertIn('/api/references/${button.dataset.addReference}/add', script)
        self.assertIn('/obsidian/retry', script)
        self.assertIn('relevanceReasons(ranking).slice(0, 3)', script)
        self.assertIn('elements.kindNav.addEventListener("click", changeWorkspace)', script)
        self.assertIn('state.activeKind === "paper" ? api(`/api/entries/${id}/references`)', script)

    def test_news_workspace_search_counts_and_profile_contract(self) -> None:
        source_id = self.database.add_source(
            "Official Accelerator News",
            "https://example.com/official.xml",
            "Company",
            content_kind="news",
        )
        entry_id, _created = self.database.upsert_entry(
            source_id=source_id,
            guid="launch-1",
            url="https://example.com/launch",
            title="Company launches an accelerator",
            content="A new chip for inference workloads.",
            content_kind="news",
        )
        profile = self.database.get_relevance_profile("news")
        assert profile is not None
        self.database.upsert_entry_ranking(
            entry_id,
            profile["id"],
            score=91,
            bucket="relevant",
            explanation={"matched_interests": ["hardware and chip announcements"]},
            model="tfidf-v1",
        )

        status, page = self.request(
            "/api/entries?kind=news&bucket=relevant&q=accelerator&limit=10"
        )
        self.assertEqual(status, 200)
        self.assertEqual([entry["id"] for entry in page["entries"]], [entry_id])
        self.assertEqual(page["entries"][0]["relevance_score"], 91)

        status, stats = self.request("/api/stats")
        self.assertEqual(status, 200)
        self.assertEqual(stats["workspaces"]["news"]["total"], 1)
        self.assertEqual(stats["workspaces"]["news"]["statuses"]["unread"], 1)

        status, profile_payload = self.request("/api/profiles/news")
        self.assertEqual(status, 200)
        self.assertIn(profile_payload["threshold_mode"], {"auto", "adaptive"})
        status, updated = self.request(
            "/api/profiles/news", method="PUT", payload={"threshold": 42}
        )
        self.assertEqual(status, 200)
        self.assertEqual(updated["profile"]["threshold_mode"], "manual")

    def test_paper_ui_uses_paginated_ranked_api_contract(self) -> None:
        self.server.service.relevance.engine.rescore("paper")

        status, stats = self.request("/api/stats")
        self.assertEqual(status, 200)
        selected_bucket = (
            "relevant" if stats["relevance"]["paper"]["relevant"] else "filtered"
        )
        status, page = self.request(
            f"/api/entries?kind=paper&bucket={selected_bucket}&limit=1"
        )

        self.assertEqual(status, 200)
        self.assertIn("entries", page)
        self.assertIn("next_cursor", page)
        self.assertEqual(len(page["entries"]), 1)
        ranked = page["entries"][0]
        self.assertIn("relevance_score", ranked)
        self.assertIn("relevance_explanation", ranked)
        self.assertIn("relevance_model", ranked)

        status, detail = self.request(f"/api/entries/{ranked['id']}")
        self.assertEqual(status, 200)
        self.assertIn("relevance", detail)
        self.assertIn("obsidian_export", detail)

        status, references = self.request(
            f"/api/entries/{ranked['id']}/references"
        )
        self.assertEqual(status, 200)
        self.assertEqual(references["entry_id"], ranked["id"])
        self.assertEqual(references["references"], [])

    def test_lists_and_filters_entries(self) -> None:
        status, entries = self.request("/api/entries?status=unread&category=Demo")
        self.assertEqual(status, 200)
        self.assertEqual(len(entries), 2)
        self.assertTrue(all(entry["source_category"] == "Demo" for entry in entries))

    def test_stats_are_not_limited_to_visible_page(self) -> None:
        source_id = self.database.add_source(
            "Large feed", "https://example.org/large.xml", "Machine Learning"
        )
        for index in range(510):
            self.database.upsert_entry(
                source_id=source_id,
                guid=f"large-{index}",
                url=f"https://example.org/large/{index}",
                title=f"Large entry {index}",
            )

        status, stats = self.request("/api/stats")
        self.assertEqual(status, 200)
        self.assertEqual(stats["total"], 512)
        self.assertEqual(stats["categories"]["Demo"], 2)

        status, entries = self.request("/api/entries?category=Demo&limit=500")
        self.assertEqual(status, 200)
        self.assertEqual(len(entries), 2)

    def test_gets_an_entry(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        status, entry = self.request(f"/api/entries/{entry_id}")
        self.assertEqual(status, 200)
        self.assertEqual(entry["id"], entry_id)
        self.assertIn("source_title", entry)

    def test_internal_article_route_redirects_to_inbox(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        with urlopen(self.base_url + f"/entry/{entry_id}") as response:
            page = response.read().decode()
        self.assertEqual(response.url, self.base_url + "/")
        self.assertIn("Herald", page)
        self.assertNotIn("article-page", page)

        status, payload = self.request(
            f"/entry/{entry_id}/action", method="POST", payload={"action": "keep"}
        )
        self.assertEqual(status, 404)
        self.assertIn("error", payload)
        self.assertEqual(self.database.get_entry(entry_id)["status"], "unread")

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
        self.assertEqual(entry["obsidian_export"]["state"], "synced")
        self.assertTrue((self.settings.vault_path / entry["exported_path"]).is_file())

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
        self.assertEqual(source["content_kind"], "paper")

        status, news_source = self.request(
            "/api/sources",
            method="POST",
            payload={
                "title": "Official News",
                "url": "https://example.com/news.xml?utm_source=setup",
                "category": "Company",
                "content_kind": "news",
            },
        )
        self.assertEqual(status, 201)
        self.assertEqual(news_source["content_kind"], "news")
        self.assertEqual(news_source["url"], "https://example.com/news.xml")

        invalid_status, invalid = self.request(
            "/api/sources",
            method="POST",
            payload={
                "title": "Invalid",
                "url": "https://example.com/invalid.xml",
                "category": "Company",
                "content_kind": "podcast",
            },
        )
        self.assertEqual(invalid_status, 400)
        self.assertIn("paper or news", invalid["error"])
        _, sources = self.request("/api/sources")
        self.assertEqual(len(sources), 3)

    def test_imports_a_paper_and_reports_idempotent_repeats(self) -> None:
        metadata = {
            "paperId": "0123456789abcdef0123456789abcdef01234567",
            "externalIds": {"DOI": "10.1145/web.1"},
            "url": "https://www.semanticscholar.org/paper/web",
            "title": "Imported through the Herald API",
            "abstract": "An abstract about chip design.",
            "authors": [{"name": "API Author"}],
            "publicationDate": "2026-08-08",
            "fieldsOfStudy": ["Computer Science"],
        }

        def fetcher(url: str, headers: object, max_bytes: int, timeout: float) -> bytes:
            return json.dumps(metadata).encode()

        self.server.service.paper_importer = PaperImporter(
            self.database, fetcher=fetcher, sleep=lambda _: None, request_delay=0
        )
        first_status, first = self.request(
            "/api/import/paper", method="POST", payload={"input": "10.1145/web.1"}
        )
        second_status, second = self.request(
            "/api/import/paper",
            method="POST",
            payload={"input": "https://doi.org/10.1145/WEB.1"},
        )

        self.assertEqual(first_status, 201)
        self.assertTrue(first["created"])
        self.assertEqual(first["entry"]["status"], "unread")
        self.assertEqual(second_status, 200)
        self.assertFalse(second["created"])
        self.assertEqual(second["entry"]["id"], first["entry"]["id"])

    def test_paper_import_validates_input_without_network_access(self) -> None:
        status, payload = self.request(
            "/api/import/paper", method="POST", payload={"input": "https://127.0.0.1/private"}
        )
        self.assertEqual(status, 400)
        self.assertIn("private or local", payload["error"])

    def test_summary_route_uses_offline_fallback(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        status, payload = self.request(
            f"/api/entries/{entry_id}/summarize",
            method="POST",
            payload={},
        )
        self.assertEqual(status, 200)
        self.assertEqual(payload["provider"], "fallback")
        self.assertTrue(payload["entry"]["summary"])
        self.assertEqual(payload["entry"]["summary_provider"], "fallback")

    def test_kept_entry_can_be_exported(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        status, payload = self.request(
            f"/api/entries/{entry_id}/export", method="POST", payload={}
        )
        self.assertEqual(status, 409)
        self.assertIn("Only kept", payload["error"])

        self.database.set_status(entry_id, "kept")
        status, payload = self.request(
            f"/api/entries/{entry_id}/export", method="POST", payload={}
        )
        self.assertEqual(status, 200)
        self.assertTrue(Path(payload["path"]).is_file())
        self.assertEqual(
            payload["entry"]["exported_path"],
            "Herald/Papers/herald-000001.md",
        )

    def test_obsidian_settings_require_an_existing_absolute_vault(self) -> None:
        status, settings = self.request("/api/settings/obsidian")
        self.assertEqual(status, 200)
        self.assertEqual(settings["vault_path"], str(self.settings.vault_path.resolve()))

        status, payload = self.request(
            "/api/settings/obsidian",
            method="PUT",
            payload={"vault_path": "relative/vault"},
        )
        self.assertEqual(status, 400)
        self.assertIn("absolute", payload["error"])

        chosen = self.settings.data_dir / "Existing Vault"
        chosen.mkdir()
        status, configured = self.request(
            "/api/settings/obsidian",
            method="PUT",
            payload={"vault_path": str(chosen)},
        )
        self.assertEqual(status, 200)
        self.assertEqual(configured["vault_path"], str(chosen.resolve()))
        self.assertTrue(configured["configured"])

    def test_obsidian_retry_returns_sync_state(self) -> None:
        entry_id = self.database.list_entries()[0]["id"]
        self.database.set_status(entry_id, "kept")

        status, payload = self.request(
            f"/api/entries/{entry_id}/obsidian/retry",
            method="POST",
            payload={},
        )

        self.assertEqual(status, 200)
        self.assertTrue(Path(payload["path"]).is_file())
        self.assertEqual(payload["entry"]["obsidian_export"]["state"], "synced")


if __name__ == "__main__":
    unittest.main()
