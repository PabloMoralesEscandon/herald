package pdf

import (
	"strings"
)

// Font is everything the text extractor needs about one font: how to split a
// string into codes, what each code means, and how wide it is.
type Font struct {
	name       Name
	subtype    Name
	bold       bool
	italic     bool
	simple     bool // one byte per code
	identity   bool // Type0 with an identity CMap
	codespaces []codespace
	cidRanges  []cidRange

	widths       map[int]float64 // by code for simple fonts, by CID otherwise
	defaultWidth float64
	toUnicode    map[int]string
	encoding     [256]string
	hasEncoding  bool
}

// codespace is one byte-length range from an embedded CMap.
type codespace struct {
	length int
	low    uint32
	high   uint32
}

// cidRange maps a code range onto consecutive CIDs.
type cidRange struct {
	low   uint32
	high  uint32
	first int
}

// glyph is one decoded character code and the CID it selects.
type glyph struct {
	code  int
	cid   int
	bytes int
}

// loadFont builds a Font from a font dictionary. It never fails: a font it
// cannot understand still yields plausible codes and widths, which keeps the
// rest of the page readable.
func (r *Reader) loadFont(dict Dict) *Font {
	font := &Font{simple: true, defaultWidth: 500}
	font.subtype, _ = r.GetName(dict["Subtype"])
	font.name, _ = r.GetName(dict["BaseFont"])

	lowered := strings.ToLower(string(font.name))
	font.bold = strings.Contains(lowered, "bold") || strings.Contains(lowered, "black") ||
		strings.Contains(lowered, "heavy")
	font.italic = strings.Contains(lowered, "italic") || strings.Contains(lowered, "oblique")

	if font.subtype == "Type0" {
		r.loadType0(font, dict)
	} else {
		r.loadSimple(font, dict)
	}
	if stream, ok := r.GetStream(dict["ToUnicode"]); ok {
		if data, err := r.Data(stream); err == nil {
			font.toUnicode = parseToUnicode(data)
		}
	}
	return font
}

// loadSimple handles Type1, TrueType, Type3, and MMType1 fonts, which use one
// byte per character code.
func (r *Reader) loadSimple(font *Font, dict Dict) {
	descriptor, _ := r.GetDict(dict["FontDescriptor"])
	symbolic := false
	if descriptor != nil {
		if flags, ok := r.GetInt(descriptor["Flags"]); ok {
			symbolic = flags&4 != 0 && flags&32 == 0
			font.italic = font.italic || flags&64 != 0
		}
		if weight, ok := r.GetNumber(descriptor["StemV"]); ok && weight > 120 {
			font.bold = true
		}
		if missing, ok := r.GetNumber(descriptor["MissingWidth"]); ok {
			font.defaultWidth = missing
		}
	}

	// A symbolic font's built-in encoding is inside the embedded font program,
	// which this package does not parse. Standard encoding is the closest
	// approximation, and any /Differences array overrides it anyway.
	base := Name("StandardEncoding")
	if !symbolic {
		base = "StandardEncoding"
	}

	switch encoding := r.Resolve(dict["Encoding"]).(type) {
	case Name:
		font.encoding = baseEncoding(encoding)
		font.hasEncoding = true
	case Dict:
		if named, ok := r.GetName(encoding["BaseEncoding"]); ok {
			font.encoding = baseEncoding(named)
		} else {
			font.encoding = baseEncoding(base)
		}
		font.hasEncoding = true
		r.applyDifferences(font, encoding["Differences"])
	default:
		font.encoding = baseEncoding(base)
		font.hasEncoding = !symbolic
	}

	font.widths = map[int]float64{}
	first, hasFirst := r.GetInt(dict["FirstChar"])
	widths, hasWidths := r.GetArray(dict["Widths"])
	if hasFirst && hasWidths {
		for index, item := range widths {
			if width, ok := r.GetNumber(item); ok {
				font.widths[first+index] = width
			}
		}
	}
	if len(font.widths) == 0 {
		// Without a /Widths array the font is one of the 14 standard fonts.
		// A single representative width keeps spacing sane; the layout pass
		// relies on gaps between glyphs rather than exact advances.
		font.defaultWidth = standardFontWidth(font.name)
	}
}

// applyDifferences overlays the /Differences array onto the base encoding.
func (r *Reader) applyDifferences(font *Font, value Object) {
	differences, ok := r.GetArray(value)
	if !ok {
		return
	}
	code := 0
	for _, item := range differences {
		switch entry := r.Resolve(item).(type) {
		case Integer:
			code = int(entry)
		case Real:
			code = int(entry)
		case Name:
			if code >= 0 && code < 256 {
				font.encoding[code] = string(entry)
			}
			code++
		}
	}
}

// loadType0 handles composite fonts, where a CMap maps multi-byte codes to
// CIDs and the widths live in a descendant font.
func (r *Reader) loadType0(font *Font, dict Dict) {
	font.simple = false
	font.widths = map[int]float64{}
	font.defaultWidth = 1000

	switch encoding := r.Resolve(dict["Encoding"]).(type) {
	case Name:
		// Identity-H and Identity-V are by far the most common, and every
		// other predefined CMap Herald is likely to meet is also two-byte.
		font.identity = strings.HasPrefix(string(encoding), "Identity")
		font.codespaces = []codespace{{length: 2, low: 0, high: 0xffff}}
	case *Stream:
		if data, err := r.Data(encoding); err == nil {
			font.codespaces, font.cidRanges = parseCMap(data)
		}
		if len(font.codespaces) == 0 {
			font.codespaces = []codespace{{length: 2, low: 0, high: 0xffff}}
		}
	default:
		font.identity = true
		font.codespaces = []codespace{{length: 2, low: 0, high: 0xffff}}
	}

	descendants, ok := r.GetArray(dict["DescendantFonts"])
	if !ok || len(descendants) == 0 {
		return
	}
	descendant, ok := r.GetDict(descendants[0])
	if !ok {
		return
	}
	if width, ok := r.GetNumber(descendant["DW"]); ok {
		font.defaultWidth = width
	}
	widths, ok := r.GetArray(descendant["W"])
	if !ok {
		return
	}
	// /W is a sequence of either "start [w1 w2 ...]" or "start end width".
	for index := 0; index < len(widths); {
		start, ok := r.GetInt(widths[index])
		if !ok {
			break
		}
		index++
		if index >= len(widths) {
			break
		}
		if list, isArray := r.Resolve(widths[index]).(Array); isArray {
			for offset, item := range list {
				if width, ok := r.GetNumber(item); ok {
					font.widths[start+offset] = width
				}
			}
			index++
			continue
		}
		end, ok := r.GetInt(widths[index])
		index++
		if !ok || index >= len(widths) {
			break
		}
		width, ok := r.GetNumber(widths[index])
		index++
		if !ok || end < start || end-start > 65_536 {
			continue
		}
		for cid := start; cid <= end; cid++ {
			font.widths[cid] = width
		}
	}
}

// Decode splits a PDF string into character codes.
func (f *Font) Decode(text []byte) []glyph {
	if f.simple {
		glyphs := make([]glyph, 0, len(text))
		for _, b := range text {
			glyphs = append(glyphs, glyph{code: int(b), cid: int(b), bytes: 1})
		}
		return glyphs
	}

	glyphs := make([]glyph, 0, len(text)/2+1)
	for index := 0; index < len(text); {
		length, code := f.matchCodespace(text[index:])
		glyphs = append(glyphs, glyph{code: int(code), cid: f.cid(code), bytes: length})
		index += length
	}
	return glyphs
}

// matchCodespace finds how many bytes the next code occupies.
func (f *Font) matchCodespace(text []byte) (int, uint32) {
	for length := 1; length <= 4 && length <= len(text); length++ {
		value := uint32(0)
		for index := range length {
			value = value<<8 | uint32(text[index])
		}
		for _, space := range f.codespaces {
			if space.length == length && value >= space.low && value <= space.high {
				return length, value
			}
		}
	}
	// No codespace matched: two bytes is the near-universal default for
	// composite fonts, and consuming a fixed width guarantees progress.
	if len(text) >= 2 {
		return 2, uint32(text[0])<<8 | uint32(text[1])
	}
	return 1, uint32(text[0])
}

func (f *Font) cid(code uint32) int {
	if f.identity || len(f.cidRanges) == 0 {
		return int(code)
	}
	for _, r := range f.cidRanges {
		if code >= r.low && code <= r.high {
			return r.first + int(code-r.low)
		}
	}
	return int(code)
}

// Width returns a glyph's advance in text space units (1/1000 em).
func (f *Font) Width(g glyph) float64 {
	key := g.code
	if !f.simple {
		key = g.cid
	}
	if width, ok := f.widths[key]; ok {
		return width
	}
	return f.defaultWidth
}

// Text returns the characters a code represents.
//
// The order matters: a ToUnicode map is the font's own statement of what its
// codes mean and always wins, then the encoding's glyph names, and only then
// the raw byte interpreted as Latin-1.
func (f *Font) Text(g glyph) string {
	if f.toUnicode != nil {
		if text, ok := f.toUnicode[g.code]; ok {
			return text
		}
	}
	if f.simple && g.code >= 0 && g.code < 256 {
		if name := f.encoding[g.code]; name != "" {
			if text := glyphText(name); text != "" {
				return text
			}
		}
		if !f.hasEncoding {
			return ""
		}
		if g.code >= 32 && g.code < 127 {
			return string(rune(g.code))
		}
		return ""
	}
	// A composite font with no ToUnicode map cannot be decoded reliably; a CID
	// is a glyph index, not a character. Reporting nothing is honest, and the
	// extractor treats a page of nothing as "no extractable text".
	return ""
}

// IsSpace reports whether a code is the single-byte space that word spacing
// applies to.
func (f *Font) IsSpace(g glyph) bool { return g.bytes == 1 && g.code == 32 }

// standardFontWidth guesses an average advance for the 14 standard fonts,
// which carry no /Widths array.
func standardFontWidth(name Name) float64 {
	lowered := strings.ToLower(string(name))
	switch {
	case strings.Contains(lowered, "courier"):
		return 600
	case strings.Contains(lowered, "times"):
		return 480
	default:
		return 520
	}
}

// parseToUnicode reads a ToUnicode CMap into a code-to-text map.
func parseToUnicode(data []byte) map[int]string {
	mapping := map[int]string{}
	lexer := newLexer(data)
	var pending []token

	for {
		t := lexer.next()
		if t.kind == tokEOF {
			return mapping
		}
		if t.kind != tokKeyword {
			// Keep a short window of operands; CMap operators follow them.
			pending = append(pending, t)
			if len(pending) > 8 {
				pending = pending[1:]
			}
			continue
		}
		switch string(t.text) {
		case "beginbfchar":
			readBfChar(lexer, mapping)
		case "beginbfrange":
			readBfRange(lexer, mapping)
		}
		pending = pending[:0]
	}
}

func readBfChar(lexer *lexer, mapping map[int]string) {
	for range maxArrayItems {
		source := lexer.next()
		if source.kind != tokString {
			return
		}
		target := lexer.next()
		if target.kind != tokString {
			return
		}
		mapping[int(beInt(source.text))] = utf16BEText(target.text)
	}
}

func readBfRange(lexer *lexer, mapping map[int]string) {
	for range maxArrayItems {
		low := lexer.next()
		if low.kind != tokString {
			return
		}
		high := lexer.next()
		if high.kind != tokString {
			return
		}
		start, end := int(beInt(low.text)), int(beInt(high.text))
		if end < start {
			end = start
		}
		// A range is capped so a corrupt CMap cannot allocate without bound.
		if end-start > 65_535 {
			end = start + 65_535
		}

		next := lexer.next()
		switch next.kind {
		case tokString:
			// Consecutive destinations counting up from one base value.
			base := []byte(next.text)
			for code := start; code <= end; code++ {
				mapping[code] = utf16BEText(incrementLast(base, code-start))
			}
		case tokArrayOpen:
			for code := start; code <= end; code++ {
				item := lexer.next()
				if item.kind != tokString {
					lexer.unread(item)
					break
				}
				mapping[code] = utf16BEText(item.text)
			}
			// Consume the closing bracket if it is next.
			if closing := lexer.next(); closing.kind != tokArrayClose {
				lexer.unread(closing)
			}
		default:
			return
		}
	}
}

// incrementLast adds an offset to the final code unit of a base destination.
func incrementLast(base []byte, offset int) []byte {
	out := append([]byte{}, base...)
	if len(out) < 2 {
		if len(out) == 1 {
			out[0] = byte(int(out[0]) + offset)
		}
		return out
	}
	value := int(out[len(out)-2])<<8 | int(out[len(out)-1])
	value += offset
	out[len(out)-2] = byte(value >> 8)
	out[len(out)-1] = byte(value)
	return out
}

func beInt(data []byte) uint32 {
	value := uint32(0)
	for _, b := range data[:min(len(data), 4)] {
		value = value<<8 | uint32(b)
	}
	return value
}

// utf16BEText converts a CMap destination, which is UTF-16BE, to a Go string.
func utf16BEText(data []byte) string {
	if len(data) == 1 {
		return string(rune(data[0]))
	}
	var out strings.Builder
	for index := 0; index+1 < len(data); index += 2 {
		unit := rune(data[index])<<8 | rune(data[index+1])
		// Combine a surrogate pair into the character it encodes.
		if unit >= 0xd800 && unit <= 0xdbff && index+3 < len(data) {
			low := rune(data[index+2])<<8 | rune(data[index+3])
			if low >= 0xdc00 && low <= 0xdfff {
				out.WriteRune(0x10000 + (unit-0xd800)<<10 + (low - 0xdc00))
				index += 2
				continue
			}
		}
		if unit == 0 {
			continue
		}
		out.WriteRune(unit)
	}
	return out.String()
}

// parseCMap reads codespace ranges and CID ranges from an embedded CMap.
func parseCMap(data []byte) ([]codespace, []cidRange) {
	var spaces []codespace
	var ranges []cidRange
	lexer := newLexer(data)

	for {
		t := lexer.next()
		if t.kind == tokEOF {
			return spaces, ranges
		}
		if t.kind != tokKeyword {
			continue
		}
		switch string(t.text) {
		case "begincodespacerange":
			for range maxArrayItems {
				low := lexer.next()
				if low.kind != tokString {
					break
				}
				high := lexer.next()
				if high.kind != tokString {
					break
				}
				spaces = append(spaces, codespace{
					length: len(low.text), low: beInt(low.text), high: beInt(high.text),
				})
			}
		case "begincidrange":
			for range maxArrayItems {
				low := lexer.next()
				if low.kind != tokString {
					break
				}
				high := lexer.next()
				if high.kind != tokString {
					break
				}
				first := lexer.next()
				if first.kind != tokNumber {
					break
				}
				ranges = append(ranges, cidRange{
					low: beInt(low.text), high: beInt(high.text), first: int(first.num),
				})
			}
		case "begincidchar":
			for range maxArrayItems {
				code := lexer.next()
				if code.kind != tokString {
					break
				}
				cid := lexer.next()
				if cid.kind != tokNumber {
					break
				}
				value := beInt(code.text)
				ranges = append(ranges, cidRange{low: value, high: value, first: int(cid.num)})
			}
		}
	}
}
