package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PabloMoralesEscandon/herald/internal/paper"
	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/vault"
)

// noteFor assembles the render input for one entry.
func (s *Service) noteFor(entryID int64) (*vault.Note, error) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &NotFoundError{EntryID: entryID}
	}
	note := &vault.Note{
		ID: entry.ID, Title: entry.Title, URL: entry.URL,
		CanonicalURL: entry.CanonicalURL, Author: entry.Author,
		DiscoveredAt: entry.DiscoveredAt, Status: entry.Status,
		Content: entry.Content, ContentMarkdown: entry.ContentMarkdown,
		Summary: entry.Summary, SummaryProvider: entry.SummaryProvider,
		SummaryModel: entry.SummaryModel, ContentKind: entry.ContentKind,
		EnrichmentStatus:   entry.EnrichmentStatus,
		EnrichmentProvider: entry.EnrichmentProvider,
		SourceTitle:        entry.SourceTitle, SourceCategory: entry.SourceCategory,
	}
	note.PublishedAt = deref(entry.PublishedAt)
	note.SummaryGeneratedAt = deref(entry.SummaryGeneratedAt)
	note.UpdatedAt = deref(entry.UpdatedAt)
	note.CanonicalKey = deref(entry.CanonicalKey)

	keywords, err := s.DB.ListEntryKeywords(entryID)
	if err != nil {
		return nil, err
	}
	for _, keyword := range keywords {
		if keyword.Kind == "topic" {
			note.Topics = append(note.Topics, keyword.Keyword)
		} else {
			note.Keywords = append(note.Keywords, keyword.Keyword)
		}
	}

	profile, err := s.DB.GetRelevanceProfile(entry.ContentKind)
	if err != nil {
		return nil, err
	}
	if profile != nil {
		ranking, err := s.DB.GetEntryRanking(entryID, profile.ID)
		if err != nil {
			return nil, err
		}
		if ranking != nil {
			note.Relevance = relevanceFor(ranking)
		}
	}

	if entry.ContentKind == "paper" {
		identifiers, err := s.DB.ListPaperIdentifiers(entryID)
		if err != nil {
			return nil, err
		}
		for _, identifier := range identifiers {
			note.Identifiers = append(note.Identifiers,
				identifier.Scheme+":"+identifier.Value)
		}
		references, err := s.DB.ListPaperReferences(entryID)
		if err != nil {
			return nil, err
		}
		for _, reference := range references {
			title := deref(reference.TargetTitle)
			if title == "" {
				title = reference.CitedTitle
			}
			if title == "" {
				title = "Untitled paper"
			}
			note.References = append(note.References, vault.Reference{
				Title: title, ExternalScheme: reference.ExternalScheme,
				ExternalID: reference.ExternalID, CitedURL: reference.CitedURL,
				CitedStatus:       deref(reference.CitedStatus),
				CitedExportState:  deref(reference.CitedExportState),
				CitedObsidianPath: deref(reference.CitedObsidianPath),
			})
		}
	}

	export, err := s.DB.GetObsidianExport(entryID)
	if err != nil {
		return nil, err
	}
	if export != nil && export.RelativePath != "" {
		note.ObsidianRelativePath = export.RelativePath
	}
	return note, nil
}

// relevanceFor renders a ranking's human-readable reasons.
func relevanceFor(ranking *store.Ranking) *vault.Relevance {
	result := &vault.Relevance{
		Score: &ranking.Score, Bucket: ranking.Bucket,
		Model: ranking.Model, ScoredAt: ranking.ScoredAt,
	}
	var explanation struct {
		MatchedInterests []string `json:"matched_interests"`
		IncludeMatches   []string `json:"include_matches"`
		NeverShowMatches []string `json:"never_show_matches"`
		Decision         string   `json:"decision"`
	}
	if json.Unmarshal(ranking.Explanation, &explanation) != nil {
		return result
	}
	seen := map[string]bool{}
	add := func(value string) {
		if value != "" && !seen[value] {
			seen[value] = true
			result.Reasons = append(result.Reasons, value)
		}
	}
	for _, value := range explanation.MatchedInterests {
		add("Matched interest: " + value)
	}
	for _, value := range explanation.IncludeMatches {
		add("Included phrase: " + value)
	}
	for _, value := range explanation.NeverShowMatches {
		add("Excluded phrase: " + value)
	}
	add(explanation.Decision)
	return result
}

// ExportEntry writes a kept entry's note.
func (s *Service) ExportEntry(entryID int64) (string, error) {
	return s.exportEntry(entryID, true, false)
}

// exportEntry writes a note and records the outcome.
//
// completesEnrichment marks the write that finishes a background enrichment
// job; until then a note is reported pending, so a consumer never sees a stale
// note as current while enrichment is still in flight.
func (s *Service) exportEntry(entryID int64, resyncCiting, completesEnrichment bool) (string, error) {
	note, err := s.noteFor(entryID)
	if err != nil {
		return "", err
	}
	exporter, err := s.Exporter()
	if err != nil {
		return "", err
	}

	exportState, err := s.DB.GetObsidianExport(entryID)
	if err != nil {
		return "", err
	}
	destination, err := exporter.Destination(note)
	if err != nil {
		return "", s.recordExportFailure(entryID, exporter, err, resyncCiting)
	}
	destinationRelative, err := exporter.RelativePath(destination)
	if err != nil {
		return "", s.recordExportFailure(entryID, exporter, err, resyncCiting)
	}

	// Notes written by older versions used mutable, title-based paths. Move
	// them through the same archive/restore lifecycle so annotations survive
	// the one-time relocation to a stable key.
	if exportState != nil && exportState.State == "synced" &&
		exportState.RelativePath != "" && exportState.RelativePath != destinationRelative {
		if _, archiveErr := s.archiveEntry(entryID, exporter); archiveErr != nil {
			// A tampered legacy path is untrusted input, not a reason to
			// refuse writing a new, safe note.
			if !isPathEscape(archiveErr) {
				return "", archiveErr
			}
		}
		exportState, err = s.DB.GetObsidianExport(entryID)
		if err != nil {
			return "", err
		}
	}

	if exportState != nil && exportState.State == "archived" {
		archives, err := s.DB.ListObsidianArchives(entryID)
		if err != nil {
			return "", err
		}
		for _, archive := range archives {
			if archive.RestoredAt != nil {
				continue
			}
			if _, err := exporter.Restore(note, archive.ArchivePath); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				break
			}
			if _, err := s.DB.MarkObsidianArchiveRestored(archive.ID); err != nil {
				return "", err
			}
			break
		}
		// Re-read the note so a restored file is merged, not overwritten.
		if note, err = s.noteFor(entryID); err != nil {
			return "", err
		}
	}

	result, err := exporter.Export(note)
	if err != nil {
		return "", s.recordExportFailure(entryID, exporter, err, resyncCiting)
	}
	relativePath, err := exporter.RelativePath(result.Path)
	if err != nil {
		return "", err
	}

	state := "synced"
	if !completesEnrichment && s.isEnriching(entryID) {
		state = "pending"
	}
	vaultPath := exporter.VaultPath
	if _, err := s.DB.UpsertObsidianExport(entryID, store.ExportUpdate{
		State: state, VaultPath: &vaultPath, RelativePath: &relativePath,
		ContentHash: &result.ContentHash,
	}); err != nil {
		return "", err
	}
	if resyncCiting {
		s.resyncCitingNotes(entryID)
	}
	return result.Path, nil
}

// recordExportFailure persists the reason a note could not be written.
//
// A conflict is distinguished from an ordinary failure because it needs the
// user to resolve a file, not just a retry.
func (s *Service) recordExportFailure(entryID int64, exporter *vault.Exporter, cause error, resyncCiting bool) error {
	state := "failed"
	var conflict *vault.ConflictError
	if errors.As(cause, &conflict) {
		state = "conflict"
	}
	vaultPath := exporter.VaultPath
	if _, err := s.DB.UpsertObsidianExport(entryID, store.ExportUpdate{
		State: state, VaultPath: &vaultPath, Error: cause.Error(),
	}); err != nil {
		return err
	}
	if resyncCiting {
		s.resyncCitingNotes(entryID)
	}
	return cause
}

func isPathEscape(err error) bool {
	return err != nil && contains(err.Error(), "escapes managed root")
}

func contains(haystack, needle string) bool {
	return len(needle) <= len(haystack) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}

// resyncCitingNotes promotes or demotes citation links after a target changes.
//
// A cited paper becoming kept and synced turns external links into real
// Obsidian links in every note that cites it, and un-keeping reverses that.
func (s *Service) resyncCitingNotes(citedEntryID int64) {
	citing, err := s.DB.ListCitingEntryIDs(citedEntryID)
	if err != nil {
		return
	}
	for _, citingEntryID := range citing {
		entry, err := s.DB.GetEntry(citingEntryID)
		if err != nil || entry == nil || entry.Status != "kept" {
			continue
		}
		export, err := s.DB.GetObsidianExport(citingEntryID)
		if err != nil || export == nil || export.State != "synced" {
			continue
		}
		// resyncCiting is false to keep this one level deep.
		_, _ = s.exportEntry(citingEntryID, false, false)
	}
}

// archiveEntry moves a note out of the vault, keeping it recoverable.
func (s *Service) archiveEntry(entryID int64, exporter *vault.Exporter) (string, error) {
	export, err := s.DB.GetObsidianExport(entryID)
	if err != nil {
		return "", err
	}
	if export == nil {
		return "", nil
	}
	switch export.State {
	case "pending", "synced", "archive_pending":
	default:
		return "", nil
	}
	relativePath := export.RelativePath
	if relativePath == "" {
		entry, err := s.DB.GetEntry(entryID)
		if err != nil {
			return "", err
		}
		if entry != nil {
			relativePath = deref(entry.ExportedPath)
		}
	}
	if exporter == nil {
		if exporter, err = s.Exporter(); err != nil {
			return "", err
		}
	}
	note, err := s.noteFor(entryID)
	if err != nil {
		return "", err
	}

	vaultPath := exporter.VaultPath
	// Mark the intent first so an interrupted archive is retryable.
	if _, err := s.DB.UpsertObsidianExport(entryID, store.ExportUpdate{
		State: "archive_pending", VaultPath: &vaultPath, RelativePath: &relativePath,
	}); err != nil {
		return "", err
	}
	result, err := exporter.Archive(note, relativePath)
	if err != nil {
		if _, updateErr := s.DB.UpsertObsidianExport(entryID, store.ExportUpdate{
			State: "archive_pending", VaultPath: &vaultPath,
			RelativePath: &relativePath, Error: err.Error(),
		}); updateErr != nil {
			return "", updateErr
		}
		s.resyncCitingNotes(entryID)
		return "", err
	}
	destination := ""
	if result != nil {
		archiveRelative, err := exporter.ArchiveRelativePath(result.ArchivePath)
		if err != nil {
			return "", err
		}
		if _, err := s.DB.AddObsidianArchive(entryID, relativePath, archiveRelative); err != nil {
			return "", err
		}
		destination = result.ArchivePath
	}
	if _, err := s.DB.UpsertObsidianExport(entryID, store.ExportUpdate{
		State: "archived", VaultPath: &vaultPath, RelativePath: &relativePath,
	}); err != nil {
		return "", err
	}
	s.resyncCitingNotes(entryID)
	return destination, nil
}

// RetryObsidian re-attempts the sync appropriate to the entry's current status.
func (s *Service) RetryObsidian(entryID int64) (string, error) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil {
		return "", err
	}
	if entry == nil {
		return "", &NotFoundError{EntryID: entryID}
	}
	if entry.Status == "kept" {
		return s.ExportEntry(entryID)
	}
	return s.archiveEntry(entryID, nil)
}

// ExportKept writes every kept entry's note.
func (s *Service) ExportKept() ([]string, error) {
	kept, err := s.DB.ListEntries(store.EntryFilter{Status: "kept"})
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range kept {
		path, err := s.ExportEntry(entry.ID)
		if err != nil {
			return paths, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// ObsidianSettings describes the configured vault.
type ObsidianSettings struct {
	VaultPath        string `json:"vault_path"`
	DefaultVaultPath string `json:"default_vault_path"`
	ArchivePath      string `json:"archive_path"`
	Configured       bool   `json:"configured"`
}

// GetObsidianSettings reports the active vault configuration.
func (s *Service) GetObsidianSettings() (*ObsidianSettings, error) {
	exporter, err := s.Exporter()
	if err != nil {
		return nil, err
	}
	configured, err := s.DB.GetSetting("obsidian", nil)
	if err != nil {
		return nil, err
	}
	return &ObsidianSettings{
		VaultPath:        exporter.VaultPath,
		DefaultVaultPath: s.DefaultVault,
		ArchivePath:      exporter.ArchiveRoot,
		Configured:       configured,
	}, nil
}

// ConfigureObsidian points Herald at a different vault.
//
// Managed notes are archived out of the old vault before kept notes are
// restored into the new one, so annotations follow the move. A partial failure
// leaves every completed step recoverable through the normal retry path.
func (s *Service) ConfigureObsidian(vaultPath string) (*ObsidianSettings, error) {
	requested := vaultPath
	if !filepath.IsAbs(requested) {
		return nil, fmt.Errorf("Obsidian vault path must be absolute")
	}
	resolved, err := filepath.Abs(requested)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("Obsidian vault path must be an existing directory")
	}
	if link, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = link
	}

	previous, err := s.Exporter()
	if err != nil {
		return nil, err
	}
	if resolved == previous.VaultPath {
		return s.GetObsidianSettings()
	}
	next, err := vault.NewExporter(resolved, s.ArchiveRoot)
	if err != nil {
		return nil, err
	}

	exports, err := s.DB.ListObsidianExports()
	if err != nil {
		return nil, err
	}
	for _, export := range exports {
		if export.State != "synced" || export.RelativePath == "" {
			continue
		}
		if _, err := s.archiveEntry(export.EntryID, previous); err != nil {
			continue
		}
	}

	if err := s.DB.SetSetting("obsidian", map[string]string{"vault_path": resolved}); err != nil {
		return nil, err
	}
	s.exporterOverride = nil

	kept, err := s.DB.ListEntries(store.EntryFilter{Status: "kept"})
	if err != nil {
		return nil, err
	}
	for _, entry := range kept {
		// The selected setting stays valid; individual failures persist and
		// can be retried without losing the kept status.
		_, _ = s.ExportEntry(entry.ID)
	}

	settings, err := s.GetObsidianSettings()
	if err != nil {
		return nil, err
	}
	settings.VaultPath = next.VaultPath
	return settings, nil
}

// ReconcileObsidian brings the vault back in line with reading state.
func (s *Service) ReconcileObsidian() error {
	entries, err := s.DB.ListEntries(store.EntryFilter{})
	if err != nil {
		return err
	}
	exporter, err := s.Exporter()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		export, err := s.DB.GetObsidianExport(entry.ID)
		if err != nil {
			return err
		}
		if entry.Status == "kept" {
			note, err := s.noteFor(entry.ID)
			if err != nil {
				continue
			}
			destination, err := exporter.Destination(note)
			if err != nil {
				continue
			}
			expected, err := exporter.RelativePath(destination)
			if err != nil {
				continue
			}
			recordedPath := ""
			if export != nil {
				recordedPath = export.RelativePath
			}
			needsSync := export == nil
			if !needsSync {
				switch export.State {
				case "pending", "failed", "archived", "archive_pending":
					needsSync = true
				case "synced":
					// A note that moved, vanished, or belongs to another
					// vault must be rewritten.
					noteFile := filepath.Join(exporter.VaultPath, filepath.FromSlash(recordedPath))
					info, statErr := os.Stat(noteFile)
					needsSync = export.VaultPath != exporter.VaultPath ||
						recordedPath != expected || statErr != nil || !info.Mode().IsRegular()
				}
			}
			if needsSync {
				if _, err := s.ExportEntry(entry.ID); err != nil {
					continue
				}
			}
			s.startKeptEnrichment(entry)
			continue
		}
		if export != nil {
			switch export.State {
			case "pending", "synced", "archive_pending":
				_, _ = s.archiveEntry(entry.ID, exporter)
			}
		}
	}
	return nil
}

// ImportPaper resolves and stores a manually supplied paper.
func (s *Service) ImportPaper(value string) (*paper.Result, error) {
	return s.Importer.Import(value, false)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
