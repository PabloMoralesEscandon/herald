from __future__ import annotations

import hashlib
import json
import math
import re
import struct
import threading
import urllib.error
import urllib.request
from collections import Counter, defaultdict
from dataclasses import dataclass
from typing import Any, Protocol, Sequence

from .storage import Database, VALID_CONTENT_KINDS, utc_now


PAPER_INTERESTS = [
    "chip design and computer architecture",
    "digital circuits and hardware design",
    "machine learning",
    "reinforcement learning",
    "operating systems and systems research",
]
NEWS_INTERESTS = [
    "new processors GPUs accelerators and chip architectures",
    "machine learning model launches and research releases",
    "developer platforms operating systems and infrastructure",
]
DEFAULT_PROFILES = {"paper": PAPER_INTERESTS, "news": NEWS_INTERESTS}
SELECTIVITY_PERCENTILES = {"broad": 0.70, "balanced": 0.90, "focused": 0.95}
TOKEN_RE = re.compile(r"[a-z0-9]+(?:[-'][a-z0-9]+)?")
STOP_WORDS = {
    "a", "an", "and", "are", "as", "at", "be", "by", "for", "from",
    "in", "into", "is", "it", "of", "on", "or", "that", "the", "their",
    "this", "to", "using", "we", "with",
}


class EmbeddingError(RuntimeError):
    """The optional embedding provider could not produce usable vectors."""


class EmbeddingProvider(Protocol):
    model: str

    def embed(self, texts: Sequence[str]) -> list[list[float]]: ...


def _text_hash(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def _pack_vector(vector: Sequence[float]) -> bytes:
    return struct.pack(f"<{len(vector)}f", *vector)


def _unpack_vector(blob: bytes, dimensions: int) -> list[float]:
    expected = dimensions * 4
    if len(blob) != expected:
        raise EmbeddingError("Cached embedding has invalid dimensions")
    return list(struct.unpack(f"<{dimensions}f", blob))


class OllamaEmbeddingProvider:
    """Small batched Ollama `/api/embed` client with a persistent SQLite cache."""

    def __init__(
        self,
        database: Database,
        base_url: str = "http://127.0.0.1:11434",
        model: str = "embeddinggemma",
        *,
        timeout: float = 4.0,
        batch_size: int = 32,
    ) -> None:
        self.database = database
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout
        self.batch_size = batch_size

    def embed(self, texts: Sequence[str]) -> list[list[float]]:
        if not texts:
            return []
        results: list[list[float] | None] = [None] * len(texts)
        missing: list[tuple[int, str, str]] = []
        for index, text in enumerate(texts):
            digest = _text_hash(text)
            cached = self.database.get_embedding(digest, self.model)
            if cached is None:
                missing.append((index, text, digest))
            else:
                results[index] = _unpack_vector(
                    bytes(cached["vector"]), int(cached["dimensions"])
                )

        for offset in range(0, len(missing), self.batch_size):
            batch = missing[offset : offset + self.batch_size]
            payload = json.dumps(
                {"model": self.model, "input": [text for _, text, _ in batch]}
            ).encode()
            request = urllib.request.Request(
                f"{self.base_url}/api/embed",
                data=payload,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            try:
                with urllib.request.urlopen(request, timeout=self.timeout) as response:
                    document = json.loads(response.read(20 * 1024 * 1024))
            except (
                urllib.error.URLError,
                TimeoutError,
                OSError,
                json.JSONDecodeError,
            ) as error:
                raise EmbeddingError(f"Ollama embeddings unavailable: {error}") from error
            vectors = document.get("embeddings") if isinstance(document, dict) else None
            if not isinstance(vectors, list) or len(vectors) != len(batch):
                raise EmbeddingError("Ollama returned an invalid embedding batch")
            for (index, _text, digest), vector in zip(batch, vectors, strict=True):
                if not isinstance(vector, list) or not vector:
                    raise EmbeddingError("Ollama returned an empty embedding")
                try:
                    numeric = [float(value) for value in vector]
                except (TypeError, ValueError) as error:
                    raise EmbeddingError("Ollama returned a non-numeric embedding") from error
                self.database.put_embedding(
                    digest, self.model, len(numeric), _pack_vector(numeric)
                )
                results[index] = numeric
        if any(vector is None for vector in results):
            raise EmbeddingError("Embedding batch was incomplete")
        return [vector for vector in results if vector is not None]


def _tokens(text: str) -> list[str]:
    return [
        token for token in TOKEN_RE.findall(text.casefold())
        if token not in STOP_WORDS and len(token) > 1
    ]


def _tfidf_vectors(texts: Sequence[str]) -> list[dict[str, float]]:
    documents = [_tokens(text) for text in texts]
    frequencies = [Counter(document) for document in documents]
    document_frequency: Counter[str] = Counter()
    for frequency in frequencies:
        document_frequency.update(frequency.keys())
    count = max(1, len(documents))
    vectors: list[dict[str, float]] = []
    for frequency in frequencies:
        vector = {
            term: (1.0 + math.log(value))
            * (math.log((1.0 + count) / (1.0 + document_frequency[term])) + 1.0)
            for term, value in frequency.items()
        }
        norm = math.sqrt(sum(value * value for value in vector.values())) or 1.0
        vectors.append({term: value / norm for term, value in vector.items()})
    return vectors


def _cosine(left: Sequence[float], right: Sequence[float]) -> float:
    if len(left) != len(right) or not left:
        return 0.0
    left_norm = math.sqrt(sum(value * value for value in left))
    right_norm = math.sqrt(sum(value * value for value in right))
    if not left_norm or not right_norm:
        return 0.0
    return sum(a * b for a, b in zip(left, right, strict=True)) / (
        left_norm * right_norm
    )


def _sparse_cosine(left: dict[str, float], right: dict[str, float]) -> float:
    if len(left) > len(right):
        left, right = right, left
    return sum(value * right.get(term, 0.0) for term, value in left.items())


def _percentile(values: Sequence[float], fraction: float) -> float:
    if not values:
        return 50.0
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(fraction * len(ordered)) - 1))
    return ordered[index]


def _entry_text(entry: dict[str, Any]) -> str:
    # Repeating the title implements the product's explicit 2x title weighting.
    return f"{entry['title']}\n{entry['title']}\n{entry.get('content', '')}".strip()


@dataclass(slots=True)
class ScoreDraft:
    entry: dict[str, Any]
    score: float
    components: dict[str, float]
    explanation: dict[str, Any]
    forced_filtered: bool


class RelevanceEngine:
    def __init__(
        self,
        database: Database,
        embedding_provider: EmbeddingProvider | None = None,
    ) -> None:
        self.database = database
        self.embedding_provider = embedding_provider
        self.ensure_default_profiles()

    def ensure_default_profiles(self) -> None:
        for kind, interests in DEFAULT_PROFILES.items():
            profile = self.database.get_relevance_profile(kind)
            if profile is None:
                profile = self.database.upsert_relevance_profile(kind, interests=interests)
            setting = f"relevance.threshold_mode.{profile['id']}"
            if self.database.get_setting(setting) is None:
                self.database.set_setting(setting, "auto")

    def update_profile(self, content_kind: str, payload: dict[str, Any]) -> dict[str, Any]:
        if content_kind not in VALID_CONTENT_KINDS:
            raise ValueError(f"Unknown content kind: {content_kind}")
        current = self.database.get_relevance_profile(content_kind)
        assert current is not None
        allowed = {
            "interests", "exclusions", "include_phrases", "never_show_phrases",
            "selectivity", "threshold", "target_precision",
        }
        unknown = set(payload) - allowed
        if unknown:
            raise ValueError(f"Unknown profile fields: {', '.join(sorted(unknown))}")
        for key in ("interests", "exclusions", "include_phrases", "never_show_phrases"):
            if key in payload and (
                not isinstance(payload[key], list)
                or not all(isinstance(item, str) for item in payload[key])
            ):
                raise ValueError(f"{key} must be a list of strings")
        interests = payload.get("interests", current["interests"])
        if not interests:
            raise ValueError("A relevance profile needs at least one interest")
        profile_definition_changed = bool(
            set(payload)
            & {
                "interests",
                "exclusions",
                "include_phrases",
                "never_show_phrases",
                "selectivity",
            }
        )
        threshold = payload.get(
            "threshold",
            None if profile_definition_changed else current["threshold"],
        )
        profile = self.database.upsert_relevance_profile(
            content_kind,
            interests=interests,
            exclusions=payload.get("exclusions", current["exclusions"]),
            include_phrases=payload.get("include_phrases", current["include_phrases"]),
            never_show_phrases=payload.get(
                "never_show_phrases", current["never_show_phrases"]
            ),
            selectivity=payload.get("selectivity", current["selectivity"]),
            threshold=threshold,
            target_precision=float(payload.get("target_precision", current["target_precision"])),
        )
        if "threshold" in payload and payload["threshold"] is not None:
            self.database.set_setting(
                f"relevance.threshold_mode.{profile['id']}", "manual"
            )
        elif profile_definition_changed or "threshold" in payload:
            self.database.set_setting(
                f"relevance.threshold_mode.{profile['id']}", "auto"
            )
        return profile

    def rescore(self, content_kind: str) -> dict[str, Any]:
        profile = self.database.get_relevance_profile(content_kind)
        if profile is None:
            raise ValueError(f"Unknown content kind: {content_kind}")
        entries = self.database.list_entries(content_kind=content_kind, limit=None)
        feedback = self.database.list_relevance_feedback(int(profile["id"]), limit=200)
        feedback_entries = [
            (item, self.database.get_entry(int(item["entry_id"]))) for item in feedback
        ]
        drafts, model, fallback_reason = self._score_entries(
            entries, profile, feedback_entries
        )
        threshold, threshold_mode = self._choose_threshold(profile, drafts, feedback)
        self.database.set_relevance_threshold(int(profile["id"]), threshold)
        if threshold_mode == "adaptive-feedback":
            self.database.set_setting(
                f"relevance.threshold_mode.{profile['id']}", "adaptive"
            )
        relevant = 0
        for draft in drafts:
            bucket = (
                "filtered"
                if draft.forced_filtered or draft.score < threshold
                else "relevant"
            )
            relevant += int(bucket == "relevant")
            explanation = {
                **draft.explanation,
                "threshold": round(threshold, 3),
                "threshold_mode": threshold_mode,
                "decision": (
                    "never-show phrase matched"
                    if draft.forced_filtered
                    else f"score {'meets' if bucket == 'relevant' else 'is below'} threshold"
                ),
            }
            self.database.upsert_entry_ranking(
                int(draft.entry["id"]),
                int(profile["id"]),
                score=round(draft.score, 4),
                bucket=bucket,
                components=draft.components,
                explanation=explanation,
                model=model,
            )
        return {
            "content_kind": content_kind,
            "scored": len(drafts),
            "relevant": relevant,
            "filtered": len(drafts) - relevant,
            "threshold": round(threshold, 3),
            "threshold_mode": threshold_mode,
            "model": model,
            "fallback_reason": fallback_reason,
        }

    def _score_entries(
        self,
        entries: list[dict[str, Any]],
        profile: dict[str, Any],
        feedback_entries: list[tuple[dict[str, Any], dict[str, Any] | None]],
    ) -> tuple[list[ScoreDraft], str, str | None]:
        if not entries:
            return [], "tfidf", None
        interests_text = " ; ".join(profile["interests"])
        exclusions_text = " ; ".join(profile["exclusions"])
        positive_text = "\n".join(
            _entry_text(entry) for item, entry in feedback_entries
            if item["label"] == "keep" and entry is not None
        )
        negative_text = "\n".join(
            _entry_text(entry) for item, entry in feedback_entries
            if item["label"] == "discard" and entry is not None
        )
        document_texts = [_entry_text(entry) for entry in entries]
        query_texts = [
            interests_text,
            exclusions_text or "__none__",
            positive_text or "__none__",
            negative_text or "__none__",
        ]
        fallback_reason: str | None = None
        dense_vectors: list[list[float]] | None = None
        if self.embedding_provider is not None:
            try:
                dense_vectors = self.embedding_provider.embed([*document_texts, *query_texts])
            except EmbeddingError as error:
                fallback_reason = str(error)

        if dense_vectors is not None:
            document_vectors: Sequence[Any] = dense_vectors[: len(entries)]
            query_vectors: Sequence[Any] = dense_vectors[len(entries) :]
            similarity = _cosine
            model = self.embedding_provider.model
        else:
            sparse = _tfidf_vectors([*document_texts, *query_texts])
            document_vectors = sparse[: len(entries)]
            query_vectors = sparse[len(entries) :]
            similarity = _sparse_cosine
            model = "tfidf-v1"

        source_feedback: dict[int, Counter[str]] = defaultdict(Counter)
        for item, entry in feedback_entries:
            if entry is not None:
                source_feedback[int(entry["source_id"])][str(item["label"])] += 1

        drafts: list[ScoreDraft] = []
        for entry, vector in zip(entries, document_vectors, strict=True):
            semantic_ratio = max(0.0, similarity(vector, query_vectors[0]))
            exclusion_ratio = (
                max(0.0, similarity(vector, query_vectors[1]))
                if exclusions_text else 0.0
            )
            positive = (
                max(0.0, similarity(vector, query_vectors[2]))
                if positive_text else 0.0
            )
            negative = (
                max(0.0, similarity(vector, query_vectors[3]))
                if negative_text else 0.0
            )
            feedback_ratio = max(-1.0, min(1.0, positive - negative))
            lexical_ratio, matched_interests = self._lexical_match(entry, profile["interests"])
            searchable = _entry_text(entry).casefold()
            include_matches = [
                phrase for phrase in profile["include_phrases"]
                if phrase.casefold() in searchable
            ]
            never_matches = [
                phrase for phrase in profile["never_show_phrases"]
                if phrase.casefold() in searchable
            ]
            source_counts = source_feedback.get(int(entry["source_id"]), Counter())
            source_total = source_counts["keep"] + source_counts["discard"]
            source_ratio = source_counts["keep"] / source_total if source_total else 0.0
            components = {
                "semantic_interest": round(70.0 * semantic_ratio, 4),
                "lexical_interest": round(25.0 * lexical_ratio, 4),
                "feedback_affinity": round(15.0 * feedback_ratio, 4),
                "source_affinity": round(5.0 * source_ratio, 4),
                "semantic_exclusion": round(-25.0 * exclusion_ratio, 4),
                "exact_include": round(min(10.0, 5.0 * len(include_matches)), 4),
            }
            score = max(0.0, min(100.0, sum(components.values())))
            drafts.append(
                ScoreDraft(
                    entry=entry,
                    score=score,
                    components=components,
                    explanation={
                        "matched_interests": matched_interests,
                        "include_matches": include_matches,
                        "never_show_matches": never_matches,
                        "embedding_provider": model,
                        "embedding_fallback_reason": fallback_reason,
                    },
                    forced_filtered=bool(never_matches),
                )
            )
        return drafts, model, fallback_reason

    @staticmethod
    def _lexical_match(
        entry: dict[str, Any], interests: Sequence[str]
    ) -> tuple[float, list[str]]:
        title_tokens = set(_tokens(str(entry["title"])))
        content_tokens = set(_tokens(str(entry.get("content", ""))))
        best = 0.0
        matched: list[str] = []
        for interest in interests:
            interest_tokens = set(_tokens(interest))
            if not interest_tokens:
                continue
            overlap = sum(
                2 if token in title_tokens else 1 if token in content_tokens else 0
                for token in interest_tokens
            )
            ratio = min(1.0, overlap / (2.0 * len(interest_tokens)))
            if ratio > 0:
                matched.append(interest)
            best = max(best, ratio)
        return best, matched

    def _choose_threshold(
        self,
        profile: dict[str, Any],
        drafts: Sequence[ScoreDraft],
        feedback: Sequence[dict[str, Any]],
    ) -> tuple[float, str]:
        scores = [draft.score for draft in drafts if not draft.forced_filtered]
        selectivity = str(profile["selectivity"])
        # Calibrate to the observed distribution. A fixed floor can empty a
        # perfectly useful offline TF-IDF queue because its cosine scores are
        # naturally lower than dense embedding scores.
        cold = _percentile(scores, SELECTIVITY_PERCENTILES[selectivity])
        current = float(profile["threshold"]) if profile["threshold"] is not None else cold
        configured_mode = self.database.get_setting(
            f"relevance.threshold_mode.{profile['id']}", "auto"
        )
        if configured_mode == "manual" and profile["threshold"] is not None:
            return current, "manual"
        keeps = sum(item["label"] == "keep" for item in feedback)
        discards = sum(item["label"] == "discard" for item in feedback)
        if keeps < 5 or discards < 15:
            return max(0.0, min(100.0, cold)), "cold-start"

        by_entry = {int(draft.entry["id"]): draft.score for draft in drafts}
        labelled = [
            (by_entry[int(item["entry_id"])], str(item["label"]))
            for item in feedback if int(item["entry_id"]) in by_entry
        ]
        candidates: list[float] = []
        for candidate in sorted({score for score, _label in labelled}):
            selected = [label for score, label in labelled if score >= candidate]
            true_positive = selected.count("keep")
            precision = true_positive / len(selected) if selected else 0.0
            if true_positive >= 5 and precision >= float(profile["target_precision"]):
                candidates.append(candidate)
        if not candidates:
            return current, "adaptive-no-safe-threshold"
        learned = min(candidates)
        # Prevent a small feedback batch from abruptly emptying or flooding the queue.
        smoothed = max(current - 3.0, min(current + 3.0, learned))
        return max(0.0, min(100.0, smoothed)), "adaptive-feedback"


class RelevanceCoordinator:
    """Runs at most one background rescore per kind and exposes observable jobs."""

    def __init__(self, engine: RelevanceEngine) -> None:
        self.engine = engine
        self._lock = threading.Lock()
        self._jobs: dict[str, dict[str, Any]] = {}

    def start(self, content_kind: str) -> dict[str, Any]:
        if content_kind not in VALID_CONTENT_KINDS:
            raise ValueError(f"Unknown content kind: {content_kind}")
        with self._lock:
            current = self._jobs.get(content_kind)
            if current and current["state"] == "running":
                current["rerun_requested"] = True
                return dict(current)
            job = {
                "content_kind": content_kind,
                "state": "running",
                "started_at": utc_now(),
                "finished_at": None,
                "result": None,
                "error": None,
                "rerun_requested": False,
            }
            self._jobs[content_kind] = job
        threading.Thread(
            target=self._run,
            args=(content_kind,),
            name=f"herald-rank-{content_kind}",
            daemon=True,
        ).start()
        return dict(job)

    def _run(self, content_kind: str) -> None:
        rerun = False
        try:
            result = self.engine.rescore(content_kind)
        except Exception as error:  # Background failures must remain observable.
            with self._lock:
                rerun = bool(self._jobs[content_kind]["rerun_requested"])
                self._jobs[content_kind].update(
                    state="failed",
                    error=str(error),
                    finished_at=utc_now(),
                    rerun_requested=False,
                )
        else:
            with self._lock:
                rerun = bool(self._jobs[content_kind]["rerun_requested"])
                self._jobs[content_kind].update(
                    state="complete",
                    result=result,
                    finished_at=utc_now(),
                    rerun_requested=False,
                )
        if rerun:
            self.start(content_kind)

    def health(self) -> dict[str, Any]:
        with self._lock:
            jobs = {kind: dict(job) for kind, job in self._jobs.items()}
        provider = self.engine.embedding_provider
        return {
            "preferred_provider": provider.model if provider is not None else "tfidf-v1",
            "fallback_provider": "tfidf-v1",
            "jobs": jobs,
            "profiles": self.engine.database.list_relevance_profiles(),
        }
