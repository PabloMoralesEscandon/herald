from __future__ import annotations

from urllib.parse import urlsplit


ARXIV_HOSTS = {"arxiv.org", "www.arxiv.org", "export.arxiv.org"}


def arxiv_pdf_url(url: str) -> str | None:
    """Return the canonical PDF URL for an arXiv abstract or PDF URL."""

    parts = urlsplit(url)
    if parts.hostname not in ARXIV_HOSTS:
        return None
    if parts.path.startswith("/abs/"):
        identifier = parts.path.removeprefix("/abs/")
    elif parts.path.startswith("/pdf/"):
        identifier = parts.path.removeprefix("/pdf/")
    else:
        return None
    identifier = identifier.removesuffix(".pdf").strip("/")
    if not identifier:
        return None
    return f"https://arxiv.org/pdf/{identifier}"

