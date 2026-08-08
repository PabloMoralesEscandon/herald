# Herald local API contract

All responses use JSON unless otherwise noted.

## Entries

- `GET /api/entries?status=unread&category=Machine%20Learning`
- `GET /api/stats` for full-database status and category counts
- `GET /api/entries/{id}`
- `POST /api/entries/{id}/action` with `{"action":"read|unread|keep|discard"}`
- `POST /api/entries/{id}/summarize`
- `POST /api/entries/{id}/export`

An entry contains `id`, `title`, `url`, `author`, `published_at`, `content`,
`summary`, `status`, `source_title`, `source_category`, and `exported_path`.

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
