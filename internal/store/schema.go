package store

// schemaSQL is the base schema. Every statement is CREATE ... IF NOT EXISTS so
// that opening an existing database is a no-op.
const schemaSQL = `
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
    resolved_url TEXT NOT NULL DEFAULT '',
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
    content_markdown TEXT NOT NULL DEFAULT '',
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
`

// extendedSchemaSQL is applied after the additive column migrations, because
// several of its indexes reference columns those migrations add.
const extendedSchemaSQL = `
CREATE INDEX IF NOT EXISTS sources_kind_idx ON sources(content_kind, enabled);
CREATE INDEX IF NOT EXISTS entries_kind_idx ON entries(content_kind);
CREATE INDEX IF NOT EXISTS entries_canonical_key_idx ON entries(canonical_key);
CREATE INDEX IF NOT EXISTS entries_canonical_url_idx ON entries(canonical_url);

CREATE TABLE IF NOT EXISTS source_bootstrap_skips (
    source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    guid TEXT NOT NULL,
    skipped_at TEXT NOT NULL,
    PRIMARY KEY (source_id, guid)
);

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
`

// sourceMigrations and entryMigrations are applied only when the column is
// absent, so upgrading an existing database never resets read, kept, or
// discarded state.
var sourceMigrations = []struct{ name, definition string }{
	{"content_kind", "TEXT NOT NULL DEFAULT 'paper'"},
	{"adapter", "TEXT NOT NULL DEFAULT 'feed'"},
	{"resolved_url", "TEXT NOT NULL DEFAULT ''"},
	{"etag", "TEXT NOT NULL DEFAULT ''"},
	{"last_modified", "TEXT NOT NULL DEFAULT ''"},
	{"refresh_attempted_at", "TEXT"},
	{"refresh_succeeded_at", "TEXT"},
	{"refresh_error", "TEXT NOT NULL DEFAULT ''"},
}

var entryMigrations = []struct{ name, definition string }{
	{"summary_provider", "TEXT NOT NULL DEFAULT ''"},
	{"summary_model", "TEXT NOT NULL DEFAULT ''"},
	{"summary_generated_at", "TEXT"},
	{"content_markdown", "TEXT NOT NULL DEFAULT ''"},
	{"content_kind", "TEXT NOT NULL DEFAULT 'paper'"},
	{"canonical_url", "TEXT NOT NULL DEFAULT ''"},
	{"canonical_key", "TEXT"},
	{"enrichment_status", "TEXT NOT NULL DEFAULT 'pending'"},
	{"enrichment_provider", "TEXT NOT NULL DEFAULT ''"},
	{"enrichment_error", "TEXT NOT NULL DEFAULT ''"},
	{"enriched_at", "TEXT"},
	{"updated_at", "TEXT"},
}

// backfillSQL repairs rows written by older versions. Each statement is
// idempotent and touches only columns that were previously unset.
var backfillSQL = []string{
	`UPDATE entries SET summary_provider = 'unknown'
	 WHERE summary <> '' AND summary_provider = ''`,
	`UPDATE entries SET canonical_url = url WHERE canonical_url = ''`,
	`UPDATE entries SET updated_at = COALESCE(updated_at, discovered_at)
	 WHERE updated_at IS NULL`,
	`INSERT INTO obsidian_exports(entry_id, relative_path, state, synced_at, updated_at)
	 SELECT id, exported_path, 'synced', updated_at, updated_at FROM entries
	 WHERE exported_path IS NOT NULL AND exported_path <> ''
	 ON CONFLICT(entry_id) DO NOTHING`,
}

// schemaVersion is written to PRAGMA user_version after a successful open.
const schemaVersion = 5
