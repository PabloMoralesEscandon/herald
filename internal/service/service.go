// Package service orchestrates ingestion, triage, and Obsidian synchronization.
//
// The rule that shapes this package: reading state is the user's, and is
// committed before any filesystem work is attempted. A vault that is missing,
// read-only, or holding a conflicting file never rolls back a Keep — it records
// a sync error and offers a retry.
package service

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/feed"
	"github.com/PabloMoralesEscandon/herald/internal/fulltext"
	"github.com/PabloMoralesEscandon/herald/internal/paper"
	"github.com/PabloMoralesEscandon/herald/internal/relevance"
	"github.com/PabloMoralesEscandon/herald/internal/sources"
	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/summary"
	"github.com/PabloMoralesEscandon/herald/internal/vault"
)

// Service ties the layers together.
type Service struct {
	DB           *store.DB
	Summarizer   summary.Provider
	Relevance    *relevance.Coordinator
	Importer     *paper.Importer
	Fetcher      FeedFetcher
	DefaultVault string
	ArchiveRoot  string
	// AutoEnrichKept starts background metadata enrichment when a paper is
	// kept. The CLI leaves it off so scripted runs stay deterministic.
	AutoEnrichKept bool
	// AutoExtractFullText additionally reads the article's own text after a
	// paper is kept. It follows the same rule as enrichment: off for scripted
	// runs, so nothing reaches the network unless the user asked it to.
	AutoExtractFullText bool
	// Extractor overrides the full-text extractor, which is how tests run the
	// whole pipeline without touching the network.
	Extractor *fulltext.Extractor
	// PDFRoot is where extracted and uploaded PDFs are stored.
	PDFRoot string
	// GROBIDURL is the REST service used to turn PDFs into structured TEI.
	GROBIDURL string
	Now       func() time.Time

	exporterOverride *vault.Exporter

	// transitionMutex serializes a triage transition against the final step of
	// a background enrichment, so an enrichment finishing at the same instant
	// as an un-keep cannot recreate the note that was just archived.
	//
	// enrichingMutex guards the in-flight set only, and is deliberately a
	// second lock: the transition path calls into the exporter, which needs to
	// read that set, and Go mutexes are not reentrant. When both are held the
	// order is always transitionMutex then enrichingMutex, so they cannot
	// deadlock against each other.
	transitionMutex sync.Mutex
	enrichingMutex  sync.Mutex
	enriching       map[int64]bool
	extracting      map[int64]bool
	enrichmentWait  sync.WaitGroup
}

// New builds a service with the standard collaborators.
func New(db *store.DB, options Options) *Service {
	service := &Service{
		DB:             db,
		Summarizer:     options.Summarizer,
		Relevance:      options.Relevance,
		Importer:       options.Importer,
		Fetcher:        options.Fetcher,
		DefaultVault:   options.DefaultVault,
		ArchiveRoot:    options.ArchiveRoot,
		AutoEnrichKept: options.AutoEnrichKept,

		AutoExtractFullText: options.AutoExtractFullText,
		Extractor:           options.Extractor,
		PDFRoot:             options.PDFRoot,
		GROBIDURL:           options.GROBIDURL,

		Now:        options.Now,
		enriching:  map[int64]bool{},
		extracting: map[int64]bool{},
	}
	if service.Fetcher == nil {
		service.Fetcher = FetchFeed
	}
	if service.Importer == nil {
		service.Importer = paper.NewImporter(db)
	}
	if service.Now == nil {
		service.Now = time.Now
	}
	if service.DefaultVault == "" {
		service.DefaultVault = filepath.Join(filepath.Dir(db.Path), "vault")
	}
	if service.ArchiveRoot == "" {
		service.ArchiveRoot = filepath.Join(filepath.Dir(db.Path), "obsidian-archive")
	}
	service.exporterOverride = options.Exporter
	return service
}

// Options are the injectable collaborators.
type Options struct {
	Summarizer          summary.Provider
	Relevance           *relevance.Coordinator
	Importer            *paper.Importer
	Fetcher             FeedFetcher
	Exporter            *vault.Exporter
	Extractor           *fulltext.Extractor
	DefaultVault        string
	ArchiveRoot         string
	PDFRoot             string
	GROBIDURL           string
	AutoEnrichKept      bool
	AutoExtractFullText bool
	Now                 func() time.Time
}

// Exporter returns the exporter for the configured vault.
func (s *Service) Exporter() (*vault.Exporter, error) {
	if s.exporterOverride != nil {
		return s.exporterOverride, nil
	}
	var configured struct {
		VaultPath string `json:"vault_path"`
	}
	if _, err := s.DB.GetSetting("obsidian", &configured); err != nil {
		return nil, err
	}
	vaultPath := configured.VaultPath
	if vaultPath == "" {
		vaultPath = s.DefaultVault
	}
	return vault.NewExporter(vaultPath, s.ArchiveRoot)
}

// SeedCuratedSources installs the packaged catalog, skipping URLs already
// present so an existing subscription's enabled state is never reset.
func (s *Service) SeedCuratedSources() (int, error) {
	existing, err := s.DB.ListSources(false)
	if err != nil {
		return 0, err
	}
	known := map[string]bool{}
	for _, source := range existing {
		known[source.URL] = true
	}
	catalog, err := sources.Catalog()
	if err != nil {
		return 0, err
	}
	created := 0
	for _, source := range catalog {
		if known[source.URL] {
			continue
		}
		enabled := source.Enabled
		if _, err := s.DB.AddSource(store.AddSourceInput{
			Title: source.Title, URL: source.URL, Category: source.Category,
			ContentKind: source.ContentKind, Enabled: &enabled,
		}); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// AddSource validates and stores a user-supplied subscription.
func (s *Service) AddSource(title, url, category, contentKind string) (int64, error) {
	if strings.TrimSpace(title) == "" {
		return 0, fmt.Errorf("Source title cannot be empty")
	}
	parts := urlSplit(strings.TrimSpace(url))
	if parts.Scheme != "http" && parts.Scheme != "https" || parts.Netloc == "" {
		return 0, fmt.Errorf("Source URL must be an HTTP or HTTPS URL")
	}
	canonical, err := feed.CanonicalizeURL(url)
	if err != nil {
		return 0, err
	}
	if category == "" {
		category = "Unsorted"
	}
	return s.DB.AddSource(store.AddSourceInput{
		Title: title, URL: canonical, Category: category, ContentKind: contentKind,
	})
}

// ExportSources returns portable configuration, excluding local state.
func (s *Service) ExportSources() (sources.Manifest, error) {
	rows, err := s.DB.ListSources(false)
	if err != nil {
		return sources.Manifest{}, err
	}
	portable := make([]sources.Source, 0, len(rows))
	for _, row := range rows {
		portable = append(portable, sources.Source{
			Title: row.Title, URL: row.URL, Category: row.Category,
			ContentKind: row.ContentKind, Enabled: row.Enabled == 1,
		})
	}
	return sources.CreateManifest(portable)
}

// ImportSources validates then merges portable configuration in one
// transaction. It never fetches and never deletes a local subscription.
func (s *Service) ImportSources(document []byte) (store.ImportResult, error) {
	validated, err := sources.ValidateManifest(document)
	if err != nil {
		return store.ImportResult{}, err
	}
	configs := make([]store.ImportSourceConfig, 0, len(validated))
	for _, source := range validated {
		configs = append(configs, store.ImportSourceConfig{
			Title: source.Title, URL: source.URL, Category: source.Category,
			ContentKind: source.ContentKind, Enabled: source.Enabled,
		})
	}
	return s.DB.ImportSourceConfigs(configs)
}

// SummarizeEntry generates and stores a summary, resyncing a kept note.
func (s *Service) SummarizeEntry(entryID int64) (summary.Result, error) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil {
		return summary.Result{}, err
	}
	if entry == nil {
		return summary.Result{}, &NotFoundError{EntryID: entryID}
	}
	result := s.Summarizer.Summarize(entry.Title, entry.Content)
	if _, err := s.DB.SetSummary(entryID, result.Text, result.Provider, result.Model); err != nil {
		return summary.Result{}, err
	}
	updated, err := s.DB.GetEntry(entryID)
	if err != nil {
		return summary.Result{}, err
	}
	if updated != nil && updated.Status == "kept" {
		// A sync failure must not discard the summary that was just stored.
		_, _ = s.ExportEntry(entryID)
	}
	return result, nil
}

// NotFoundError reports a missing entry.
type NotFoundError struct{ EntryID int64 }

func (e *NotFoundError) Error() string { return fmt.Sprintf("Entry %d does not exist", e.EntryID) }

// IsNotFound reports whether err is a missing-entry error.
func IsNotFound(err error) bool {
	var notFound *NotFoundError
	return errors.As(err, &notFound)
}

// EntryWithObsidianState returns an entry plus its note synchronization state.
func (s *Service) EntryWithObsidianState(entryID int64) (map[string]any, error) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil || entry == nil {
		return nil, err
	}
	export, err := s.DB.GetObsidianExport(entryID)
	if err != nil {
		return nil, err
	}
	text, err := s.DB.GetFullText(entryID)
	if err != nil {
		return nil, err
	}
	return withObsidianExport(entry, export, text), nil
}
