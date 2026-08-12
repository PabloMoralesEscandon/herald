package pdf

import (
	"bytes"
	"regexp"
	"strconv"
)

// Structural limits. A cross-reference chain can be made to loop, and object
// streams can nest, so both are capped.
const (
	maxXrefSections = 512
	maxResolveDepth = 32
	maxObjects      = 1 << 21
)

// location says where an object's bytes live: at a byte offset, or inside a
// compressed object stream.
type location struct {
	offset     int64
	inStream   bool
	streamNum  int
	streamSlot int
}

// Reader is a parsed PDF document. It resolves objects lazily and caches them,
// because a font or resource dictionary is referenced from every page that
// uses it.
type Reader struct {
	data []byte

	locations map[int]location
	cache     map[int]Object
	loading   map[int]bool
	trailer   Dict

	// authoritative marks the object numbers that came from a cross-reference
	// section, so the reconstruction scan never overrides them.
	authoritative map[int]bool

	decryptor  *decryptor
	objStreams map[int][]Object

	// currentObject tracks the indirect object being parsed so strings can be
	// decrypted with the right per-object key.
	currentNumber int
	currentGen    int
}

// NewReader parses a document's structure. It does not read any page yet.
func NewReader(data []byte) (*Reader, error) {
	if len(data) == 0 {
		return nil, errorf("The PDF file is empty")
	}
	if !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")) &&
		!bytes.Contains(data, []byte("%PDF-")) {
		return nil, errorf("The file is not a PDF")
	}
	reader := &Reader{
		data:       data,
		locations:  map[int]location{},
		cache:      map[int]Object{},
		loading:    map[int]bool{},
		objStreams: map[int][]Object{},
		trailer:    Dict{},
	}

	// A damaged cross-reference table is common, and reconstructing by scanning
	// for object headers recovers nearly all of those files. The scan runs
	// first so that table entries, which are authoritative, overwrite it.
	reader.reconstruct()
	reader.readXrefChain()

	if err := reader.setupDecryption(); err != nil {
		return nil, err
	}
	if len(reader.locations) == 0 {
		return nil, errorf("The PDF contains no readable objects")
	}
	return reader, nil
}

// Trailer exposes the document trailer dictionary.
func (r *Reader) Trailer() Dict { return r.trailer }

var objectHeaderPattern = regexp.MustCompile(`(?m)(\d{1,10})[\x00\t\f\r\n ]+(\d{1,5})[\x00\t\f\r\n ]+obj\b`)

// reconstruct scans the whole file for object headers.
//
// It is the fallback for files whose cross-reference data is missing or wrong,
// which is the single most common form of PDF damage. A later object with the
// same number wins, matching how incremental updates are meant to be applied.
func (r *Reader) reconstruct() {
	for _, match := range objectHeaderPattern.FindAllSubmatchIndex(r.data, -1) {
		number, err := strconv.Atoi(string(r.data[match[2]:match[3]]))
		if err != nil || number <= 0 || number > maxObjects {
			continue
		}
		r.locations[number] = location{offset: int64(match[0])}
	}
	// The trailer dictionary may also only exist in the file body.
	for _, keyword := range [][]byte{[]byte("trailer")} {
		offset := bytes.LastIndex(r.data, keyword)
		for offset >= 0 {
			lexer := newLexer(r.data)
			lexer.pos = offset + len(keyword)
			if dict, err := lexer.parseObject(0, true); err == nil {
				if trailer, ok := dict.(Dict); ok {
					r.mergeTrailer(trailer)
					break
				}
			}
			offset = bytes.LastIndex(r.data[:offset], keyword)
		}
	}
}

// mergeTrailer keeps the first value seen for each key, so the newest trailer
// in an incrementally updated file wins.
func (r *Reader) mergeTrailer(trailer Dict) {
	for key, value := range trailer {
		if _, present := r.trailer[key]; !present {
			r.trailer[key] = value
		}
	}
}

// readXrefChain follows startxref and every /Prev pointer.
func (r *Reader) readXrefChain() {
	offset, ok := r.startxref()
	if !ok {
		return
	}
	seen := map[int64]bool{}
	for sections := 0; sections < maxXrefSections; sections++ {
		if offset <= 0 || offset >= int64(len(r.data)) || seen[offset] {
			return
		}
		seen[offset] = true

		trailer, ok := r.readXrefSection(offset)
		if !ok {
			return
		}
		r.mergeTrailer(trailer)

		// A hybrid-reference file keeps some entries in a stream that
		// table-only readers ignore; Herald reads both.
		if hybrid, ok := toInt(trailer["XRefStm"]); ok && !seen[int64(hybrid)] {
			seen[int64(hybrid)] = true
			if stmTrailer, ok := r.readXrefSection(int64(hybrid)); ok {
				r.mergeTrailer(stmTrailer)
			}
		}
		previous, ok := toInt(trailer["Prev"])
		if !ok {
			return
		}
		offset = int64(previous)
	}
}

func (r *Reader) startxref() (int64, bool) {
	tail := r.data
	if len(tail) > 2048 {
		tail = tail[len(tail)-2048:]
	}
	index := bytes.LastIndex(tail, []byte("startxref"))
	if index < 0 {
		return 0, false
	}
	lexer := newLexer(tail)
	lexer.pos = index + len("startxref")
	t := lexer.next()
	if t.kind != tokNumber {
		return 0, false
	}
	return int64(t.num), true
}

// readXrefSection reads either a classic table or a cross-reference stream and
// returns the trailer that accompanies it.
func (r *Reader) readXrefSection(offset int64) (Dict, bool) {
	lexer := newLexer(r.data)
	lexer.pos = int(offset)
	lexer.skipSpace()

	if bytes.HasPrefix(r.data[lexer.pos:], []byte("xref")) {
		lexer.pos += len("xref")
		return r.readXrefTable(lexer)
	}
	// Otherwise this must be "N G obj" introducing a cross-reference stream.
	object, _, ok := r.parseIndirectAt(int(offset), false)
	if !ok {
		return nil, false
	}
	stream, ok := object.(*Stream)
	if !ok {
		return nil, false
	}
	if !r.readXrefStream(stream) {
		return nil, false
	}
	return stream.Dict, true
}

func (r *Reader) readXrefTable(lexer *lexer) (Dict, bool) {
	for {
		lexer.skipSpace()
		if bytes.HasPrefix(r.data[lexer.pos:], []byte("trailer")) {
			lexer.pos += len("trailer")
			trailer, err := lexer.parseObject(0, true)
			if err != nil {
				return nil, false
			}
			dict, ok := trailer.(Dict)
			if !ok {
				return nil, false
			}
			return dict, true
		}
		start := lexer.next()
		if start.kind != tokNumber {
			// No trailer keyword: the section is still usable if entries were
			// read, which happens in files that end abruptly.
			return Dict{}, true
		}
		count := lexer.next()
		if count.kind != tokNumber {
			return Dict{}, true
		}
		first, entries := int(start.num), int(count.num)
		if entries < 0 || entries > maxObjects {
			return Dict{}, true
		}
		for index := range entries {
			lexer.skipSpace()
			offsetToken := lexer.next()
			generationToken := lexer.next()
			kindToken := lexer.next()
			if offsetToken.kind != tokNumber || generationToken.kind != tokNumber {
				return Dict{}, true
			}
			inUse := kindToken.kind == tokKeyword && len(kindToken.text) > 0 && kindToken.text[0] == 'n'
			number := first + index
			if !inUse || number <= 0 || number > maxObjects {
				continue
			}
			// Entries from the newest section win, and the newest section is
			// read first, so an existing entry is never replaced.
			if _, present := r.locations[number]; present && r.fromTable(number) {
				continue
			}
			r.locations[number] = location{offset: int64(offsetToken.num)}
			r.tableEntries(number)
		}
	}
}

// tableEntries and fromTable record which numbers came from an authoritative
// cross-reference section rather than the reconstruction scan.
func (r *Reader) tableEntries(number int) {
	if r.authoritative == nil {
		r.authoritative = map[int]bool{}
	}
	r.authoritative[number] = true
}

func (r *Reader) fromTable(number int) bool { return r.authoritative[number] }

func (r *Reader) readXrefStream(stream *Stream) bool {
	data, err := r.Data(stream)
	if err != nil {
		return false
	}
	widths, ok := r.Resolve(stream.Dict["W"]).(Array)
	if !ok || len(widths) < 3 {
		return false
	}
	fieldWidths := make([]int, len(widths))
	rowLength := 0
	for index, item := range widths {
		width, _ := toInt(r.Resolve(item))
		if width < 0 || width > 8 {
			return false
		}
		fieldWidths[index] = width
		rowLength += width
	}
	if rowLength == 0 {
		return false
	}

	size, _ := toInt(r.Resolve(stream.Dict["Size"]))
	var ranges []int
	if index, ok := r.Resolve(stream.Dict["Index"]).(Array); ok {
		for _, item := range index {
			value, _ := toInt(r.Resolve(item))
			ranges = append(ranges, value)
		}
	}
	if len(ranges) < 2 {
		ranges = []int{0, size}
	}

	position := 0
	for pair := 0; pair+1 < len(ranges); pair += 2 {
		first, count := ranges[pair], ranges[pair+1]
		if count < 0 || count > maxObjects {
			return false
		}
		for index := range count {
			if position+rowLength > len(data) {
				return true
			}
			fields := make([]int64, len(fieldWidths))
			for field, width := range fieldWidths {
				value := int64(0)
				for range width {
					value = value<<8 | int64(data[position])
					position++
				}
				fields[field] = value
			}
			// A zero-width type field means type 1, per the specification.
			entryType := int64(1)
			if fieldWidths[0] > 0 {
				entryType = fields[0]
			}
			number := first + index
			if number <= 0 || number > maxObjects {
				continue
			}
			if _, present := r.locations[number]; present && r.fromTable(number) {
				continue
			}
			switch entryType {
			case 1:
				r.locations[number] = location{offset: fields[1]}
				r.tableEntries(number)
			case 2:
				r.locations[number] = location{
					inStream: true, streamNum: int(fields[1]), streamSlot: int(fields[2]),
				}
				r.tableEntries(number)
			}
		}
	}
	return true
}

// parseIndirectAt reads the "N G obj ... endobj" at a byte offset.
func (r *Reader) parseIndirectAt(offset int, decrypt bool) (Object, int, bool) {
	if offset < 0 || offset >= len(r.data) {
		return nil, 0, false
	}
	lexer := newLexer(r.data)
	lexer.pos = offset

	numberToken := lexer.next()
	generationToken := lexer.next()
	objToken := lexer.next()
	if numberToken.kind != tokNumber || generationToken.kind != tokNumber ||
		objToken.kind != tokKeyword || string(objToken.text) != "obj" {
		return nil, 0, false
	}

	previousNumber, previousGen := r.currentNumber, r.currentGen
	if decrypt {
		r.currentNumber, r.currentGen = int(numberToken.num), int(generationToken.num)
	}
	defer func() { r.currentNumber, r.currentGen = previousNumber, previousGen }()

	object, err := lexer.parseObject(0, true)
	if err != nil {
		return nil, 0, false
	}
	if decrypt {
		object = r.decryptObject(object, 0)
	}

	// A dictionary immediately followed by "stream" is a stream object.
	if dict, ok := object.(Dict); ok {
		saved := lexer.pos
		next := lexer.next()
		if next.kind == tokKeyword && string(next.text) == "stream" {
			return r.readStreamBody(dict, lexer, decrypt), int(numberToken.num), true
		}
		lexer.pos = saved
		lexer.pushed = nil
	}
	return object, int(numberToken.num), true
}

// readStreamBody extracts a stream's bytes, trusting /Length only when it
// lands on an "endstream" keyword.
func (r *Reader) readStreamBody(dict Dict, lexer *lexer, decrypt bool) *Stream {
	position := lexer.pos
	// The keyword is followed by CRLF or LF, never CR alone.
	if position < len(r.data) && r.data[position] == '\r' {
		position++
	}
	if position < len(r.data) && r.data[position] == '\n' {
		position++
	}

	end := -1
	if length, ok := toInt(r.Resolve(dict["Length"])); ok && length >= 0 && position+length <= len(r.data) {
		candidate := position + length
		trailing := r.data[candidate:min(candidate+32, len(r.data))]
		if bytes.Contains(trailing, []byte("endstream")) {
			end = candidate
		}
	}
	if end < 0 {
		// A wrong or indirect /Length is common; locating the terminator
		// directly recovers the stream.
		relative := bytes.Index(r.data[position:], []byte("endstream"))
		if relative < 0 {
			end = len(r.data)
		} else {
			end = position + relative
			// Trim the end-of-line that precedes the keyword.
			for end > position && (r.data[end-1] == '\n' || r.data[end-1] == '\r') {
				end--
				if end > position && r.data[end] == '\n' && r.data[end-1] == '\r' {
					end--
				}
				break
			}
		}
	}

	raw := r.data[position:end]
	if decrypt && r.decryptor != nil && !r.decryptor.skipStream(r, dict) {
		raw = r.decryptor.decrypt(raw, r.currentNumber, r.currentGen)
	}
	return &Stream{Dict: dict, Raw: raw}
}

// Resolve follows indirect references until it reaches a direct object.
func (r *Reader) Resolve(object Object) Object {
	return r.resolveDepth(object, 0)
}

func (r *Reader) resolveDepth(object Object, depth int) Object {
	if depth > maxResolveDepth {
		return nil
	}
	ref, ok := object.(Ref)
	if !ok {
		return object
	}
	if cached, present := r.cache[ref.Number]; present {
		return cached
	}
	// A reference cycle would otherwise recurse until the stack is exhausted.
	if r.loading[ref.Number] {
		return nil
	}
	r.loading[ref.Number] = true
	defer delete(r.loading, ref.Number)

	value := r.loadObject(ref.Number)
	if inner, ok := value.(Ref); ok {
		value = r.resolveDepth(inner, depth+1)
	}
	r.cache[ref.Number] = value
	return value
}

func (r *Reader) loadObject(number int) Object {
	place, ok := r.locations[number]
	if !ok {
		return nil
	}
	if !place.inStream {
		object, parsedNumber, ok := r.parseIndirectAt(int(place.offset), true)
		if !ok {
			return nil
		}
		// An offset that lands on a different object means the table is wrong;
		// the reconstruction scan is then more trustworthy.
		if parsedNumber != number {
			return nil
		}
		return object
	}
	objects := r.objectStream(place.streamNum)
	if place.streamSlot < 0 || place.streamSlot >= len(objects) {
		return nil
	}
	return objects[place.streamSlot]
}

// objectStream parses and caches the contents of one compressed object stream.
func (r *Reader) objectStream(number int) []Object {
	if objects, ok := r.objStreams[number]; ok {
		return objects
	}
	// Record an empty result first: an object stream that references itself
	// would otherwise recurse forever.
	r.objStreams[number] = nil

	stream, ok := r.Resolve(Ref{Number: number}).(*Stream)
	if !ok {
		return nil
	}
	data, err := r.Data(stream)
	if err != nil {
		return nil
	}
	count, _ := toInt(r.Resolve(stream.Dict["N"]))
	first, _ := toInt(r.Resolve(stream.Dict["First"]))
	if count <= 0 || count > maxObjects || first < 0 || first > len(data) {
		return nil
	}

	header := newLexer(data[:first])
	offsets := make([]int, 0, count)
	for range count {
		numberToken := header.next()
		offsetToken := header.next()
		if numberToken.kind != tokNumber || offsetToken.kind != tokNumber {
			break
		}
		offsets = append(offsets, first+int(offsetToken.num))
	}

	objects := make([]Object, len(offsets))
	for index, offset := range offsets {
		if offset < 0 || offset > len(data) {
			continue
		}
		body := newLexer(data)
		body.pos = offset
		// Objects inside an object stream were decrypted with the stream, so
		// they are parsed without a second decryption pass.
		if object, err := body.parseObject(0, true); err == nil {
			objects[index] = object
		}
	}
	r.objStreams[number] = objects
	return objects
}

// GetDict resolves a value and returns it as a dictionary.
func (r *Reader) GetDict(object Object) (Dict, bool) {
	switch value := r.Resolve(object).(type) {
	case Dict:
		return value, true
	case *Stream:
		return value.Dict, true
	}
	return nil, false
}

// GetArray resolves a value and returns it as an array. A single object is
// promoted to a one-element array, which several PDF keys allow.
func (r *Reader) GetArray(object Object) (Array, bool) {
	switch value := r.Resolve(object).(type) {
	case Array:
		return value, true
	case nil:
		return nil, false
	default:
		return Array{value}, true
	}
}

// GetName resolves a value and returns it as a name.
func (r *Reader) GetName(object Object) (Name, bool) {
	name, ok := r.Resolve(object).(Name)
	return name, ok
}

// GetNumber resolves a value and returns it as a float.
func (r *Reader) GetNumber(object Object) (float64, bool) {
	return toFloat(r.Resolve(object))
}

// GetInt resolves a value and returns it as an int.
func (r *Reader) GetInt(object Object) (int, bool) {
	return toInt(r.Resolve(object))
}

// GetStream resolves a value and returns it as a stream.
func (r *Reader) GetStream(object Object) (*Stream, bool) {
	stream, ok := r.Resolve(object).(*Stream)
	return stream, ok
}
