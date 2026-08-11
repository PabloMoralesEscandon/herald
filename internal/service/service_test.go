package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/paper"
	"github.com/PabloMoralesEscandon/herald/internal/relevance"
	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/summary"
	"github.com/PabloMoralesEscandon/herald/internal/vault"
)

// offlineSummarizer never touches the network.
type offlineSummarizer struct{}

func (offlineSummarizer) Summarize(title, content string) summary.Result {
	return summary.Result{
		Text: summary.Deterministic(title, content), Provider: "fallback",
		FallbackReason: "summarization disabled in tests",
	}
}

func newService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	vaultPath := filepath.Join(root, "vault")
	if err := os.MkdirAll(vaultPath, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	db, err := store.Open(filepath.Join(root, "herald.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	engine, err := relevance.NewEngine(db, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	importer := paper.NewImporter(db)
	importer.RequestDelay = 0
	importer.Sleep = func(time.Duration) {}
	importer.Fetcher = func(string, map[string]string, int, time.Duration) ([]byte, error) {
		return nil, &paper.FetchError{Reason: "network disabled in tests"}
	}
	svc := New(db, Options{
		Summarizer:   offlineSummarizer{},
		Relevance:    relevance.NewCoordinator(engine),
		Importer:     importer,
		DefaultVault: vaultPath,
		ArchiveRoot:  filepath.Join(root, "archive"),
	})
	return svc, vaultPath
}

func seedEntry(t *testing.T, svc *Service, kind string) int64 {
	t.Helper()
	sourceID, err := svc.DB.AddSource(store.AddSourceInput{
		Title: "Systems Lab", URL: "https://lab.example/f.xml",
		Category: "Chip Design", ContentKind: kind,
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	published := "2026-08-06T10:00:00+00:00"
	entryID, _, err := svc.DB.UpsertEntry(store.UpsertEntryInput{
		SourceID: sourceID, GUID: "g1", URL: "https://example.org/a",
		CanonicalURL: "https://example.org/a", Title: "A Chiplet Interconnect",
		Author: "Ada Researcher", PublishedAt: &published,
		Content: "A chiplet interconnect that lowers latency.", ContentKind: kind,
	})
	if err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	return entryID
}

// withTimeout fails the test if fn does not finish, converting a deadlock into
// a clear failure rather than a hung test binary.
func withTimeout(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not complete: the triage path is deadlocked", name)
	}
}

// TestKeepDoesNotDeadlock is a regression test for a real deadlock.
//
// The triage transition holds a lock and then calls into the exporter, which
// needs to know whether an enrichment is in flight. Go mutexes are not
// reentrant, so guarding both with one lock hangs every later request behind
// the first Keep.
func TestKeepDoesNotDeadlock(t *testing.T) {
	svc, _ := newService(t)
	entryID := seedEntry(t, svc, "paper")

	withTimeout(t, "keep", func() {
		if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
			t.Errorf("ChangeStatus: %v", err)
		}
	})
	// A second request must not be queued behind a stuck lock.
	withTimeout(t, "summarize after keep", func() {
		if _, err := svc.SummarizeEntry(entryID); err != nil {
			t.Errorf("SummarizeEntry: %v", err)
		}
	})
	withTimeout(t, "discard", func() {
		if _, err := svc.ChangeStatus(entryID, "discarded"); err != nil {
			t.Errorf("ChangeStatus: %v", err)
		}
	})
	svc.Relevance.Wait()
	svc.WaitForEnrichment()
}

// TestKeepWritesNoteAndSurvivesUnkeep covers the documented lifecycle.
func TestKeepWritesNoteAndSurvivesUnkeep(t *testing.T) {
	svc, vaultPath := newService(t)
	entryID := seedEntry(t, svc, "paper")

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("keep: %v", err)
	}
	export, err := svc.DB.GetObsidianExport(entryID)
	if err != nil || export == nil {
		t.Fatalf("no export recorded: %v", err)
	}
	if export.State != "synced" {
		t.Errorf("export state is %q, want synced", export.State)
	}
	notePath := filepath.Join(vaultPath, filepath.FromSlash(export.RelativePath))
	document, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatalf("note was not written: %v", err)
	}
	if !strings.Contains(string(document), vault.FrontmatterStart) {
		t.Error("the note has no managed frontmatter block")
	}

	// Un-keeping archives the note out of the vault.
	if _, err := svc.ChangeStatus(entryID, "unread"); err != nil {
		t.Fatalf("unkeep: %v", err)
	}
	if _, err := os.Stat(notePath); !os.IsNotExist(err) {
		t.Error("the note is still in the vault after un-keeping")
	}
	export, _ = svc.DB.GetObsidianExport(entryID)
	if export.State != "archived" {
		t.Errorf("export state is %q, want archived", export.State)
	}

	// Keeping again restores it.
	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("re-keep: %v", err)
	}
	if _, err := os.Stat(notePath); err != nil {
		t.Errorf("the note was not restored: %v", err)
	}
	svc.Relevance.Wait()
	svc.WaitForEnrichment()
}

// TestKeepSucceedsWhenTheVaultIsUnavailable covers the central promise: reading
// state is committed before any filesystem work, so a broken vault records a
// sync error instead of undoing the Keep.
func TestKeepSucceedsWhenTheVaultIsUnavailable(t *testing.T) {
	svc, vaultPath := newService(t)
	entryID := seedEntry(t, svc, "paper")

	// Replace the vault directory with a file so no note can be written.
	if err := os.RemoveAll(vaultPath); err != nil {
		t.Fatalf("remove vault: %v", err)
	}
	if err := os.WriteFile(vaultPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	result, err := svc.ChangeStatus(entryID, "kept")
	if err != nil {
		t.Fatalf("Keep failed because the vault was unavailable: %v", err)
	}
	if status, _ := result["status"].(string); status != "kept" {
		t.Errorf("status is %q, want the keep to stand", status)
	}
	entry, _ := svc.DB.GetEntry(entryID)
	if entry.Status != "kept" {
		t.Errorf("stored status is %q, want kept", entry.Status)
	}
	// The failure is recorded so the dashboard can offer a retry.
	export, _ := svc.DB.GetObsidianExport(entryID)
	if export == nil {
		t.Fatal("no sync state was recorded for the failure")
	}
	if export.State != "failed" && export.State != "conflict" {
		t.Errorf("export state is %q, want a recorded failure", export.State)
	}
	if export.Error == "" {
		t.Error("no error message was recorded")
	}
	svc.Relevance.Wait()
	svc.WaitForEnrichment()
}

// TestTriageRecordsRelevanceFeedback covers keep/discard feeding the ranker and
// leaving that state when the entry returns to unread.
func TestTriageRecordsRelevanceFeedback(t *testing.T) {
	svc, _ := newService(t)
	entryID := seedEntry(t, svc, "paper")
	profile, err := svc.DB.GetRelevanceProfile("paper")
	if err != nil || profile == nil {
		t.Fatalf("profile: %v", err)
	}

	for _, step := range []struct {
		status string
		want   string
	}{{"kept", "keep"}, {"discarded", "discard"}} {
		if _, err := svc.ChangeStatus(entryID, step.status); err != nil {
			t.Fatalf("%s: %v", step.status, err)
		}
		feedback, err := svc.DB.ListRelevanceFeedback(profile.ID, 0)
		if err != nil {
			t.Fatalf("feedback: %v", err)
		}
		if len(feedback) != 1 || feedback[0].Label != step.want {
			t.Errorf("after %s the feedback is %+v, want one %q", step.status, feedback, step.want)
		}
	}

	if _, err := svc.ChangeStatus(entryID, "unread"); err != nil {
		t.Fatalf("unread: %v", err)
	}
	feedback, _ := svc.DB.ListRelevanceFeedback(profile.ID, 0)
	if len(feedback) != 0 {
		t.Errorf("returning to unread left feedback %+v", feedback)
	}
	svc.Relevance.Wait()
	svc.WaitForEnrichment()
}

// TestRefreshIsolatesABadFeed covers one dead source never blocking the others.
func TestRefreshIsolatesABadFeed(t *testing.T) {
	svc, _ := newService(t)
	good, err := svc.DB.AddSource(store.AddSourceInput{
		Title: "Good", URL: "https://good.example/f.xml",
		Category: "Chips", ContentKind: "paper",
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if _, err := svc.DB.AddSource(store.AddSourceInput{
		Title: "Bad", URL: "https://bad.example/f.xml",
		Category: "Chips", ContentKind: "paper",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	svc.Fetcher = func(url, etag, lastModified string) (FeedResponse, error) {
		if strings.Contains(url, "bad.example") {
			return FeedResponse{}, &FetchError{Reason: "Could not fetch: refused"}
		}
		return FeedResponse{URL: url, Document: []byte(`<?xml version="1.0"?>
			<rss version="2.0"><channel><item>
				<guid>a-1</guid><title>Good Item</title>
				<link>https://good.example/a</link>
			</item></channel></rss>`)}, nil
	}

	results, err := svc.RefreshAll()
	if err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	var created, failed int
	for _, result := range results {
		created += result.Created
		if result.Error != nil {
			failed++
		}
	}
	if created != 1 {
		t.Errorf("ingested %d entries, want the healthy feed's 1", created)
	}
	if failed != 1 {
		t.Errorf("recorded %d failures, want 1", failed)
	}
	// The failure is recorded on the source itself for the dashboard.
	source, _ := svc.DB.GetSource(good)
	if source.RefreshSucceededAt == nil {
		t.Error("the healthy source was not marked as refreshed")
	}
	svc.Relevance.Wait()
}

// TestNewsBootstrapTakesOnlyRecentItems covers the documented 30-day window on
// a news source's first refresh.
func TestNewsBootstrapTakesOnlyRecentItems(t *testing.T) {
	svc, _ := newService(t)
	svc.Now = func() time.Time {
		return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	}
	sourceID, err := svc.DB.AddSource(store.AddSourceInput{
		Title: "Newsroom", URL: "https://news.example/f.xml",
		Category: "AI News", ContentKind: "news",
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	svc.Fetcher = func(url, etag, lastModified string) (FeedResponse, error) {
		return FeedResponse{URL: url, Document: []byte(`<?xml version="1.0"?>
			<rss version="2.0"><channel>
				<item><guid>recent</guid><title>Recent</title>
					<link>https://news.example/recent</link>
					<pubDate>Fri, 07 Aug 2026 09:00:00 GMT</pubDate></item>
				<item><guid>old</guid><title>Old</title>
					<link>https://news.example/old</link>
					<pubDate>Thu, 01 Jan 2026 09:00:00 GMT</pubDate></item>
			</channel></rss>`)}, nil
	}

	result, err := svc.RefreshSource(sourceID)
	if err != nil {
		t.Fatalf("RefreshSource: %v", err)
	}
	if result.Created != 1 {
		t.Errorf("bootstrap imported %d items, want only the recent one", result.Created)
	}

	// A later refresh accepts everything newly observed, but must not
	// re-import what bootstrap deliberately skipped.
	second, err := svc.RefreshSource(sourceID)
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if second.Created != 0 {
		t.Errorf("the second refresh imported %d skipped items, want 0", second.Created)
	}
	svc.Relevance.Wait()
}

// TestConfigureObsidianValidatesThePath covers the documented rule that only an
// existing absolute directory is accepted.
func TestConfigureObsidianValidatesThePath(t *testing.T) {
	svc, _ := newService(t)
	root := t.TempDir()
	notADirectory := filepath.Join(root, "file.md")
	if err := os.WriteFile(notADirectory, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, invalid := range []string{
		"relative/path",
		filepath.Join(root, "does-not-exist"),
		notADirectory,
	} {
		if _, err := svc.ConfigureObsidian(invalid); err == nil {
			t.Errorf("ConfigureObsidian accepted %q", invalid)
		}
	}
}

// TestChangingVaultPreservesAnnotations covers the documented move: managed
// notes are archived out of the old vault and restored into the new one, so a
// user's own additions follow the move.
func TestChangingVaultPreservesAnnotations(t *testing.T) {
	svc, oldVault := newService(t)
	entryID := seedEntry(t, svc, "paper")
	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("keep: %v", err)
	}
	export, _ := svc.DB.GetObsidianExport(entryID)
	oldNote := filepath.Join(oldVault, filepath.FromSlash(export.RelativePath))
	annotated := strings.TrimRight(mustReadFile(t, oldNote), "\n") + "\nMy annotation.\n"
	if err := os.WriteFile(oldNote, []byte(annotated), 0o644); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	newVault := filepath.Join(t.TempDir(), "new-vault")
	if err := os.MkdirAll(newVault, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	settings, err := svc.ConfigureObsidian(newVault)
	if err != nil {
		t.Fatalf("ConfigureObsidian: %v", err)
	}
	if !settings.Configured {
		t.Error("the new vault was not recorded as configured")
	}

	export, _ = svc.DB.GetObsidianExport(entryID)
	if export.State != "synced" {
		t.Fatalf("export state is %q, want synced in the new vault", export.State)
	}
	newNote := filepath.Join(newVault, filepath.FromSlash(export.RelativePath))
	if !strings.Contains(mustReadFile(t, newNote), "My annotation.") {
		t.Error("the annotation did not follow the move to the new vault")
	}
	if _, err := os.Stat(oldNote); !os.IsNotExist(err) {
		t.Error("the note was left behind in the old vault")
	}
	svc.Relevance.Wait()
	svc.WaitForEnrichment()
}

// TestConfigureObsidianIsANoOpForTheSameVaultViaSymlink covers path
// canonicalization at the settings boundary: naming the current vault through a
// symlink must be recognized as the same vault, not treated as a move that
// archives every note.
func TestConfigureObsidianIsANoOpForTheSameVaultViaSymlink(t *testing.T) {
	svc, vaultPath := newService(t)
	entryID := seedEntry(t, svc, "paper")
	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("keep: %v", err)
	}
	before, _ := svc.DB.GetObsidianExport(entryID)

	link := filepath.Join(t.TempDir(), "vault-link")
	if err := os.Symlink(vaultPath, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := svc.ConfigureObsidian(link); err != nil {
		t.Fatalf("ConfigureObsidian via symlink: %v", err)
	}

	after, _ := svc.DB.GetObsidianExport(entryID)
	if after.State != before.State || after.RelativePath != before.RelativePath {
		t.Errorf("naming the same vault through a symlink disturbed the note:\n  before: %s %s\n  after:  %s %s",
			before.State, before.RelativePath, after.State, after.RelativePath)
	}
	if _, err := os.Stat(filepath.Join(vaultPath, filepath.FromSlash(after.RelativePath))); err != nil {
		t.Errorf("the note was archived out of its own vault: %v", err)
	}
	svc.Relevance.Wait()
	svc.WaitForEnrichment()
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(document)
}
