package fulltext

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
)

// A bibliography entry is free-form text: every journal, every style file, and
// every author does it differently. Herald therefore takes identifiers where
// it can find them and treats everything else as a best effort. That order
// matters — a DOI or an arXiv ID matches a paper exactly, so those are what
// citation linking relies on, and the parsed title is only a fallback for
// entries that carry no identifier at all.

var (
	doiInText   = regexp.MustCompile(`(?i)\b10\.\d{4,9}/[-._;()/:a-z0-9]*[a-z0-9]`)
	arxivInText = regexp.MustCompile(
		`(?i)arxiv[:\s]*((?:\d{4}\.\d{4,5}|[a-z-]+(?:\.[a-z]{2})?/\d{7})(?:v\d+)?)`)
	arxivURLInText = regexp.MustCompile(
		`(?i)arxiv\.org/(?:abs|pdf)/((?:\d{4}\.\d{4,5}|[a-z-]+(?:\.[a-z]{2})?/\d{7})(?:v\d+)?)`)
	urlInText  = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+`)
	yearInText = regexp.MustCompile(`\b(1[89]\d{2}|20\d{2})\b`)
	// A quoted span is the title in most citation styles that use quotes.
	quotedTitle = regexp.MustCompile(`[“"]([^“”"]{8,300})[”"]`)
	// An author list segment: initials, "et al.", or "and" between names.
	authorish       = regexp.MustCompile(`(?i)(\b[A-Z]\.\s*){1,4}|(\bet\s+al\b)|(\band\b)`)
	numericLabel    = regexp.MustCompile(`^\s*[\[(]?\s*(\d{1,4})\s*[\])).]?\s*`)
	trailingVersion = regexp.MustCompile(`(?i)v\d+$`)
	nonWord         = regexp.MustCompile(`[^a-z0-9]+`)
)

// ParseReference turns one raw bibliography entry into a Reference.
func ParseReference(label, raw string, position int) Reference {
	raw = textx.CollapseStrip(raw)
	reference := Reference{
		Label:    textx.CollapseStrip(label),
		Raw:      raw,
		Position: position,
	}
	if reference.Label == "" {
		if match := numericLabel.FindStringSubmatch(raw); match != nil {
			reference.Label = match[1]
			raw = strings.TrimSpace(raw[len(match[0]):])
			reference.Raw = raw
		}
	}
	reference.Label = strings.Trim(reference.Label, "[]().,  ")

	if match := doiInText.FindString(raw); match != "" {
		reference.DOI = strings.ToLower(strings.TrimRight(match, ".,;)"))
	}
	if match := arxivURLInText.FindStringSubmatch(raw); match != nil {
		reference.ArxivID = normalizeArxiv(match[1])
	} else if match := arxivInText.FindStringSubmatch(raw); match != nil {
		reference.ArxivID = normalizeArxiv(match[1])
	}
	if match := urlInText.FindString(raw); match != "" {
		reference.URL = strings.TrimRight(match, ".,;)")
	}
	if match := yearInText.FindAllString(raw, -1); len(match) > 0 {
		// The publication year is the last plausible one; a volume or page
		// range can look like a year earlier in the string.
		reference.Year = match[len(match)-1]
	}
	reference.Title = guessTitle(raw)
	reference.Authors = guessAuthors(raw)
	reference.Key = referenceKey(reference)
	return reference
}

func normalizeArxiv(value string) string {
	return trailingVersion.ReplaceAllString(strings.ToLower(strings.TrimSpace(value)), "")
}

// referenceKey derives the identity used to merge this entry with the citation
// edges providers report.
//
// The scheme prefixes match the ones the importer already writes, so a
// bibliography entry carrying a DOI lands on the same row as the provider's
// reference to that DOI instead of duplicating it.
func referenceKey(reference Reference) string {
	switch {
	case reference.DOI != "":
		return "doi:" + reference.DOI
	case reference.ArxivID != "":
		return "arxiv:" + reference.ArxivID
	}
	if normalized := normalizeForMatch(reference.Title); normalized != "" {
		return "bib:" + shortHash(normalized)
	}
	if reference.URL != "" {
		return "url:" + reference.URL
	}
	if normalized := normalizeForMatch(reference.Raw); normalized != "" {
		return "bib:" + shortHash(normalized)
	}
	return "bibpos:" + strconv.Itoa(reference.Position)
}

// shortHash keeps a derived key short and stable across re-extractions.
func shortHash(value string) string {
	digest := sha1.Sum([]byte(value))
	return hex.EncodeToString(digest[:8])
}

// normalizeForMatch reduces a title to comparable words.
func normalizeForMatch(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	return strings.Trim(nonWord.ReplaceAllString(lowered, " "), " ")
}

// NormalizeTitle exposes the comparison form so callers can match a parsed
// entry against titles Herald already stores.
func NormalizeTitle(value string) string { return normalizeForMatch(value) }

// guessTitle extracts the most likely title from a raw reference.
func guessTitle(raw string) string {
	if match := quotedTitle.FindStringSubmatch(raw); match != nil {
		return strings.Trim(textx.CollapseStrip(match[1]), " .,")
	}
	// Strip a leading parenthesized year, which APA-style entries put right
	// after the authors and which would otherwise start the title segment.
	working := raw
	if index := strings.Index(working, ")."); index > 0 && index < 80 {
		working = working[index+2:]
	}

	best := ""
	for _, segment := range splitSentences(working) {
		candidate := strings.Trim(textx.CollapseStrip(segment), " .,")
		words := strings.Fields(candidate)
		if len(words) < 3 || len(words) > 40 {
			continue
		}
		// Skip the author list and the venue, which are the two segments a
		// title is usually sandwiched between.
		if looksLikeAuthors(candidate) || looksLikeVenue(candidate) {
			continue
		}
		best = candidate
		break
	}
	if best == "" {
		return ""
	}
	if runes := []rune(best); len(runes) > 300 {
		best = string(runes[:300])
	}
	return best
}

// splitSentences breaks a reference on the separators citation styles use,
// keeping abbreviations such as "J. Smith" and "Proc." intact.
func splitSentences(raw string) []string {
	var segments []string
	var current strings.Builder
	runes := []rune(raw)
	for index, r := range runes {
		if r != '.' {
			current.WriteRune(r)
			continue
		}
		previous := rune(0)
		if index > 0 {
			previous = runes[index-1]
		}
		next := rune(0)
		if index+1 < len(runes) {
			next = runes[index+1]
		}
		// A period after a single capital letter is an initial, not a break.
		isInitial := index >= 1 && previous >= 'A' && previous <= 'Z' &&
			(index < 2 || !isLetter(runes[index-2]))
		if isInitial || next != ' ' && next != 0 {
			current.WriteRune(r)
			continue
		}
		segments = append(segments, current.String())
		current.Reset()
	}
	if current.Len() > 0 {
		segments = append(segments, current.String())
	}
	return segments
}

func isLetter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func looksLikeAuthors(segment string) bool {
	words := strings.Fields(segment)
	if len(words) == 0 {
		return false
	}
	matches := authorish.FindAllString(segment, -1)
	// A segment that is mostly initials and connectives is the author list.
	if len(matches) >= 2 && len(words) <= 20 {
		return true
	}
	commas := strings.Count(segment, ",")
	return commas >= 2 && len(matches) >= 1
}

var venueWords = map[string]bool{
	"proc": true, "proceedings": true, "conference": true, "journal": true,
	"transactions": true, "symposium": true, "workshop": true, "press": true,
	"volume": true, "vol": true, "pages": true, "pp": true, "eds": true,
	"editors": true, "preprint": true, "technical": true, "report": true,
	"university": true, "acm": true, "ieee": true, "springer": true,
	"elsevier": true, "advances": true, "arxiv": true,
}

func looksLikeVenue(segment string) bool {
	fields := strings.Fields(strings.ToLower(segment))
	hits := 0
	for _, field := range fields {
		if venueWords[strings.Trim(field, ".,:;()")] {
			hits++
		}
	}
	return hits*4 >= len(fields) && hits > 0
}

// guessAuthors returns the leading author segment, which is what an
// author-year citation marker refers to.
func guessAuthors(raw string) string {
	segments := splitSentences(raw)
	if len(segments) == 0 {
		return ""
	}
	candidate := strings.Trim(textx.CollapseStrip(segments[0]), " .,")
	if candidate == "" || len(candidate) > 300 {
		return ""
	}
	return candidate
}

// Surnames lists the family names in an author segment, which is what an
// author-year marker prints.
func (r Reference) Surnames() []string {
	var names []string
	seen := map[string]bool{}
	// Split on the separators author lists use, then take the longest
	// alphabetic word in each part: initials are short, surnames are not.
	for _, part := range strings.FieldsFunc(r.Authors, func(r rune) bool {
		return r == ',' || r == ';' || r == '&'
	}) {
		best := ""
		for _, word := range strings.Fields(part) {
			cleaned := strings.Trim(word, ".,;()[]")
			if len(cleaned) <= 2 || strings.EqualFold(cleaned, "and") ||
				strings.EqualFold(cleaned, "et") || strings.EqualFold(cleaned, "al") {
				continue
			}
			if len(cleaned) > len(best) {
				best = cleaned
			}
		}
		lowered := strings.ToLower(best)
		if lowered != "" && !seen[lowered] {
			seen[lowered] = true
			names = append(names, lowered)
		}
	}
	return names
}
