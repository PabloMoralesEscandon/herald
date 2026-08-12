package vault

import (
	"strings"
	"testing"

	"github.com/PabloMoralesEscandon/herald/internal/fulltext"
)

// noteWithFullText is a kept paper whose body cites two works: one that is
// itself kept and synced in the vault, and one that is not.
func noteWithFullText() *Note {
	note := keptNote()
	note.References = []Reference{
		{
			Key: "doi:10.1145/cited", Label: "1", Title: "The Cited Paper",
			ExternalScheme: "doi", ExternalID: "10.1145/cited",
			CitedStatus: "kept", CitedExportState: "synced",
			CitedObsidianPath: "Herald/Papers/doi-10.1145-cited.md",
		},
		{
			Key: "arxiv:2401.09999", Label: "2", Title: "A Paper Not In The Vault",
			ExternalScheme: "arxiv", ExternalID: "2401.09999",
			CitedStatus: "unread",
		},
	}
	note.FullText = &FullText{
		State: "extracted", SourceKind: "arxiv-html",
		SourceURL: "https://arxiv.org/html/2402.00001", Format: "html",
		Markdown: "### 1 Introduction\n\nEarlier work " +
			fulltext.CitePlaceholder("doi:10.1145/cited", "1") +
			" introduced this, extended later by " +
			fulltext.CitePlaceholder("arxiv:2401.09999", "2") + ".",
	}
	return note
}

func TestCitationBecomesAnObsidianLinkWhenTheCitedPaperIsInTheVault(t *testing.T) {
	rendered := Render(noteWithFullText())

	// The whole point of the feature: following the citation inside one note
	// opens the other note, and the graph view shows the connection.
	if !strings.Contains(rendered, "[[Herald/Papers/doi-10.1145-cited|1]]") {
		t.Fatalf("citation did not become an Obsidian link:\n%s", rendered)
	}
	// A paper that is not in the vault keeps the text the article printed.
	if !strings.Contains(rendered, "extended later by 2.") {
		t.Fatalf("uncited-in-vault marker lost its printed form:\n%s", rendered)
	}
	if strings.Contains(rendered, "herald:cite") {
		t.Fatalf("a citation placeholder leaked into the note:\n%s", rendered)
	}
}

func TestPrintedBracketsDoNotWrapACitationLink(t *testing.T) {
	note := keptNote()
	note.References = []Reference{{
		Key: "doi:10.1145/cited", Title: "The Cited Paper",
		CitedStatus: "kept", CitedExportState: "synced",
		CitedObsidianPath: "Herald/Papers/doi-10.1145-cited.md",
	}}
	note.FullText = &FullText{
		State: "extracted",
		Markdown: "Earlier work [" +
			fulltext.CitePlaceholder("doi:10.1145/cited", "1") + "] introduced this.",
	}
	rendered := Render(note)

	// "[[[note|1]]]" is ambiguous markup: the printed brackets are dropped and
	// the link carries the citation.
	if strings.Contains(rendered, "[[[") {
		t.Fatalf("printed brackets were left wrapping a link:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Earlier work [[Herald/Papers/doi-10.1145-cited|1]] introduced this.") {
		t.Fatalf("unexpected citation rendering:\n%s", rendered)
	}
}

func TestBracketsAroundOrdinaryTextAreUntouched(t *testing.T) {
	note := keptNote()
	note.FullText = &FullText{
		State:    "extracted",
		Markdown: "The interval [0, 1] and the matrix element [i] stay as written.",
	}
	rendered := Render(note)

	if !strings.Contains(rendered, "The interval [0, 1] and the matrix element [i] stay as written.") {
		t.Fatalf("ordinary brackets were altered:\n%s", rendered)
	}
}

func TestFullTextSectionCarriesItsProvenance(t *testing.T) {
	rendered := Render(noteWithFullText())

	if !strings.Contains(rendered, "## Full text") {
		t.Fatalf("full text section missing:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Extracted from the arXiv HTML rendering") {
		t.Fatalf("extraction provenance missing:\n%s", rendered)
	}
	if !strings.Contains(rendered, `fulltext_state: "extracted"`) {
		t.Fatalf("frontmatter should record the extraction state:\n%s", rendered)
	}
	if !strings.Contains(rendered, `fulltext_source: "arxiv-html"`) {
		t.Fatalf("frontmatter should record the source:\n%s", rendered)
	}
	// The article's own headings nest under Herald's section heading.
	if !strings.Contains(rendered, "### 1 Introduction") {
		t.Fatalf("article heading depth changed:\n%s", rendered)
	}
}

func TestNoticeAsksForAPDFWhenNoOpenCopyExists(t *testing.T) {
	note := keptNote()
	note.FullText = &FullText{State: "needs_pdf"}
	rendered := Render(note)

	if !strings.Contains(rendered, "> [!warning] Full text not available automatically") {
		t.Fatalf("expected a callout explaining the missing body:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Upload the PDF in Herald") {
		t.Fatalf("the notice should say what would fix it:\n%s", rendered)
	}
	if strings.Contains(rendered, "## Full text") {
		t.Fatalf("no body was extracted, so there should be no body section:\n%s", rendered)
	}
	if !strings.Contains(rendered, `fulltext_state: "needs_pdf"`) {
		t.Fatalf("frontmatter should record the state:\n%s", rendered)
	}
}

func TestFailedExtractionExplainsItself(t *testing.T) {
	note := keptNote()
	note.FullText = &FullText{State: "failed", Error: "the publisher returned HTTP 503"}
	rendered := Render(note)

	if !strings.Contains(rendered, "Full text could not be extracted") {
		t.Fatalf("expected a failure callout:\n%s", rendered)
	}
	if !strings.Contains(rendered, "HTTP 503") {
		t.Fatalf("the recorded reason should be visible:\n%s", rendered)
	}
}

func TestNewsNotesNeverGainAFullTextSection(t *testing.T) {
	note := keptNote()
	note.ContentKind = "news"
	note.FullText = &FullText{State: "needs_pdf"}
	rendered := Render(note)

	if strings.Contains(rendered, "Full text") {
		t.Fatalf("news has no article to extract:\n%s", rendered)
	}
}

func TestTruncatedExtractionSaysSo(t *testing.T) {
	note := keptNote()
	note.FullText = &FullText{
		State: "extracted", SourceKind: "arxiv-pdf",
		Markdown: "Body text that was cut short.", Truncated: true,
	}
	rendered := Render(note)

	if !strings.Contains(rendered, "longer than Herald's extraction limit") {
		t.Fatalf("a truncated article should say so rather than appear complete:\n%s", rendered)
	}
}

func TestReferenceListLinksKeptPapersAndLabelsEntries(t *testing.T) {
	rendered := Render(noteWithFullText())

	if !strings.Contains(rendered, "- [[Herald/Papers/doi-10.1145-cited|(1) The Cited Paper]]") {
		t.Fatalf("kept reference should be an internal link with its label:\n%s", rendered)
	}
	// The label is parenthesized: brackets are not legal inside a Markdown
	// link's text, and would make this line render as broken markup.
	if !strings.Contains(rendered, "- [(2) A Paper Not In The Vault](https://arxiv.org/abs/2401.09999)") {
		t.Fatalf("external reference should link to its resolver:\n%s", rendered)
	}
	if strings.Contains(rendered, "[[2]") {
		t.Fatalf("a bracketed label would be ambiguous markup:\n%s", rendered)
	}
}

func TestExtractedBodySurvivesAResync(t *testing.T) {
	// Re-rendering must be stable: an unchanged note that is exported twice
	// should produce identical bytes, or every sync would show as a change in
	// the user's vault history.
	first := Render(noteWithFullText())
	second := Render(noteWithFullText())
	if first != second {
		t.Fatal("rendering the same note twice produced different output")
	}
}

func TestCitationLinkPromotesWhenTheCitedPaperIsKeptLater(t *testing.T) {
	note := noteWithFullText()
	// Before: the second reference is not in the vault, so it reads as text.
	if strings.Count(Render(note), "[[Herald/Papers/") != 2 {
		t.Fatalf("expected exactly the one cited note to be linked:\n%s", Render(note))
	}

	// After the user keeps the cited paper, the same note must link it.
	note.References[1].CitedStatus = "kept"
	note.References[1].CitedExportState = "synced"
	note.References[1].CitedObsidianPath = "Herald/Papers/arxiv-2401.09999.md"

	rendered := Render(note)
	if !strings.Contains(rendered, "[[Herald/Papers/arxiv-2401.09999|2]]") {
		t.Fatalf("keeping a cited paper should turn its markers into links:\n%s", rendered)
	}
}

func TestPlaceholdersNeverReachTheVaultEvenWhenUnknown(t *testing.T) {
	note := keptNote()
	note.FullText = &FullText{
		State:    "extracted",
		Markdown: "Text citing " + fulltext.CitePlaceholder("doi:10.1/never-seen", "9") + ".",
	}
	rendered := Render(note)

	if strings.Contains(rendered, "herald:cite") {
		t.Fatalf("an unresolvable placeholder must still be replaced:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Text citing 9.") {
		t.Fatalf("unresolved citation should read as printed:\n%s", rendered)
	}
}
