package paper

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// Public metadata APIs. Both are free and require no key.
const (
	semanticScholarAPI   = "https://api.semanticscholar.org/graph/v1/paper/"
	crossrefAPI          = "https://api.crossref.org/works/"
	providerCacheSeconds = 7 * 24 * time.Hour
)

// semanticScholarFields is the field selection requested from the graph API.
const semanticScholarFields = "paperId,externalIds,url,title,abstract,authors,publicationDate,year," +
	"fieldsOfStudy,references.paperId,references.externalIds," +
	"references.url,references.title"

// requestJSON fetches and caches a provider response.
//
// Successful responses are cached locally for a week, so repeating an import is
// free and Herald stays a polite client of these public services.
func (i *Importer) requestJSON(provider, cacheKey, requestURL string) (map[string]any, error) {
	if cached, err := i.DB.GetProviderCache(provider, cacheKey, providerCacheSeconds); err == nil && cached != nil {
		var payload map[string]any
		if json.Unmarshal(cached, &payload) == nil {
			return payload, nil
		}
	}

	// Space out requests per provider; Semantic Scholar is stricter.
	configuredDelay := i.RequestDelay
	if configuredDelay < 0 {
		if provider == "semantic-scholar" {
			configuredDelay = time.Second
		} else {
			configuredDelay = 100 * time.Millisecond
		}
	}
	delay := configuredDelay
	if last, ok := i.lastRequestAt[provider]; ok {
		if elapsed := i.now().Sub(last); elapsed < configuredDelay {
			delay = configuredDelay - elapsed
		} else {
			delay = 0
		}
	}
	if delay > 0 {
		i.Sleep(delay)
	}

	var lastError error
	for attempt := 0; attempt <= i.Retries; attempt++ {
		document, err := i.Fetcher(requestURL,
			map[string]string{"Accept": "application/json"}, MaxProviderBytes, i.Timeout)
		if err == nil {
			i.lastRequestAt[provider] = i.now()
			var payload map[string]any
			if json.Unmarshal(document, &payload) != nil {
				return nil, &FetchError{Reason: provider + " returned invalid JSON"}
			}
			return payload, nil
		}
		if IsNotFound(err) {
			return nil, err
		}
		lastError = err
		var fetchErr *FetchError
		if !asFetchError(err, &fetchErr) || !fetchErr.Transient || attempt == i.Retries {
			break
		}
		// Exponential backoff, capped, for transient provider failures.
		backoff := time.Duration(1<<attempt) * time.Second
		if backoff > 4*time.Second {
			backoff = 4 * time.Second
		}
		i.Sleep(backoff)
	}
	return nil, lastError
}

func asFetchError(err error, target **FetchError) bool {
	if typed, ok := err.(*FetchError); ok {
		*target = typed
		return true
	}
	return false
}

// semanticScholar queries the graph API for a paper and its references.
func (i *Importer) semanticScholar(locator Locator) (Metadata, error) {
	prefix := map[string]string{"doi": "DOI:", "arxiv": "ARXIV:", "s2": ""}[locator.Scheme]
	paperID := prefix + locator.Value
	requestURL := semanticScholarAPI + url.PathEscape(paperID) + "?fields=" + semanticScholarFields
	// The cache key is versioned because older responses were requested
	// without references and would otherwise suppress citation enrichment.
	cacheKey := strings.ToLower(paperID) + ":references-v1"

	payload, err := i.requestJSON("semantic-scholar", cacheKey, requestURL)
	if err != nil {
		return Metadata{}, err
	}
	title := cleanText(stringField(payload, "title"))
	if title == "" {
		return Metadata{}, &NotFoundError{Reason: "Semantic Scholar did not return paper metadata"}
	}
	if document, marshalErr := json.Marshal(payload); marshalErr == nil {
		_ = i.DB.PutProviderCache("semantic-scholar", cacheKey, document)
	}

	identifiers := map[string]string{}
	if external, ok := payload["externalIds"].(map[string]any); ok {
		if doi := stringField(external, "DOI"); doi != "" {
			if normalized, err := NormalizeDOI(doi); err == nil {
				identifiers["doi"] = normalized
			}
		}
		if arxiv := stringField(external, "ArXiv"); arxiv != "" {
			if normalized, err := NormalizeArxivID(arxiv); err == nil {
				identifiers["arxiv"] = normalized
			}
		}
	}
	if paperIDValue := strings.ToLower(strings.TrimSpace(stringField(payload, "paperId"))); paperIDValue != "" {
		identifiers["s2"] = paperIDValue
	}

	var authors []string
	if items, ok := payload["authors"].([]any); ok {
		for _, item := range items {
			if author, ok := item.(map[string]any); ok {
				if name := cleanText(stringField(author, "name")); name != "" {
					authors = append(authors, name)
				}
			}
		}
	}
	var topics []string
	if items, ok := payload["fieldsOfStudy"].([]any); ok {
		for _, item := range items {
			if topic, ok := item.(string); ok && cleanText(topic) != "" {
				topics = append(topics, cleanText(topic))
			}
		}
	}

	year := ""
	if value, ok := payload["year"].(float64); ok {
		year = fmt.Sprintf("%d", int(value))
	}
	return Metadata{
		Title:       title,
		Authors:     authors,
		Abstract:    cleanText(stringField(payload, "abstract")),
		PublishedAt: normalizeDate(stringField(payload, "publicationDate"), year),
		URL:         cleanText(stringField(payload, "url")),
		Identifiers: identifiers,
		Topics:      topics,
		References:  semanticScholarReferences(payload["references"]),
		Provider:    "semantic-scholar",
	}, nil
}

// semanticScholarReferences normalizes the cited-paper list.
func semanticScholarReferences(value any) []ReferenceMetadata {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var results []ReferenceMetadata
	for _, raw := range items {
		record, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Some responses nest the cited paper one level down.
		if nested, ok := record["citedPaper"].(map[string]any); ok {
			record = nested
		}
		identifiers := map[string]string{}
		if external, ok := record["externalIds"].(map[string]any); ok {
			if doi := stringField(external, "DOI"); doi != "" {
				if normalized, err := NormalizeDOI(doi); err == nil {
					identifiers["doi"] = normalized
				}
			}
			if arxiv := stringField(external, "ArXiv"); arxiv != "" {
				if normalized, err := NormalizeArxivID(arxiv); err == nil {
					identifiers["arxiv"] = normalized
				}
			}
		}
		paperID := strings.ToLower(strings.TrimSpace(stringField(record, "paperId")))
		if s2Pattern.MatchString(paperID) {
			identifiers["s2"] = paperID
		}
		title := cleanText(stringField(record, "title"))
		referenceURL := cleanText(stringField(record, "url"))
		if referenceURL == "" && identifiers["doi"] != "" {
			referenceURL = "https://doi.org/" + identifiers["doi"]
		} else if referenceURL == "" && identifiers["arxiv"] != "" {
			referenceURL = "https://arxiv.org/abs/" + identifiers["arxiv"]
		}
		if len(identifiers) > 0 || referenceURL != "" {
			results = append(results, ReferenceMetadata{
				Title: title, URL: referenceURL, Identifiers: identifiers,
			})
		}
	}
	return results
}

var htmlTagPattern = regexp.MustCompile(`<[^>]+>`)

// crossref is the free DOI fallback when Semantic Scholar has no record.
func (i *Importer) crossref(doi string) (Metadata, error) {
	requestURL := crossrefAPI + urlx.Quote(doi, "")
	payload, err := i.requestJSON("crossref", doi, requestURL)
	if err != nil {
		return Metadata{}, err
	}
	message, ok := payload["message"].(map[string]any)
	if !ok {
		return Metadata{}, &NotFoundError{Reason: "Crossref did not return paper metadata"}
	}
	title := ""
	if titles, ok := message["title"].([]any); ok && len(titles) > 0 {
		if value, ok := titles[0].(string); ok {
			title = cleanText(value)
		}
	}
	if title == "" {
		return Metadata{}, &NotFoundError{Reason: "Crossref did not return a paper title"}
	}
	if document, marshalErr := json.Marshal(payload); marshalErr == nil {
		_ = i.DB.PutProviderCache("crossref", doi, document)
	}

	var authors []string
	if items, ok := message["author"].([]any); ok {
		for _, item := range items {
			author, ok := item.(map[string]any)
			if !ok {
				continue
			}
			parts := []string{}
			for _, key := range []string{"given", "family"} {
				if value := stringField(author, key); value != "" {
					parts = append(parts, value)
				}
			}
			if name := cleanText(strings.Join(parts, " ")); name != "" {
				authors = append(authors, name)
			}
		}
	}

	var publishedAt *string
	for _, key := range []string{"published", "published-print", "published-online"} {
		source, ok := message[key].(map[string]any)
		if !ok {
			continue
		}
		parts, ok := source["date-parts"].([]any)
		if !ok || len(parts) == 0 {
			continue
		}
		first, ok := parts[0].([]any)
		if !ok || len(first) == 0 {
			continue
		}
		numbers := []int{1, 1, 1}
		valid := true
		for index := 0; index < len(first) && index < 3; index++ {
			value, ok := first[index].(float64)
			if !ok {
				valid = false
				break
			}
			numbers[index] = int(value)
		}
		if !valid {
			break
		}
		stamp := time.Date(numbers[0], time.Month(numbers[1]), numbers[2],
			0, 0, 0, 0, time.UTC).Format("2006-01-02T15:04:05-07:00")
		publishedAt = &stamp
		break
	}

	var topics []string
	if items, ok := message["subject"].([]any); ok {
		for _, item := range items {
			if value, ok := item.(string); ok && cleanText(value) != "" {
				topics = append(topics, cleanText(value))
			}
		}
	}

	var references []ReferenceMetadata
	if items, ok := message["reference"].([]any); ok {
		for _, item := range items {
			record, ok := item.(map[string]any)
			if !ok {
				continue
			}
			doiValue := stringField(record, "DOI")
			if doiValue == "" {
				doiValue = stringField(record, "doi")
			}
			if doiValue == "" {
				continue
			}
			normalized, err := NormalizeDOI(doiValue)
			if err != nil {
				continue
			}
			titleValue := stringField(record, "article-title")
			if titleValue == "" {
				titleValue = stringField(record, "volume-title")
			}
			references = append(references, ReferenceMetadata{
				Title:       cleanText(titleValue),
				URL:         "https://doi.org/" + normalized,
				Identifiers: map[string]string{"doi": normalized},
			})
		}
	}

	paperURL := cleanText(stringField(message, "URL"))
	if paperURL == "" {
		paperURL = "https://doi.org/" + doi
	}
	return Metadata{
		Title:       title,
		Authors:     authors,
		Abstract:    cleanText(htmlTagPattern.ReplaceAllString(stringField(message, "abstract"), " ")),
		PublishedAt: publishedAt,
		URL:         paperURL,
		Identifiers: map[string]string{"doi": doi},
		Topics:      topics,
		References:  references,
		Provider:    "crossref",
	}, nil
}

func stringField(payload map[string]any, key string) string {
	if value, ok := payload[key].(string); ok {
		return value
	}
	return ""
}

// mergeMetadata prefers the provider record and fills gaps from the page.
func mergeMetadata(primary, fallback Metadata) Metadata {
	merged := Metadata{
		Title:       firstNonEmpty(primary.Title, fallback.Title),
		Abstract:    firstNonEmpty(primary.Abstract, fallback.Abstract),
		URL:         firstNonEmpty(primary.URL, fallback.URL),
		Identifiers: map[string]string{},
		Provider:    primary.Provider,
	}
	if len(primary.Authors) > 0 {
		merged.Authors = primary.Authors
	} else {
		merged.Authors = fallback.Authors
	}
	if primary.PublishedAt != nil {
		merged.PublishedAt = primary.PublishedAt
	} else {
		merged.PublishedAt = fallback.PublishedAt
	}
	for key, value := range fallback.Identifiers {
		merged.Identifiers[key] = value
	}
	for key, value := range primary.Identifiers {
		merged.Identifiers[key] = value
	}
	merged.Topics = dedupe(append(append([]string{}, primary.Topics...), fallback.Topics...))
	merged.SuppliedKeywords = dedupe(
		append(append([]string{}, primary.SuppliedKeywords...), fallback.SuppliedKeywords...))
	if len(primary.References) > 0 {
		merged.References = primary.References
	} else {
		merged.References = fallback.References
	}
	return merged
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
