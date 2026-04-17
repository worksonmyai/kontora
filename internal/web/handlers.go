package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
)

func (s *Server) handleListTickets(w http.ResponseWriter, _ *http.Request) {
	tickets := s.svc.ListTickets()
	if tickets == nil {
		tickets = []TicketInfo{}
	}
	writeJSON(w, http.StatusOK, struct {
		Tickets       []TicketInfo `json:"tickets"`
		RunningAgents int          `json:"running_agents"`
	}{tickets, s.svc.RunningAgents()})
}

func (s *Server) handleGetTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tkt, err := s.svc.GetTicket(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func (s *Server) handleDeleteTicket(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Kontora-Confirm") != "delete-ticket-file" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing delete confirmation"})
		return
	}
	id := r.PathValue("id")
	if err := s.svc.DeleteTicket(id); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func(id string) error { return s.svc.PauseTicket(id) })
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func(id string) error { return s.svc.RetryTicket(id) })
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func(id string) error { return s.svc.RunTicket(id) })
}

func (s *Server) handleSkip(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, func(id string) error { return s.svc.SkipStage(id) })
}

func (s *Server) handleSetStage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Stage string `json:"stage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Stage == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stage is required"})
		return
	}

	if err := s.svc.SetStage(id, body.Stage); err != nil {
		writeServiceError(w, err)
		return
	}
	tkt, err := s.svc.GetTicket(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Status == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status is required"})
		return
	}

	if err := s.svc.MoveTicket(id, body.Status); err != nil {
		writeServiceError(w, err)
		return
	}
	tkt, err := s.svc.GetTicket(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request, action func(string) error) {
	id := r.PathValue("id")
	if err := action(id); err != nil {
		writeServiceError(w, err)
		return
	}
	tkt, err := s.svc.GetTicket(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	var req CreateTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	if containsNewline(req.Title) || containsNewline(req.Path) || containsNewline(req.Pipeline) || containsNewline(req.Status) || containsNewline(req.Agent) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fields must not contain newlines"})
		return
	}
	if req.Status != "" && req.Status != "todo" && req.Status != "open" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be 'todo' or 'open'"})
		return
	}
	req.Branch = strings.TrimSpace(req.Branch)
	if req.Branch != "" && !validBranchName(req.Branch) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid branch name"})
		return
	}

	tkt, err := s.svc.CreateTicket(req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tkt)
}

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req InitTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	if containsNewline(req.Pipeline) || containsNewline(req.Path) || containsNewline(req.Agent) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fields must not contain newlines"})
		return
	}

	if err := s.svc.InitTicket(id, req); err != nil {
		writeServiceError(w, err)
		return
	}
	tkt, err := s.svc.GetTicket(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func (s *Server) handleUpdateTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req UpdateTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Pipeline != nil && containsNewline(*req.Pipeline) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pipeline must not contain newlines"})
		return
	}
	if req.Path != nil && containsNewline(*req.Path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path must not contain newlines"})
		return
	}
	if req.Agent != nil && containsNewline(*req.Agent) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent must not contain newlines"})
		return
	}
	if req.Branch != nil {
		trimmed := strings.TrimSpace(*req.Branch)
		req.Branch = &trimmed
		if trimmed != "" && !validBranchName(trimmed) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid branch name"})
			return
		}
	}

	if err := s.svc.UpdateTicket(id, req); err != nil {
		writeServiceError(w, err)
		return
	}
	tkt, err := s.svc.GetTicket(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func (s *Server) handleUploadTickets(w http.ResponseWriter, r *http.Request) {
	const maxRequestSize = 10 << 20 // 10 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)

	if err := r.ParseMultipartForm(maxRequestSize); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart form"})
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no files provided"})
		return
	}

	type uploadError struct {
		File  string `json:"file"`
		Error string `json:"error"`
	}

	var tickets []TicketInfo
	var errs []uploadError

	for _, fh := range files {
		if !strings.HasSuffix(strings.ToLower(fh.Filename), ".md") {
			errs = append(errs, uploadError{File: fh.Filename, Error: "file must have .md extension"})
			continue
		}

		f, err := fh.Open()
		if err != nil {
			errs = append(errs, uploadError{File: fh.Filename, Error: "failed to read file"})
			continue
		}
		content, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			errs = append(errs, uploadError{File: fh.Filename, Error: "failed to read file"})
			continue
		}

		info, err := s.svc.UploadTicket(content)
		if err != nil {
			errs = append(errs, uploadError{File: fh.Filename, Error: err.Error()})
			continue
		}
		tickets = append(tickets, info)
	}

	if tickets == nil {
		tickets = []TicketInfo{}
	}

	status := http.StatusCreated
	if len(tickets) == 0 {
		status = http.StatusBadRequest
	}

	writeJSON(w, status, struct {
		Tickets []TicketInfo  `json:"tickets"`
		Errors  []uploadError `json:"errors"`
	}{tickets, errs})
}

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stage := r.URL.Query().Get("stage")
	if stage != "" {
		stage = filepath.Base(stage)
	}
	content, err := s.svc.GetLogs(id, stage)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": content})
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.GetConfig())
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ch, unsub := s.broker.Subscribe()
	defer unsub()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			var data []byte
			if strings.HasPrefix(ev.Type, "plannotator_") {
				data, _ = json.Marshal(map[string]string{
					"ticket_id": ev.Ticket.ID,
					"outcome":   ev.Outcome,
					"message":   ev.Message,
				})
			} else {
				data, _ = json.Marshal(ev.Ticket)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handlePlannotatorReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.svc.StartPlannotatorReview(id); err != nil {
		switch {
		case errors.Is(err, ErrTicketNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrInvalidState):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrPlannotatorInFlight):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrPlannotatorBinary):
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func containsNewline(s string) bool {
	return strings.ContainsAny(s, "\n\r")
}

// validBranchName checks whether s is a valid git branch name.
func validBranchName(s string) bool {
	return exec.Command("git", "check-ref-format", "--branch", s).Run() == nil
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTicketNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrLogNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrInvalidState):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrUnknownAgent):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrDeleteRejected):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}
