package sources

import (
	"encoding/json"
	"testing"
)

// TestCatalogLoads guards the embedded catalog: it must be present in the
// binary, non-empty, and pass the same validation an imported file does.
func TestCatalogLoads(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(catalog) != 48 {
		t.Errorf("catalog has %d sources, want 48", len(catalog))
	}
	papers, news := 0, 0
	for _, source := range catalog {
		switch source.ContentKind {
		case "paper":
			papers++
		case "news":
			news++
		}
		if source.Title == "" || source.URL == "" || source.Category == "" {
			t.Errorf("catalog entry is incomplete: %+v", source)
		}
	}
	if papers != 11 || news != 37 {
		t.Errorf("catalog split is %d papers / %d news, want 11 / 37", papers, news)
	}
}

// TestManifestRejectsUnsafeDocuments pins the validation contract. An import
// merges directly into the subscription list, so each of these must be refused
// before anything is applied.
func TestManifestRejectsUnsafeDocuments(t *testing.T) {
	tests := []struct {
		name     string
		document string
	}{
		{"not an object", `[]`},
		{"wrong format", `{"format":"other","version":1,"sources":[]}`},
		{"missing format", `{"version":1,"sources":[]}`},
		{"unsupported version", `{"format":"herald.sources","version":2,"sources":[]}`},
		{"sources not an array", `{"format":"herald.sources","version":1,"sources":{}}`},
		{
			name: "unsupported field",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"A","url":"https://e.org/f","category":"C","content_kind":"news","enabled":true,"secret":"x"}]}`,
		},
		{
			name: "empty title",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"  ","url":"https://e.org/f","category":"C","content_kind":"news"}]}`,
		},
		{
			name: "non-web scheme",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"A","url":"file:///etc/passwd","category":"C","content_kind":"news"}]}`,
		},
		{
			name: "credentials in url",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"A","url":"https://user:pw@e.org/f","category":"C","content_kind":"news"}]}`,
		},
		{
			name: "non-standard port",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"A","url":"https://e.org:8080/f","category":"C","content_kind":"news"}]}`,
		},
		{
			name: "invalid port",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"A","url":"https://e.org:bad/f","category":"C","content_kind":"news"}]}`,
		},
		{
			name: "bad content kind",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"A","url":"https://e.org/f","category":"C","content_kind":"video"}]}`,
		},
		{
			name: "enabled not boolean",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"A","url":"https://e.org/f","category":"C","content_kind":"news","enabled":"yes"}]}`,
		},
		{
			name: "duplicate url after canonicalization",
			document: `{"format":"herald.sources","version":1,"sources":[
				{"title":"A","url":"https://e.org/f","category":"C","content_kind":"news"},
				{"title":"B","url":"HTTPS://E.ORG:443/f","category":"C","content_kind":"news"}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ValidateManifest([]byte(test.document)); err == nil {
				t.Fatal("ValidateManifest accepted an unsafe document")
			}
		})
	}
}

// TestManifestAcceptsValidDocument covers the happy path, including the
// documented default for a missing "enabled" field.
func TestManifestAcceptsValidDocument(t *testing.T) {
	document := `{"format":"herald.sources","version":1,"sources":[
		{"title":" Spaced ","url":"HTTPS://Example.ORG/feed?utm_source=x&id=1","category":"Chips","content_kind":"paper"},
		{"title":"B","url":"http://e.org:80/f","category":"News","content_kind":"news","enabled":false}]}`
	parsed, err := ValidateManifest([]byte(document))
	if err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("got %d sources, want 2", len(parsed))
	}
	if parsed[0].Title != "Spaced" {
		t.Errorf("title %q was not trimmed", parsed[0].Title)
	}
	if parsed[0].URL != "https://example.org/feed?id=1" {
		t.Errorf("url %q was not canonicalized", parsed[0].URL)
	}
	if !parsed[0].Enabled {
		t.Error("a missing enabled field must default to true")
	}
	if parsed[1].Enabled {
		t.Error("an explicit enabled=false must be preserved")
	}
}

// TestCreateManifestIsStableAndPortable checks that an export is deterministic
// and carries no personal data.
func TestCreateManifestIsStableAndPortable(t *testing.T) {
	rows := []Source{
		{Title: "zeta", URL: "https://e.org/z", Category: "Beta", ContentKind: "news", Enabled: true},
		{Title: "Alpha", URL: "https://e.org/a", Category: "beta", ContentKind: "news", Enabled: false},
		{Title: "Paper", URL: "https://e.org/p", Category: "Alpha", ContentKind: "paper", Enabled: true},
	}
	manifest, err := CreateManifest(rows)
	if err != nil {
		t.Fatalf("CreateManifest: %v", err)
	}
	// Ordering is by content kind first, and "news" sorts before "paper".
	// Within a kind it is category then title, both case-insensitively.
	wantOrder := []string{"Alpha", "zeta", "Paper"}
	for index, want := range wantOrder {
		if manifest.Sources[index].Title != want {
			t.Errorf("position %d is %q, want %q", index, manifest.Sources[index].Title, want)
		}
	}

	document, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe []map[string]any
	var envelope struct {
		Sources []map[string]any `json:"sources"`
	}
	if err := json.Unmarshal(document, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	probe = envelope.Sources
	for _, source := range probe {
		for field := range source {
			if !allowedFields[field] {
				t.Errorf("export leaked non-portable field %q", field)
			}
		}
	}

	// A second export of the same rows must be byte-identical.
	again, err := CreateManifest(rows)
	if err != nil {
		t.Fatalf("CreateManifest: %v", err)
	}
	repeat, _ := json.Marshal(again)
	if string(document) != string(repeat) {
		t.Error("repeated exports are not byte-identical")
	}
}
