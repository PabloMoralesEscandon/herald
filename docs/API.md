# Herald local API contract

All responses use JSON unless otherwise noted.

## Entries

- `GET /api/entries?status=unread&category=Machine%20Learning`
- `GET /api/entries?kind=paper&bucket=relevant&limit=100&cursor=…`
- `GET /api/stats` for full-database status and category counts
- `GET /api/entries/{id}`
- `POST /api/entries/{id}/action` with `{"action":"read|unread|keep|discard"}`
- `POST /api/entries/{id}/summarize`
- `POST /api/entries/{id}/obsidian/retry`
- `POST /api/entries/{id}/export` (backward-compatible retry alias)

An entry contains `id`, `title`, `url`, `author`, `published_at`, `content`,
`summary`, `status`, `source_title`, `source_category`, and `exported_path`.
Ranked pages return `{ "entries": […], "next_cursor": "…" }`, are ordered by
score, and include `relevance_score`, `relevance_bucket`, explainable
`relevance_components`, model provenance, and scoring time. The cursor is
opaque. `filtered` entries remain stored and can be paged exactly like
`relevant` entries.

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
- `POST /api/sources` with `{"title":"…","url":"…","category":"…"}`
- `POST /api/refresh` to fetch all enabled sources

## Manual paper import

- `POST /api/import/paper` with `{"input":"10.1145/example"}`

The input may be a DOI, an arXiv ID or URL, a Semantic Scholar paper URL, or a
public HTTPS paper page containing standard `citation_*` metadata. A new paper
is created in the **Manual Imports** source with `unread` status. Repeating the
same import returns the existing entry without changing its triage status.

The response contains `entry`, `created`, `identifiers`, and `keywords`.
Herald queries Semantic Scholar first, falls back to Crossref for DOI metadata,
and caches successful provider responses locally. Arbitrary page fetches reject
credentials, non-HTTPS/alternate-port URLs, private network destinations, and
responses larger than 2 MiB.

Errors have the shape `{"error":"human-readable message"}` and an appropriate
HTTP status.
