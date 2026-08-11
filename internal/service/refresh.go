package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/feed"
	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/summary"
	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// maxFeedBytes bounds a feed document.
const maxFeedBytes = 10 * 1024 * 1024

// newsBootstrapWindow is how far back the first refresh of a news source
// reaches. Without it, subscribing to a decade-old feed would flood the queue.
const newsBootstrapWindow = 30 * 24 * time.Hour

// FetchError reports that a feed could not be retrieved.
type FetchError struct{ Reason string }

func (e *FetchError) Error() string { return e.Reason }

// FeedResponse is a fetched feed document plus its cache validators.
type FeedResponse struct {
	Document     []byte
	URL          string
	ETag         string
	LastModified string
	NotModified  bool
}

// FeedFetcher retrieves a feed. It is an interface seam for tests.
type FeedFetcher func(url, etag, lastModified string) (FeedResponse, error)

// FetchFeed retrieves a feed using conditional requests.
//
// ETag and Last-Modified are sent so an unchanged feed costs a 304 rather than
// a full download, which is what makes refreshing 48 sources cheap and polite.
func FetchFeed(url, etag, lastModified string) (FeedResponse, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return FeedResponse{}, &FetchError{Reason: fmt.Sprintf("Could not fetch %s: %v", url, err)}
	}
	request.Header.Set("User-Agent", "Herald/0.1 (+local research reader)")
	request.Header.Set("Accept",
		"application/atom+xml, application/rss+xml, application/xml, text/xml, text/html")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		request.Header.Set("If-Modified-Since", lastModified)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return FeedResponse{}, &FetchError{Reason: fmt.Sprintf("Could not fetch %s: %v", url, err)}
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		return FeedResponse{
			URL:          url,
			ETag:         headerOr(response, "ETag", etag),
			LastModified: headerOr(response, "Last-Modified", lastModified),
			NotModified:  true,
		}, nil
	}
	if response.StatusCode >= 400 {
		return FeedResponse{}, &FetchError{
			Reason: fmt.Sprintf("Could not fetch %s: HTTP %d", url, response.StatusCode),
		}
	}
	document, err := io.ReadAll(io.LimitReader(response.Body, maxFeedBytes+1))
	if err != nil {
		return FeedResponse{}, &FetchError{Reason: fmt.Sprintf("Could not fetch %s: %v", url, err)}
	}
	if len(document) > maxFeedBytes {
		return FeedResponse{}, &FetchError{Reason: fmt.Sprintf("Feed exceeds the 10 MiB limit: %s", url)}
	}
	return FeedResponse{
		Document:     document,
		URL:          response.Request.URL.String(),
		ETag:         response.Header.Get("ETag"),
		LastModified: response.Header.Get("Last-Modified"),
	}, nil
}

func headerOr(response *http.Response, name, fallback string) string {
	if value := response.Header.Get(name); value != "" {
		return value
	}
	return fallback
}

// RefreshResult reports one source's refresh outcome.
type RefreshResult struct {
	SourceID    int64   `json:"source_id"`
	SourceTitle string  `json:"source_title"`
	ContentKind string  `json:"content_kind"`
	Fetched     int     `json:"fetched"`
	Created     int     `json:"created"`
	Updated     int     `json:"updated"`
	Error       *string `json:"error"`
	NotModified bool    `json:"not_modified"`
}

// RefreshSource fetches one source and ingests its entries.
func (s *Service) RefreshSource(sourceID int64) (RefreshResult, error) {
	source, err := s.DB.GetSource(sourceID)
	if err != nil {
		return RefreshResult{}, err
	}
	if source == nil {
		return RefreshResult{}, fmt.Errorf("Source %d does not exist", sourceID)
	}
	result := RefreshResult{
		SourceID: sourceID, SourceTitle: source.Title, ContentKind: source.ContentKind,
	}

	fetchURL := source.ResolvedURL
	if fetchURL == "" {
		fetchURL = source.URL
	}
	response, resolvedURL, entries, err := s.fetchAndParse(source, fetchURL)
	if err != nil {
		// Record the failure on the source before propagating, so the
		// dashboard can show which feed is unhealthy.
		_, _ = s.DB.UpdateSourceRefreshState(sourceID, store.RefreshState{
			Succeeded: false, Error: err.Error(),
		})
		return result, err
	}
	if response.NotModified {
		_, _ = s.DB.UpdateSourceRefreshState(sourceID, store.RefreshState{
			Succeeded: true,
			ETag:      optional(response.ETag), LastModified: optional(response.LastModified),
		})
		result.NotModified = true
		return result, nil
	}

	skips, err := s.DB.ListSourceBootstrapSkips(sourceID)
	if err != nil {
		return result, err
	}
	var newSkips []string
	for _, entry := range entries {
		if !s.includeFeedEntry(source, entry, skips) {
			if source.RefreshSucceededAt == nil {
				newSkips = append(newSkips, entry.GUID)
			}
			continue
		}
		canonicalURL, canonErr := feed.CanonicalizeURL(entry.URL)
		if canonErr != nil {
			return result, canonErr
		}
		var publishedAt *string
		if entry.PublishedAt != "" {
			published := entry.PublishedAt
			publishedAt = &published
		}
		entryID, created, err := s.DB.UpsertEntry(store.UpsertEntryInput{
			SourceID: sourceID, GUID: entry.GUID, URL: entry.URL, Title: entry.Title,
			Author: entry.Author, PublishedAt: publishedAt, Content: entry.Content,
			ContentMarkdown: entry.ContentMarkdown,
			Summary:         summary.Deterministic(entry.Title, entry.Content),
			SummaryProvider: "extractive", ContentKind: source.ContentKind,
			CanonicalURL: canonicalURL,
		})
		if err != nil {
			return result, err
		}
		if created {
			result.Created++
		} else {
			result.Updated++
		}
		// News is not enriched by metadata providers.
		if source.ContentKind == "news" {
			if _, err := s.DB.SetEnrichmentState(entryID, "not_applicable", store.EnrichmentState{}); err != nil {
				return result, err
			}
		}
		// A refreshed article that is already kept has its note resynced.
		current, err := s.DB.GetEntry(entryID)
		if err != nil {
			return result, err
		}
		if current != nil && current.Status == "kept" {
			_, _ = s.ExportEntry(entryID)
		}
	}
	if err := s.DB.AddSourceBootstrapSkips(sourceID, newSkips); err != nil {
		return result, err
	}
	if _, err := s.DB.UpdateSourceRefreshState(sourceID, store.RefreshState{
		Succeeded: true, ETag: optional(response.ETag),
		LastModified: optional(response.LastModified), ResolvedURL: optional(resolvedURL),
	}); err != nil {
		return result, err
	}

	result.Fetched = len(entries)
	if result.Created > 0 && source.ContentKind == "news" && s.Relevance != nil {
		_, _ = s.Relevance.Start("news")
	}
	return result, nil
}

// fetchAndParse retrieves a source, falling back to feed autodiscovery when the
// configured URL turns out to be a web page rather than a feed.
func (s *Service) fetchAndParse(source *store.Source, fetchURL string) (FeedResponse, string, []feed.Entry, error) {
	response, err := s.Fetcher(fetchURL, source.ETag, source.LastModified)
	if err != nil {
		return FeedResponse{}, "", nil, err
	}
	if response.NotModified {
		return response, "", nil, nil
	}

	resolvedURL := source.ResolvedURL
	if resolvedURL == "" && response.URL != "" {
		fetched, fetchErr := feed.CanonicalizeURL(response.URL)
		configured, configErr := feed.CanonicalizeURL(source.URL)
		if fetchErr == nil && configErr == nil && fetched != configured {
			resolvedURL = fetched
		}
	}

	entries, parseErr := feed.Parse(response.Document)
	if parseErr == nil {
		return response, resolvedURL, entries, nil
	}
	// Only try autodiscovery for a source that has not already resolved to a
	// real feed; otherwise a transient HTML error page could rewrite the URL.
	if resolvedURL != "" {
		return FeedResponse{}, "", nil, parseErr
	}
	discovered := feed.DiscoverFeedURL(response.Document, response.URL)
	if discovered == "" {
		return FeedResponse{}, "", nil, parseErr
	}
	feedResponse, err := s.Fetcher(discovered, "", "")
	if err != nil {
		return FeedResponse{}, "", nil, err
	}
	if feedResponse.NotModified {
		return feedResponse, discovered, nil, nil
	}
	entries, err = feed.Parse(feedResponse.Document)
	if err != nil {
		return FeedResponse{}, "", nil, err
	}
	return feedResponse, discovered, entries, nil
}

// includeFeedEntry decides whether a feed item should be ingested.
//
// Papers are always ingested. A news source's first refresh takes only the last
// 30 days; every later refresh accepts anything newly observed.
func (s *Service) includeFeedEntry(source *store.Source, entry feed.Entry, skips map[string]bool) bool {
	if source.ContentKind != "news" {
		return true
	}
	if source.RefreshSucceededAt != nil {
		return !skips[entry.GUID]
	}
	published, ok := parseTimestamp(entry.PublishedAt)
	if !ok {
		return false
	}
	return !published.Before(s.Now().UTC().Add(-newsBootstrapWindow))
}

func parseTimestamp(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	candidate := strings.Replace(value, "Z", "+00:00", 1)
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
	} {
		if parsed, err := time.Parse(layout, candidate); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// RefreshAll fetches every enabled source.
//
// One failing feed is recorded and reported but never prevents the others from
// ingesting, which is what keeps a single dead source from breaking refresh.
func (s *Service) RefreshAll() ([]RefreshResult, error) {
	sourceRows, err := s.DB.ListSources(true)
	if err != nil {
		return nil, err
	}
	var results []RefreshResult
	for _, source := range sourceRows {
		if source.Adapter == "manual" {
			continue
		}
		result, err := s.RefreshSource(source.ID)
		if err != nil {
			if !isSourceFailure(err) {
				return results, err
			}
			message := err.Error()
			results = append(results, RefreshResult{
				SourceID: source.ID, SourceTitle: source.Title,
				ContentKind: source.ContentKind, Error: &message,
			})
			continue
		}
		results = append(results, result)
	}
	return results, nil
}

// isSourceFailure reports whether an error is a per-source health problem
// rather than a bug that should abort the whole refresh.
func isSourceFailure(err error) bool {
	var fetchError *FetchError
	if errors.As(err, &fetchError) || feed.IsParseError(err) {
		return true
	}
	// A malformed URL in a feed is source data, not a programming error.
	return errors.Is(err, urlx.ErrInvalidPort)
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func urlSplit(raw string) urlx.Parts { return urlx.Split(raw) }
