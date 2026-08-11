// Package textx supplies whitespace handling that matches the semantics
// Herald's text normalization has always had.
//
// This exists because Go's regexp \s matches only ASCII [\t\n\f\r ], while the
// original implementation's \s matched Unicode whitespace. Feed content is full
// of non-breaking spaces, so the difference is not academic: using Go's default
// would leave U+00A0 embedded in note titles, filenames, and summaries.
package textx

import (
	"regexp"
	"strings"
)

// SpaceClass is a regexp character class equal to Python's Unicode-mode \s.
// It is exported so other packages can compose patterns that need identical
// whitespace semantics.
//
// The set is Py_UNICODE_ISSPACE: C0 whitespace and the information separators
// 0x1C-0x1F, plus NEL, NBSP, and the Unicode space separators. Note that it
// deliberately excludes U+200B ZERO WIDTH SPACE, which Python does not treat as
// whitespace either.
const SpaceClass = `[\t\n\v\f\r\x{1c}-\x{1f} \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var spaceRun = regexp.MustCompile(SpaceClass + `+`)

// IsSpace reports whether r is whitespace under the same rule.
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x1c, 0x1d, 0x1e, 0x1f,
		0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Collapse replaces every run of whitespace with a single space, matching
// re.sub(r"\s+", " ", value).
func Collapse(value string) string {
	return spaceRun.ReplaceAllString(value, " ")
}

// Strip removes leading and trailing whitespace, matching str.strip().
func Strip(value string) string {
	return strings.TrimFunc(value, IsSpace)
}

// CollapseStrip is the very common combination of both, matching
// " ".join(value.split()) and re.sub(r"\s+", " ", value).strip().
func CollapseStrip(value string) string {
	return Strip(Collapse(value))
}

// Fields splits on whitespace runs, matching str.split() with no argument.
func Fields(value string) []string {
	return strings.FieldsFunc(value, IsSpace)
}

// IsLineBoundary reports whether r terminates a line for SplitLines.
func IsLineBoundary(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// SplitLines matches str.splitlines(): it breaks on the full set of Unicode
// line boundaries, treats "\r\n" as one break, and — importantly — returns no
// lines at all for the empty string.
//
// strings.Split(value, "\n") differs on both of the last two points, and the
// empty-string case is the one that bites: it turns an empty block into a
// spurious one-line result.
func SplitLines(value string) []string {
	if value == "" {
		return nil
	}
	var lines []string
	start := 0
	runes := []rune(value)
	for index := 0; index < len(runes); index++ {
		if !IsLineBoundary(runes[index]) {
			continue
		}
		lines = append(lines, string(runes[start:index]))
		if runes[index] == '\r' && index+1 < len(runes) && runes[index+1] == '\n' {
			index++
		}
		start = index + 1
	}
	if start < len(runes) {
		lines = append(lines, string(runes[start:]))
	}
	return lines
}
