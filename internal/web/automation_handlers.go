package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
)

const automationRequestMax = 64 << 10

func (s *Server) registerAutomationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/automations", s.handleAutomations)
	mux.HandleFunc("POST /api/automations", s.handleCreateAutomation)
	mux.HandleFunc("POST /api/automations/preview", s.handleAutomationPreview)
	mux.HandleFunc("GET /api/automations/{id}", s.handleAutomation)
	mux.HandleFunc("PUT /api/automations/{id}", s.handleUpdateAutomation)
	mux.HandleFunc("GET /api/automations/{id}/occurrences", s.handleAutomationOccurrences)
	mux.HandleFunc("POST /api/automations/{id}/occurrences/{occurrence_id}/skip", s.handleSkipAutomationOccurrence)
}

func (s *Server) automationService(w http.ResponseWriter) (AutomationService, bool) {
	svc, ok := s.svc.(AutomationService)
	if !ok {
		writeAutomationError(w, automation.ErrUnavailable)
	}
	return svc, ok
}

func (s *Server) handleAutomations(w http.ResponseWriter, _ *http.Request) {
	response := AutomationsResponse{Definitions: []automation.Definition{}}
	svc, ok := s.svc.(AutomationService)
	if !ok {
		response.Error = automation.ErrUnavailable.Error()
		writeJSON(w, http.StatusOK, response)
		return
	}
	definitions, err := svc.ListAutomations()
	if err != nil {
		if !errors.Is(err, automation.ErrUnavailable) {
			writeAutomationError(w, err)
			return
		}
		response.Error = err.Error()
	} else {
		response.Available = true
		if definitions != nil {
			response.Definitions = definitions
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleAutomation(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	definition, err := svc.GetAutomation(r.PathValue("id"))
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, definition)
}

func (s *Server) handleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	var req struct {
		RequestID  string `json:"request_id"`
		Definition *struct {
			automation.Definition
			SkipOverlap *bool `json:"skip_overlap"`
		} `json:"definition"`
	}
	if err := decodeAutomationJSON(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.RequestID) == "" || len(req.RequestID) > 200 || req.Definition == nil {
		writeJSONError(w, http.StatusBadRequest, "request_id (at most 200 bytes) and definition are required")
		return
	}
	input := req.Definition.Definition
	if input.ID != "" || input.Revision != 0 || input.ScheduleRevision != 0 || !input.Next.IsZero() {
		writeJSONError(w, http.StatusBadRequest, "id, revision, schedule_revision and next are server-managed")
		return
	}
	if input.Enabled {
		writeJSONError(w, http.StatusBadRequest, "new automations must be disabled")
		return
	}
	input.SkipOverlap = req.Definition.SkipOverlap == nil || *req.Definition.SkipOverlap
	definition, err := svc.CreateAutomation(req.RequestID, input)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, definition)
}

func (s *Server) handleUpdateAutomation(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	var input automation.Definition
	if err := decodeAutomationJSON(w, r, &input); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if input.Revision == 0 || (input.ID != "" && input.ID != id) {
		writeJSONError(w, http.StatusBadRequest, "revision is required and id must match the URL")
		return
	}
	current, err := svc.GetAutomation(id)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	if input.Revision != current.Revision {
		writeAutomationError(w, automation.ErrConflict)
		return
	}
	// The scheduler can advance next without changing the editable revision.
	if input.ScheduleRevision != 0 && input.ScheduleRevision != current.ScheduleRevision {
		writeJSONError(w, http.StatusBadRequest, "schedule_revision is server-managed")
		return
	}
	input.ID, input.ScheduleRevision, input.Next = id, current.ScheduleRevision, current.Next
	definition, err := svc.UpdateAutomation(input)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, definition)
}

func (s *Server) handleAutomationPreview(w http.ResponseWriter, r *http.Request) {
	var schedule automation.Schedule
	if err := decodeAutomationJSON(w, r, &schedule); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := schedule.Validate(); err != nil {
		writeAutomationError(w, err)
		return
	}
	occurrences, err := schedule.Preview(time.Now(), 3)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	if occurrences == nil {
		occurrences = []time.Time{}
	}
	writeJSON(w, http.StatusOK, struct {
		Occurrences []time.Time `json:"occurrences"`
	}{occurrences})
}

func (s *Server) handleAutomationOccurrences(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	offset, err := automationPageParam(r, "offset", 0, 0, int(^uint(0)>>1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := automationPageParam(r, "limit", 50, 1, 200)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	occurrences, err := svc.AutomationOccurrences(r.PathValue("id"))
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	total := len(occurrences)
	start := min(offset, total)
	end := start + min(limit, total-start)
	page := occurrences[start:end]
	if page == nil {
		page = []automation.Occurrence{}
	}
	writeJSON(w, http.StatusOK, AutomationOccurrencesResponse{Occurrences: page, Total: total})
}

func (s *Server) handleSkipAutomationOccurrence(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	var req struct {
		Revision uint64 `json:"revision"`
	}
	if err := decodeAutomationJSON(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Revision == 0 {
		writeJSONError(w, http.StatusBadRequest, "revision is required")
		return
	}
	occurrence, err := svc.SkipAutomationOccurrence(r.PathValue("id"), r.PathValue("occurrence_id"), req.Revision)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, occurrence)
}

func automationPageParam(r *http.Request, name string, fallback, minimum, maximum int) (int, error) {
	values, ok := r.URL.Query()[name]
	if !ok {
		return fallback, nil
	}
	if len(values) == 1 {
		value, err := strconv.Atoi(values[0])
		if err == nil && value >= minimum && value <= maximum {
			return value, nil
		}
	}
	return 0, fmt.Errorf("%s must be an integer from %d to %d", name, minimum, maximum)
}

func decodeAutomationJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, automationRequestMax)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return errors.New("invalid JSON body (maximum 64 KiB)")
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return errors.New("body must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("body must contain a single JSON object")
	}
	return nil
}

func writeAutomationError(w http.ResponseWriter, err error) {
	var status int
	switch {
	case errors.Is(err, automation.ErrValidation):
		status = http.StatusBadRequest
	case errors.Is(err, automation.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, automation.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, automation.ErrUnavailable):
		status = http.StatusServiceUnavailable
	default:
		writeServiceError(w, err)
		return
	}
	writeJSONError(w, status, err.Error())
}
