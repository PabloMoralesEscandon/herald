package fulltext

import (
	"regexp"
	"strconv"
	"strings"
)

// Index resolves the citation markers printed in an article's body to entries
// in its bibliography.
//
// Detection is deliberately conservative: a marker is linked only when it
// resolves to a bibliography entry that was actually parsed. Scientific prose
// is full of bracketed numbers and parenthesized years that are not citations
// at all — matrix indices, equation references, date ranges — and turning one
// of those into a link would put a wrong connection in the user's vault, which
// is worse than leaving the text alone.
type Index struct {
	byNumber     map[int]*Reference
	byAuthorYear map[string][]*Reference
	numeric      bool
}

// NewIndex builds a lookup over parsed bibliography entries.
func NewIndex(references []Reference) *Index {
	index := &Index{
		byNumber:     map[int]*Reference{},
		byAuthorYear: map[string][]*Reference{},
	}
	for position := range references {
		reference := &references[position]
		if number, err := strconv.Atoi(reference.Label); err == nil && number > 0 {
			index.byNumber[number] = reference
			index.numeric = true
		}
		if reference.Year == "" {
			continue
		}
		for _, surname := range reference.Surnames() {
			key := surname + "|" + reference.Year
			index.byAuthorYear[key] = append(index.byAuthorYear[key], reference)
		}
	}
	return index
}

// Empty reports an index with nothing to link against.
func (i *Index) Empty() bool {
	return i == nil || len(i.byNumber) == 0 && len(i.byAuthorYear) == 0
}

var (
	// A bracketed run of numbers, ranges, and separators: [12], [1, 3], [4-6].
	numericMarker = regexp.MustCompile(`\[\s*\d{1,4}(?:\s*[,;]\s*\d{1,4}|\s*[-–—]\s*\d{1,4})*\s*\]`)
	numberOrRange = regexp.MustCompile(`(\d{1,4})(?:\s*([-–—])\s*(\d{1,4}))?`)
	// A parenthesized citation carrying at least one name and one year.
	parentheticalMarker = regexp.MustCompile(`\(([^()]{4,300})\)`)
	// The narrative form, where only the year is parenthesized.
	narrativeMarker = regexp.MustCompile(
		`([A-Z][\p{L}'’-]+(?:\s+(?:et\s+al\.?|and\s+[A-Z][\p{L}'’-]+|&\s*[A-Z][\p{L}'’-]+))?)\s*\((\d{4}[a-z]?)\)`)
	yearInMarker = regexp.MustCompile(`\b((?:1[89]|20)\d{2})[a-z]?\b`)
	nameInMarker = regexp.MustCompile(`\p{Lu}[\p{L}'’-]{2,}`)
)

// Link rewrites every citation marker it can resolve into a placeholder.
//
// The order is fixed: parenthesized markers first, then narrative ones, then
// bracketed numbers. Each pass writes placeholders that the later passes
// cannot match, so no marker is ever rewritten twice.
func (i *Index) Link(text string) string {
	if i.Empty() || text == "" {
		return text
	}
	text = i.linkParenthetical(text)
	text = i.linkNarrative(text)
	return i.linkNumeric(text)
}

// linkNumeric rewrites bracketed citation numbers.
func (i *Index) linkNumeric(text string) string {
	if !i.numeric {
		return text
	}
	return numericMarker.ReplaceAllStringFunc(text, func(marker string) string {
		inner := strings.TrimSpace(strings.Trim(marker, "[]"))
		matches := numberOrRange.FindAllStringSubmatchIndex(inner, -1)
		if len(matches) == 0 {
			return marker
		}

		// Every number in the bracket must resolve. A partly resolving marker
		// is far more likely to be an equation or index reference than a
		// citation with a typo.
		var resolved []*Reference
		for _, match := range matches {
			first, err := strconv.Atoi(inner[match[2]:match[3]])
			if err != nil {
				return marker
			}
			reference, ok := i.byNumber[first]
			if !ok {
				return marker
			}
			resolved = append(resolved, reference)
			if match[6] < 0 {
				continue
			}
			last, err := strconv.Atoi(inner[match[6]:match[7]])
			if err != nil || last < first || last-first > 200 {
				return marker
			}
			// A range cites everything between its endpoints, so all of them
			// must be present for the marker to be a citation.
			for number := first + 1; number <= last; number++ {
				reference, ok := i.byNumber[number]
				if !ok {
					return marker
				}
				resolved = append(resolved, reference)
			}
		}

		// Rebuild the marker with each printed number linked, preserving the
		// separators the article used.
		var out strings.Builder
		out.WriteByte('[')
		position := 0
		for _, match := range matches {
			out.WriteString(inner[position:match[0]])
			first := inner[match[2]:match[3]]
			if reference, ok := i.byNumber[atoi(first)]; ok {
				out.WriteString(CitePlaceholder(reference.Key, first))
			} else {
				out.WriteString(first)
			}
			if match[6] >= 0 {
				out.WriteString(inner[match[4]:match[5]])
				last := inner[match[6]:match[7]]
				if reference, ok := i.byNumber[atoi(last)]; ok {
					out.WriteString(CitePlaceholder(reference.Key, last))
				} else {
					out.WriteString(last)
				}
			}
			position = match[1]
		}
		out.WriteString(inner[position:])
		out.WriteByte(']')
		return out.String()
	})
}

func atoi(value string) int {
	number, _ := strconv.Atoi(value)
	return number
}

// linkParenthetical rewrites "(Smith et al., 2020; Jones, 2019)".
func (i *Index) linkParenthetical(text string) string {
	if len(i.byAuthorYear) == 0 {
		return text
	}
	return parentheticalMarker.ReplaceAllStringFunc(text, func(marker string) string {
		inner := marker[1 : len(marker)-1]
		if !yearInMarker.MatchString(inner) || !nameInMarker.MatchString(inner) {
			return marker
		}
		// Several works are separated by semicolons inside one pair of
		// parentheses; each is resolved on its own.
		parts := strings.Split(inner, ";")
		linked := make([]string, len(parts))
		any := false
		for index, part := range parts {
			reference := i.matchAuthorYear(part)
			if reference == nil {
				linked[index] = part
				continue
			}
			any = true
			leading := part[:len(part)-len(strings.TrimLeft(part, " "))]
			linked[index] = leading + CitePlaceholder(reference.Key, strings.TrimSpace(part))
		}
		if !any {
			return marker
		}
		return "(" + strings.Join(linked, ";") + ")"
	})
}

// linkNarrative rewrites "Smith et al. (2020)".
func (i *Index) linkNarrative(text string) string {
	if len(i.byAuthorYear) == 0 {
		return text
	}
	return narrativeMarker.ReplaceAllStringFunc(text, func(marker string) string {
		groups := narrativeMarker.FindStringSubmatch(marker)
		if len(groups) != 3 {
			return marker
		}
		reference := i.lookupAuthorYear(groups[1], groups[2])
		if reference == nil {
			return marker
		}
		return CitePlaceholder(reference.Key, marker)
	})
}

// matchAuthorYear resolves one "name and year" fragment.
func (i *Index) matchAuthorYear(part string) *Reference {
	year := yearInMarker.FindStringSubmatch(part)
	if year == nil {
		return nil
	}
	for _, name := range nameInMarker.FindAllString(part, -1) {
		if reference := i.lookupAuthorYear(name, year[1]); reference != nil {
			return reference
		}
	}
	return nil
}

func (i *Index) lookupAuthorYear(name, year string) *Reference {
	// A year suffix such as "2020a" disambiguates two works by the same
	// authors; the bibliography stores the bare year.
	year = strings.TrimRight(year, "abcdefgh")
	for _, candidate := range nameInMarker.FindAllString(name, -1) {
		matches := i.byAuthorYear[strings.ToLower(candidate)+"|"+year]
		// An ambiguous name and year pair is left unlinked rather than guessed.
		if len(matches) == 1 {
			return matches[0]
		}
	}
	return nil
}
