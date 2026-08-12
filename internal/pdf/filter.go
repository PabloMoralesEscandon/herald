package pdf

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"io"
)

// maxDecodedStream bounds one decoded stream. A compressed stream can expand
// enormously, so the limit is applied while decoding rather than after it.
const maxDecodedStream = 64 << 20

// Data returns the stream's decoded bytes, applying every filter in order.
//
// Image filters (DCT, JPX, CCITT, JBIG2) are left encoded and reported as
// unsupported: this package reads text, and an image stream is never text.
func (r *Reader) Data(stream *Stream) ([]byte, error) {
	if stream == nil {
		return nil, errorf("PDF stream is missing")
	}
	if stream.decodeDone {
		return stream.decoded, stream.decodeErr
	}
	stream.decodeDone = true
	stream.decoded, stream.decodeErr = r.decodeStream(stream)
	return stream.decoded, stream.decodeErr
}

func (r *Reader) decodeStream(stream *Stream) ([]byte, error) {
	filters := r.filterNames(stream)
	parameters := r.decodeParameters(stream, len(filters))

	data := stream.Raw
	for index, filter := range filters {
		var err error
		var params Dict
		if index < len(parameters) {
			params = parameters[index]
		}
		switch filter {
		case "FlateDecode", "Fl":
			data, err = flateDecode(data)
		case "LZWDecode", "LZW":
			early := 1
			if params != nil {
				if value, ok := toInt(r.Resolve(params["EarlyChange"])); ok {
					early = value
				}
			}
			data, err = lzwDecode(data, early != 0)
		case "ASCIIHexDecode", "AHx":
			data, err = asciiHexDecode(data)
		case "ASCII85Decode", "A85":
			data, err = ascii85Decode(data)
		case "RunLengthDecode", "RL":
			data, err = runLengthDecode(data)
		case "Crypt":
			// Identity crypt filters are the only ones Herald sees; decryption
			// has already happened at the object level.
		default:
			return nil, errorf("PDF stream uses the unsupported %s filter", filter)
		}
		if err != nil {
			return nil, err
		}
		if params != nil {
			if data, err = r.applyPredictor(data, params); err != nil {
				return nil, err
			}
		}
	}
	return data, nil
}

// filterNames normalizes /Filter, which may be one name or an array.
func (r *Reader) filterNames(stream *Stream) []Name {
	value := r.Resolve(stream.Dict["Filter"])
	if value == nil {
		value = r.Resolve(stream.Dict["F"])
		// /F is also the "external file" key. Only trust it when it names a
		// filter rather than a file specification.
		if _, isName := value.(Name); !isName {
			if _, isArray := value.(Array); !isArray {
				return nil
			}
		}
	}
	switch typed := value.(type) {
	case Name:
		return []Name{typed}
	case Array:
		var names []Name
		for _, item := range typed {
			if name, ok := r.Resolve(item).(Name); ok {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}

// decodeParameters normalizes /DecodeParms into one entry per filter.
func (r *Reader) decodeParameters(stream *Stream, count int) []Dict {
	value := r.Resolve(stream.Dict["DecodeParms"])
	if value == nil {
		value = r.Resolve(stream.Dict["DP"])
	}
	parameters := make([]Dict, count)
	switch typed := value.(type) {
	case Dict:
		if count > 0 {
			parameters[0] = typed
		}
	case Array:
		for index, item := range typed {
			if index >= count {
				break
			}
			if dict, ok := r.Resolve(item).(Dict); ok {
				parameters[index] = dict
			}
		}
	}
	return parameters
}

func flateDecode(data []byte) ([]byte, error) {
	// Some producers pad the stream with leading whitespace before the zlib
	// header, which the reader would reject.
	start := 0
	for start < len(data) && isWhitespace(data[start]) {
		start++
	}
	data = data[start:]
	if len(data) == 0 {
		return nil, nil
	}

	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		// A missing zlib wrapper is common enough in the wild to be worth a
		// second attempt as a raw deflate stream.
		if raw, rawErr := inflateRaw(data); rawErr == nil {
			return raw, nil
		}
		return nil, errorf("PDF stream could not be decompressed")
	}
	defer reader.Close()
	out, err := readLimited(reader)
	if err != nil && len(out) == 0 {
		if raw, rawErr := inflateRaw(data); rawErr == nil {
			return raw, nil
		}
		return nil, errorf("PDF stream could not be decompressed")
	}
	// A truncated stream still yields everything before the damage, which is
	// usually the whole page.
	return out, nil
}

// inflateRaw decompresses a deflate stream that carries no zlib header.
func inflateRaw(data []byte) ([]byte, error) {
	reader := flate.NewReader(bytes.NewReader(data))
	defer reader.Close()
	out, err := readLimited(reader)
	if len(out) == 0 && err != nil {
		return nil, err
	}
	return out, nil
}

// readLimited reads at most maxDecodedStream bytes, reporting an error when
// the source is larger.
func readLimited(reader io.Reader) ([]byte, error) {
	var buffer bytes.Buffer
	written, err := io.Copy(&buffer, io.LimitReader(reader, maxDecodedStream+1))
	if written > maxDecodedStream {
		return nil, errorf("PDF stream expands beyond Herald's size limit")
	}
	return buffer.Bytes(), err
}

func asciiHexDecode(data []byte) ([]byte, error) {
	var out []byte
	var current int
	haveHigh := false
	for _, b := range data {
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
			continue
		}
		current, haveHigh = value, true
	}
	if haveHigh {
		out = append(out, byte(current<<4))
	}
	return out, nil
}

func ascii85Decode(data []byte) ([]byte, error) {
	var out []byte
	var group [5]byte
	count := 0
	// The <~ introducer is optional; the ~> terminator ends the stream.
	if bytes.HasPrefix(bytes.TrimLeft(data, "\x00\t\n\f\r "), []byte("<~")) {
		data = data[bytes.Index(data, []byte("<~"))+2:]
	}
	for index := 0; index < len(data); index++ {
		b := data[index]
		switch {
		case isWhitespace(b):
			continue
		case b == '~':
			index = len(data)
		case b == 'z' && count == 0:
			out = append(out, 0, 0, 0, 0)
			continue
		case b < '!' || b > 'u':
			continue
		default:
			group[count] = b - '!'
			count++
			if count == 5 {
				out = appendBase85(out, group, 5)
				count = 0
			}
			continue
		}
		break
	}
	if count > 0 {
		// A partial group is padded with the highest digit and yields one
		// fewer byte than digits supplied.
		for index := count; index < 5; index++ {
			group[index] = 84
		}
		out = appendBase85(out, group, count)
	}
	if len(out) > maxDecodedStream {
		return nil, errorf("PDF stream expands beyond Herald's size limit")
	}
	return out, nil
}

func appendBase85(out []byte, group [5]byte, count int) []byte {
	value := uint32(0)
	for _, digit := range group {
		value = value*85 + uint32(digit)
	}
	full := [4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
	if count == 5 {
		return append(out, full[:]...)
	}
	return append(out, full[:count-1]...)
}

func runLengthDecode(data []byte) ([]byte, error) {
	var out []byte
	for index := 0; index < len(data); {
		length := int(data[index])
		index++
		switch {
		case length == 128:
			return out, nil
		case length < 128:
			end := index + length + 1
			if end > len(data) {
				end = len(data)
			}
			out = append(out, data[index:end]...)
			index = end
		default:
			if index >= len(data) {
				return out, nil
			}
			for range 257 - length {
				out = append(out, data[index])
			}
			index++
		}
		if len(out) > maxDecodedStream {
			return nil, errorf("PDF stream expands beyond Herald's size limit")
		}
	}
	return out, nil
}

// applyPredictor undoes the PNG or TIFF prediction applied before compression.
//
// Cross-reference streams almost always use PNG prediction, so a document
// cannot be read at all without this step.
func (r *Reader) applyPredictor(data []byte, params Dict) ([]byte, error) {
	predictor, ok := toInt(r.Resolve(params["Predictor"]))
	if !ok || predictor <= 1 {
		return data, nil
	}
	colors := 1
	if value, ok := toInt(r.Resolve(params["Colors"])); ok && value > 0 {
		colors = value
	}
	bitsPerComponent := 8
	if value, ok := toInt(r.Resolve(params["BitsPerComponent"])); ok && value > 0 {
		bitsPerComponent = value
	}
	columns := 1
	if value, ok := toInt(r.Resolve(params["Columns"])); ok && value > 0 {
		columns = value
	}
	if colors > 64 || bitsPerComponent > 32 || columns > 1<<20 {
		return nil, errorf("PDF predictor parameters are out of range")
	}

	bitsPerPixel := colors * bitsPerComponent
	bytesPerPixel := max((bitsPerPixel+7)/8, 1)
	rowLength := (columns*bitsPerPixel + 7) / 8

	if predictor == 2 {
		return tiffPredictor(data, colors, bitsPerComponent, columns), nil
	}

	// PNG prediction prefixes every row with a filter-type byte.
	var out []byte
	previous := make([]byte, rowLength)
	current := make([]byte, rowLength)
	for offset := 0; offset+1 <= len(data); offset += rowLength + 1 {
		filterType := data[offset]
		end := offset + 1 + rowLength
		if end > len(data) {
			end = len(data)
		}
		row := data[offset+1 : end]
		clear(current)
		copy(current, row)

		switch filterType {
		case 0: // None
		case 1: // Sub
			for index := bytesPerPixel; index < rowLength; index++ {
				current[index] += current[index-bytesPerPixel]
			}
		case 2: // Up
			for index := range rowLength {
				current[index] += previous[index]
			}
		case 3: // Average
			for index := range rowLength {
				left := 0
				if index >= bytesPerPixel {
					left = int(current[index-bytesPerPixel])
				}
				current[index] += byte((left + int(previous[index])) / 2)
			}
		case 4: // Paeth
			for index := range rowLength {
				var left, upperLeft byte
				if index >= bytesPerPixel {
					left = current[index-bytesPerPixel]
					upperLeft = previous[index-bytesPerPixel]
				}
				current[index] += paeth(left, previous[index], upperLeft)
			}
		default:
			return nil, errorf("PDF row uses an unknown PNG predictor")
		}
		out = append(out, current...)
		copy(previous, current)
	}
	return out, nil
}

func paeth(left, above, upperLeft byte) byte {
	prediction := int(left) + int(above) - int(upperLeft)
	distanceLeft := abs(prediction - int(left))
	distanceAbove := abs(prediction - int(above))
	distanceUpperLeft := abs(prediction - int(upperLeft))
	if distanceLeft <= distanceAbove && distanceLeft <= distanceUpperLeft {
		return left
	}
	if distanceAbove <= distanceUpperLeft {
		return above
	}
	return upperLeft
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// tiffPredictor undoes horizontal differencing. Only the 8-bit case appears in
// practice; other depths are returned unchanged rather than corrupted.
func tiffPredictor(data []byte, colors, bitsPerComponent, columns int) []byte {
	if bitsPerComponent != 8 {
		return data
	}
	rowLength := columns * colors
	if rowLength <= 0 {
		return data
	}
	for offset := 0; offset+rowLength <= len(data); offset += rowLength {
		row := data[offset : offset+rowLength]
		for index := colors; index < rowLength; index++ {
			row[index] += row[index-colors]
		}
	}
	return data
}

// lzwDecode implements the LZW variant PDF uses.
//
// Go's compress/lzw cannot be reused: PDF defaults to EarlyChange, which
// increases the code width one code sooner than the TIFF and GIF variants the
// standard library implements. Decoding with the wrong timing yields plausible
// but wrong bytes, which is worse than failing.
func lzwDecode(data []byte, earlyChange bool) ([]byte, error) {
	const (
		clearCode = 256
		eodCode   = 257
		maxCode   = 4096
	)
	dictionary := make([][]byte, maxCode)
	var next int
	var codeWidth uint

	reset := func() {
		for index := range 256 {
			dictionary[index] = []byte{byte(index)}
		}
		next = 258
		codeWidth = 9
	}
	reset()

	var out []byte
	var previous []byte
	bitBuffer, bitCount := uint32(0), uint(0)
	early := 0
	if earlyChange {
		early = 1
	}

	for index := 0; ; {
		for bitCount < codeWidth {
			if index >= len(data) {
				return out, nil
			}
			bitBuffer = bitBuffer<<8 | uint32(data[index])
			bitCount += 8
			index++
		}
		code := int(bitBuffer >> (bitCount - codeWidth) & (1<<codeWidth - 1))
		bitCount -= codeWidth

		switch {
		case code == eodCode:
			return out, nil
		case code == clearCode:
			reset()
			previous = nil
			continue
		}

		var entry []byte
		switch {
		case code < next && dictionary[code] != nil:
			entry = dictionary[code]
		case previous != nil:
			// The KwKwK case: the code being defined is used immediately.
			entry = append(append([]byte{}, previous...), previous[0])
		default:
			return out, nil
		}

		out = append(out, entry...)
		if len(out) > maxDecodedStream {
			return nil, errorf("PDF stream expands beyond Herald's size limit")
		}
		if previous != nil && next < maxCode {
			dictionary[next] = append(append([]byte{}, previous...), entry[0])
			next++
		}
		previous = entry

		// The width grows as the dictionary fills. EarlyChange makes each step
		// happen one code before the dictionary actually needs it.
		switch {
		case next+early >= 1<<11:
			codeWidth = 12
		case next+early >= 1<<10:
			codeWidth = 11
		case next+early >= 1<<9:
			codeWidth = 10
		}
	}
}
