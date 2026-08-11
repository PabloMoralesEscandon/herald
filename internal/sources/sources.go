// Package sources handles Herald's portable subscription format and the
// versioned catalog of public feeds shipped with the application.
//
// A manifest carries subscriptions only. It deliberately excludes articles,
// reading history, rankings, refresh metadata, and database identifiers, so an
// export can be shared or moved between machines without leaking personal data.
package sources

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/feed"
	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// Manifest identity and limits.
const (
	ManifestFormat  = "herald.sources"
	ManifestVersion = 1
	MaxSources      = 1_000
	maxTitleLength  = 200
	maxURLLength    = 2_048
	// MaxManifestBytes bounds an imported document before it is parsed.
	MaxManifestBytes = 1_000_000
)

//go:embed data/sources.json
var catalogFS embed.FS

// ManifestError reports an invalid or unsafe manifest. Every message names the
// offending index so a hand-edited file can be corrected.
type ManifestError struct{ Reason string }

func (e *ManifestError) Error() string { return e.Reason }

func manifestErrorf(format string, args ...any) *ManifestError {
	return &ManifestError{Reason: fmt.Sprintf(format, args...)}
}

// Source is one portable subscription.
type Source struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Category    string `json:"category"`
	ContentKind string `json:"content_kind"`
	Enabled     bool   `json:"enabled"`
}

// Manifest is the document exchanged by export and import.
type Manifest struct {
	Format  string   `json:"format"`
	Version int      `json:"version"`
	Sources []Source `json:"sources"`
}

var allowedFields = map[string]bool{
	"title": true, "url": true, "category": true,
	"content_kind": true, "enabled": true,
}

// ValidateManifest checks a decoded manifest completely before any of it is
// applied, so an import is all-or-nothing.
func ValidateManifest(payload []byte) ([]Source, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, &ManifestError{Reason: "Source manifest must be a JSON object"}
	}
	var format string
	if err := json.Unmarshal(envelope["format"], &format); err != nil || format != ManifestFormat {
		return nil, manifestErrorf("format must be %q", ManifestFormat)
	}
	var version any
	_ = json.Unmarshal(envelope["version"], &version)
	number, ok := version.(float64)
	if !ok || int(number) != ManifestVersion || number != float64(int(number)) {
		return nil, manifestErrorf("Unsupported source manifest version: %v", version)
	}

	var rawSources []map[string]any
	if err := json.Unmarshal(envelope["sources"], &rawSources); err != nil {
		return nil, &ManifestError{Reason: "sources must be a JSON array"}
	}
	if len(rawSources) > MaxSources {
		return nil, manifestErrorf("A manifest may contain at most %d sources", MaxSources)
	}

	result := make([]Source, 0, len(rawSources))
	seen := make(map[string]bool, len(rawSources))
	for index, raw := range rawSources {
		source, err := validateSource(raw, index)
		if err != nil {
			return nil, err
		}
		if seen[source.URL] {
			return nil, manifestErrorf("Duplicate source URL: %s", source.URL)
		}
		seen[source.URL] = true
		result = append(result, source)
	}
	return result, nil
}

func validateSource(raw map[string]any, index int) (Source, error) {
	var unexpected []string
	for field := range raw {
		if !allowedFields[field] {
			unexpected = append(unexpected, field)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return Source{}, manifestErrorf("sources[%d] has unsupported fields: %s",
			index, strings.Join(unexpected, ", "))
	}

	title, err := requiredText(raw, "title", index)
	if err != nil {
		return Source{}, err
	}
	rawURL, err := requiredText(raw, "url", index)
	if err != nil {
		return Source{}, err
	}
	if err := validateSourceURL(rawURL, index); err != nil {
		return Source{}, err
	}
	canonical, canonErr := feed.CanonicalizeURL(rawURL)
	if canonErr != nil {
		return Source{}, manifestErrorf("sources[%d].url has an invalid port", index)
	}
	category, err := requiredText(raw, "category", index)
	if err != nil {
		return Source{}, err
	}
	kind, _ := raw["content_kind"].(string)
	if kind != "paper" && kind != "news" {
		return Source{}, manifestErrorf("sources[%d].content_kind must be paper or news", index)
	}
	enabled := true
	if value, present := raw["enabled"]; present {
		flag, ok := value.(bool)
		if !ok {
			return Source{}, manifestErrorf("sources[%d].enabled must be a boolean", index)
		}
		enabled = flag
	}
	return Source{
		Title:       title,
		URL:         canonical,
		Category:    category,
		ContentKind: kind,
		Enabled:     enabled,
	}, nil
}

// validateSourceURL rejects URLs Herald must never fetch: non-web schemes,
// embedded credentials, and non-standard ports.
func validateSourceURL(rawURL string, index int) error {
	parts := urlx.Split(rawURL)
	port, present, err := parts.Port()
	if err != nil {
		return manifestErrorf("sources[%d].url has an invalid port", index)
	}
	if parts.Scheme != "http" && parts.Scheme != "https" || parts.Hostname() == "" {
		return manifestErrorf("sources[%d].url must be an HTTP(S) URL", index)
	}
	if parts.HasUserinfo() {
		return manifestErrorf("sources[%d].url must not contain credentials", index)
	}
	if present && port != 80 && port != 443 {
		return manifestErrorf("sources[%d].url must use a standard HTTP(S) port", index)
	}
	return nil
}

func requiredText(raw map[string]any, key string, index int) (string, error) {
	value, ok := raw[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", manifestErrorf("sources[%d].%s must be a non-empty string", index, key)
	}
	value = strings.TrimSpace(value)
	limit := maxTitleLength
	if key == "url" {
		limit = maxURLLength
	}
	if len([]rune(value)) > limit {
		return "", manifestErrorf("sources[%d].%s is too long", index, key)
	}
	return value, nil
}

// CreateManifest builds a portable document from stored source rows, sorted so
// that repeated exports of unchanged subscriptions are byte-identical.
func CreateManifest(rows []Source) (Manifest, error) {
	portable := append([]Source{}, rows...)
	sort.SliceStable(portable, func(left, right int) bool {
		a, b := portable[left], portable[right]
		if a.ContentKind != b.ContentKind {
			return a.ContentKind < b.ContentKind
		}
		if !strings.EqualFold(a.Category, b.Category) {
			return strings.ToLower(a.Category) < strings.ToLower(b.Category)
		}
		if !strings.EqualFold(a.Title, b.Title) {
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		}
		return a.URL < b.URL
	})
	document, err := json.Marshal(Manifest{
		Format:  ManifestFormat,
		Version: ManifestVersion,
		Sources: portable,
	})
	if err != nil {
		return Manifest{}, err
	}
	// Round-trip through validation so an export can never emit a document
	// Herald would refuse to import.
	validated, err := ValidateManifest(document)
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{Format: ManifestFormat, Version: ManifestVersion, Sources: validated}, nil
}

// Catalog returns the versioned public feeds shipped with the application.
//
// It is embedded in the binary, so `herald init` works from any directory with
// no data files alongside the executable.
func Catalog() ([]Source, error) {
	document, err := catalogFS.ReadFile("data/sources.json")
	if err != nil {
		return nil, manifestErrorf("Could not load packaged source catalog: %v", err)
	}
	catalog, err := ValidateManifest(document)
	if err != nil {
		return nil, manifestErrorf("Could not load packaged source catalog: %v", err)
	}
	return catalog, nil
}
