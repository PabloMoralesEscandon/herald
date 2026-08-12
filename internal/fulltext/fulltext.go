// Package fulltext turns an article's own source — an open-access HTML page or
// a PDF — into the Obsidian-compatible Markdown Herald writes into a note.
//
// Two rules shape it.
//
// The first is where text may come from. Herald reads an article's full text
// only from a location the publisher has made openly available (an arXiv HTML
// or PDF rendering, a provider-declared open-access PDF, a page's own declared
// PDF link) or from a file the user supplied themselves. It follows no paywall,
// sends no credentials, and treats "no open copy exists" as a normal outcome
// that asks the user for their copy rather than something to work around.
//
// The second is how citations survive. An extracted bibliography is not stored
// as prose: every in-text marker becomes a placeholder naming a reference key,
// and the note renderer turns that placeholder into a real Obsidian link when
// the cited paper is itself in the vault. That is what makes the vault a
// navigable citation graph rather than a pile of documents.
package fulltext

import (
	"regexp"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
)

// Format identifies how a document was extracted.
type Format string

const (
	FormatHTML Format = "html"
	FormatPDF  Format = "pdf"
)

// BlockKind is the role of one piece of a document.
type BlockKind string

const (
	BlockHeading   BlockKind = "heading"
	BlockParagraph BlockKind = "paragraph"
	BlockListItem  BlockKind = "list"
	BlockQuote     BlockKind = "quote"
	BlockCode      BlockKind = "code"
	BlockCaption   BlockKind = "caption"
	BlockMath      BlockKind = "math"
	BlockTable     BlockKind = "table"
)

// Block is one rendered element of the article body.
type Block struct {
	Kind  BlockKind
	Level int    // heading depth, 1 to 6
	Text  string // already Markdown-inline, including citation placeholders
}

// Reference is one entry parsed out of the article's bibliography.
type Reference struct {
	// Key is the stable identity used to merge this entry with the citation
	// edges providers report, and to address it from a placeholder.
	Key      string
	Label    string // "12", or "Smith et al., 2020"
	Raw      string
	Title    string
	Authors  string
	Year     string
	DOI      string
	ArxivID  string
	URL      string
	Position int
}

// Document is an extracted article.
type Document struct {
	Title      string
	Abstract   string
	Format     Format
	Blocks     []Block
	References []Reference
	// Truncated reports that the article was longer than Herald's limits and
	// the tail was dropped, so the note can say so rather than appearing to be
	// a complete article that simply ends.
	Truncated bool
}

// Limits bound one extraction. They exist because both inputs are untrusted
// and because a note is a file in the user's vault, not a database row.
const (
	MaxBlocks         = 12_000
	MaxBlockRunes     = 20_000
	MaxReferences     = 2_000
	MaxMarkdownRunes  = 1_200_000
	MinUsefulTextRuns = 500
)

// citePlaceholderPattern matches the marker Herald embeds for one citation.
//
// The stored Markdown carries placeholders rather than finished links because
// whether a link can be made depends on state that changes later: a cited
// paper that is kept next week must turn every marker citing it into a real
// link, without re-extracting anything.
var citePlaceholderPattern = regexp.MustCompile(`\{\{herald:cite:([^|{}]*)\|([^{}]*)\}\}`)

// CitePlaceholder builds the marker for one citation.
func CitePlaceholder(key, display string) string {
	key = sanitizePlaceholderField(key)
	display = sanitizePlaceholderField(display)
	if key == "" || display == "" {
		return display
	}
	return "{{herald:cite:" + key + "|" + display + "}}"
}

// sanitizePlaceholderField removes the characters that delimit a placeholder,
// so no key or label can terminate one early.
func sanitizePlaceholderField(value string) string {
	replacer := strings.NewReplacer("{", "", "}", "", "|", " ", "\n", " ", "\r", " ")
	return textx.CollapseStrip(replacer.Replace(value))
}

// ResolveCitations replaces every citation placeholder in a document.
//
// resolve receives the reference key and the text the article printed, and
// returns what belongs in the note. Every placeholder is replaced, including
// ones resolve cannot identify, because a placeholder left behind would end up
// visible in a file the user owns.
func ResolveCitations(markdown string, resolve func(key, display string) string) string {
	return citePlaceholderPattern.ReplaceAllStringFunc(markdown, func(match string) string {
		groups := citePlaceholderPattern.FindStringSubmatch(match)
		if len(groups) != 3 {
			return ""
		}
		key, display := groups[1], groups[2]
		if resolve == nil {
			return display
		}
		replacement := resolve(key, display)
		if replacement == "" {
			return display
		}
		return replacement
	})
}

// StripCitations reduces a document to the text an article printed, dropping
// every placeholder. It is used for previews and for search indexing, where a
// link would be noise.
func StripCitations(markdown string) string {
	return ResolveCitations(markdown, nil)
}

// RemapCitationKeys rewrites the reference keys a document's placeholders use.
//
// It is needed when a bibliography entry turns out to describe a work Herald
// already has a citation edge for under a different key: the stored text must
// point at the key that survived the merge, or its markers would never resolve.
func RemapCitationKeys(markdown string, mapping map[string]string) string {
	if len(mapping) == 0 {
		return markdown
	}
	return citePlaceholderPattern.ReplaceAllStringFunc(markdown, func(match string) string {
		groups := citePlaceholderPattern.FindStringSubmatch(match)
		if len(groups) != 3 {
			return match
		}
		replacement, ok := mapping[groups[1]]
		if !ok || replacement == "" {
			return match
		}
		return CitePlaceholder(replacement, groups[2])
	})
}

// CitationKeys lists the reference keys a document's Markdown cites, in first
// appearance order.
func CitationKeys(markdown string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, match := range citePlaceholderPattern.FindAllStringSubmatch(markdown, -1) {
		key := match[1]
		if key != "" && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

// Markdown renders a document's body.
//
// The bibliography is deliberately not rendered here: Herald already owns a
// References section built from every citation edge it knows about, whether it
// learned of them from a provider or from this document, and rendering the
// article's own list again would duplicate it.
func (d *Document) Markdown() string {
	var sections []string
	var listRun bool

	for _, block := range d.Blocks {
		text := strings.TrimRight(block.Text, " \t")
		if text == "" {
			continue
		}
		if block.Kind != BlockListItem {
			listRun = false
		}
		switch block.Kind {
		case BlockHeading:
			level := min(max(block.Level, 1), 6)
			// Headings start at level three so the article's own structure
			// nests under the note's "Full text" section.
			sections = append(sections, strings.Repeat("#", min(level+2, 6))+" "+text)
		case BlockListItem:
			item := "- " + text
			if listRun && len(sections) > 0 {
				sections[len(sections)-1] += "\n" + item
			} else {
				sections = append(sections, item)
			}
			listRun = true
		case BlockQuote:
			sections = append(sections, "> "+strings.ReplaceAll(text, "\n", "\n> "))
		case BlockCode:
			sections = append(sections, "```\n"+text+"\n```")
		case BlockCaption:
			sections = append(sections, "*"+text+"*")
		case BlockMath:
			sections = append(sections, "$$\n"+text+"\n$$")
		case BlockTable:
			sections = append(sections, text)
		default:
			sections = append(sections, text)
		}
	}
	if d.Truncated {
		sections = append(sections,
			"*Herald stopped extracting here because the article exceeded its size limit.*")
	}

	out := strings.Join(sections, "\n\n")
	if len([]rune(out)) > MaxMarkdownRunes {
		runes := []rune(out)[:MaxMarkdownRunes]
		out = string(runes) + "\n\n*Herald truncated the remaining text.*"
	}
	return out
}

// TextLength reports the extracted body length, ignoring citation markup. It
// is how callers judge whether an extraction is worth keeping.
func (d *Document) TextLength() int {
	total := 0
	for _, block := range d.Blocks {
		total += len([]rune(StripCitations(block.Text)))
	}
	return total
}

// appendBlock adds a block, enforcing the per-document limits.
func (d *Document) appendBlock(kind BlockKind, level int, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(d.Blocks) >= MaxBlocks {
		d.Truncated = true
		return
	}
	if runes := []rune(text); len(runes) > MaxBlockRunes {
		text = string(runes[:MaxBlockRunes])
		d.Truncated = true
	}
	d.Blocks = append(d.Blocks, Block{Kind: kind, Level: level, Text: text})
}
