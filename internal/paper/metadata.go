package paper

import (
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
	"github.com/PabloMoralesEscandon/herald/internal/urlx"
	xhtml "golang.org/x/net/html"
)

// ReferenceMetadata is one cited work as reported by a provider.
type ReferenceMetadata struct {
	Title       string
	URL         string
	Identifiers map[string]string
}

// Metadata is everything Herald learned about a paper from all sources.
type Metadata struct {
	Title            string
	Authors          []string
	Abstract         string
	PublishedAt      *string
	URL              string
	Identifiers      map[string]string
	Topics           []string
	SuppliedKeywords []string
	References       []ReferenceMetadata
	Provider         string
}

var wordPattern = regexp.MustCompile(`(?i)[a-z](?:[a-z0-9+.-]*[a-z0-9+])?`)

var keywordStopwords = map[string]bool{
	"about": true, "after": true, "against": true, "also": true, "among": true,
	"and": true, "are": true, "based": true, "been": true, "before": true,
	"being": true, "between": true, "both": true, "but": true, "can": true,
	"could": true, "does": true, "each": true, "for": true, "from": true,
	"have": true, "into": true, "its": true, "more": true, "most": true,
	"not": true, "our": true, "over": true, "paper": true, "results": true,
	"show": true, "such": true, "than": true, "that": true, "the": true,
	"their": true, "these": true, "they": true, "this": true, "through": true,
	"using": true, "was": true, "were": true, "which": true, "while": true,
	"with": true, "within": true, "without": true, "would": true, "you": true,
}

// citationMeta holds the values scraped from a publication page's <meta> tags.
type citationMeta struct {
	values       map[string][]string
	canonicalURL string
	pageTitle    string
}

// parseCitationPage reads standard citation_* metadata from a publication page.
//
// Only declared metadata is read; Herald does not scrape article body content.
func parseCitationPage(document []byte) (*citationMeta, error) {
	meta := &citationMeta{values: map[string][]string{}}
	tokenizer := xhtml.NewTokenizer(strings.NewReader(string(document)))
	var titleParts []string
	insideTitle := false
	for {
		switch tokenizer.Next() {
		case xhtml.ErrorToken:
			meta.pageTitle = strings.Join(strings.Fields(strings.Join(titleParts, "")), " ")
			return meta, nil
		case xhtml.TextToken:
			if insideTitle {
				titleParts = append(titleParts, string(tokenizer.Text()))
			}
		case xhtml.EndTagToken:
			if name, _ := tokenizer.TagName(); string(name) == "title" {
				insideTitle = false
			}
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			token := tokenizer.Token()
			switch strings.ToLower(token.Data) {
			case "meta":
				var name, property, content string
				for _, attribute := range token.Attr {
					switch strings.ToLower(attribute.Key) {
					case "name":
						name = attribute.Val
					case "property":
						property = attribute.Val
					case "content":
						content = attribute.Val
					}
				}
				key := strings.ToLower(name)
				if key == "" {
					key = strings.ToLower(property)
				}
				content = strings.TrimSpace(html.UnescapeString(content))
				if key != "" && content != "" {
					meta.values[key] = append(meta.values[key], content)
				}
			case "link":
				var rel, href string
				for _, attribute := range token.Attr {
					switch strings.ToLower(attribute.Key) {
					case "rel":
						rel = attribute.Val
					case "href":
						href = attribute.Val
					}
				}
				if strings.EqualFold(rel, "canonical") {
					meta.canonicalURL = strings.TrimSpace(href)
				}
			case "title":
				insideTitle = true
			}
		}
	}
}

func (m *citationMeta) first(names ...string) string {
	for _, name := range names {
		if items := m.values[name]; len(items) > 0 {
			return strings.TrimSpace(items[0])
		}
	}
	return ""
}

// ParseCitationMetadata converts a publication page into paper metadata.
func ParseCitationMetadata(document []byte, pageURL string) (Metadata, error) {
	meta, err := parseCitationPage(document)
	if err != nil {
		return Metadata{}, &ImportError{
			Reason: fmt.Sprintf("Paper page metadata could not be parsed: %v", err),
		}
	}
	identifiers := map[string]string{}
	if value := meta.first("citation_doi", "dc.identifier", "prism.doi"); value != "" {
		if doi, err := NormalizeDOI(value); err == nil {
			identifiers["doi"] = doi
		}
	}
	if value := meta.first("citation_arxiv_id", "arxiv_id"); value != "" {
		if arxiv, err := NormalizeArxivID(value); err == nil {
			identifiers["arxiv"] = arxiv
		}
	}

	var keywords []string
	for _, value := range append(
		append([]string{}, meta.values["citation_keywords"]...),
		meta.values["keywords"]...) {
		for _, item := range strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ';'
		}) {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				keywords = append(keywords, trimmed)
			}
		}
	}

	canonical := pageURL
	if meta.canonicalURL != "" {
		canonical = urlx.Join(pageURL, meta.canonicalURL)
	}
	// A canonical link that downgrades the scheme is not trusted.
	if urlx.Scheme(canonical) != "https" {
		canonical = pageURL
	}

	var authors []string
	for _, item := range append(
		append([]string{}, meta.values["citation_author"]...),
		meta.values["dc.creator"]...) {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			authors = append(authors, trimmed)
		}
	}

	title := meta.first("citation_title", "dc.title", "og:title")
	if title == "" {
		title = meta.pageTitle
	}
	return Metadata{
		Title:    title,
		Authors:  authors,
		Abstract: meta.first("citation_abstract", "dc.description", "description", "og:description"),
		PublishedAt: normalizeDate(
			meta.first("citation_publication_date", "citation_date", "dc.date"), ""),
		URL:              canonical,
		Identifiers:      identifiers,
		SuppliedKeywords: keywords,
		Provider:         "page-meta",
	}, nil
}

// normalizeDate accepts the many shapes providers use and returns UTC ISO 8601.
// An unparseable value is truncated and kept rather than discarded.
func normalizeDate(value string, year string) *string {
	text := strings.TrimSpace(value)
	if text == "" && year != "" {
		text = year
	}
	if text == "" {
		return nil
	}
	if matched, _ := regexp.MatchString(`^\d{4}$`, text); matched {
		result := text + "-01-01T00:00:00+00:00"
		return &result
	}
	candidate := strings.Replace(text, "Z", "+00:00", 1)
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
	} {
		if parsed, err := time.Parse(layout, candidate); err == nil {
			result := parsed.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05-07:00")
			return &result
		}
	}
	runes := []rune(text)
	if len(runes) > 64 {
		runes = runes[:64]
	}
	result := string(runes)
	return &result
}

// cleanText collapses whitespace and resolves entities.
func cleanText(value string) string {
	return textx.CollapseStrip(html.UnescapeString(value))
}

// ExtractedKeyword is one deterministic keyword with its rank score.
type ExtractedKeyword struct {
	Keyword  string
	Score    float64
	Provider string
}

// ExtractKeywords ranks deterministic uni/bi/tri-grams with a title boost.
//
// It runs entirely offline so every imported paper gets keywords even with no
// model and no provider metadata.
func ExtractKeywords(title, abstract string, limit int) []ExtractedKeyword {
	if limit <= 0 {
		limit = 8
	}
	titleTokens := lowerWords(title)
	bodyTokens := lowerWords(abstract)
	scores := map[string]float64{}
	for _, group := range []struct {
		tokens []string
		weight float64
	}{{titleTokens, 4.0}, {bodyTokens, 1.0}} {
		for size := 1; size <= 3; size++ {
			for index := 0; index+size <= len(group.tokens); index++ {
				phraseTokens := group.tokens[index : index+size]
				if anyStopword(phraseTokens) {
					continue
				}
				phrase := strings.Join(phraseTokens, " ")
				scores[phrase] += group.weight * (1.0 + 0.55*float64(size-1))
			}
		}
	}

	type ranked struct {
		phrase string
		score  float64
	}
	var candidates []ranked
	for phrase, score := range scores {
		if len([]rune(phrase)) < 4 {
			continue
		}
		candidates = append(candidates, ranked{phrase: phrase, score: roundTo(score, 3)})
	}
	sort.Slice(candidates, func(left, right int) bool {
		a, b := candidates[left], candidates[right]
		if a.score != b.score {
			return a.score > b.score
		}
		aSpaces := strings.Count(a.phrase, " ")
		bSpaces := strings.Count(b.phrase, " ")
		if aSpaces != bSpaces {
			return aSpaces > bSpaces
		}
		return a.phrase < b.phrase
	})

	var selected []ExtractedKeyword
	for _, candidate := range candidates {
		// Drop a phrase that is a strict subset of one already selected, so
		// "learning" does not crowd out "reinforcement learning".
		if isProperSubsetOfAny(candidate.phrase, selected) {
			continue
		}
		selected = append(selected, ExtractedKeyword{
			Keyword: candidate.phrase, Score: candidate.score,
			Provider: "deterministic-tfidf",
		})
		if len(selected) == limit {
			break
		}
	}
	return selected
}

func lowerWords(text string) []string {
	matches := wordPattern.FindAllString(text, -1)
	words := make([]string, len(matches))
	for index, match := range matches {
		words[index] = strings.ToLower(match)
	}
	return words
}

func anyStopword(tokens []string) bool {
	for _, token := range tokens {
		if keywordStopwords[token] {
			return true
		}
	}
	return false
}

func isProperSubsetOfAny(phrase string, selected []ExtractedKeyword) bool {
	words := map[string]bool{}
	for _, word := range strings.Fields(phrase) {
		words[word] = true
	}
	for _, existing := range selected {
		existingWords := map[string]bool{}
		for _, word := range strings.Fields(existing.Keyword) {
			existingWords[word] = true
		}
		if len(words) >= len(existingWords) {
			continue
		}
		subset := true
		for word := range words {
			if !existingWords[word] {
				subset = false
				break
			}
		}
		if subset {
			return true
		}
	}
	return false
}

func roundTo(value float64, places int) float64 {
	factor := 1.0
	for range places {
		factor *= 10
	}
	rounded := float64(int64(value*factor+copySign(0.5, value))) / factor
	return rounded
}

func copySign(magnitude, sign float64) float64 {
	if sign < 0 {
		return -magnitude
	}
	return magnitude
}
