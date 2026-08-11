package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Profile is a relevance profile: what a workspace should surface.
type Profile struct {
	ID               int64    `json:"id"`
	ContentKind      string   `json:"content_kind"`
	Interests        []string `json:"interests"`
	Exclusions       []string `json:"exclusions"`
	IncludePhrases   []string `json:"include_phrases"`
	NeverShowPhrases []string `json:"never_show_phrases"`
	Selectivity      string   `json:"selectivity"`
	Threshold        *float64 `json:"threshold"`
	TargetPrecision  float64  `json:"target_precision"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	ThresholdMode    string   `json:"threshold_mode,omitempty"`
}

// Ranking is one entry's score against one profile.
type Ranking struct {
	EntryID     int64           `json:"entry_id"`
	ProfileID   int64           `json:"profile_id"`
	Score       float64         `json:"score"`
	Bucket      string          `json:"bucket"`
	Components  json.RawMessage `json:"components"`
	Explanation json.RawMessage `json:"explanation"`
	Model       string          `json:"model"`
	ScoredAt    string          `json:"scored_at"`

	// Joined entry columns, used by threshold learning.
	Status   string `json:"status,omitempty"`
	SourceID int64  `json:"source_id,omitempty"`
	Title    string `json:"title,omitempty"`
	Content  string `json:"content,omitempty"`
}

// Feedback is one keep/discard signal used to learn a threshold.
type Feedback struct {
	EntryID   int64  `json:"entry_id"`
	ProfileID int64  `json:"profile_id"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

const profileColumns = `id, content_kind, interests_json, exclusions_json,
	include_phrases_json, never_show_phrases_json, selectivity, threshold,
	target_precision, created_at, updated_at`

func scanProfile(row interface{ Scan(...any) error }) (*Profile, error) {
	var profile Profile
	var interests, exclusions, includes, never string
	if err := row.Scan(&profile.ID, &profile.ContentKind, &interests, &exclusions,
		&includes, &never, &profile.Selectivity, &profile.Threshold,
		&profile.TargetPrecision, &profile.CreatedAt, &profile.UpdatedAt); err != nil {
		return nil, err
	}
	for target, encoded := range map[*[]string]string{
		&profile.Interests:        interests,
		&profile.Exclusions:       exclusions,
		&profile.IncludePhrases:   includes,
		&profile.NeverShowPhrases: never,
	} {
		if err := json.Unmarshal([]byte(encoded), target); err != nil {
			return nil, err
		}
		if *target == nil {
			*target = []string{}
		}
	}
	return &profile, nil
}

// ProfileInput is a complete profile definition.
type ProfileInput struct {
	Interests        []string
	Exclusions       []string
	IncludePhrases   []string
	NeverShowPhrases []string
	Selectivity      string
	Threshold        *float64
	TargetPrecision  float64
}

// UpsertRelevanceProfile writes a profile, deduplicating and trimming each
// phrase list case-insensitively while preserving the order given.
func (d *DB) UpsertRelevanceProfile(contentKind string, input ProfileInput) (*Profile, error) {
	if !valid(ValidContentKinds, contentKind) {
		return nil, fmt.Errorf("Unknown content kind: %s", contentKind)
	}
	if input.Selectivity == "" {
		input.Selectivity = "balanced"
	}
	if !valid(ValidSelectivity, input.Selectivity) {
		return nil, fmt.Errorf("Unknown selectivity: %s", input.Selectivity)
	}
	if input.Threshold != nil && (*input.Threshold < 0 || *input.Threshold > 100) {
		return nil, fmt.Errorf("Threshold must be between 0 and 100")
	}
	if input.TargetPrecision < 0 || input.TargetPrecision > 1 {
		return nil, fmt.Errorf("Target precision must be between 0 and 1")
	}
	encoded := make([]string, 4)
	for index, values := range [][]string{
		input.Interests, input.Exclusions, input.IncludePhrases, input.NeverShowPhrases,
	} {
		document, err := json.Marshal(cleanPhrases(values))
		if err != nil {
			return nil, err
		}
		encoded[index] = string(document)
	}

	var profile *Profile
	err := d.tx(func(tx *sql.Tx) error {
		now := UTCNow()
		if _, err := tx.Exec(`
			INSERT INTO relevance_profiles(
				content_kind, interests_json, exclusions_json, include_phrases_json,
				never_show_phrases_json, selectivity, threshold, target_precision,
				created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(content_kind) DO UPDATE SET
				interests_json = excluded.interests_json,
				exclusions_json = excluded.exclusions_json,
				include_phrases_json = excluded.include_phrases_json,
				never_show_phrases_json = excluded.never_show_phrases_json,
				selectivity = excluded.selectivity,
				threshold = excluded.threshold,
				target_precision = excluded.target_precision,
				updated_at = excluded.updated_at`,
			contentKind, encoded[0], encoded[1], encoded[2], encoded[3],
			input.Selectivity, input.Threshold, input.TargetPrecision, now, now); err != nil {
			return err
		}
		row := tx.QueryRow("SELECT "+profileColumns+" FROM relevance_profiles WHERE content_kind = ?", contentKind)
		var err error
		profile, err = scanProfile(row)
		return err
	})
	return profile, err
}

// cleanPhrases trims, drops blanks, and removes case-insensitive duplicates
// while keeping the first spelling the user typed.
func cleanPhrases(values []string) []string {
	cleaned := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		folded := strings.ToLower(normalized)
		if normalized == "" || seen[folded] {
			continue
		}
		cleaned = append(cleaned, normalized)
		seen[folded] = true
	}
	return cleaned
}

// GetRelevanceProfile returns a profile, or nil when none exists yet.
func (d *DB) GetRelevanceProfile(contentKind string) (*Profile, error) {
	if !valid(ValidContentKinds, contentKind) {
		return nil, fmt.Errorf("Unknown content kind: %s", contentKind)
	}
	row := d.sql.QueryRow("SELECT "+profileColumns+" FROM relevance_profiles WHERE content_kind = ?", contentKind)
	profile, err := scanProfile(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return profile, err
}

// ListRelevanceProfiles returns every profile, ordered by kind.
func (d *DB) ListRelevanceProfiles() ([]*Profile, error) {
	rows, err := d.sql.Query("SELECT " + profileColumns + " FROM relevance_profiles ORDER BY content_kind")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := []*Profile{}
	for rows.Next() {
		profile, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

// SetRelevanceThreshold stores a learned or manual threshold.
func (d *DB) SetRelevanceThreshold(profileID int64, threshold float64) (bool, error) {
	if threshold < 0 || threshold > 100 {
		return false, fmt.Errorf("Threshold must be between 0 and 100")
	}
	result, err := d.sql.Exec(
		"UPDATE relevance_profiles SET threshold = ?, updated_at = ? WHERE id = ?",
		threshold, UTCNow(), profileID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// RankingInput is one scoring result to persist.
type RankingInput struct {
	Score       float64
	Bucket      string
	Components  any
	Explanation any
	Model       string
}

// UpsertEntryRanking stores an entry's score against a profile.
func (d *DB) UpsertEntryRanking(entryID, profileID int64, input RankingInput) (*Ranking, error) {
	if !valid(ValidRelevanceBuckets, input.Bucket) {
		return nil, fmt.Errorf("Unknown relevance bucket: %s", input.Bucket)
	}
	components, err := marshalSorted(input.Components)
	if err != nil {
		return nil, err
	}
	explanation, err := marshalSorted(input.Explanation)
	if err != nil {
		return nil, err
	}
	var ranking *Ranking
	err = d.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO entry_rankings(
				entry_id, profile_id, score, bucket, components_json,
				explanation_json, model, scored_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(entry_id, profile_id) DO UPDATE SET
				score = excluded.score,
				bucket = excluded.bucket,
				components_json = excluded.components_json,
				explanation_json = excluded.explanation_json,
				model = excluded.model,
				scored_at = excluded.scored_at`,
			entryID, profileID, input.Score, input.Bucket, components,
			explanation, input.Model, UTCNow()); err != nil {
			return err
		}
		row := tx.QueryRow(`
			SELECT entry_id, profile_id, score, bucket, components_json,
				explanation_json, model, scored_at
			FROM entry_rankings WHERE entry_id = ? AND profile_id = ?`,
			entryID, profileID)
		ranking = &Ranking{}
		var components, explanation string
		if err := row.Scan(&ranking.EntryID, &ranking.ProfileID, &ranking.Score,
			&ranking.Bucket, &components, &explanation, &ranking.Model,
			&ranking.ScoredAt); err != nil {
			return err
		}
		ranking.Components = json.RawMessage(components)
		ranking.Explanation = json.RawMessage(explanation)
		return nil
	})
	return ranking, err
}

// GetEntryRanking returns one score, or nil when the entry is unscored.
func (d *DB) GetEntryRanking(entryID, profileID int64) (*Ranking, error) {
	row := d.sql.QueryRow(`
		SELECT entry_id, profile_id, score, bucket, components_json,
			explanation_json, model, scored_at
		FROM entry_rankings WHERE entry_id = ? AND profile_id = ?`,
		entryID, profileID)
	var ranking Ranking
	var components, explanation string
	err := row.Scan(&ranking.EntryID, &ranking.ProfileID, &ranking.Score,
		&ranking.Bucket, &components, &explanation, &ranking.Model, &ranking.ScoredAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ranking.Components = json.RawMessage(components)
	ranking.Explanation = json.RawMessage(explanation)
	return &ranking, nil
}

// ListEntryRankings returns a profile's scores, highest first.
func (d *DB) ListEntryRankings(profileID int64) ([]*Ranking, error) {
	rows, err := d.sql.Query(`
		SELECT entry_rankings.entry_id, entry_rankings.profile_id,
			entry_rankings.score, entry_rankings.bucket,
			entry_rankings.components_json, entry_rankings.explanation_json,
			entry_rankings.model, entry_rankings.scored_at,
			entries.status, entries.source_id, entries.title, entries.content
		FROM entry_rankings
		JOIN entries ON entries.id = entry_rankings.entry_id
		WHERE profile_id = ?
		ORDER BY score DESC, entry_id DESC`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rankings := []*Ranking{}
	for rows.Next() {
		var ranking Ranking
		var components, explanation string
		if err := rows.Scan(&ranking.EntryID, &ranking.ProfileID, &ranking.Score,
			&ranking.Bucket, &components, &explanation, &ranking.Model,
			&ranking.ScoredAt, &ranking.Status, &ranking.SourceID,
			&ranking.Title, &ranking.Content); err != nil {
			return nil, err
		}
		ranking.Components = json.RawMessage(components)
		ranking.Explanation = json.RawMessage(explanation)
		rankings = append(rankings, &ranking)
	}
	return rankings, rows.Err()
}

// RecordRelevanceFeedback stores a keep or discard signal.
func (d *DB) RecordRelevanceFeedback(entryID, profileID int64, label string) error {
	if !valid(ValidFeedbackLabels, label) {
		return fmt.Errorf("Unknown feedback label: %s", label)
	}
	now := UTCNow()
	_, err := d.sql.Exec(`
		INSERT INTO entry_feedback(entry_id, profile_id, label, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(entry_id, profile_id) DO UPDATE SET
			label = excluded.label, updated_at = excluded.updated_at`,
		entryID, profileID, label, now, now)
	return err
}

// ClearRelevanceFeedback removes a signal when an entry leaves kept/discarded.
func (d *DB) ClearRelevanceFeedback(entryID, profileID int64) (bool, error) {
	result, err := d.sql.Exec(
		"DELETE FROM entry_feedback WHERE entry_id = ? AND profile_id = ?",
		entryID, profileID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// ListRelevanceFeedback returns a profile's signals, most recent first.
func (d *DB) ListRelevanceFeedback(profileID int64, limit int) ([]*Feedback, error) {
	query := `SELECT entry_id, profile_id, label, created_at, updated_at
		FROM entry_feedback WHERE profile_id = ?
		ORDER BY updated_at DESC, entry_id DESC`
	args := []any{profileID}
	if limit != 0 {
		if limit < 1 {
			return nil, fmt.Errorf("Limit must be at least 1")
		}
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	feedback := []*Feedback{}
	for rows.Next() {
		var item Feedback
		if err := rows.Scan(&item.EntryID, &item.ProfileID, &item.Label,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		feedback = append(feedback, &item)
	}
	return feedback, rows.Err()
}

// PutEmbedding caches one vector keyed by content hash and model.
func (d *DB) PutEmbedding(contentHash, model string, dimensions int, vector []byte) error {
	if contentHash == "" || model == "" || dimensions < 1 || len(vector) == 0 {
		return fmt.Errorf("Embedding metadata and vector must be non-empty")
	}
	_, err := d.sql.Exec(`
		INSERT INTO embedding_cache(content_hash, model, dimensions, vector, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(content_hash, model) DO UPDATE SET
			dimensions = excluded.dimensions,
			vector = excluded.vector,
			created_at = excluded.created_at`,
		contentHash, model, dimensions, vector, UTCNow())
	return err
}

// Embedding is a cached vector.
type Embedding struct {
	Dimensions int
	Vector     []byte
}

// GetEmbedding returns a cached vector, or nil on a miss.
func (d *DB) GetEmbedding(contentHash, model string) (*Embedding, error) {
	row := d.sql.QueryRow(
		"SELECT dimensions, vector FROM embedding_cache WHERE content_hash = ? AND model = ?",
		contentHash, model)
	var embedding Embedding
	err := row.Scan(&embedding.Dimensions, &embedding.Vector)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &embedding, err
}

// marshalSorted encodes with sorted map keys so stored JSON is stable across
// runs and diffs of exported notes stay readable.
func marshalSorted(value any) (string, error) {
	if value == nil {
		return "{}", nil
	}
	document, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	// encoding/json already sorts map keys; struct field order is fixed.
	return string(document), nil
}
