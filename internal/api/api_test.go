package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/fulltext"
	"github.com/PabloMoralesEscandon/herald/internal/paper"
	"github.com/PabloMoralesEscandon/herald/internal/relevance"
	"github.com/PabloMoralesEscandon/herald/internal/service"
	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/summary"
)

type offlineSummarizer struct{}

func (offlineSummarizer) Summarize(title, content string) summary.Result {
	return summary.Result{Text: summary.Deterministic(title, content), Provider: "fallback"}
}

// newServer builds a server whose extractor answers from memory, so the API
// tests exercise every route without touching the network.
func newServer(t *testing.T, document []byte) (*Server, *service.Service, int64) {
	t.Helper()
	root := t.TempDir()
	vaultPath := filepath.Join(root, "vault")
	if err := os.MkdirAll(vaultPath, 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	db, err := store.Open(filepath.Join(root, "herald.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	engine, err := relevance.NewEngine(db, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	extractor := &fulltext.Extractor{
		Fetch: func(url string, headers map[string]string, maxBytes int, timeout time.Duration) ([]byte, error) {
			if document == nil {
				return nil, fmt.Errorf("no open copy")
			}
			return document, nil
		},
		GROBID: fulltext.GROBIDProcessorFunc(func([]byte) ([]byte, error) {
			return []byte(`<TEI xmlns="http://www.tei-c.org/ns/1.0"><text><body><div>` +
				`<head n="1">Introduction</head><p>` + strings.Repeat(
				"This line of the paper carries real body text for the extractor. ", 14) +
				`</p></div></body></text></TEI>`), nil
		}),
	}
	// The importer is offline too. Extraction asks it where a paper's
	// open-access copy is, and the real one would reach a metadata provider.
	importer := paper.NewImporter(db)
	importer.RequestDelay = 0
	importer.Retries = 0
	importer.Sleep = func(time.Duration) {}
	importer.Fetcher = func(string, map[string]string, int, time.Duration) ([]byte, error) {
		return nil, &paper.FetchError{Reason: "network disabled in tests"}
	}

	svc := service.New(db, service.Options{
		Summarizer: offlineSummarizer{}, Relevance: relevance.NewCoordinator(engine),
		Importer: importer, DefaultVault: vaultPath,
		ArchiveRoot: filepath.Join(root, "archive"),
		PDFRoot:     filepath.Join(root, "pdfs"), Extractor: extractor,
		AutoExtractFullText: true,
	})

	sourceID, err := db.AddSource(store.AddSourceInput{
		Title: "Systems Lab", URL: "https://lab.example/f.xml",
		Category: "Chip Design", ContentKind: "paper",
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	entryID, _, err := db.UpsertEntry(store.UpsertEntryInput{
		SourceID: sourceID, GUID: "g1", URL: "https://arxiv.org/abs/2402.00001",
		CanonicalURL: "https://arxiv.org/abs/2402.00001", Title: "A Chiplet Interconnect",
		Content: "An abstract.", ContentKind: "paper",
	})
	if err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	if _, err := db.AddPaperIdentifier(entryID, "arxiv", "2402.00001", true); err != nil {
		t.Fatalf("AddPaperIdentifier: %v", err)
	}
	return New(db, svc), svc, entryID
}

func do(t *testing.T, server *Server, method, path, contentType string, body []byte) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	var payload map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	return recorder, payload
}

// articlePDF builds a PDF with enough text to be a usable article.
func articlePDF() []byte {
	var content strings.Builder
	for index := range 14 {
		fmt.Fprintf(&content,
			"BT /F1 10 Tf 72 %d Td (This line of the paper carries real body text for the extractor.) Tj ET\n",
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

func TestEntryResponseCarriesExtractionState(t *testing.T) {
	server, svc, entryID := newServer(t, articlePDF())

	recorder, payload := do(t, server, "POST", fmt.Sprintf("/api/entries/%d/action", entryID),
		"application/json", []byte(`{"action":"keep"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("keep returned %d: %s", recorder.Code, recorder.Body)
	}
	// The field is present from the first response so the dashboard can draw
	// the panel without a second request.
	if _, present := payload["fulltext"]; !present {
		t.Fatalf("the entry response should carry a fulltext field: %v", payload)
	}
	svc.WaitForEnrichment()

	recorder, payload = do(t, server, "GET", fmt.Sprintf("/api/entries/%d", entryID), "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get entry returned %d", recorder.Code)
	}
	record, ok := payload["fulltext"].(map[string]any)
	if !ok {
		t.Fatalf("expected an extraction record: %v", payload["fulltext"])
	}
	if record["state"] != "extracted" {
		t.Fatalf("expected extracted, got %v (%v)", record["state"], record["error"])
	}
}

func TestFullTextEndpointReportsState(t *testing.T) {
	server, _, entryID := newServer(t, nil)

	recorder, payload := do(t, server, "GET",
		fmt.Sprintf("/api/entries/%d/fulltext", entryID), "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body)
	}
	if _, present := payload["entry"]; !present {
		t.Fatalf("the response should include the entry: %v", payload)
	}
}

func TestFullTextEndpointsRejectUnknownEntries(t *testing.T) {
	server, _, _ := newServer(t, nil)

	for _, request := range []struct{ method, path, contentType string }{
		{"GET", "/api/entries/999999/fulltext", ""},
		{"POST", "/api/entries/999999/fulltext/retry", "application/json"},
		{"POST", "/api/entries/999999/fulltext/pdf", "application/pdf"},
	} {
		recorder, _ := do(t, server, request.method, request.path, request.contentType, articlePDF())
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s %s returned %d, want 404", request.method, request.path, recorder.Code)
		}
	}
}

func TestUploadPDFCompletesExtraction(t *testing.T) {
	// No open copy exists, which is what the upload endpoint is for.
	server, svc, entryID := newServer(t, nil)

	if recorder, _ := do(t, server, "POST", fmt.Sprintf("/api/entries/%d/action", entryID),
		"application/json", []byte(`{"action":"keep"}`)); recorder.Code != http.StatusOK {
		t.Fatalf("keep returned %d", recorder.Code)
	}
	svc.WaitForEnrichment()

	recorder, payload := do(t, server, "GET",
		fmt.Sprintf("/api/entries/%d/fulltext", entryID), "", nil)
	record := payload["fulltext"].(map[string]any)
	if record["state"] != "needs_pdf" {
		t.Fatalf("expected needs_pdf, got %v", record["state"])
	}

	recorder, payload = do(t, server, "POST", fmt.Sprintf("/api/entries/%d/fulltext/pdf", entryID),
		"application/pdf", articlePDF())
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload returned %d: %s", recorder.Code, recorder.Body)
	}
	record = payload["fulltext"].(map[string]any)
	if record["state"] != "extracted" {
		t.Fatalf("expected extracted after upload, got %v (%v)", record["state"], record["error"])
	}
	if record["source_kind"] != "upload" {
		t.Fatalf("the note should record the user's upload, got %v", record["source_kind"])
	}
}

func TestUploadRejectsSomethingThatIsNotAPDF(t *testing.T) {
	server, _, entryID := newServer(t, nil)

	recorder, payload := do(t, server, "POST", fmt.Sprintf("/api/entries/%d/fulltext/pdf", entryID),
		"application/pdf", []byte("this is plainly not a PDF"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body)
	}
	if payload["error"] == nil {
		t.Fatal("the response should explain the rejection")
	}
}

func TestRetryEndpointReExtracts(t *testing.T) {
	server, svc, entryID := newServer(t, articlePDF())

	if recorder, _ := do(t, server, "POST", fmt.Sprintf("/api/entries/%d/action", entryID),
		"application/json", []byte(`{"action":"keep"}`)); recorder.Code != http.StatusOK {
		t.Fatalf("keep failed")
	}
	svc.WaitForEnrichment()

	recorder, payload := do(t, server, "POST",
		fmt.Sprintf("/api/entries/%d/fulltext/retry", entryID), "application/json", []byte(`{}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("retry returned %d: %s", recorder.Code, recorder.Body)
	}
	record := payload["fulltext"].(map[string]any)
	if record["state"] != "extracted" {
		t.Fatalf("expected extracted, got %v", record["state"])
	}
}

func TestNewsHasNoArticleToExtract(t *testing.T) {
	server, svc, _ := newServer(t, articlePDF())
	sources, err := svc.DB.ListSources(false)
	if err != nil || len(sources) == 0 {
		t.Fatalf("ListSources: %v", err)
	}
	newsID, _, err := svc.DB.UpsertEntry(store.UpsertEntryInput{
		SourceID: sources[0].ID, GUID: "news-1", URL: "https://example.org/n",
		Title: "An announcement", ContentKind: "news",
	})
	if err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}

	recorder, _ := do(t, server, "POST", fmt.Sprintf("/api/entries/%d/fulltext/pdf", newsID),
		"application/pdf", articlePDF())
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a news entry, got %d", recorder.Code)
	}
}
