from __future__ import annotations

import http.client
import json
import re
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Callable, Protocol


DEFAULT_OLLAMA_URL = "http://127.0.0.1:11434"
DEFAULT_OLLAMA_MODEL = "qwen2.5:3b"
MAX_INPUT_CHARACTERS = 12_000
MAX_SUMMARY_CHARACTERS = 2_000


class OllamaError(RuntimeError):
    """Raised when Ollama cannot return a usable completion."""


@dataclass(frozen=True, slots=True)
class SummaryResult:
    text: str
    provider: str
    model: str | None = None
    fallback_reason: str | None = None


class SummaryProvider(Protocol):
    def summarize(self, title: str, content: str) -> SummaryResult: ...


def _normalize(value: str) -> str:
    return re.sub(r"\s+", " ", value).strip()


def _trim_to_word(value: str, maximum: int) -> str:
    if len(value) <= maximum:
        return value
    shortened = value[: maximum - 1].rsplit(" ", 1)[0].rstrip(" ,;:-")
    return (shortened or value[: maximum - 1]).rstrip() + "…"


def deterministic_summary(title: str, content: str, maximum: int = 600) -> str:
    """Produce the same concise extractive summary for the same input."""

    text = _normalize(content)
    abstract = re.search(r"\bAbstract:\s*(.+)", text, flags=re.IGNORECASE)
    if abstract and abstract.start() < 160:
        text = abstract.group(1)
    if not text:
        text = _normalize(title)
    if not text:
        return "No summary is available."

    sentences = re.split(r"(?<=[.!?])\s+(?=[A-Z0-9])", text)
    selected: list[str] = []
    for sentence in sentences:
        candidate = " ".join((*selected, sentence))
        if selected and len(candidate) > maximum:
            break
        selected.append(sentence)
        if len(selected) == 3 or len(candidate) >= maximum:
            break
    return _trim_to_word(" ".join(selected), maximum)


class OllamaClient:
    def __init__(
        self,
        base_url: str = DEFAULT_OLLAMA_URL,
        model: str = DEFAULT_OLLAMA_MODEL,
        *,
        timeout: float = 8.0,
    ):
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout

    def generate(self, prompt: str) -> str:
        payload = json.dumps(
            {
                "model": self.model,
                "prompt": prompt,
                "stream": False,
                "options": {"temperature": 0},
            }
        ).encode("utf-8")
        request = urllib.request.Request(
            f"{self.base_url}/api/generate",
            data=payload,
            headers={
                "Content-Type": "application/json",
                "Accept": "application/json",
            },
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                document = response.read(1024 * 1024 + 1)
        except (
            urllib.error.URLError,
            http.client.HTTPException,
            TimeoutError,
            OSError,
        ) as error:
            raise OllamaError(str(error)) from error
        if len(document) > 1024 * 1024:
            raise OllamaError("Ollama response exceeds the 1 MiB limit")
        try:
            decoded = json.loads(document)
            completion = decoded["response"]
        except (json.JSONDecodeError, KeyError, TypeError) as error:
            raise OllamaError("Ollama returned an invalid response") from error
        if not isinstance(completion, str) or not completion.strip():
            raise OllamaError("Ollama returned an empty summary")
        return completion


Generator = Callable[[str], str]


class LocalSummarizer:
    def __init__(
        self,
        ollama_url: str = DEFAULT_OLLAMA_URL,
        ollama_model: str = DEFAULT_OLLAMA_MODEL,
        *,
        generator: Generator | None = None,
    ):
        self.model = ollama_model
        self.generator = generator or OllamaClient(
            ollama_url, ollama_model
        ).generate

    def summarize(self, title: str, content: str) -> SummaryResult:
        fallback = deterministic_summary(title, content)
        source_text = _normalize(content) or _normalize(title)
        if not source_text:
            return SummaryResult(fallback, "fallback", fallback_reason="empty content")
        prompt = (
            "Summarize the following technical article in 2 to 4 factual sentences. "
            "State the problem, approach, and key result when they are present. "
            "Do not add facts or use a heading.\n\n"
            f"Title: {_normalize(title)}\n\n"
            f"Article: {source_text[:MAX_INPUT_CHARACTERS]}"
        )
        try:
            generated = _normalize(self.generator(prompt))
            if not generated:
                raise OllamaError("Ollama returned an empty summary")
        except (OllamaError, ValueError, TypeError, TimeoutError, OSError) as error:
            return SummaryResult(
                fallback,
                "fallback",
                fallback_reason=str(error) or error.__class__.__name__,
            )
        return SummaryResult(
            _trim_to_word(generated, MAX_SUMMARY_CHARACTERS),
            "ollama",
            model=self.model,
        )
