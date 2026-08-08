from __future__ import annotations

import base64
import binascii
import json
import sqlite3
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Iterator


VALID_STATUSES = {"unread", "read", "kept", "discarded"}
VALID_CONTENT_KINDS = {"paper", "news"}
VALID_ENRICHMENT_STATES = {"pending", "enriched", "failed", "not_applicable"}
VALID_RELEVANCE_BUCKETS = {"pending", "relevant", "filtered"}
VALID_FEEDBACK_LABELS = {"keep", "discard"}
VALID_SELECTIVITY_LEVELS = {"broad", "balanced", "focused"}
VALID_EXPORT_STATES = {
    "pending",
    "synced",
    "failed",
    "conflict",
    "archive_pending",
    "archived",
}


SCHEMA = """
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS sources (
    id INTEGER PRIMARY KEY,
    title TEXT NOT NULL,
    url TEXT NOT NULL UNIQUE,
    category TEXT NOT NULL DEFAULT 'Unsorted',
    enabled INTEGER NOT NULL DEFAULT 1,
    content_kind TEXT NOT NULL DEFAULT 'paper'
        CHECK (content_kind IN ('paper', 'news')),
    adapter TEXT NOT NULL DEFAULT 'feed',
    etag TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    refresh_attempted_at TEXT,
    refresh_succeeded_at TEXT,
    refresh_error TEXT NOT NULL DEFAULT '',
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
    summary_provider TEXT NOT NULL DEFAULT '',
    summary_model TEXT NOT NULL DEFAULT '',
    summary_generated_at TEXT,
    status TEXT NOT NULL DEFAULT 'unread'
        CHECK (status IN ('unread', 'read', 'kept', 'discarded')),
    exported_path TEXT,
    content_kind TEXT NOT NULL DEFAULT 'paper'
        CHECK (content_kind IN ('paper', 'news')),
    canonical_url TEXT NOT NULL DEFAULT '',
    canonical_key TEXT,
    enrichment_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (enrichment_status IN ('pending', 'enriched', 'failed', 'not_applicable')),
    enrichment_provider TEXT NOT NULL DEFAULT '',
    enrichment_error TEXT NOT NULL DEFAULT '',
    enriched_at TEXT,
    updated_at TEXT,
    UNIQUE(source_id, guid)
);

CREATE INDEX IF NOT EXISTS entries_status_idx ON entries(status);
CREATE INDEX IF NOT EXISTS entries_published_idx ON entries(published_at DESC);
CREATE INDEX IF NOT EXISTS entries_url_idx ON entries(url);
"""


EXTENDED_SCHEMA = """
CREATE INDEX IF NOT EXISTS sources_kind_idx ON sources(content_kind, enabled);
CREATE INDEX IF NOT EXISTS entries_kind_idx ON entries(content_kind);
CREATE INDEX IF NOT EXISTS entries_canonical_key_idx ON entries(canonical_key);

CREATE TABLE IF NOT EXISTS paper_identifiers (
    entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    scheme TEXT NOT NULL,
    value TEXT NOT NULL,
    is_primary INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    PRIMARY KEY (entry_id, scheme, value),
    UNIQUE (scheme, value)
);

CREATE INDEX IF NOT EXISTS paper_identifiers_entry_idx
    ON paper_identifiers(entry_id, is_primary DESC);

CREATE TABLE IF NOT EXISTS paper_references (
    id INTEGER PRIMARY KEY,
    citing_entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    cited_entry_id INTEGER REFERENCES entries(id) ON DELETE SET NULL,
    reference_key TEXT NOT NULL,
    external_scheme TEXT NOT NULL DEFAULT '',
    external_id TEXT NOT NULL DEFAULT '',
    cited_title TEXT NOT NULL DEFAULT '',
    cited_url TEXT NOT NULL DEFAULT '',
    position INTEGER,
    provider TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (citing_entry_id, reference_key)
);

CREATE INDEX IF NOT EXISTS paper_references_citing_idx
    ON paper_references(citing_entry_id, position, id);
CREATE INDEX IF NOT EXISTS paper_references_cited_idx
    ON paper_references(cited_entry_id) WHERE cited_entry_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS paper_references_external_idx
    ON paper_references(external_scheme, external_id);

CREATE TABLE IF NOT EXISTS entry_keywords (
    entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    keyword TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'keyword'
        CHECK (kind IN ('keyword', 'topic')),
    score REAL,
    provider TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    PRIMARY KEY (entry_id, kind, keyword)
);

CREATE TABLE IF NOT EXISTS relevance_profiles (
    id INTEGER PRIMARY KEY,
    content_kind TEXT NOT NULL UNIQUE
        CHECK (content_kind IN ('paper', 'news')),
    interests_json TEXT NOT NULL DEFAULT '[]',
    exclusions_json TEXT NOT NULL DEFAULT '[]',
    include_phrases_json TEXT NOT NULL DEFAULT '[]',
    never_show_phrases_json TEXT NOT NULL DEFAULT '[]',
    selectivity TEXT NOT NULL DEFAULT 'balanced',
    threshold REAL,
    target_precision REAL NOT NULL DEFAULT 0.82,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS entry_rankings (
    entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    profile_id INTEGER NOT NULL REFERENCES relevance_profiles(id) ON DELETE CASCADE,
    score REAL NOT NULL,
    bucket TEXT NOT NULL DEFAULT 'pending'
        CHECK (bucket IN ('pending', 'relevant', 'filtered')),
    components_json TEXT NOT NULL DEFAULT '{}',
    explanation_json TEXT NOT NULL DEFAULT '{}',
    model TEXT NOT NULL DEFAULT '',
    scored_at TEXT NOT NULL,
    PRIMARY KEY (entry_id, profile_id)
);

CREATE INDEX IF NOT EXISTS entry_rankings_queue_idx
    ON entry_rankings(profile_id, bucket, score DESC, entry_id DESC);

CREATE TABLE IF NOT EXISTS entry_feedback (
    entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    profile_id INTEGER NOT NULL REFERENCES relevance_profiles(id) ON DELETE CASCADE,
    label TEXT NOT NULL CHECK (label IN ('keep', 'discard')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (entry_id, profile_id)
);

CREATE INDEX IF NOT EXISTS entry_feedback_profile_idx
    ON entry_feedback(profile_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS embedding_cache (
    content_hash TEXT NOT NULL,
    model TEXT NOT NULL,
    dimensions INTEGER NOT NULL,
    vector BLOB NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (content_hash, model)
);

CREATE TABLE IF NOT EXISTS application_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_cache (
    provider TEXT NOT NULL,
    cache_key TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    fetched_at TEXT NOT NULL,
    PRIMARY KEY (provider, cache_key)
);

CREATE TABLE IF NOT EXISTS obsidian_exports (
    entry_id INTEGER PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
    vault_path TEXT NOT NULL DEFAULT '',
    relative_path TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'synced', 'failed', 'conflict',
                         'archive_pending', 'archived')),
    content_hash TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    synced_at TEXT,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS obsidian_exports_state_idx
    ON obsidian_exports(state, updated_at);

CREATE TABLE IF NOT EXISTS obsidian_archives (
    id INTEGER PRIMARY KEY,
    entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
    original_relative_path TEXT NOT NULL,
    archive_path TEXT NOT NULL,
    archived_at TEXT NOT NULL,
    restored_at TEXT
);

CREATE INDEX IF NOT EXISTS obsidian_archives_entry_idx
    ON obsidian_archives(entry_id, archived_at DESC);
"""


def utc_now() -> str:
    return datetime.now(UTC).replace(microsecond=0).isoformat()


def encode_entry_cursor(sort_at: str, entry_id: int) -> str:
    payload = json.dumps([sort_at, entry_id], separators=(",", ":")).encode()
    return base64.urlsafe_b64encode(payload).decode().rstrip("=")


def decode_entry_cursor(cursor: str) -> tuple[str, int]:
    try:
        padded = cursor + "=" * (-len(cursor) % 4)
        payload = json.loads(base64.urlsafe_b64decode(padded).decode())
        if (
            not isinstance(payload, list)
            or len(payload) != 2
            or not isinstance(payload[0], str)
            or not isinstance(payload[1], int)
        ):
            raise ValueError
        return payload[0], payload[1]
    except (
        ValueError,
        UnicodeDecodeError,
        json.JSONDecodeError,
        binascii.Error,
    ) as error:
        raise ValueError("Invalid entry cursor") from error


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
        except Exception:
            connection.rollback()
            raise
        finally:
            connection.close()

    def initialize(self) -> None:
        with self.connect() as connection:
            connection.executescript(SCHEMA)
            source_columns = {
                str(row["name"])
                for row in connection.execute("PRAGMA table_info(sources)")
            }
            source_migrations = {
                "content_kind": "TEXT NOT NULL DEFAULT 'paper'",
                "adapter": "TEXT NOT NULL DEFAULT 'feed'",
                "etag": "TEXT NOT NULL DEFAULT ''",
                "last_modified": "TEXT NOT NULL DEFAULT ''",
                "refresh_attempted_at": "TEXT",
                "refresh_succeeded_at": "TEXT",
                "refresh_error": "TEXT NOT NULL DEFAULT ''",
            }
            for name, definition in source_migrations.items():
                if name not in source_columns:
                    connection.execute(
                        f"ALTER TABLE sources ADD COLUMN {name} {definition}"
                    )

            entry_columns = {
                str(row["name"])
                for row in connection.execute("PRAGMA table_info(entries)")
            }
            entry_migrations = {
                "summary_provider": "TEXT NOT NULL DEFAULT ''",
                "summary_model": "TEXT NOT NULL DEFAULT ''",
                "summary_generated_at": "TEXT",
                "content_kind": "TEXT NOT NULL DEFAULT 'paper'",
                "canonical_url": "TEXT NOT NULL DEFAULT ''",
                "canonical_key": "TEXT",
                "enrichment_status": "TEXT NOT NULL DEFAULT 'pending'",
                "enrichment_provider": "TEXT NOT NULL DEFAULT ''",
                "enrichment_error": "TEXT NOT NULL DEFAULT ''",
                "enriched_at": "TEXT",
                "updated_at": "TEXT",
            }
            for name, definition in entry_migrations.items():
                if name not in entry_columns:
                    connection.execute(
                        f"ALTER TABLE entries ADD COLUMN {name} {definition}"
                    )
            connection.executescript(EXTENDED_SCHEMA)
            connection.execute(
                """
                UPDATE entries
                SET summary_provider = 'unknown'
                WHERE summary <> '' AND summary_provider = ''
                """
            )
            connection.execute(
                "UPDATE entries SET canonical_url = url WHERE canonical_url = ''"
            )
            connection.execute(
                """
                UPDATE entries
                SET updated_at = COALESCE(updated_at, discovered_at)
                WHERE updated_at IS NULL
                """
            )
            connection.execute(
                """
                INSERT INTO obsidian_exports(
                    entry_id, relative_path, state, synced_at, updated_at
                )
                SELECT id, exported_path, 'synced', updated_at, updated_at
                FROM entries
                WHERE exported_path IS NOT NULL AND exported_path <> ''
                ON CONFLICT(entry_id) DO NOTHING
                """
            )
            connection.execute("PRAGMA user_version = 3")

    def add_source(
        self,
        title: str,
        url: str,
        category: str = "Unsorted",
        *,
        content_kind: str = "paper",
        adapter: str = "feed",
    ) -> int:
        if content_kind not in VALID_CONTENT_KINDS:
            raise ValueError(f"Unknown content kind: {content_kind}")
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO sources(
                    title, url, category, content_kind, adapter, created_at
                )
                VALUES (?, ?, ?, ?, ?, ?)
                ON CONFLICT(url) DO UPDATE SET
                    title = excluded.title,
                    category = excluded.category,
                    content_kind = excluded.content_kind,
                    adapter = excluded.adapter
                """,
                (
                    title.strip(),
                    url.strip(),
                    category.strip(),
                    content_kind,
                    adapter.strip() or "feed",
                    utc_now(),
                ),
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
        summary_provider: str = "",
        summary_model: str = "",
        content_kind: str | None = None,
        canonical_url: str | None = None,
        canonical_key: str | None = None,
    ) -> tuple[int, bool]:
        if content_kind is not None and content_kind not in VALID_CONTENT_KINDS:
            raise ValueError(f"Unknown content kind: {content_kind}")
        with self.connect() as connection:
            source = connection.execute(
                "SELECT content_kind FROM sources WHERE id = ?", (source_id,)
            ).fetchone()
            if source is None:
                raise KeyError(f"Source {source_id} does not exist")
            resolved_kind = content_kind or str(source["content_kind"])
            resolved_canonical_url = (canonical_url or url).strip()
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
                        summary = CASE
                            WHEN summary = '' AND ? <> '' THEN ?
                            ELSE summary
                        END,
                        summary_provider = CASE
                            WHEN summary = '' AND ? <> '' THEN ?
                            ELSE summary_provider
                        END,
                        summary_model = CASE
                            WHEN summary = '' AND ? <> '' THEN ?
                            ELSE summary_model
                        END,
                        summary_generated_at = CASE
                            WHEN summary = '' AND ? <> '' THEN ?
                            ELSE summary_generated_at
                        END,
                        content_kind = ?,
                        canonical_url = ?,
                        canonical_key = COALESCE(?, canonical_key),
                        updated_at = ?
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
                        summary,
                        summary_provider,
                        summary,
                        summary_model,
                        summary,
                        utc_now(),
                        resolved_kind,
                        resolved_canonical_url,
                        canonical_key,
                        utc_now(),
                        existing["id"],
                    ),
                )
                return int(existing["id"]), False

            cursor = connection.execute(
                """
                INSERT INTO entries(
                    source_id, guid, url, title, author, published_at,
                    discovered_at, content, summary, summary_provider,
                    summary_model, summary_generated_at, content_kind,
                    canonical_url, canonical_key, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
                    summary_provider,
                    summary_model,
                    utc_now() if summary else None,
                    resolved_kind,
                    resolved_canonical_url,
                    canonical_key,
                    utc_now(),
                ),
            )
            return int(cursor.lastrowid), True

    def get_entry(self, entry_id: int) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                """
                SELECT entries.*, sources.title AS source_title,
                       sources.category AS source_category,
                       sources.adapter AS source_adapter
                FROM entries JOIN sources ON sources.id = entries.source_id
                WHERE entries.id = ?
                """,
                (entry_id,),
            ).fetchone()
            return dict(row) if row else None

    def find_entry_by_canonical_key(self, canonical_key: str) -> dict[str, Any] | None:
        normalized = canonical_key.strip().lower()
        if not normalized:
            return None
        with self.connect() as connection:
            row = connection.execute(
                """
                SELECT entries.*, sources.title AS source_title,
                       sources.category AS source_category,
                       sources.adapter AS source_adapter
                FROM entries JOIN sources ON sources.id = entries.source_id
                WHERE LOWER(entries.canonical_key) = ?
                ORDER BY entries.id LIMIT 1
                """,
                (normalized,),
            ).fetchone()
            return dict(row) if row else None

    def find_entry_by_url(self, url: str) -> dict[str, Any] | None:
        normalized = url.strip()
        if not normalized:
            return None
        with self.connect() as connection:
            row = connection.execute(
                """
                SELECT entries.*, sources.title AS source_title,
                       sources.category AS source_category,
                       sources.adapter AS source_adapter
                FROM entries JOIN sources ON sources.id = entries.source_id
                WHERE entries.url = ? OR entries.canonical_url = ?
                ORDER BY entries.id LIMIT 1
                """,
                (normalized, normalized),
            ).fetchone()
            return dict(row) if row else None

    def update_entry_metadata(
        self,
        entry_id: int,
        *,
        title: str,
        author: str = "",
        published_at: str | None = None,
        content: str = "",
        url: str | None = None,
        canonical_url: str | None = None,
        canonical_key: str | None = None,
    ) -> bool:
        """Augment an entry without replacing populated fields with blanks."""
        now = utc_now()
        with self.connect() as connection:
            cursor = connection.execute(
                """
                UPDATE entries
                SET title = CASE WHEN ? <> '' THEN ? ELSE title END,
                    author = CASE WHEN ? <> '' THEN ? ELSE author END,
                    published_at = COALESCE(?, published_at),
                    content = CASE WHEN ? <> '' THEN ? ELSE content END,
                    url = COALESCE(?, url),
                    canonical_url = COALESCE(?, canonical_url),
                    canonical_key = COALESCE(?, canonical_key),
                    updated_at = ?
                WHERE id = ?
                """,
                (
                    title.strip(), title.strip(), author.strip(), author.strip(),
                    published_at, content.strip(), content.strip(), url,
                    canonical_url, canonical_key, now, entry_id,
                ),
            )
            return cursor.rowcount == 1

    def list_entries(
        self,
        *,
        status: str | None = None,
        category: str | None = None,
        content_kind: str | None = None,
        relevance_bucket: str | None = None,
        profile_id: int | None = None,
        cursor: str | None = None,
        limit: int | None = 100,
    ) -> list[dict[str, Any]]:
        clauses: list[str] = []
        params: list[Any] = []
        join_params: list[Any] = []
        if status:
            if status not in VALID_STATUSES:
                raise ValueError(f"Unknown status: {status}")
            clauses.append("entries.status = ?")
            params.append(status)
        if category:
            clauses.append("sources.category = ?")
            params.append(category)
        if content_kind:
            if content_kind not in VALID_CONTENT_KINDS:
                raise ValueError(f"Unknown content kind: {content_kind}")
            clauses.append("entries.content_kind = ?")
            params.append(content_kind)
        ranking_join = ""
        ranking_fields = ""
        ranked_order = False
        if profile_id is not None:
            ranking_join = (
                "JOIN entry_rankings AS selected_ranking "
                "ON selected_ranking.entry_id = entries.id "
                "AND selected_ranking.profile_id = ?"
            )
            join_params.append(profile_id)
            ranking_fields = (
                ", selected_ranking.score AS relevance_score"
                ", selected_ranking.bucket AS relevance_bucket"
                ", selected_ranking.components_json AS relevance_components_json"
                ", selected_ranking.explanation_json AS relevance_explanation_json"
                ", selected_ranking.model AS relevance_model"
                ", selected_ranking.scored_at AS relevance_scored_at"
            )
            ranked_order = True
        if relevance_bucket:
            if relevance_bucket not in VALID_RELEVANCE_BUCKETS:
                raise ValueError(f"Unknown relevance bucket: {relevance_bucket}")
            if profile_id is not None:
                clauses.append("selected_ranking.bucket = ?")
                params.append(relevance_bucket)
            else:
                clauses.append(
                    "EXISTS (SELECT 1 FROM entry_rankings "
                    "WHERE entry_rankings.entry_id = entries.id "
                    "AND entry_rankings.bucket = ?)"
                )
                params.append(relevance_bucket)
        if cursor:
            sort_at, entry_id = decode_entry_cursor(cursor)
            if ranked_order:
                try:
                    sort_score = float(sort_at)
                except ValueError as error:
                    raise ValueError("Invalid ranked entry cursor") from error
                clauses.append(
                    "(selected_ranking.score < ? OR "
                    "(selected_ranking.score = ? AND entries.id < ?))"
                )
                params.extend((sort_score, sort_score, entry_id))
            else:
                clauses.append(
                    "(COALESCE(entries.published_at, entries.discovered_at) < ? "
                    "OR (COALESCE(entries.published_at, entries.discovered_at) = ? "
                    "AND entries.id < ?))"
                )
                params.extend((sort_at, sort_at, entry_id))
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        limit_clause = ""
        if limit is not None:
            if limit < 1:
                raise ValueError("Limit must be at least 1")
            limit_clause = "LIMIT ?"
            params.append(limit)
        with self.connect() as connection:
            rows = connection.execute(
                f"""
                SELECT entries.*, sources.title AS source_title,
                       sources.category AS source_category,
                       sources.adapter AS source_adapter
                       {ranking_fields}
                FROM entries JOIN sources ON sources.id = entries.source_id
                {ranking_join}
                {where}
                ORDER BY {"selected_ranking.score" if ranked_order else "COALESCE(published_at, discovered_at)"} DESC,
                         entries.id DESC
                {limit_clause}
                """,
                [*join_params, *params],
            ).fetchall()
            results = [dict(row) for row in rows]
            for result in results:
                if "relevance_components_json" in result:
                    result["relevance_components"] = json.loads(
                        str(result.pop("relevance_components_json"))
                    )
                    result["relevance_explanation"] = json.loads(
                        str(result.pop("relevance_explanation_json"))
                    )
            return results

    def list_entries_page(
        self,
        *,
        limit: int = 100,
        **filters: Any,
    ) -> dict[str, Any]:
        """Return a stable page without imposing a hidden maximum page size."""
        if limit < 1:
            raise ValueError("Limit must be at least 1")
        rows = self.list_entries(limit=limit + 1, **filters)
        has_more = len(rows) > limit
        entries = rows[:limit]
        next_cursor = None
        if has_more and entries:
            last = entries[-1]
            sort_value = (
                str(last["relevance_score"])
                if "relevance_score" in last
                else str(last["published_at"] or last["discovered_at"])
            )
            next_cursor = encode_entry_cursor(
                sort_value, int(last["id"])
            )
        return {"entries": entries, "next_cursor": next_cursor}

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
            content_kinds = {kind: 0 for kind in sorted(VALID_CONTENT_KINDS)}
            content_kinds.update(
                {
                    str(row["content_kind"]): int(row["count"])
                    for row in connection.execute(
                        """
                        SELECT content_kind, COUNT(*) AS count
                        FROM entries GROUP BY content_kind
                        """
                    )
                }
            )
            relevance = {
                kind: {"pending": content_kinds[kind], "relevant": 0, "filtered": 0}
                for kind in sorted(VALID_CONTENT_KINDS)
            }
            for row in connection.execute(
                """
                SELECT relevance_profiles.content_kind, entry_rankings.bucket,
                       COUNT(*) AS count
                FROM entry_rankings
                JOIN relevance_profiles
                  ON relevance_profiles.id = entry_rankings.profile_id
                JOIN entries ON entries.id = entry_rankings.entry_id
                WHERE entries.content_kind = relevance_profiles.content_kind
                GROUP BY relevance_profiles.content_kind, entry_rankings.bucket
                """
            ):
                kind = str(row["content_kind"])
                bucket = str(row["bucket"])
                relevance[kind][bucket] = int(row["count"])
            for kind in relevance:
                relevance[kind]["pending"] = max(
                    0,
                    content_kinds[kind]
                    - relevance[kind]["relevant"]
                    - relevance[kind]["filtered"],
                )
        return {
            "total": total,
            "statuses": statuses,
            "categories": categories,
            "content_kinds": content_kinds,
            "relevance": relevance,
        }

    def set_status(self, entry_id: int, status: str) -> bool:
        if status not in VALID_STATUSES:
            raise ValueError(f"Unknown status: {status}")
        with self.connect() as connection:
            cursor = connection.execute(
                "UPDATE entries SET status = ?, updated_at = ? WHERE id = ?",
                (status, utc_now(), entry_id),
            )
            return cursor.rowcount == 1

    def set_summary(
        self,
        entry_id: int,
        summary: str,
        *,
        provider: str,
        model: str | None = None,
    ) -> bool:
        with self.connect() as connection:
            cursor = connection.execute(
                """
                UPDATE entries
                SET summary = ?, summary_provider = ?, summary_model = ?,
                    summary_generated_at = ?, updated_at = ?
                WHERE id = ?
                """,
                (summary, provider, model or "", utc_now(), utc_now(), entry_id),
            )
            return cursor.rowcount == 1

    def set_exported_path(self, entry_id: int, exported_path: str) -> bool:
        with self.connect() as connection:
            cursor = connection.execute(
                "UPDATE entries SET exported_path = ? WHERE id = ?",
                (exported_path, entry_id),
            )
            if cursor.rowcount == 1:
                now = utc_now()
                connection.execute(
                    """
                    INSERT INTO obsidian_exports(
                        entry_id, relative_path, state, synced_at, updated_at
                    ) VALUES (?, ?, 'synced', ?, ?)
                    ON CONFLICT(entry_id) DO UPDATE SET
                        relative_path = excluded.relative_path,
                        state = 'synced',
                        error = '',
                        synced_at = excluded.synced_at,
                        updated_at = excluded.updated_at
                    """,
                    (entry_id, exported_path, now, now),
                )
            return cursor.rowcount == 1

    def update_source_refresh_state(
        self,
        source_id: int,
        *,
        succeeded: bool,
        etag: str | None = None,
        last_modified: str | None = None,
        error: str = "",
    ) -> bool:
        now = utc_now()
        with self.connect() as connection:
            cursor = connection.execute(
                """
                UPDATE sources
                SET refresh_attempted_at = ?,
                    refresh_succeeded_at = CASE WHEN ? THEN ? ELSE refresh_succeeded_at END,
                    refresh_error = CASE WHEN ? THEN '' ELSE ? END,
                    etag = COALESCE(?, etag),
                    last_modified = COALESCE(?, last_modified)
                WHERE id = ?
                """,
                (
                    now,
                    int(succeeded),
                    now,
                    int(succeeded),
                    error,
                    etag,
                    last_modified,
                    source_id,
                ),
            )
            return cursor.rowcount == 1

    def set_enrichment_state(
        self,
        entry_id: int,
        state: str,
        *,
        provider: str = "",
        error: str = "",
        canonical_url: str | None = None,
        canonical_key: str | None = None,
    ) -> bool:
        if state not in VALID_ENRICHMENT_STATES:
            raise ValueError(f"Unknown enrichment state: {state}")
        now = utc_now()
        with self.connect() as connection:
            cursor = connection.execute(
                """
                UPDATE entries
                SET enrichment_status = ?, enrichment_provider = ?,
                    enrichment_error = ?,
                    enriched_at = CASE WHEN ? = 'enriched' THEN ? ELSE enriched_at END,
                    canonical_url = COALESCE(?, canonical_url),
                    canonical_key = COALESCE(?, canonical_key),
                    updated_at = ?
                WHERE id = ?
                """,
                (
                    state,
                    provider,
                    error,
                    state,
                    now,
                    canonical_url,
                    canonical_key,
                    now,
                    entry_id,
                ),
            )
            return cursor.rowcount == 1

    @staticmethod
    def _normalize_identifier(scheme: str, value: str) -> tuple[str, str]:
        normalized_scheme = scheme.strip().lower()
        normalized_value = value.strip()
        if not normalized_scheme or not normalized_value:
            raise ValueError("Identifier scheme and value cannot be empty")
        if normalized_scheme in {"doi", "arxiv"}:
            normalized_value = normalized_value.lower()
        return normalized_scheme, normalized_value

    def add_paper_identifier(
        self,
        entry_id: int,
        scheme: str,
        value: str,
        *,
        is_primary: bool = False,
    ) -> bool:
        scheme, value = self._normalize_identifier(scheme, value)
        with self.connect() as connection:
            if is_primary:
                connection.execute(
                    "UPDATE paper_identifiers SET is_primary = 0 WHERE entry_id = ?",
                    (entry_id,),
                )
            cursor = connection.execute(
                """
                INSERT INTO paper_identifiers(
                    entry_id, scheme, value, is_primary, created_at
                ) VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(entry_id, scheme, value) DO UPDATE SET
                    is_primary = MAX(is_primary, excluded.is_primary)
                """,
                (entry_id, scheme, value, int(is_primary), utc_now()),
            )
            return cursor.rowcount == 1

    def list_paper_identifiers(self, entry_id: int) -> list[dict[str, Any]]:
        with self.connect() as connection:
            return [
                dict(row)
                for row in connection.execute(
                    """
                    SELECT * FROM paper_identifiers
                    WHERE entry_id = ?
                    ORDER BY is_primary DESC, scheme, value
                    """,
                    (entry_id,),
                )
            ]

    def find_entry_by_identifier(
        self, scheme: str, value: str
    ) -> dict[str, Any] | None:
        scheme, value = self._normalize_identifier(scheme, value)
        with self.connect() as connection:
            row = connection.execute(
                """
                SELECT entries.*, sources.title AS source_title,
                       sources.category AS source_category,
                       sources.adapter AS source_adapter
                FROM paper_identifiers
                JOIN entries ON entries.id = paper_identifiers.entry_id
                JOIN sources ON sources.id = entries.source_id
                WHERE paper_identifiers.scheme = ? AND paper_identifiers.value = ?
                """,
                (scheme, value),
            ).fetchone()
            return dict(row) if row else None

    def upsert_paper_reference(
        self,
        citing_entry_id: int,
        reference_key: str,
        *,
        cited_entry_id: int | None = None,
        external_scheme: str = "",
        external_id: str = "",
        cited_title: str = "",
        cited_url: str = "",
        position: int | None = None,
        provider: str = "",
    ) -> int:
        reference_key = reference_key.strip()
        if not reference_key:
            raise ValueError("Reference key cannot be empty")
        if external_scheme or external_id:
            external_scheme, external_id = self._normalize_identifier(
                external_scheme, external_id
            )
        now = utc_now()
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO paper_references(
                    citing_entry_id, cited_entry_id, reference_key,
                    external_scheme, external_id, cited_title, cited_url,
                    position, provider, created_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(citing_entry_id, reference_key) DO UPDATE SET
                    cited_entry_id = excluded.cited_entry_id,
                    external_scheme = excluded.external_scheme,
                    external_id = excluded.external_id,
                    cited_title = excluded.cited_title,
                    cited_url = excluded.cited_url,
                    position = excluded.position,
                    provider = excluded.provider,
                    updated_at = excluded.updated_at
                """,
                (
                    citing_entry_id,
                    cited_entry_id,
                    reference_key,
                    external_scheme,
                    external_id,
                    cited_title,
                    cited_url,
                    position,
                    provider,
                    now,
                    now,
                ),
            )
            row = connection.execute(
                """
                SELECT id FROM paper_references
                WHERE citing_entry_id = ? AND reference_key = ?
                """,
                (citing_entry_id, reference_key),
            ).fetchone()
            return int(row["id"])

    def list_paper_references(self, entry_id: int) -> list[dict[str, Any]]:
        with self.connect() as connection:
            return [
                dict(row)
                for row in connection.execute(
                    """
                    SELECT paper_references.*,
                           entries.status AS cited_status,
                           entries.exported_path AS cited_exported_path
                    FROM paper_references
                    LEFT JOIN entries ON entries.id = paper_references.cited_entry_id
                    WHERE paper_references.citing_entry_id = ?
                    ORDER BY paper_references.position IS NULL,
                             paper_references.position, paper_references.id
                    """,
                    (entry_id,),
                )
            ]

    def replace_entry_keywords(
        self,
        entry_id: int,
        keywords: list[dict[str, Any]],
    ) -> None:
        now = utc_now()
        with self.connect() as connection:
            connection.execute("DELETE FROM entry_keywords WHERE entry_id = ?", (entry_id,))
            for keyword in keywords:
                kind = str(keyword.get("kind", "keyword"))
                if kind not in {"keyword", "topic"}:
                    raise ValueError(f"Unknown keyword kind: {kind}")
                value = str(keyword.get("keyword", "")).strip()
                if not value:
                    raise ValueError("Keyword cannot be empty")
                connection.execute(
                    """
                    INSERT INTO entry_keywords(
                        entry_id, keyword, kind, score, provider, created_at
                    ) VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (
                        entry_id,
                        value,
                        kind,
                        keyword.get("score"),
                        str(keyword.get("provider", "")),
                        now,
                    ),
                )

    def list_entry_keywords(self, entry_id: int) -> list[dict[str, Any]]:
        with self.connect() as connection:
            return [
                dict(row)
                for row in connection.execute(
                    """
                    SELECT * FROM entry_keywords WHERE entry_id = ?
                    ORDER BY kind, score DESC, keyword
                    """,
                    (entry_id,),
                )
            ]

    @staticmethod
    def _decode_profile(row: sqlite3.Row | None) -> dict[str, Any] | None:
        if row is None:
            return None
        result = dict(row)
        for name in (
            "interests",
            "exclusions",
            "include_phrases",
            "never_show_phrases",
        ):
            result[name] = json.loads(str(result.pop(f"{name}_json")))
        return result

    def upsert_relevance_profile(
        self,
        content_kind: str,
        *,
        interests: list[str],
        exclusions: list[str] | None = None,
        include_phrases: list[str] | None = None,
        never_show_phrases: list[str] | None = None,
        selectivity: str = "balanced",
        threshold: float | None = None,
        target_precision: float = 0.82,
    ) -> dict[str, Any]:
        if content_kind not in VALID_CONTENT_KINDS:
            raise ValueError(f"Unknown content kind: {content_kind}")
        if selectivity not in VALID_SELECTIVITY_LEVELS:
            raise ValueError(f"Unknown selectivity: {selectivity}")
        if threshold is not None and not 0.0 <= threshold <= 100.0:
            raise ValueError("Threshold must be between 0 and 100")
        if not 0.0 <= target_precision <= 1.0:
            raise ValueError("Target precision must be between 0 and 1")
        cleaned_lists = []
        for values_list in (
            interests,
            exclusions or [],
            include_phrases or [],
            never_show_phrases or [],
        ):
            cleaned: list[str] = []
            seen: set[str] = set()
            for value in values_list:
                normalized = str(value).strip()
                if normalized and normalized.casefold() not in seen:
                    cleaned.append(normalized)
                    seen.add(normalized.casefold())
            cleaned_lists.append(cleaned)
        now = utc_now()
        values = tuple(json.dumps(value) for value in cleaned_lists)
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO relevance_profiles(
                    content_kind, interests_json, exclusions_json,
                    include_phrases_json, never_show_phrases_json,
                    selectivity, threshold, target_precision, created_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(content_kind) DO UPDATE SET
                    interests_json = excluded.interests_json,
                    exclusions_json = excluded.exclusions_json,
                    include_phrases_json = excluded.include_phrases_json,
                    never_show_phrases_json = excluded.never_show_phrases_json,
                    selectivity = excluded.selectivity,
                    threshold = excluded.threshold,
                    target_precision = excluded.target_precision,
                    updated_at = excluded.updated_at
                """,
                (
                    content_kind,
                    *values,
                    selectivity,
                    threshold,
                    target_precision,
                    now,
                    now,
                ),
            )
            row = connection.execute(
                "SELECT * FROM relevance_profiles WHERE content_kind = ?",
                (content_kind,),
            ).fetchone()
            decoded = self._decode_profile(row)
            assert decoded is not None
            return decoded

    def get_relevance_profile(self, content_kind: str) -> dict[str, Any] | None:
        if content_kind not in VALID_CONTENT_KINDS:
            raise ValueError(f"Unknown content kind: {content_kind}")
        with self.connect() as connection:
            row = connection.execute(
                "SELECT * FROM relevance_profiles WHERE content_kind = ?",
                (content_kind,),
            ).fetchone()
            return self._decode_profile(row)

    def list_relevance_profiles(self) -> list[dict[str, Any]]:
        with self.connect() as connection:
            return [
                profile
                for row in connection.execute(
                    "SELECT * FROM relevance_profiles ORDER BY content_kind"
                )
                if (profile := self._decode_profile(row)) is not None
            ]

    def set_relevance_threshold(self, profile_id: int, threshold: float) -> bool:
        if not 0.0 <= threshold <= 100.0:
            raise ValueError("Threshold must be between 0 and 100")
        with self.connect() as connection:
            cursor = connection.execute(
                "UPDATE relevance_profiles SET threshold = ?, updated_at = ? WHERE id = ?",
                (threshold, utc_now(), profile_id),
            )
            return cursor.rowcount == 1

    def upsert_entry_ranking(
        self,
        entry_id: int,
        profile_id: int,
        *,
        score: float,
        bucket: str,
        components: dict[str, float] | None = None,
        explanation: dict[str, Any] | None = None,
        model: str = "",
    ) -> dict[str, Any]:
        if bucket not in VALID_RELEVANCE_BUCKETS:
            raise ValueError(f"Unknown relevance bucket: {bucket}")
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO entry_rankings(
                    entry_id, profile_id, score, bucket, components_json,
                    explanation_json, model, scored_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(entry_id, profile_id) DO UPDATE SET
                    score = excluded.score,
                    bucket = excluded.bucket,
                    components_json = excluded.components_json,
                    explanation_json = excluded.explanation_json,
                    model = excluded.model,
                    scored_at = excluded.scored_at
                """,
                (
                    entry_id,
                    profile_id,
                    score,
                    bucket,
                    json.dumps(components or {}, sort_keys=True),
                    json.dumps(explanation or {}, sort_keys=True),
                    model,
                    utc_now(),
                ),
            )
            row = connection.execute(
                """
                SELECT * FROM entry_rankings
                WHERE entry_id = ? AND profile_id = ?
                """,
                (entry_id, profile_id),
            ).fetchone()
            result = dict(row)
            result["components"] = json.loads(str(result.pop("components_json")))
            result["explanation"] = json.loads(str(result.pop("explanation_json")))
            return result

    def get_entry_ranking(
        self, entry_id: int, profile_id: int
    ) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                """
                SELECT * FROM entry_rankings
                WHERE entry_id = ? AND profile_id = ?
                """,
                (entry_id, profile_id),
            ).fetchone()
            if row is None:
                return None
            result = dict(row)
            result["components"] = json.loads(str(result.pop("components_json")))
            result["explanation"] = json.loads(str(result.pop("explanation_json")))
            return result

    def list_entry_rankings(self, profile_id: int) -> list[dict[str, Any]]:
        with self.connect() as connection:
            rows = connection.execute(
                """
                SELECT entry_rankings.*, entries.status, entries.source_id,
                       entries.title, entries.content
                FROM entry_rankings
                JOIN entries ON entries.id = entry_rankings.entry_id
                WHERE profile_id = ?
                ORDER BY score DESC, entry_id DESC
                """,
                (profile_id,),
            ).fetchall()
        results: list[dict[str, Any]] = []
        for row in rows:
            result = dict(row)
            result["components"] = json.loads(str(result.pop("components_json")))
            result["explanation"] = json.loads(str(result.pop("explanation_json")))
            results.append(result)
        return results

    def record_relevance_feedback(
        self, entry_id: int, profile_id: int, label: str
    ) -> None:
        if label not in VALID_FEEDBACK_LABELS:
            raise ValueError(f"Unknown feedback label: {label}")
        now = utc_now()
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO entry_feedback(
                    entry_id, profile_id, label, created_at, updated_at
                ) VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(entry_id, profile_id) DO UPDATE SET
                    label = excluded.label,
                    updated_at = excluded.updated_at
                """,
                (entry_id, profile_id, label, now, now),
            )

    def clear_relevance_feedback(self, entry_id: int, profile_id: int) -> bool:
        with self.connect() as connection:
            cursor = connection.execute(
                "DELETE FROM entry_feedback WHERE entry_id = ? AND profile_id = ?",
                (entry_id, profile_id),
            )
            return cursor.rowcount == 1

    def list_relevance_feedback(
        self, profile_id: int, *, limit: int | None = None
    ) -> list[dict[str, Any]]:
        params: list[Any] = [profile_id]
        limit_clause = ""
        if limit is not None:
            if limit < 1:
                raise ValueError("Limit must be at least 1")
            limit_clause = "LIMIT ?"
            params.append(limit)
        with self.connect() as connection:
            return [
                dict(row)
                for row in connection.execute(
                    f"""
                    SELECT * FROM entry_feedback
                    WHERE profile_id = ?
                    ORDER BY updated_at DESC, entry_id DESC
                    {limit_clause}
                    """,
                    params,
                )
            ]

    def put_embedding(
        self, content_hash: str, model: str, dimensions: int, vector: bytes
    ) -> None:
        if not content_hash or not model or dimensions < 1 or not vector:
            raise ValueError("Embedding metadata and vector must be non-empty")
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO embedding_cache(
                    content_hash, model, dimensions, vector, created_at
                ) VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(content_hash, model) DO UPDATE SET
                    dimensions = excluded.dimensions,
                    vector = excluded.vector,
                    created_at = excluded.created_at
                """,
                (content_hash, model, dimensions, vector, utc_now()),
            )

    def get_embedding(self, content_hash: str, model: str) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                """
                SELECT * FROM embedding_cache
                WHERE content_hash = ? AND model = ?
                """,
                (content_hash, model),
            ).fetchone()
            return dict(row) if row else None

    def set_setting(self, key: str, value: Any) -> None:
        key = key.strip()
        if not key:
            raise ValueError("Setting key cannot be empty")
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO application_settings(key, value, updated_at)
                VALUES (?, ?, ?)
                ON CONFLICT(key) DO UPDATE SET
                    value = excluded.value,
                    updated_at = excluded.updated_at
                """,
                (key, json.dumps(value), utc_now()),
            )

    def get_setting(self, key: str, default: Any = None) -> Any:
        with self.connect() as connection:
            row = connection.execute(
                "SELECT value FROM application_settings WHERE key = ?", (key,)
            ).fetchone()
            return default if row is None else json.loads(str(row["value"]))

    def put_provider_cache(
        self, provider: str, cache_key: str, payload: dict[str, Any]
    ) -> None:
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO provider_cache(provider, cache_key, payload_json, fetched_at)
                VALUES (?, ?, ?, ?)
                ON CONFLICT(provider, cache_key) DO UPDATE SET
                    payload_json = excluded.payload_json,
                    fetched_at = excluded.fetched_at
                """,
                (
                    provider.strip(), cache_key.strip(),
                    json.dumps(payload, ensure_ascii=False, separators=(",", ":")),
                    utc_now(),
                ),
            )

    def get_provider_cache(
        self, provider: str, cache_key: str, *, max_age_seconds: int = 86_400
    ) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                """
                SELECT payload_json, fetched_at FROM provider_cache
                WHERE provider = ? AND cache_key = ?
                """,
                (provider.strip(), cache_key.strip()),
            ).fetchone()
        if row is None:
            return None
        try:
            fetched_at = datetime.fromisoformat(str(row["fetched_at"]))
            if fetched_at.tzinfo is None:
                fetched_at = fetched_at.replace(tzinfo=UTC)
            if (datetime.now(UTC) - fetched_at).total_seconds() > max_age_seconds:
                return None
            payload = json.loads(str(row["payload_json"]))
        except (ValueError, TypeError, json.JSONDecodeError):
            return None
        return payload if isinstance(payload, dict) else None

    def upsert_obsidian_export(
        self,
        entry_id: int,
        *,
        state: str,
        vault_path: str = "",
        relative_path: str = "",
        content_hash: str = "",
        error: str = "",
    ) -> dict[str, Any]:
        if state not in VALID_EXPORT_STATES:
            raise ValueError(f"Unknown Obsidian export state: {state}")
        now = utc_now()
        with self.connect() as connection:
            connection.execute(
                """
                INSERT INTO obsidian_exports(
                    entry_id, vault_path, relative_path, state, content_hash,
                    error, synced_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(entry_id) DO UPDATE SET
                    vault_path = excluded.vault_path,
                    relative_path = excluded.relative_path,
                    state = excluded.state,
                    content_hash = excluded.content_hash,
                    error = excluded.error,
                    synced_at = excluded.synced_at,
                    updated_at = excluded.updated_at
                """,
                (
                    entry_id,
                    vault_path,
                    relative_path,
                    state,
                    content_hash,
                    error,
                    now if state == "synced" else None,
                    now,
                ),
            )
            if relative_path:
                connection.execute(
                    "UPDATE entries SET exported_path = ? WHERE id = ?",
                    (relative_path, entry_id),
                )
            row = connection.execute(
                "SELECT * FROM obsidian_exports WHERE entry_id = ?", (entry_id,)
            ).fetchone()
            return dict(row)

    def get_obsidian_export(self, entry_id: int) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                "SELECT * FROM obsidian_exports WHERE entry_id = ?", (entry_id,)
            ).fetchone()
            return dict(row) if row else None

    def add_obsidian_archive(
        self,
        entry_id: int,
        *,
        original_relative_path: str,
        archive_path: str,
    ) -> int:
        with self.connect() as connection:
            cursor = connection.execute(
                """
                INSERT INTO obsidian_archives(
                    entry_id, original_relative_path, archive_path, archived_at
                ) VALUES (?, ?, ?, ?)
                """,
                (entry_id, original_relative_path, archive_path, utc_now()),
            )
            return int(cursor.lastrowid)

    def list_obsidian_archives(self, entry_id: int) -> list[dict[str, Any]]:
        with self.connect() as connection:
            return [
                dict(row)
                for row in connection.execute(
                    """
                    SELECT * FROM obsidian_archives WHERE entry_id = ?
                    ORDER BY archived_at DESC, id DESC
                    """,
                    (entry_id,),
                )
            ]
