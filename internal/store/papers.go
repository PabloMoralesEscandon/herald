package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// PaperIdentifier is one external identity for a paper.
type PaperIdentifier struct {
	EntryID   int64  `json:"entry_id"`
	Scheme    string `json:"scheme"`
	Value     string `json:"value"`
	IsPrimary int    `json:"is_primary"`
	CreatedAt string `json:"created_at"`
}

// PaperReference is one directed citation edge from a paper to another work.
type PaperReference struct {
	ID             int64  `json:"id"`
	CitingEntryID  int64  `json:"citing_entry_id"`
	CitedEntryID   *int64 `json:"cited_entry_id"`
	ReferenceKey   string `json:"reference_key"`
	ExternalScheme string `json:"external_scheme"`
	ExternalID     string `json:"external_id"`
	CitedTitle     string `json:"cited_title"`
	CitedURL       string `json:"cited_url"`
	Position       *int64 `json:"position"`
	Provider       string `json:"provider"`
	// BibLabel and BibRaw are how the citing article itself printed this
	// reference, filled in when its full text was extracted.
	BibLabel  string `json:"bib_label"`
	BibRaw    string `json:"bib_raw"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	// Joined target columns, present when the cited paper is in Herald.
	TargetTitle       *string `json:"target_title"`
	TargetURL         *string `json:"target_url"`
	CitedStatus       *string `json:"cited_status"`
	CitedExportedPath *string `json:"cited_exported_path"`
	CitedExportState  *string `json:"cited_export_state,omitempty"`
	CitedObsidianPath *string `json:"cited_obsidian_path,omitempty"`
}

// Keyword is one extracted keyword or provider-supplied topic.
type Keyword struct {
	EntryID   int64    `json:"entry_id"`
	Keyword   string   `json:"keyword"`
	Kind      string   `json:"kind"`
	Score     *float64 `json:"score"`
	Provider  string   `json:"provider"`
	CreatedAt string   `json:"created_at"`
}

// normalizeIdentifier lowercases the schemes whose values are case-insensitive.
func normalizeIdentifier(scheme, value string) (string, string, error) {
	normalizedScheme := strings.ToLower(strings.TrimSpace(scheme))
	normalizedValue := strings.TrimSpace(value)
	if normalizedScheme == "" || normalizedValue == "" {
		return "", "", fmt.Errorf("Identifier scheme and value cannot be empty")
	}
	switch normalizedScheme {
	case "doi", "arxiv", "s2":
		normalizedValue = strings.ToLower(normalizedValue)
	}
	return normalizedScheme, normalizedValue, nil
}

// AddPaperIdentifier records an external identity and immediately resolves any
// dangling citation edges that were waiting for it.
func (d *DB) AddPaperIdentifier(entryID int64, scheme, value string, isPrimary bool) (bool, error) {
	scheme, value, err := normalizeIdentifier(scheme, value)
	if err != nil {
		return false, err
	}
	var inserted bool
	err = d.tx(func(tx *sql.Tx) error {
		if isPrimary {
			if _, err := tx.Exec(
				"UPDATE paper_identifiers SET is_primary = 0 WHERE entry_id = ?", entryID); err != nil {
				return err
			}
		}
		now := UTCNow()
		result, err := tx.Exec(`
			INSERT INTO paper_identifiers(entry_id, scheme, value, is_primary, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(entry_id, scheme, value) DO UPDATE SET
				is_primary = MAX(is_primary, excluded.is_primary)`,
			entryID, scheme, value, boolToInt(isPrimary), now)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		inserted = affected == 1
		// Resolution is deliberately directed: only cited_entry_id changes,
		// so Herald never manufactures a reverse citation.
		_, err = tx.Exec(`
			UPDATE paper_references SET cited_entry_id = ?, updated_at = ?
			WHERE cited_entry_id IS NULL AND external_scheme = ? AND external_id = ?
			  AND citing_entry_id <> ?`,
			entryID, now, scheme, value, entryID)
		return err
	})
	return inserted, err
}

// ListPaperIdentifiers returns a paper's identities, primary first.
func (d *DB) ListPaperIdentifiers(entryID int64) ([]*PaperIdentifier, error) {
	rows, err := d.sql.Query(`
		SELECT entry_id, scheme, value, is_primary, created_at
		FROM paper_identifiers WHERE entry_id = ?
		ORDER BY is_primary DESC, scheme, value`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	identifiers := []*PaperIdentifier{}
	for rows.Next() {
		var item PaperIdentifier
		if err := rows.Scan(&item.EntryID, &item.Scheme, &item.Value,
			&item.IsPrimary, &item.CreatedAt); err != nil {
			return nil, err
		}
		identifiers = append(identifiers, &item)
	}
	return identifiers, rows.Err()
}

// FindEntryByIdentifier resolves a paper by one of its external identities.
func (d *DB) FindEntryByIdentifier(scheme, value string) (*Entry, error) {
	scheme, value, err := normalizeIdentifier(scheme, value)
	if err != nil {
		return nil, err
	}
	row := d.sql.QueryRow(`SELECT `+entryColumns+`
		FROM paper_identifiers
		JOIN entries ON entries.id = paper_identifiers.entry_id
		JOIN sources ON sources.id = entries.source_id
		WHERE paper_identifiers.scheme = ? AND paper_identifiers.value = ?`,
		scheme, value)
	entry, scanErr := scanEntry(row, false)
	if scanErr == sql.ErrNoRows {
		return nil, nil
	}
	return entry, scanErr
}

// UpsertReferenceInput describes one citation edge.
type UpsertReferenceInput struct {
	CitedEntryID   *int64
	ExternalScheme string
	ExternalID     string
	CitedTitle     string
	CitedURL       string
	Position       *int64
	Provider       string
}

// UpsertPaperReference records a citation, resolving its target when possible.
func (d *DB) UpsertPaperReference(citingEntryID int64, referenceKey string, input UpsertReferenceInput) (int64, error) {
	referenceKey = strings.TrimSpace(referenceKey)
	if referenceKey == "" {
		return 0, fmt.Errorf("Reference key cannot be empty")
	}
	scheme, identifier := input.ExternalScheme, input.ExternalID
	if scheme != "" || identifier != "" {
		var err error
		scheme, identifier, err = normalizeIdentifier(scheme, identifier)
		if err != nil {
			return 0, err
		}
	}
	var referenceID int64
	err := d.tx(func(tx *sql.Tx) error {
		citedEntryID := input.CitedEntryID
		if citedEntryID == nil && scheme != "" && identifier != "" {
			var target int64
			if err := tx.QueryRow(`
				SELECT entry_id FROM paper_identifiers
				WHERE scheme = ? AND value = ? AND entry_id <> ?`,
				scheme, identifier, citingEntryID).Scan(&target); err == nil {
				citedEntryID = &target
			}
		}
		if citedEntryID == nil && strings.TrimSpace(input.CitedURL) != "" {
			url := strings.TrimSpace(input.CitedURL)
			var target int64
			if err := tx.QueryRow(`
				SELECT id FROM entries
				WHERE id <> ? AND (url = ? OR canonical_url = ?)
				ORDER BY id LIMIT 1`, citingEntryID, url, url).Scan(&target); err == nil {
				citedEntryID = &target
			}
		}
		// A paper never cites itself in Herald's graph.
		if citedEntryID != nil && *citedEntryID == citingEntryID {
			citedEntryID = nil
		}
		now := UTCNow()
		if _, err := tx.Exec(`
			INSERT INTO paper_references(
				citing_entry_id, cited_entry_id, reference_key, external_scheme,
				external_id, cited_title, cited_url, position, provider,
				created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(citing_entry_id, reference_key) DO UPDATE SET
				cited_entry_id = excluded.cited_entry_id,
				external_scheme = excluded.external_scheme,
				external_id = excluded.external_id,
				cited_title = excluded.cited_title,
				cited_url = excluded.cited_url,
				position = excluded.position,
				provider = excluded.provider,
				updated_at = excluded.updated_at`,
			citingEntryID, citedEntryID, referenceKey, scheme, identifier,
			input.CitedTitle, input.CitedURL, input.Position, input.Provider,
			now, now); err != nil {
			return err
		}
		return tx.QueryRow(`
			SELECT id FROM paper_references
			WHERE citing_entry_id = ? AND reference_key = ?`,
			citingEntryID, referenceKey).Scan(&referenceID)
	})
	return referenceID, err
}

// GetPaperReference returns one citation edge with its target details.
func (d *DB) GetPaperReference(referenceID int64) (*PaperReference, error) {
	row := d.sql.QueryRow(`
		SELECT paper_references.id, paper_references.citing_entry_id,
			paper_references.cited_entry_id, paper_references.reference_key,
			paper_references.external_scheme, paper_references.external_id,
			paper_references.cited_title, paper_references.cited_url,
			paper_references.position, paper_references.provider,
			paper_references.bib_label, paper_references.bib_raw,
			paper_references.created_at, paper_references.updated_at,
			target.title, target.canonical_url, target.status, target.exported_path
		FROM paper_references
		LEFT JOIN entries AS target ON target.id = paper_references.cited_entry_id
		WHERE paper_references.id = ?`, referenceID)
	var reference PaperReference
	err := row.Scan(&reference.ID, &reference.CitingEntryID, &reference.CitedEntryID,
		&reference.ReferenceKey, &reference.ExternalScheme, &reference.ExternalID,
		&reference.CitedTitle, &reference.CitedURL, &reference.Position,
		&reference.Provider, &reference.BibLabel, &reference.BibRaw,
		&reference.CreatedAt, &reference.UpdatedAt,
		&reference.TargetTitle, &reference.TargetURL, &reference.CitedStatus,
		&reference.CitedExportedPath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &reference, err
}

// ListPaperReferences returns the works a paper cites, in citation order.
func (d *DB) ListPaperReferences(entryID int64) ([]*PaperReference, error) {
	rows, err := d.sql.Query(`
		SELECT paper_references.id, paper_references.citing_entry_id,
			paper_references.cited_entry_id, paper_references.reference_key,
			paper_references.external_scheme, paper_references.external_id,
			paper_references.cited_title, paper_references.cited_url,
			paper_references.position, paper_references.provider,
			paper_references.bib_label, paper_references.bib_raw,
			paper_references.created_at, paper_references.updated_at,
			entries.title, entries.canonical_url, entries.status,
			entries.exported_path, obsidian_exports.state, obsidian_exports.relative_path
		FROM paper_references
		LEFT JOIN entries ON entries.id = paper_references.cited_entry_id
		LEFT JOIN obsidian_exports ON obsidian_exports.entry_id = paper_references.cited_entry_id
		WHERE paper_references.citing_entry_id = ?
		ORDER BY paper_references.position IS NULL,
			paper_references.position, paper_references.id`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	references := []*PaperReference{}
	for rows.Next() {
		var reference PaperReference
		if err := rows.Scan(&reference.ID, &reference.CitingEntryID,
			&reference.CitedEntryID, &reference.ReferenceKey,
			&reference.ExternalScheme, &reference.ExternalID, &reference.CitedTitle,
			&reference.CitedURL, &reference.Position, &reference.Provider,
			&reference.BibLabel, &reference.BibRaw,
			&reference.CreatedAt, &reference.UpdatedAt, &reference.TargetTitle,
			&reference.TargetURL, &reference.CitedStatus, &reference.CitedExportedPath,
			&reference.CitedExportState, &reference.CitedObsidianPath); err != nil {
			return nil, err
		}
		references = append(references, &reference)
	}
	return references, rows.Err()
}

// ReconcilePaperReferences resolves dangling edges against identifiers and
// canonical URLs, and reports how many were linked.
func (d *DB) ReconcilePaperReferences() (int, error) {
	var resolved int
	err := d.tx(func(tx *sql.Tx) error {
		now := UTCNow()
		result, err := tx.Exec(`
			UPDATE paper_references AS reference
			SET cited_entry_id = (
					SELECT identifier.entry_id FROM paper_identifiers AS identifier
					WHERE identifier.scheme = reference.external_scheme
					  AND identifier.value = reference.external_id
					  AND identifier.entry_id <> reference.citing_entry_id
					LIMIT 1),
				updated_at = ?
			WHERE reference.cited_entry_id IS NULL
			  AND reference.external_scheme <> '' AND reference.external_id <> ''
			  AND EXISTS (
					SELECT 1 FROM paper_identifiers AS identifier
					WHERE identifier.scheme = reference.external_scheme
					  AND identifier.value = reference.external_id
					  AND identifier.entry_id <> reference.citing_entry_id)`, now)
		if err != nil {
			return err
		}
		byIdentifier, err := result.RowsAffected()
		if err != nil {
			return err
		}
		result, err = tx.Exec(`
			UPDATE paper_references AS reference
			SET cited_entry_id = (
					SELECT entry.id FROM entries AS entry
					WHERE entry.id <> reference.citing_entry_id
					  AND (entry.url = reference.cited_url
					       OR entry.canonical_url = reference.cited_url)
					ORDER BY entry.id LIMIT 1),
				updated_at = ?
			WHERE reference.cited_entry_id IS NULL AND reference.cited_url <> ''
			  AND EXISTS (
					SELECT 1 FROM entries AS entry
					WHERE entry.id <> reference.citing_entry_id
					  AND (entry.url = reference.cited_url
					       OR entry.canonical_url = reference.cited_url))`, now)
		if err != nil {
			return err
		}
		byURL, err := result.RowsAffected()
		if err != nil {
			return err
		}
		resolved = int(byIdentifier + byURL)
		return nil
	})
	return resolved, err
}

// ListCitingEntryIDs returns the entries with a directed reference to a target.
func (d *DB) ListCitingEntryIDs(citedEntryID int64) ([]int64, error) {
	rows, err := d.sql.Query(`
		SELECT DISTINCT citing_entry_id FROM paper_references
		WHERE cited_entry_id = ? AND citing_entry_id <> ?
		ORDER BY citing_entry_id`, citedEntryID, citedEntryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReplaceEntryKeywords swaps an entry's keywords atomically.
func (d *DB) ReplaceEntryKeywords(entryID int64, keywords []Keyword) error {
	now := UTCNow()
	return d.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM entry_keywords WHERE entry_id = ?", entryID); err != nil {
			return err
		}
		for _, keyword := range keywords {
			kind := keyword.Kind
			if kind == "" {
				kind = "keyword"
			}
			if kind != "keyword" && kind != "topic" {
				return fmt.Errorf("Unknown keyword kind: %s", kind)
			}
			value := strings.TrimSpace(keyword.Keyword)
			if value == "" {
				return fmt.Errorf("Keyword cannot be empty")
			}
			if _, err := tx.Exec(`
				INSERT INTO entry_keywords(entry_id, keyword, kind, score, provider, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`,
				entryID, value, kind, keyword.Score, keyword.Provider, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListEntryKeywords returns an entry's keywords and topics.
func (d *DB) ListEntryKeywords(entryID int64) ([]*Keyword, error) {
	rows, err := d.sql.Query(`
		SELECT entry_id, keyword, kind, score, provider, created_at
		FROM entry_keywords WHERE entry_id = ?
		ORDER BY kind, score DESC, keyword`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keywords := []*Keyword{}
	for rows.Next() {
		var keyword Keyword
		if err := rows.Scan(&keyword.EntryID, &keyword.Keyword, &keyword.Kind,
			&keyword.Score, &keyword.Provider, &keyword.CreatedAt); err != nil {
			return nil, err
		}
		keywords = append(keywords, &keyword)
	}
	return keywords, rows.Err()
}
