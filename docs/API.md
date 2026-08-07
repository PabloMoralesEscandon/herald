# Herald local API contract

All responses use JSON unless otherwise noted.

## Entries

- `GET /api/entries?status=unread&category=Machine%20Learning`
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

Errors have the shape `{"error":"human-readable message"}` and an appropriate
HTTP status.

