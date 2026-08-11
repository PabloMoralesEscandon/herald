package urlx

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Pair is one decoded query-string parameter, preserving document order.
type Pair struct {
	Key   string
	Value string
}

// ParseQuery decodes a query string the way parse_qsl(qs, keep_blank_values=True)
// does. Order is preserved because callers sort explicitly.
//
// net/url.ParseQuery is unsuitable: it returns an unordered map and rejects
// inputs that parse_qsl accepts.
func ParseQuery(query string) []Pair {
	var pairs []Pair
	for _, field := range strings.Split(query, "&") {
		if field == "" {
			continue
		}
		name, value, found := strings.Cut(field, "=")
		if !found {
			value = ""
		}
		pairs = append(pairs, Pair{
			Key:   Unquote(strings.ReplaceAll(name, "+", " ")),
			Value: Unquote(strings.ReplaceAll(value, "+", " ")),
		})
	}
	return pairs
}

// EncodeQuery re-encodes pairs the way urlencode(pairs, doseq=True) does, using
// quote_plus with an empty safe set.
func EncodeQuery(pairs []Pair) string {
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, QuotePlus(pair.Key)+"="+QuotePlus(pair.Value))
	}
	return strings.Join(parts, "&")
}

// SortPairs orders parameters by case-folded key then raw value, which is how
// Herald makes two spellings of the same link canonicalize identically.
func SortPairs(pairs []Pair) {
	sort.SliceStable(pairs, func(left, right int) bool {
		leftKey := strings.ToLower(pairs[left].Key)
		rightKey := strings.ToLower(pairs[right].Key)
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		return pairs[left].Value < pairs[right].Value
	})
}

// QuotePlus percent-encodes value with an empty safe set, encoding space as
// '+', matching urllib.parse.quote_plus.
func QuotePlus(value string) string {
	if !strings.Contains(value, " ") {
		return Quote(value, "")
	}
	return strings.ReplaceAll(Quote(value, " "), " ", "+")
}

// Unquote decodes %XX escapes as UTF-8, matching urllib.parse.unquote with the
// default errors="replace". A '%' not followed by two hex digits is literal.
func Unquote(value string) string {
	if !strings.Contains(value, "%") {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] != '%' {
			out.WriteByte(value[index])
			index++
			continue
		}
		// Collect the maximal run of %XX escapes so multi-byte UTF-8
		// sequences decode as one code point.
		var decoded []byte
		for index+2 < len(value) && value[index] == '%' {
			high, highOK := hexValue(value[index+1])
			low, lowOK := hexValue(value[index+2])
			if !highOK || !lowOK {
				break
			}
			decoded = append(decoded, high<<4|low)
			index += 3
		}
		if len(decoded) == 0 {
			out.WriteByte(value[index])
			index++
			continue
		}
		writeReplacingInvalid(&out, decoded)
	}
	return out.String()
}

// writeReplacingInvalid writes bytes as UTF-8, substituting U+FFFD for each
// invalid byte the way Python's errors="replace" does.
func writeReplacingInvalid(out *strings.Builder, decoded []byte) {
	for len(decoded) > 0 {
		r, size := utf8.DecodeRune(decoded)
		if r == utf8.RuneError && size <= 1 {
			out.WriteRune(utf8.RuneError)
			decoded = decoded[1:]
			continue
		}
		out.Write(decoded[:size])
		decoded = decoded[size:]
	}
}

func hexValue(char byte) (byte, bool) {
	switch {
	case char >= '0' && char <= '9':
		return char - '0', true
	case char >= 'a' && char <= 'f':
		return char - 'a' + 10, true
	case char >= 'A' && char <= 'F':
		return char - 'A' + 10, true
	}
	return 0, false
}
