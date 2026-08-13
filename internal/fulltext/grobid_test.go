package fulltext

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestGROBIDClientSubmitsFulltextMultipartRequest(t *testing.T) {
	pdf := []byte("%PDF-1.7\nfixture")
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/processFulltextDocument" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Accept") != "application/xml" {
			t.Errorf("unexpected Accept header: %q", r.Header.Get("Accept"))
		}
		if err := r.ParseMultipartForm(int64(len(pdf) + 4096)); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		file, _, err := r.FormFile("input")
		if err != nil {
			t.Fatalf("missing PDF input: %v", err)
		}
		defer file.Close()
		received, err := io.ReadAll(file)
		if err != nil || string(received) != string(pdf) {
			t.Errorf("unexpected PDF payload: %q (%v)", received, err)
		}
		for field, expected := range map[string]string{
			"includeRawCitations":  "1",
			"consolidateHeader":    "0",
			"consolidateCitations": "0",
		} {
			if got := r.FormValue(field); got != expected {
				t.Errorf("%s = %q, want %q", field, got, expected)
			}
		}
		result := response(http.StatusOK, `<TEI xmlns="http://www.tei-c.org/ns/1.0"><text><body>`+
			`<p>Extracted by GROBID.</p></body></text></TEI>`)
		result.Header.Set("Content-Type", "application/xml")
		return result, nil
	})

	client := NewGROBIDClient("http://grobid.test")
	client.HTTPClient = &http.Client{Transport: transport}
	response, err := client.ProcessFulltext(pdf)
	if err != nil {
		t.Fatalf("ProcessFulltext: %v", err)
	}
	if !strings.Contains(string(response), "Extracted by GROBID") {
		t.Fatalf("unexpected TEI response: %s", response)
	}
}

func TestGROBIDNoContentIsAScannedError(t *testing.T) {
	client := NewGROBIDClient("http://grobid.test")
	client.HTTPClient = &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return response(http.StatusNoContent, ""), nil
		},
	)}
	_, err := client.ProcessFulltext([]byte("%PDF-1.7\nfixture"))
	if _, ok := err.(*ScannedError); !ok {
		t.Fatalf("expected ScannedError, got %T: %v", err, err)
	}
}

func TestGROBIDBusyResponseIsActionable(t *testing.T) {
	client := NewGROBIDClient("http://grobid.test")
	client.HTTPClient = &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return response(http.StatusServiceUnavailable, ""), nil
		},
	)}
	_, err := client.ProcessFulltext([]byte("%PDF-1.7\nfixture"))
	if err == nil || !strings.Contains(err.Error(), "busy or not ready") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := err.(*GROBIDServiceError); !ok {
		t.Fatalf("expected a service error, got %T", err)
	}
}

func TestTEICitationClusterLinksEveryTarget(t *testing.T) {
	targets := map[string]Reference{
		"b0": {Key: "doi:10.1/one", Label: "1"},
		"b1": {Key: "doi:10.1/two", Label: "2"},
	}
	linked := linkTEICitation("[1, 2]", "#b0 #b1", targets)
	if keys := CitationKeys(linked); len(keys) != 2 {
		t.Fatalf("expected both targets to be linked, got %v in %q", keys, linked)
	}
	if plain := StripCitations(linked); plain != "[1, 2]" {
		t.Fatalf("printed citation changed: %q", plain)
	}
}
