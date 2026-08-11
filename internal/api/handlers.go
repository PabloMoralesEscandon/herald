package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/relevance"
	"github.com/PabloMoralesEscandon/herald/internal/service"
	"github.com/PabloMoralesEscandon/herald/internal/store"
)

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := store.EntryFilter{
		Status:          query.Get("status"),
		Category:        query.Get("category"),
		ContentKind:     query.Get("kind"),
		RelevanceBucket: query.Get("bucket"),
		Search:          query.Get("q"),
		Cursor:          query.Get("cursor"),
		Limit:           100,
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid limit")
			return
		}
		filter.Limit = limit
	}
	// A ranked page needs the profile joined so scores can order it.
	if filter.ContentKind != "" && filter.RelevanceBucket != "" {
		profile, err := s.DB.GetRelevanceProfile(filter.ContentKind)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if profile != nil {
			filter.ProfileID = &profile.ID
		}
	}
	page, err := s.DB.ListEntriesPage(filter)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The bare-list shape is retained for the unfiltered call the original
	// dashboard still uses; every paged call gets the envelope.
	if filter.Cursor != "" || filter.ContentKind != "" || filter.RelevanceBucket != "" {
		writeJSON(w, http.StatusOK, page)
		return
	}
	writeJSON(w, http.StatusOK, page.Entries)
}

func (s *Server) getEntry(w http.ResponseWriter, r *http.Request) {
	entryID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Entry not found")
		return
	}
	entry, err := s.Service.EntryWithObsidianState(entryID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if entry == nil {
		writeError(w, http.StatusNotFound, "Entry not found")
		return
	}
	kind, _ := entry["content_kind"].(string)
	profile, err := s.DB.GetRelevanceProfile(kind)
	if err == nil && profile != nil {
		ranking, err := s.DB.GetEntryRanking(entryID, profile.ID)
		if err == nil {
			entry["relevance"] = ranking
		}
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) getReferences(w http.ResponseWriter, r *http.Request) {
	entryID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Entry not found")
		return
	}
	references, err := s.Service.ListPaperReferences(entryID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entry_id": entryID, "references": references,
	})
}

func (s *Server) changeStatus(w http.ResponseWriter, r *http.Request) {
	entryID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Entry not found")
		return
	}
	payload, ok := readJSON(w, r)
	if !ok {
		return
	}
	status, known := actionStatuses[requiredText(payload, "action")]
	if !known {
		writeError(w, http.StatusBadRequest, "action must be read, unread, keep, or discard")
		return
	}
	entry, err := s.Service.ChangeStatus(entryID, status)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) summarize(w http.ResponseWriter, r *http.Request) {
	entryID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Entry not found")
		return
	}
	result, err := s.Service.SummarizeEntry(entryID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	entry, err := s.DB.GetEntry(entryID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var fallbackReason *string
	if result.FallbackReason != "" {
		fallbackReason = &result.FallbackReason
	}
	var model *string
	if result.Model != "" {
		model = &result.Model
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entry": entry, "provider": result.Provider,
		"model": model, "fallback_reason": fallbackReason,
	})
}

func (s *Server) exportEntry(w http.ResponseWriter, r *http.Request) {
	entryID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Entry not found")
		return
	}
	destination, err := s.Service.ExportEntry(entryID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	entry, err := s.Service.EntryWithObsidianState(entryID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": entry, "path": destination})
}

func (s *Server) retryObsidian(w http.ResponseWriter, r *http.Request) {
	entryID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Entry not found")
		return
	}
	destination, err := s.Service.RetryObsidian(entryID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	entry, err := s.Service.EntryWithObsidianState(entryID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var path *string
	if destination != "" {
		path = &destination
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": entry, "path": path})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	counts, err := s.DB.EntryCounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	sourceRows, err := s.DB.ListSources(false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sourceRows)
}

func (s *Server) exportSources(w http.ResponseWriter, r *http.Request) {
	manifest, err := s.Service.ExportSources()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *Server) addSource(w http.ResponseWriter, r *http.Request) {
	payload, ok := readJSON(w, r)
	if !ok {
		return
	}
	title := requiredText(payload, "title")
	url := requiredText(payload, "url")
	category := requiredText(payload, "category")
	if title == "" || url == "" || category == "" {
		writeError(w, http.StatusBadRequest, "title, url, and category are required")
		return
	}
	contentKind := requiredText(payload, "content_kind")
	if contentKind == "" {
		contentKind = "paper"
	}
	if contentKind != "paper" && contentKind != "news" {
		writeError(w, http.StatusBadRequest, "content_kind must be paper or news")
		return
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		writeError(w, http.StatusBadRequest, "url must be an HTTP(S) URL")
		return
	}
	sourceID, err := s.Service.AddSource(title, url, category, contentKind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	source, err := s.DB.GetSource(sourceID)
	if err != nil || source == nil {
		writeError(w, http.StatusInternalServerError, "Source could not be read back")
		return
	}
	writeJSON(w, http.StatusCreated, source)
}

func (s *Server) importSources(w http.ResponseWriter, r *http.Request) {
	document, ok := rawJSON(w, r)
	if !ok {
		return
	}
	result, err := s.Service.ImportSources(document)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sourceRows, err := s.DB.ListSources(false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"imported": result.Imported, "created": result.Created,
		"updated": result.Updated, "unchanged": result.Unchanged,
		"sources": sourceRows,
	})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	results, err := s.Service.RefreshAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	created, updated, failures := 0, 0, 0
	newPapers := false
	for _, result := range results {
		created += result.Created
		updated += result.Updated
		if result.Error != nil {
			failures++
		}
		if result.Created > 0 && result.ContentKind == "paper" {
			newPapers = true
		}
	}
	if newPapers && s.Service.Relevance != nil {
		_, _ = s.Service.Relevance.Start("paper")
	}
	if results == nil {
		results = []service.RefreshResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sources": results, "created": created,
		"updated": updated, "errors": failures,
	})
}

// profileKind validates the workspace segment of a profile route.
func profileKind(r *http.Request) (string, bool) {
	kind := r.PathValue("kind")
	return kind, kind == "paper" || kind == "news"
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	kind, ok := profileKind(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	profile, err := s.DB.GetRelevanceProfile(kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if profile == nil {
		writeError(w, http.StatusNotFound, "Profile not found")
		return
	}
	s.attachThresholdMode(profile)
	writeJSON(w, http.StatusOK, profile)
}

// attachThresholdMode adds whether the threshold is manual, auto, or learned.
func (s *Server) attachThresholdMode(profile *store.Profile) {
	mode := "auto"
	if _, err := s.DB.GetSetting("relevance.threshold_mode."+
		strconv.FormatInt(profile.ID, 10), &mode); err != nil {
		mode = "auto"
	}
	profile.ThresholdMode = mode
}

func (s *Server) updateProfile(w http.ResponseWriter, r *http.Request) {
	kind, ok := profileKind(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	payload, ok := readJSON(w, r)
	if !ok {
		return
	}
	update, err := decodeProfileUpdate(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	profile, err := s.Service.Relevance.Engine.UpdateProfile(kind, update)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.attachThresholdMode(profile)
	job, err := s.Service.Relevance.Start(kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": profile, "rescore": job})
}

// decodeProfileUpdate validates the partial profile payload.
func decodeProfileUpdate(payload map[string]any) (relevance.ProfileUpdate, error) {
	allowed := map[string]bool{
		"interests": true, "exclusions": true, "include_phrases": true,
		"never_show_phrases": true, "selectivity": true, "threshold": true,
		"target_precision": true,
	}
	var unknown []string
	for key := range payload {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sortStrings(unknown)
		return relevance.ProfileUpdate{}, &service.InvalidRequestError{
			Reason: "Unknown profile fields: " + strings.Join(unknown, ", "),
		}
	}

	var update relevance.ProfileUpdate
	// Each phrase list is optional; absent means "leave unchanged", which is
	// why these are pointers rather than plain slices.
	lists := []struct {
		key    string
		target **[]string
	}{
		{"interests", &update.Interests},
		{"exclusions", &update.Exclusions},
		{"include_phrases", &update.IncludePhrases},
		{"never_show_phrases", &update.NeverShowPhrases},
	}
	for _, list := range lists {
		raw, present := payload[list.key]
		if !present {
			continue
		}
		values, err := stringList(raw)
		if err != nil {
			return relevance.ProfileUpdate{}, &service.InvalidRequestError{
				Reason: list.key + " must be a list of strings",
			}
		}
		*list.target = &values
	}
	if raw, present := payload["selectivity"]; present {
		value, ok := raw.(string)
		if !ok {
			return relevance.ProfileUpdate{}, &service.InvalidRequestError{
				Reason: "selectivity must be broad, balanced, or focused",
			}
		}
		update.Selectivity = &value
	}
	if raw, present := payload["threshold"]; present {
		update.ThresholdSet = true
		if raw != nil {
			value, ok := raw.(float64)
			if !ok {
				return relevance.ProfileUpdate{}, &service.InvalidRequestError{
					Reason: "threshold must be a number",
				}
			}
			update.Threshold = &value
		}
	}
	if raw, present := payload["target_precision"]; present {
		value, ok := raw.(float64)
		if !ok {
			return relevance.ProfileUpdate{}, &service.InvalidRequestError{
				Reason: "target_precision must be a number",
			}
		}
		update.TargetPrecision = &value
	}
	return update, nil
}

// stringList converts a decoded JSON array into a string slice.
func stringList(raw any) ([]string, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, errNotAStringList
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, errNotAStringList
		}
		values = append(values, text)
	}
	return values, nil
}

var errNotAStringList = errors.New("not a list of strings")

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func (s *Server) rescore(w http.ResponseWriter, r *http.Request) {
	kind, ok := profileKind(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	job, err := s.Service.Relevance.Start(kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) relevanceHealth(w http.ResponseWriter, r *http.Request) {
	health, err := s.Service.Relevance.Health()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) getObsidianSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Service.GetObsidianSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) configureObsidian(w http.ResponseWriter, r *http.Request) {
	payload, ok := readJSON(w, r)
	if !ok {
		return
	}
	vaultPath := requiredText(payload, "vault_path")
	if vaultPath == "" {
		writeError(w, http.StatusBadRequest, "vault_path is required")
		return
	}
	settings, err := s.Service.ConfigureObsidian(vaultPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) importPaper(w http.ResponseWriter, r *http.Request) {
	payload, ok := readJSON(w, r)
	if !ok {
		return
	}
	value := requiredText(payload, "input")
	if value == "" {
		writeError(w, http.StatusBadRequest, "input is required")
		return
	}
	result, err := s.Service.ImportPaper(value)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (s *Server) addReference(w http.ResponseWriter, r *http.Request) {
	referenceID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "Reference not found")
		return
	}
	result, err := s.Service.AddPaperReference(referenceID)
	if err != nil {
		if service.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "Reference not found")
			return
		}
		writeServiceError(w, err)
		return
	}
	status := http.StatusOK
	if result.Import != nil && result.Import.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}
