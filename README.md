# Herald

Herald is a local-first inbox for technical news and academic research. It
collects RSS feeds, helps triage articles, produces local summaries, and exports
kept material as Obsidian-compatible Markdown.

The working sample has no runtime dependencies beyond Python 3.11. No account,
API key, hosted service, or paid subscription is required.

```bash
python -m herald.cli init
python -m herald.cli refresh
python -m herald.cli demo
python -m herald.cli serve
```

Open <http://127.0.0.1:8765>. The demo command adds two deterministic entries,
so the dashboard is useful even when the computer is offline. Use the refresh
button to fetch the configured feeds. Select an entry to generate a summary,
keep or discard it, and export kept entries to the Markdown vault.

`init` creates the local data directories and seeds RSS feeds for hardware
architecture, operating systems, machine learning, and reinforcement learning.
`refresh` fetches every enabled feed and deduplicates cross-listed articles.
Summaries use the configured local Ollama model when it is reachable and fall
back to deterministic extractive text when it is not. Kept entries export into
category folders below the configured Obsidian vault.

## Configuration

All files remain local. Defaults can be changed with environment variables:

- `HERALD_DATA_DIR` — database and default vault root (default `.herald`)
- `HERALD_VAULT` — an existing Obsidian vault or export folder
- `HERALD_PORT` — local dashboard port (default `8765`)
- `HERALD_OLLAMA_URL` — Ollama address (default `http://127.0.0.1:11434`)
- `HERALD_OLLAMA_MODEL` — local model name (default `qwen2.5:3b`)

Ollama is optional. If it is absent, Herald creates a deterministic extractive
summary without making an external API request.
