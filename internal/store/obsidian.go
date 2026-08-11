package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Export is the recorded synchronization state of one Obsidian note.
type Export struct {
	EntryID      int64   `json:"entry_id"`
	VaultPath    string  `json:"vault_path"`
	RelativePath string  `json:"relative_path"`
	State        string  `json:"state"`
	ContentHash  string  `json:"content_hash"`
	Error        string  `json:"error"`
	SyncedAt     *string `json:"synced_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// Archive is one note moved out of the vault and still recoverable.
type Archive struct {
	ID                   int64   `json:"id"`
	EntryID              int64   `json:"entry_id"`
	OriginalRelativePath string  `json:"original_relative_path"`
	ArchivePath          string  `json:"archive_path"`
	ArchivedAt           string  `json:"archived_at"`
	RestoredAt           *string `json:"restored_at"`
}

// ExportUpdate carries an export state change. Nil pointers leave the stored
// value unchanged, so a failure retains the path it was trying to write.
type ExportUpdate struct {
	State        string
	VaultPath    *string
	RelativePath *string
	ContentHash  *string
	Error        string
}

// UpsertObsidianExport records a note's synchronization state.
//
// It also keeps entries.exported_path consistent: archiving clears it, and a
// successful write sets it, so the column and this table never disagree.
func (d *DB) UpsertObsidianExport(entryID int64, update ExportUpdate) (*Export, error) {
	if !valid(ValidExportStates, update.State) {
		return nil, fmt.Errorf("Unknown Obsidian export state: %s", update.State)
	}
	var export *Export
	err := d.tx(func(tx *sql.Tx) error {
		now := UTCNow()
		var syncedAt any
		if update.State == "synced" {
			syncedAt = now
		}
		if _, err := tx.Exec(`
			INSERT INTO obsidian_exports(
				entry_id, vault_path, relative_path, state, content_hash,
				error, synced_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(entry_id) DO UPDATE SET
				vault_path = COALESCE(?, obsidian_exports.vault_path),
				relative_path = COALESCE(?, obsidian_exports.relative_path),
				state = excluded.state,
				content_hash = COALESCE(?, obsidian_exports.content_hash),
				error = excluded.error,
				synced_at = CASE WHEN excluded.state = 'synced'
					THEN excluded.synced_at ELSE obsidian_exports.synced_at END,
				updated_at = excluded.updated_at`,
			entryID, derefOr(update.VaultPath, ""), derefOr(update.RelativePath, ""),
			update.State, derefOr(update.ContentHash, ""), update.Error, syncedAt, now,
			update.VaultPath, update.RelativePath, update.ContentHash); err != nil {
			return err
		}
		switch {
		case update.State == "archived" || update.State == "archive_pending":
			if _, err := tx.Exec("UPDATE entries SET exported_path = NULL WHERE id = ?", entryID); err != nil {
				return err
			}
		case update.RelativePath != nil && *update.RelativePath != "":
			if _, err := tx.Exec("UPDATE entries SET exported_path = ? WHERE id = ?",
				*update.RelativePath, entryID); err != nil {
				return err
			}
		}
		row := tx.QueryRow(`SELECT entry_id, vault_path, relative_path, state,
			content_hash, error, synced_at, updated_at
			FROM obsidian_exports WHERE entry_id = ?`, entryID)
		export = &Export{}
		return row.Scan(&export.EntryID, &export.VaultPath, &export.RelativePath,
			&export.State, &export.ContentHash, &export.Error, &export.SyncedAt,
			&export.UpdatedAt)
	})
	return export, err
}

// GetObsidianExport returns a note's state, or nil when it was never exported.
func (d *DB) GetObsidianExport(entryID int64) (*Export, error) {
	row := d.sql.QueryRow(`SELECT entry_id, vault_path, relative_path, state,
		content_hash, error, synced_at, updated_at
		FROM obsidian_exports WHERE entry_id = ?`, entryID)
	var export Export
	err := row.Scan(&export.EntryID, &export.VaultPath, &export.RelativePath,
		&export.State, &export.ContentHash, &export.Error, &export.SyncedAt,
		&export.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &export, err
}

// ListObsidianExports returns every recorded note state.
func (d *DB) ListObsidianExports() ([]*Export, error) {
	rows, err := d.sql.Query(`SELECT entry_id, vault_path, relative_path, state,
		content_hash, error, synced_at, updated_at
		FROM obsidian_exports ORDER BY entry_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exports := []*Export{}
	for rows.Next() {
		var export Export
		if err := rows.Scan(&export.EntryID, &export.VaultPath, &export.RelativePath,
			&export.State, &export.ContentHash, &export.Error, &export.SyncedAt,
			&export.UpdatedAt); err != nil {
			return nil, err
		}
		exports = append(exports, &export)
	}
	return exports, rows.Err()
}

// SetExportedPath marks a note as synced at a path.
func (d *DB) SetExportedPath(entryID int64, exportedPath string) (bool, error) {
	var updated bool
	err := d.tx(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE entries SET exported_path = ? WHERE id = ?",
			exportedPath, entryID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		updated = affected == 1
		if !updated {
			return nil
		}
		now := UTCNow()
		_, err = tx.Exec(`
			INSERT INTO obsidian_exports(entry_id, relative_path, state, synced_at, updated_at)
			VALUES (?, ?, 'synced', ?, ?)
			ON CONFLICT(entry_id) DO UPDATE SET
				relative_path = excluded.relative_path,
				state = 'synced', error = '',
				synced_at = excluded.synced_at,
				updated_at = excluded.updated_at`,
			entryID, exportedPath, now, now)
		return err
	})
	return updated, err
}

// AddObsidianArchive records a note moved out of the vault.
func (d *DB) AddObsidianArchive(entryID int64, originalRelativePath, archivePath string) (int64, error) {
	result, err := d.sql.Exec(`
		INSERT INTO obsidian_archives(entry_id, original_relative_path, archive_path, archived_at)
		VALUES (?, ?, ?, ?)`, entryID, originalRelativePath, archivePath, UTCNow())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// ListObsidianArchives returns an entry's archived notes, newest first.
func (d *DB) ListObsidianArchives(entryID int64) ([]*Archive, error) {
	rows, err := d.sql.Query(`
		SELECT id, entry_id, original_relative_path, archive_path, archived_at, restored_at
		FROM obsidian_archives WHERE entry_id = ?
		ORDER BY archived_at DESC, id DESC`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	archives := []*Archive{}
	for rows.Next() {
		var archive Archive
		if err := rows.Scan(&archive.ID, &archive.EntryID,
			&archive.OriginalRelativePath, &archive.ArchivePath,
			&archive.ArchivedAt, &archive.RestoredAt); err != nil {
			return nil, err
		}
		archives = append(archives, &archive)
	}
	return archives, rows.Err()
}

// MarkObsidianArchiveRestored records that annotations were restored.
func (d *DB) MarkObsidianArchiveRestored(archiveID int64) (bool, error) {
	result, err := d.sql.Exec(
		"UPDATE obsidian_archives SET restored_at = ? WHERE id = ? AND restored_at IS NULL",
		UTCNow(), archiveID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// SetSetting stores a JSON-encoded application setting.
func (d *DB) SetSetting(key string, value any) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("Setting key cannot be empty")
	}
	document, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(`
		INSERT INTO application_settings(key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value, updated_at = excluded.updated_at`,
		key, string(document), UTCNow())
	return err
}

// GetSetting reads a setting into target. It reports whether one was stored.
func (d *DB) GetSetting(key string, target any) (bool, error) {
	var raw string
	err := d.sql.QueryRow("SELECT value FROM application_settings WHERE key = ?", key).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if target == nil {
		return true, nil
	}
	return true, json.Unmarshal([]byte(raw), target)
}

// PutProviderCache stores a metadata provider response.
func (d *DB) PutProviderCache(provider, cacheKey string, payload json.RawMessage) error {
	_, err := d.sql.Exec(`
		INSERT INTO provider_cache(provider, cache_key, payload_json, fetched_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(provider, cache_key) DO UPDATE SET
			payload_json = excluded.payload_json, fetched_at = excluded.fetched_at`,
		strings.TrimSpace(provider), strings.TrimSpace(cacheKey), string(payload), UTCNow())
	return err
}

// GetProviderCache returns a cached response if it is younger than maxAge.
//
// Caching keeps repeated imports of the same paper free and keeps Herald a
// polite client of the public metadata APIs it depends on.
func (d *DB) GetProviderCache(provider, cacheKey string, maxAge time.Duration) (json.RawMessage, error) {
	var payload, fetchedAt string
	err := d.sql.QueryRow(
		"SELECT payload_json, fetched_at FROM provider_cache WHERE provider = ? AND cache_key = ?",
		strings.TrimSpace(provider), strings.TrimSpace(cacheKey)).Scan(&payload, &fetchedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	stamp, parseErr := parseStoredTime(fetchedAt)
	if parseErr != nil || time.Since(stamp) > maxAge {
		return nil, nil
	}
	// Only a JSON object is a usable cached payload.
	var probe map[string]any
	if json.Unmarshal([]byte(payload), &probe) != nil {
		return nil, nil
	}
	return json.RawMessage(payload), nil
}

func parseStoredTime(value string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02T15:04:05-07:00", "2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05", "2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			if parsed.Location() == time.UTC && !strings.ContainsAny(value, "Z+") {
				return parsed.UTC(), nil
			}
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp: %s", value)
}

func derefOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}
