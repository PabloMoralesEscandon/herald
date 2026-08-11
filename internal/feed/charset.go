package feed

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// charsetReader decodes the encodings an XML parser is required to support,
// and rejects everything else.
//
// The supported set matches expat's built-ins, so a feed declaring an exotic
// encoding fails to parse rather than being silently misread as UTF-8 — which
// would corrupt titles and store mojibake in the database.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "latin-1", "latin1", "iso8859-1", "iso_8859-1":
		return newLatin1Reader(input)
	case "utf-16", "utf16", "utf-16le", "utf-16be":
		return newUTF16Reader(input)
	}
	return nil, fmt.Errorf("unsupported feed encoding: %s", charset)
}

func newLatin1Reader(input io.Reader) (io.Reader, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Grow(len(raw))
	for _, char := range raw {
		out.WriteRune(rune(char))
	}
	return &out, nil
}

func newUTF16Reader(input io.Reader) (io.Reader, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	if len(raw)%2 != 0 {
		return nil, fmt.Errorf("truncated UTF-16 feed document")
	}
	bigEndian := true
	if len(raw) >= 2 {
		switch {
		case raw[0] == 0xFF && raw[1] == 0xFE:
			bigEndian, raw = false, raw[2:]
		case raw[0] == 0xFE && raw[1] == 0xFF:
			bigEndian, raw = true, raw[2:]
		}
	}
	units := make([]uint16, 0, len(raw)/2)
	for index := 0; index+1 < len(raw); index += 2 {
		if bigEndian {
			units = append(units, uint16(raw[index])<<8|uint16(raw[index+1]))
		} else {
			units = append(units, uint16(raw[index+1])<<8|uint16(raw[index]))
		}
	}
	var out bytes.Buffer
	out.Grow(len(units) * int(utf8.UTFMax))
	for _, char := range utf16.Decode(units) {
		out.WriteRune(char)
	}
	return &out, nil
}
