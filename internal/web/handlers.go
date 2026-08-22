package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func (s *Server) handleListTickets(w http.ResponseWriter, r *http.Request) {
	tickets := s.svc.ListTickets(ListTicketsOptions{
		IncludeHidden: r.URL.Query().Get("all") == "true",
	})
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

func (s *Server) handleAddNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}

	if err := s.svc.AddNote(id, body.Text); err != nil {
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

func (s *Server) handleSetSummary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}

	if err := s.svc.SetSummary(id, body.Text); err != nil {
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

func (s *Server) handleAddDependency(w http.ResponseWriter, r *http.Request) {
	s.handleRelation(w, r, true, func(id string, related []string) error {
		return s.svc.AddDependency(id, related[0])
	})
}

func (s *Server) handleRemoveDependency(w http.ResponseWriter, r *http.Request) {
	s.handleRelation(w, r, true, func(id string, related []string) error {
		return s.svc.RemoveDependency(id, related[0])
	})
}

func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	s.handleRelation(w, r, false, s.svc.LinkTickets)
}

func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	s.handleRelation(w, r, false, s.svc.UnlinkTickets)
}

// handleRelation decodes a relation request and answers with the changed
// ticket. single is true for the dependency verbs, which relate exactly two
// tickets.
func (s *Server) handleRelation(w http.ResponseWriter, r *http.Request, single bool, mutate func(id string, related []string) error) {
	id := r.PathValue("id")

	var body RelationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if len(body.Related) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "related is required"})
		return
	}
	if slices.Contains(body.Related, "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "related ids must not be empty"})
		return
	}
	if single && len(body.Related) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a dependency relates exactly one ticket"})
		return
	}

	if err := mutate(id, body.Related); err != nil {
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
	// Format only. That the base names an existing branch is checked further
	// down in CheckRepo, which has the repository path.
	req.BaseBranch = strings.TrimSpace(req.BaseBranch)
	if req.BaseBranch != "" && !validBranchName(req.BaseBranch) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base branch name"})
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
	req.Branch = strings.TrimSpace(req.Branch)
	if req.Branch != "" && !validBranchName(req.Branch) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid branch name"})
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
	// Format only. Whether the base exists depends on the repository at the
	// ticket's path, which can change after an update, so it is left to the
	// daemon: worktree.Create fails and pauses the ticket with the git error.
	if req.BaseBranch != nil {
		trimmed := strings.TrimSpace(*req.BaseBranch)
		req.BaseBranch = &trimmed
		if trimmed != "" && !validBranchName(trimmed) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid base branch name"})
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
	// multipart/form-data is a CORS-simple content type, so unlike the JSON
	// routes this one is forgeable from another site without a preflight. The
	// header is not, which is the same reason the delete route asks for one.
	if r.Header.Get("X-Kontora-Confirm") != "upload-tickets" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing upload confirmation"})
		return
	}

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

func (s *Server) handleGetActivity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stage := r.URL.Query().Get("stage")
	if stage != "" {
		stage = filepath.Base(stage)
	}
	run := 0
	if raw := r.URL.Query().Get("run"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run must be a non-negative integer"})
			return
		}
		run = n
	}
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "after must be a non-negative integer"})
			return
		}
		after = n
	}
	activity, err := s.svc.GetActivity(ActivityQuery{
		ID:          id,
		Stage:       stage,
		Run:         run,
		After:       after,
		IfNoneMatch: r.Header.Get("If-None-Match"),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if activity.ETag != "" {
		w.Header().Set("ETag", activity.ETag)
	}
	if activity.NotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, activity)
}

// statsRanges maps a range chip to its window length in days. The client alone
// picks these values, so an unrecognised one is a bug and is rejected rather
// than defaulted. Project and pipeline are not matched against the config: the
// chips come from one the client fetched earlier, and a reload can retire a
// project between the fetch and the query. Their length is bounded though —
// each distinct pair keys a cached payload, and a name that long is not one.
var statsRanges = map[string]int{"1d": 1, "1w": 7, "30d": 35, "90d": 98, "all": 182}

const statsFilterMax = 128

func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "90d"
	}
	days, ok := statsRanges[rng]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "range must be one of 1d, 1w, 30d, 90d, all"})
		return
	}
	project, pipeline := r.URL.Query().Get("project"), r.URL.Query().Get("pipeline")
	if len(project) > statsFilterMax || len(pipeline) > statsFilterMax {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project and pipeline must be at most 128 characters"})
		return
	}
	info, err := s.svc.GetStats(StatsQuery{
		Days:     days,
		Project:  project,
		Pipeline: pipeline,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleGetChanges(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	changes, err := s.svc.GetChanges(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, changes)
}

func (s *Server) handleGetChain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	chain, err := s.svc.GetChain(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, chain)
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.GetConfig())
}

func (s *Server) handleGetRawConfig(w http.ResponseWriter, _ *http.Request) {
	content, err := s.svc.GetRawConfig()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": content})
}

func (s *Server) handlePutRawConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if err := s.svc.PutRawConfig(body.Content); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	writePlannotatorResult(w, s.svc.StartPlannotatorReview(r.PathValue("id")))
}

func (s *Server) handlePlannotatorAnnotate(w http.ResponseWriter, r *http.Request) {
	writePlannotatorResult(w, s.svc.StartPlannotatorAnnotate(r.PathValue("id")))
}

func writePlannotatorResult(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrTicketNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalidState), errors.Is(err, ErrPlannotatorInFlight):
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError sends an error in the {"error": ...} shape every client
// parses: the CLI's decodeError and the UI both read that field and otherwise
// show a bare status code.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
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
	case errors.Is(err, ErrUnknownPipeline):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrDeleteRejected):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrInvalidConfig):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrConfigPathNotSet):
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrAssistantDisabled):
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrAssistantNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrAssistantGateNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrAssistantBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrAssistantAtCapacity):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrAssistantStale):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrAssistantGateDenied):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}
