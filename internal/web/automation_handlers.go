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

const automationRequestMax = 512 << 10

func (s *Server) registerAutomationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/automations", s.handleAutomations)
	mux.HandleFunc("POST /api/automations", s.handleCreateAutomation)
	mux.HandleFunc("POST /api/automations/preview", s.handleAutomationPreview)
	mux.HandleFunc("GET /api/automations/{id}", s.handleAutomation)
	mux.HandleFunc("PUT /api/automations/{id}", s.handleUpdateAutomation)
	mux.HandleFunc("DELETE /api/automations/{id}", s.handleDeleteAutomation)
	mux.HandleFunc("POST /api/automations/{id}/run", s.handleRunAutomation)
	mux.HandleFunc("POST /api/automations/{id}/stop", s.handleStopAutomation)
	mux.HandleFunc("POST /api/automations/{id}/pause", s.handlePauseAutomation)
	mux.HandleFunc("POST /api/automations/{id}/resume", s.handlePauseAutomation)
	mux.HandleFunc("GET /api/automations/{id}/runs", s.handleAutomationRuns)
	mux.HandleFunc("GET /api/automations/{id}/runs/{n}/log", s.handleAutomationLog)
	mux.HandleFunc("GET /api/automations/{id}/runs/{n}/terminal", s.handleAutomationTerminal)
	mux.HandleFunc("GET /ws/automations/{id}/{n}", s.handleAutomationTerminalWS)
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
	rows, err := svc.ListAutomations()
	if err != nil {
		if !errors.Is(err, automation.ErrUnavailable) {
			writeAutomationError(w, err)
			return
		}
		response.Error = err.Error()
	} else {
		response.Available = true
		if rows != nil {
			response.Definitions = rows
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleAutomation(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	row, err := svc.GetAutomation(r.PathValue("id"))
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	var req struct {
		RequestID  string           `json:"request_id"`
		Key        string           `json:"key"`
		Definition *automation.Spec `json:"definition"`
	}
	if err := decodeAutomationJSON(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Definition == nil || strings.TrimSpace(req.RequestID) == "" || len(req.RequestID) > 200 {
		writeJSONError(w, http.StatusBadRequest, "request_id (at most 200 bytes) and definition are required")
		return
	}
	row, err := svc.CreateAutomation(req.RequestID, req.Key, *req.Definition)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) handleUpdateAutomation(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	var req struct {
		Revision   uint64           `json:"revision"`
		Definition *automation.Spec `json:"definition"`
	}
	if err := decodeAutomationJSON(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Revision == 0 || req.Definition == nil {
		writeJSONError(w, http.StatusBadRequest, "revision and definition are required")
		return
	}
	row, err := svc.UpdateAutomation(r.PathValue("id"), req.Revision, *req.Definition)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleDeleteAutomation(w http.ResponseWriter, r *http.Request) {
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
	if err := svc.DeleteAutomation(r.PathValue("id"), req.Revision); err != nil {
		writeAutomationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunAutomation(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	var req struct {
		RequestID string `json:"request_id"`
	}
	if err := decodeAutomationJSON(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.RequestID) == "" || len(req.RequestID) > 200 {
		writeJSONError(w, http.StatusBadRequest, "request_id is required (at most 200 bytes)")
		return
	}
	run, err := svc.RunAutomation(r.PathValue("id"), req.RequestID)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleStopAutomation(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	var req struct {
		N uint64 `json:"n"`
	}
	if err := decodeAutomationJSON(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.N == 0 {
		writeJSONError(w, http.StatusBadRequest, "n is required")
		return
	}
	run, err := svc.StopAutomation(r.PathValue("id"), req.N)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handlePauseAutomation(w http.ResponseWriter, r *http.Request) {
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
	row, err := svc.GetAutomation(r.PathValue("id"))
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	row.Enabled = strings.HasSuffix(r.URL.Path, "/resume")
	row, err = svc.UpdateAutomation(row.ID, req.Revision, row.Spec)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleAutomationPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedule string `json:"schedule"`
		Timezone string `json:"timezone"`
	}
	if err := decodeAutomationJSON(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	dates, err := automation.Preview(req.Schedule, req.Timezone, time.Now(), 4)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Occurrences []time.Time `json:"occurrences"`
		Human       string      `json:"human"`
	}{dates, automation.Human(req.Schedule)})
}

func (s *Server) handleAutomationRuns(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	offset, err := automationPageParam(r, "offset", 0, 0, int(^uint(0)>>1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := automationPageParam(r, "limit", 20, 1, 200)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	runs, err := svc.AutomationRuns(r.PathValue("id"))
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	start := min(offset, len(runs))
	end := start + min(limit, len(runs)-start)
	page := runs[start:end]
	if page == nil {
		page = []automation.Run{}
	}
	writeJSON(w, http.StatusOK, AutomationRunsResponse{Runs: page, Total: len(runs)})
}

func (s *Server) handleAutomationLog(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.automationService(w)
	if !ok {
		return
	}
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	if err != nil || n == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid run number")
		return
	}
	lines, raw, err := svc.AutomationLog(r.PathValue("id"), n)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	if r.URL.Query().Get("raw") == "1" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, raw)
		return
	}
	if lines == nil {
		lines = []automation.LogLine{}
	}
	writeJSON(w, http.StatusOK, struct {
		Lines []automation.LogLine `json:"lines"`
		Raw   string               `json:"raw"`
	}{lines, raw})
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
		return errors.New("invalid JSON body (maximum 512 KiB)")
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
