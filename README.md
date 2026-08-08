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
it. A kept entry can be exported with **Export to Obsidian**. The resulting
Markdown note contains frontmatter, summary, feed text, original URL, author,
publication date, and a blank notes section.

## Fetch real sources

`init` seeds 11 arXiv feeds covering chip design and digital circuits,
operating systems, machine learning, and reinforcement learning. Fetch them
from the dashboard's refresh button or the command line:

```bash
python -m herald.cli refresh
```

Refresh requires internet access. A failed source is reported without losing
entries fetched from the other sources. Repeated refreshes update existing
entries and deduplicate articles cross-listed by URL.

To add another RSS or Atom feed, use `POST /api/sources` as documented in
[`docs/API.md`](docs/API.md). To inspect or automate Herald from the command
line, run `python -m herald.cli --help`.

## Data and Obsidian

By default Herald writes everything below `.herald/` in the current directory:

- `.herald/herald.db` — SQLite database
- `.herald/vault/Herald/<category>/*.md` — exported notes

Point exports at an existing Obsidian vault by setting `HERALD_VAULT` before
starting Herald. Existing exported notes are updated in place.

```bash
HERALD_VAULT="/path/to/Obsidian Vault" python -m herald.cli serve
```

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

## Tests

```bash
python -m unittest -v
```

The suite uses only the Python standard library and does not require network
access or Ollama.
