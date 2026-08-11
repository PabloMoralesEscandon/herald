package service

import (
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/paper"
	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// BackfillResult reports what an upgrade pass changed.
type BackfillResult struct {
	Entries            int            `json:"entries"`
	IdentifiersAdded   int            `json:"identifiers_added"`
	CanonicalKeysAdded int            `json:"canonical_keys_added"`
	KeywordsAdded      int            `json:"keywords_added"`
	ReferencesLinked   int            `json:"references_linked"`
	Rankings           map[string]any `json:"rankings"`
	ObsidianReconciled bool           `json:"obsidian_reconciled"`
}

var arxivHosts = map[string]bool{
	"arxiv.org": true, "www.arxiv.org": true, "export.arxiv.org": true,
}

// Backfill upgrades existing rows offline, without changing any reading status.
//
// It is idempotent, so it is safe to run after every upgrade: identifiers are
// derived from URLs already stored, keywords are extracted locally, citation
// edges are reconciled, and both relevance indexes are rebuilt.
func (s *Service) Backfill(syncObsidian bool) (*BackfillResult, error) {
	entries, err := s.DB.ListEntries(store.EntryFilter{})
	if err != nil {
		return nil, err
	}
	result := &BackfillResult{Entries: len(entries), Rankings: map[string]any{}}

	for _, entry := range entries {
		existingKeywords, err := s.DB.ListEntryKeywords(entry.ID)
		if err != nil {
			return nil, err
		}
		if len(existingKeywords) == 0 {
			extracted := paper.ExtractKeywords(entry.Title, entry.Content, 8)
			keywords := make([]store.Keyword, 0, len(extracted))
			for _, item := range extracted {
				score := item.Score
				keywords = append(keywords, store.Keyword{
					Keyword: item.Keyword, Kind: "keyword",
					Score: &score, Provider: item.Provider,
				})
			}
			if err := s.DB.ReplaceEntryKeywords(entry.ID, keywords); err != nil {
				return nil, err
			}
			result.KeywordsAdded += len(keywords)
		}
		if entry.ContentKind != "paper" {
			continue
		}

		scheme, value := deriveIdentifier(entry)
		if scheme == "" {
			continue
		}
		canonicalKey := scheme + ":" + value
		if deref(entry.CanonicalKey) == "" {
			if _, err := s.DB.UpdateEntryMetadata(entry.ID, store.MetadataUpdate{
				Title: entry.Title, CanonicalKey: &canonicalKey,
			}); err != nil {
				return nil, err
			}
			result.CanonicalKeysAdded++
		}
		known, err := s.DB.ListPaperIdentifiers(entry.ID)
		if err != nil {
			return nil, err
		}
		present := false
		for _, identifier := range known {
			if identifier.Scheme == scheme && identifier.Value == value {
				present = true
				break
			}
		}
		if !present {
			if _, err := s.DB.AddPaperIdentifier(entry.ID, scheme, value, true); err != nil {
				return nil, err
			}
			result.IdentifiersAdded++
		}
	}

	linked, err := s.DB.ReconcilePaperReferences()
	if err != nil {
		return nil, err
	}
	result.ReferencesLinked = linked

	for _, kind := range []string{"paper", "news"} {
		rescored, err := s.Relevance.Engine.Rescore(kind)
		if err != nil {
			return nil, err
		}
		result.Rankings[kind] = rescored
	}

	if syncObsidian {
		if err := s.ReconcileObsidian(); err != nil {
			return nil, err
		}
	}
	result.ObsidianReconciled = syncObsidian
	return result, nil
}

// deriveIdentifier recovers a paper's identity from an arXiv or DOI URL that
// predates identifier tracking.
func deriveIdentifier(entry *store.Entry) (scheme, value string) {
	url := entry.CanonicalURL
	if url == "" {
		url = entry.URL
	}
	parts := urlx.Split(url)
	host := parts.Hostname()
	switch {
	case arxivHosts[host]:
		path := parts.Path
		for _, prefix := range []string{"/abs/", "/pdf/"} {
			if strings.HasPrefix(path, prefix) {
				path = path[len(prefix):]
				break
			}
		}
		identifier, err := paper.NormalizeArxivID(path)
		if err != nil {
			return "", ""
		}
		return "arxiv", identifier
	case host == "doi.org" || host == "dx.doi.org":
		identifier, err := paper.NormalizeDOI(strings.TrimPrefix(parts.Path, "/"))
		if err != nil {
			return "", ""
		}
		return "doi", identifier
	}
	return "", ""
}
