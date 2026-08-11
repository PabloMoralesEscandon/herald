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

	// ObsidianRelativePath is the note's existing location, used to keep a
	// note in place once it has been written.
	ObsidianRelativePath string
}

// Reference is one rendered citation.
type Reference struct {
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

// referenceMarkdown renders one citation.
//
// A cited paper that is itself kept and synced becomes a real Obsidian link, so
// the vault forms a navigable citation graph. Everything else stays external.
func referenceMarkdown(reference Reference) string {
	title := markdownLabel(reference.Title)
	if title == "" {
		title = "Untitled paper"
	}
	relativePath := strings.TrimSpace(reference.CitedObsidianPath)
	internal := reference.CitedStatus == "kept" &&
		reference.CitedExportState == "synced" &&
		strings.HasPrefix(relativePath, "Herald/Papers/") &&
		strings.HasSuffix(relativePath, ".md")
	if internal {
		target := strings.NewReplacer("|", " ", "]", " ").Replace(
			strings.TrimSuffix(relativePath, ".md"))
		return "- [[" + target + "|" + strings.ReplaceAll(title, "|", " ") + "]]"
	}
	if external := externalReferenceURL(reference); external != "" {
		return "- [" + title + "](" + external + ")"
	}
	return "- " + title
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
		"",
		contentHeading,
		"",
		strings.TrimSpace(content),
		"",
		"## Source",
		"",
		original,
		"- " + publicationLabel + ": " + sourceTitle,
		"- Author: " + author,
		"- Published: " + published,
	}
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
