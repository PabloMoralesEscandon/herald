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

Configuration environment variables:

- `HERALD_DATA_DIR` — database and default vault root (default `.herald`)
- `HERALD_DATABASE` — SQLite database path
- `HERALD_VAULT` — Obsidian vault or export folder
- `HERALD_HOST` — server interface (default `127.0.0.1`)
- `HERALD_PORT` — dashboard port (default `8765`)
- `HERALD_OLLAMA_URL` — Ollama address (default `http://127.0.0.1:11434`)
- `HERALD_OLLAMA_MODEL` — local model name (default `qwen2.5:3b`)

## Tests

```bash
python -m unittest -v
```

The suite uses only the Python standard library and does not require network
access or Ollama.
