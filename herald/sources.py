from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class CuratedSource:
    title: str
    url: str
    category: str
    content_kind: str = "paper"


RESEARCH_SOURCES = (
    CuratedSource(
        "arXiv: Hardware Architecture",
        "https://rss.arxiv.org/rss/cs.AR",
        "Chip Design & Digital Circuits",
    ),
    CuratedSource(
        "arXiv: Emerging Technologies",
        "https://rss.arxiv.org/rss/cs.ET",
        "Chip Design & Digital Circuits",
    ),
    CuratedSource(
        "arXiv: Operating Systems",
        "https://rss.arxiv.org/rss/cs.OS",
        "Operating Systems",
    ),
    CuratedSource(
        "arXiv: Distributed, Parallel, and Cluster Computing",
        "https://rss.arxiv.org/rss/cs.DC",
        "Operating Systems",
    ),
    CuratedSource(
        "arXiv: Performance",
        "https://rss.arxiv.org/rss/cs.PF",
        "Operating Systems",
    ),
    CuratedSource(
        "arXiv: Machine Learning",
        "https://rss.arxiv.org/rss/cs.LG",
        "Machine Learning",
    ),
    CuratedSource(
        "arXiv: Statistical Machine Learning",
        "https://rss.arxiv.org/rss/stat.ML",
        "Machine Learning",
    ),
    CuratedSource(
        "arXiv: Artificial Intelligence",
        "https://rss.arxiv.org/rss/cs.AI",
        "Machine Learning",
    ),
    CuratedSource(
        "arXiv: Robotics",
        "https://rss.arxiv.org/rss/cs.RO",
        "Reinforcement Learning",
    ),
    CuratedSource(
        "arXiv: Multiagent Systems",
        "https://rss.arxiv.org/rss/cs.MA",
        "Reinforcement Learning",
    ),
    CuratedSource(
        "arXiv: Systems and Control",
        "https://rss.arxiv.org/rss/eess.SY",
        "Reinforcement Learning",
    ),
)


NEWS_SOURCES = (
    CuratedSource(
        "NVIDIA Newsroom",
        "https://nvidianews.nvidia.com/releases.xml",
        "NVIDIA",
        "news",
    ),
    CuratedSource(
        "OpenAI News",
        "https://openai.com/news/rss.xml",
        "OpenAI",
        "news",
    ),
    CuratedSource(
        "AMD Newsroom",
        "https://newsroom.amd.com/rss.xml",
        "AMD",
        "news",
    ),
    CuratedSource(
        "Intel Newsroom",
        "https://newsroom.intel.com/feed",
        "Intel",
        "news",
    ),
)


CURATED_SOURCES = RESEARCH_SOURCES + NEWS_SOURCES
