# Herald

Herald is a local-first inbox for technical news and academic research. It
collects RSS feeds, supports read/keep/discard triage, creates local summaries,
and exports kept entries as Obsidian-compatible Markdown.

It requires only Python 3.11 or newer. There are no required third-party Python
packages, accounts, API keys, hosted services, or paid subscriptions.

## Run the sample

From this repository:

```bash
python -m herald.cli init
python -m herald.cli demo
python -m herald.cli serve
```

Open <http://127.0.0.1:8765>. `demo` adds two deterministic articles, so this
flow works without internet access. Stop the server with Ctrl+C.

In the dashboard, select an entry, generate its summary, then keep or discard
it. Keeping an entry automatically creates its Obsidian note. The resulting
Markdown contains frontmatter, summary, feed text, original URL, author,
publication date, and a personal notes section. Herald updates only its marked
generated blocks, so custom properties and personal notes survive every sync.

The dashboard opens on **Relevant + Unread**, so the inbox contains only the
strongest current matches. Switch to **Filtered** to recover everything below
the adaptive threshold; filtering never deletes or discards an entry. Papers
and News have separate workspaces, profiles, filters, counts, and source health.

## Fetch real sources

`init` seeds 11 arXiv feeds covering chip design and digital circuits,
operating systems, machine learning, and reinforcement learning, plus official
announcement feeds from NVIDIA, OpenAI, AMD, and Intel. Fetch them
from the dashboard's refresh button or the command line:

```bash
python -m herald.cli refresh
```

Refresh requires internet access. A failed source is reported without losing
entries fetched from the other sources. Repeated refreshes update existing
entries and deduplicate articles cross-listed by canonical URL. Herald removes
known tracking parameters, uses ETag/Last-Modified conditional requests, and
records per-source refresh health. The initial News refresh imports only the
latest 30 days; later refreshes ingest every newly observed announcement.

To add another RSS or Atom feed, or a website exposing a standard feed
autodiscovery link, use **Add source** in either dashboard workspace. Choose
whether to fetch its entries immediately; the source and its category appear as
soon as it is saved. Automation can use `POST /api/sources` as documented in
[`docs/API.md`](docs/API.md). To inspect Herald from the command line, run
`python -m herald.cli --help`.

Papers that are not in a feed can be imported through `POST /api/import/paper`
using a DOI, arXiv ID or URL, Semantic Scholar URL, or public paper page. Herald
enriches the paper with free Semantic Scholar/Crossref metadata, extracts local
keywords, and stores it as unread. Provider responses are cached in the local
database; no API key or paid service is required.

Keeping a recognized arXiv, DOI, or Semantic Scholar paper returns immediately,
creates its baseline note, and starts metadata enrichment in the background.
When free provider metadata arrives, Herald adds identifiers, keywords, topics,
and outgoing citations, then safely resynchronizes the note. Provider failure
does not undo Keep or remove the baseline note.

## Data and Obsidian

By default Herald writes everything below `.herald/` in the current directory:

- `.herald/herald.db` — SQLite database
- `.herald/vault/Herald/Papers/*.md` — kept paper notes
- `.herald/vault/Herald/News/<publisher>/*.md` — kept news notes
- `.herald/obsidian-archive/` — recoverable notes removed from the vault

Point exports at an existing Obsidian vault by setting `HERALD_VAULT` before
starting Herald or with `PUT /api/settings/obsidian`. The API accepts only an
existing absolute directory. Stable note names do not change with article
titles. Leaving the Kept state moves the note outside the vault into the
archive; keeping it again restores the latest copy and its annotations.

```bash
HERALD_VAULT="/path/to/Obsidian Vault" python -m herald.cli serve
```

Keep remains successful when the vault is unavailable or a note has a safety
conflict. Herald records the sync error and the dashboard offers a retry rather
than rolling back reading state or overwriting an unknown file.

## Optional local AI

If Ollama is running, Herald asks it for a two-to-four-sentence summary. Ollama
is optional: when the default local Ollama service is absent, Herald
automatically uses a deterministic extractive summary and makes no paid or
hosted AI request.

Relevance scoring follows the same rule. Herald prefers batched
`embeddinggemma` embeddings from Ollama and caches them locally. If Ollama or
the model is unavailable, it immediately uses its bundled deterministic TF-IDF
scorer. Paper and News interests, exclusions, exact include rules, and
never-show rules are independent. Below-threshold entries remain recoverable in
the **Filtered** bucket; the filter never discards an entry or imposes a fixed
daily quota.

Configuration environment variables:

- `HERALD_DATA_DIR` — database and default vault root (default `.herald`)
- `HERALD_DATABASE` — SQLite database path
- `HERALD_VAULT` — Obsidian vault or export folder
- `HERALD_HOST` — server interface (default `127.0.0.1`)
- `HERALD_PORT` — dashboard port (default `8765`)
- `HERALD_OLLAMA_URL` — Ollama address (default `http://127.0.0.1:11434`)
- `HERALD_OLLAMA_MODEL` — local model name (default `qwen2.5:3b`)
- `HERALD_OLLAMA_EMBEDDING_MODEL` — local relevance model (default
  `embeddinggemma`)

To enable the preferred local models:

```bash
ollama pull embeddinggemma
ollama pull qwen2.5:3b
ollama serve
```

`GET /api/relevance/health` reports the preferred embedding provider, the
always-available TF-IDF fallback, and each Paper/News scoring job. Summary
provenance is shown on every preview and exported note, so Ollama output is
never confused with the non-AI fallback.

## Upgrade an existing Herald database

Herald applies additive SQLite schema migrations every time it starts. Existing
read, kept, and discarded states are never reset. After upgrading, run:

```bash
python -m herald.cli backfill
```

This offline-safe command derives identifiers from existing arXiv/DOI URLs,
extracts missing keywords, reconciles known citation edges, rebuilds both
relevance indexes, and safely reconciles kept Obsidian notes. It is idempotent.
Use `--no-obsidian` to inspect or rebuild database metadata without touching a
vault. Legacy notes are migrated only when Herald can identify their generated
structure; ambiguous files become conflicts and are not overwritten.

## Tests

```bash
python -m unittest -v
```

The suite uses only the Python standard library and does not require network
access or Ollama.
