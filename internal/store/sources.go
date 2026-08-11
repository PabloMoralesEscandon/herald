package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Source is one subscription row, including its local refresh health.
type Source struct {
	ID                 int64   `json:"id"`
	Title              string  `json:"title"`
	URL                string  `json:"url"`
	Category           string  `json:"category"`
	Enabled            int     `json:"enabled"`
	ContentKind        string  `json:"content_kind"`
	Adapter            string  `json:"adapter"`
	ResolvedURL        string  `json:"resolved_url"`
	ETag               string  `json:"etag"`
	LastModified       string  `json:"last_modified"`
	RefreshAttemptedAt *string `json:"refresh_attempted_at"`
	RefreshSucceededAt *string `json:"refresh_succeeded_at"`
	RefreshError       string  `json:"refresh_error"`
	CreatedAt          string  `json:"created_at"`
}

const sourceColumns = `id, title, url, category, enabled, content_kind, adapter,
	resolved_url, etag, last_modified, refresh_attempted_at,
	refresh_succeeded_at, refresh_error, created_at`

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	var source Source
	err := row.Scan(&source.ID, &source.Title, &source.URL, &source.Category,
		&source.Enabled, &source.ContentKind, &source.Adapter, &source.ResolvedURL,
		&source.ETag, &source.LastModified, &source.RefreshAttemptedAt,
		&source.RefreshSucceededAt, &source.RefreshError, &source.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &source, nil
}

// AddSourceInput describes a subscription to create or update.
//
// Enabled is a pointer so that "not specified" is distinguishable from
// "explicitly disabled": re-seeding the catalog must not silently re-enable a
// feed the user turned off.
type AddSourceInput struct {
	Title       string
	URL         string
	Category    string
	ContentKind string
	Adapter     string
	Enabled     *bool
}

// AddSource inserts or updates a subscription and returns its identifier.
func (d *DB) AddSource(input AddSourceInput) (int64, error) {
	if input.ContentKind == "" {
		input.ContentKind = "paper"
	}
	if !valid(ValidContentKinds, input.ContentKind) {
		return 0, fmt.Errorf("Unknown content kind: %s", input.ContentKind)
	}
	if input.Category == "" {
		input.Category = "Unsorted"
	}
	adapter := strings.TrimSpace(input.Adapter)
	if adapter == "" {
		adapter = "feed"
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	url := strings.TrimSpace(input.URL)

	var sourceID int64
	err := d.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO sources(title, url, category, content_kind, adapter, enabled, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(url) DO UPDATE SET
				title = excluded.title,
				category = excluded.category,
				content_kind = excluded.content_kind,
				adapter = excluded.adapter,
				enabled = CASE WHEN ? THEN excluded.enabled ELSE sources.enabled END`,
			strings.TrimSpace(input.Title), url, strings.TrimSpace(input.Category),
			input.ContentKind, adapter, boolToInt(enabled), UTCNow(),
			boolToInt(input.Enabled != nil))
		if err != nil {
			return err
		}
		return tx.QueryRow("SELECT id FROM sources WHERE url = ?", url).Scan(&sourceID)
	})
	return sourceID, err
}

// ListSources returns subscriptions ordered by category then title.
func (d *DB) ListSources(enabledOnly bool) ([]*Source, error) {
	query := "SELECT " + sourceColumns + " FROM sources"
	if enabledOnly {
		query += " WHERE enabled = 1"
	}
	query += " ORDER BY category, title"
	rows, err := d.sql.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := []*Source{}
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// GetSource returns one subscription, or nil when it does not exist.
func (d *DB) GetSource(sourceID int64) (*Source, error) {
	row := d.sql.QueryRow("SELECT "+sourceColumns+" FROM sources WHERE id = ?", sourceID)
	source, err := scanSource(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return source, err
}

// ImportResult reports what a manifest merge changed.
type ImportResult struct {
	Imported  int `json:"imported"`
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
}

// ImportSourceConfig is one validated subscription to merge.
type ImportSourceConfig struct {
	Title       string
	URL         string
	Category    string
	ContentKind string
	Enabled     bool
}

// ImportSourceConfigs merges a fully validated manifest in one transaction.
//
// The merge is additive: matching URLs are updated, missing URLs are added, and
// local subscriptions absent from the file are retained rather than deleted.
func (d *DB) ImportSourceConfigs(configs []ImportSourceConfig) (ImportResult, error) {
	result := ImportResult{Imported: len(configs)}
	err := d.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT url, title, category, content_kind, enabled FROM sources")
		if err != nil {
			return err
		}
		type snapshot struct {
			title, category, kind string
			enabled               int
		}
		existing := map[string]snapshot{}
		for rows.Next() {
			var url string
			var current snapshot
			if err := rows.Scan(&url, &current.title, &current.category,
				&current.kind, &current.enabled); err != nil {
				rows.Close()
				return err
			}
			existing[url] = current
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, config := range configs {
			previous, present := existing[config.URL]
			switch {
			case !present:
				result.Created++
			case previous.title == config.Title &&
				previous.category == config.Category &&
				previous.kind == config.ContentKind &&
				previous.enabled == boolToInt(config.Enabled):
				result.Unchanged++
			default:
				result.Updated++
			}
			if _, err := tx.Exec(`
				INSERT INTO sources(title, url, category, content_kind, adapter, enabled, created_at)
				VALUES (?, ?, ?, ?, 'feed', ?, ?)
				ON CONFLICT(url) DO UPDATE SET
					title = excluded.title,
					category = excluded.category,
					content_kind = excluded.content_kind,
					adapter = excluded.adapter,
					enabled = excluded.enabled`,
				config.Title, config.URL, config.Category, config.ContentKind,
				boolToInt(config.Enabled), UTCNow()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

// UpdateSourceRefreshState records the outcome of a refresh attempt, including
// the conditional-request validators to send next time.
func (d *DB) UpdateSourceRefreshState(sourceID int64, state RefreshState) (bool, error) {
	now := UTCNow()
	result, err := d.sql.Exec(`
		UPDATE sources SET
			refresh_attempted_at = ?,
			refresh_succeeded_at = CASE WHEN ? THEN ? ELSE refresh_succeeded_at END,
			refresh_error = CASE WHEN ? THEN '' ELSE ? END,
			etag = COALESCE(?, etag),
			last_modified = COALESCE(?, last_modified),
			resolved_url = COALESCE(?, resolved_url)
		WHERE id = ?`,
		now, boolToInt(state.Succeeded), now, boolToInt(state.Succeeded), state.Error,
		state.ETag, state.LastModified, state.ResolvedURL, sourceID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// RefreshState carries a refresh outcome. The pointer fields leave a value
// unchanged when nil.
type RefreshState struct {
	Succeeded    bool
	Error        string
	ETag         *string
	LastModified *string
	ResolvedURL  *string
}

// ListSourceBootstrapSkips returns the guids skipped during a source's first
// refresh, so later refreshes do not re-import them.
func (d *DB) ListSourceBootstrapSkips(sourceID int64) (map[string]bool, error) {
	rows, err := d.sql.Query(
		"SELECT guid FROM source_bootstrap_skips WHERE source_id = ?", sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	skips := map[string]bool{}
	for rows.Next() {
		var guid string
		if err := rows.Scan(&guid); err != nil {
			return nil, err
		}
		skips[guid] = true
	}
	return skips, rows.Err()
}

// AddSourceBootstrapSkips records guids deliberately not imported.
func (d *DB) AddSourceBootstrapSkips(sourceID int64, guids []string) error {
	if len(guids) == 0 {
		return nil
	}
	skippedAt := UTCNow()
	return d.tx(func(tx *sql.Tx) error {
		statement, err := tx.Prepare(`
			INSERT INTO source_bootstrap_skips(source_id, guid, skipped_at)
			VALUES (?, ?, ?) ON CONFLICT(source_id, guid) DO NOTHING`)
		if err != nil {
			return err
		}
		defer statement.Close()
		for _, guid := range guids {
			if _, err := statement.Exec(sourceID, guid, skippedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// Counts is the full-database summary the dashboard header renders.
type Counts struct {
	Total        int                       `json:"total"`
	Statuses     map[string]int            `json:"statuses"`
	Categories   map[string]int            `json:"categories"`
	ContentKinds map[string]int            `json:"content_kinds"`
	Relevance    map[string]map[string]int `json:"relevance"`
	Workspaces   map[string]Workspace      `json:"workspaces"`
}

// Workspace is the per-kind breakdown.
type Workspace struct {
	Total    int            `json:"total"`
	Statuses map[string]int `json:"statuses"`
}

// EntryCounts summarizes the whole database, not just the visible page.
func (d *DB) EntryCounts() (*Counts, error) {
	counts := &Counts{
		Statuses:     map[string]int{},
		Categories:   map[string]int{},
		ContentKinds: map[string]int{},
		Relevance:    map[string]map[string]int{},
		Workspaces:   map[string]Workspace{},
	}
	if err := d.sql.QueryRow("SELECT COUNT(*) FROM entries").Scan(&counts.Total); err != nil {
		return nil, err
	}
	for _, status := range ValidStatuses {
		counts.Statuses[status] = 0
	}
	if err := d.tally("SELECT status, COUNT(*) FROM entries GROUP BY status",
		func(key string, value int) { counts.Statuses[key] = value }); err != nil {
		return nil, err
	}
	if err := d.tally(`SELECT sources.category, COUNT(entries.id)
		FROM sources LEFT JOIN entries ON entries.source_id = sources.id
		GROUP BY sources.category ORDER BY sources.category`,
		func(key string, value int) { counts.Categories[key] = value }); err != nil {
		return nil, err
	}
	for _, kind := range ValidContentKinds {
		counts.ContentKinds[kind] = 0
	}
	if err := d.tally("SELECT content_kind, COUNT(*) FROM entries GROUP BY content_kind",
		func(key string, value int) { counts.ContentKinds[key] = value }); err != nil {
		return nil, err
	}

	for _, kind := range sortedStrings(ValidContentKinds) {
		statuses := map[string]int{}
		for _, status := range ValidStatuses {
			statuses[status] = 0
		}
		counts.Workspaces[kind] = Workspace{Total: counts.ContentKinds[kind], Statuses: statuses}
		counts.Relevance[kind] = map[string]int{
			"pending": counts.ContentKinds[kind], "relevant": 0, "filtered": 0,
		}
	}

	rows, err := d.sql.Query(
		"SELECT content_kind, status, COUNT(*) FROM entries GROUP BY content_kind, status")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, status string
		var count int
		if err := rows.Scan(&kind, &status, &count); err != nil {
			rows.Close()
			return nil, err
		}
		if workspace, ok := counts.Workspaces[kind]; ok {
			workspace.Statuses[status] = count
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Only rankings whose profile matches the entry's own kind are counted,
	// so a paper scored against the news profile is not double counted.
	rows, err = d.sql.Query(`
		SELECT relevance_profiles.content_kind, entry_rankings.bucket, COUNT(*)
		FROM entry_rankings
		JOIN relevance_profiles ON relevance_profiles.id = entry_rankings.profile_id
		JOIN entries ON entries.id = entry_rankings.entry_id
		WHERE entries.content_kind = relevance_profiles.content_kind
		GROUP BY relevance_profiles.content_kind, entry_rankings.bucket`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, bucket string
		var count int
		if err := rows.Scan(&kind, &bucket, &count); err != nil {
			rows.Close()
			return nil, err
		}
		if buckets, ok := counts.Relevance[kind]; ok {
			buckets[bucket] = count
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Anything not yet scored counts as pending.
	for kind, buckets := range counts.Relevance {
		pending := counts.ContentKinds[kind] - buckets["relevant"] - buckets["filtered"]
		if pending < 0 {
			pending = 0
		}
		buckets["pending"] = pending
	}
	return counts, nil
}

func (d *DB) tally(query string, apply func(string, int)) error {
	rows, err := d.sql.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value int
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		apply(key, value)
	}
	return rows.Err()
}

func sortedStrings(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return out
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
