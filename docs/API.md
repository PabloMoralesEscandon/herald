# Herald local API contract

All responses use JSON unless otherwise noted.

## Entries

- `GET /api/entries?status=unread&category=Machine%20Learning`
- `GET /api/entries?kind=paper&bucket=relevant&limit=100&cursor=…`
- `GET /api/stats` for full-database status and category counts
- `GET /api/entries/{id}`
- `POST /api/entries/{id}/action` with `{"action":"read|unread|keep|discard"}`
- `POST /api/entries/{id}/summarize`
- `POST /api/entries/{id}/export`

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

## Sources and ingestion

- `GET /api/sources`
- `POST /api/sources` with `{"title":"…","url":"…","category":"…"}`
- `POST /api/refresh` to fetch all enabled sources

Errors have the shape `{"error":"human-readable message"}` and an appropriate
HTTP status.
