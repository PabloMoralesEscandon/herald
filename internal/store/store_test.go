package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "herald.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return db
}

func addSource(t *testing.T, db *DB, title, url, kind string) int64 {
	t.Helper()
	id, err := db.AddSource(AddSourceInput{
		Title: title, URL: url, Category: "Chips", ContentKind: kind,
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	return id
}

// TestInitializeIsIdempotent covers the guarantee that Herald applies schema
// migrations on every start.
func TestInitializeIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	for range 3 {
		if err := db.Initialize(); err != nil {
			t.Fatalf("repeat Initialize: %v", err)
		}
	}
	var version int
	if err := db.SQL().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version is %d, want %d", version, schemaVersion)
	}
}

// TestLegacyDatabaseMigratesWithoutResettingState pins the upgrade path: a
// database written before content_kind, canonical_url, and the export table
// existed must gain them without losing triage state.
func TestLegacyDatabaseMigratesWithoutResettingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE sources (
			id INTEGER PRIMARY KEY, title TEXT NOT NULL, url TEXT NOT NULL UNIQUE,
			category TEXT NOT NULL DEFAULT 'Unsorted',
			enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL);
		CREATE TABLE entries (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
			guid TEXT NOT NULL, url TEXT NOT NULL, title TEXT NOT NULL,
			author TEXT NOT NULL DEFAULT '', published_at TEXT,
			discovered_at TEXT NOT NULL, content TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'unread'
				CHECK (status IN ('unread','read','kept','discarded')),
			exported_path TEXT, UNIQUE(source_id, guid));
		INSERT INTO sources(id,title,url,category,enabled,created_at)
			VALUES (1,'Legacy','https://legacy.example/f.xml','Chips',1,'2026-01-01T00:00:00+00:00');
		INSERT INTO entries(id,source_id,guid,url,title,discovered_at,content,summary,status,exported_path)
			VALUES (1,1,'g1','https://legacy.example/a','Kept','2026-01-02T00:00:00+00:00','Body.','Sum.','kept','Herald/Papers/old.md'),
			       (2,1,'g2','https://legacy.example/b','Read','2026-01-03T00:00:00+00:00','Body.','','read',NULL);`); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	legacy.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	kept, err := db.GetEntry(1)
	if err != nil || kept == nil {
		t.Fatalf("GetEntry(1): %v", err)
	}
	if kept.Status != "kept" {
		t.Errorf("status is %q, want kept: migration reset triage state", kept.Status)
	}
	if kept.ContentKind != "paper" {
		t.Errorf("content_kind is %q, want the paper default", kept.ContentKind)
	}
	// canonical_url is backfilled from url so deduplication works immediately.
	if kept.CanonicalURL != "https://legacy.example/a" {
		t.Errorf("canonical_url is %q, want it backfilled from url", kept.CanonicalURL)
	}
	// A pre-existing summary gains explicit provenance rather than being
	// mistaken for AI output.
	if kept.SummaryProvider != "unknown" {
		t.Errorf("summary_provider is %q, want unknown", kept.SummaryProvider)
	}
	if kept.UpdatedAt == nil {
		t.Error("updated_at was not backfilled from discovered_at")
	}

	// The existing exported note is adopted into the export table as synced.
	export, err := db.GetObsidianExport(1)
	if err != nil || export == nil {
		t.Fatalf("GetObsidianExport: %v", err)
	}
	if export.State != "synced" || export.RelativePath != "Herald/Papers/old.md" {
		t.Errorf("export is %+v, want the legacy note adopted as synced", export)
	}

	unread, err := db.GetEntry(2)
	if err != nil || unread == nil {
		t.Fatalf("GetEntry(2): %v", err)
	}
	if unread.SummaryProvider != "" {
		t.Errorf("summary_provider is %q, want empty for an unsummarized entry", unread.SummaryProvider)
	}
}

// TestUpsertDeduplicatesAcrossSources covers cross-feed deduplication: the same
// article syndicated to two feeds must collapse into one entry and keep the
// triage already applied to it.
func TestUpsertDeduplicatesAcrossSources(t *testing.T) {
	db := newTestDB(t)
	first := addSource(t, db, "Feed A", "https://a.example/f.xml", "news")
	second := addSource(t, db, "Feed B", "https://b.example/f.xml", "news")

	originalID, created, err := db.UpsertEntry(UpsertEntryInput{
		SourceID: first, GUID: "a-1", URL: "https://news.example/story",
		CanonicalURL: "https://news.example/story", Title: "Story", Content: "Body",
	})
	if err != nil || !created {
		t.Fatalf("first upsert: id=%d created=%v err=%v", originalID, created, err)
	}
	if _, err := db.SetStatus(originalID, "kept"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	duplicateID, created, err := db.UpsertEntry(UpsertEntryInput{
		SourceID: second, GUID: "b-9", URL: "https://news.example/story",
		CanonicalURL: "https://news.example/story", Title: "Story (syndicated)", Content: "Body",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if created {
		t.Error("the syndicated copy was inserted as a new entry")
	}
	if duplicateID != originalID {
		t.Errorf("duplicate resolved to %d, want the original %d", duplicateID, originalID)
	}
	entry, err := db.GetEntry(originalID)
	if err != nil || entry == nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if entry.Status != "kept" {
		t.Errorf("status is %q, want the existing keep preserved", entry.Status)
	}
	if entry.Title != "Story (syndicated)" {
		t.Errorf("title is %q, want the newer feed's title", entry.Title)
	}
}

// TestUpsertNeverBlanksExistingContent covers the rule that a later, emptier
// feed revision must not erase text the reader already has.
func TestUpsertNeverBlanksExistingContent(t *testing.T) {
	db := newTestDB(t)
	source := addSource(t, db, "Feed", "https://a.example/f.xml", "paper")
	id, _, err := db.UpsertEntry(UpsertEntryInput{
		SourceID: source, GUID: "g", URL: "https://e.org/p", Title: "T",
		Content: "Full body", ContentMarkdown: "Full **body**",
		Summary: "A summary", SummaryProvider: "extractive",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, _, err := db.UpsertEntry(UpsertEntryInput{
		SourceID: source, GUID: "g", URL: "https://e.org/p", Title: "T2",
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	entry, _ := db.GetEntry(id)
	if entry.Content != "Full body" {
		t.Errorf("content is %q, want it preserved", entry.Content)
	}
	if entry.ContentMarkdown != "Full **body**" {
		t.Errorf("content_markdown is %q, want it preserved", entry.ContentMarkdown)
	}
	if entry.Summary != "A summary" {
		t.Errorf("summary is %q, want it preserved", entry.Summary)
	}
}

// TestCursorRoundTrip pins the opaque pagination cursor, which is public API.
func TestCursorRoundTrip(t *testing.T) {
	cursor := EncodeCursor("2026-08-06T10:00:00+00:00", 4242)
	sortAt, entryID, err := DecodeCursor(cursor)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if sortAt != "2026-08-06T10:00:00+00:00" || entryID != 4242 {
		t.Errorf("round trip gave (%q, %d)", sortAt, entryID)
	}
	// The encoding is unpadded URL-safe base64 of a compact JSON pair, so
	// cursors issued by earlier versions still decode.
	if _, _, err := DecodeCursor("WyIyMDI2LTA4LTA2VDEwOjAwOjAwKzAwOjAwIiw0MjQyXQ"); err != nil {
		t.Errorf("a previously issued cursor no longer decodes: %v", err)
	}
	for _, bad := range []string{"not-base64!!", "", "eyJhIjoxfQ", "WyJhIl0", "WyJhIiwxLjVd"} {
		if _, _, err := DecodeCursor(bad); err == nil {
			t.Errorf("DecodeCursor(%q) accepted a malformed cursor", bad)
		}
	}
}

// TestListEntriesPageHasNoHiddenCeiling covers the documented promise that
// paging is bounded only by the requested limit.
func TestListEntriesPageHasNoHiddenCeiling(t *testing.T) {
	db := newTestDB(t)
	source := addSource(t, db, "Feed", "https://a.example/f.xml", "paper")
	const total = 250
	for index := range total {
		published := "2026-08-" + twoDigits(1+index%28) + "T10:00:00+00:00"
		if _, _, err := db.UpsertEntry(UpsertEntryInput{
			SourceID: source, GUID: "g" + twoDigits(index),
			URL:   "https://e.org/" + twoDigits(index),
			Title: "Paper " + twoDigits(index), PublishedAt: &published,
		}); err != nil {
			t.Fatalf("upsert %d: %v", index, err)
		}
	}
	seen := map[int64]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		page, err := db.ListEntriesPage(EntryFilter{Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, entry := range page.Entries {
			if seen[entry.ID] {
				t.Errorf("entry %d appeared on more than one page", entry.ID)
			}
			seen[entry.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(seen) != total {
		t.Errorf("paged through %d entries, want %d", len(seen), total)
	}
}

// TestEntryCountsCoverWholeDatabase covers /api/stats reporting on everything,
// not just the visible page.
func TestEntryCountsCoverWholeDatabase(t *testing.T) {
	db := newTestDB(t)
	papers := addSource(t, db, "Papers", "https://a.example/f.xml", "paper")
	news := addSource(t, db, "News", "https://b.example/f.xml", "news")
	for index, spec := range []struct {
		source int64
		status string
	}{
		{papers, "unread"}, {papers, "kept"}, {papers, "read"},
		{news, "unread"}, {news, "discarded"},
	} {
		id, _, err := db.UpsertEntry(UpsertEntryInput{
			SourceID: spec.source, GUID: "g" + twoDigits(index),
			URL: "https://e.org/" + twoDigits(index), Title: "T",
		})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if _, err := db.SetStatus(id, spec.status); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
	}
	counts, err := db.EntryCounts()
	if err != nil {
		t.Fatalf("EntryCounts: %v", err)
	}
	if counts.Total != 5 {
		t.Errorf("total is %d, want 5", counts.Total)
	}
	if counts.Statuses["unread"] != 2 || counts.Statuses["kept"] != 1 ||
		counts.Statuses["read"] != 1 || counts.Statuses["discarded"] != 1 {
		t.Errorf("statuses are %v", counts.Statuses)
	}
	if counts.ContentKinds["paper"] != 3 || counts.ContentKinds["news"] != 2 {
		t.Errorf("content kinds are %v", counts.ContentKinds)
	}
	if counts.Workspaces["paper"].Total != 3 || counts.Workspaces["news"].Statuses["discarded"] != 1 {
		t.Errorf("workspaces are %+v", counts.Workspaces)
	}
	// Nothing is scored yet, so everything is pending.
	if counts.Relevance["paper"]["pending"] != 3 {
		t.Errorf("relevance is %v, want all papers pending", counts.Relevance)
	}
}

func twoDigits(value int) string {
	digits := []byte{byte('0' + value/100%10), byte('0' + value/10%10), byte('0' + value%10)}
	return string(digits)
}
