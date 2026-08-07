from __future__ import annotations

import sqlite3
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Iterator


VALID_STATUSES = {"unread", "read", "kept", "discarded"}


SCHEMA = """
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY,
    title TEXT NOT NULL,
    url TEXT NOT NULL UNIQUE,
    category TEXT NOT NULL DEFAULT 'Unsorted',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS entries (
    id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    guid TEXT NOT NULL,
    url TEXT NOT NULL,
    title TEXT NOT NULL,
    author TEXT NOT NULL DEFAULT '',
    published_at TEXT,
    discovered_at TEXT NOT NULL,
    content TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unread'
        CHECK (status IN ('unread', 'read', 'kept', 'discarded')),
    exported_path TEXT,
    UNIQUE(source_id, guid)
);

CREATE INDEX IF NOT EXISTS entries_status_idx ON entries(status);
CREATE INDEX IF NOT EXISTS entries_published_idx ON entries(published_at DESC);
CREATE INDEX IF NOT EXISTS entries_url_idx ON entries(url);
"""


def utc_now() -> str:
    return datetime.now(UTC).replace(microsecond=0).isoformat()


class Database:
    def __init__(self, path: str | Path):
        self.path = Path(path)

    @contextmanager
    def connect(self) -> Iterator[sqlite3.Connection]:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        connection = sqlite3.connect(self.path)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys = ON")
        try:
            yield connection
            connection.commit()
        finally:
            connection.close()

    def initialize(self) -> None:
        with self.connect() as connection:
            connection.executescript(SCHEMA)

    def add_source(self, title: str, url: str, category: str = "Unsorted") -> int:
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO sources(title, url, category, created_at)
                VALUES (?, ?, ?, ?)
                ON CONFLICT(url) DO UPDATE SET
                    title = excluded.title,
                    category = excluded.category
                """,
                (title.strip(), url.strip(), category.strip(), utc_now()),
            )
            row = connection.execute(
                "SELECT id FROM sources WHERE url = ?", (url.strip(),)
            ).fetchone()
            return int(row["id"])

    def list_sources(self, enabled_only: bool = False) -> list[dict[str, Any]]:
        query = "SELECT * FROM sources"
        params: tuple[Any, ...] = ()
        if enabled_only:
            query += " WHERE enabled = 1"
        query += " ORDER BY category, title"
        with self.connect() as connection:
            return [dict(row) for row in connection.execute(query, params)]

    def upsert_entry(
        self,
        *,
        source_id: int,
        guid: str,
        url: str,
        title: str,
        author: str = "",
        published_at: str | None = None,
        content: str = "",
        summary: str = "",
    ) -> tuple[int, bool]:
        with self.connect() as connection:
            existing = connection.execute(
                """
                SELECT id FROM entries
                WHERE (source_id = ? AND guid = ?)
                   OR (? <> '' AND url = ?)
                ORDER BY CASE
                    WHEN source_id = ? AND guid = ? THEN 0
                    ELSE 1
                END
                LIMIT 1
                """,
                (source_id, guid, url, url, source_id, guid),
            ).fetchone()
            if existing:
                connection.execute(
                    """
                    UPDATE entries
                    SET url = ?, title = ?, author = ?, published_at = ?,
                        content = CASE WHEN ? <> '' THEN ? ELSE content END,
                        summary = CASE WHEN ? <> '' THEN ? ELSE summary END
                    WHERE id = ?
                    """,
                    (
                        url,
                        title,
                        author,
                        published_at,
                        content,
                        content,
                        summary,
                        summary,
                        existing["id"],
                    ),
                )
                return int(existing["id"]), False

            cursor = connection.execute(
                """
                INSERT INTO entries(
                    source_id, guid, url, title, author, published_at,
                    discovered_at, content, summary
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    source_id,
                    guid,
                    url,
                    title,
                    author,
                    published_at,
                    utc_now(),
                    content,
                    summary,
                ),
            )
            return int(cursor.lastrowid), True

    def get_entry(self, entry_id: int) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                """
                SELECT entries.*, sources.title AS source_title,
                       sources.category AS source_category
                FROM entries JOIN sources ON sources.id = entries.source_id
                WHERE entries.id = ?
                """,
                (entry_id,),
            ).fetchone()
            return dict(row) if row else None

    def list_entries(
        self,
        *,
        status: str | None = None,
        category: str | None = None,
        limit: int = 100,
    ) -> list[dict[str, Any]]:
        clauses: list[str] = []
        params: list[Any] = []
        if status:
            if status not in VALID_STATUSES:
                raise ValueError(f"Unknown status: {status}")
            clauses.append("entries.status = ?")
            params.append(status)
        if category:
            clauses.append("sources.category = ?")
            params.append(category)
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        params.append(max(1, min(limit, 500)))
        with self.connect() as connection:
            rows = connection.execute(
                f"""
                SELECT entries.*, sources.title AS source_title,
                       sources.category AS source_category
                FROM entries JOIN sources ON sources.id = entries.source_id
                {where}
                ORDER BY COALESCE(published_at, discovered_at) DESC, entries.id DESC
                LIMIT ?
                """,
                params,
            ).fetchall()
            return [dict(row) for row in rows]

    def entry_counts(self) -> dict[str, Any]:
        with self.connect() as connection:
            total = int(connection.execute("SELECT COUNT(*) FROM entries").fetchone()[0])
            statuses = {status: 0 for status in sorted(VALID_STATUSES)}
            statuses.update(
                {
                    str(row["status"]): int(row["count"])
                    for row in connection.execute(
                        "SELECT status, COUNT(*) AS count FROM entries GROUP BY status"
                    )
                }
            )
            categories = {
                str(row["category"]): int(row["count"])
                for row in connection.execute(
                    """
                    SELECT sources.category, COUNT(entries.id) AS count
                    FROM sources
                    LEFT JOIN entries ON entries.source_id = sources.id
                    GROUP BY sources.category
                    ORDER BY sources.category
                    """
                )
            }
        return {"total": total, "statuses": statuses, "categories": categories}

    def set_status(self, entry_id: int, status: str) -> bool:
        if status not in VALID_STATUSES:
            raise ValueError(f"Unknown status: {status}")
        with self.connect() as connection:
            cursor = connection.execute(
                "UPDATE entries SET status = ? WHERE id = ?", (status, entry_id)
            )
            return cursor.rowcount == 1

    def set_summary(self, entry_id: int, summary: str) -> bool:
        with self.connect() as connection:
            cursor = connection.execute(
                "UPDATE entries SET summary = ? WHERE id = ?", (summary, entry_id)
            )
            return cursor.rowcount == 1

    def set_exported_path(self, entry_id: int, exported_path: str) -> bool:
        with self.connect() as connection:
            cursor = connection.execute(
                "UPDATE entries SET exported_path = ? WHERE id = ?",
                (exported_path, entry_id),
            )
            return cursor.rowcount == 1
