// Package relevance ranks entries against a user's profile.
//
// Scoring prefers batched local embeddings from Ollama and always has a
// deterministic TF-IDF fallback, so the queue works with no model installed and
// with no hosted service. Below-threshold entries are bucketed as filtered and
// remain recoverable; nothing is ever discarded by ranking.
package relevance

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/store"
)

// Default interests for a fresh installation.
var (
	PaperInterests = []string{
		"chip design and computer architecture",
		"digital circuits and hardware design",
		"machine learning",
		"reinforcement learning",
		"operating systems and systems research",
	}
	NewsInterests = []string{
		"new processors GPUs accelerators and chip architectures",
		"machine learning model launches and research releases",
		"developer platforms operating systems and infrastructure",
	}
)

// DefaultProfiles maps each workspace to its starting interests.
var DefaultProfiles = map[string][]string{
	"paper": PaperInterests,
	"news":  NewsInterests,
}

// selectivityPercentiles set where the cold-start threshold sits in the
// observed score distribution.
var selectivityPercentiles = map[string]float64{
	"broad": 0.70, "balanced": 0.90, "focused": 0.95,
}

var tokenPattern = regexp.MustCompile(`[a-z0-9]+(?:[-'][a-z0-9]+)?`)

var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "into": true,
	"is": true, "it": true, "of": true, "on": true, "or": true, "that": true,
	"the": true, "their": true, "this": true, "to": true, "using": true,
	"we": true, "with": true,
}

// tokens lowercases and filters a text into scoring terms.
func tokens(text string) []string {
	var result []string
	for _, token := range tokenPattern.FindAllString(strings.ToLower(text), -1) {
		if !stopWords[token] && len([]rune(token)) > 1 {
			result = append(result, token)
		}
	}
	return result
}

// sparseVector is a term-weight map with unit length.
type sparseVector map[string]float64

// tfidfVectors builds L2-normalized TF-IDF vectors over the given texts.
//
// This is the always-available scorer. Its cosine similarities are naturally
// lower than dense embeddings', which is precisely why the threshold is
// calibrated to the observed distribution rather than to a fixed floor.
func tfidfVectors(texts []string) []sparseVector {
	documents := make([][]string, len(texts))
	frequencies := make([]map[string]float64, len(texts))
	documentFrequency := map[string]float64{}
	for index, text := range texts {
		documents[index] = tokens(text)
		frequency := map[string]float64{}
		for _, term := range documents[index] {
			frequency[term]++
		}
		frequencies[index] = frequency
		for term := range frequency {
			documentFrequency[term]++
		}
	}
	count := float64(max(1, len(documents)))
	vectors := make([]sparseVector, len(texts))
	for index, frequency := range frequencies {
		vector := sparseVector{}
		for term, value := range frequency {
			vector[term] = (1.0 + math.Log(value)) *
				(math.Log((1.0+count)/(1.0+documentFrequency[term])) + 1.0)
		}
		norm := 0.0
		for _, value := range vector {
			norm += value * value
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			norm = 1.0
		}
		for term := range vector {
			vector[term] /= norm
		}
		vectors[index] = vector
	}
	return vectors
}

// sparseCosine is the dot product of two unit-length sparse vectors.
func sparseCosine(left, right sparseVector) float64 {
	if len(left) > len(right) {
		left, right = right, left
	}
	total := 0.0
	for term, value := range left {
		total += value * right[term]
	}
	return total
}

// denseCosine is the cosine similarity of two embedding vectors.
func denseCosine(left, right []float64) float64 {
	if len(left) != len(right) || len(left) == 0 {
		return 0.0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += left[index] * right[index]
		leftNorm += left[index] * left[index]
		rightNorm += right[index] * right[index]
	}
	leftNorm, rightNorm = math.Sqrt(leftNorm), math.Sqrt(rightNorm)
	if leftNorm == 0 || rightNorm == 0 {
		return 0.0
	}
	return dot / (leftNorm * rightNorm)
}

// percentile returns the value at the given fraction of a distribution.
func percentile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 50.0
	}
	ordered := append([]float64{}, values...)
	sort.Float64s(ordered)
	index := int(math.Ceil(fraction*float64(len(ordered)))) - 1
	if index < 0 {
		index = 0
	}
	if index > len(ordered)-1 {
		index = len(ordered) - 1
	}
	return ordered[index]
}

// entryText is the text scored for one entry.
//
// The title is repeated deliberately: that is the product's 2x title weighting.
// Source title and category are appended so a topical feed is recognized even
// when an individual headline is terse.
func entryText(entry *store.Entry) string {
	var context []string
	if title := strings.TrimSpace(entry.SourceTitle); title != "" {
		context = append(context, title)
	}
	if category := strings.TrimSpace(entry.SourceCategory); category != "" {
		context = append(context, category)
	}
	return strings.TrimSpace(
		entry.Title + "\n" + entry.Title + "\n" + entry.Content + "\n" +
			strings.Join(context, " "))
}

// lexicalMatch scores literal interest-token overlap, weighting the title
// twice, and reports which interests matched.
func lexicalMatch(entry *store.Entry, interests []string) (float64, []string) {
	titleTokens := map[string]bool{}
	for _, token := range tokens(entry.Title) {
		titleTokens[token] = true
	}
	contentTokens := map[string]bool{}
	for _, token := range tokens(entry.Content) {
		contentTokens[token] = true
	}
	best := 0.0
	matched := []string{}
	for _, interest := range interests {
		interestTokens := uniqueTokens(interest)
		if len(interestTokens) == 0 {
			continue
		}
		overlap := 0
		for _, token := range interestTokens {
			switch {
			case titleTokens[token]:
				overlap += 2
			case contentTokens[token]:
				overlap++
			}
		}
		ratio := math.Min(1.0, float64(overlap)/(2.0*float64(len(interestTokens))))
		if ratio > 0 {
			matched = append(matched, interest)
		}
		best = math.Max(best, ratio)
	}
	return best, matched
}

// uniqueTokens returns an interest's distinct tokens, order-independent.
func uniqueTokens(text string) []string {
	seen := map[string]bool{}
	var result []string
	for _, token := range tokens(text) {
		if !seen[token] {
			seen[token] = true
			result = append(result, token)
		}
	}
	return result
}

// round4 matches the 4-decimal rounding used for stored component scores.
func round4(value float64) float64 { return roundTo(value, 4) }

func roundTo(value float64, places int) float64 {
	factor := math.Pow(10, float64(places))
	rounded := math.Round(value*factor) / factor
	if rounded == 0 {
		// Avoid emitting negative zero into stored JSON.
		return 0
	}
	return rounded
}
