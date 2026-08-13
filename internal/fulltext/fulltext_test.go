package fulltext

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// arXiv's HTML rendering marks up citations with anchors into its own
// bibliography, which is the case worth pinning: it is the one that produces
// exact links rather than heuristic ones.
const latexmlPage = `<!DOCTYPE html><html><head><title>Sparse Attention for Chips</title></head>
<body>
<div class="ltx_page_navbar">skip this navigation</div>
<div class="ltx_page_content">
<article class="ltx_document">
<h1 class="ltx_title ltx_title_document">Sparse Attention for Chips</h1>
<div class="ltx_authors">A. Researcher</div>
<div class="ltx_abstract"><p class="ltx_p">We study sparse attention on hardware.</p></div>
<section class="ltx_section" id="S1">
<h2 class="ltx_title ltx_title_section">1 Introduction</h2>
<div class="ltx_para"><p class="ltx_p">Earlier work
<cite class="ltx_cite">[<a href="#bib.bib1" class="ltx_ref">1</a>]</cite>
introduced the idea, later extended
<cite class="ltx_cite">[<a href="#bib.bib2" class="ltx_ref">2</a>]</cite>.
The energy is <math alttext="E = mc^2" display="inline"><mi>E</mi></math> throughout.</p></div>
<div class="ltx_para"><p class="ltx_p">A second paragraph with <strong>bold</strong> and
<a href="/other/page">a relative link</a>.</p></div>
</section>
<section class="ltx_bibliography" id="bib">
<h2 class="ltx_title">References</h2>
<ul class="ltx_biblist">
<li class="ltx_bibitem" id="bib.bib1"><span class="ltx_bibtag">[1]</span>
<span class="ltx_bibblock">J. Smith and A. Jones. Attention on silicon. Journal of Chips, 2019.
<a href="https://doi.org/10.1234/abcd">doi:10.1234/abcd</a></span></li>
<li class="ltx_bibitem" id="bib.bib2"><span class="ltx_bibtag">[2]</span>
<span class="ltx_bibblock">B. Writer. Follow-up study. arXiv preprint
<a href="https://arxiv.org/abs/2401.01234">arXiv:2401.01234</a>, 2024.</span></li>
</ul>
</section>
</article>
</div>
<footer class="ltx_page_footer">skip this footer</footer>
</body></html>`

func TestHTMLExtractionStructure(t *testing.T) {
	document, err := FromHTML([]byte(latexmlPage), "https://arxiv.org/html/2402.00001")
	if err != nil {
		t.Fatalf("FromHTML returned an error: %v", err)
	}
	if document.Title != "Sparse Attention for Chips" {
		t.Fatalf("unexpected title %q", document.Title)
	}
	if !strings.Contains(document.Abstract, "sparse attention on hardware") {
		t.Fatalf("abstract was not captured: %q", document.Abstract)
	}

	markdown := document.Markdown()
	if strings.Contains(markdown, "navigation") || strings.Contains(markdown, "skip this footer") {
		t.Fatalf("page furniture leaked into the body:\n%s", markdown)
	}
	if strings.Contains(markdown, "A. Researcher") {
		t.Fatalf("the author block belongs in note properties, not the body:\n%s", markdown)
	}
	if !strings.Contains(markdown, "### 1 Introduction") {
		t.Fatalf("section heading missing or at the wrong depth:\n%s", markdown)
	}
	if !strings.Contains(markdown, "$E = mc^2$") {
		t.Fatalf("inline math should carry the original LaTeX:\n%s", markdown)
	}
	if !strings.Contains(markdown, "**bold**") {
		t.Fatalf("emphasis was lost:\n%s", markdown)
	}
	if !strings.Contains(markdown, "https://arxiv.org/other/page") {
		t.Fatalf("relative link was not resolved:\n%s", markdown)
	}
	// Herald renders the reference list from its own citation edges.
	if strings.Contains(markdown, "## References") {
		t.Fatalf("the article's own reference list should not be duplicated:\n%s", markdown)
	}
}

func TestHTMLCitationsPointAtTheirBibliographyEntry(t *testing.T) {
	document, err := FromHTML([]byte(latexmlPage), "https://arxiv.org/html/2402.00001")
	if err != nil {
		t.Fatalf("FromHTML returned an error: %v", err)
	}
	if len(document.References) != 2 {
		t.Fatalf("expected 2 references, got %d", len(document.References))
	}
	if document.References[0].Key != "doi:10.1234/abcd" {
		t.Fatalf("a DOI reference should key on its DOI, got %q", document.References[0].Key)
	}
	if document.References[1].Key != "arxiv:2401.01234" {
		t.Fatalf("an arXiv reference should key on its identifier, got %q", document.References[1].Key)
	}

	markdown := document.Markdown()
	keys := CitationKeys(markdown)
	if len(keys) != 2 || keys[0] != "doi:10.1234/abcd" || keys[1] != "arxiv:2401.01234" {
		t.Fatalf("citations did not resolve to their entries: %v\n%s", keys, markdown)
	}
	if !strings.Contains(markdown, "{{herald:cite:doi:10.1234/abcd|1}}") {
		t.Fatalf("citation kept its printed label:\n%s", markdown)
	}
}

func TestResolveCitationsAlwaysReplacesPlaceholders(t *testing.T) {
	body := "Prior work " + CitePlaceholder("doi:10.1/x", "[1]") +
		" and " + CitePlaceholder("bib:unknown", "[2]") + "."

	resolved := ResolveCitations(body, func(key, display string) string {
		if key == "doi:10.1/x" {
			return "[[Herald/Papers/doi-10.1-x|" + display + "]]"
		}
		return ""
	})
	if strings.Contains(resolved, "herald:cite") {
		t.Fatalf("a placeholder survived into the note: %q", resolved)
	}
	if !strings.Contains(resolved, "[[Herald/Papers/doi-10.1-x|[1]]]") {
		t.Fatalf("known citation was not linked: %q", resolved)
	}
	// An unresolved citation keeps the text the article printed.
	if !strings.Contains(resolved, "and [2].") {
		t.Fatalf("unknown citation lost its printed form: %q", resolved)
	}
}

func TestStripCitationsLeavesPlainText(t *testing.T) {
	body := "See " + CitePlaceholder("doi:10.1/x", "[1]") + " for details."
	if got := StripCitations(body); got != "See [1] for details." {
		t.Fatalf("unexpected stripped text: %q", got)
	}
}

func TestPlaceholderFieldsCannotEscape(t *testing.T) {
	// A key or label carrying the placeholder's own delimiters must not be
	// able to terminate it early and smuggle a second placeholder in. The
	// delimiters are stripped, so what remains is one inert placeholder.
	placeholder := CitePlaceholder("doi:10.1/x}}evil", "[1]|{{herald:cite:other|x}}")

	if keys := CitationKeys(placeholder); len(keys) != 1 {
		t.Fatalf("expected exactly one placeholder, got %v in %q", keys, placeholder)
	}
	calls := 0
	resolved := ResolveCitations(placeholder, func(key, display string) string {
		calls++
		if strings.ContainsAny(key, "{}|") || strings.ContainsAny(display, "{}") {
			t.Fatalf("delimiters survived sanitizing: key=%q display=%q", key, display)
		}
		return "LINK"
	})
	if calls != 1 {
		t.Fatalf("expected one resolution, got %d", calls)
	}
	if resolved != "LINK" || strings.Contains(resolved, "herald:cite") {
		t.Fatalf("nested placeholder survived: %q", resolved)
	}
}

func TestParseReferenceExtractsIdentifiers(t *testing.T) {
	reference := ParseReference("12",
		`J. Smith, A. Jones. Deep learning for chip layout. In Proc. NeurIPS, 2019. doi:10.1145/1234.5678`, 12)

	if reference.DOI != "10.1145/1234.5678" {
		t.Fatalf("DOI not extracted: %q", reference.DOI)
	}
	if reference.Year != "2019" {
		t.Fatalf("year not extracted: %q", reference.Year)
	}
	if reference.Key != "doi:10.1145/1234.5678" {
		t.Fatalf("unexpected key: %q", reference.Key)
	}
	if !strings.Contains(strings.ToLower(reference.Title), "deep learning for chip layout") {
		t.Fatalf("title not recovered: %q", reference.Title)
	}
}

func TestParseReferenceWithoutIdentifierIsStable(t *testing.T) {
	raw := `K. Author. A paper with no identifiers at all. Journal of Things, 2011.`
	first := ParseReference("3", raw, 3)
	second := ParseReference("3", raw, 3)
	if first.Key != second.Key {
		t.Fatalf("keys must be stable across extractions: %q vs %q", first.Key, second.Key)
	}
	if !strings.HasPrefix(first.Key, "bib:") {
		t.Fatalf("expected a derived key, got %q", first.Key)
	}
	// A different reference must not collide with it.
	other := ParseReference("4", `K. Author. A different paper entirely. Journal, 2012.`, 4)
	if other.Key == first.Key {
		t.Fatal("two different references produced the same key")
	}
}

func TestNumericCitationLinking(t *testing.T) {
	references := []Reference{
		ParseReference("1", "A. One. First paper. 2019. doi:10.1/one", 1),
		ParseReference("2", "B. Two. Second paper. 2020. doi:10.1/two", 2),
		ParseReference("3", "C. Three. Third paper. 2021. doi:10.1/three", 3),
	}
	index := NewIndex(references)

	linked := index.Link("As shown in [1] and later [2, 3], the method works.")
	keys := CitationKeys(linked)
	if len(keys) != 3 {
		t.Fatalf("expected three citations, got %v in %q", keys, linked)
	}
	if !strings.Contains(StripCitations(linked), "[1] and later [2, 3]") {
		t.Fatalf("printed form changed: %q", StripCitations(linked))
	}
}

func TestNumericRangeLinksEveryEntry(t *testing.T) {
	var references []Reference
	for number := 1; number <= 4; number++ {
		references = append(references, ParseReference(
			fmt.Sprint(number),
			fmt.Sprintf("A. Author. Paper %d. 2020. doi:10.1/p%d", number, number), number))
	}
	index := NewIndex(references)

	linked := index.Link("Several works [1-3] agree.")
	if keys := CitationKeys(linked); len(keys) != 2 {
		// The endpoints are linked and the range is preserved as printed.
		t.Fatalf("expected the range endpoints to link, got %v in %q", keys, linked)
	}
	if !strings.Contains(StripCitations(linked), "[1-3]") {
		t.Fatalf("range lost its printed form: %q", StripCitations(linked))
	}
}

func TestUnresolvableBracketsAreLeftAlone(t *testing.T) {
	references := []Reference{
		ParseReference("1", "A. One. First paper. 2019. doi:10.1/one", 1),
	}
	index := NewIndex(references)

	// Bracketed numbers that are not citations are everywhere in scientific
	// prose. Linking one would put a wrong connection in the vault.
	for _, text := range []string{
		"the matrix element [2] is zero",
		"indices [0] and [7] differ",
		"the interval [1, 9] is closed",
	} {
		if linked := index.Link(text); linked != text {
			t.Fatalf("non-citation was rewritten: %q became %q", text, linked)
		}
	}
	if linked := index.Link("as in [1]"); !strings.Contains(linked, "herald:cite") {
		t.Fatalf("a real citation should still link: %q", linked)
	}
}

func TestAuthorYearCitationLinking(t *testing.T) {
	references := []Reference{
		ParseReference("", "Smith, J. and Jones, A. (2019). Attention on silicon. Journal of Chips.", 1),
		ParseReference("", "Writer, B. (2024). Follow-up study. Conference on Things.", 2),
	}
	index := NewIndex(references)

	parenthetical := index.Link("This was shown earlier (Smith and Jones, 2019).")
	if keys := CitationKeys(parenthetical); len(keys) != 1 {
		t.Fatalf("parenthetical citation did not link: %q", parenthetical)
	}
	narrative := index.Link("Writer (2024) extended the method.")
	if keys := CitationKeys(narrative); len(keys) != 1 {
		t.Fatalf("narrative citation did not link: %q", narrative)
	}
	if !strings.Contains(StripCitations(narrative), "Writer (2024) extended") {
		t.Fatalf("narrative citation lost its printed form: %q", StripCitations(narrative))
	}
}

func TestAmbiguousAuthorYearIsNotGuessed(t *testing.T) {
	references := []Reference{
		ParseReference("", "Smith, J. (2019). One paper. Journal A.", 1),
		ParseReference("", "Smith, R. (2019). Another paper entirely. Journal B.", 2),
	}
	index := NewIndex(references)

	text := "Prior work (Smith, 2019) is relevant."
	if linked := index.Link(text); linked != text {
		t.Fatalf("an ambiguous citation must be left alone, got %q", linked)
	}
}

var grobidArticleTEI = `<?xml version="1.0" encoding="UTF-8"?>
<TEI xmlns="http://www.tei-c.org/ns/1.0">
  <teiHeader><fileDesc><titleStmt><title type="main">Sparse Attention for Chips</title></titleStmt></fileDesc>
    <profileDesc><abstract><div><p>We study sparse attention on hardware.</p></div></abstract></profileDesc>
  </teiHeader>
  <text><body><div><head n="1">Introduction</head>
    <p>Earlier work <ref type="bibr" target="#b0">[1]</ref> introduced the idea, and a later study examined it further
    <ref type="bibr" target="#b1">[2]</ref> on real hardware. The first paragraph then stops here.</p>
    <p>` + strings.Repeat("The second paragraph and Body line 11 carry real content that must survive as useful extracted article text. ", 7) + `</p>
    <list><item>First measured result</item><item>Second measured result</item></list>
    <formula>E = mc^2</formula>
    <figure type="table"><head>Measured throughput</head><table>
      <row><cell>Design</cell><cell>Score</cell></row><row><cell>Ours</cell><cell>42</cell></row>
    </table></figure>
  </div></body><back><div type="references"><listBibl>
    <biblStruct xml:id="b0" n="1"><analytic><title level="a">Attention on silicon</title>
      <author><persName><forename>J.</forename><surname>Smith</surname></persName></author></analytic>
      <monogr><imprint><date when="2019"/></imprint></monogr><idno type="DOI">10.1234/ABCD</idno>
      <note type="raw_reference">J. Smith. Attention on silicon. Journal of Chips, 2019. doi:10.1234/abcd</note>
    </biblStruct>
    <biblStruct xml:id="b1" n="2"><analytic><title level="a">Follow-up study</title></analytic>
      <idno type="arXiv">2401.01234v2</idno>
      <note type="raw_reference">B. Writer. Follow-up study. arXiv:2401.01234, 2024.</note>
    </biblStruct>
  </listBibl></div></back></text>
</TEI>`

var testPDF = []byte("%PDF-1.7\nGROBID test fixture")

func testGROBIDProcessor() GROBIDProcessor {
	return GROBIDProcessorFunc(func([]byte) ([]byte, error) {
		return []byte(grobidArticleTEI), nil
	})
}

func TestPDFExtractionUsesGROBIDStructure(t *testing.T) {
	document, err := FromPDF(testPDF, testGROBIDProcessor())
	if err != nil {
		t.Fatalf("FromPDF returned an error: %v", err)
	}
	markdown := document.Markdown()

	if document.Title != "Sparse Attention for Chips" ||
		!strings.Contains(document.Abstract, "sparse attention") {
		t.Fatalf("GROBID header was not preserved: %+v", document)
	}
	for _, expected := range []string{
		"### 1 Introduction", "- First measured result", "- Second measured result",
		"$$\nE = mc^2\n$$", "*Measured throughput*", "| Design | Score |",
	} {
		if !strings.Contains(markdown, expected) {
			t.Fatalf("missing %q in GROBID markdown:\n%s", expected, markdown)
		}
	}

	if len(document.References) != 2 {
		t.Fatalf("expected 2 parsed references, got %d: %+v", len(document.References), document.References)
	}
	if document.References[0].Key != "doi:10.1234/abcd" {
		t.Fatalf("unexpected first reference key: %q", document.References[0].Key)
	}
	if document.References[1].Key != "arxiv:2401.01234" {
		t.Fatalf("unexpected second reference key: %q", document.References[1].Key)
	}

	keys := CitationKeys(markdown)
	if len(keys) != 2 {
		t.Fatalf("expected both in-text citations to link, got %v:\n%s", keys, markdown)
	}
	if !strings.Contains(StripCitations(markdown), "Earlier work [1]") ||
		strings.Contains(StripCitations(markdown), "herald:cite") {
		t.Fatalf("TEI citations were not preserved cleanly:\n%s", markdown)
	}
}

func TestGROBIDDocumentWithoutBodyIsReportedDistinctly(t *testing.T) {
	processor := GROBIDProcessorFunc(func([]byte) ([]byte, error) {
		return []byte(`<TEI xmlns="http://www.tei-c.org/ns/1.0"><text><body/></text></TEI>`), nil
	})
	_, err := FromPDF(testPDF, processor)
	if _, ok := err.(*ScannedError); !ok {
		t.Fatalf("expected a scanned-PDF report, got %T: %v", err, err)
	}
}

func TestInvalidGROBIDTEIIsRejected(t *testing.T) {
	processor := GROBIDProcessorFunc(func([]byte) ([]byte, error) {
		return []byte(`<TEI><text>`), nil
	})
	if _, err := FromPDF(testPDF, processor); err == nil {
		t.Fatal("expected malformed GROBID XML to fail")
	}
}

func TestCandidatesPreferArxivHTML(t *testing.T) {
	candidates := Candidates(Locators{
		ArxivID:       "2401.01234",
		OpenAccessPDF: "https://example.org/paper.pdf",
	})
	if len(candidates) != 3 {
		t.Fatalf("expected three candidates, got %d: %+v", len(candidates), candidates)
	}
	if candidates[0].Kind != SourceArxivHTML {
		t.Fatalf("HTML rendering should be tried first, got %q", candidates[0].Kind)
	}
	if candidates[1].Kind != SourceArxivPDF || candidates[2].Kind != SourceOpenAccess {
		t.Fatalf("unexpected candidate order: %+v", candidates)
	}
}

func TestCandidatesRejectNonHTTPS(t *testing.T) {
	candidates := Candidates(Locators{OpenAccessPDF: "http://example.org/paper.pdf"})
	if len(candidates) != 0 {
		t.Fatalf("plain HTTP must not be fetched: %+v", candidates)
	}
}

func TestExtractFallsThroughToTheNextSource(t *testing.T) {
	extractor := &Extractor{
		Fetch: func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
			if strings.Contains(url, "/html/") {
				return nil, fmt.Errorf("no HTML rendering exists")
			}
			return testPDF, nil
		},
		GROBID: testGROBIDProcessor(),
	}

	result, err := extractor.Extract(Locators{ArxivID: "2401.01234"})
	if err != nil {
		t.Fatalf("Extract returned an error: %v", err)
	}
	if result.SourceKind != SourceArxivPDF {
		t.Fatalf("expected the PDF fallback, got %q", result.SourceKind)
	}
	if len(result.PDF) == 0 {
		t.Fatal("the PDF bytes should be retained so the file can be kept")
	}
}

func TestExtractReportsNoOpenSource(t *testing.T) {
	extractor := &Extractor{
		Fetch: func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
			return nil, fmt.Errorf("HTTP 403")
		},
	}
	_, err := extractor.Extract(Locators{ArxivID: "2401.01234", PageURL: "https://example.org/paper"})

	noSource, ok := err.(*NoSourceError)
	if !ok {
		t.Fatalf("expected a NoSourceError, got %T: %v", err, err)
	}
	if len(noSource.Attempts) == 0 {
		t.Fatal("the failure should record what was tried")
	}
}

func TestExtractReportsGROBIDOutageAsServiceFailure(t *testing.T) {
	extractor := &Extractor{
		Fetch: func(string, map[string]string, int, time.Duration) ([]byte, error) {
			return testPDF, nil
		},
		GROBID: GROBIDProcessorFunc(func([]byte) ([]byte, error) {
			return nil, &GROBIDServiceError{Reason: "GROBID is unavailable"}
		}),
	}
	_, err := extractor.Extract(Locators{ArxivID: "2401.01234"})
	if _, ok := err.(*GROBIDServiceError); !ok {
		t.Fatalf("expected GROBIDServiceError, got %T: %v", err, err)
	}
}

func TestExtractUsesPageDeclaredPDF(t *testing.T) {
	extractor := &Extractor{
		Fetch: func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
			switch {
			case strings.HasSuffix(url, "/article"):
				return []byte(`<html><head>` +
					`<meta name="citation_pdf_url" content="https://example.org/article.pdf">` +
					`</head><body>abstract page</body></html>`), nil
			case strings.HasSuffix(url, "/article.pdf"):
				return testPDF, nil
			}
			return nil, fmt.Errorf("not found")
		},
		GROBID: testGROBIDProcessor(),
	}

	result, err := extractor.Extract(Locators{PageURL: "https://example.org/article"})
	if err != nil {
		t.Fatalf("Extract returned an error: %v", err)
	}
	if result.SourceKind != SourcePagePDF {
		t.Fatalf("expected the page-declared PDF, got %q", result.SourceKind)
	}
}

func TestFromUploadRejectsNonPDF(t *testing.T) {
	if _, err := FromUpload([]byte("this is a text file")); err == nil {
		t.Fatal("expected a non-PDF upload to be rejected")
	}
}

func TestMarkdownNestsHeadingsUnderTheNoteSection(t *testing.T) {
	document := &Document{}
	document.appendBlock(BlockHeading, 1, "Introduction")
	document.appendBlock(BlockParagraph, 0, "Body text.")
	document.appendBlock(BlockListItem, 0, "first")
	document.appendBlock(BlockListItem, 0, "second")

	markdown := document.Markdown()
	// The article's top-level heading must sit below the note's own "Full
	// text" heading rather than competing with it.
	if !strings.Contains(markdown, "### Introduction") {
		t.Fatalf("unexpected heading depth:\n%s", markdown)
	}
	if !strings.Contains(markdown, "- first\n- second") {
		t.Fatalf("consecutive list items should form one list:\n%s", markdown)
	}
}

func TestExtractionLimitsAreEnforced(t *testing.T) {
	document := &Document{}
	for index := range MaxBlocks + 100 {
		document.appendBlock(BlockParagraph, 0, fmt.Sprintf("paragraph %d", index))
	}
	if len(document.Blocks) > MaxBlocks {
		t.Fatalf("block limit was exceeded: %d", len(document.Blocks))
	}
	if !document.Truncated {
		t.Fatal("hitting the limit should be reported as truncation")
	}
}
