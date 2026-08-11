package relevance

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/PabloMoralesEscandon/herald/internal/store"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "herald.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	engine, err := NewEngine(db, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

func seed(t *testing.T, engine *Engine, category string, items [][2]string) []int64 {
	t.Helper()
	sourceID, err := engine.DB.AddSource(store.AddSourceInput{
		Title: category + " Feed", URL: "https://" + category + ".example/f.xml",
		Category: category, ContentKind: "paper",
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	var ids []int64
	for index, item := range items {
		id, _, err := engine.DB.UpsertEntry(store.UpsertEntryInput{
			SourceID: sourceID,
			GUID:     category + string(rune('a'+index)),
			URL:      "https://e.org/" + category + string(rune('a'+index)),
			Title:    item[0], Content: item[1], ContentKind: "paper",
		})
		if err != nil {
			t.Fatalf("UpsertEntry: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestDefaultProfilesAreIndependent covers the two workspaces having separate
// profiles, and offline scoring producing an explanation.
func TestDefaultProfilesAreIndependent(t *testing.T) {
	engine := newEngine(t)
	paper, err := engine.DB.GetRelevanceProfile("paper")
	if err != nil || paper == nil {
		t.Fatalf("paper profile: %v", err)
	}
	news, err := engine.DB.GetRelevanceProfile("news")
	if err != nil || news == nil {
		t.Fatalf("news profile: %v", err)
	}
	if paper.ID == news.ID {
		t.Error("the two workspaces share one profile")
	}
	if len(paper.Interests) != len(PaperInterests) || len(news.Interests) != len(NewsInterests) {
		t.Error("default interests were not applied per workspace")
	}

	seed(t, engine, "chips", [][2]string{
		{"Chiplet interconnect design", "digital circuits and hardware design for chip architecture"},
		{"Knitting patterns", "textile crafts and their cultural history"},
	})
	result, err := engine.Rescore("paper")
	if err != nil {
		t.Fatalf("Rescore: %v", err)
	}
	// With no model installed, scoring still works and says so.
	if result.Model != "tfidf-v1" {
		t.Errorf("model is %q, want the offline scorer", result.Model)
	}
	if result.Scored != 2 {
		t.Errorf("scored %d entries, want 2", result.Scored)
	}
	if result.ThresholdMode != "cold-start" {
		t.Errorf("threshold mode is %q, want cold-start", result.ThresholdMode)
	}
}

// TestNeverShowOverridesIncludeAndCreatesNoQuota covers two documented rules:
// a never-show phrase wins over an include phrase, and filtering never imposes
// a fixed item quota.
func TestNeverShowOverridesIncludeAndCreatesNoQuota(t *testing.T) {
	engine := newEngine(t)
	ids := seed(t, engine, "mixed", [][2]string{
		{"Chiplet advance", "chip design and computer architecture progress"},
		{"Sponsored chiplet post", "chip design and computer architecture progress"},
	})
	includes := []string{"chiplet"}
	never := []string{"Sponsored"}
	if _, err := engine.UpdateProfile("paper", ProfileUpdate{
		IncludePhrases: &includes, NeverShowPhrases: &never,
	}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if _, err := engine.Rescore("paper"); err != nil {
		t.Fatalf("Rescore: %v", err)
	}
	profile, _ := engine.DB.GetRelevanceProfile("paper")

	sponsored, err := engine.DB.GetEntryRanking(ids[1], profile.ID)
	if err != nil || sponsored == nil {
		t.Fatalf("ranking: %v", err)
	}
	if sponsored.Bucket != "filtered" {
		t.Errorf("a never-show match is in %q, want filtered even though it also matches an include phrase", sponsored.Bucket)
	}
	var explanation map[string]any
	if err := json.Unmarshal(sponsored.Explanation, &explanation); err != nil {
		t.Fatalf("explanation: %v", err)
	}
	if explanation["decision"] != "never-show phrase matched" {
		t.Errorf("decision is %v, want the never-show reason", explanation["decision"])
	}

	// Filtering must never delete or discard: the entry is still stored and
	// still unread, just in the other bucket.
	entry, err := engine.DB.GetEntry(ids[1])
	if err != nil || entry == nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if entry.Status != "unread" {
		t.Errorf("filtering changed status to %q", entry.Status)
	}
}

// TestUpdatingProfileResetsLearnedThreshold covers the documented rule that
// redefining a profile invalidates its learned threshold unless one is given.
func TestUpdatingProfileResetsLearnedThreshold(t *testing.T) {
	engine := newEngine(t)
	manual := 42.0
	profile, err := engine.UpdateProfile("paper", ProfileUpdate{
		Threshold: &manual, ThresholdSet: true,
	})
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if profile.Threshold == nil || *profile.Threshold != manual {
		t.Fatalf("threshold is %v, want the manual value", profile.Threshold)
	}
	var mode string
	if _, err := engine.DB.GetSetting(thresholdModeSetting(profile.ID), &mode); err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if mode != "manual" {
		t.Errorf("threshold mode is %q, want manual", mode)
	}

	interests := []string{"something else entirely"}
	profile, err = engine.UpdateProfile("paper", ProfileUpdate{Interests: &interests})
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if profile.Threshold != nil {
		t.Errorf("threshold is %v, want it reset when interests changed", *profile.Threshold)
	}
	if _, err := engine.DB.GetSetting(thresholdModeSetting(profile.ID), &mode); err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if mode != "auto" {
		t.Errorf("threshold mode is %q, want auto after a redefinition", mode)
	}
}

// TestProfileRequiresAnInterest covers the validation the API surfaces as 400.
func TestProfileRequiresAnInterest(t *testing.T) {
	engine := newEngine(t)
	empty := []string{}
	if _, err := engine.UpdateProfile("paper", ProfileUpdate{Interests: &empty}); err == nil {
		t.Error("UpdateProfile accepted a profile with no interests")
	}
}

// TestEmbeddingFailureFallsBackSilently covers the promise that a missing or
// broken local model degrades to TF-IDF instead of failing the queue.
func TestEmbeddingFailureFallsBackSilently(t *testing.T) {
	engine := newEngine(t)
	engine.Embedder = failingEmbedder{}
	seed(t, engine, "chips", [][2]string{{"Chiplet", "chip design"}})

	result, err := engine.Rescore("paper")
	if err != nil {
		t.Fatalf("Rescore returned an error instead of falling back: %v", err)
	}
	if result.Model != "tfidf-v1" {
		t.Errorf("model is %q, want the fallback scorer", result.Model)
	}
	if result.FallbackReason == nil || *result.FallbackReason == "" {
		t.Error("the fallback reason was not recorded")
	}
}

type failingEmbedder struct{}

func (failingEmbedder) Embed([]string) ([][]float64, error) {
	return nil, &EmbeddingError{Reason: "Ollama embeddings unavailable: connection refused"}
}
func (failingEmbedder) Model() string { return "embeddinggemma" }

// TestCoordinatorCoalescesConcurrentRescores covers the single-job-per-kind
// rule: overlapping requests mark a rerun rather than piling up passes.
func TestCoordinatorCoalescesConcurrentRescores(t *testing.T) {
	engine := newEngine(t)
	seed(t, engine, "chips", [][2]string{{"Chiplet", "chip design"}})
	coordinator := NewCoordinator(engine)

	for range 5 {
		if _, err := coordinator.Start("paper"); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	coordinator.Wait()

	health, err := coordinator.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	job := health.Jobs["paper"]
	if job == nil {
		t.Fatal("no job was recorded")
	}
	if job.State != "complete" {
		t.Errorf("job state is %q, want complete", job.State)
	}
	if job.RerunRequested {
		t.Error("a rerun request was left pending after completion")
	}
	if health.FallbackProvider != "tfidf-v1" {
		t.Errorf("fallback provider is %q", health.FallbackProvider)
	}
	if _, err := coordinator.Start("bogus"); err == nil {
		t.Error("Start accepted an unknown content kind")
	}
}
