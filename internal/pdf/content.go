package pdf

import "math"

// matrix is a PDF transformation matrix [a b c d e f].
type matrix [6]float64

var identityMatrix = matrix{1, 0, 0, 1, 0, 0}

// mul returns m × n, in PDF's row-vector convention.
func (m matrix) mul(n matrix) matrix {
	return matrix{
		m[0]*n[0] + m[1]*n[2],
		m[0]*n[1] + m[1]*n[3],
		m[2]*n[0] + m[3]*n[2],
		m[2]*n[1] + m[3]*n[3],
		m[4]*n[0] + m[5]*n[2] + n[4],
		m[4]*n[1] + m[5]*n[3] + n[5],
	}
}

// scale is the matrix's overall magnification, used to turn a font size in
// text space into one on the page.
func (m matrix) scale() float64 {
	return math.Sqrt(math.Abs(m[0]*m[3] - m[1]*m[2]))
}

// textGlyph is one drawn piece of text with its position on the page.
type textGlyph struct {
	text    string
	x       float64
	y       float64
	width   float64 // advance in page units
	size    float64
	bold    bool
	italic  bool
	spaceAt bool // the glyph is an explicit space character
}

// graphicsState is the subset of the state that affects text placement.
type graphicsState struct {
	ctm matrix

	font       *Font
	fontSize   float64
	charSpace  float64
	wordSpace  float64
	horizontal float64 // Tz, stored as a fraction
	leading    float64
	rise       float64
	renderMode int
}

// interpreter walks one content stream and collects positioned text.
type interpreter struct {
	reader *Reader
	glyphs []textGlyph

	state graphicsState
	stack []graphicsState

	textMatrix matrix
	lineMatrix matrix

	fontCache map[string]*Font
	images    int
	depth     int
	budget    *int
}

// Operand and work limits. A content stream is untrusted, and form XObjects
// can nest, so the interpreter carries a global glyph budget.
const (
	maxOperands   = 64
	maxFormDepth  = 12
	maxPageGlyphs = 400_000
)

func (r *Reader) newInterpreter(budget *int) *interpreter {
	return &interpreter{
		reader:    r,
		state:     graphicsState{ctm: identityMatrix, horizontal: 1},
		fontCache: map[string]*Font{},
		budget:    budget,
	}
}

// run executes a content stream against a resource dictionary.
func (in *interpreter) run(content []byte, resources Dict) {
	if in.depth > maxFormDepth || *in.budget <= 0 {
		return
	}
	lexer := newLexer(content)
	var operands []Object

	for {
		if *in.budget <= 0 {
			return
		}
		t := lexer.peek()
		if t.kind == tokEOF {
			return
		}
		if t.kind != tokKeyword {
			// Content streams contain no indirect references, so "0 0 R" here
			// is three operands rather than a reference.
			object, err := lexer.parseObject(0, false)
			if err != nil {
				return
			}
			if len(operands) < maxOperands {
				operands = append(operands, object)
			}
			continue
		}

		lexer.next()
		operator := string(t.text)
		if operator == "BI" {
			// An inline image's binary data is not valid PDF syntax, so it is
			// skipped as raw bytes rather than tokenized.
			in.images++
			skipInlineImage(lexer)
			operands = operands[:0]
			continue
		}
		in.execute(operator, operands, resources)
		operands = operands[:0]
	}
}

func (in *interpreter) execute(operator string, operands []Object, resources Dict) {
	switch operator {
	case "q":
		if len(in.stack) < maxFormDepth*8 {
			in.stack = append(in.stack, in.state)
		}
	case "Q":
		if n := len(in.stack); n > 0 {
			in.state = in.stack[n-1]
			in.stack = in.stack[:n-1]
		}
	case "cm":
		if m, ok := operandMatrix(operands); ok {
			in.state.ctm = m.mul(in.state.ctm)
		}

	case "BT":
		in.textMatrix, in.lineMatrix = identityMatrix, identityMatrix
	case "ET":

	case "Tf":
		if len(operands) >= 2 {
			if name, ok := operands[len(operands)-2].(Name); ok {
				in.state.font = in.lookupFont(name, resources)
			}
			in.state.fontSize, _ = toFloat(operands[len(operands)-1])
		}
	case "Tc":
		in.state.charSpace = lastNumber(operands)
	case "Tw":
		in.state.wordSpace = lastNumber(operands)
	case "Tz":
		in.state.horizontal = lastNumber(operands) / 100
	case "TL":
		in.state.leading = lastNumber(operands)
	case "Ts":
		in.state.rise = lastNumber(operands)
	case "Tr":
		in.state.renderMode = int(lastNumber(operands))

	case "Td":
		if len(operands) >= 2 {
			tx, _ := toFloat(operands[len(operands)-2])
			ty, _ := toFloat(operands[len(operands)-1])
			in.lineMatrix = matrix{1, 0, 0, 1, tx, ty}.mul(in.lineMatrix)
			in.textMatrix = in.lineMatrix
		}
	case "TD":
		if len(operands) >= 2 {
			tx, _ := toFloat(operands[len(operands)-2])
			ty, _ := toFloat(operands[len(operands)-1])
			in.state.leading = -ty
			in.lineMatrix = matrix{1, 0, 0, 1, tx, ty}.mul(in.lineMatrix)
			in.textMatrix = in.lineMatrix
		}
	case "Tm":
		if m, ok := operandMatrix(operands); ok {
			in.lineMatrix = m
			in.textMatrix = m
		}
	case "T*":
		in.nextLine()

	case "Tj":
		if len(operands) >= 1 {
			if text, ok := operands[len(operands)-1].(String); ok {
				in.show([]byte(text))
			}
		}
	case "'":
		in.nextLine()
		if len(operands) >= 1 {
			if text, ok := operands[len(operands)-1].(String); ok {
				in.show([]byte(text))
			}
		}
	case `"`:
		if len(operands) >= 3 {
			in.state.wordSpace, _ = toFloat(operands[len(operands)-3])
			in.state.charSpace, _ = toFloat(operands[len(operands)-2])
			in.nextLine()
			if text, ok := operands[len(operands)-1].(String); ok {
				in.show([]byte(text))
			}
		}
	case "TJ":
		if len(operands) >= 1 {
			array, ok := operands[len(operands)-1].(Array)
			if !ok {
				return
			}
			for _, item := range array {
				switch value := item.(type) {
				case String:
					in.show([]byte(value))
				default:
					if adjustment, ok := toFloat(value); ok {
						in.adjust(adjustment)
					}
				}
			}
		}

	case "Do":
		if len(operands) >= 1 {
			if name, ok := operands[len(operands)-1].(Name); ok {
				in.doXObject(name, resources)
			}
		}
	}
}

func (in *interpreter) nextLine() {
	in.lineMatrix = matrix{1, 0, 0, 1, 0, -in.state.leading}.mul(in.lineMatrix)
	in.textMatrix = in.lineMatrix
}

// adjust applies a TJ spacing number, which moves the text position without
// drawing anything. A large adjustment is how PDFs represent a space.
func (in *interpreter) adjust(amount float64) {
	tx := -amount / 1000 * in.state.fontSize * in.state.horizontal
	in.textMatrix = matrix{1, 0, 0, 1, tx, 0}.mul(in.textMatrix)
}

// show draws a string and records where each glyph landed.
func (in *interpreter) show(text []byte) {
	font := in.state.font
	if font == nil || *in.budget <= 0 {
		return
	}
	for _, g := range font.Decode(text) {
		if *in.budget <= 0 {
			return
		}
		combined := in.textMatrix.mul(in.state.ctm)
		// The rendering matrix positions the glyph; only its translation and
		// scale are needed, because this package does not draw anything.
		render := matrix{
			in.state.fontSize * in.state.horizontal, 0,
			0, in.state.fontSize,
			0, in.state.rise,
		}.mul(combined)

		advance := font.Width(g) / 1000
		displacement := (advance*in.state.fontSize + in.state.charSpace +
			in.wordSpacing(font, g)) * in.state.horizontal

		// Render mode 3 is invisible text. That is exactly what an OCR layer
		// over a scanned page looks like, so it is kept rather than skipped.
		if in.state.renderMode != 7 {
			content := font.Text(g)
			if content != "" {
				*in.budget--
				in.glyphs = append(in.glyphs, textGlyph{
					text:    content,
					x:       render[4],
					y:       render[5],
					width:   advance * in.state.fontSize * combined.scale() * in.state.horizontal,
					size:    in.state.fontSize * combined.scale(),
					bold:    font.bold,
					italic:  font.italic,
					spaceAt: content == " ",
				})
			}
		}
		in.textMatrix = matrix{1, 0, 0, 1, displacement, 0}.mul(in.textMatrix)
	}
}

func (in *interpreter) wordSpacing(font *Font, g glyph) float64 {
	if font.IsSpace(g) {
		return in.state.wordSpace
	}
	return 0
}

// lookupFont resolves and caches a font by its resource name.
func (in *interpreter) lookupFont(name Name, resources Dict) *Font {
	fonts, ok := in.reader.GetDict(resources["Font"])
	if !ok {
		return in.state.font
	}
	// The cache key includes the reference so two resource dictionaries using
	// the same short name do not collide.
	key := string(name)
	if ref, isRef := fonts[name].(Ref); isRef {
		key = "ref:" + string(rune(ref.Number))
	}
	if font, cached := in.fontCache[key]; cached {
		return font
	}
	dict, ok := in.reader.GetDict(fonts[name])
	if !ok {
		return in.state.font
	}
	font := in.reader.loadFont(dict)
	in.fontCache[key] = font
	return font
}

// doXObject runs a form XObject inline, which is how many producers place
// figures, headers, and occasionally whole page bodies.
func (in *interpreter) doXObject(name Name, resources Dict) {
	xobjects, ok := in.reader.GetDict(resources["XObject"])
	if !ok {
		return
	}
	stream, ok := in.reader.GetStream(xobjects[name])
	if !ok {
		return
	}
	subtype, _ := in.reader.GetName(stream.Dict["Subtype"])
	if subtype == "Image" {
		in.images++
		return
	}
	if subtype != "Form" {
		return
	}
	data, err := in.reader.Data(stream)
	if err != nil {
		return
	}

	saved, savedText, savedLine := in.state, in.textMatrix, in.lineMatrix
	if m, ok := in.reader.GetArray(stream.Dict["Matrix"]); ok && len(m) >= 6 {
		var form matrix
		for index := range 6 {
			form[index], _ = in.reader.GetNumber(m[index])
		}
		in.state.ctm = form.mul(in.state.ctm)
	}
	formResources := resources
	if own, ok := in.reader.GetDict(stream.Dict["Resources"]); ok {
		formResources = own
	}
	in.depth++
	in.run(data, formResources)
	in.depth--
	in.state, in.textMatrix, in.lineMatrix = saved, savedText, savedLine
}

func operandMatrix(operands []Object) (matrix, bool) {
	if len(operands) < 6 {
		return identityMatrix, false
	}
	var m matrix
	for index := range 6 {
		value, ok := toFloat(operands[len(operands)-6+index])
		if !ok {
			return identityMatrix, false
		}
		m[index] = value
	}
	return m, true
}

func lastNumber(operands []Object) float64 {
	if len(operands) == 0 {
		return 0
	}
	value, _ := toFloat(operands[len(operands)-1])
	return value
}

// skipInlineImage advances past a BI ... ID ... EI sequence.
//
// The bytes between ID and EI are raw image data that can contain anything,
// including sequences that look like operators, so they must be skipped by
// scanning rather than parsed.
func skipInlineImage(lexer *lexer) {
	// Read the image dictionary up to the ID operator.
	for {
		t := lexer.next()
		if t.kind == tokEOF {
			return
		}
		if t.kind == tokKeyword && string(t.text) == "ID" {
			break
		}
	}
	position := lexer.pos
	if position < len(lexer.data) && isWhitespace(lexer.data[position]) {
		position++
	}
	for index := position; index+1 < len(lexer.data); index++ {
		if lexer.data[index] != 'E' || lexer.data[index+1] != 'I' {
			continue
		}
		// A real terminator is preceded by whitespace and followed by a
		// delimiter, which distinguishes it from "EI" inside image data.
		if index > position && !isWhitespace(lexer.data[index-1]) {
			continue
		}
		after := index + 2
		if after < len(lexer.data) && !isWhitespace(lexer.data[after]) && !isDelimiter(lexer.data[after]) {
			continue
		}
		lexer.pos = after
		lexer.pushed = nil
		return
	}
	lexer.pos = len(lexer.data)
	lexer.pushed = nil
}
