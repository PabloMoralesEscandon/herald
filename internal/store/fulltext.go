package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// ValidFullTextStates mirrors the schema's CHECK constraint.
//
// The states are a small state machine. "pending" is a paper Herald has not
// tried yet; "extracting" is one in flight; "extracted" carries the text.
// "needs_pdf" is the one that matters to the user: no openly available copy
// exists, so the note is kept with a notice asking for theirs. "failed" is a
// transient problem worth retrying, and "not_applicable" covers everything
// that has no article to extract, such as news.
var ValidFullTextStates = []string{
	"pending", "extracting", "extracted", "needs_pdf", "failed", "not_applicable",
}

// FullText is an entry's extracted article text and the provenance of it.
type FullText struct {
	EntryID        int64   `json:"entry_id"`
	State          string  `json:"state"`
	SourceKind     string  `json:"source_kind"`
	SourceURL      string  `json:"source_url"`
	Format         string  `json:"format"`
	Markdown       string  `json:"-"`
	CharacterCount int64   `json:"character_count"`
	ReferenceCount int64   `json:"reference_count"`
	Truncated      bool    `json:"truncated"`
	PDFPath        string  `json:"pdf_path"`
	PDFBytes       int64   `json:"pdf_bytes"`
	Attempts       int64   `json:"attempts"`
	Error          string  `json:"error"`
	AttemptedAt    *string `json:"attempted_at"`
	ExtractedAt    *string `json:"extracted_at"`
	UpdatedAt      string  `json:"updated_at"`
}

// HasText reports whether a note can render an article body from this row.
func (f *FullText) HasText() bool {
	return f != nil && f.State == "extracted" && strings.TrimSpace(f.Markdown) != ""
}

// FullTextUpdate is a partial update. A nil field is left unchanged, which is
// what lets a retry record a new error without discarding text extracted
// earlier.
type FullTextUpdate struct {
	State          string
	SourceKind     *string
	SourceURL      *string
	Format         *string
	Markdown       *string
	CharacterCount *int64
	ReferenceCount *int64
	Truncated      *bool
	PDFPath        *string
	PDFBytes       *int64
	Error          string
	// CountAttempt increments the attempt counter and stamps attempted_at.
	CountAttempt bool
	// MarkExtracted stamps extracted_at with the current time.
	MarkExtracted bool
}

const fullTextColumns = `entry_id, state, source_kind, source_url, format, markdown,
	character_count, reference_count, truncated, pdf_path, pdf_bytes, attempts,
	error, attempted_at, extracted_at, updated_at`

func scanFullText(row interface{ Scan(...any) error }) (*FullText, error) {
	var record FullText
	var truncated int
	err := row.Scan(&record.EntryID, &record.State, &record.SourceKind, &record.SourceURL,
		&record.Format, &record.Markdown, &record.CharacterCount, &record.ReferenceCount,
		&truncated, &record.PDFPath, &record.PDFBytes, &record.Attempts, &record.Error,
		&record.AttemptedAt, &record.ExtractedAt, &record.UpdatedAt)
	if err != nil {
		return nil, err
	}
	record.Truncated = truncated != 0
	return &record, nil
}

// GetFullText returns an entry's extraction row, or nil when there is none.
func (d *DB) GetFullText(entryID int64) (*FullText, error) {
	row := d.sql.QueryRow(`SELECT `+fullTextColumns+` FROM paper_fulltext WHERE entry_id = ?`, entryID)
	record, err := scanFullText(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return record, err
}

// UpsertFullText creates or updates an entry's extraction row.
func (d *DB) UpsertFullText(entryID int64, update FullTextUpdate) (*FullText, error) {
	if update.State != "" && !valid(ValidFullTextStates, update.State) {
		return nil, fmt.Errorf("Unknown full-text state: %s", update.State)
	}
	now := UTCNow()

	err := d.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO paper_fulltext(entry_id, state, updated_at)
			VALUES (?, COALESCE(NULLIF(?, ''), 'pending'), ?)
			ON CONFLICT(entry_id) DO NOTHING`, entryID, update.State, now); err != nil {
			return err
		}

		assignments := []string{"updated_at = ?"}
		arguments := []any{now}
		set := func(column string, value any) {
			assignments = append(assignments, column+" = ?")
			arguments = append(arguments, value)
		}
		if update.State != "" {
			set("state", update.State)
		}
		if update.SourceKind != nil {
			set("source_kind", *update.SourceKind)
		}
		if update.SourceURL != nil {
			set("source_url", *update.SourceURL)
		}
		if update.Format != nil {
			set("format", *update.Format)
		}
		if update.Markdown != nil {
			set("markdown", *update.Markdown)
		}
		if update.CharacterCount != nil {
			set("character_count", *update.CharacterCount)
		}
		if update.ReferenceCount != nil {
			set("reference_count", *update.ReferenceCount)
		}
		if update.Truncated != nil {
			set("truncated", boolToInt(*update.Truncated))
		}
		if update.PDFPath != nil {
			set("pdf_path", *update.PDFPath)
		}
		if update.PDFBytes != nil {
			set("pdf_bytes", *update.PDFBytes)
		}
		// An empty error clears a previous one: a successful retry must not
		// leave the failure that preceded it on display.
		set("error", update.Error)
		if update.CountAttempt {
			assignments = append(assignments, "attempts = attempts + 1")
			set("attempted_at", now)
		}
		if update.MarkExtracted {
			set("extracted_at", now)
		}

		arguments = append(arguments, entryID)
		statement := "UPDATE paper_fulltext SET " + strings.Join(assignments, ", ") +
			" WHERE entry_id = ?"
		_, err := tx.Exec(statement, arguments...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return d.GetFullText(entryID)
}

// ClearFullText removes an entry's extracted text while keeping the row, which
// is what a re-extraction needs so the old body never lingers next to new
// provenance.
func (d *DB) ClearFullText(entryID int64) error {
	_, err := d.sql.Exec(`
		UPDATE paper_fulltext
		SET markdown = '', character_count = 0, reference_count = 0,
			truncated = 0, extracted_at = NULL, updated_at = ?
		WHERE entry_id = ?`, UTCNow(), entryID)
	return err
}

// ListFullTextByState returns the entries in one extraction state, oldest
// first, so a retry pass works through the backlog in order.
func (d *DB) ListFullTextByState(state string, limit int) ([]*FullText, error) {
	if state != "" && !valid(ValidFullTextStates, state) {
		return nil, fmt.Errorf("Unknown full-text state: %s", state)
	}
	if limit <= 0 || limit > 10_000 {
		limit = 500
	}
	query := `SELECT ` + fullTextColumns + ` FROM paper_fulltext`
	arguments := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		arguments = append(arguments, state)
	}
	query += ` ORDER BY updated_at, entry_id LIMIT ?`
	arguments = append(arguments, limit)

	rows, err := d.sql.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []*FullText{}
	for rows.Next() {
		record, err := scanFullText(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// FullTextCounts reports how many entries sit in each extraction state.
func (d *DB) FullTextCounts() (map[string]int64, error) {
	rows, err := d.sql.Query(`SELECT state, COUNT(*) FROM paper_fulltext GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for _, state := range ValidFullTextStates {
		counts[state] = 0
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		counts[state] = count
	}
	return counts, rows.Err()
}

// SetReferenceBibliography records how the citing article printed one
// reference, without disturbing the identifiers a provider supplied for it.
func (d *DB) SetReferenceBibliography(referenceID int64, label, raw string) error {
	_, err := d.sql.Exec(`
		UPDATE paper_references SET bib_label = ?, bib_raw = ?, updated_at = ?
		WHERE id = ?`, label, raw, UTCNow(), referenceID)
	return err
}

// FindReferenceByNormalizedTitle locates an existing citation edge whose cited
// title matches, ignoring case and punctuation.
//
// It exists so a bibliography entry parsed out of an article merges with the
// provider's edge for the same work instead of creating a second row for it.
// Matching happens in Go rather than SQL because SQLite has no way to apply
// Herald's normalization inside a query.
func (d *DB) FindReferenceByNormalizedTitle(citingEntryID int64, normalize func(string) string, title string) (*PaperReference, error) {
	if normalize == nil || strings.TrimSpace(title) == "" {
		return nil, nil
	}
	wanted := normalize(title)
	if wanted == "" {
		return nil, nil
	}
	references, err := d.ListPaperReferences(citingEntryID)
	if err != nil {
		return nil, err
	}
	for _, reference := range references {
		candidates := []string{reference.CitedTitle}
		if reference.TargetTitle != nil {
			candidates = append(candidates, *reference.TargetTitle)
		}
		for _, candidate := range candidates {
			if candidate != "" && normalize(candidate) == wanted {
				return reference, nil
			}
		}
	}
	return nil, nil
}
