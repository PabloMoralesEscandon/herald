package fulltext

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/urlx"
)

// Where full text may come from.
//
// Every candidate below is a copy the publisher has itself made openly
// available: arXiv's own HTML and PDF renderings, an open-access location a
// free metadata provider reports, or the PDF a publication page links from its
// own citation_pdf_url metadata. Herald sends no credentials, follows nothing
// that asks for a login, and never tries to reach a copy that is not offered.
// When no open copy exists the extraction fails cleanly and the user is asked
// for theirs — that is the designed outcome, not a fallback to work around.

// SourceKind names where an extraction's text came from. It is stored with the
// note so the provenance of the text in a user's vault is never a guess.
const (
	SourceArxivHTML  = "arxiv-html"
	SourceArxivPDF   = "arxiv-pdf"
	SourceOpenAccess = "open-access-pdf"
	SourcePagePDF    = "page-pdf"
	SourceUpload     = "upload"
)

// Fetcher retrieves a document. It matches the signature of Herald's
// SSRF-guarded fetcher so the caller passes that in directly, and tests pass a
// function that never touches the network.
type Fetcher func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error)

// Locators are the identifiers and URLs an extraction can work from.
type Locators struct {
	ArxivID string
	DOI     string
	// OpenAccessPDF is a direct open-access PDF a metadata provider reported.
	OpenAccessPDF string
	// PageURL is the article's publication page, inspected only for its own
	// declared PDF link.
	PageURL      string
	CanonicalURL string
}

// Candidate is one place full text might be read from.
type Candidate struct {
	Kind   string
	URL    string
	Format Format
}

// Size and time limits for remote documents.
const (
	MaxPDFBytes  = 40 << 20
	MaxHTMLBytes = 12 << 20
	fetchTimeout = 45 * time.Second
)

// Candidates lists the open sources to try, best first.
//
// arXiv's HTML rendering comes first because it states its own structure and
// marks every citation with the bibliography entry it points at, which no PDF
// can do.
func Candidates(locators Locators) []Candidate {
	var candidates []Candidate
	seen := map[string]bool{}
	add := func(kind, url string, format Format) {
		url = strings.TrimSpace(url)
		if url == "" || seen[url] || urlx.Scheme(url) != "https" {
			return
		}
		seen[url] = true
		candidates = append(candidates, Candidate{Kind: kind, URL: url, Format: format})
	}

	if arxiv := strings.TrimSpace(locators.ArxivID); arxiv != "" {
		add(SourceArxivHTML, "https://arxiv.org/html/"+arxiv, FormatHTML)
		add(SourceArxivPDF, "https://arxiv.org/pdf/"+arxiv, FormatPDF)
	}
	add(SourceOpenAccess, locators.OpenAccessPDF, FormatPDF)
	return candidates
}

// Extractor runs an extraction against the open sources.
type Extractor struct {
	Fetch   Fetcher
	Timeout time.Duration
}

// Result is a completed extraction.
type Result struct {
	Document   *Document
	SourceKind string
	SourceURL  string
	// PDF is the document bytes when the source was a PDF, so the caller can
	// keep the file it extracted from.
	PDF []byte
}

// NoSourceError reports that no open copy of the article could be read.
//
// It is the error that drives the dashboard to ask the user for the PDF, so it
// carries what was tried in a form a person can act on.
type NoSourceError struct {
	Reason   string
	Attempts []string
}

func (e *NoSourceError) Error() string { return e.Reason }

// Extract reads the article from the best open source available.
func (e *Extractor) Extract(locators Locators) (*Result, error) {
	if e.Fetch == nil {
		return nil, &NoSourceError{Reason: "Herald has no way to fetch the article"}
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = fetchTimeout
	}

	candidates := Candidates(locators)
	var attempts []string

	for _, candidate := range candidates {
		result, err := e.tryCandidate(candidate, timeout)
		if err == nil {
			return result, nil
		}
		attempts = append(attempts, fmt.Sprintf("%s: %s", candidate.Kind, err.Error()))
	}

	// A publication page may declare its own PDF. That link is the publisher's
	// own statement that the file is the openly available copy.
	if pdfURL, err := e.declaredPDF(locators, timeout); err == nil && pdfURL != "" {
		candidate := Candidate{Kind: SourcePagePDF, URL: pdfURL, Format: FormatPDF}
		result, err := e.tryCandidate(candidate, timeout)
		if err == nil {
			return result, nil
		}
		attempts = append(attempts, fmt.Sprintf("%s: %s", candidate.Kind, err.Error()))
	}

	reason := "Herald could not find an openly available copy of this paper"
	if len(attempts) > 0 {
		reason = "Herald could not read an openly available copy of this paper"
	}
	return nil, &NoSourceError{Reason: reason, Attempts: attempts}
}

func (e *Extractor) tryCandidate(candidate Candidate, timeout time.Duration) (*Result, error) {
	limit, accept := MaxHTMLBytes, "text/html, application/xhtml+xml"
	if candidate.Format == FormatPDF {
		limit, accept = MaxPDFBytes, "application/pdf"
	}
	document, err := e.Fetch(candidate.URL, map[string]string{"Accept": accept}, limit, timeout)
	if err != nil {
		return nil, err
	}

	// The declared format is a hint, not a promise: an arXiv HTML URL for a
	// paper without an HTML rendering answers with something else entirely.
	extracted, err := Parse(document, candidate.URL)
	if err != nil {
		return nil, err
	}
	if extracted.TextLength() < MinUsefulTextRuns {
		return nil, &Error{Reason: "the fetched document had almost no text"}
	}

	result := &Result{Document: extracted, SourceKind: candidate.Kind, SourceURL: candidate.URL}
	if extracted.Format == FormatPDF {
		result.PDF = document
	}
	return result, nil
}

// Parse extracts a document from bytes whose format is detected from content.
func Parse(document []byte, sourceURL string) (*Document, error) {
	switch {
	case IsPDF(document):
		return FromPDF(document)
	case looksLikeHTML(document):
		return FromHTML(document, sourceURL)
	default:
		return nil, &Error{Reason: "the document is neither HTML nor a PDF"}
	}
}

// IsPDF reports whether bytes begin with the PDF signature.
func IsPDF(document []byte) bool {
	// A few producers emit junk before the header, which readers tolerate.
	window := document
	if len(window) > 1024 {
		window = window[:1024]
	}
	return bytes.Contains(window, []byte("%PDF-"))
}

func looksLikeHTML(document []byte) bool {
	window := document
	if len(window) > 4096 {
		window = window[:4096]
	}
	lowered := bytes.ToLower(window)
	return bytes.Contains(lowered, []byte("<html")) ||
		bytes.Contains(lowered, []byte("<!doctype html")) ||
		bytes.Contains(lowered, []byte("<body"))
}

// citationPDFMeta finds the PDF a publication page declares for itself.
var citationPDFMeta = regexp.MustCompile(
	`(?is)<meta[^>]+(?:name|property)\s*=\s*["']citation_pdf_url["'][^>]*content\s*=\s*["']([^"']+)["']`)

var citationPDFMetaReversed = regexp.MustCompile(
	`(?is)<meta[^>]+content\s*=\s*["']([^"']+)["'][^>]*(?:name|property)\s*=\s*["']citation_pdf_url["']`)

// declaredPDF reads a publication page's own citation_pdf_url.
func (e *Extractor) declaredPDF(locators Locators, timeout time.Duration) (string, error) {
	pageURL := strings.TrimSpace(locators.PageURL)
	if pageURL == "" {
		pageURL = strings.TrimSpace(locators.CanonicalURL)
	}
	if pageURL == "" || urlx.Scheme(pageURL) != "https" {
		return "", nil
	}
	page, err := e.Fetch(pageURL,
		map[string]string{"Accept": "text/html, application/xhtml+xml"}, MaxHTMLBytes, timeout)
	if err != nil {
		return "", err
	}
	for _, pattern := range []*regexp.Regexp{citationPDFMeta, citationPDFMetaReversed} {
		if match := pattern.FindSubmatch(page); match != nil {
			resolved := urlx.Join(pageURL, strings.TrimSpace(string(match[1])))
			if urlx.Scheme(resolved) == "https" {
				return resolved, nil
			}
		}
	}
	return "", nil
}

// FromUpload extracts a document from a PDF the user supplied.
func FromUpload(document []byte) (*Result, error) {
	if !IsPDF(document) {
		return nil, &Error{Reason: "That file is not a PDF"}
	}
	extracted, err := FromPDF(document)
	if err != nil {
		return nil, err
	}
	if extracted.TextLength() < MinUsefulTextRuns {
		return nil, &ScannedError{
			Reason: "Herald found almost no text in that PDF. If it is a scan, a searchable copy will work.",
		}
	}
	return &Result{
		Document: extracted, SourceKind: SourceUpload, SourceURL: "", PDF: document,
	}, nil
}
