package pdf

import (
	"strings"
)

// Options bounds one extraction.
type Options struct {
	// MaxPages stops after this many pages. Zero means the default.
	MaxPages int
}

const defaultMaxPages = 200

// Document is a PDF's extracted text.
type Document struct {
	Pages []Page
	Title string
	// ImageOnly reports a document that draws images but no text, which is
	// what a scan without an OCR layer looks like. The caller needs to tell
	// that apart from a parse failure, because no amount of retrying will fix
	// it and the user has to supply a searchable copy instead.
	ImageOnly bool
}

// Text renders the whole document as plain text, one line per line.
func (d *Document) Text() string {
	var builder strings.Builder
	for _, page := range d.Pages {
		for _, line := range page.Lines {
			builder.WriteString(line.Text)
			builder.WriteByte('\n')
		}
		builder.WriteByte('\n')
	}
	return builder.String()
}

// CharacterCount reports how much text was extracted, which callers use to
// decide whether extraction actually succeeded.
func (d *Document) CharacterCount() int {
	total := 0
	for _, page := range d.Pages {
		for _, line := range page.Lines {
			total += len(line.Text)
		}
	}
	return total
}

// Extract reads the text of a PDF document.
//
// It returns an error only when the file cannot be read at all. A file that
// parses but contains no text is reported as a Document with no lines, so the
// caller can distinguish "not a PDF" from "a scan of one".
func Extract(data []byte, options Options) (*Document, error) {
	reader, err := NewReader(data)
	if err != nil {
		return nil, err
	}
	limit := options.MaxPages
	if limit <= 0 {
		limit = defaultMaxPages
	}

	nodes := reader.pages(limit)
	if len(nodes) == 0 {
		return nil, errorf("The PDF contains no pages Herald can read")
	}

	document := &Document{Title: reader.documentTitle()}
	budget := maxPageGlyphs * 4
	images := 0

	for index, node := range nodes {
		content := reader.contents(node.dict)
		if len(content) == 0 {
			document.Pages = append(document.Pages, Page{
				Number: index + 1,
				Width:  node.mediaBox[2] - node.mediaBox[0],
				Height: node.mediaBox[3] - node.mediaBox[1],
			})
			continue
		}
		interpreter := reader.newInterpreter(&budget)
		// The media box origin is not always (0,0); shifting here means every
		// later stage can treat the page as starting at the origin.
		interpreter.state.ctm = matrix{1, 0, 0, 1, -node.mediaBox[0], -node.mediaBox[1]}
		interpreter.run(content, node.resources)

		width := node.mediaBox[2] - node.mediaBox[0]
		height := node.mediaBox[3] - node.mediaBox[1]
		glyphs := interpreter.glyphs
		if node.rotate != 0 {
			glyphs = rotateGlyphs(glyphs, node.rotate, width, height)
			if node.rotate == 90 || node.rotate == 270 {
				width, height = height, width
			}
		}
		images += interpreter.images
		document.Pages = append(document.Pages,
			buildPage(glyphs, width, height, index+1, interpreter.images))
	}

	document.ImageOnly = document.CharacterCount() == 0 && images > 0
	return document, nil
}

// rotateGlyphs maps glyph positions through a page's /Rotate value, so a
// landscape page reads in the direction it is displayed.
func rotateGlyphs(glyphs []textGlyph, rotate int, width, height float64) []textGlyph {
	out := make([]textGlyph, len(glyphs))
	copy(out, glyphs)
	for index := range out {
		x, y := out[index].x, out[index].y
		switch rotate {
		case 90:
			out[index].x, out[index].y = y, width-x
		case 180:
			out[index].x, out[index].y = width-x, height-y
		case 270:
			out[index].x, out[index].y = height-y, x
		}
	}
	return out
}

// documentTitle reads /Info /Title, which is a useful cross-check on the
// title a metadata provider reported.
func (r *Reader) documentTitle() string {
	info, ok := r.GetDict(r.trailer["Info"])
	if !ok {
		return ""
	}
	value, ok := r.Resolve(info["Title"]).(String)
	if !ok {
		return ""
	}
	return strings.TrimSpace(decodeTextString([]byte(value)))
}

// decodeTextString converts a PDF text string, which is either UTF-16BE with a
// byte order mark or PDFDocEncoded, to a Go string.
func decodeTextString(data []byte) string {
	if len(data) >= 2 && data[0] == 0xfe && data[1] == 0xff {
		return utf16BEText(data[2:])
	}
	var out strings.Builder
	for _, b := range data {
		// PDFDocEncoding agrees with Latin-1 across the range Herald cares
		// about for a title.
		out.WriteRune(rune(b))
	}
	return out.String()
}
