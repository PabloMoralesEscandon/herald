package markdown

import (
	"encoding/json"
	"os"
	"testing"
)

// TestMatchesReferenceImplementation pins the converter against output captured
// from Herald's original implementation. Obsidian notes are user-owned files,
// so a silent change in conversion would rewrite text people have already read
// and annotated; these goldens make any such change explicit.
func TestMatchesReferenceImplementation(t *testing.T) {
	var cases [][]string
	readJSON(t, "testdata/cases.json", &cases)
	var golden []string
	readJSON(t, "testdata/golden.json", &golden)

	if len(cases) != len(golden) {
		t.Fatalf("corpus and golden differ in length: %d vs %d", len(cases), len(golden))
	}
	for index, testCase := range cases {
		value, baseURL := testCase[0], testCase[1]
		if got := ToMarkdown(value, baseURL); got != golden[index] {
			t.Errorf("case %d input %q base %q:\n got: %q\nwant: %q",
				index, value, baseURL, got, golden[index])
		}
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

// TestDocumentedBehaviour restates the guarantees README.md makes, so they
// survive independently of the golden corpus.
func TestDocumentedBehaviour(t *testing.T) {
	tests := []struct {
		name, input, baseURL, want string
	}{
		{
			name:  "existing markdown passes through unchanged",
			input: "A **curated** paragraph.\n\n- One\n- Two",
			want:  "A **curated** paragraph.\n\n- One\n- Two",
		},
		{
			name:  "omitted closing tags still nest correctly",
			input: "<ul><li>One<li>Two<ul><li>Nested<li>Again</ul><li>Three</ul>",
			want:  "- One\n- Two\n  - Nested\n  - Again\n- Three",
		},
		{
			name:  "unsafe link schemes lose the href but keep the label",
			input: `<a href="javascript:alert(1)">Open</a>`,
			want:  "Open",
		},
		{
			name: "tables render with a header separator",
			input: "<table><tr><th>Name</th><th>Value</th></tr>" +
				"<tr><td>Latency</td><td>12 ms</td></tr></table>",
			want: "| Name | Value |\n| --- | --- |\n| Latency | 12 ms |",
		},
		{
			name:    "relative links resolve against the article URL",
			input:   `<p>Read <a href="/background">the background</a>.</p>`,
			baseURL: "https://example.org/news/item",
			want:    "Read [the background](<https://example.org/background>).",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ToMarkdown(test.input, test.baseURL); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

// TestActiveContentIsRemoved covers the security-relevant half of the contract.
func TestActiveContentIsRemoved(t *testing.T) {
	converted := ToMarkdown(
		`<p>Safe</p><script>alert("unsafe")</script><style>.x{}</style>`+
			`<iframe src="https://evil.example"></iframe><svg><path/></svg>`,
		"https://example.org/article",
	)
	for _, forbidden := range []string{"alert", "evil.example", ".x{", "<path"} {
		if contains(converted, forbidden) {
			t.Errorf("converted output leaked %q: %q", forbidden, converted)
		}
	}
	if converted != "Safe" {
		t.Errorf("got %q, want %q", converted, "Safe")
	}
}

func contains(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
