// Package api serves Herald's local dashboard and JSON API.
//
// The API is unauthenticated and bound to loopback by default, for single-user
// local operation. Binding to a non-loopback address exposes reading data and
// state-changing endpoints to anything that can reach the port; put an
// authenticated reverse proxy in front if you do that deliberately.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/paper"
	"github.com/PabloMoralesEscandon/herald/internal/service"
	"github.com/PabloMoralesEscandon/herald/internal/store"
	"github.com/PabloMoralesEscandon/herald/internal/vault"
	"github.com/PabloMoralesEscandon/herald/web"
)

// maxRequestBytes bounds a JSON request body.
const maxRequestBytes = 1_000_000

// actionStatuses maps the documented action verbs to stored statuses.
var actionStatuses = map[string]string{
	"read": "read", "unread": "unread", "keep": "kept", "discard": "discarded",
}

// Server owns the routes.
type Server struct {
	DB      *store.DB
	Service *service.Service
	mux     *http.ServeMux
}

// New builds a server with every documented route registered.
func New(db *store.DB, svc *service.Service) *Server {
	server := &Server{DB: db, Service: svc, mux: http.NewServeMux()}
	server.routes()
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Handler exposes the router.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	// Method-and-path patterns keep routing declarative, replacing the
	// hand-rolled path splitting the original used.
	s.mux.HandleFunc("GET /{$}", s.serveAsset("index.html"))
	s.mux.HandleFunc("GET /index.html", s.serveAsset("index.html"))
	s.mux.HandleFunc("GET /static/styles.css", s.serveAsset("styles.css"))
	s.mux.HandleFunc("GET /static/app.js", s.serveAsset("app.js"))

	s.mux.HandleFunc("GET /api/entries", s.listEntries)
	s.mux.HandleFunc("GET /api/entries/{id}", s.getEntry)
	s.mux.HandleFunc("GET /api/entries/{id}/references", s.getReferences)
	s.mux.HandleFunc("POST /api/entries/{id}/action", s.changeStatus)
	s.mux.HandleFunc("POST /api/entries/{id}/summarize", s.summarize)
	s.mux.HandleFunc("POST /api/entries/{id}/export", s.exportEntry)
	s.mux.HandleFunc("POST /api/entries/{id}/obsidian/retry", s.retryObsidian)

	s.mux.HandleFunc("GET /api/stats", s.stats)
	s.mux.HandleFunc("GET /api/sources", s.listSources)
	s.mux.HandleFunc("GET /api/sources/export", s.exportSources)
	s.mux.HandleFunc("POST /api/sources", s.addSource)
	s.mux.HandleFunc("POST /api/sources/import", s.importSources)
	s.mux.HandleFunc("POST /api/refresh", s.refresh)

	s.mux.HandleFunc("GET /api/profiles/{kind}", s.getProfile)
	s.mux.HandleFunc("PUT /api/profiles/{kind}", s.updateProfile)
	s.mux.HandleFunc("POST /api/profiles/{kind}/rescore", s.rescore)
	s.mux.HandleFunc("GET /api/relevance/health", s.relevanceHealth)

	s.mux.HandleFunc("GET /api/settings/obsidian", s.getObsidianSettings)
	s.mux.HandleFunc("PUT /api/settings/obsidian", s.configureObsidian)

	s.mux.HandleFunc("POST /api/import/paper", s.importPaper)
	s.mux.HandleFunc("POST /api/references/{id}/add", s.addReference)

	// The dashboard is a single page; a deep link to an article returns to it.
	s.mux.HandleFunc("GET /entry/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "Not found")
	})
}

// serveAsset serves one embedded dashboard file.
//
// Assets are embedded in the binary, so Herald runs from any directory, and
// served no-store so a stale dashboard never outlives an upgrade.
func (s *Server) serveAsset(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		document, err := web.Assets.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusNotFound, "Web asset not found")
			return
		}
		contentType := mime.TypeByExtension(filepath.Ext(name))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if !strings.Contains(contentType, "charset") {
			contentType += "; charset=utf-8"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(document)))
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		w.Write(document)
	}
}

// writeJSON sends a JSON response with Herald's standard headers.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	// Match the reference encoder: raw UTF-8, no HTML escaping.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		http.Error(w, `{"error":"encoding failed"}`, http.StatusInternalServerError)
		return
	}
	body := strings.TrimRight(buffer.String(), "\n")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// readJSON decodes and size-limits a request body.
func readJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return nil, false
	}
	if r.ContentLength > maxRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "Request is too large")
		return nil, false
	}
	document, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON")
		return nil, false
	}
	if len(document) > maxRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "Request is too large")
		return nil, false
	}
	var payload any
	if err := json.Unmarshal(document, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON")
		return nil, false
	}
	object, ok := payload.(map[string]any)
	if !ok {
		writeError(w, http.StatusBadRequest, "JSON body must be an object")
		return nil, false
	}
	return object, true
}

// rawJSON returns the body bytes for endpoints that validate their own schema.
func rawJSON(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return nil, false
	}
	document, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil || len(document) > maxRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "Request is too large")
		return nil, false
	}
	return document, true
}

func requiredText(payload map[string]any, key string) string {
	if value, ok := payload[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// pathID reads a numeric path segment.
func pathID(r *http.Request) (int64, bool) {
	value, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// writeServiceError maps a service error to the documented status code.
func writeServiceError(w http.ResponseWriter, err error) {
	var conflict *vault.ConflictError
	var invalid *service.InvalidRequestError
	switch {
	case service.IsNotFound(err):
		writeError(w, http.StatusNotFound, "Entry not found")
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, err.Error())
	case paper.IsUnsafeURL(err), paper.IsInvalidInput(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case paper.IsNotFound(err):
		writeError(w, http.StatusNotFound, err.Error())
	case paper.IsFetchError(err):
		writeError(w, http.StatusBadGateway, err.Error())
	case paper.IsImportError(err):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
