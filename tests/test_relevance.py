from __future__ import annotations

import json
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from herald.config import Settings
from herald.relevance import (
    EmbeddingError,
    OllamaEmbeddingProvider,
    RelevanceCoordinator,
    RelevanceEngine,
    ScoreDraft,
)
from herald.service import HeraldService
from herald.storage import Database
from herald.web import make_server


class BrokenEmbeddings:
    model = "broken-local-model"

    def embed(self, texts: object) -> list[list[float]]:
        raise EmbeddingError("model is not installed")


class FakeResponse:
    def __init__(self, payload: object):
        self.payload = json.dumps(payload).encode()

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *args: object) -> None:
        return None

    def read(self, limit: int = -1) -> bytes:
        return self.payload[:limit] if limit >= 0 else self.payload


class RelevanceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.database = Database(Path(self.temporary_directory.name) / "herald.db")
        self.database.initialize()
        self.entry_counter = 0
        self.paper_source = self.database.add_source(
            "Research", "https://example.org/papers", "Research"
        )
        self.news_source = self.database.add_source(
            "News",
            "https://example.org/news",
            "Announcements",
            content_kind="news",
        )

    def add_entry(
        self, title: str, *, content: str = "", kind: str = "paper"
    ) -> int:
        source = self.paper_source if kind == "paper" else self.news_source
        self.entry_counter += 1
        entry_id, _ = self.database.upsert_entry(
            source_id=source,
            guid=f"{kind}-{self.entry_counter}-{title}",
            url=f"https://example.org/{kind}/{self.entry_counter}",
            title=title,
            content=content or title,
        )
        return entry_id

    def test_defaults_are_independent_and_offline_scoring_is_explainable(self) -> None:
        relevant_id = self.add_entry("Digital circuits for low latency chip design")
        irrelevant_id = self.add_entry("A historical study of medieval poetry")
        news_id = self.add_entry("New GPU architecture launches", kind="news")
        engine = RelevanceEngine(self.database, BrokenEmbeddings())

        result = engine.rescore("paper")

        self.assertEqual(result["model"], "tfidf-v1")
        self.assertIn("model is not installed", result["fallback_reason"])
        paper_profile = self.database.get_relevance_profile("paper")
        news_profile = self.database.get_relevance_profile("news")
        self.assertIn("reinforcement learning", paper_profile["interests"])
        self.assertNotEqual(paper_profile["id"], news_profile["id"])
        relevant = self.database.get_entry_ranking(relevant_id, paper_profile["id"])
        irrelevant = self.database.get_entry_ranking(irrelevant_id, paper_profile["id"])
        self.assertEqual(relevant["bucket"], "relevant")
        self.assertEqual(irrelevant["bucket"], "filtered")
        self.assertGreater(relevant["components"]["semantic_interest"], 0)
        self.assertIn("threshold", relevant["explanation"])
        self.assertIsNone(
            self.database.get_entry_ranking(news_id, paper_profile["id"])
        )
        counts = self.database.entry_counts()["relevance"]
        self.assertEqual(counts["paper"]["relevant"], 1)
        self.assertEqual(counts["news"]["pending"], 1)

    def test_source_context_recovers_topical_feed_without_whitelisting_brand(self) -> None:
        apple_ml = self.database.add_source(
            "Apple Machine Learning Research",
            "https://machinelearning.apple.com/rss.xml",
            "Machine Learning",
            content_kind="news",
        )
        apple_newsroom = self.database.add_source(
            "Apple Newsroom",
            "https://www.apple.com/newsroom/rss-feed.rss",
            "Company News",
            content_kind="news",
        )
        ml_ids = []
        for index, title in enumerate(
            ("Learning representations on device", "A framework for private inference"),
            start=1,
        ):
            entry_id, _ = self.database.upsert_entry(
                source_id=apple_ml,
                guid=f"apple-ml-{index}",
                url=f"https://machinelearning.apple.com/research/{index}",
                title=title,
                content="Technical methods and evaluation results.",
                content_kind="news",
            )
            ml_ids.append(entry_id)
        sports_id, _ = self.database.upsert_entry(
            source_id=apple_newsroom,
            guid="apple-sports",
            url="https://www.apple.com/newsroom/2026/08/sports-season/",
            title="Apple celebrates the start of the new sports season",
            content="Fans can follow teams, fixtures, and match highlights.",
            content_kind="news",
        )
        engine = RelevanceEngine(self.database, BrokenEmbeddings())

        result = engine.rescore("news")

        profile = self.database.get_relevance_profile("news")
        self.assertGreaterEqual(result["relevant"], 1)
        self.assertTrue(
            any(
                self.database.get_entry_ranking(entry_id, profile["id"])["bucket"]
                == "relevant"
                for entry_id in ml_ids
            )
        )
        sports = self.database.get_entry_ranking(sports_id, profile["id"])
        self.assertEqual(sports["bucket"], "filtered")
        self.assertEqual(sports["explanation"]["decision"], "score is below threshold")

    def test_never_show_overrides_include_and_ties_do_not_create_a_quota(self) -> None:
        engine = RelevanceEngine(self.database)
        profile = engine.update_profile(
            "paper",
            {
                "interests": ["chip design"],
                "include_phrases": ["must read"],
                "never_show_phrases": ["survey"],
            },
        )
        ids = [self.add_entry("Must read chip design") for _ in range(20)]
        blocked = self.add_entry("Must read chip design survey")

        result = engine.rescore("paper")

        self.assertEqual(result["relevant"], 20)
        self.assertTrue(
            all(
                self.database.get_entry_ranking(entry_id, profile["id"])["bucket"]
                == "relevant"
                for entry_id in ids
            )
        )
        blocked_ranking = self.database.get_entry_ranking(blocked, profile["id"])
        self.assertEqual(blocked_ranking["bucket"], "filtered")
        self.assertEqual(
            blocked_ranking["explanation"]["decision"],
            "never-show phrase matched",
        )

    def test_adaptive_threshold_requires_feedback_and_moves_smoothly(self) -> None:
        engine = RelevanceEngine(self.database)
        profile = engine.update_profile("paper", {"threshold": 50.0})
        self.database.set_setting(f"relevance.threshold_mode.{profile['id']}", "auto")
        drafts: list[ScoreDraft] = []
        feedback: list[dict[str, object]] = []
        for index in range(20):
            entry_id = self.add_entry(f"Feedback paper {index}")
            score = float(90 - index) if index < 5 else float(70 - index)
            label = "keep" if index < 5 else "discard"
            drafts.append(
                ScoreDraft(
                    entry=self.database.get_entry(entry_id),
                    score=score,
                    components={},
                    explanation={},
                    forced_filtered=False,
                )
            )
            feedback.append({"entry_id": entry_id, "label": label})

        threshold, mode = engine._choose_threshold(profile, drafts, feedback)

        self.assertEqual(mode, "adaptive-feedback")
        self.assertEqual(threshold, 53.0)

    def test_ranked_cursor_pages_by_score_without_a_five_hundred_ceiling(self) -> None:
        RelevanceEngine(self.database)
        profile = self.database.get_relevance_profile("paper")
        for index in range(620):
            entry_id = self.add_entry(f"Paper {index}")
            self.database.upsert_entry_ranking(
                entry_id,
                profile["id"],
                score=float(index),
                bucket="relevant",
                components={"semantic_interest": float(index)},
            )

        seen: list[int] = []
        scores: list[float] = []
        cursor = None
        while True:
            page = self.database.list_entries_page(
                content_kind="paper",
                relevance_bucket="relevant",
                profile_id=profile["id"],
                cursor=cursor,
                limit=113,
            )
            seen.extend(entry["id"] for entry in page["entries"])
            scores.extend(entry["relevance_score"] for entry in page["entries"])
            cursor = page["next_cursor"]
            if cursor is None:
                break
        self.assertEqual(len(seen), 620)
        self.assertEqual(len(set(seen)), 620)
        self.assertEqual(scores, sorted(scores, reverse=True))

    def test_ollama_embeddings_are_batched_and_cached(self) -> None:
        provider = OllamaEmbeddingProvider(
            self.database,
            "http://ollama.invalid",
            batch_size=2,
        )
        replies = [
            FakeResponse({"embeddings": [[1, 0], [0, 1]]}),
            FakeResponse({"embeddings": [[0.5, 0.5]]}),
        ]
        with patch("herald.relevance.urllib.request.urlopen", side_effect=replies) as call:
            first = provider.embed(["one", "two", "three"])
            second = provider.embed(["one", "two", "three"])

        self.assertEqual(first, second)
        self.assertEqual(call.call_count, 2)
        self.assertEqual(first[2], [0.5, 0.5])


class RelevanceWebTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        root = Path(self.temporary_directory.name)
        self.database = Database(root / "herald.db")
        self.database.initialize()
        source = self.database.add_source("Research", "https://example.org/feed")
        for index in range(12):
            self.database.upsert_entry(
                source_id=source,
                guid=str(index),
                url=f"https://example.org/{index}",
                title=(
                    f"Reinforcement learning system {index}"
                    if index < 4
                    else f"Unrelated article {index}"
                ),
            )
        engine = RelevanceEngine(self.database)
        relevance = RelevanceCoordinator(engine)
        service = HeraldService(self.database, relevance=relevance)
        settings = Settings(
            data_dir=root,
            database_path=root / "herald.db",
            vault_path=root / "vault",
            host="127.0.0.1",
            port=0,
            ollama_url="http://127.0.0.1:1",
            ollama_model="demo",
        )
        self.server = make_server(settings, self.database, port=0, service=service)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self._stop)
        self.base_url = f"http://127.0.0.1:{self.server.server_address[1]}"

    def _stop(self) -> None:
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
            headers={"Content-Type": "application/json"} if body is not None else {},
        )
        try:
            with urlopen(request) as response:
                return response.status, json.loads(response.read())
        except HTTPError as error:
            try:
                return error.code, json.loads(error.read())
            finally:
                error.close()

    def wait_for_rescore(self) -> dict[str, object]:
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            _status, health = self.request("/api/relevance/health")
            job = health["jobs"].get("paper")
            if job and job["state"] != "running":
                return job
            time.sleep(0.01)
        self.fail("rescore did not complete")

    def test_profile_rescore_feedback_and_paginated_queue_contract(self) -> None:
        status, profile = self.request("/api/profiles/paper")
        self.assertEqual(status, 200)
        self.assertEqual(profile["selectivity"], "balanced")

        status, updated = self.request(
            "/api/profiles/paper",
            method="PUT",
            payload={
                "interests": ["reinforcement learning"],
                "selectivity": "broad",
                "include_phrases": ["system"],
            },
        )
        self.assertEqual(status, 200)
        self.assertEqual(updated["profile"]["selectivity"], "broad")
        self.assertEqual(self.wait_for_rescore()["state"], "complete")

        status, page = self.request(
            "/api/entries?kind=paper&bucket=relevant&limit=2"
        )
        self.assertEqual(status, 200)
        self.assertLessEqual(len(page["entries"]), 2)
        self.assertTrue(
            all("relevance_components" in entry for entry in page["entries"])
        )
        if page["next_cursor"]:
            status, second = self.request(
                "/api/entries?kind=paper&bucket=relevant&limit=2&cursor="
                + page["next_cursor"]
            )
            self.assertEqual(status, 200)
            self.assertFalse(
                {entry["id"] for entry in page["entries"]}
                & {entry["id"] for entry in second["entries"]}
            )

        entry_id = self.database.list_entries()[0]["id"]
        status, entry = self.request(
            f"/api/entries/{entry_id}/action",
            method="POST",
            payload={"action": "discard"},
        )
        self.assertEqual(status, 200)
        self.assertEqual(entry["status"], "discarded")
        profile = self.database.get_relevance_profile("paper")
        self.assertEqual(
            self.database.list_relevance_feedback(profile["id"])[0]["label"],
            "discard",
        )

    def test_profile_validation_and_health_provenance(self) -> None:
        status, error = self.request(
            "/api/profiles/news",
            method="PUT",
            payload={"interests": [], "selectivity": "impossible"},
        )
        self.assertEqual(status, 400)
        self.assertIn("interest", error["error"])

        status, health = self.request("/api/relevance/health")
        self.assertEqual(status, 200)
        self.assertEqual(health["fallback_provider"], "tfidf-v1")
        self.assertEqual(len(health["profiles"]), 2)


if __name__ == "__main__":
    unittest.main()
