package feed

import (
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
	"golang.org/x/net/html"
)

// breakOnOpen and breakOnClose are the tags that contribute a line break when
// reducing feed markup to plain text.
var (
	breakOnOpen  = map[string]bool{"br": true, "p": true, "div": true, "li": true, "h1": true, "h2": true, "h3": true, "tr": true}
	breakOnClose = map[string]bool{"p": true, "div": true, "li": true, "h1": true, "h2": true, "h3": true, "tr": true}
)

// extractText flattens markup into the plain-text projection Herald stores as
// entries.content and feeds to the summarizer.
//
// This is intentionally not the Markdown converter's text fallback: the pieces
// are joined with spaces (including the break markers), which is what keeps
// punctuation separated the way the stored content has always been. It also
// does not drop script or style bodies, matching the established behaviour.
func extractText(value string) string {
	var parts []string
	tokenizer := html.NewTokenizer(strings.NewReader(value))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return strings.Join(parts, " ")
		case html.TextToken:
			parts = append(parts, string(tokenizer.Text()))
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			if breakOnOpen[string(name)] {
				parts = append(parts, "\n")
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			if breakOnClose[string(name)] {
				parts = append(parts, "\n")
			}
		}
	}
}

// plainText reduces feed markup to a single normalized line of text.
func plainText(value string) string {
	if value == "" {
		return ""
	}
	if strings.Contains(value, "<") && strings.Contains(value, ">") {
		value = extractText(value)
	}
	return textx.CollapseStrip(value)
}
