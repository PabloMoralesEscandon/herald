package feed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// goldenEntry mirrors the reference implementation's serialized FeedEntry.
type goldenEntry struct {
	Author          string `json:"author"`
	Content         string `json:"content"`
	ContentMarkdown string `json:"content_markdown"`
	GUID            string `json:"guid"`
	PublishedAt     any    `json:"published_at"`
	Title           string `json:"title"`
	URL             string `json:"url"`
}

// TestParseMatchesReference checks every fixture against output captured from
// the reference implementation. Feed parsing decides entry identity (guid and
// canonical URL), so a divergence here would duplicate or lose articles that
// users have already triaged.
func TestParseMatchesReference(t *testing.T) {
	document, err := os.ReadFile("testdata/feeds_golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(document, &golden); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	if len(golden) == 0 {
		t.Fatal("golden file is empty")
	}

	for name, raw := range golden {
		t.Run(name, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("testdata/feeds", name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			// A fixture the reference implementation rejected must be
			// rejected here too, or Herald would ingest entries the
			// original refused.
			var failure struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(raw, &failure) == nil && failure.Error != "" {
				if _, err := Parse(source); err == nil {
					t.Fatalf("Parse succeeded, want failure %s", failure.Error)
				}
				return
			}
			var want []goldenEntry
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("decode golden entries: %v", err)
			}
			got, err := Parse(source)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("got %d entries, want %d", len(got), len(want))
			}
			for index := range want {
				compareEntry(t, index, got[index], want[index])
			}
		})
	}
}

func compareEntry(t *testing.T, index int, got Entry, want goldenEntry) {
	t.Helper()
	published := ""
	if want.PublishedAt != nil {
		published, _ = want.PublishedAt.(string)
	}
	for _, field := range []struct{ name, got, want string }{
		{"guid", got.GUID, want.GUID},
		{"url", got.URL, want.URL},
		{"title", got.Title, want.Title},
		{"author", got.Author, want.Author},
		{"published_at", got.PublishedAt, published},
		{"content", got.Content, want.Content},
		{"content_markdown", got.ContentMarkdown, want.ContentMarkdown},
	} {
		if field.got != field.want {
			t.Errorf("entry %d %s:\n got: %q\nwant: %q", index, field.name, field.got, field.want)
		}
	}
}

// TestCanonicalizeMatchesReference pins URL canonicalization, which is the
// cross-feed deduplication key stored in the database.
func TestCanonicalizeMatchesReference(t *testing.T) {
	var cases []string
	readJSON(t, "testdata/canonical_cases.json", &cases)
	var golden []string
	readJSON(t, "testdata/canonical_golden.json", &golden)

	for index, input := range cases {
		got, err := CanonicalizeURL(input)
		if err != nil {
			got = "!!ERR:ValueError"
		}
		if got != golden[index] {
			t.Errorf("case %d %q:\n got: %q\nwant: %q", index, input, got, golden[index])
		}
	}
}

// TestRejectsUnsupportedDocuments covers the documented parse failures.
func TestRejectsUnsupportedDocuments(t *testing.T) {
	for _, document := range []string{"<rss>", "<html></html>", "", "not xml at all"} {
		if _, err := Parse([]byte(document)); err == nil {
			t.Errorf("Parse(%q) succeeded, want a parse error", document)
		} else if !IsParseError(err) {
			t.Errorf("Parse(%q) returned %T, want a *ParseError", document, err)
		}
	}
}

// TestDiscoverFeedURL covers autodiscovery, including the guarantee that
// Herald reads only standard link metadata and never scrapes page links.
func TestDiscoverFeedURL(t *testing.T) {
	tests := []struct {
		name, document, pageURL, want string
	}{
		{
			name: "relative atom link resolves against the page",
			document: `<html><head>
              <link rel="alternate" type="application/atom+xml" href="/updates.atom">
            </head><body><a href="/not-a-feed">News</a></body></html>`,
			pageURL: "https://example.com/news",
			want:    "https://example.com/updates.atom",
		},
		{
			name:     "anchors are never treated as feeds",
			document: `<html><body><a href="/feed.xml">Feed</a></body></html>`,
			pageURL:  "https://example.com/",
			want:     "",
		},
		{
			name:     "non-feed media types are ignored",
			document: `<link rel="alternate" type="text/html" href="/x">`,
			pageURL:  "https://example.com/",
			want:     "",
		},
		{
			name:     "rss media type is accepted",
			document: `<link rel="alternate" type="application/rss+xml" href="/f.rss">`,
			pageURL:  "https://example.com/",
			want:     "https://example.com/f.rss",
		},
		{
			name:     "media type parameters are tolerated",
			document: `<link rel="ALTERNATE" type="application/atom+xml; charset=utf-8" href="/a">`,
			pageURL:  "https://example.com/",
			want:     "https://example.com/a",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DiscoverFeedURL([]byte(test.document), test.pageURL); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(document, target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}
