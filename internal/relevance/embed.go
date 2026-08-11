package relevance

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/store"
)

// Embedder produces dense vectors. It is optional: when it fails, scoring falls
// back to TF-IDF rather than surfacing an error to the user.
type Embedder interface {
	Embed(texts []string) ([][]float64, error)
	Model() string
}

// EmbeddingError reports that the optional provider could not produce vectors.
type EmbeddingError struct{ Reason string }

func (e *EmbeddingError) Error() string { return e.Reason }

// OllamaEmbedder is a batched Ollama /api/embed client with a SQLite cache.
type OllamaEmbedder struct {
	DB        *store.DB
	BaseURL   string
	ModelName string
	BatchSize int
	Client    *http.Client
}

// NewOllamaEmbedder builds a provider with Herald's short timeout, so a missing
// or slow model degrades to TF-IDF quickly instead of stalling the queue.
func NewOllamaEmbedder(db *store.DB, baseURL, model string) *OllamaEmbedder {
	return &OllamaEmbedder{
		DB:        db,
		BaseURL:   strings.TrimRight(baseURL, "/"),
		ModelName: model,
		BatchSize: 32,
		Client:    &http.Client{Timeout: 4 * time.Second},
	}
}

// Model names the provider for stored ranking provenance.
func (o *OllamaEmbedder) Model() string { return o.ModelName }

// Embed returns one vector per text, serving cache hits without a request.
func (o *OllamaEmbedder) Embed(texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	results := make([][]float64, len(texts))
	type pending struct {
		index  int
		text   string
		digest string
	}
	var missing []pending
	for index, text := range texts {
		digest := textHash(text)
		cached, err := o.DB.GetEmbedding(digest, o.ModelName)
		if err != nil {
			return nil, &EmbeddingError{Reason: err.Error()}
		}
		if cached == nil {
			missing = append(missing, pending{index: index, text: text, digest: digest})
			continue
		}
		vector, err := unpackVector(cached.Vector, cached.Dimensions)
		if err != nil {
			return nil, err
		}
		results[index] = vector
	}

	for offset := 0; offset < len(missing); offset += o.BatchSize {
		end := min(offset+o.BatchSize, len(missing))
		batch := missing[offset:end]
		inputs := make([]string, len(batch))
		for index, item := range batch {
			inputs[index] = item.text
		}
		vectors, err := o.requestBatch(inputs)
		if err != nil {
			return nil, err
		}
		if len(vectors) != len(batch) {
			return nil, &EmbeddingError{Reason: "Ollama returned an invalid embedding batch"}
		}
		for index, item := range batch {
			vector := vectors[index]
			if len(vector) == 0 {
				return nil, &EmbeddingError{Reason: "Ollama returned an empty embedding"}
			}
			if err := o.DB.PutEmbedding(item.digest, o.ModelName, len(vector), packVector(vector)); err != nil {
				return nil, &EmbeddingError{Reason: err.Error()}
			}
			results[item.index] = vector
		}
	}
	for _, vector := range results {
		if vector == nil {
			return nil, &EmbeddingError{Reason: "Embedding batch was incomplete"}
		}
	}
	return results, nil
}

func (o *OllamaEmbedder) requestBatch(inputs []string) ([][]float64, error) {
	payload, err := json.Marshal(map[string]any{"model": o.ModelName, "input": inputs})
	if err != nil {
		return nil, &EmbeddingError{Reason: err.Error()}
	}
	request, err := http.NewRequest(http.MethodPost, o.BaseURL+"/api/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, &EmbeddingError{Reason: err.Error()}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := o.Client.Do(request)
	if err != nil {
		return nil, &EmbeddingError{Reason: fmt.Sprintf("Ollama embeddings unavailable: %v", err)}
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 20*1024*1024))
	if err != nil {
		return nil, &EmbeddingError{Reason: fmt.Sprintf("Ollama embeddings unavailable: %v", err)}
	}
	var decoded struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		return nil, &EmbeddingError{Reason: fmt.Sprintf("Ollama embeddings unavailable: %v", err)}
	}
	if decoded.Embeddings == nil {
		return nil, &EmbeddingError{Reason: "Ollama returned an invalid embedding batch"}
	}
	return decoded.Embeddings, nil
}

func textHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// packVector stores a vector as little-endian float32, matching the existing
// cache format so a database written by either version stays usable.
func packVector(vector []float64) []byte {
	buffer := make([]byte, 4*len(vector))
	for index, value := range vector {
		binary.LittleEndian.PutUint32(buffer[index*4:], math.Float32bits(float32(value)))
	}
	return buffer
}

func unpackVector(blob []byte, dimensions int) ([]float64, error) {
	if len(blob) != dimensions*4 {
		return nil, &EmbeddingError{Reason: "Cached embedding has invalid dimensions"}
	}
	vector := make([]float64, dimensions)
	for index := range vector {
		vector[index] = float64(math.Float32frombits(
			binary.LittleEndian.Uint32(blob[index*4:])))
	}
	return vector, nil
}
