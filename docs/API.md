# Herald local API contract

All responses use JSON unless otherwise noted.

## Entries

- `GET /api/entries?status=unread&category=Machine%20Learning`
- `GET /api/entries?kind=paper&bucket=relevant&limit=100&cursor=…`
- `GET /api/stats` for full-database status and category counts
- `GET /api/entries/{id}`
- `GET /api/entries/{id}/references`
- `POST /api/entries/{id}/action` with `{"action":"read|unread|keep|discard"}`
- `POST /api/entries/{id}/summarize`
- `POST /api/entries/{id}/obsidian/retry`
- `POST /api/entries/{id}/export` (backward-compatible retry alias)

An entry contains `id`, `title`, `url`, `author`, `published_at`, `content`,
`content_markdown`, `summary`, `status`, `source_title`, `source_category`, and
`exported_path`. `content` is normalized plain text for Herald's reader and
summarizer; `content_markdown` preserves supported rich feed structure for
Obsidian export.
Ranked pages return `{ "entries": […], "next_cursor": "…" }`, are ordered by
score, and include `relevance_score`, `relevance_bucket`, explainable
`relevance_components`, model provenance, and scoring time. The cursor is
opaque. `filtered` entries remain stored and can be paged exactly like
`relevant` entries.

## Full article text

- `GET /api/entries/{id}/fulltext`
- `POST /api/entries/{id}/fulltext/retry`
- `POST /api/entries/{id}/fulltext/pdf` with the PDF itself as the request body
  and `Content-Type: application/pdf`

Every entry response also carries a `fulltext` object, or `null` for entries
Herald has not tried to extract. It reports `state`, `source_kind`,
`source_url`, `format`, `character_count`, `reference_count`, `truncated`,
`pdf_path`, `pdf_bytes`, `attempts`, `error`, and timestamps.

`state` is one of:

| State | Meaning |
| --- | --- |
| `pending` | Queued; nothing has been fetched yet |
| `extracting` | An extraction is running now |
| `extracted` | The note carries the article text |
| `needs_pdf` | No openly available copy exists; upload one to finish the note |
| `failed` | A transient failure worth retrying |
| `not_applicable` | Nothing to extract, such as a news item |

`source_kind` records where the text came from: `arxiv-html`, `arxiv-pdf`,
`open-access-pdf`, `page-pdf`, or `upload`.

Herald reads full text only from a copy the publisher has itself made openly
available, or from a PDF supplied through the upload endpoint. `needs_pdf` is a
normal outcome rather than an error: the entry stays kept, the note is written
with the abstract and a notice, and the upload endpoint completes it. Uploads
are capped at 100 MB and must be PDFs; a scan with no text layer is rejected
with an explanation rather than stored as an empty article.

## Relevance profiles

- `GET /api/profiles/paper`
- `GET /api/profiles/news`
- `PUT /api/profiles/{paper|news}` with any of `interests`, `exclusions`,
  `include_phrases`, `never_show_phrases`, `selectivity`, `threshold`, and
  `target_precision`
- `POST /api/profiles/{paper|news}/rescore` schedules a non-blocking rescore
- `GET /api/relevance/health` reports providers, jobs, results, failures, and
  current profiles

`selectivity` is `broad`, `balanced`, or `focused`. Updating a profile schedules
rescoring and resets its learned threshold unless an explicit threshold is
provided. Balanced cold start follows the strongest part of the observed score
distribution without enforcing an item quota. After at least five Keeps and
fifteen Discards, Herald learns toward the profile's target precision and limits
each threshold change to three points.

Single-entry and status-action responses also contain `obsidian_export`, whose
state is `pending`, `synced`, `failed`, `conflict`, `archive_pending`, or
`archived`.

The `keep` action commits the reading status before attempting the filesystem
sync. Consequently a successful action response may contain a failed or
conflicted `obsidian_export`; use the retry endpoint after correcting the
reported error. Moving a kept entry to another status archives its note outside
the vault, and keeping it again restores the latest annotated copy.

## Obsidian settings

- `GET /api/settings/obsidian`
- `PUT /api/settings/obsidian` with `{"vault_path":"/absolute/existing/vault"}`

Changing vaults archives managed notes from the old vault before restoring kept
notes into the new one. The archive location is managed by Herald and cannot be
placed inside the vault.

## Sources and ingestion

- `GET /api/sources`
- `GET /api/sources/export` returns a portable `herald.sources` JSON document
- `POST /api/sources` with
  `{"title":"…","url":"…","category":"…","content_kind":"paper|news"}`
- `POST /api/sources/import` with a portable source document
- `POST /api/refresh` to fetch all enabled sources

Source export contains only `title`, `url`, `category`, `content_kind`, and
`enabled`. It excludes database IDs, articles, reading state, rankings,
validators, refresh health, and timestamps. Import validates the complete file
before making one atomic, additive merge: matching URLs are updated, missing
URLs are added, and local sources absent from the file are retained. Import
never fetches feeds. The same format is available from the dashboard and with
`herald sources export|import`.

Herald seeds the versioned public catalog embedded from
`internal/sources/data/sources.json`, which
contains the curated research and news feeds shipped with the application. A
source URL may point directly to RSS/Atom or to a page with a standard RSS/Atom
autodiscovery link. Herald does not scrape HTML lists.
The first successful refresh of a News source imports only items published in
the latest 30 days; later refreshes accept every newly observed item.

Source records expose `resolved_url`, `etag`, `last_modified`,
`refresh_attempted_at`, `refresh_succeeded_at`, and `refresh_error`. Refreshes
use conditional HTTP requests and treat `304 Not Modified` as a healthy no-op.
Article URLs are canonicalized and known tracking parameters are removed before
cross-source deduplication.

Keeping an RSS paper with a supported public page starts background enrichment.
Herald inspects the publication page for `citation_*` metadata and combines it
with Semantic Scholar/Crossref data before resynchronizing the Obsidian note.
Paper keywords are written to note properties; outgoing references are rendered
as direct Obsidian links when the cited paper is also kept and synchronized.

## Manual paper import

- `POST /api/import/paper` with `{"input":"10.1145/example"}`

The input may be a DOI, an arXiv ID or URL, a Semantic Scholar paper URL, or a
public HTTPS paper page containing standard `citation_*` metadata. A new paper
is created in the **Manual Imports** source with `unread` status. Repeating the
same import returns the existing entry without changing its triage status.

The response contains `entry`, `created`, `identifiers`, and `keywords`.
It also contains the paper's directed outgoing `references`. Unresolved
references retain their external identifier, title, and URL.
Herald queries Semantic Scholar first, falls back to Crossref for DOI metadata,
and caches successful provider responses locally. Arbitrary page fetches reject
credentials, non-HTTPS/alternate-port URLs, private network destinations, and
responses larger than 2 MiB.

## Citation references

- `GET /api/entries/{id}/references` lists only references cited by that paper.
- `POST /api/references/{reference-id}/add` imports one selected cited paper.

References reconcile idempotently by normalized DOI, arXiv ID, or Semantic
Scholar ID when a matching paper reaches Herald. Add to Herald never crawls the
cited paper's references into entries: a newly added target is `unread`, is not
kept, and is not exported. Repeating Add returns the existing target without
changing its status. Provider failures leave the reference unresolved.

Errors have the shape `{"error":"human-readable message"}` and an appropriate
HTTP status.
