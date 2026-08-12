// Package pdf extracts positioned text from PDF documents.
//
// It exists because Herald is a single self-contained binary with no runtime
// alongside it: shelling out to pdftotext or embedding a third-party parser
// would break that promise. The scope is deliberately narrow — this reads text
// and where it sits on the page, and nothing else. It renders nothing, draws
// nothing, and executes nothing.
//
// A PDF is untrusted input that arrives from the network or from a file the
// user picked, so every loop here is bounded, every recursion is depth-limited,
// and a malformed document produces an error rather than a panic.
package pdf

import (
	"fmt"
	"strconv"
)

// Object is any PDF object: nil, Bool, Integer, Real, String, Name, Array,
// Dict, *Stream, or Ref.
type Object any

// The PDF object types. Integer and Real stay distinct because object numbers,
// stream lengths, and array indexes must not be rounded through a float.
type (
	Bool    bool
	Integer int64
	Real    float64
	String  []byte
	Name    string
	Array   []Object
	Dict    map[Name]Object
)

// Ref is an indirect reference: "12 0 R".
type Ref struct {
	Number     int
	Generation int
}

// Stream is a dictionary with attached bytes. Raw holds the still-encoded
// bytes; decoding happens on demand and is cached, because a content stream is
// read once but a font program may be referenced by many pages.
type Stream struct {
	Dict Dict
	Raw  []byte

	decoded    []byte
	decodeErr  error
	decodeDone bool
}

// Error is a PDF failure phrased for the person who supplied the file.
type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

func errorf(format string, args ...any) *Error {
	return &Error{Reason: fmt.Sprintf(format, args...)}
}

// Parsing limits. They bound the work a hostile or corrupt file can cause.
const (
	maxNesting    = 64
	maxArrayItems = 200_000
	maxDictKeys   = 8_192
)

// Numeric accessors. Each returns ok=false rather than a zero value that could
// be mistaken for a real measurement.

func toFloat(object Object) (float64, bool) {
	switch value := object.(type) {
	case Integer:
		return float64(value), true
	case Real:
		return float64(value), true
	}
	return 0, false
}

func toInt(object Object) (int, bool) {
	switch value := object.(type) {
	case Integer:
		return int(value), true
	case Real:
		// Real object counts appear in damaged files; truncating is closer to
		// the author's intent than refusing the whole document.
		return int(value), true
	}
	return 0, false
}

// isWhitespace and isDelimiter follow the character classes in the PDF
// specification. Byte 0 counts as whitespace, which is why this is not a
// unicode.IsSpace call.
func isWhitespace(b byte) bool {
	switch b {
	case 0x00, 0x09, 0x0a, 0x0c, 0x0d, 0x20:
		return true
	}
	return false
}

func isDelimiter(b byte) bool {
	switch b {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func isRegular(b byte) bool { return !isWhitespace(b) && !isDelimiter(b) }

// tokenKind identifies one lexical token.
type tokenKind int

const (
	tokEOF tokenKind = iota
	tokNumber
	tokName
	tokString
	tokArrayOpen
	tokArrayClose
	tokDictOpen
	tokDictClose
	tokBraceOpen
	tokBraceClose
	tokKeyword
)

type token struct {
	kind  tokenKind
	text  []byte // name text, string bytes, or keyword text
	num   float64
	isInt bool
	pos   int // offset of the token's first byte
}

// lexer walks PDF syntax. It is shared by the file parser and the content
// stream interpreter, which use the same token grammar.
type lexer struct {
	data []byte
	pos  int

	// pushed holds tokens returned by peeking. Resolving "12 0 R" needs two
	// tokens of lookahead, so the buffer is two deep.
	pushed []token
}

func newLexer(data []byte) *lexer { return &lexer{data: data} }

func (l *lexer) skipSpace() {
	for l.pos < len(l.data) {
		b := l.data[l.pos]
		switch {
		case isWhitespace(b):
			l.pos++
		case b == '%':
			// A comment runs to the end of the line.
			for l.pos < len(l.data) && l.data[l.pos] != '\n' && l.data[l.pos] != '\r' {
				l.pos++
			}
		default:
			return
		}
	}
}

func (l *lexer) unread(t token) { l.pushed = append(l.pushed, t) }

func (l *lexer) next() token {
	if n := len(l.pushed); n > 0 {
		t := l.pushed[n-1]
		l.pushed = l.pushed[:n-1]
		return t
	}
	return l.scan()
}

// peek returns the next token without consuming it.
func (l *lexer) peek() token {
	t := l.next()
	l.unread(t)
	return t
}

func (l *lexer) scan() token {
	l.skipSpace()
	if l.pos >= len(l.data) {
		return token{kind: tokEOF, pos: l.pos}
	}
	start := l.pos
	b := l.data[l.pos]

	switch b {
	case '[':
		l.pos++
		return token{kind: tokArrayOpen, pos: start}
	case ']':
		l.pos++
		return token{kind: tokArrayClose, pos: start}
	case '{':
		l.pos++
		return token{kind: tokBraceOpen, pos: start}
	case '}':
		l.pos++
		return token{kind: tokBraceClose, pos: start}
	case '/':
		return l.scanName()
	case '(':
		return l.scanLiteralString()
	case '<':
		if l.pos+1 < len(l.data) && l.data[l.pos+1] == '<' {
			l.pos += 2
			return token{kind: tokDictOpen, pos: start}
		}
		return l.scanHexString()
	case '>':
		if l.pos+1 < len(l.data) && l.data[l.pos+1] == '>' {
			l.pos += 2
			return token{kind: tokDictClose, pos: start}
		}
		// A stray '>' is skipped rather than treated as a fatal error.
		l.pos++
		return l.scan()
	case ')':
		l.pos++
		return l.scan()
	}

	if b == '+' || b == '-' || b == '.' || (b >= '0' && b <= '9') {
		return l.scanNumber()
	}
	return l.scanKeyword()
}

func (l *lexer) scanName() token {
	start := l.pos
	l.pos++ // consume '/'
	var out []byte
	for l.pos < len(l.data) && isRegular(l.data[l.pos]) {
		b := l.data[l.pos]
		// #xx is a hex escape, the only escape names have.
		if b == '#' && l.pos+2 < len(l.data) {
			high, okHigh := hexValue(l.data[l.pos+1])
			low, okLow := hexValue(l.data[l.pos+2])
			if okHigh && okLow {
				out = append(out, byte(high<<4|low))
				l.pos += 3
				continue
			}
		}
		out = append(out, b)
		l.pos++
	}
	return token{kind: tokName, text: out, pos: start}
}

func hexValue(b byte) (int, bool) {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0'), true
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10, true
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10, true
	}
	return 0, false
}

// scanLiteralString reads (...) with balanced parentheses and backslash
// escapes.
func (l *lexer) scanLiteralString() token {
	start := l.pos
	l.pos++ // consume '('
	var out []byte
	depth := 1
	for l.pos < len(l.data) {
		b := l.data[l.pos]
		l.pos++
		switch b {
		case '\\':
			if l.pos >= len(l.data) {
				break
			}
			escape := l.data[l.pos]
			l.pos++
			switch escape {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '(', ')', '\\':
				out = append(out, escape)
			case '\r':
				// A backslash before a line break is a line continuation.
				if l.pos < len(l.data) && l.data[l.pos] == '\n' {
					l.pos++
				}
			case '\n':
			default:
				if escape >= '0' && escape <= '7' {
					value := int(escape - '0')
					for range 2 {
						if l.pos < len(l.data) && l.data[l.pos] >= '0' && l.data[l.pos] <= '7' {
							value = value*8 + int(l.data[l.pos]-'0')
							l.pos++
						}
					}
					out = append(out, byte(value))
				} else {
					out = append(out, escape)
				}
			}
		case '(':
			depth++
			out = append(out, b)
		case ')':
			depth--
			if depth == 0 {
				return token{kind: tokString, text: out, pos: start}
			}
			out = append(out, b)
		default:
			out = append(out, b)
		}
	}
	return token{kind: tokString, text: out, pos: start}
}

func (l *lexer) scanHexString() token {
	start := l.pos
	l.pos++ // consume '<'
	var out []byte
	var current int
	haveHigh := false
	for l.pos < len(l.data) {
		b := l.data[l.pos]
		l.pos++
		if b == '>' {
			break
		}
		value, ok := hexValue(b)
		if !ok {
			continue
		}
		if haveHigh {
			out = append(out, byte(current<<4|value))
			haveHigh = false
		} else {
			current = value
			haveHigh = true
		}
	}
	if haveHigh {
		// An odd number of digits is padded with a trailing zero.
		out = append(out, byte(current<<4))
	}
	return token{kind: tokString, text: out, pos: start}
}

func (l *lexer) scanNumber() token {
	start := l.pos
	for l.pos < len(l.data) && isRegular(l.data[l.pos]) {
		l.pos++
	}
	text := string(l.data[start:l.pos])
	if value, err := strconv.ParseInt(text, 10, 64); err == nil {
		return token{kind: tokNumber, num: float64(value), isInt: true, pos: start}
	}
	value, err := strconv.ParseFloat(cleanNumber(text), 64)
	if err != nil {
		// Producers emit malformed numbers such as "--3" and "1.-5"; treating
		// them as zero keeps the surrounding page readable.
		return token{kind: tokNumber, num: 0, pos: start}
	}
	return token{kind: tokNumber, num: value, pos: start}
}

// cleanNumber repairs the number spellings real producers emit that Go's
// parser rejects: a leading '.', a trailing '.', repeated signs, and a sign
// appearing mid-token.
func cleanNumber(text string) string {
	var out []byte
	seenDot := false
	for index := 0; index < len(text); index++ {
		b := text[index]
		switch {
		case b == '+' || b == '-':
			if len(out) == 0 {
				if b == '-' {
					out = append(out, b)
				}
			}
		case b == '.':
			if seenDot {
				continue
			}
			seenDot = true
			if len(out) == 0 || out[len(out)-1] == '-' {
				out = append(out, '0')
			}
			out = append(out, b)
		case b >= '0' && b <= '9':
			out = append(out, b)
		}
	}
	if len(out) == 0 || out[len(out)-1] == '.' {
		out = append(out, '0')
	}
	if len(out) == 1 && out[0] == '-' {
		return "0"
	}
	return string(out)
}

func (l *lexer) scanKeyword() token {
	start := l.pos
	for l.pos < len(l.data) && isRegular(l.data[l.pos]) {
		l.pos++
	}
	if l.pos == start {
		// Not a regular character and not handled above: skip it so the lexer
		// always makes progress.
		l.pos++
		return l.scan()
	}
	return token{kind: tokKeyword, text: l.data[start:l.pos], pos: start}
}

// parseObject reads one object. resolveRefs controls whether "12 0 R" becomes
// a Ref; content streams have no indirect references, so they parse with it
// off and treat those tokens as plain numbers and operators.
func (l *lexer) parseObject(depth int, resolveRefs bool) (Object, error) {
	if depth > maxNesting {
		return nil, errorf("PDF object nesting is too deep")
	}
	t := l.next()
	switch t.kind {
	case tokEOF:
		return nil, errorf("PDF ended in the middle of an object")

	case tokNumber:
		if resolveRefs && t.isInt {
			if ref, ok := l.tryReference(t); ok {
				return ref, nil
			}
		}
		if t.isInt {
			return Integer(int64(t.num)), nil
		}
		return Real(t.num), nil

	case tokName:
		return Name(t.text), nil

	case tokString:
		return String(t.text), nil

	case tokArrayOpen:
		var array Array
		for {
			next := l.peek()
			if next.kind == tokArrayClose {
				l.next()
				return array, nil
			}
			if next.kind == tokEOF {
				// An unterminated array keeps what it collected: the page it
				// belongs to is usually still readable.
				return array, nil
			}
			if next.kind == tokDictClose {
				// A stray '>>' inside an array means the array was never
				// closed; stop before consuming the enclosing dictionary's end.
				return array, nil
			}
			item, err := l.parseObject(depth+1, resolveRefs)
			if err != nil {
				return array, nil
			}
			if len(array) >= maxArrayItems {
				return nil, errorf("PDF array is unreasonably large")
			}
			array = append(array, item)
		}

	case tokDictOpen:
		dict := Dict{}
		for {
			next := l.next()
			if next.kind == tokDictClose || next.kind == tokEOF {
				return dict, nil
			}
			if next.kind != tokName {
				// A non-name key is a damaged dictionary. Skip the token and
				// keep reading rather than discarding the whole object.
				continue
			}
			value, err := l.parseObject(depth+1, resolveRefs)
			if err != nil {
				return dict, nil
			}
			if len(dict) >= maxDictKeys {
				return nil, errorf("PDF dictionary is unreasonably large")
			}
			dict[Name(next.text)] = value
		}

	case tokKeyword:
		switch string(t.text) {
		case "true":
			return Bool(true), nil
		case "false":
			return Bool(false), nil
		case "null":
			return nil, nil
		}
		return keyword(t.text), nil

	case tokArrayClose, tokDictClose, tokBraceOpen, tokBraceClose:
		// Unbalanced punctuation: report nothing rather than failing, so one
		// broken object cannot take the document with it.
		return nil, nil
	}
	return nil, nil
}

// keyword is an operator or bare keyword token. It never appears in a
// well-formed object tree, only in content streams.
type keyword string

// tryReference recognizes the "<num> <gen> R" pattern, restoring the lexer
// position when the lookahead does not match.
func (l *lexer) tryReference(first token) (Ref, bool) {
	second := l.next()
	if second.kind != tokNumber || !second.isInt {
		l.unread(second)
		return Ref{}, false
	}
	third := l.next()
	if third.kind == tokKeyword && len(third.text) == 1 && third.text[0] == 'R' {
		return Ref{Number: int(first.num), Generation: int(second.num)}, true
	}
	l.unread(third)
	l.unread(second)
	return Ref{}, false
}
