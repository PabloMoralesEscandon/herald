package fulltext

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/PabloMoralesEscandon/herald/internal/pdf"
	"github.com/PabloMoralesEscandon/herald/internal/textx"
)

// A PDF states none of the structure a reader sees: there are no paragraphs,
// no headings, and no bibliography, only text at coordinates. This file
// reconstructs that structure from the two signals a typeset page actually
// carries — type size and position — and does it conservatively, because a
// wrong guess ends up in a file the user keeps.

var (
	numberedHeading = regexp.MustCompile(`^(\d+(?:\.\d+)*)\.?\s+(\S.*)$`)
	appendixHeading = regexp.MustCompile(`^(?i)(appendix\s+[A-Z0-9]+|[A-Z])[.:]?\s+(\S.*)$`)
	captionStart    = regexp.MustCompile(`^(?i)(figure|fig\.?|table|algorithm|listing)\s*\d+[.:)]?\s`)
	pageNumberOnly  = regexp.MustCompile(`^(?i)(page\s+)?[ivxlcdm\d]{1,6}([./]\d{1,6})?$`)
	bibNumberStart  = regexp.MustCompile(`^\s*[\[(]\s*(\d{1,4})\s*[\])]\s*`)
	bibOrdinalStart = regexp.MustCompile(`^\s*(\d{1,4})\.\s+\S`)
	sentenceEnd     = regexp.MustCompile(`[.!?][)”"']?$`)
)

// FromPDF extracts an article from a PDF document.
func FromPDF(data []byte) (*Document, error) {
	extracted, err := pdf.Extract(data, pdf.Options{})
	if err != nil {
		return nil, &Error{Reason: err.Error()}
	}
	if extracted.ImageOnly {
		return nil, &ScannedError{
			Reason: "The PDF contains page images with no text layer, so there is nothing to extract",
		}
	}

	document := &Document{Format: FormatPDF, Title: extracted.Title}
	lines := flattenPages(extracted)
	if len(lines) == 0 {
		return nil, &ScannedError{Reason: "The PDF contains no extractable text"}
	}
	lines = removeRunningHeads(lines, len(extracted.Pages))

	bodySize := dominantSize(lines)
	body, bibliography := splitAtReferences(lines, bodySize)
	body = trimFrontMatter(body, bodySize)

	assembleBody(document, body, bodySize)
	document.References = parseBibliography(bibliography)

	if index := NewIndex(document.References); !index.Empty() {
		for position := range document.Blocks {
			document.Blocks[position].Text = index.Link(document.Blocks[position].Text)
		}
	}
	return document, nil
}

// ScannedError reports a PDF with no text layer. It is separate from a parse
// failure because the user's next step differs: retrying will never help, and
// a searchable copy of the same paper will.
type ScannedError struct{ Reason string }

func (e *ScannedError) Error() string { return e.Reason }

// placedLine is one extracted line with the page it came from.
type placedLine struct {
	pdf.Line
	page  int
	first bool // first line of its page
	last  bool // last line of its page
}

func flattenPages(document *pdf.Document) []placedLine {
	var lines []placedLine
	for _, page := range document.Pages {
		for index, line := range page.Lines {
			if strings.TrimSpace(line.Text) == "" {
				continue
			}
			lines = append(lines, placedLine{
				Line: line, page: page.Number,
				first: index == 0, last: index == len(page.Lines)-1,
			})
		}
	}
	return lines
}

// removeRunningHeads drops the repeated header, footer, and page number lines
// that would otherwise interrupt the text every page.
func removeRunningHeads(lines []placedLine, pageCount int) []placedLine {
	if pageCount < 3 {
		// With too few pages, a repeated line is more likely to be real text.
		return dropPageNumbers(lines)
	}
	counts := map[string]map[int]bool{}
	for _, line := range lines {
		if !line.first && !line.last {
			continue
		}
		key := normalizeRunningHead(line.Text)
		if key == "" {
			continue
		}
		if counts[key] == nil {
			counts[key] = map[int]bool{}
		}
		counts[key][line.page] = true
	}

	threshold := max(3, pageCount/3)
	var kept []placedLine
	for _, line := range lines {
		if line.first || line.last {
			if pages := counts[normalizeRunningHead(line.Text)]; len(pages) >= threshold {
				continue
			}
		}
		kept = append(kept, line)
	}
	return dropPageNumbers(kept)
}

// normalizeRunningHead ignores the page number inside a running head, so
// "Smith et al. 3" and "Smith et al. 4" count as the same head.
func normalizeRunningHead(text string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) {
			builder.WriteRune(r)
		}
	}
	value := builder.String()
	if len(value) < 4 {
		return ""
	}
	return value
}

func dropPageNumbers(lines []placedLine) []placedLine {
	var kept []placedLine
	for _, line := range lines {
		text := strings.TrimSpace(line.Text)
		if (line.first || line.last) && pageNumberOnly.MatchString(text) {
			continue
		}
		kept = append(kept, line)
	}
	return kept
}

// dominantSize is the type size most of the body is set in, measured by
// characters rather than lines so headings cannot outvote the body.
func dominantSize(lines []placedLine) float64 {
	weights := map[float64]int{}
	for _, line := range lines {
		weights[math.Round(line.Size*2)/2] += len(line.Text)
	}
	best, bestWeight := 10.0, 0
	for size, weight := range weights {
		if weight > bestWeight || weight == bestWeight && size < best {
			best, bestWeight = size, weight
		}
	}
	if best <= 0 {
		return 10
	}
	return best
}

// splitAtReferences divides the document at its bibliography heading.
//
// Finding the word is not enough. A paper with a table of contents lists
// "References" on its second page, styled exactly like the real heading, and
// splitting there would throw away almost the entire article. So each
// candidate is judged by what follows it: a real bibliography is followed by
// long lines carrying years and author initials, while a contents entry is
// followed by bare page numbers.
func splitAtReferences(lines []placedLine, bodySize float64) (body, bibliography []placedLine) {
	best, bestScore := -1, 0.0
	for index, line := range lines {
		text := strings.TrimSpace(line.Text)
		if len([]rune(text)) > 40 || !isReferencesHeading(text) {
			continue
		}
		// A heading is set larger or bolder than the body; the same word in a
		// sentence is not a section start.
		if line.Size < bodySize*1.02 && !line.Bold {
			continue
		}
		// On a tie the later candidate wins, which keeps as much of the
		// article in the body as the evidence allows.
		if score := bibliographyScore(lines[index+1:]); score >= bestScore {
			best, bestScore = index, score
		}
	}
	if best < 0 || bestScore < 0.25 {
		return lines, nil
	}
	return lines[:best], lines[best+1:]
}

// bibliographyScore rates how much the lines after a heading look like a
// reference list.
func bibliographyScore(following []placedLine) float64 {
	sample := following
	if len(sample) > 40 {
		sample = sample[:40]
	}
	if len(sample) == 0 {
		return 0
	}
	substantial, dated := 0, 0
	for _, line := range sample {
		text := strings.TrimSpace(line.Text)
		if len([]rune(text)) >= 25 {
			substantial++
		}
		if yearInText.MatchString(text) {
			dated++
		}
	}
	return float64(substantial+dated) / float64(2*len(sample))
}

// trimFrontMatter drops a paper's title block, author list, affiliations, and
// table of contents.
//
// None of it is article text, and all of it extracts badly: a title set across
// three lines becomes three headings, and a row of affiliation markers becomes
// a line of stray letters. Herald already records the title, the authors, and
// the abstract as note properties, so the body starts where the article does.
//
// The cut is only made when the article's own first section can be identified
// with confidence, and the same trap applies here as to the bibliography: a
// table of contents lists "1 Introduction" too, so the heading must be
// followed by prose rather than by more contents entries.
func trimFrontMatter(lines []placedLine, bodySize float64) []placedLine {
	limit := min(len(lines), 150)
	for index := range limit {
		line := lines[index]
		if line.page > 3 {
			break
		}
		// The anchor has to be set as a heading. A page-one footnote often
		// begins "1 " too, and cutting there leaves the front matter in.
		if !line.Bold && line.Size < bodySize*1.02 {
			continue
		}
		match := numberedHeading.FindStringSubmatch(strings.TrimSpace(line.Text))
		// Only the paper's first section starts the body; a later one would
		// cut real text away.
		if match == nil || match[1] != "1" {
			continue
		}
		if !followedByProse(lines[index+1:]) {
			continue
		}
		return lines[index:]
	}
	return lines
}

// followedByProse reports whether the lines after a heading read like body
// text rather than like a list of contents entries.
func followedByProse(following []placedLine) bool {
	sample := following
	if len(sample) > 10 {
		sample = sample[:10]
	}
	substantial := 0
	for _, line := range sample {
		if len([]rune(strings.TrimSpace(line.Text))) >= 40 {
			substantial++
		}
	}
	return substantial >= 3
}

// assembleBody turns positioned lines into headings, paragraphs, and captions.
func assembleBody(document *Document, lines []placedLine, bodySize float64) {
	leftMargin := columnLeftMargin(lines)
	rightEdge := columnRightEdge(lines)

	var paragraph []placedLine
	flush := func() {
		if len(paragraph) == 0 {
			return
		}
		text := joinLines(paragraph)
		kind := BlockParagraph
		if captionStart.MatchString(text) {
			kind = BlockCaption
		}
		document.appendBlock(kind, 0, text)
		paragraph = paragraph[:0]
	}

	for index, line := range lines {
		text := strings.TrimSpace(line.Text)
		if text == "" {
			continue
		}
		if level, ok := headingLevel(line, bodySize); ok {
			flush()
			document.appendBlock(BlockHeading, level, text)
			continue
		}
		if len(paragraph) > 0 && startsNewParagraph(paragraph[len(paragraph)-1], line, bodySize, leftMargin, rightEdge) {
			flush()
		}
		paragraph = append(paragraph, line)
		if len(document.Blocks) >= MaxBlocks {
			document.Truncated = true
			break
		}
		_ = index
	}
	flush()
}

// headingLevel reports whether a line is a section heading and how deep it is.
func headingLevel(line placedLine, bodySize float64) (int, bool) {
	text := strings.TrimSpace(line.Text)
	runes := []rune(text)
	if len(runes) == 0 || len(runes) > 120 {
		return 0, false
	}
	// A heading never ends in a sentence terminator or a comma.
	if strings.HasSuffix(text, ",") || strings.HasSuffix(text, ";") {
		return 0, false
	}

	larger := line.Size >= bodySize*1.12
	if match := numberedHeading.FindStringSubmatch(text); match != nil {
		depth := strings.Count(match[1], ".") + 1
		// A numbered heading is only a heading when it is set apart; a
		// sentence can start with "2. " inside an enumerated list.
		if larger || line.Bold {
			return min(depth, 4), true
		}
		return 0, false
	}
	if !larger && !line.Bold {
		return 0, false
	}
	if sentenceEnd.MatchString(text) && len(runes) > 40 {
		return 0, false
	}
	if isReferencesHeading(text) {
		return 1, true
	}
	if appendixHeading.MatchString(text) && (larger || line.Bold) {
		return 1, true
	}
	switch {
	case line.Size >= bodySize*1.45:
		return 1, true
	case line.Size >= bodySize*1.18:
		return 2, true
	case line.Bold && len(runes) <= 80:
		return 3, true
	}
	return 0, false
}

// startsNewParagraph decides whether a line begins a new paragraph.
func startsNewParagraph(previous, current placedLine, bodySize, leftMargin, rightEdge float64) bool {
	sameColumn := math.Abs(current.X0-previous.X0) < bodySize*4 && current.page == previous.page

	// A jump backwards up the page is a column or page break. The paragraph
	// continues across it only when the previous line was full and unfinished.
	if !sameColumn && (current.Y > previous.Y+bodySize || current.page != previous.page) {
		return sentenceEnd.MatchString(strings.TrimSpace(previous.Text)) ||
			previous.X1 < rightEdge-bodySize*2
	}
	gap := previous.Y - current.Y
	if gap > bodySize*1.8 {
		return true
	}
	// A first-line indent starts a paragraph; so does the previous line
	// stopping well short of the column's right edge.
	if current.X0 > leftMargin+bodySize*0.6 && current.X0 > previous.X0+bodySize*0.6 {
		return true
	}
	if previous.X1 < rightEdge-bodySize*2.5 {
		return true
	}
	return false
}

// columnLeftMargin is the left edge most lines start at.
func columnLeftMargin(lines []placedLine) float64 {
	if len(lines) == 0 {
		return 0
	}
	values := make([]float64, 0, len(lines))
	for _, line := range lines {
		values = append(values, line.X0)
	}
	sort.Float64s(values)
	// The tenth percentile ignores a handful of outdented headings without
	// being dragged right by indented paragraphs.
	return values[len(values)/10]
}

// columnRightEdge is the right edge a full line reaches.
func columnRightEdge(lines []placedLine) float64 {
	if len(lines) == 0 {
		return 0
	}
	values := make([]float64, 0, len(lines))
	for _, line := range lines {
		values = append(values, line.X1)
	}
	sort.Float64s(values)
	return values[len(values)*9/10]
}

// joinLines concatenates a paragraph's lines, repairing the hyphenation the
// typesetter introduced at line ends.
func joinLines(lines []placedLine) string {
	var builder strings.Builder
	for index, line := range lines {
		text := strings.TrimSpace(line.Text)
		if index == 0 {
			builder.WriteString(text)
			continue
		}
		previous := builder.String()
		if hyphenated(previous, text) {
			// The hyphen was inserted to break the word; the word itself has
			// no hyphen, so it is removed on rejoining.
			builder.Reset()
			builder.WriteString(strings.TrimSuffix(previous, "-"))
			builder.WriteString(text)
			continue
		}
		builder.WriteByte(' ')
		builder.WriteString(text)
	}
	return textx.CollapseStrip(builder.String())
}

// hyphenated reports a line-ending hyphen that split one word in two.
func hyphenated(previous, next string) bool {
	if !strings.HasSuffix(previous, "-") || len(previous) < 3 || next == "" {
		return false
	}
	// A trailing hyphen after a digit or an uppercase letter is usually part
	// of a compound or an identifier, not a line break.
	stem := []rune(strings.TrimSuffix(previous, "-"))
	if len(stem) == 0 {
		return false
	}
	last := stem[len(stem)-1]
	if !unicode.IsLower(last) {
		return false
	}
	return unicode.IsLower([]rune(next)[0])
}

// parseBibliography splits the reference section into entries.
func parseBibliography(lines []placedLine) []Reference {
	if len(lines) == 0 {
		return nil
	}
	groups := groupBibliographyEntries(lines)

	references := make([]Reference, 0, len(groups))
	for position, group := range groups {
		if len(references) >= MaxReferences {
			break
		}
		raw := joinLines(group)
		if len([]rune(raw)) < 12 {
			continue
		}
		label := ""
		if match := bibNumberStart.FindStringSubmatch(raw); match != nil {
			label = match[1]
			raw = strings.TrimSpace(raw[len(match[0]):])
		} else if match := bibOrdinalStart.FindStringSubmatch(raw); match != nil {
			label = match[1]
			raw = strings.TrimSpace(raw[len(match[0]):])
		}
		reference := ParseReference(label, raw, position+1)
		if reference.Raw == "" {
			continue
		}
		references = append(references, reference)
	}
	return references
}

// groupBibliographyEntries decides where one reference ends and the next
// begins, using the numbering when there is any and the hanging indent that
// every reference style uses when there is not.
func groupBibliographyEntries(lines []placedLine) [][]placedLine {
	numbered := 0
	for _, line := range lines {
		if bibNumberStart.MatchString(line.Text) || bibOrdinalStart.MatchString(line.Text) {
			numbered++
		}
	}

	leftMargin := columnLeftMargin(lines)
	bodySize := dominantSize(lines)

	var groups [][]placedLine
	var current []placedLine
	for index, line := range lines {
		starts := false
		switch {
		case numbered >= 2:
			starts = bibNumberStart.MatchString(line.Text) || bibOrdinalStart.MatchString(line.Text)
		default:
			// Without numbering, an entry begins at the margin and its
			// continuation lines are indented under it.
			starts = line.X0 <= leftMargin+bodySize*0.4
			if index > 0 && lines[index-1].page != line.page {
				starts = starts || line.X0 <= leftMargin+bodySize*0.4
			}
		}
		if starts && len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}
