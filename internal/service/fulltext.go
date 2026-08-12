package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/fulltext"
	"github.com/PabloMoralesEscandon/herald/internal/paper"
	"github.com/PabloMoralesEscandon/herald/internal/store"
)

// Full-text extraction runs in the background after a paper is kept, on the
// same principle as the rest of this package: the Keep is committed first, and
// nothing that happens afterwards can undo it. An article with no openly
// available copy is not a failure to hide — the note is written either way,
// and the extraction row records that the user's own PDF is what would finish
// the job.

// MaxUploadedPDFBytes bounds a PDF the user supplies.
const MaxUploadedPDFBytes = 100 << 20

// pdfFileMode keeps stored PDFs readable only by the user who ran Herald.
const pdfFileMode = 0o600

// FullTextResult is the outcome of one extraction, as the API reports it.
type FullTextResult struct {
	FullText *store.FullText `json:"fulltext"`
	Entry    map[string]any  `json:"entry"`
}

// extractionLocators gathers everything an extraction can work from.
func (s *Service) extractionLocators(entry *store.Entry) (fulltext.Locators, bool) {
	if entry == nil || entry.ContentKind != "paper" {
		return fulltext.Locators{}, false
	}
	locators := fulltext.Locators{CanonicalURL: entry.CanonicalURL}
	if locators.CanonicalURL == "" {
		locators.CanonicalURL = entry.URL
	}

	identifiers, err := s.DB.ListPaperIdentifiers(entry.ID)
	if err != nil {
		return fulltext.Locators{}, false
	}
	for _, identifier := range identifiers {
		switch identifier.Scheme {
		case "arxiv":
			locators.ArxivID = identifier.Value
		case "doi":
			locators.DOI = identifier.Value
		}
	}

	// A publication page is inspected only for the PDF it declares itself.
	pageURL := entry.CanonicalURL
	if pageURL == "" {
		pageURL = entry.URL
	}
	if strings.HasPrefix(strings.ToLower(pageURL), "https://") &&
		!strings.Contains(strings.ToLower(pageURL), "arxiv.org") {
		locators.PageURL = pageURL
	}

	// A provider's open-access location is normally already cached from the
	// enrichment that just ran, so asking for it costs no request.
	if s.Importer != nil {
		switch {
		case locators.DOI != "":
			locators.OpenAccessPDF = s.Importer.OpenAccessPDF("doi", locators.DOI)
		case locators.ArxivID != "":
			locators.OpenAccessPDF = s.Importer.OpenAccessPDF("arxiv", locators.ArxivID)
		}
	}

	hasSource := locators.ArxivID != "" || locators.OpenAccessPDF != "" || locators.PageURL != ""
	return locators, hasSource
}

// fullTextCandidate reports whether a kept entry should have text extracted.
func (s *Service) fullTextCandidate(entry *store.Entry) bool {
	if !s.AutoExtractFullText || entry == nil || entry.ContentKind != "paper" {
		return false
	}
	existing, err := s.DB.GetFullText(entry.ID)
	if err != nil {
		return false
	}
	if existing == nil {
		return true
	}
	// Text already extracted stays; a user-supplied PDF is never overwritten
	// by a later automatic attempt.
	switch existing.State {
	case "extracted", "needs_pdf", "extracting":
		return false
	}
	return true
}

// runFullTextExtraction fetches and stores one paper's article text.
//
// It reports no error to its caller: every outcome is recorded on the
// extraction row, which is what the note and the dashboard read.
func (s *Service) runFullTextExtraction(entryID int64) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil || entry == nil {
		return
	}
	if _, err := s.DB.UpsertFullText(entryID, store.FullTextUpdate{
		State: "extracting", CountAttempt: true,
	}); err != nil {
		return
	}

	locators, hasSource := s.extractionLocators(entry)
	if !hasSource {
		s.recordNeedsPDF(entryID,
			"This paper has no arXiv identifier, open-access location, or public page for Herald to read")
		return
	}

	extractor := s.fullTextExtractor()
	result, err := extractor.Extract(locators)
	if err != nil {
		var noSource *fulltext.NoSourceError
		var scanned *fulltext.ScannedError
		switch {
		case errors.As(err, &noSource):
			s.recordNeedsPDF(entryID, noSourceReason(noSource))
		case errors.As(err, &scanned):
			s.recordNeedsPDF(entryID, scanned.Reason)
		default:
			// A transient problem is worth retrying, so it is kept separate
			// from "no open copy exists".
			_, _ = s.DB.UpsertFullText(entryID, store.FullTextUpdate{
				State: "failed", Error: err.Error(),
			})
		}
		return
	}
	s.storeExtraction(entryID, result)
}

// noSourceReason renders a no-open-copy failure for a person to read.
func noSourceReason(err *fulltext.NoSourceError) string {
	if len(err.Attempts) == 0 {
		return err.Reason
	}
	return err.Reason + " (" + strings.Join(err.Attempts, "; ") + ")"
}

func (s *Service) recordNeedsPDF(entryID int64, reason string) {
	_, _ = s.DB.UpsertFullText(entryID, store.FullTextUpdate{
		State: "needs_pdf", Error: reason,
	})
}

// fullTextExtractor returns the configured extractor, defaulting to Herald's
// SSRF-guarded fetcher.
func (s *Service) fullTextExtractor() *fulltext.Extractor {
	if s.Extractor != nil {
		return s.Extractor
	}
	// paper.FetchPublicDocument is the same guarded fetcher the metadata
	// providers use: public HTTPS only, re-validated on every redirect hop.
	return &fulltext.Extractor{Fetch: paper.FetchPublicDocument}
}

// storeExtraction persists a successful extraction and its bibliography.
func (s *Service) storeExtraction(entryID int64, result *fulltext.Result) {
	document := result.Document
	markdown := document.Markdown()

	// The bibliography is merged into Herald's citation edges first, because
	// merging can change the key a reference is stored under, and the stored
	// text has to cite the key that survived.
	remapped, referenceCount := s.mergeBibliography(entryID, document.References)
	markdown = fulltext.RemapCitationKeys(markdown, remapped)

	pdfPath := ""
	pdfBytes := int64(0)
	if len(result.PDF) > 0 {
		if path, err := s.storePDF(entryID, result.PDF); err == nil {
			pdfPath, pdfBytes = path, int64(len(result.PDF))
		}
	}

	sourceKind, sourceURL := result.SourceKind, result.SourceURL
	format := string(document.Format)
	characterCount := int64(document.TextLength())
	truncated := document.Truncated

	if _, err := s.DB.UpsertFullText(entryID, store.FullTextUpdate{
		State: "extracted", SourceKind: &sourceKind, SourceURL: &sourceURL,
		Format: &format, Markdown: &markdown, CharacterCount: &characterCount,
		ReferenceCount: &referenceCount, Truncated: &truncated,
		PDFPath: &pdfPath, PDFBytes: &pdfBytes, MarkExtracted: true,
	}); err != nil {
		return
	}
}

// mergeBibliography records the article's own reference list as citation
// edges, merging each entry with the edge a provider already reported for the
// same work.
//
// It returns the key changes the merge forced, so the stored text can be
// rewritten to match, and how many references the article listed.
func (s *Service) mergeBibliography(entryID int64, references []fulltext.Reference) (map[string]string, int64) {
	remapped := map[string]string{}
	if len(references) == 0 {
		return remapped, 0
	}

	existing, err := s.DB.ListPaperReferences(entryID)
	if err != nil {
		return remapped, 0
	}
	byKey := map[string]*store.PaperReference{}
	byTitle := map[string]*store.PaperReference{}
	for _, reference := range existing {
		byKey[reference.ReferenceKey] = reference
		for _, candidate := range []string{reference.CitedTitle, deref(reference.TargetTitle)} {
			if normalized := fulltext.NormalizeTitle(candidate); normalized != "" {
				byTitle[normalized] = reference
			}
		}
	}

	stored := int64(0)
	for _, reference := range references {
		key := reference.Key
		scheme, identifier := "", ""
		switch {
		case reference.DOI != "":
			scheme, identifier = "doi", reference.DOI
		case reference.ArxivID != "":
			scheme, identifier = "arxiv", reference.ArxivID
		}

		// Without an identifier of its own, an entry can still be the same
		// work a provider already reported. Matching on the normalized title
		// keeps one edge rather than creating a near-duplicate.
		var match *store.PaperReference
		if current, ok := byKey[key]; ok {
			match = current
		} else if scheme == "" {
			if current, ok := byTitle[fulltext.NormalizeTitle(reference.Title)]; ok &&
				fulltext.NormalizeTitle(reference.Title) != "" {
				match = current
			}
		}
		if match != nil && match.ReferenceKey != key {
			remapped[key] = match.ReferenceKey
			key = match.ReferenceKey
		}

		input := store.UpsertReferenceInput{
			ExternalScheme: scheme, ExternalID: identifier,
			CitedTitle: reference.Title, CitedURL: reference.URL,
			Provider: "fulltext",
		}
		if match != nil {
			// Provider-supplied details are more reliable than parsed ones, so
			// they are only filled in where they are missing.
			input.CitedEntryID = match.CitedEntryID
			if input.ExternalScheme == "" {
				input.ExternalScheme, input.ExternalID = match.ExternalScheme, match.ExternalID
			}
			if match.CitedTitle != "" {
				input.CitedTitle = match.CitedTitle
			}
			if match.CitedURL != "" {
				input.CitedURL = match.CitedURL
			}
			if match.Provider != "" && match.Provider != "fulltext" {
				input.Provider = match.Provider
			}
		}
		// The article's own numbering is what its citation markers print, so
		// it orders the reference list better than a provider's arbitrary one.
		if position, err := strconv.ParseInt(reference.Label, 10, 64); err == nil && position > 0 {
			input.Position = &position
		} else if match != nil {
			input.Position = match.Position
		}

		referenceID, err := s.DB.UpsertPaperReference(entryID, key, input)
		if err != nil {
			continue
		}
		if err := s.DB.SetReferenceBibliography(referenceID, reference.Label, reference.Raw); err != nil {
			continue
		}
		stored++
	}
	if _, err := s.DB.ReconcilePaperReferences(); err != nil {
		return remapped, stored
	}
	return remapped, stored
}

// storePDF writes an extracted or uploaded PDF next to the database.
//
// The file is kept because it is the evidence behind the note's text: it lets
// a re-extraction run offline, and it means a PDF the user supplied is not
// lost the moment it has been read.
func (s *Service) storePDF(entryID int64, document []byte) (string, error) {
	root := s.pdfRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	digest := sha256.Sum256(document)
	name := fmt.Sprintf("%d-%s.pdf", entryID, hex.EncodeToString(digest[:6]))
	path := filepath.Join(root, name)

	// A temporary file and a rename keep a partial write from being mistaken
	// for a stored PDF.
	temporary, err := os.CreateTemp(root, "."+name+".*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(pdfFileMode); err != nil {
		temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(document); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Service) pdfRoot() string {
	if s.PDFRoot != "" {
		return s.PDFRoot
	}
	return filepath.Join(filepath.Dir(s.DB.Path), "pdfs")
}

// UploadPaperPDF stores a PDF the user supplied and extracts its text.
//
// This is the answer to a paper Herald could not read: the user has legitimate
// access to the article, and this brings their copy into the same pipeline the
// automatic sources use, including citation linking.
func (s *Service) UploadPaperPDF(entryID int64, document []byte) (*FullTextResult, error) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &NotFoundError{EntryID: entryID}
	}
	if entry.ContentKind != "paper" {
		return nil, &InvalidRequestError{Reason: "Only papers have an article to extract"}
	}
	if len(document) == 0 {
		return nil, &InvalidRequestError{Reason: "The uploaded file is empty"}
	}
	if len(document) > MaxUploadedPDFBytes {
		return nil, &InvalidRequestError{Reason: "The uploaded PDF is larger than Herald's limit"}
	}
	if !fulltext.IsPDF(document) {
		return nil, &InvalidRequestError{Reason: "That file is not a PDF"}
	}

	if _, err := s.DB.UpsertFullText(entryID, store.FullTextUpdate{
		State: "extracting", CountAttempt: true,
	}); err != nil {
		return nil, err
	}
	result, err := fulltext.FromUpload(document)
	if err != nil {
		// The upload failing leaves the entry asking for a PDF, because that
		// is still what would fix it.
		s.recordNeedsPDF(entryID, err.Error())
		return nil, &InvalidRequestError{Reason: err.Error()}
	}

	s.storeExtraction(entryID, result)
	s.resyncAfterExtraction(entryID)
	return s.fullTextResult(entryID)
}

// RetryFullText re-runs extraction for one paper.
func (s *Service) RetryFullText(entryID int64) (*FullTextResult, error) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &NotFoundError{EntryID: entryID}
	}
	if entry.ContentKind != "paper" {
		return nil, &InvalidRequestError{Reason: "Only papers have an article to extract"}
	}
	if s.isExtracting(entryID) {
		return nil, &InvalidRequestError{Reason: "Herald is already extracting this paper"}
	}
	if !s.claimExtraction(entryID) {
		return nil, &InvalidRequestError{Reason: "Herald is already extracting this paper"}
	}
	defer s.releaseExtraction(entryID)

	s.runFullTextExtraction(entryID)
	s.resyncAfterExtraction(entryID)
	return s.fullTextResult(entryID)
}

// GetFullText reports one paper's extraction state.
func (s *Service) GetFullText(entryID int64) (*FullTextResult, error) {
	entry, err := s.DB.GetEntry(entryID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &NotFoundError{EntryID: entryID}
	}
	return s.fullTextResult(entryID)
}

// FullTextMarkdown returns the extracted body with its citations resolved to
// plain text, for previewing in the dashboard.
func (s *Service) FullTextMarkdown(entryID int64) (string, error) {
	record, err := s.DB.GetFullText(entryID)
	if err != nil || record == nil {
		return "", err
	}
	return fulltext.StripCitations(record.Markdown), nil
}

func (s *Service) fullTextResult(entryID int64) (*FullTextResult, error) {
	record, err := s.DB.GetFullText(entryID)
	if err != nil {
		return nil, err
	}
	entry, err := s.EntryWithObsidianState(entryID)
	if err != nil {
		return nil, err
	}
	return &FullTextResult{FullText: record, Entry: entry}, nil
}

// resyncAfterExtraction rewrites a kept paper's note so the new body and its
// citation links appear.
func (s *Service) resyncAfterExtraction(entryID int64) {
	s.transitionMutex.Lock()
	defer s.transitionMutex.Unlock()

	entry, err := s.DB.GetEntry(entryID)
	if err != nil || entry == nil || entry.Status != "kept" {
		return
	}
	_, _ = s.exportEntry(entryID, true, false)
}

// The in-flight set for extraction is separate from the enrichment one: a
// retry requested from the dashboard must be rejected while a background
// extraction of the same paper is running, but must not be confused with the
// metadata enrichment that precedes it.

func (s *Service) isExtracting(entryID int64) bool {
	s.enrichingMutex.Lock()
	defer s.enrichingMutex.Unlock()
	return s.extracting[entryID]
}

func (s *Service) claimExtraction(entryID int64) bool {
	s.enrichingMutex.Lock()
	defer s.enrichingMutex.Unlock()
	if s.extracting == nil {
		s.extracting = map[int64]bool{}
	}
	if s.extracting[entryID] {
		return false
	}
	s.extracting[entryID] = true
	return true
}

func (s *Service) releaseExtraction(entryID int64) {
	s.enrichingMutex.Lock()
	defer s.enrichingMutex.Unlock()
	delete(s.extracting, entryID)
}

// ExtractPendingFullText works through papers whose text has not been
// extracted yet. It backs the command line and runs nothing in parallel, so a
// long backlog stays a polite client of every source it reads.
func (s *Service) ExtractPendingFullText(limit int) (int, error) {
	kept, err := s.DB.ListEntries(store.EntryFilter{Status: "kept", ContentKind: "paper"})
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, entry := range kept {
		if limit > 0 && processed >= limit {
			break
		}
		record, err := s.DB.GetFullText(entry.ID)
		if err != nil {
			return processed, err
		}
		// A paper waiting for the user's PDF is skipped: retrying it would
		// reach the same closed door.
		if record != nil {
			switch record.State {
			case "extracted", "needs_pdf", "extracting":
				continue
			}
		}
		if !s.claimExtraction(entry.ID) {
			continue
		}
		s.runFullTextExtraction(entry.ID)
		s.releaseExtraction(entry.ID)
		s.resyncAfterExtraction(entry.ID)
		processed++
	}
	return processed, nil
}
