// Package summary produces article summaries, preferring a local Ollama model
// and always falling back to a deterministic extractive summary.
//
// Herald never makes a paid or hosted AI request: when Ollama is absent the
// fallback runs, and the provenance of every summary is recorded so the two are
// never confused.
package summary

import (
	"regexp"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
)

// Default endpoints and limits.
const (
	DefaultOllamaURL     = "http://127.0.0.1:11434"
	DefaultOllamaModel   = "qwen2.5:3b"
	MaxInputCharacters   = 12_000
	MaxSummaryCharacters = 2_000
	defaultMaximum       = 600
)

// Result is a summary together with its provenance.
type Result struct {
	Text           string
	Provider       string
	Model          string
	FallbackReason string
}

// Provider produces a summary for one article.
type Provider interface {
	Summarize(title, content string) Result
}

var abstractPattern = regexp.MustCompile(`(?i)\bAbstract:` + textx.SpaceClass + `*`)

// Deterministic produces the same concise extractive summary for the same
// input. It is the offline fallback and never contacts a network service.
func Deterministic(title, content string) string {
	return deterministicWithLimit(title, content, defaultMaximum)
}

func deterministicWithLimit(title, content string, maximum int) string {
	text := textx.CollapseStrip(content)
	// An explicit "Abstract:" marker near the start wins over the lead text.
	if match := abstractPattern.FindStringIndex(text); match != nil {
		if runeLen(text[:match[0]]) < 160 && match[1] < len(text) {
			text = text[match[1]:]
		}
	}
	if text == "" {
		text = textx.CollapseStrip(title)
	}
	if text == "" {
		return "No summary is available."
	}

	sentences := splitSentences(text)
	var selected []string
	for _, sentence := range sentences {
		candidate := strings.Join(append(append([]string{}, selected...), sentence), " ")
		if len(selected) > 0 && runeLen(candidate) > maximum {
			break
		}
		selected = append(selected, sentence)
		if len(selected) == 3 || runeLen(candidate) >= maximum {
			break
		}
	}
	return trimToWord(strings.Join(selected, " "), maximum)
}

// splitSentences reproduces the split on r"(?<=[.!?])\s+(?=[A-Z0-9])".
//
// Go's regexp engine has no lookaround, so the boundary condition is applied
// directly: a whitespace run preceded by sentence-ending punctuation and
// followed by an ASCII capital or digit.
func splitSentences(text string) []string {
	runes := []rune(text)
	var sentences []string
	start := 0
	for index := 0; index < len(runes); index++ {
		if !textx.IsSpace(runes[index]) {
			continue
		}
		if index == 0 || !isSentenceEnd(runes[index-1]) {
			continue
		}
		end := index
		for end < len(runes) && textx.IsSpace(runes[end]) {
			end++
		}
		if end >= len(runes) || !isSentenceStart(runes[end]) {
			continue
		}
		sentences = append(sentences, string(runes[start:index]))
		start = end
		index = end - 1
	}
	sentences = append(sentences, string(runes[start:]))
	return sentences
}

func isSentenceEnd(r rune) bool { return r == '.' || r == '!' || r == '?' }

// isSentenceStart matches [A-Z0-9] exactly: ASCII only, as in the original.
func isSentenceStart(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// trimToWord shortens to a word boundary and marks the elision.
//
// All limits are measured in characters, not bytes, so multi-byte titles are
// not truncated mid-rune or cut shorter than intended.
func trimToWord(value string, maximum int) string {
	if runeLen(value) <= maximum {
		return value
	}
	runes := []rune(value)
	head := string(runes[:maximum-1])
	shortened := head
	if cut := strings.LastIndex(head, " "); cut >= 0 {
		shortened = head[:cut]
	}
	shortened = strings.TrimRight(shortened, " ,;:-")
	if shortened == "" {
		shortened = head
	}
	return strings.TrimRightFunc(shortened, textx.IsSpace) + "…"
}

func runeLen(value string) int { return len([]rune(value)) }
