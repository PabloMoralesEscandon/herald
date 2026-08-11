package paper

import (
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/summary"
)

// ManualSourceURL identifies the pseudo-source that owns manually imported
// papers. It is never fetched.
const ManualSourceURL = "herald://manual-imports"

// Importer resolves a paper's metadata and persists it.
type Importer struct {
	DB           *store.DB
	Fetcher      Fetcher
	Sleep        func(time.Duration)
	RequestDelay time.Duration
	Retries      int
	Timeout      time.Duration

	lastRequestAt map[string]time.Time
	now           func() time.Time
}

// NewImporter builds an importer with Herald's polite defaults.
//
// RequestDelay of -1 means "use the per-provider default", which is how a
// caller asks for the standard rate limiting without naming a number.
func NewImporter(db *store.DB) *Importer {
	return &Importer{
		DB:            db,
		Fetcher:       FetchPublicDocument,
		Sleep:         time.Sleep,
		RequestDelay:  -1,
		Retries:       2,
		Timeout:       10 * time.Second,
		lastRequestAt: map[string]time.Time{},
		now:           time.Now,
	}
}

// Result is the outcome of an import.
type Result struct {
	Entry       *store.Entry             `json:"entry"`
	Created     bool                     `json:"created"`
	Identifiers []*store.PaperIdentifier `json:"identifiers"`
	Keywords    []*store.Keyword         `json:"keywords"`
	References  []*store.PaperReference  `json:"references"`
}

// Import resolves a locator to paper metadata and stores it.
//
// inspectPage additionally reads the publication page for author-supplied
// citation metadata, which is how keeping an RSS paper enriches it.
func (i *Importer) Import(value string, inspectPage bool) (*Result, error) {
	if i.lastRequestAt == nil {
		i.lastRequestAt = map[string]time.Time{}
	}
	if i.now == nil {
		i.now = time.Now
	}
	locator, err := ParseLocator(value)
	if err != nil {
		return nil, err
	}

	var providerErrors []error
	var pageMetadata *Metadata
	providerLocator := locator

	requestedURL := ""
	trimmed := strings.TrimSpace(value)
	if strings.EqualFold(scheme(trimmed), "https") {
		if safe, err := validatePublicHTTPSURL(trimmed, false); err == nil {
			requestedURL = safe
		}
	}
	pageURL := locator.PageURL
	if pageURL == "" && inspectPage {
		pageURL = requestedURL
	}
	if pageURL != "" {
		document, fetchErr := i.Fetcher(pageURL,
			map[string]string{"Accept": "text/html, application/xhtml+xml"},
			MaxPaperPageBytes, i.Timeout)
		if fetchErr != nil {
			// A DOI/arXiv/S2 identifier can still be enriched by a provider
			// when the page is unavailable. A generic page URL has no other
			// identifier to fall back on, so that failure is fatal.
			if locator.Scheme == "url" {
				return nil, fetchErr
			}
			providerErrors = append(providerErrors, fetchErr)
		} else {
			parsed, parseErr := ParseCitationMetadata(document, pageURL)
			if parseErr != nil {
				if locator.Scheme == "url" {
					return nil, parseErr
				}
				providerErrors = append(providerErrors, parseErr)
			} else {
				pageMetadata = &parsed
			}
		}
	}
	// A page that declares its own DOI or arXiv ID gives a better provider key
	// than the URL the user pasted.
	if pageMetadata != nil {
		if doi, ok := pageMetadata.Identifiers["doi"]; ok {
			providerLocator = Locator{Scheme: "doi", Value: doi, PageURL: locator.PageURL}
		} else if arxiv, ok := pageMetadata.Identifiers["arxiv"]; ok {
			providerLocator = Locator{Scheme: "arxiv", Value: arxiv, PageURL: locator.PageURL}
		}
	}

	var metadata *Metadata
	switch providerLocator.Scheme {
	case "doi", "arxiv", "s2":
		resolved, err := i.semanticScholar(providerLocator)
		if err != nil {
			providerErrors = append(providerErrors, err)
		} else {
			metadata = &resolved
		}
		if metadata == nil && providerLocator.Scheme == "doi" {
			resolved, err := i.crossref(providerLocator.Value)
			if err != nil {
				providerErrors = append(providerErrors, err)
			} else {
				metadata = &resolved
			}
		}
	}

	switch {
	case metadata == nil:
		metadata = pageMetadata
	case pageMetadata != nil:
		merged := mergeMetadata(*metadata, *pageMetadata)
		metadata = &merged
	}
	if metadata == nil || metadata.Title == "" {
		// Surface a transport failure in preference to a generic "not found",
		// because the two need different user action.
		for index := len(providerErrors) - 1; index >= 0; index-- {
			if IsFetchError(providerErrors[index]) {
				return nil, providerErrors[index]
			}
		}
		if len(providerErrors) > 0 {
			messages := make([]string, len(providerErrors))
			for index, err := range providerErrors {
				messages[index] = err.Error()
			}
			return nil, &NotFoundError{Reason: strings.Join(messages, "; ")}
		}
		return nil, &NotFoundError{Reason: "No citation metadata was found"}
	}

	if metadata.Identifiers == nil {
		metadata.Identifiers = map[string]string{}
	}
	if providerLocator.Scheme != "url" {
		if _, present := metadata.Identifiers[providerLocator.Scheme]; !present {
			metadata.Identifiers[providerLocator.Scheme] = providerLocator.Value
		}
	}
	if locator.Scheme == "s2" {
		if _, present := metadata.Identifiers["s2"]; !present {
			metadata.Identifiers["s2"] = locator.Value
		}
	}
	return i.persist(*metadata, locator)
}

func scheme(value string) string {
	if index := strings.Index(value, ":"); index > 0 {
		return value[:index]
	}
	return ""
}

// identity derives the stable guid, canonical URL, and identity key.
//
// The preference order is DOI, then arXiv, then Semantic Scholar, then the page
// URL, so the most durable identifier available becomes the note's stable name.
func identity(metadata Metadata, locator Locator) (guid, canonicalURL, canonicalKey string) {
	if doi := metadata.Identifiers["doi"]; doi != "" {
		return "doi:" + doi, "https://doi.org/" + doi, "doi:" + doi
	}
	if arxiv := metadata.Identifiers["arxiv"]; arxiv != "" {
		return "arxiv:" + arxiv, "https://arxiv.org/abs/" + arxiv, "arxiv:" + arxiv
	}
	if s2 := metadata.Identifiers["s2"]; s2 != "" {
		return "semantic-scholar:" + s2, metadata.URL, "s2:" + s2
	}
	pageURL := metadata.URL
	if pageURL == "" {
		pageURL = locator.PageURL
	}
	return "url:" + pageURL, pageURL, "url:" + pageURL
}

// referenceIdentity derives a stable key for one cited work.
func referenceIdentity(reference ReferenceMetadata) (key, scheme, value string, ok bool) {
	for _, candidate := range []string{"doi", "arxiv", "s2"} {
		if identifier := strings.TrimSpace(reference.Identifiers[candidate]); identifier != "" {
			return candidate + ":" + identifier, candidate, identifier, true
		}
	}
	if url := strings.TrimSpace(reference.URL); url != "" {
		return "url:" + url, "", "", true
	}
	return "", "", "", false
}

// persist writes the resolved paper, reusing an existing entry when the
// identifiers match one Herald already has.
func (i *Importer) persist(metadata Metadata, locator Locator) (*Result, error) {
	guid, canonicalURL, canonicalKey := identity(metadata, locator)

	matches := map[int64]*store.Entry{}
	for scheme, value := range metadata.Identifiers {
		match, err := i.DB.FindEntryByIdentifier(scheme, value)
		if err != nil {
			return nil, err
		}
		if match != nil {
			matches[match.ID] = match
		}
	}
	if match, err := i.DB.FindEntryByCanonicalKey(canonicalKey); err != nil {
		return nil, err
	} else if match != nil {
		matches[match.ID] = match
	}
	candidateURLs := map[string]bool{canonicalURL: true, locator.PageURL: true}
	if arxiv := metadata.Identifiers["arxiv"]; arxiv != "" {
		candidateURLs["https://arxiv.org/abs/"+arxiv] = true
		candidateURLs["http://arxiv.org/abs/"+arxiv] = true
	}
	for candidate := range candidateURLs {
		if candidate == "" {
			continue
		}
		match, err := i.DB.FindEntryByURL(candidate)
		if err != nil {
			return nil, err
		}
		if match != nil {
			matches[match.ID] = match
		}
	}
	// Ambiguity is reported rather than guessed at: silently merging two
	// entries would destroy the triage state of one of them.
	if len(matches) > 1 {
		return nil, &ImportError{
			Reason: "Paper identifiers match multiple Herald entries; resolve the duplicate before importing",
		}
	}

	var entryID int64
	created := false
	var existing *store.Entry
	for _, match := range matches {
		existing = match
	}
	if existing == nil {
		sourceID, err := i.DB.AddSource(store.AddSourceInput{
			Title: "Manual Imports", URL: ManualSourceURL, Category: "Manual Imports",
			ContentKind: "paper", Adapter: "manual",
		})
		if err != nil {
			return nil, err
		}
		entryID, created, err = i.DB.UpsertEntry(store.UpsertEntryInput{
			SourceID: sourceID, GUID: guid, URL: canonicalURL,
			CanonicalURL: canonicalURL, CanonicalKey: &canonicalKey,
			Title: metadata.Title, Author: strings.Join(metadata.Authors, ", "),
			PublishedAt: metadata.PublishedAt, Content: metadata.Abstract,
			Summary:         summary.Deterministic(metadata.Title, metadata.Abstract),
			SummaryProvider: "extractive", ContentKind: "paper",
		})
		if err != nil {
			return nil, err
		}
	} else {
		entryID = existing.ID
		if _, err := i.DB.UpdateEntryMetadata(entryID, store.MetadataUpdate{
			Title: metadata.Title, Author: strings.Join(metadata.Authors, ", "),
			PublishedAt: metadata.PublishedAt, Content: metadata.Abstract,
			CanonicalURL: &canonicalURL, CanonicalKey: &canonicalKey,
		}); err != nil {
			return nil, err
		}
	}

	primaryScheme, _, _ := strings.Cut(canonicalKey, ":")
	for scheme, identifier := range metadata.Identifiers {
		if _, err := i.DB.AddPaperIdentifier(entryID, scheme, identifier, scheme == primaryScheme); err != nil {
			return nil, err
		}
	}
	for position, reference := range metadata.References {
		key, externalScheme, externalID, ok := referenceIdentity(reference)
		if !ok {
			continue
		}
		order := int64(position + 1)
		if _, err := i.DB.UpsertPaperReference(entryID, key, store.UpsertReferenceInput{
			ExternalScheme: externalScheme, ExternalID: externalID,
			CitedTitle: reference.Title, CitedURL: reference.URL,
			Position: &order, Provider: metadata.Provider,
		}); err != nil {
			return nil, err
		}
	}
	if _, err := i.DB.ReconcilePaperReferences(); err != nil {
		return nil, err
	}

	// Author-supplied keywords rank above extracted ones; topics are separate.
	var keywords []store.Keyword
	seen := map[string]bool{}
	for _, keyword := range metadata.SuppliedKeywords {
		normalized := strings.ToLower(keyword)
		if !seen[normalized] && len(keywords) < 8 {
			keywords = append(keywords, store.Keyword{
				Keyword: keyword, Kind: "keyword", Provider: "page-meta",
			})
			seen[normalized] = true
		}
	}
	for _, extracted := range ExtractKeywords(metadata.Title, metadata.Abstract, 8) {
		normalized := strings.ToLower(extracted.Keyword)
		if !seen[normalized] && len(keywords) < 8 {
			score := extracted.Score
			keywords = append(keywords, store.Keyword{
				Keyword: extracted.Keyword, Kind: "keyword",
				Score: &score, Provider: extracted.Provider,
			})
			seen[normalized] = true
		}
	}
	for _, topic := range metadata.Topics {
		keywords = append(keywords, store.Keyword{
			Keyword: topic, Kind: "topic", Provider: metadata.Provider,
		})
	}
	if err := i.DB.ReplaceEntryKeywords(entryID, keywords); err != nil {
		return nil, err
	}
	if _, err := i.DB.SetEnrichmentState(entryID, "enriched", store.EnrichmentState{
		Provider: metadata.Provider, CanonicalURL: &canonicalURL, CanonicalKey: &canonicalKey,
	}); err != nil {
		return nil, err
	}

	entry, err := i.DB.GetEntry(entryID)
	if err != nil {
		return nil, err
	}
	identifiers, err := i.DB.ListPaperIdentifiers(entryID)
	if err != nil {
		return nil, err
	}
	storedKeywords, err := i.DB.ListEntryKeywords(entryID)
	if err != nil {
		return nil, err
	}
	references, err := i.DB.ListPaperReferences(entryID)
	if err != nil {
		return nil, err
	}
	return &Result{
		Entry: entry, Created: created, Identifiers: identifiers,
		Keywords: storedKeywords, References: references,
	}, nil
}
