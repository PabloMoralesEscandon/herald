// Package vault renders and writes Obsidian notes.
//
// Notes are user-owned files. Herald only ever rewrites the regions between its
// own managed markers, so custom frontmatter properties and personal notes
// survive every sync. A file it cannot prove it owns becomes a conflict rather
// than being overwritten.
package vault

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/fulltext"
	"github.com/PabloMoralesEscandon/herald/internal/markdown"
	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// Managed-region markers. Their exact spelling is part of the note format.
const (
	FrontmatterStart = "# herald:managed:start"
	FrontmatterEnd   = "# herald:managed:end"
	BodyStart        = "<!-- herald:managed:start -->"
	BodyEnd          = "<!-- herald:managed:end -->"
)

// ConflictError reports a note Herald cannot safely update.
type ConflictError struct{ Reason string }

func (e *ConflictError) Error() string { return e.Reason }

// Note is the data a rendered note needs. It is assembled by the service from
// the entry and its related rows.
type Note struct {
	ID                 int64
	Title              string
	URL                string
	CanonicalURL       string
	CanonicalKey       string
	Author             string
	PublishedAt        string
	DiscoveredAt       string
	UpdatedAt          string
	Status             string
	Content            string
	ContentMarkdown    string
	Summary            string
	SummaryProvider    string
	SummaryModel       string
	SummaryGeneratedAt string
	ContentKind        string
	EnrichmentStatus   string
	EnrichmentProvider string
	SourceTitle        string
	SourceCategory     string

	Keywords    []string
	Topics      []string
	Identifiers []string
	References  []Reference
	Relevance   *Relevance
	FullText    *FullText

	// ObsidianRelativePath is the note's existing location, used to keep a
	// note in place once it has been written.
	ObsidianRelativePath string
}

// FullText is the article body Herald extracted, and where it came from.
type FullText struct {
	// State is the extraction state. Only "extracted" renders a body; the
	// states that need the user's help render a notice instead.
	State      string
	SourceKind string
	SourceURL  string
	Format     string
	// Markdown still carries citation placeholders. They are resolved during
	// rendering, because whether a citation can become a link depends on
	// whether the cited paper is in the vault right now.
	Markdown  string
	Truncated bool
	Error     string
}

// Reference is one rendered citation.
type Reference struct {
	// Key is the reference's stable identity, which is how a citation
	// placeholder in the article body finds the work it points at.
	Key            string
	Label          string
	Title          string
	ExternalScheme string
	ExternalID     string
	CitedURL       string
	// Internal linking requires the target to be kept and synced.
	CitedStatus       string
	CitedExportState  string
	CitedObsidianPath string
}

// Relevance is the ranking rendered into a news note.
type Relevance struct {
	Score    *float64
	Bucket   string
	Model    string
	ScoredAt string
	Reasons  []string
}

// yamlString quotes a value as a JSON string, which is valid YAML and escapes
// every character that could otherwise break the frontmatter block.
//
// This is hand-rolled rather than using json.Marshal because Go's encoder
// escapes &, <, and > as & and friends. Those characters are common in
// publisher names and categories, and escaping them would make note properties
// unreadable and differ from every note already in a user's vault.
func yamlString(value string) string {
	var out strings.Builder
	out.Grow(len(value) + 2)
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

var (
	unsafeComponent = regexp.MustCompile(`[<>:"/\\|?*#\[\]\x00-\x1f]`)
	whitespaceRun   = regexp.MustCompile(`\s+`)
	tagUnsafe       = regexp.MustCompile(`[^a-z0-9]+`)
)

// safeComponent turns arbitrary text into a safe single path component.
func safeComponent(value, fallback string, maximum int) string {
	value = unsafeComponent.ReplaceAllString(value, " ")
	value = strings.Trim(whitespaceRun.ReplaceAllString(value, " "), " .")
	runes := []rune(value)
	if len(runes) > maximum {
		runes = runes[:maximum]
	}
	value = strings.TrimRight(string(runes), " .")
	if value == "" {
		return fallback
	}
	return value
}

// StableKey is the note's filename stem.
//
// It is derived from the paper's identity rather than its title, so renaming an
// article upstream does not orphan the note the user has annotated.
func StableKey(note *Note) string {
	fallback := fmt.Sprintf("herald-%06d", note.ID)
	if canonical := strings.TrimSpace(note.CanonicalKey); canonical != "" {
		return safeComponent(canonical, fallback, 140)
	}
	return fallback
}

func tag(value string) string {
	value = strings.ReplaceAll(strings.ToLower(value), "&", " and ")
	value = strings.Trim(tagUnsafe.ReplaceAllString(value, "-"), "-")
	if value == "" {
		return "unsorted"
	}
	return value
}

func blockquote(value string) string {
	lines := strings.Split(value, "\n")
	if value == "" {
		lines = []string{""}
	}
	quoted := make([]string, len(lines))
	for index, line := range lines {
		if line == "" {
			quoted[index] = ">"
		} else {
			quoted[index] = "> " + line
		}
	}
	return strings.Join(quoted, "\n")
}

func yamlList(name string, values []string) []string {
	if len(values) == 0 {
		return []string{name + ": []"}
	}
	lines := []string{name + ":"}
	for _, value := range values {
		lines = append(lines, "  - "+yamlString(value))
	}
	return lines
}

func paperProperties(note *Note) []string {
	lines := []string{
		"enrichment_provider: " + yamlString(note.EnrichmentProvider),
	}
	state, sourceKind, sourceURL := "not_applicable", "", ""
	if note.FullText != nil {
		state = note.FullText.State
		sourceKind, sourceURL = note.FullText.SourceKind, note.FullText.SourceURL
	}
	lines = append(lines,
		"fulltext_state: "+yamlString(state),
		"fulltext_source: "+yamlString(sourceKind),
		"fulltext_url: "+yamlString(sourceURL),
	)
	lines = append(lines, yamlList("identifiers", note.Identifiers)...)
	lines = append(lines, yamlList("keywords", note.Keywords)...)
	return append(lines, yamlList("topics", note.Topics)...)
}

func newsProperties(note *Note) []string {
	score := "null"
	bucket := "pending"
	model, scoredAt := "", ""
	var reasons []string
	if note.Relevance != nil {
		if note.Relevance.Score != nil {
			score = formatFloat(*note.Relevance.Score)
		}
		if note.Relevance.Bucket != "" {
			bucket = note.Relevance.Bucket
		}
		model, scoredAt = note.Relevance.Model, note.Relevance.ScoredAt
		reasons = note.Relevance.Reasons
	}
	lines := []string{
		`type: "news"`,
		"publisher: " + yamlString(note.SourceTitle),
		"canonical_source: " + yamlString(sourceURL(note)),
		"updated_at: " + yamlString(note.UpdatedAt),
	}
	lines = append(lines, yamlList("keywords", note.Keywords)...)
	lines = append(lines, yamlList("topics", note.Topics)...)
	lines = append(lines,
		"relevance_score: "+score,
		"relevance_bucket: "+yamlString(bucket),
		"relevance_model: "+yamlString(model),
		"relevance_scored_at: "+yamlString(scoredAt),
	)
	return append(lines, yamlList("relevance_reasons", reasons)...)
}

func sourceURL(note *Note) string {
	if note.CanonicalURL != "" {
		return note.CanonicalURL
	}
	return note.URL
}

func managedFrontmatter(note *Note) string {
	contentKind := note.ContentKind
	if contentKind == "" {
		contentKind = "paper"
	}
	category := note.SourceCategory
	if category == "" {
		category = "Unsorted"
	}
	tags := []string{"herald", tag(category)}
	if contentKind != tags[0] && contentKind != tags[1] {
		tags = append(tags, contentKind)
	}

	lines := []string{
		FrontmatterStart,
		fmt.Sprintf("herald_id: %d", note.ID),
		"herald_key: " + yamlString(StableKey(note)),
		"title: " + yamlString(note.Title),
		"source: " + yamlString(sourceURL(note)),
		"source_title: " + yamlString(note.SourceTitle),
		"category: " + yamlString(note.SourceCategory),
		"content_kind: " + yamlString(contentKind),
		"author: " + yamlString(note.Author),
		"published_at: " + yamlString(note.PublishedAt),
		"discovered_at: " + yamlString(note.DiscoveredAt),
		"status: " + yamlString(note.Status),
		"enrichment_status: " + yamlString(note.EnrichmentStatus),
		"summary_provider: " + yamlString(note.SummaryProvider),
		"summary_model: " + yamlString(note.SummaryModel),
		"summary_generated_at: " + yamlString(note.SummaryGeneratedAt),
	}
	if contentKind == "news" {
		lines = append(lines, newsProperties(note)...)
	} else {
		lines = append(lines, paperProperties(note)...)
	}
	lines = append(lines, "tags:")
	for _, item := range tags {
		lines = append(lines, "  - "+yamlString(item))
	}
	return strings.Join(append(lines, FrontmatterEnd), "\n")
}

var markdownLabelUnsafe = regexp.MustCompile(`[\[\]\n\r]+`)

func markdownLabel(value string) string {
	cleaned := strings.TrimSpace(markdownLabelUnsafe.ReplaceAllString(value, " "))
	if cleaned == "" {
		return "Untitled"
	}
	return cleaned
}

// externalReferenceURL builds a resolver link for a cited work.
func externalReferenceURL(reference Reference) string {
	scheme := strings.ToLower(reference.ExternalScheme)
	identifier := strings.TrimSpace(reference.ExternalID)
	if identifier != "" {
		switch scheme {
		case "doi":
			return "https://doi.org/" + urlx.Quote(identifier, "/:.-_")
		case "arxiv":
			return "https://arxiv.org/abs/" + urlx.Quote(identifier, "/.:-_")
		case "s2":
			return "https://www.semanticscholar.org/paper/" + urlx.Quote(identifier, "-_")
		}
	}
	return strings.TrimSpace(reference.CitedURL)
}

// noteLinkTarget reports the vault path a cited paper's note lives at, when
// that paper is itself kept and synced.
//
// Both conditions are required. A note that is not written yet cannot be
// linked to, and an entry that is no longer kept has had its note archived out
// of the vault, so a link would dangle.
func noteLinkTarget(reference Reference) (string, bool) {
	relativePath := strings.TrimSpace(reference.CitedObsidianPath)
	if reference.CitedStatus != "kept" || reference.CitedExportState != "synced" ||
		!strings.HasPrefix(relativePath, "Herald/Papers/") ||
		!strings.HasSuffix(relativePath, ".md") {
		return "", false
	}
	target := strings.NewReplacer("|", " ", "]", " ", "[", " ").Replace(
		strings.TrimSuffix(relativePath, ".md"))
	return target, true
}

// wikilink builds an Obsidian link with an alias.
func wikilink(target, alias string) string {
	// The alias cannot contain the characters that delimit a link, so they are
	// replaced rather than escaped: Obsidian has no escape for them.
	alias = strings.NewReplacer("|", " ", "[", "(", "]", ")").Replace(alias)
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return "[[" + target + "]]"
	}
	return "[[" + target + "|" + alias + "]]"
}

// referenceMarkdown renders one citation.
//
// A cited paper that is itself kept and synced becomes a real Obsidian link, so
// the vault forms a navigable citation graph. Everything else stays external.
func referenceMarkdown(reference Reference) string {
	title := markdownLabel(reference.Title)
	if title == "" {
		title = "Untitled paper"
	}
	// The printed label is parenthesized rather than bracketed: brackets are
	// not legal inside either a Markdown link's text or an Obsidian alias, and
	// this line is rendered as one or the other.
	if label := strings.TrimSpace(reference.Label); label != "" {
		title = "(" + markdownLabel(label) + ") " + title
	}
	if target, ok := noteLinkTarget(reference); ok {
		return "- " + wikilink(target, title)
	}
	if external := externalReferenceURL(reference); external != "" {
		return "- [" + title + "](" + external + ")"
	}
	return "- " + title
}

// citationResolver turns one in-text citation placeholder into what belongs in
// the note.
//
// This is where a citation becomes a connection. When the cited paper is in
// the vault the marker becomes a real Obsidian link, so following a citation
// inside one paper's note opens the other paper's note and the graph view
// shows the edge. When it is not, the marker keeps exactly the text the
// article printed, because a note is a document the user reads, not a database
// dump, and an unresolved citation should read as the article wrote it.
func citationResolver(references []Reference) func(key, display string) string {
	byKey := make(map[string]Reference, len(references))
	for _, reference := range references {
		if key := strings.TrimSpace(reference.Key); key != "" {
			byKey[key] = reference
		}
	}
	return func(key, display string) string {
		reference, ok := byKey[key]
		if !ok {
			return display
		}
		target, ok := noteLinkTarget(reference)
		if !ok {
			return display
		}
		return wikilink(target, display)
	}
}

// fullTextSection renders the extracted article body with its citations
// resolved.
func fullTextSection(note *Note) string {
	if note.ContentKind == "news" || note.FullText == nil {
		return ""
	}
	body := strings.TrimSpace(note.FullText.Markdown)
	if note.FullText.State != "extracted" || body == "" {
		return ""
	}
	resolved := strings.TrimSpace(unwrapBracketedLinks(
		fulltext.ResolveCitations(body, citationResolver(note.References))))
	if resolved == "" {
		return ""
	}

	lines := []string{"## Full text", ""}
	if provenance := fullTextProvenance(note.FullText); provenance != "" {
		lines = append(lines, provenance, "")
	}
	lines = append(lines, resolved)
	if note.FullText.Truncated {
		lines = append(lines, "",
			"*This article was longer than Herald's extraction limit; the remainder was not included.*")
	}
	return strings.Join(lines, "\n")
}

// unwrapBracketedLinks removes the literal brackets a citation was printed in
// once its contents have become Obsidian links.
//
// An article prints "[12]" and Herald turns the number into a link, which
// would otherwise leave "[[[note|12]]]" in the note. Three consecutive
// brackets are ambiguous markup, so the printed pair is dropped and the link
// itself carries the citation. Brackets around ordinary text are untouched.
func unwrapBracketedLinks(text string) string {
	var out strings.Builder
	out.Grow(len(text))

	for index := 0; index < len(text); {
		if text[index] != '[' {
			out.WriteByte(text[index])
			index++
			continue
		}
		// A wikilink is copied whole so its own brackets are never scanned.
		if end, ok := wikilinkEnd(text, index); ok {
			out.WriteString(text[index:end])
			index = end
			continue
		}
		end, ok := matchingBracket(text, index)
		if !ok {
			out.WriteByte(text[index])
			index++
			continue
		}
		inner := text[index+1 : end]
		if strings.Contains(inner, "[[") {
			out.WriteString(inner)
		} else {
			out.WriteString(text[index : end+1])
		}
		index = end + 1
	}
	return out.String()
}

// wikilinkEnd reports the offset just past a "[[target|alias]]" starting at
// index.
func wikilinkEnd(text string, index int) (int, bool) {
	if !strings.HasPrefix(text[index:], "[[") {
		return 0, false
	}
	closing := strings.Index(text[index:], "]]")
	if closing < 0 {
		return 0, false
	}
	// A nested "[" before the terminator means this is not a wikilink.
	if strings.ContainsAny(text[index+2:index+closing], "[]") {
		return 0, false
	}
	return index + closing + 2, true
}

// matchingBracket finds the "]" closing a literal "[", stepping over any
// wikilinks in between.
func matchingBracket(text string, index int) (int, bool) {
	for scan := index + 1; scan < len(text); {
		switch {
		case strings.HasPrefix(text[scan:], "[["):
			end, ok := wikilinkEnd(text, scan)
			if !ok {
				return 0, false
			}
			scan = end
		case text[scan] == ']':
			return scan, true
		case text[scan] == '[':
			// A second literal bracket means this is not a simple span.
			return 0, false
		default:
			scan++
		}
	}
	return 0, false
}

// fullTextProvenance states where the text came from, so a reader can always
// tell extracted text from the feed's own words.
func fullTextProvenance(text *FullText) string {
	source := map[string]string{
		"arxiv-html":      "the arXiv HTML rendering",
		"arxiv-pdf":       "the arXiv PDF",
		"open-access-pdf": "an open-access PDF",
		"page-pdf":        "the publisher's PDF",
		"upload":          "a PDF you uploaded",
	}[text.SourceKind]
	if source == "" {
		return ""
	}
	line := "*Extracted from " + source
	if text.SourceURL != "" {
		line += " (<" + text.SourceURL + ">)"
	}
	return line + ".*"
}

// fullTextNotice explains, inside the note itself, that the article body is
// missing and what would fix it.
//
// It is written into the note as well as shown in the dashboard because the
// vault outlives any one session: someone reading this note in Obsidian months
// later should be able to see that the body is absent by design, not lost.
func fullTextNotice(note *Note) string {
	if note.ContentKind == "news" || note.FullText == nil {
		return ""
	}
	switch note.FullText.State {
	case "needs_pdf":
		return strings.Join([]string{
			"> [!warning] Full text not available automatically",
			"> Herald found no openly available copy of this paper, so this note holds " +
				"the abstract only.",
			"> Upload the PDF in Herald to add the full text and turn its citations into links.",
		}, "\n")
	case "failed":
		reason := strings.TrimSpace(note.FullText.Error)
		lines := []string{
			"> [!warning] Full text could not be extracted",
			"> Herald could not read the article's text on its last attempt.",
		}
		if reason != "" {
			lines = append(lines, "> Reason: "+markdownLabel(reason))
		}
		lines = append(lines, "> Retry in Herald, or upload the PDF yourself.")
		return strings.Join(lines, "\n")
	case "extracting", "pending":
		return "> [!info] Full text extraction is still running\n" +
			"> Herald is fetching this paper's text and will update the note when it finishes."
	}
	return ""
}

func managedBody(note *Note) string {
	title := strings.TrimSpace(strings.ReplaceAll(note.Title, "\n", " "))
	if title == "" {
		title = "Untitled"
	}
	summaryText := strings.TrimSpace(note.Summary)
	if summaryText == "" {
		summaryText = "No summary is available."
	}
	provider := note.SummaryProvider
	if provider == "" {
		provider = "unknown"
	}
	provenance := provider
	if note.SummaryModel != "" {
		provenance = provider + " (" + note.SummaryModel + ")"
	}
	source := sourceURL(note)
	content := strings.TrimSpace(note.ContentMarkdown)
	if content == "" {
		body := note.Content
		if body == "" {
			body = "No article text was supplied by the feed."
		}
		content = markdown.ToMarkdown(body, source)
	}
	sourceTitle := note.SourceTitle
	if sourceTitle == "" {
		sourceTitle = "Unknown source"
	}
	author := note.Author
	if author == "" {
		author = "Unknown"
	}
	published := note.PublishedAt
	if published == "" {
		published = "Unknown"
	}

	isNews := note.ContentKind == "news"
	contentHeading := "## Article text"
	publicationLabel := "Publication"
	if isNews {
		contentHeading = "## Announcement"
		publicationLabel = "Publisher"
	}
	original := "- Original: unavailable"
	if source != "" {
		original = "- Original: <" + source + ">"
	}

	sections := []string{
		BodyStart,
		"# " + title,
		"",
		"> [!abstract] Summary",
		"> Method: " + provenance,
		">",
		blockquote(summaryText),
	}
	if notice := fullTextNotice(note); notice != "" {
		sections = append(sections, "", notice)
	}
	sections = append(sections,
		"",
		contentHeading,
		"",
		strings.TrimSpace(content),
	)
	if body := fullTextSection(note); body != "" {
		sections = append(sections, "", body)
	}
	sections = append(sections,
		"",
		"## Source",
		"",
		original,
		"- "+publicationLabel+": "+sourceTitle,
		"- Author: "+author,
		"- Published: "+published,
	)
	if !isNews {
		sections = append(sections, "", "## References", "")
		if len(note.References) == 0 {
			sections = append(sections, "No references are available.")
		} else {
			for _, reference := range note.References {
				sections = append(sections, referenceMarkdown(reference))
			}
		}
	}
	return strings.Join(append(sections, BodyEnd), "\n")
}

// Render produces a complete new note, including the empty personal section.
func Render(note *Note) string {
	return strings.Join([]string{
		"---",
		managedFrontmatter(note),
		"---",
		"",
		managedBody(note),
		"",
		"## My Notes",
		"",
		"",
	}, "\n")
}

// formatFloat renders a score the way the reference implementation's float
// repr did: shortest round-trip, but always carrying a decimal point.
//
// Go's encoder writes 0.0 as "0" and 100.0 as "100". The stored note property
// has always been "0.0" and "100.0", and rewriting it on every sync would show
// up as a spurious change in the user's vault history.
func formatFloat(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "null"
	}
	absolute := math.Abs(value)
	if value == 0 || (absolute >= 1e-4 && absolute < 1e16) {
		text := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
