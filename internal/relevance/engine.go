package relevance

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/store"
)

// Engine scores entries against a profile and buckets them.
type Engine struct {
	DB       *store.DB
	Embedder Embedder
}

// NewEngine creates an engine and ensures the default profiles exist.
func NewEngine(db *store.DB, embedder Embedder) (*Engine, error) {
	engine := &Engine{DB: db, Embedder: embedder}
	return engine, engine.EnsureDefaultProfiles()
}

// thresholdModeSetting is where a profile's manual/auto choice is recorded.
func thresholdModeSetting(profileID int64) string {
	return fmt.Sprintf("relevance.threshold_mode.%d", profileID)
}

// EnsureDefaultProfiles creates the paper and news profiles on first run.
func (e *Engine) EnsureDefaultProfiles() error {
	for _, kind := range []string{"paper", "news"} {
		profile, err := e.DB.GetRelevanceProfile(kind)
		if err != nil {
			return err
		}
		if profile == nil {
			profile, err = e.DB.UpsertRelevanceProfile(kind, store.ProfileInput{
				Interests:       DefaultProfiles[kind],
				Selectivity:     "balanced",
				TargetPrecision: 0.82,
			})
			if err != nil {
				return err
			}
		}
		setting := thresholdModeSetting(profile.ID)
		present, err := e.DB.GetSetting(setting, nil)
		if err != nil {
			return err
		}
		if !present {
			if err := e.DB.SetSetting(setting, "auto"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ProfileUpdate is a partial profile change; nil fields are left unchanged.
type ProfileUpdate struct {
	Interests        *[]string
	Exclusions       *[]string
	IncludePhrases   *[]string
	NeverShowPhrases *[]string
	Selectivity      *string
	Threshold        *float64
	ThresholdSet     bool
	TargetPrecision  *float64
}

// UpdateProfile applies a partial change.
//
// Redefining what a profile means invalidates its learned threshold, so the
// threshold resets to automatic unless the caller sets one explicitly.
func (e *Engine) UpdateProfile(contentKind string, update ProfileUpdate) (*store.Profile, error) {
	current, err := e.DB.GetRelevanceProfile(contentKind)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("Unknown content kind: %s", contentKind)
	}

	input := store.ProfileInput{
		Interests:        current.Interests,
		Exclusions:       current.Exclusions,
		IncludePhrases:   current.IncludePhrases,
		NeverShowPhrases: current.NeverShowPhrases,
		Selectivity:      current.Selectivity,
		Threshold:        current.Threshold,
		TargetPrecision:  current.TargetPrecision,
	}
	definitionChanged := false
	if update.Interests != nil {
		input.Interests = *update.Interests
		definitionChanged = true
	}
	if len(input.Interests) == 0 {
		return nil, fmt.Errorf("A relevance profile needs at least one interest")
	}
	if update.Exclusions != nil {
		input.Exclusions = *update.Exclusions
		definitionChanged = true
	}
	if update.IncludePhrases != nil {
		input.IncludePhrases = *update.IncludePhrases
		definitionChanged = true
	}
	if update.NeverShowPhrases != nil {
		input.NeverShowPhrases = *update.NeverShowPhrases
		definitionChanged = true
	}
	if update.Selectivity != nil {
		input.Selectivity = *update.Selectivity
		definitionChanged = true
	}
	if update.TargetPrecision != nil {
		input.TargetPrecision = *update.TargetPrecision
	}
	switch {
	case update.ThresholdSet:
		input.Threshold = update.Threshold
	case definitionChanged:
		input.Threshold = nil
	}

	profile, err := e.DB.UpsertRelevanceProfile(contentKind, input)
	if err != nil {
		return nil, err
	}
	switch {
	case update.ThresholdSet && update.Threshold != nil:
		err = e.DB.SetSetting(thresholdModeSetting(profile.ID), "manual")
	case definitionChanged || update.ThresholdSet:
		err = e.DB.SetSetting(thresholdModeSetting(profile.ID), "auto")
	}
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// RescoreResult summarizes one scoring pass.
type RescoreResult struct {
	ContentKind    string  `json:"content_kind"`
	Scored         int     `json:"scored"`
	Relevant       int     `json:"relevant"`
	Filtered       int     `json:"filtered"`
	Threshold      float64 `json:"threshold"`
	ThresholdMode  string  `json:"threshold_mode"`
	Model          string  `json:"model"`
	FallbackReason *string `json:"fallback_reason"`
}

// scoreDraft is one entry's score before bucketing.
type scoreDraft struct {
	entry          *store.Entry
	score          float64
	components     map[string]float64
	explanation    map[string]any
	forcedFiltered bool
}

// Rescore scores every entry of a kind and writes the resulting buckets.
func (e *Engine) Rescore(contentKind string) (*RescoreResult, error) {
	profile, err := e.DB.GetRelevanceProfile(contentKind)
	if err != nil {
		return nil, err
	}
	if profile == nil {
		return nil, fmt.Errorf("Unknown content kind: %s", contentKind)
	}
	entries, err := e.DB.ListEntries(store.EntryFilter{ContentKind: contentKind})
	if err != nil {
		return nil, err
	}
	feedback, err := e.DB.ListRelevanceFeedback(profile.ID, 200)
	if err != nil {
		return nil, err
	}
	feedbackEntries := make([]feedbackEntry, 0, len(feedback))
	for _, item := range feedback {
		entry, err := e.DB.GetEntry(item.EntryID)
		if err != nil {
			return nil, err
		}
		feedbackEntries = append(feedbackEntries, feedbackEntry{item: item, entry: entry})
	}

	drafts, model, fallbackReason := e.scoreEntries(entries, profile, feedbackEntries)
	threshold, thresholdMode, err := e.chooseThreshold(profile, drafts, feedback)
	if err != nil {
		return nil, err
	}
	if _, err := e.DB.SetRelevanceThreshold(profile.ID, threshold); err != nil {
		return nil, err
	}
	if thresholdMode == "adaptive-feedback" {
		if err := e.DB.SetSetting(thresholdModeSetting(profile.ID), "adaptive"); err != nil {
			return nil, err
		}
	}

	relevant := 0
	for _, draft := range drafts {
		bucket := "relevant"
		if draft.forcedFiltered || draft.score < threshold {
			bucket = "filtered"
		}
		if bucket == "relevant" {
			relevant++
		}
		explanation := map[string]any{}
		for key, value := range draft.explanation {
			explanation[key] = value
		}
		explanation["threshold"] = roundTo(threshold, 3)
		explanation["threshold_mode"] = thresholdMode
		switch {
		case draft.forcedFiltered:
			explanation["decision"] = "never-show phrase matched"
		case bucket == "relevant":
			explanation["decision"] = "score meets threshold"
		default:
			explanation["decision"] = "score is below threshold"
		}
		if _, err := e.DB.UpsertEntryRanking(draft.entry.ID, profile.ID, store.RankingInput{
			Score:       round4(draft.score),
			Bucket:      bucket,
			Components:  draft.components,
			Explanation: explanation,
			Model:       model,
		}); err != nil {
			return nil, err
		}
	}
	return &RescoreResult{
		ContentKind:    contentKind,
		Scored:         len(drafts),
		Relevant:       relevant,
		Filtered:       len(drafts) - relevant,
		Threshold:      roundTo(threshold, 3),
		ThresholdMode:  thresholdMode,
		Model:          model,
		FallbackReason: fallbackReason,
	}, nil
}

type feedbackEntry struct {
	item  *store.Feedback
	entry *store.Entry
}

// scoreEntries computes every entry's score, preferring dense embeddings.
func (e *Engine) scoreEntries(
	entries []*store.Entry,
	profile *store.Profile,
	feedbackEntries []feedbackEntry,
) ([]scoreDraft, string, *string) {
	if len(entries) == 0 {
		return nil, "tfidf", nil
	}
	interestsText := strings.Join(profile.Interests, " ; ")
	exclusionsText := strings.Join(profile.Exclusions, " ; ")
	var positives, negatives []string
	for _, item := range feedbackEntries {
		if item.entry == nil {
			continue
		}
		if item.item.Label == "keep" {
			positives = append(positives, entryText(item.entry))
		} else {
			negatives = append(negatives, entryText(item.entry))
		}
	}
	positiveText := strings.Join(positives, "\n")
	negativeText := strings.Join(negatives, "\n")

	documentTexts := make([]string, len(entries))
	for index, entry := range entries {
		documentTexts[index] = entryText(entry)
	}
	queryTexts := []string{
		interestsText,
		orPlaceholder(exclusionsText),
		orPlaceholder(positiveText),
		orPlaceholder(negativeText),
	}

	var fallbackReason *string
	var denseVectors [][]float64
	if e.Embedder != nil {
		vectors, err := e.Embedder.Embed(append(append([]string{}, documentTexts...), queryTexts...))
		if err != nil {
			reason := err.Error()
			fallbackReason = &reason
		} else {
			denseVectors = vectors
		}
	}

	var similarity func(int, int) float64
	var model string
	if denseVectors != nil {
		documents := denseVectors[:len(entries)]
		queries := denseVectors[len(entries):]
		similarity = func(document, query int) float64 {
			return denseCosine(documents[document], queries[query])
		}
		model = e.Embedder.Model()
	} else {
		sparse := tfidfVectors(append(append([]string{}, documentTexts...), queryTexts...))
		documents := sparse[:len(entries)]
		queries := sparse[len(entries):]
		similarity = func(document, query int) float64 {
			return sparseCosine(documents[document], queries[query])
		}
		model = "tfidf-v1"
	}

	// Keeps and discards on a source shift its future entries slightly.
	sourceFeedback := map[int64]map[string]int{}
	for _, item := range feedbackEntries {
		if item.entry == nil {
			continue
		}
		counts, ok := sourceFeedback[item.entry.SourceID]
		if !ok {
			counts = map[string]int{}
			sourceFeedback[item.entry.SourceID] = counts
		}
		counts[item.item.Label]++
	}

	drafts := make([]scoreDraft, 0, len(entries))
	for index, entry := range entries {
		semantic := math.Max(0, similarity(index, 0))
		exclusion := 0.0
		if exclusionsText != "" {
			exclusion = math.Max(0, similarity(index, 1))
		}
		positive := 0.0
		if positiveText != "" {
			positive = math.Max(0, similarity(index, 2))
		}
		negative := 0.0
		if negativeText != "" {
			negative = math.Max(0, similarity(index, 3))
		}
		feedbackRatio := math.Max(-1.0, math.Min(1.0, positive-negative))
		lexical, matchedInterests := lexicalMatch(entry, profile.Interests)

		searchable := strings.ToLower(entryText(entry))
		includeMatches := matchingPhrases(profile.IncludePhrases, searchable)
		neverMatches := matchingPhrases(profile.NeverShowPhrases, searchable)

		counts := sourceFeedback[entry.SourceID]
		sourceTotal := counts["keep"] + counts["discard"]
		sourceRatio := 0.0
		if sourceTotal > 0 {
			sourceRatio = float64(counts["keep"]) / float64(sourceTotal)
		}

		components := map[string]float64{
			"semantic_interest":  round4(70.0 * semantic),
			"lexical_interest":   round4(25.0 * lexical),
			"feedback_affinity":  round4(15.0 * feedbackRatio),
			"source_affinity":    round4(5.0 * sourceRatio),
			"semantic_exclusion": round4(-25.0 * exclusion),
			"exact_include":      round4(math.Min(10.0, 5.0*float64(len(includeMatches)))),
		}
		total := 0.0
		for _, value := range components {
			total += value
		}
		score := math.Max(0.0, math.Min(100.0, total))

		drafts = append(drafts, scoreDraft{
			entry:      entry,
			score:      score,
			components: components,
			explanation: map[string]any{
				"matched_interests":         matchedInterests,
				"include_matches":           includeMatches,
				"never_show_matches":        neverMatches,
				"embedding_provider":        model,
				"embedding_fallback_reason": fallbackReason,
			},
			// A never-show phrase filters unconditionally, outranking any
			// include phrase and any score.
			forcedFiltered: len(neverMatches) > 0,
		})
	}
	return drafts, model, fallbackReason
}

func matchingPhrases(phrases []string, searchable string) []string {
	matches := []string{}
	for _, phrase := range phrases {
		if strings.Contains(searchable, strings.ToLower(phrase)) {
			matches = append(matches, phrase)
		}
	}
	return matches
}

// orPlaceholder keeps the query vector count fixed when a list is empty.
func orPlaceholder(value string) string {
	if value == "" {
		return "__none__"
	}
	return value
}

// chooseThreshold calibrates the relevant/filtered boundary.
//
// Cold start follows the observed score distribution rather than a fixed floor,
// because an offline TF-IDF queue scores lower than a dense one and a fixed
// floor would empty a perfectly useful queue.
func (e *Engine) chooseThreshold(
	profile *store.Profile,
	drafts []scoreDraft,
	feedback []*store.Feedback,
) (float64, string, error) {
	var scores []float64
	for _, draft := range drafts {
		if !draft.forcedFiltered {
			scores = append(scores, draft.score)
		}
	}
	cold := percentile(scores, selectivityPercentiles[profile.Selectivity])
	current := cold
	if profile.Threshold != nil {
		current = *profile.Threshold
	}

	var mode string
	if _, err := e.DB.GetSetting(thresholdModeSetting(profile.ID), &mode); err != nil {
		return 0, "", err
	}
	if mode == "manual" && profile.Threshold != nil {
		return current, "manual", nil
	}

	keeps, discards := 0, 0
	for _, item := range feedback {
		if item.Label == "keep" {
			keeps++
		} else {
			discards++
		}
	}
	// Learning needs enough signal in both directions to be meaningful.
	if keeps < 5 || discards < 15 {
		return clamp(cold, 0, 100), "cold-start", nil
	}

	byEntry := map[int64]float64{}
	for _, draft := range drafts {
		byEntry[draft.entry.ID] = draft.score
	}
	type labelled struct {
		score float64
		label string
	}
	var labels []labelled
	for _, item := range feedback {
		if score, ok := byEntry[item.EntryID]; ok {
			labels = append(labels, labelled{score: score, label: item.Label})
		}
	}
	distinct := map[float64]bool{}
	for _, item := range labels {
		distinct[item.score] = true
	}
	candidateScores := make([]float64, 0, len(distinct))
	for score := range distinct {
		candidateScores = append(candidateScores, score)
	}
	sort.Float64s(candidateScores)

	var candidates []float64
	for _, candidate := range candidateScores {
		selected, truePositive := 0, 0
		for _, item := range labels {
			if item.score >= candidate {
				selected++
				if item.label == "keep" {
					truePositive++
				}
			}
		}
		precision := 0.0
		if selected > 0 {
			precision = float64(truePositive) / float64(selected)
		}
		if truePositive >= 5 && precision >= profile.TargetPrecision {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return current, "adaptive-no-safe-threshold", nil
	}
	learned := candidates[0]
	for _, candidate := range candidates {
		learned = math.Min(learned, candidate)
	}
	// Cap movement so a small feedback batch cannot abruptly empty or flood
	// the queue.
	smoothed := math.Max(current-3.0, math.Min(current+3.0, learned))
	return clamp(smoothed, 0, 100), "adaptive-feedback", nil
}

func clamp(value, low, high float64) float64 {
	return math.Max(low, math.Min(high, value))
}
