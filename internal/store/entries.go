package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Entry is one article row joined with its source.
//
// The JSON tags are the documented API contract. Nullable columns are pointers
// so they serialize as null rather than "", which the dashboard distinguishes.
type Entry struct {
	ID                 int64   `json:"id"`
	SourceID           int64   `json:"source_id"`
	GUID               string  `json:"guid"`
	URL                string  `json:"url"`
	Title              string  `json:"title"`
	Author             string  `json:"author"`
	PublishedAt        *string `json:"published_at"`
	DiscoveredAt       string  `json:"discovered_at"`
	Content            string  `json:"content"`
	ContentMarkdown    string  `json:"content_markdown"`
	Summary            string  `json:"summary"`
	SummaryProvider    string  `json:"summary_provider"`
	SummaryModel       string  `json:"summary_model"`
	SummaryGeneratedAt *string `json:"summary_generated_at"`
	Status             string  `json:"status"`
	ExportedPath       *string `json:"exported_path"`
	ContentKind        string  `json:"content_kind"`
	CanonicalURL       string  `json:"canonical_url"`
	CanonicalKey       *string `json:"canonical_key"`
	EnrichmentStatus   string  `json:"enrichment_status"`
	EnrichmentProvider string  `json:"enrichment_provider"`
	EnrichmentError    string  `json:"enrichment_error"`
	EnrichedAt         *string `json:"enriched_at"`
	UpdatedAt          *string `json:"updated_at"`
	SourceTitle        string  `json:"source_title"`
	SourceCategory     string  `json:"source_category"`
	SourceAdapter      string  `json:"source_adapter"`

	// Ranking fields appear only on ranked pages, matching the reference
	// behaviour of adding them solely when a profile is joined.
	RelevanceScore       *float64        `json:"relevance_score,omitempty"`
	RelevanceBucket      *string         `json:"relevance_bucket,omitempty"`
	RelevanceComponents  json.RawMessage `json:"relevance_components,omitempty"`
	RelevanceExplanation json.RawMessage `json:"relevance_explanation,omitempty"`
	RelevanceModel       *string         `json:"relevance_model,omitempty"`
	RelevanceScoredAt    *string         `json:"relevance_scored_at,omitempty"`
}

// entryColumns is the projection shared by every entry query.
const entryColumns = `entries.id, entries.source_id, entries.guid, entries.url,
	entries.title, entries.author, entries.published_at, entries.discovered_at,
	entries.content, entries.content_markdown, entries.summary,
	entries.summary_provider, entries.summary_model, entries.summary_generated_at,
	entries.status, entries.exported_path, entries.content_kind,
	entries.canonical_url, entries.canonical_key, entries.enrichment_status,
	entries.enrichment_provider, entries.enrichment_error, entries.enriched_at,
	entries.updated_at, sources.title AS source_title,
	sources.category AS source_category, sources.adapter AS source_adapter`

// scanEntry reads the shared projection, optionally followed by ranking fields.
func scanEntry(rows interface{ Scan(...any) error }, ranked bool) (*Entry, error) {
	var entry Entry
	targets := []any{
		&entry.ID, &entry.SourceID, &entry.GUID, &entry.URL, &entry.Title,
		&entry.Author, &entry.PublishedAt, &entry.DiscoveredAt, &entry.Content,
		&entry.ContentMarkdown, &entry.Summary, &entry.SummaryProvider,
		&entry.SummaryModel, &entry.SummaryGeneratedAt, &entry.Status,
		&entry.ExportedPath, &entry.ContentKind, &entry.CanonicalURL,
		&entry.CanonicalKey, &entry.EnrichmentStatus, &entry.EnrichmentProvider,
		&entry.EnrichmentError, &entry.EnrichedAt, &entry.UpdatedAt,
		&entry.SourceTitle, &entry.SourceCategory, &entry.SourceAdapter,
	}
	if ranked {
		var (
			score       float64
			bucket      string
			components  string
			explanation string
			model       string
			scoredAt    string
		)
		targets = append(targets, &score, &bucket, &components, &explanation, &model, &scoredAt)
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		entry.RelevanceScore = &score
		entry.RelevanceBucket = &bucket
		entry.RelevanceComponents = json.RawMessage(components)
		entry.RelevanceExplanation = json.RawMessage(explanation)
		entry.RelevanceModel = &model
		entry.RelevanceScoredAt = &scoredAt
		return &entry, nil
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, err
	}
	return &entry, nil
}

// UpsertEntry inserts or updates one article and reports whether it was new.
//
// Matching is by (source_id, guid) first, then by url or canonical_url, which
// is how the same article syndicated to several feeds collapses into a single
// entry without losing the triage state already applied to it.
func (d *DB) UpsertEntry(input UpsertEntryInput) (entryID int64, created bool, err error) {
	if input.ContentKind != "" && !valid(ValidContentKinds, input.ContentKind) {
		return 0, false, fmt.Errorf("Unknown content kind: %s", input.ContentKind)
	}
	err = d.tx(func(tx *sql.Tx) error {
		var sourceKind string
		row := tx.QueryRow("SELECT content_kind FROM sources WHERE id = ?", input.SourceID)
		if scanErr := row.Scan(&sourceKind); scanErr != nil {
			if scanErr == sql.ErrNoRows {
				return fmt.Errorf("Source %d does not exist", input.SourceID)
			}
			return scanErr
		}
		resolvedKind := input.ContentKind
		if resolvedKind == "" {
			resolvedKind = sourceKind
		}
		canonicalURL := strings.TrimSpace(input.CanonicalURL)
		if canonicalURL == "" {
			canonicalURL = strings.TrimSpace(input.URL)
		}

		var existing int64
		found := tx.QueryRow(`
			SELECT id FROM entries
			WHERE (source_id = ? AND guid = ?)
			   OR (? <> '' AND url = ?)
			   OR (? <> '' AND canonical_url = ?)
			ORDER BY CASE WHEN source_id = ? AND guid = ? THEN 0 ELSE 1 END
			LIMIT 1`,
			input.SourceID, input.GUID, input.URL, input.URL,
			canonicalURL, canonicalURL, input.SourceID, input.GUID,
		).Scan(&existing)
		now := UTCNow()

		if found == nil {
			// Content and summary fields are only filled in, never blanked:
			// a later feed revision with an empty body must not erase text
			// the reader already has.
			_, execErr := tx.Exec(`
				UPDATE entries SET
					url = ?, title = ?, author = ?, published_at = ?,
					content = CASE WHEN ? <> '' THEN ? ELSE content END,
					content_markdown = CASE WHEN ? <> '' THEN ? ELSE content_markdown END,
					summary = CASE WHEN summary = '' AND ? <> '' THEN ? ELSE summary END,
					summary_provider = CASE WHEN summary = '' AND ? <> '' THEN ? ELSE summary_provider END,
					summary_model = CASE WHEN summary = '' AND ? <> '' THEN ? ELSE summary_model END,
					summary_generated_at = CASE WHEN summary = '' AND ? <> '' THEN ? ELSE summary_generated_at END,
					content_kind = ?, canonical_url = ?,
					canonical_key = COALESCE(?, canonical_key), updated_at = ?
				WHERE id = ?`,
				input.URL, input.Title, input.Author, input.PublishedAt,
				input.Content, input.Content,
				input.ContentMarkdown, input.ContentMarkdown,
				input.Summary, input.Summary,
				input.Summary, input.SummaryProvider,
				input.Summary, input.SummaryModel,
				input.Summary, now,
				resolvedKind, canonicalURL, input.CanonicalKey, now, existing,
			)
			if execErr != nil {
				return execErr
			}
			entryID, created = existing, false
			return nil
		}
		if found != sql.ErrNoRows {
			return found
		}

		var summaryGeneratedAt any
		if input.Summary != "" {
			summaryGeneratedAt = now
		}
		result, execErr := tx.Exec(`
			INSERT INTO entries(
				source_id, guid, url, title, author, published_at, discovered_at,
				content, content_markdown, summary, summary_provider, summary_model,
				summary_generated_at, content_kind, canonical_url, canonical_key, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			input.SourceID, input.GUID, input.URL, input.Title, input.Author,
			input.PublishedAt, now, input.Content, input.ContentMarkdown,
			input.Summary, input.SummaryProvider, input.SummaryModel,
			summaryGeneratedAt, resolvedKind, canonicalURL, input.CanonicalKey, now,
		)
		if execErr != nil {
			return execErr
		}
		entryID, execErr = result.LastInsertId()
		created = true
		return execErr
	})
	return entryID, created, err
}

// UpsertEntryInput carries the fields an ingested article can set.
type UpsertEntryInput struct {
	SourceID        int64
	GUID            string
	URL             string
	Title           string
	Author          string
	PublishedAt     *string
	Content         string
	ContentMarkdown string
	Summary         string
	SummaryProvider string
	SummaryModel    string
	ContentKind     string
	CanonicalURL    string
	CanonicalKey    *string
}

// GetEntry returns one entry, or nil when it does not exist.
func (d *DB) GetEntry(entryID int64) (*Entry, error) {
	row := d.sql.QueryRow(`SELECT `+entryColumns+`
		FROM entries JOIN sources ON sources.id = entries.source_id
		WHERE entries.id = ?`, entryID)
	entry, err := scanEntry(row, false)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return entry, err
}

// FindEntryByCanonicalKey resolves an entry by its identity key, case-insensitively.
func (d *DB) FindEntryByCanonicalKey(canonicalKey string) (*Entry, error) {
	normalized := strings.ToLower(strings.TrimSpace(canonicalKey))
	if normalized == "" {
		return nil, nil
	}
	row := d.sql.QueryRow(`SELECT `+entryColumns+`
		FROM entries JOIN sources ON sources.id = entries.source_id
		WHERE LOWER(entries.canonical_key) = ?
		ORDER BY entries.id LIMIT 1`, normalized)
	entry, err := scanEntry(row, false)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return entry, err
}

// FindEntryByURL resolves an entry by either its raw or canonical URL.
func (d *DB) FindEntryByURL(url string) (*Entry, error) {
	normalized := strings.TrimSpace(url)
	if normalized == "" {
		return nil, nil
	}
	row := d.sql.QueryRow(`SELECT `+entryColumns+`
		FROM entries JOIN sources ON sources.id = entries.source_id
		WHERE entries.url = ? OR entries.canonical_url = ?
		ORDER BY entries.id LIMIT 1`, normalized, normalized)
	entry, err := scanEntry(row, false)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return entry, err
}

// UpdateEntryMetadata augments an entry without replacing populated fields
// with blanks, so enrichment can only ever add information.
func (d *DB) UpdateEntryMetadata(entryID int64, input MetadataUpdate) (bool, error) {
	now := UTCNow()
	title := strings.TrimSpace(input.Title)
	author := strings.TrimSpace(input.Author)
	content := strings.TrimSpace(input.Content)
	result, err := d.sql.Exec(`
		UPDATE entries SET
			title = CASE WHEN ? <> '' THEN ? ELSE title END,
			author = CASE WHEN ? <> '' THEN ? ELSE author END,
			published_at = COALESCE(?, published_at),
			content = CASE WHEN ? <> '' THEN ? ELSE content END,
			url = COALESCE(?, url),
			canonical_url = COALESCE(?, canonical_url),
			canonical_key = COALESCE(?, canonical_key),
			updated_at = ?
		WHERE id = ?`,
		title, title, author, author, input.PublishedAt, content, content,
		input.URL, input.CanonicalURL, input.CanonicalKey, now, entryID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// MetadataUpdate carries optional enrichment results.
type MetadataUpdate struct {
	Title        string
	Author       string
	Content      string
	PublishedAt  *string
	URL          *string
	CanonicalURL *string
	CanonicalKey *string
}

// EntryFilter selects and pages entries.
type EntryFilter struct {
	Status          string
	Category        string
	ContentKind     string
	RelevanceBucket string
	ProfileID       *int64
	Search          string
	Cursor          string
	// Limit of 0 means unlimited.
	Limit int
}

// ListEntries returns entries newest-first, or by descending score when a
// profile is joined.
func (d *DB) ListEntries(filter EntryFilter) ([]*Entry, error) {
	query, args, ranked, err := buildEntryQuery(filter)
	if err != nil {
		return nil, err
	}
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []*Entry
	for rows.Next() {
		entry, err := scanEntry(rows, ranked)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func buildEntryQuery(filter EntryFilter) (string, []any, bool, error) {
	var clauses []string
	var joinArgs, args []any

	if filter.Status != "" {
		if !valid(ValidStatuses, filter.Status) {
			return "", nil, false, fmt.Errorf("Unknown status: %s", filter.Status)
		}
		clauses = append(clauses, "entries.status = ?")
		args = append(args, filter.Status)
	}
	if filter.Category != "" {
		clauses = append(clauses, "sources.category = ?")
		args = append(args, filter.Category)
	}
	if filter.ContentKind != "" {
		if !valid(ValidContentKinds, filter.ContentKind) {
			return "", nil, false, fmt.Errorf("Unknown content kind: %s", filter.ContentKind)
		}
		clauses = append(clauses, "entries.content_kind = ?")
		args = append(args, filter.ContentKind)
	}
	if search := strings.TrimSpace(filter.Search); search != "" {
		term := "%" + search + "%"
		clauses = append(clauses,
			"(entries.title LIKE ? OR entries.author LIKE ? "+
				"OR entries.summary LIKE ? OR entries.content LIKE ? "+
				"OR sources.title LIKE ? OR sources.category LIKE ?)")
		for i := 0; i < 6; i++ {
			args = append(args, term)
		}
	}

	rankingJoin, rankingFields := "", ""
	ranked := false
	if filter.ProfileID != nil {
		rankingJoin = `JOIN entry_rankings AS selected_ranking
			ON selected_ranking.entry_id = entries.id
			AND selected_ranking.profile_id = ?`
		joinArgs = append(joinArgs, *filter.ProfileID)
		rankingFields = `, selected_ranking.score, selected_ranking.bucket,
			selected_ranking.components_json, selected_ranking.explanation_json,
			selected_ranking.model, selected_ranking.scored_at`
		ranked = true
	}
	if filter.RelevanceBucket != "" {
		if !valid(ValidRelevanceBuckets, filter.RelevanceBucket) {
			return "", nil, false, fmt.Errorf("Unknown relevance bucket: %s", filter.RelevanceBucket)
		}
		if filter.ProfileID != nil {
			clauses = append(clauses, "selected_ranking.bucket = ?")
		} else {
			clauses = append(clauses, `EXISTS (SELECT 1 FROM entry_rankings
				WHERE entry_rankings.entry_id = entries.id
				AND entry_rankings.bucket = ?)`)
		}
		args = append(args, filter.RelevanceBucket)
	}

	if filter.Cursor != "" {
		sortAt, entryID, err := DecodeCursor(filter.Cursor)
		if err != nil {
			return "", nil, false, err
		}
		if ranked {
			score, convErr := strconv.ParseFloat(sortAt, 64)
			if convErr != nil {
				return "", nil, false, fmt.Errorf("Invalid ranked entry cursor")
			}
			clauses = append(clauses,
				"(selected_ranking.score < ? OR (selected_ranking.score = ? AND entries.id < ?))")
			args = append(args, score, score, entryID)
		} else {
			clauses = append(clauses,
				"(COALESCE(entries.published_at, entries.discovered_at) < ? "+
					"OR (COALESCE(entries.published_at, entries.discovered_at) = ? "+
					"AND entries.id < ?))")
			args = append(args, sortAt, sortAt, entryID)
		}
	}

	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	orderBy := "COALESCE(entries.published_at, entries.discovered_at)"
	if ranked {
		orderBy = "selected_ranking.score"
	}
	limitClause := ""
	if filter.Limit != 0 {
		if filter.Limit < 1 {
			return "", nil, false, fmt.Errorf("Limit must be at least 1")
		}
		limitClause = "LIMIT ?"
		args = append(args, filter.Limit)
	}

	query := fmt.Sprintf(`SELECT %s%s
		FROM entries JOIN sources ON sources.id = entries.source_id
		%s %s
		ORDER BY %s DESC, entries.id DESC
		%s`, entryColumns, rankingFields, rankingJoin, where, orderBy, limitClause)
	return query, append(joinArgs, args...), ranked, nil
}

// Page is one page of entries plus the cursor for the next.
type Page struct {
	Entries    []*Entry `json:"entries"`
	NextCursor *string  `json:"next_cursor"`
}

// ListEntriesPage returns a stable page and its continuation cursor.
//
// One extra row is fetched to detect whether more exist, so there is no hidden
// maximum page size.
func (d *DB) ListEntriesPage(filter EntryFilter) (*Page, error) {
	limit := filter.Limit
	if limit < 1 {
		return nil, fmt.Errorf("Limit must be at least 1")
	}
	probe := filter
	probe.Limit = limit + 1
	rows, err := d.ListEntries(probe)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := &Page{Entries: rows}
	if page.Entries == nil {
		page.Entries = []*Entry{}
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		var sortValue string
		switch {
		case last.RelevanceScore != nil:
			sortValue = formatCursorFloat(*last.RelevanceScore)
		case last.PublishedAt != nil:
			sortValue = *last.PublishedAt
		default:
			sortValue = last.DiscoveredAt
		}
		cursor := EncodeCursor(sortValue, last.ID)
		page.NextCursor = &cursor
	}
	return page, nil
}

// formatCursorFloat renders a score the way the reference implementation's
// str(float) did, so cursors issued by either version decode identically.
func formatCursorFloat(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatFloat(value, 'f', 1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// SetStatus applies a triage transition.
func (d *DB) SetStatus(entryID int64, status string) (bool, error) {
	if !valid(ValidStatuses, status) {
		return false, fmt.Errorf("Unknown status: %s", status)
	}
	result, err := d.sql.Exec(
		"UPDATE entries SET status = ?, updated_at = ? WHERE id = ?",
		status, UTCNow(), entryID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// SetSummary records a summary together with its provenance.
func (d *DB) SetSummary(entryID int64, text, provider, model string) (bool, error) {
	now := UTCNow()
	result, err := d.sql.Exec(`
		UPDATE entries SET summary = ?, summary_provider = ?, summary_model = ?,
			summary_generated_at = ?, updated_at = ? WHERE id = ?`,
		text, provider, model, now, now, entryID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// SetEnrichmentState records the outcome of a metadata enrichment attempt.
func (d *DB) SetEnrichmentState(entryID int64, state string, input EnrichmentState) (bool, error) {
	if !valid(ValidEnrichmentStates, state) {
		return false, fmt.Errorf("Unknown enrichment state: %s", state)
	}
	now := UTCNow()
	result, err := d.sql.Exec(`
		UPDATE entries SET
			enrichment_status = ?, enrichment_provider = ?, enrichment_error = ?,
			enriched_at = CASE WHEN ? = 'enriched' THEN ? ELSE enriched_at END,
			canonical_url = COALESCE(?, canonical_url),
			canonical_key = COALESCE(?, canonical_key),
			updated_at = ?
		WHERE id = ?`,
		state, input.Provider, input.Error, state, now,
		input.CanonicalURL, input.CanonicalKey, now, entryID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// EnrichmentState carries the optional fields of an enrichment update.
type EnrichmentState struct {
	Provider     string
	Error        string
	CanonicalURL *string
	CanonicalKey *string
}
