# Herald

Herald is a local-first inbox for technical news and academic research. It
collects RSS feeds, supports read/keep/discard triage, creates local summaries,
and exports kept entries as Obsidian-compatible Markdown.

Herald is a single self-contained binary. The dashboard and the source catalog
are embedded in it, so there is nothing to install alongside it: no runtime, no
virtual environment, no accounts, API keys, hosted services, or paid
subscriptions.

## Install and run

Build the binary with Go 1.26 or newer and run it:

```bash
go build -o herald ./cmd/herald
./herald init
./herald serve
```

The resulting binary is portable and needs no working directory of its own; run
it from wherever you want your `.herald/` data to live. `go install
github.com/PabloMoralesEscandon/herald/cmd/herald@latest` also works.

Open <http://127.0.0.1:8765> and stop the server with Ctrl+C. `herald init`
creates the local data directories and installs the packaged source catalog; it
does not fetch anything. Use the dashboard refresh button when you are ready.

For an offline sample, run `herald demo` before `herald serve`. It adds two
deterministic entries and never contacts a feed.

In the dashboard, select an entry, generate its summary, then keep or discard
it. Keeping an entry automatically creates its Obsidian note. The resulting
Markdown contains frontmatter, summary, feed text, original URL, author,
publication date, and a personal notes section. Herald updates only its marked
generated blocks, so custom properties and personal notes survive every sync.
Rich feed content is converted automatically to Obsidian-compatible Markdown,
preserving common links, headings, lists, emphasis, quotes, code, and tables.

The Papers workspace opens on **Relevant + Unread**, showing the strongest
current matches. **Filtered** retains everything below the adaptive threshold;
filtering never deletes or discards an entry. News is intentionally unfiltered:
it shows every matching status/category item newest-first so announcements can
be triaged quickly. Papers and News still have separate workspaces, counts,
categories, and source health.

## Fetch real sources

`init` loads a versioned catalog of 48 public feeds: 11 arXiv categories for
chip design and digital circuits, operating systems, machine learning, and
reinforcement learning, plus 37 official or established editorial news feeds
covering AI, semiconductors, defense, space, European technology, and startups.
The catalog is [`internal/sources/data/sources.json`](internal/sources/data/sources.json)
and is embedded in the binary. Fetch the
enabled feeds from the dashboard or command line:

```bash
herald refresh
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
`herald --help`.

The same dialog can export and import the entire subscription list as portable
JSON. This moves source configuration without copying articles, reading
history, rankings, refresh metadata, database IDs, or other personal data.
Imports are validated first, merged by canonical URL in one transaction, never
delete unlisted local sources, and never refresh automatically. CLI equivalents:

```bash
herald sources export herald-sources.json
herald sources import herald-sources.json
herald sources                 # backward-compatible detailed local listing
```

An export preserves enabled/disabled choices. Treat exports as private if you
use private feed URLs or query-string tokens; `herald-sources.json` is ignored
by Git by default. The packaged catalog contains public URLs only and is safe to
version with the application.

Papers that are not in a feed can be imported through `POST /api/import/paper`
using a DOI, arXiv ID or URL, Semantic Scholar URL, or public paper page. Herald
enriches the paper with free Semantic Scholar/Crossref metadata, extracts local
keywords, and stores it as unread. Provider responses are cached in the local
database; no API key or paid service is required.

Keeping a paper with a supported public page returns immediately, creates its
baseline note, and starts metadata enrichment in the background. Herald inspects
the linked publication page for author-supplied keywords, then uses free
Semantic Scholar/Crossref metadata for identifiers, topics, and outgoing
citations. The note is safely resynchronized afterward; citations to other kept
Herald papers become direct Obsidian links. Page or provider failure does not
undo Keep or remove the baseline note.

## Full article text and citation links

Keeping a paper does not just save its abstract. Herald reads the article
itself and writes it into the note under **Full text**, with the paper's own
headings, paragraphs, lists, tables, captions, and mathematics preserved.

It reads from the openly available copy, preferring arXiv's HTML rendering
where one exists, then the arXiv PDF, then an open-access PDF a free metadata
provider reports, then the PDF a publication page declares for itself. PDFs are
parsed in-process: there is no `pdftotext` to install and nothing is sent
anywhere. Two-column layouts, ligatures, hyphens broken across lines, running
heads, and page numbers are all handled, so the note reads as prose rather than
as a dump of page fragments.

**In-text citations become Obsidian links.** When a paper cites a work that is
also kept in your vault, the marker in the running text is a real link to that
paper's note, and the two are connected in Obsidian's graph view. A citation to
something you have not kept stays exactly as the article printed it, and it
turns into a link by itself the moment you keep the cited paper. Herald links
only citations it can resolve to a parsed bibliography entry, so a bracketed
matrix index or a parenthesized date is never turned into a false connection.

Not every paper has an open copy. When there is none, the Keep still succeeds
and the note is still written; the entry is marked **Needs your PDF**, the note
carries a notice saying so, and the dashboard offers an **Upload PDF** button
next to the kept paper. Uploading your copy runs it through the same pipeline,
including citation linking. Extraction failures never undo a Keep or remove a
note, and every note records where its text came from in its
`fulltext_state` and `fulltext_source` properties.

From the command line:

```bash
herald fulltext                      # work through kept papers awaiting text
herald fulltext --entry 42           # extract one paper
herald fulltext --entry 42 --pdf paper.pdf   # read a local PDF, offline
```

Extracted and uploaded PDFs are kept in `.herald/pdfs/`, outside the vault, so
a re-extraction can run offline and the file you supplied is not lost.

## Data and Obsidian

By default Herald writes everything below `.herald/` in the current directory:

- `.herald/herald.db` — SQLite database
- `.herald/vault/Herald/Papers/*.md` — kept paper notes
- `.herald/vault/Herald/News/<publisher>/*.md` — kept news notes
- `.herald/obsidian-archive/` — recoverable notes removed from the vault
- `.herald/pdfs/` — PDFs Herald extracted from, and the ones you uploaded

`.herald/`, database files and sidecars, vaults, local exports, `.env` files,
logs, caches, and build output are excluded by `.gitignore`. The
repository contains no fetched articles or reading history. Before pushing,
still inspect `git status` in case you deliberately placed personal data at a
nonstandard path.

Point exports at an existing Obsidian vault by setting `HERALD_VAULT` before
starting Herald or with `PUT /api/settings/obsidian`. The API accepts only an
existing absolute directory. Stable note names do not change with article
titles. Leaving the Kept state moves the note outside the vault into the
archive; keeping it again restores the latest copy and its annotations.

```bash
HERALD_VAULT="/path/to/Obsidian Vault" herald serve
```

Keep remains successful when the vault is unavailable or a note has a safety
conflict. Herald records the sync error and the dashboard offers a retry rather
than rolling back reading state or overwriting an unknown file.

## Optional local AI

If Ollama is running, Herald asks it for a two-to-four-sentence summary. Ollama
is optional: when the default local Ollama service is absent, Herald
automatically uses a deterministic extractive summary and makes no paid or
hosted AI request.

Paper relevance scoring follows the same rule. Herald prefers batched
`embeddinggemma` embeddings from Ollama and caches them locally. If Ollama or
the model is unavailable, it immediately uses its bundled deterministic TF-IDF
scorer. Paper interests, exclusions, exact include rules, and never-show rules
control its ranked queue. Below-threshold papers remain recoverable in the
**Filtered** bucket; the filter never discards an entry or imposes a fixed daily
quota. News bypasses relevance filtering and remains newest-first.

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

[.env.example](.env.example) lists safe local defaults. Herald intentionally
does not load `.env` files itself; copy it to `.env`, edit locally, and load it
through your shell or process manager. The real `.env` remains ignored.

To enable the preferred local models:

```bash
ollama pull embeddinggemma
ollama pull qwen2.5:3b
ollama serve
```

`GET /api/relevance/health` reports the preferred embedding provider, the
always-available TF-IDF fallback, and scoring jobs. Summary
provenance is shown on every preview and exported note, so Ollama output is
never confused with the non-AI fallback.

## How the code is organized

```
cmd/herald/        CLI entry point and the demo fixture
internal/store/    SQLite schema, additive migrations, and every query
internal/feed/     RSS 2.0, RSS 1.0/RDF, and Atom parsing; URL canonicalization
internal/markdown/ feed HTML to Obsidian-compatible Markdown
internal/paper/    DOI/arXiv/S2 identifiers, metadata providers, SSRF guards
internal/pdf/      PDF parsing and positioned text extraction, no dependencies
internal/fulltext/ article HTML and PDF to Markdown, bibliographies, citations
internal/relevance/ TF-IDF and Ollama scoring, thresholds, background rescoring
internal/vault/    note rendering and the safe write/archive/restore lifecycle
internal/service/  ingestion, triage, and vault synchronization
internal/api/      HTTP routes
web/               dashboard assets, embedded with embed.FS
```

`internal/textx` and `internal/urlx` hold small primitives (Unicode-aware
whitespace handling, URL splitting and percent-encoding) that several packages
share. They exist so the rules those packages depend on are stated once rather
than reimplemented slightly differently in each.

## Security and repository status

Herald's web API is unauthenticated. The default `127.0.0.1` binding is for
single-user local operation. Setting `HERALD_HOST` to a non-loopback address
exposes reading data and state-changing endpoints to other machines that can
reach the port; use an authenticated reverse proxy if you intentionally do so.
See [`SECURITY.md`](SECURITY.md) for handling and reporting guidance.

No software license has been selected yet. Before treating the repository as an
open-source project or accepting outside contributions, the owner must choose
and add a license; this repository does not assume one on their behalf.

## Upgrade an existing Herald database

Herald applies additive SQLite schema migrations every time it starts. Existing
read, kept, and discarded states are never reset. After upgrading, run:

```bash
herald backfill
```

This offline-safe command derives identifiers from existing arXiv/DOI URLs,
extracts missing keywords, reconciles known citation edges, rebuilds both
relevance indexes, and safely reconciles kept Obsidian notes. It is idempotent.
Use `--no-obsidian` to inspect or rebuild database metadata without touching a
vault. Legacy notes are migrated only when Herald can identify their generated
structure; ambiguous files become conflicts and are not overwritten.

## Tests

```bash
go test ./...
```

The suite needs no network access and no Ollama. Alongside ordinary unit tests
it carries golden files under `internal/*/testdata/`, which pin the exact output
of the pieces whose formats are contracts: HTML-to-Markdown conversion, URL
canonicalization (the cross-feed deduplication key), feed parsing, and the
rendered Obsidian note. Those goldens exist because their output is written into
files users own and annotate, so any change to them should be a deliberate,
reviewable edit rather than a silent drift.

Run `go test -race ./...` to additionally exercise the background scoring and
enrichment workers under the race detector.
