package pdf

import (
	"math"
	"sort"
	"strings"
)

// Reading order is the whole difficulty of extracting text from a PDF. A page
// is a bag of positioned glyphs with no notion of a paragraph, a column, or
// even a line, and an academic paper is usually two columns with a full-width
// title and abstract above them. Reading glyphs in the order the file happens
// to draw them produces text that interleaves the columns.
//
// The approach here is a recursive XY cut: find the widest band of whitespace
// crossing the region, split there, and recurse. A vertical band is a column
// gutter and a horizontal band is a paragraph or section break, so the
// recursion reproduces the page's own structure without needing to guess at a
// fixed layout.

// fragment is a run of glyphs on one baseline, uninterrupted by a wide gap.
type fragment struct {
	glyphs []textGlyph
	x0     float64
	x1     float64
	y      float64
	size   float64
}

// Line is one line of text with its position on the page.
type Line struct {
	Text   string
	X0     float64
	X1     float64
	Y      float64
	Size   float64
	Bold   bool
	Italic bool
}

// Page is one page's text in reading order.
type Page struct {
	Number int
	Width  float64
	Height float64
	Lines  []Line
	// Images counts the image XObjects drawn on the page. A page with images
	// and no text is a scan, which is worth reporting differently from a page
	// that is simply blank.
	Images int
}

const (
	maxCutDepth   = 24
	minRegionSize = 2
)

// buildPage turns one page's glyphs into ordered lines.
func buildPage(glyphs []textGlyph, width, height float64, number, images int) Page {
	page := Page{Number: number, Width: width, Height: height, Images: images}
	if len(glyphs) == 0 {
		return page
	}
	fragments := buildFragments(glyphs)
	if len(fragments) == 0 {
		return page
	}
	ordered := make([]fragment, 0, len(fragments))
	cut(fragments, width, height, 0, &ordered)
	page.Lines = mergeFragments(ordered)
	return page
}

// buildFragments groups glyphs into baseline runs.
func buildFragments(glyphs []textGlyph) []fragment {
	sorted := make([]textGlyph, len(glyphs))
	copy(sorted, glyphs)
	// Sort top to bottom, then left to right. PDF's y axis points up, so a
	// larger y is higher on the page.
	sort.SliceStable(sorted, func(left, right int) bool {
		if math.Abs(sorted[left].y-sorted[right].y) > 0.01 {
			return sorted[left].y > sorted[right].y
		}
		return sorted[left].x < sorted[right].x
	})

	median := medianSize(sorted)
	tolerance := math.Max(median*0.35, 0.6)

	var rows [][]textGlyph
	var current []textGlyph
	currentY := math.NaN()
	for _, g := range sorted {
		if math.IsNaN(currentY) || math.Abs(g.y-currentY) <= tolerance {
			if math.IsNaN(currentY) {
				currentY = g.y
			}
			current = append(current, g)
			continue
		}
		rows = append(rows, current)
		current = []textGlyph{g}
		currentY = g.y
	}
	if len(current) > 0 {
		rows = append(rows, current)
	}

	var fragments []fragment
	for _, row := range rows {
		sort.SliceStable(row, func(left, right int) bool { return row[left].x < row[right].x })
		fragments = append(fragments, splitRow(row)...)
	}
	return fragments
}

// splitRow breaks one baseline row at gaps wide enough to be a column gutter
// or a table cell boundary rather than a word space.
func splitRow(row []textGlyph) []fragment {
	var out []fragment
	start := 0
	for index := 1; index < len(row); index++ {
		previous := row[index-1]
		gap := row[index].x - (previous.x + previous.width)
		threshold := math.Max(previous.size*1.6, 8)
		if gap > threshold {
			out = append(out, makeFragment(row[start:index]))
			start = index
		}
	}
	if start < len(row) {
		out = append(out, makeFragment(row[start:]))
	}
	return out
}

func makeFragment(glyphs []textGlyph) fragment {
	f := fragment{glyphs: glyphs, x0: glyphs[0].x, y: glyphs[0].y}
	for _, g := range glyphs {
		f.x0 = math.Min(f.x0, g.x)
		f.x1 = math.Max(f.x1, g.x+g.width)
	}
	f.size = medianSize(glyphs)
	return f
}

func medianSize(glyphs []textGlyph) float64 {
	if len(glyphs) == 0 {
		return 10
	}
	sizes := make([]float64, 0, len(glyphs))
	for _, g := range glyphs {
		if g.size > 0 {
			sizes = append(sizes, g.size)
		}
	}
	if len(sizes) == 0 {
		return 10
	}
	sort.Float64s(sizes)
	return sizes[len(sizes)/2]
}

// cut recursively splits a region at its widest whitespace band and appends
// the fragments in reading order.
func cut(fragments []fragment, width, height float64, depth int, out *[]fragment) {
	if len(fragments) == 0 {
		return
	}
	if depth >= maxCutDepth || len(fragments) <= minRegionSize {
		emit(fragments, out)
		return
	}

	// A column gutter is a vertical band no fragment crosses. It must be wide
	// enough not to be mistaken for the space between two words.
	verticalGap, verticalAt := widestVerticalGap(fragments)
	minGutter := math.Max(width*0.02, medianFragmentSize(fragments)*1.2)

	if verticalGap >= minGutter {
		var left, right []fragment
		for _, f := range fragments {
			if f.x1 <= verticalAt {
				left = append(left, f)
			} else {
				right = append(right, f)
			}
		}
		if len(left) > 0 && len(right) > 0 {
			cut(left, width, height, depth+1, out)
			cut(right, width, height, depth+1, out)
			return
		}
	}

	// Otherwise split at the widest horizontal band, which separates a title
	// from a body, or one block from the next.
	horizontalGap, horizontalAt := widestHorizontalGap(fragments)
	if horizontalGap >= medianFragmentSize(fragments)*1.6 {
		var top, bottom []fragment
		for _, f := range fragments {
			if f.y >= horizontalAt {
				top = append(top, f)
			} else {
				bottom = append(bottom, f)
			}
		}
		if len(top) > 0 && len(bottom) > 0 {
			cut(top, width, height, depth+1, out)
			cut(bottom, width, height, depth+1, out)
			return
		}
	}
	emit(fragments, out)
}

// widestVerticalGap finds the widest x band that no fragment overlaps.
func widestVerticalGap(fragments []fragment) (float64, float64) {
	type span struct{ start, end float64 }
	spans := make([]span, 0, len(fragments))
	for _, f := range fragments {
		spans = append(spans, span{f.x0, f.x1})
	}
	sort.Slice(spans, func(left, right int) bool { return spans[left].start < spans[right].start })

	bestGap, bestAt := 0.0, 0.0
	reach := spans[0].end
	for _, s := range spans[1:] {
		if s.start > reach {
			if gap := s.start - reach; gap > bestGap {
				bestGap, bestAt = gap, reach+gap/2
			}
		}
		reach = math.Max(reach, s.end)
	}
	return bestGap, bestAt
}

// widestHorizontalGap finds the widest vertical distance between consecutive
// baselines.
func widestHorizontalGap(fragments []fragment) (float64, float64) {
	baselines := make([]float64, 0, len(fragments))
	seen := map[float64]bool{}
	for _, f := range fragments {
		if !seen[f.y] {
			seen[f.y] = true
			baselines = append(baselines, f.y)
		}
	}
	if len(baselines) < 2 {
		return 0, 0
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(baselines)))

	bestGap, bestAt := 0.0, 0.0
	for index := 1; index < len(baselines); index++ {
		if gap := baselines[index-1] - baselines[index]; gap > bestGap {
			bestGap, bestAt = gap, baselines[index-1]
		}
	}
	return bestGap, bestAt
}

func medianFragmentSize(fragments []fragment) float64 {
	sizes := make([]float64, 0, len(fragments))
	for _, f := range fragments {
		sizes = append(sizes, f.size)
	}
	if len(sizes) == 0 {
		return 10
	}
	sort.Float64s(sizes)
	return sizes[len(sizes)/2]
}

// emit appends a leaf region's fragments top to bottom, left to right.
func emit(fragments []fragment, out *[]fragment) {
	ordered := make([]fragment, len(fragments))
	copy(ordered, fragments)
	sort.SliceStable(ordered, func(left, right int) bool {
		if math.Abs(ordered[left].y-ordered[right].y) > math.Max(ordered[left].size*0.35, 0.6) {
			return ordered[left].y > ordered[right].y
		}
		return ordered[left].x0 < ordered[right].x0
	})
	*out = append(*out, ordered...)
}

// mergeFragments joins fragments that share a baseline back into single lines,
// now that reading order has been decided.
func mergeFragments(fragments []fragment) []Line {
	var lines []Line
	var pending []fragment

	flush := func() {
		if len(pending) == 0 {
			return
		}
		lines = append(lines, buildLine(pending))
		pending = pending[:0]
	}

	for _, f := range fragments {
		if len(pending) > 0 {
			last := pending[len(pending)-1]
			sameLine := math.Abs(f.y-last.y) <= math.Max(last.size*0.35, 0.6) && f.x0 >= last.x0
			if !sameLine {
				flush()
			}
		}
		pending = append(pending, f)
	}
	flush()
	return lines
}

// buildLine renders one line's glyphs to text, inserting spaces where the
// geometry shows a gap the file did not draw a space character for.
func buildLine(fragments []fragment) Line {
	var glyphs []textGlyph
	for _, f := range fragments {
		glyphs = append(glyphs, f.glyphs...)
	}
	sort.SliceStable(glyphs, func(left, right int) bool { return glyphs[left].x < glyphs[right].x })

	var builder strings.Builder
	bold, italic := 0, 0
	for index, g := range glyphs {
		if index > 0 {
			previous := glyphs[index-1]
			gap := g.x - (previous.x + previous.width)
			// A quarter of the font size is about the width of a space in
			// every text face; PDFs frequently position words rather than
			// drawing the space between them.
			if gap > previous.size*0.22 && !strings.HasSuffix(builder.String(), " ") && !g.spaceAt {
				builder.WriteByte(' ')
			}
		}
		builder.WriteString(g.text)
		if g.bold {
			bold++
		}
		if g.italic {
			italic++
		}
	}

	line := Line{
		Text: strings.TrimRight(collapseSpaces(builder.String()), " "),
		Y:    fragments[0].y,
		Size: medianSize(glyphs),
		Bold: bold*2 > len(glyphs),
		// A line is italic only when nearly all of it is, so an italic term
		// inside a sentence does not turn the whole line into emphasis.
		Italic: italic*10 > len(glyphs)*9,
	}
	line.X0 = fragments[0].x0
	for _, f := range fragments {
		line.X0 = math.Min(line.X0, f.x0)
		line.X1 = math.Max(line.X1, f.x1)
	}
	return line
}

// collapseSpaces squeezes repeated spaces, which positioned text produces
// wherever a producer draws several small advances in a row.
func collapseSpaces(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	previousSpace := false
	for _, r := range text {
		isSpace := r == ' ' || r == '\t' || r == ' '
		if isSpace {
			if previousSpace {
				continue
			}
			out.WriteByte(' ')
			previousSpace = true
			continue
		}
		previousSpace = false
		out.WriteRune(r)
	}
	return strings.TrimSpace(out.String())
}
