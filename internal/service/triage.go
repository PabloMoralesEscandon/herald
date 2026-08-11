package service

import (
	"encoding/json"

	"github.com/PabloMoralesEscandon/herald/internal/paper"
	"github.com/PabloMoralesEscandon/herald/internal/store"
)

// ChangeStatus applies a triage transition and synchronizes the vault.
//
// The whole transition holds the enrichment lock. A background enrichment that
// finishes at the same instant as an un-keep takes the same lock for its final
// status check, so it cannot recreate a note that was just archived.
func (s *Service) ChangeStatus(entryID int64, status string) (map[string]any, error) {
	s.transitionMutex.Lock()
	result, entry, err := s.changeStatusLocked(entryID, status)
	s.transitionMutex.Unlock()
	if err != nil {
		return nil, err
	}
	// Enrichment is started after releasing the lock; it takes the lock
	// itself for its own short critical sections.
	if entry != nil {
		s.startKeptEnrichment(entry)
	}
	return result, nil
}

// changeStatusLocked performs the transition. The caller holds the lock.
func (s *Service) changeStatusLocked(entryID int64, status string) (map[string]any, *store.Entry, error) {
	previous, err := s.DB.GetEntry(entryID)
	if err != nil {
		return nil, nil, err
	}
	if previous == nil {
		return nil, nil, &NotFoundError{EntryID: entryID}
	}
	// Reading state is committed before any filesystem work is attempted, so
	// a broken vault can never undo a Keep.
	updatedRow, err := s.DB.SetStatus(entryID, status)
	if err != nil {
		return nil, nil, err
	}
	if !updatedRow {
		return nil, nil, &NotFoundError{EntryID: entryID}
	}

	profile, err := s.DB.GetRelevanceProfile(previous.ContentKind)
	if err != nil {
		return nil, nil, err
	}
	if profile != nil {
		switch status {
		case "kept":
			err = s.DB.RecordRelevanceFeedback(entryID, profile.ID, "keep")
		case "discarded":
			err = s.DB.RecordRelevanceFeedback(entryID, profile.ID, "discard")
		default:
			_, err = s.DB.ClearRelevanceFeedback(entryID, profile.ID)
		}
		if err != nil {
			return nil, nil, err
		}
		if s.Relevance != nil {
			if _, err := s.Relevance.Start(previous.ContentKind); err != nil {
				return nil, nil, err
			}
		}
	}

	updated, err := s.DB.GetEntry(entryID)
	if err != nil {
		return nil, nil, err
	}
	var enrichCandidate *store.Entry
	switch {
	case status == "kept":
		// A sync failure is recorded, not propagated: the Keep stands.
		_, _ = s.exportEntry(entryID, true, false)
		enrichCandidate = updated
		s.markEnrichmentPending(updated)
	case previous.Status == "kept":
		_, _ = s.archiveEntry(entryID, nil)
	}

	// Re-read the entry: exporting sets exported_path and archiving clears it,
	// so the snapshot taken before that work would report a stale path.
	current, err := s.DB.GetEntry(entryID)
	if err != nil {
		return nil, nil, err
	}
	export, err := s.DB.GetObsidianExport(entryID)
	if err != nil {
		return nil, nil, err
	}
	return withObsidianExport(current, export), enrichCandidate, nil
}

// withObsidianExport merges an entry and its note state into the response
// shape the API documents.
func withObsidianExport(entry *store.Entry, export *store.Export) map[string]any {
	document, err := json.Marshal(entry)
	if err != nil {
		return nil
	}
	var result map[string]any
	if json.Unmarshal(document, &result) != nil {
		return nil
	}
	if export == nil {
		result["obsidian_export"] = nil
	} else {
		result["obsidian_export"] = export
	}
	return result
}

// enrichmentCandidate reports whether a kept entry should be enriched.
func (s *Service) enrichmentCandidate(entry *store.Entry) (string, bool) {
	if !s.AutoEnrichKept || entry == nil || entry.ContentKind != "paper" {
		return "", false
	}
	if entry.EnrichmentStatus == "enriched" {
		return "", false
	}
	url := entry.CanonicalURL
	if url == "" {
		url = entry.URL
	}
	if _, err := paper.ParseLocator(url); err != nil {
		return "", false
	}
	return url, true
}

// isEnriching reports whether a background enrichment is in flight.
func (s *Service) isEnriching(entryID int64) bool {
	s.enrichingMutex.Lock()
	defer s.enrichingMutex.Unlock()
	return s.enriching[entryID]
}

// claimEnrichment marks an entry as being enriched, reporting whether this
// caller won the claim.
func (s *Service) claimEnrichment(entryID int64) bool {
	s.enrichingMutex.Lock()
	defer s.enrichingMutex.Unlock()
	if s.enriching[entryID] {
		return false
	}
	s.enriching[entryID] = true
	return true
}

// releaseEnrichment clears the in-flight marker. It is idempotent.
func (s *Service) releaseEnrichment(entryID int64) {
	s.enrichingMutex.Lock()
	defer s.enrichingMutex.Unlock()
	delete(s.enriching, entryID)
}

// markEnrichmentPending records that a synced note is about to change.
//
// Enrichment and the note rewrite are one observable job: the importer must
// commit metadata before the note can be rendered, so consumers must not treat
// the already-synced note as current while that second step is in flight.
// The caller holds the lock.
func (s *Service) markEnrichmentPending(entry *store.Entry) {
	if _, ok := s.enrichmentCandidate(entry); !ok {
		return
	}
	if s.isEnriching(entry.ID) {
		return
	}
	export, err := s.DB.GetObsidianExport(entry.ID)
	if err != nil || export == nil {
		return
	}
	vaultPath, relativePath, contentHash := export.VaultPath, export.RelativePath, export.ContentHash
	_, _ = s.DB.UpsertObsidianExport(entry.ID, store.ExportUpdate{
		State: "pending", VaultPath: &vaultPath,
		RelativePath: &relativePath, ContentHash: &contentHash,
	})
}

// startKeptEnrichment begins background metadata enrichment for a kept paper.
//
// Keeping returns immediately with a baseline note; provider lookups happen
// here. A page or provider failure never undoes the Keep or removes the note.
func (s *Service) startKeptEnrichment(entry *store.Entry) {
	url, ok := s.enrichmentCandidate(entry)
	if !ok {
		return
	}
	entryID := entry.ID
	if !s.claimEnrichment(entryID) {
		return
	}
	s.enrichmentWait.Add(1)

	go func() {
		defer s.enrichmentWait.Done()
		defer s.releaseEnrichment(entryID)

		if _, err := s.Importer.Import(url, true); err != nil {
			_, _ = s.DB.SetEnrichmentState(entryID, "failed", store.EnrichmentState{
				Error: err.Error(),
			})
		}

		// Serialize the final status check and note write with triage. If the
		// user un-kept the paper during provider I/O, ChangeStatus has already
		// archived the note and this worker must not recreate it.
		s.transitionMutex.Lock()
		defer s.transitionMutex.Unlock()
		current, err := s.DB.GetEntry(entryID)
		if err != nil || current == nil || current.Status != "kept" {
			return
		}
		// Clear the in-flight marker before the final write so that write is
		// recorded as synced rather than pending.
		s.releaseEnrichment(entryID)
		_, _ = s.exportEntry(entryID, true, true)
	}()
}

// WaitForEnrichment blocks until background enrichment settles. It exists for
// tests and for an orderly shutdown.
func (s *Service) WaitForEnrichment() { s.enrichmentWait.Wait() }

// ListPaperReferences returns the works a paper cites.
func (s *Service) ListPaperReferences(entryID int64) ([]*store.PaperReference, error) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &NotFoundError{EntryID: entryID}
	}
	if entry.ContentKind != "paper" {
		return nil, &InvalidRequestError{Reason: "Only papers have citation references"}
	}
	return s.DB.ListPaperReferences(entryID)
}

// InvalidRequestError reports a request the caller can correct.
type InvalidRequestError struct{ Reason string }

func (e *InvalidRequestError) Error() string { return e.Reason }

// ReferenceImport is the response to adding one cited paper.
type ReferenceImport struct {
	Reference *store.PaperReference `json:"reference"`
	Import    *paper.Result         `json:"import"`
}

// AddPaperReference imports exactly one cited paper.
//
// It never crawls further: the new entry is unread, not kept, and not exported,
// and repeating the call returns the existing target unchanged.
func (s *Service) AddPaperReference(referenceID int64) (*ReferenceImport, error) {
	reference, err := s.DB.GetPaperReference(referenceID)
	if err != nil {
		return nil, err
	}
	if reference == nil {
		return nil, &NotFoundError{EntryID: referenceID}
	}

	if reference.CitedEntryID != nil {
		citedID := *reference.CitedEntryID
		entry, err := s.DB.GetEntry(citedID)
		if err != nil {
			return nil, err
		}
		identifiers, err := s.DB.ListPaperIdentifiers(citedID)
		if err != nil {
			return nil, err
		}
		keywords, err := s.DB.ListEntryKeywords(citedID)
		if err != nil {
			return nil, err
		}
		references, err := s.DB.ListPaperReferences(citedID)
		if err != nil {
			return nil, err
		}
		return &ReferenceImport{
			Reference: reference,
			Import: &paper.Result{
				Entry: entry, Created: false, Identifiers: identifiers,
				Keywords: keywords, References: references,
			},
		}, nil
	}

	var locator string
	switch reference.ExternalScheme {
	case "doi":
		locator = "doi:" + reference.ExternalID
	case "arxiv":
		locator = "arXiv:" + reference.ExternalID
	case "s2":
		locator = "https://www.semanticscholar.org/paper/" + reference.ExternalID
	default:
		locator = reference.CitedURL
	}
	if locator == "" {
		return nil, &paper.ImportError{
			Reason: "This reference has no DOI, arXiv ID, Semantic Scholar ID, or paper URL",
		}
	}

	result, err := s.ImportPaper(locator)
	if err != nil {
		return nil, err
	}
	// The selected edge stays authoritative even when the provider URL
	// redirects to a different canonical identifier during import.
	citedID := result.Entry.ID
	if _, err := s.DB.UpsertPaperReference(reference.CitingEntryID, reference.ReferenceKey,
		store.UpsertReferenceInput{
			CitedEntryID: &citedID, ExternalScheme: reference.ExternalScheme,
			ExternalID: reference.ExternalID, CitedTitle: reference.CitedTitle,
			CitedURL: reference.CitedURL, Position: reference.Position,
			Provider: reference.Provider,
		}); err != nil {
		return nil, err
	}
	updated, err := s.DB.GetPaperReference(referenceID)
	if err != nil {
		return nil, err
	}
	return &ReferenceImport{Reference: updated, Import: result}, nil
}
