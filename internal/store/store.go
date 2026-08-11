// Package store owns Herald's SQLite database: the schema, its additive
// migrations, and every query.
//
// The schema is compatible with databases written by earlier versions of
// Herald. Migrations only add columns and backfill previously unset values, so
// existing read, kept, and discarded state is never reset.
package store

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so builds stay portable
)

// Valid enumerations, mirrored from the schema's CHECK constraints so callers
// can validate before touching the database.
var (
	ValidStatuses         = []string{"unread", "read", "kept", "discarded"}
	ValidContentKinds     = []string{"paper", "news"}
	ValidEnrichmentStates = []string{"pending", "enriched", "failed", "not_applicable"}
	ValidRelevanceBuckets = []string{"pending", "relevant", "filtered"}
	ValidFeedbackLabels   = []string{"keep", "discard"}
	ValidSelectivity      = []string{"broad", "balanced", "focused"}
	ValidExportStates     = []string{
		"pending", "synced", "failed", "conflict", "archive_pending", "archived",
	}
)

func valid(set []string, value string) bool {
	for _, candidate := range set {
		if candidate == value {
			return true
		}
	}
	return false
}

// DB is a handle to Herald's local database.
type DB struct {
	Path string
	sql  *sql.DB
}

// Open connects to the database, creating the file and its parent directory if
// needed.
//
// A single pooled connection is used deliberately. SQLite permits only one
// writer, and Herald's background scoring and enrichment run concurrently with
// request handling; serializing here removes an entire class of SQLITE_BUSY
// failures that the previous connection-per-operation design was exposed to.
func Open(path string) (*DB, error) {
	if directory := filepath.Dir(path); directory != "" {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, err
		}
	}
	// busy_timeout still matters for other processes holding the file, and
	// WAL keeps readers from blocking the writer.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	handle.SetConnMaxLifetime(0)
	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, err
	}
	return &DB{Path: path, sql: handle}, nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the underlying handle for the few callers that need it.
func (d *DB) SQL() *sql.DB { return d.sql }

// Initialize creates the schema and applies additive migrations. It is safe to
// run on every start and against a database from any earlier version.
func (d *DB) Initialize() error {
	return d.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(schemaSQL); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		if err := addMissingColumns(tx, "sources", sourceMigrations); err != nil {
			return err
		}
		if err := addMissingColumns(tx, "entries", entryMigrations); err != nil {
			return err
		}
		if _, err := tx.Exec(extendedSchemaSQL); err != nil {
			return fmt.Errorf("create extended schema: %w", err)
		}
		for _, statement := range backfillSQL {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("backfill: %w", err)
			}
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
		return nil
	})
}

// addMissingColumns adds only the columns a database does not already have.
func addMissingColumns(tx *sql.Tx, table string, migrations []struct{ name, definition string }) error {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var (
			cid          int
			name, kind   string
			notNull, pk  int
			defaultValue sql.NullString
		)
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, migration := range migrations {
		if existing[migration.name] {
			continue
		}
		statement := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s",
			table, migration.name, migration.definition)
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", table, migration.name, err)
		}
	}
	return nil
}

// tx runs fn inside a transaction, rolling back on error.
func (d *DB) tx(fn func(*sql.Tx) error) error {
	transaction, err := d.sql.Begin()
	if err != nil {
		return err
	}
	if err := fn(transaction); err != nil {
		transaction.Rollback()
		return err
	}
	return transaction.Commit()
}

// UTCNow is the timestamp format stored throughout the database: whole-second
// ISO 8601 in UTC, matching every existing row.
func UTCNow() string {
	return time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05-07:00")
}

// EncodeCursor builds the opaque pagination cursor.
//
// The encoding is unpadded URL-safe base64 over a compact two-element JSON
// array, identical to previously issued cursors so links already in flight keep
// working.
func EncodeCursor(sortAt string, entryID int64) string {
	payload, _ := json.Marshal([]any{sortAt, entryID})
	return strings.TrimRight(base64.URLEncoding.EncodeToString(payload), "=")
}

// DecodeCursor reverses EncodeCursor, rejecting anything malformed.
func DecodeCursor(cursor string) (string, int64, error) {
	padded := cursor
	if remainder := len(padded) % 4; remainder != 0 {
		padded += strings.Repeat("=", 4-remainder)
	}
	raw, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		return "", 0, fmt.Errorf("Invalid entry cursor")
	}
	var payload []json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload) != 2 {
		return "", 0, fmt.Errorf("Invalid entry cursor")
	}
	var sortAt string
	if err := json.Unmarshal(payload[0], &sortAt); err != nil {
		return "", 0, fmt.Errorf("Invalid entry cursor")
	}
	// The identifier must be a JSON integer; a float or string is a forged
	// or corrupted cursor.
	var entryID int64
	decoder := json.NewDecoder(strings.NewReader(string(payload[1])))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return "", 0, fmt.Errorf("Invalid entry cursor")
	}
	if strings.ContainsAny(number.String(), ".eE") {
		return "", 0, fmt.Errorf("Invalid entry cursor")
	}
	entryID, err = number.Int64()
	if err != nil {
		return "", 0, fmt.Errorf("Invalid entry cursor")
	}
	return sortAt, entryID, nil
}
