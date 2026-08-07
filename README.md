# Herald

Herald is a local-first inbox for technical news and academic research. It
collects RSS feeds, helps triage articles, produces local summaries, and exports
kept material as Obsidian-compatible Markdown.

The application is currently under construction. The foundation has no runtime
dependencies beyond Python 3.11.

```bash
python -m herald.cli init
python -m herald.cli refresh
python -m herald.cli demo
python -m herald.cli list
python -m herald.cli summarize 1
python -m herald.cli export 1
```

`init` creates the local data directories and seeds RSS feeds for hardware
architecture, operating systems, machine learning, and reinforcement learning.
`refresh` fetches every enabled feed and deduplicates cross-listed articles.
Summaries use the configured local Ollama model when it is reachable and fall
back to deterministic extractive text when it is not. Kept entries export into
category folders below the configured Obsidian vault.
