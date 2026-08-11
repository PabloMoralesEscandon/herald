package paper

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/store"
)

func newImporter(t *testing.T, responses map[string]string) *Importer {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "herald.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	importer := NewImporter(db)
	importer.RequestDelay = 0
	importer.Sleep = func(time.Duration) {}
	importer.Fetcher = func(url string, _ map[string]string, _ int, _ time.Duration) ([]byte, error) {
		for prefix, body := range responses {
			if strings.HasPrefix(url, prefix) {
				return []byte(body), nil
			}
		}
		return nil, &NotFoundError{Reason: "Paper metadata was not found"}
	}
	return importer
}

const semanticScholarResponse = `{
	"paperId":"0123456789abcdef0123456789abcdef01234567",
	"externalIds":{"DOI":"10.1145/Example","ArXiv":"2301.00001v2"},
	"url":"https://www.semanticscholar.org/paper/0123456789abcdef0123456789abcdef01234567",
	"title":"A Low-Latency Chiplet Interconnect",
	"abstract":"We present a chiplet interconnect that reduces latency.",
	"authors":[{"name":"Ada Researcher"},{"name":"Lin Scientist"}],
	"publicationDate":"2026-08-06","year":2026,
	"fieldsOfStudy":["Computer Science","Engineering"],
	"references":[
		{"paperId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","externalIds":{"DOI":"10.1000/cited-one"},"title":"Cited One","url":"https://doi.org/10.1000/cited-one"},
		{"externalIds":{"ArXiv":"2101.00002"},"title":"Cited Two"},
		{"title":"No identifier at all"}
	]}`

// TestImportStoresIdentifiersKeywordsAndReferences covers the documented
// contract of POST /api/import/paper.
func TestImportStoresIdentifiersKeywordsAndReferences(t *testing.T) {
	importer := newImporter(t, map[string]string{
		semanticScholarAPI: semanticScholarResponse,
	})
	result, err := importer.Import("10.1145/example", false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !result.Created {
		t.Error("a first import must report created")
	}
	// A newly imported paper is unread and is not kept or exported.
	if result.Entry.Status != "unread" {
		t.Errorf("status is %q, want unread", result.Entry.Status)
	}
	if result.Entry.ExportedPath != nil {
		t.Error("a manual import must not be exported")
	}
	if result.Entry.CanonicalURL != "https://doi.org/10.1145/example" {
		t.Errorf("canonical url is %q", result.Entry.CanonicalURL)
	}
	if result.Entry.EnrichmentStatus != "enriched" {
		t.Errorf("enrichment status is %q, want enriched", result.Entry.EnrichmentStatus)
	}
	if result.Entry.SourceTitle != "Manual Imports" {
		t.Errorf("source is %q, want Manual Imports", result.Entry.SourceTitle)
	}

	schemes := map[string]string{}
	primary := ""
	for _, identifier := range result.Identifiers {
		schemes[identifier.Scheme] = identifier.Value
		if identifier.IsPrimary == 1 {
			primary = identifier.Scheme
		}
	}
	if schemes["doi"] != "10.1145/example" {
		t.Errorf("doi is %q, want it normalized to lower case", schemes["doi"])
	}
	if schemes["arxiv"] != "2301.00001" {
		t.Errorf("arxiv is %q, want the version suffix stripped", schemes["arxiv"])
	}
	if primary != "doi" {
		t.Errorf("primary identifier is %q, want doi", primary)
	}

	// Only references carrying an identifier or URL are stored.
	if len(result.References) != 2 {
		t.Fatalf("stored %d references, want 2", len(result.References))
	}
	if result.References[0].ExternalID != "10.1000/cited-one" {
		t.Errorf("first reference is %+v", result.References[0])
	}
	// An arXiv reference without a URL gets a derived one.
	if result.References[1].CitedURL != "https://arxiv.org/abs/2101.00002" {
		t.Errorf("second reference url is %q", result.References[1].CitedURL)
	}

	var topics, keywords int
	for _, keyword := range result.Keywords {
		switch keyword.Kind {
		case "topic":
			topics++
		case "keyword":
			keywords++
		}
	}
	if topics != 2 {
		t.Errorf("stored %d topics, want the 2 fields of study", topics)
	}
	if keywords == 0 {
		t.Error("no keywords were extracted")
	}
}

// TestRepeatImportPreservesTriage covers the promise that re-importing returns
// the existing entry without changing its status.
func TestRepeatImportPreservesTriage(t *testing.T) {
	importer := newImporter(t, map[string]string{
		semanticScholarAPI: semanticScholarResponse,
	})
	first, err := importer.Import("10.1145/example", false)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := importer.DB.SetStatus(first.Entry.ID, "kept"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	second, err := importer.Import("https://doi.org/10.1145/EXAMPLE", false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Created {
		t.Error("a repeat import reported created")
	}
	if second.Entry.ID != first.Entry.ID {
		t.Errorf("repeat import created entry %d, want %d", second.Entry.ID, first.Entry.ID)
	}
	if second.Entry.Status != "kept" {
		t.Errorf("status is %q, want the keep preserved", second.Entry.Status)
	}
}

// TestCrossrefIsUsedWhenSemanticScholarHasNoRecord covers the free fallback.
func TestCrossrefIsUsedWhenSemanticScholarHasNoRecord(t *testing.T) {
	importer := newImporter(t, map[string]string{
		crossrefAPI: `{"message":{
			"title":["A Crossref Only Paper"],
			"author":[{"given":"Ada","family":"Researcher"}],
			"published":{"date-parts":[[2026,8,6]]},
			"subject":["Hardware"],
			"URL":"https://doi.org/10.1000/only-crossref",
			"abstract":"<jats:p>Abstract with <jats:italic>markup</jats:italic>.</jats:p>",
			"reference":[{"DOI":"10.1000/cited","article-title":"Cited"}]}}`,
	})
	result, err := importer.Import("10.1000/only-crossref", false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.Entry.Title != "A Crossref Only Paper" {
		t.Errorf("title is %q", result.Entry.Title)
	}
	if result.Entry.Author != "Ada Researcher" {
		t.Errorf("author is %q", result.Entry.Author)
	}
	// Markup in a Crossref abstract is stripped rather than stored raw.
	if strings.Contains(result.Entry.Content, "<") {
		t.Errorf("abstract retained markup: %q", result.Entry.Content)
	}
	if result.Entry.PublishedAt == nil || !strings.HasPrefix(*result.Entry.PublishedAt, "2026-08-06") {
		t.Errorf("published_at is %v", result.Entry.PublishedAt)
	}
}

// TestPageMetadataDiscoversDOIThenMergesKeywords covers importing a public
// paper page: the page's own DOI drives the provider lookup, and the author's
// keywords survive the merge.
func TestPageMetadataDiscoversDOIThenMergesKeywords(t *testing.T) {
	importer := newImporter(t, map[string]string{
		"https://journal.example/article": `<html><head>
			<title>Fallback Title</title>
			<meta name="citation_doi" content="10.1145/Example">
			<meta name="citation_title" content="Page Supplied Title">
			<meta name="citation_author" content="Page Author">
			<meta name="citation_keywords" content="chiplets, interconnects; latency">
			<link rel="canonical" href="https://journal.example/article/v2">
			</head><body></body></html>`,
		semanticScholarAPI: semanticScholarResponse,
	})
	result, err := importer.Import("https://journal.example/article", false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	// The provider record wins for the title, having been located via the
	// DOI the page declared.
	if result.Entry.Title != "A Low-Latency Chiplet Interconnect" {
		t.Errorf("title is %q, want the provider's", result.Entry.Title)
	}
	var supplied []string
	for _, keyword := range result.Keywords {
		if keyword.Provider == "page-meta" {
			supplied = append(supplied, keyword.Keyword)
		}
	}
	if len(supplied) != 3 {
		t.Errorf("author keywords are %v, want the three declared", supplied)
	}
}

// TestUnsafeURLsAreRejectedWithoutFetching covers the SSRF boundary. None of
// these may reach the fetcher at all.
func TestUnsafeURLsAreRejectedWithoutFetching(t *testing.T) {
	fetched := false
	importer := newImporter(t, nil)
	importer.Fetcher = func(string, map[string]string, int, time.Duration) ([]byte, error) {
		fetched = true
		return nil, nil
	}
	for _, unsafe := range []string{
		"http://example.org/paper",
		"https://user:pw@example.org/paper",
		"https://example.org:8443/paper",
		"https://127.0.0.1/paper",
		"https://10.0.0.1/paper",
		"https://192.168.1.1/paper",
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]/paper",
		"file:///etc/passwd",
		"javascript:alert(1)",
	} {
		if _, err := importer.Import(unsafe, false); err == nil {
			t.Errorf("Import(%q) was accepted", unsafe)
		}
		if fetched {
			t.Fatalf("Import(%q) reached the network before validation", unsafe)
		}
	}
}

// TestProviderFailureLeavesDatabaseUnchanged covers the promise that a failed
// import writes nothing.
func TestProviderFailureLeavesDatabaseUnchanged(t *testing.T) {
	importer := newImporter(t, nil)
	attempts := 0
	importer.Fetcher = func(string, map[string]string, int, time.Duration) ([]byte, error) {
		attempts++
		return nil, &FetchError{Reason: "Metadata provider returned HTTP 503", Transient: true}
	}
	if _, err := importer.Import("10.1145/example", false); err == nil {
		t.Fatal("Import succeeded despite a provider failure")
	}
	// A transient failure is retried before giving up, and a DOI is then
	// tried against Crossref, so both providers exhaust their retries.
	wantAttempts := 2 * (importer.Retries + 1)
	if attempts != wantAttempts {
		t.Errorf("made %d attempts, want %d (both providers, with retries)", attempts, wantAttempts)
	}
	entries, err := importer.DB.ListEntries(store.EntryFilter{})
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed import wrote %d entries", len(entries))
	}
}

// TestProviderResponseIsCached covers the local cache that keeps repeat
// imports free and Herald polite.
func TestProviderResponseIsCached(t *testing.T) {
	importer := newImporter(t, map[string]string{
		semanticScholarAPI: semanticScholarResponse,
	})
	requests := 0
	inner := importer.Fetcher
	importer.Fetcher = func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
		requests++
		return inner(url, headers, maxBytes, timeout)
	}
	if _, err := importer.Import("10.1145/example", false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	before := requests
	if _, err := importer.Import("10.1145/example", false); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if requests != before {
		t.Errorf("the second import made %d extra requests, want 0", requests-before)
	}

	cached, err := importer.DB.GetProviderCache(
		"semantic-scholar", "doi:10.1145/example:references-v1", time.Hour)
	if err != nil || cached == nil {
		t.Fatalf("provider cache miss: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(cached, &payload); err != nil {
		t.Fatalf("cached payload is not an object: %v", err)
	}
}
