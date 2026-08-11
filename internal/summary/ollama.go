package summary

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
)

// maxOllamaResponse caps the completion body Herald will read.
const maxOllamaResponse = 1024 * 1024

// Error reports that Ollama could not return a usable completion. It is always
// recoverable: the caller substitutes the deterministic summary.
type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

// Generator turns a prompt into a completion. It is an interface seam so tests
// and the CLI can run entirely offline.
type Generator func(prompt string) (string, error)

// OllamaClient talks to a local Ollama server.
type OllamaClient struct {
	BaseURL string
	Model   string
	Client  *http.Client
}

// NewOllamaClient builds a client with Herald's short timeout: a local model
// that is slow to respond should not stall triage, it should fall back.
func NewOllamaClient(baseURL, model string) *OllamaClient {
	return &OllamaClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		Client:  &http.Client{Timeout: 8 * time.Second},
	}
}

// Generate requests a single deterministic, non-streaming completion.
func (c *OllamaClient) Generate(prompt string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"model":   c.Model,
		"prompt":  prompt,
		"stream":  false,
		"options": map[string]any{"temperature": 0},
	})
	if err != nil {
		return "", &Error{Reason: err.Error()}
	}
	request, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/generate", bytes.NewReader(payload))
	if err != nil {
		return "", &Error{Reason: err.Error()}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.Client.Do(request)
	if err != nil {
		return "", &Error{Reason: err.Error()}
	}
	defer response.Body.Close()

	document, err := io.ReadAll(io.LimitReader(response.Body, maxOllamaResponse+1))
	if err != nil {
		return "", &Error{Reason: err.Error()}
	}
	if len(document) > maxOllamaResponse {
		return "", &Error{Reason: "Ollama response exceeds the 1 MiB limit"}
	}
	var decoded struct {
		Response *string `json:"response"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil || decoded.Response == nil {
		return "", &Error{Reason: "Ollama returned an invalid response"}
	}
	if strings.TrimSpace(*decoded.Response) == "" {
		return "", &Error{Reason: "Ollama returned an empty summary"}
	}
	return *decoded.Response, nil
}

// LocalSummarizer prefers a local model and falls back deterministically.
type LocalSummarizer struct {
	Model     string
	Generate  Generator
	generated bool
}

// NewLocalSummarizer wires a summarizer to a local Ollama endpoint.
func NewLocalSummarizer(ollamaURL, model string) *LocalSummarizer {
	client := NewOllamaClient(ollamaURL, model)
	return &LocalSummarizer{Model: model, Generate: client.Generate}
}

// NewOfflineSummarizer always uses the deterministic summary. The CLI uses it
// so that scripted runs never wait on a model.
func NewOfflineSummarizer(model string) *LocalSummarizer {
	return &LocalSummarizer{
		Model: model,
		Generate: func(string) (string, error) {
			return "", &Error{Reason: "summarization is disabled"}
		},
	}
}

// summaryPrompt is kept verbatim: changing it would change every summary
// generated from here on, with no signal in the note that anything moved.
const summaryPrompt = "Summarize the following technical article in 2 to 4 factual sentences. " +
	"State the problem, approach, and key result when they are present. " +
	"Do not add facts or use a heading.\n\n" +
	"Title: %s\n\n" +
	"Article: %s"

// Summarize returns the model's summary, or the deterministic fallback along
// with the reason the model was not used.
func (s *LocalSummarizer) Summarize(title, content string) Result {
	fallback := Deterministic(title, content)
	sourceText := textx.CollapseStrip(content)
	if sourceText == "" {
		sourceText = textx.CollapseStrip(title)
	}
	if sourceText == "" {
		return Result{Text: fallback, Provider: "fallback", FallbackReason: "empty content"}
	}
	prompt := fmt.Sprintf(summaryPrompt, textx.CollapseStrip(title), truncateRunes(sourceText, MaxInputCharacters))

	generated, err := s.Generate(prompt)
	if err == nil {
		if generated = textx.CollapseStrip(generated); generated == "" {
			err = &Error{Reason: "Ollama returned an empty summary"}
		}
	}
	if err != nil {
		reason := err.Error()
		if reason == "" {
			reason = "OllamaError"
		}
		return Result{Text: fallback, Provider: "fallback", FallbackReason: reason}
	}
	return Result{
		Text:     trimToWord(generated, MaxSummaryCharacters),
		Provider: "ollama",
		Model:    s.Model,
	}
}

// truncateRunes cuts to a character count, never splitting a rune.
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// IsOllamaError reports whether err came from the optional local model.
func IsOllamaError(err error) bool {
	var ollamaError *Error
	return errors.As(err, &ollamaError)
}
