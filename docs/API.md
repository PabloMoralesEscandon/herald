# Herald local API contract

All responses use JSON unless otherwise noted.

## Entries

- `GET /api/entries?status=unread&category=Machine%20Learning`
- `GET /api/stats` for full-database status and category counts
- `GET /api/entries/{id}`
- `POST /api/entries/{id}/action` with `{"action":"read|unread|keep|discard"}`
- `POST /api/entries/{id}/summarize`
- `POST /api/entries/{id}/obsidian/retry`
- `POST /api/entries/{id}/export` (backward-compatible retry alias)

An entry contains `id`, `title`, `url`, `author`, `published_at`, `content`,
`summary`, `status`, `source_title`, `source_category`, and `exported_path`.
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

Errors have the shape `{"error":"human-readable message"}` and an appropriate
HTTP status.
