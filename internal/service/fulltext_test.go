package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/fulltext"
	"github.com/PabloMoralesEscandon/herald/internal/store"
)

// articleHTML is an arXiv-style rendering: two citations, each anchored to the
// bibliography entry it names.
var articleHTML = `<html><head><title>A Chiplet Interconnect</title></head><body>
<article class="ltx_document">
<h1 class="ltx_title ltx_title_document">A Chiplet Interconnect</h1>
<section class="ltx_section"><h2 class="ltx_title ltx_title_section">1 Introduction</h2>
<div class="ltx_para"><p class="ltx_p">` + longSentence + `
Earlier work <cite class="ltx_cite">[<a href="#bib.bib1">1</a>]</cite> introduced the idea,
extended later <cite class="ltx_cite">[<a href="#bib.bib2">2</a>]</cite>.</p></div>
</section>
<section class="ltx_bibliography"><h2>References</h2><ul class="ltx_biblist">
<li class="ltx_bibitem" id="bib.bib1"><span class="ltx_bibtag">[1]</span>
<span class="ltx_bibblock">J. Smith. Attention on silicon. Journal of Chips, 2019.
<a href="https://doi.org/10.1145/cited">doi:10.1145/cited</a></span></li>
<li class="ltx_bibitem" id="bib.bib2"><span class="ltx_bibtag">[2]</span>
<span class="ltx_bibblock">B. Writer. A follow-up study of interconnects.
<a href="https://arxiv.org/abs/2401.09999">arXiv:2401.09999</a>, 2024.</span></li>
</ul></section>
</article></body></html>`

// longSentence pads the article past the minimum length an extraction must
// reach to be considered useful.
var longSentence = strings.Repeat(
	"This paragraph describes the interconnect design in enough detail to count as an article body. ", 8)

// staticExtractor answers every candidate with the same document.
func staticExtractor(document []byte) *fulltext.Extractor {
	return &fulltext.Extractor{
		Fetch: func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
			return document, nil
		},
	}
}

// failingExtractor refuses every candidate, as if no open copy existed.
func failingExtractor() *fulltext.Extractor {
	return &fulltext.Extractor{
		Fetch: func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
			return nil, fmt.Errorf("HTTP 403")
		},
	}
}

// seedPaperWithIdentifier gives an entry an arXiv identity, which is what
// makes it an extraction candidate.
func seedPaperWithIdentifier(t *testing.T, svc *Service) int64 {
	t.Helper()
	entryID := seedEntry(t, svc, "paper")
	if _, err := svc.DB.AddPaperIdentifier(entryID, "arxiv", "2402.00001", true); err != nil {
		t.Fatalf("AddPaperIdentifier: %v", err)
	}
	return entryID
}

func keptNoteText(t *testing.T, svc *Service, vaultPath string, entryID int64) string {
	t.Helper()
	export, err := svc.DB.GetObsidianExport(entryID)
	if err != nil {
		t.Fatalf("GetObsidianExport: %v", err)
	}
	if export == nil || export.RelativePath == "" {
		t.Fatal("the kept entry has no note")
	}
	document, err := os.ReadFile(filepath.Join(vaultPath, filepath.FromSlash(export.RelativePath)))
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	return string(document)
}

func TestKeepingAPaperExtractsItsFullText(t *testing.T) {
	svc, vaultPath := newService(t)
	svc.AutoExtractFullText = true
	svc.Extractor = staticExtractor([]byte(articleHTML))
	entryID := seedPaperWithIdentifier(t, svc)

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	record, err := svc.DB.GetFullText(entryID)
	if err != nil || record == nil {
		t.Fatalf("no extraction row: %v", err)
	}
	if record.State != "extracted" {
		t.Fatalf("expected extracted, got %q (%s)", record.State, record.Error)
	}
	if record.SourceKind != fulltext.SourceArxivHTML {
		t.Fatalf("unexpected source: %q", record.SourceKind)
	}
	if record.ReferenceCount != 2 {
		t.Fatalf("expected 2 stored references, got %d", record.ReferenceCount)
	}

	note := keptNoteText(t, svc, vaultPath, entryID)
	if !strings.Contains(note, "## Full text") {
		t.Fatalf("the note has no extracted body:\n%s", note)
	}
	if !strings.Contains(note, "### 1 Introduction") {
		t.Fatalf("the article's structure was lost:\n%s", note)
	}
	if strings.Contains(note, "herald:cite") {
		t.Fatalf("a citation placeholder reached the vault:\n%s", note)
	}
}

func TestCitationBecomesALinkOnceTheCitedPaperIsKept(t *testing.T) {
	svc, vaultPath := newService(t)
	svc.AutoExtractFullText = true
	svc.Extractor = staticExtractor([]byte(articleHTML))
	entryID := seedPaperWithIdentifier(t, svc)

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	// Before the cited paper exists in Herald, the marker reads as printed.
	if note := keptNoteText(t, svc, vaultPath, entryID); strings.Contains(note, "[[Herald/Papers/doi") {
		t.Fatalf("nothing should be linked yet:\n%s", note)
	}

	// The user adds and keeps the cited paper. Extraction is disabled for it
	// so this test covers only the linking.
	citedID := addCitedPaper(t, svc, "doi", "10.1145/cited", "Attention on Silicon")
	svc.AutoExtractFullText = false
	if _, err := svc.ChangeStatus(citedID, "kept"); err != nil {
		t.Fatalf("keep cited paper: %v", err)
	}
	svc.WaitForEnrichment()

	// Keeping the cited paper must rewrite the citing note's markers.
	note := keptNoteText(t, svc, vaultPath, entryID)
	if !strings.Contains(note, "Earlier work [[Herald/Papers/doi 10.1145 cited|1]] introduced the idea") {
		t.Fatalf("the in-text citation did not become a link:\n%s", note)
	}
	// The second citation is still only a bibliography entry, so it stays as
	// the article printed it.
	if !strings.Contains(note, "extended later [2].") {
		t.Fatalf("an uncited-in-vault marker should read as printed:\n%s", note)
	}
}

// addCitedPaper creates a second paper Herald can resolve a citation to.
func addCitedPaper(t *testing.T, svc *Service, scheme, identifier, title string) int64 {
	t.Helper()
	sourceID, err := svc.DB.AddSource(store.AddSourceInput{
		Title: "Manual Imports", URL: "herald://manual-imports",
		Category: "Manual Imports", ContentKind: "paper", Adapter: "manual",
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	key := scheme + ":" + identifier
	entryID, _, err := svc.DB.UpsertEntry(store.UpsertEntryInput{
		SourceID: sourceID, GUID: key, URL: "https://doi.org/" + identifier,
		CanonicalURL: "https://doi.org/" + identifier, CanonicalKey: &key,
		Title: title, ContentKind: "paper",
	})
	if err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	if _, err := svc.DB.AddPaperIdentifier(entryID, scheme, identifier, true); err != nil {
		t.Fatalf("AddPaperIdentifier: %v", err)
	}
	if _, err := svc.DB.ReconcilePaperReferences(); err != nil {
		t.Fatalf("ReconcilePaperReferences: %v", err)
	}
	return entryID
}

func TestNoOpenCopyAsksForAPDFWithoutUndoingTheKeep(t *testing.T) {
	svc, vaultPath := newService(t)
	svc.AutoExtractFullText = true
	svc.Extractor = failingExtractor()
	entryID := seedPaperWithIdentifier(t, svc)

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	entry, err := svc.DB.GetEntry(entryID)
	if err != nil || entry == nil {
		t.Fatalf("GetEntry: %v", err)
	}
	// The rule this package is built on: reading state is the user's.
	if entry.Status != "kept" {
		t.Fatalf("a failed extraction must not undo the Keep, status is %q", entry.Status)
	}

	record, err := svc.DB.GetFullText(entryID)
	if err != nil || record == nil {
		t.Fatalf("no extraction row: %v", err)
	}
	if record.State != "needs_pdf" {
		t.Fatalf("expected needs_pdf, got %q", record.State)
	}
	if record.Error == "" {
		t.Fatal("the row should record why no copy could be read")
	}

	note := keptNoteText(t, svc, vaultPath, entryID)
	if !strings.Contains(note, "Full text not available automatically") {
		t.Fatalf("the note should explain the missing body:\n%s", note)
	}
	if !strings.Contains(note, `fulltext_state: "needs_pdf"`) {
		t.Fatalf("the note should record the state:\n%s", note)
	}
}

func TestUploadedPDFCompletesTheNote(t *testing.T) {
	svc, vaultPath := newService(t)
	svc.AutoExtractFullText = true
	svc.Extractor = failingExtractor()
	entryID := seedPaperWithIdentifier(t, svc)

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	result, err := svc.UploadPaperPDF(entryID, uploadablePDF())
	if err != nil {
		t.Fatalf("UploadPaperPDF: %v", err)
	}
	if result.FullText.State != "extracted" {
		t.Fatalf("expected extracted, got %q (%s)", result.FullText.State, result.FullText.Error)
	}
	if result.FullText.SourceKind != fulltext.SourceUpload {
		t.Fatalf("the note should record that the user supplied the file, got %q",
			result.FullText.SourceKind)
	}

	// The stored PDF is the evidence behind the note's text.
	if result.FullText.PDFPath == "" {
		t.Fatal("the uploaded PDF should be kept")
	}
	if info, err := os.Stat(result.FullText.PDFPath); err != nil {
		t.Fatalf("stored PDF is missing: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("a stored PDF should not be world-readable, got %v", info.Mode().Perm())
	}

	note := keptNoteText(t, svc, vaultPath, entryID)
	if !strings.Contains(note, "## Full text") {
		t.Fatalf("the uploaded text did not reach the note:\n%s", note)
	}
	if !strings.Contains(note, "Extracted from a PDF you uploaded") {
		t.Fatalf("the note should say where the text came from:\n%s", note)
	}
	if strings.Contains(note, "Full text not available automatically") {
		t.Fatalf("the notice should be gone once the text exists:\n%s", note)
	}
}

func TestUploadRejectsFilesThatAreNotPDFs(t *testing.T) {
	svc, _ := newService(t)
	entryID := seedPaperWithIdentifier(t, svc)
	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}

	if _, err := svc.UploadPaperPDF(entryID, []byte("just some text")); err == nil {
		t.Fatal("expected a non-PDF upload to be rejected")
	}
}

func TestUploadIsRejectedForNews(t *testing.T) {
	svc, _ := newService(t)
	entryID := seedEntry(t, svc, "news")
	if _, err := svc.UploadPaperPDF(entryID, uploadablePDF()); err == nil {
		t.Fatal("news has no article to extract")
	}
}

func TestExtractedBibliographyMergesWithProviderReferences(t *testing.T) {
	svc, _ := newService(t)
	svc.AutoExtractFullText = true
	svc.Extractor = staticExtractor([]byte(articleHTML))
	entryID := seedPaperWithIdentifier(t, svc)

	// A provider already reported one of the two cited works.
	if _, err := svc.DB.UpsertPaperReference(entryID, "doi:10.1145/cited",
		store.UpsertReferenceInput{
			ExternalScheme: "doi", ExternalID: "10.1145/cited",
			CitedTitle: "Attention On Silicon", Provider: "semantic-scholar",
		}); err != nil {
		t.Fatalf("UpsertPaperReference: %v", err)
	}

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	references, err := svc.DB.ListPaperReferences(entryID)
	if err != nil {
		t.Fatalf("ListPaperReferences: %v", err)
	}
	// Two cited works, not three: the extracted entry merged onto the
	// provider's edge rather than duplicating it.
	if len(references) != 2 {
		t.Fatalf("expected 2 citation edges, got %d: %+v", len(references), references)
	}
	for _, reference := range references {
		if reference.BibLabel == "" {
			t.Fatalf("the printed label should be recorded: %+v", reference)
		}
	}
	// The provider's better title survives the merge.
	for _, reference := range references {
		if reference.ReferenceKey == "doi:10.1145/cited" && reference.CitedTitle != "Attention On Silicon" {
			t.Fatalf("the provider's title should not be overwritten: %q", reference.CitedTitle)
		}
	}
}

func TestUnkeepingDuringExtractionDoesNotRecreateTheNote(t *testing.T) {
	svc, vaultPath := newService(t)
	svc.AutoExtractFullText = true

	release := make(chan struct{})
	svc.Extractor = &fulltext.Extractor{
		Fetch: func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
			<-release
			return []byte(articleHTML), nil
		},
	}
	entryID := seedPaperWithIdentifier(t, svc)

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	// Un-keep while the fetch is still blocked.
	if _, err := svc.ChangeStatus(entryID, "read"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	close(release)
	svc.WaitForEnrichment()

	export, err := svc.DB.GetObsidianExport(entryID)
	if err != nil {
		t.Fatalf("GetObsidianExport: %v", err)
	}
	if export != nil && export.State == "synced" {
		t.Fatalf("an un-kept entry must not have a synced note, state is %q", export.State)
	}
	papers := filepath.Join(vaultPath, "Herald", "Papers")
	entries, err := os.ReadDir(papers)
	if err == nil && len(entries) > 0 {
		t.Fatalf("the archived note was recreated by the extraction: %v", entries)
	}
}

func TestExtractionIsSkippedWithoutAnIdentifier(t *testing.T) {
	svc, _ := newService(t)
	svc.AutoExtractFullText = true
	svc.Extractor = staticExtractor([]byte(articleHTML))
	// No arXiv identifier, and the canonical URL is not a publication page
	// Herald can read.
	entryID := seedEntry(t, svc, "paper")

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	record, err := svc.DB.GetFullText(entryID)
	if err != nil {
		t.Fatalf("GetFullText: %v", err)
	}
	if record != nil && record.State == "extracted" {
		t.Fatal("nothing should have been extracted without a readable source")
	}
}

func TestNewsIsNeverExtracted(t *testing.T) {
	svc, _ := newService(t)
	svc.AutoExtractFullText = true
	svc.Extractor = staticExtractor([]byte(articleHTML))
	entryID := seedEntry(t, svc, "news")

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	record, err := svc.DB.GetFullText(entryID)
	if err != nil {
		t.Fatalf("GetFullText: %v", err)
	}
	if record != nil {
		t.Fatalf("news should have no extraction row, got %q", record.State)
	}
}

func TestRetryReExtracts(t *testing.T) {
	svc, _ := newService(t)
	svc.AutoExtractFullText = true
	svc.Extractor = failingExtractor()
	entryID := seedPaperWithIdentifier(t, svc)

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	// The source becomes available; a retry must pick it up.
	svc.Extractor = staticExtractor([]byte(articleHTML))
	result, err := svc.RetryFullText(entryID)
	if err != nil {
		t.Fatalf("RetryFullText: %v", err)
	}
	if result.FullText.State != "extracted" {
		t.Fatalf("expected extracted after retry, got %q (%s)",
			result.FullText.State, result.FullText.Error)
	}
	if result.FullText.Error != "" {
		t.Fatalf("a successful retry should clear the earlier error: %q", result.FullText.Error)
	}
}

func TestExtractionIsNotRepeatedForAlreadyExtractedPapers(t *testing.T) {
	svc, _ := newService(t)
	svc.AutoExtractFullText = true

	fetches := 0
	svc.Extractor = &fulltext.Extractor{
		Fetch: func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
			fetches++
			return []byte(articleHTML), nil
		},
	}
	entryID := seedPaperWithIdentifier(t, svc)

	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()
	before := fetches

	// Un-keeping and keeping again must not re-download an article Herald has.
	if _, err := svc.ChangeStatus(entryID, "read"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	if _, err := svc.ChangeStatus(entryID, "kept"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	svc.WaitForEnrichment()

	if fetches != before {
		t.Fatalf("re-keeping refetched the article: %d extra requests", fetches-before)
	}
}

// uploadablePDF builds a small PDF with enough text to be a usable article.
func uploadablePDF() []byte {
	var content strings.Builder
	for index := range 14 {
		fmt.Fprintf(&content,
			"BT /F1 10 Tf 72 %d Td (This line of the uploaded paper carries real body text for extraction.) Tj ET\n",
			700-index*14)
	}
	stream := content.String()

	objects := []string{
		"<</Type/Catalog/Pages 2 0 R>>",
		"<</Type/Pages/Kids[3 0 R]/Count 1>>",
		"<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
			"/Resources<</Font<</F1 5 0 R>>>>/Contents 4 0 R>>",
		fmt.Sprintf("<</Length %d>>\nstream\n%s\nendstream", len(stream), stream),
		"<</Type/Font/Subtype/Type1/BaseFont/Helvetica/Encoding/WinAnsiEncoding>>",
	}

	var out strings.Builder
	out.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(objects)+1)
	for index, body := range objects {
		offsets[index+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", index+1, body)
	}
	xrefAt := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for index := 1; index <= len(objects); index++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&out, "trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xrefAt)
	return []byte(out.String())
}
