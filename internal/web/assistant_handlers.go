package web

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// assistantTextMax bounds one composer message. It is generous enough for a
// pasted stack trace and short enough that the argv the agent is spawned with
// stays under the per-string limit.
const assistantTextMax = 32 << 10

// assistantContextMax bounds the page context a message carries. The pane sends
// a handful of lines about the open view, so anything larger is a client that
// went wrong rather than a page worth describing.
const assistantContextMax = 2 << 10

func (s *Server) handleAssistantConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.AssistantConfig())
}

func (s *Server) handleListAssistantThreads(w http.ResponseWriter, _ *http.Request) {
	threads, err := s.svc.ListAssistantThreads()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if threads == nil {
		threads = []AssistantThreadInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

func (s *Server) handleCreateAssistantThread(w http.ResponseWriter, r *http.Request) {
	var req CreateAssistantThreadRequest
	// An empty body is a thread on the configured defaults, which is what the
	// pane's own "new chat" sends.
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	thread, err := s.svc.CreateAssistantThread(req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, thread)
}

func (s *Server) handleGetAssistantThread(w http.ResponseWriter, r *http.Request) {
	thread, err := s.svc.GetAssistantThread(r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

func (s *Server) handleDeleteAssistantThread(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteAssistantThread(r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssistantActivity(w http.ResponseWriter, r *http.Request) {
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = n
	}
	info, err := s.svc.AssistantActivity(AssistantActivityQuery{
		ID:          r.PathValue("id"),
		After:       after,
		IfNoneMatch: r.Header.Get("If-None-Match"),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if info.ETag != "" {
		w.Header().Set("ETag", info.ETag)
	}
	if info.NotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleAssistantMessage(w http.ResponseWriter, r *http.Request) {
	var req AssistantMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Text == "" {
		writeJSONError(w, http.StatusBadRequest, "text is required")
		return
	}
	if len(req.Text) > assistantTextMax {
		writeJSONError(w, http.StatusBadRequest, "text is too long")
		return
	}
	if len(req.Context) > assistantContextMax {
		writeJSONError(w, http.StatusBadRequest, "context is too long")
		return
	}
	if err := s.svc.PostAssistantMessage(r.PathValue("id"), req); err != nil {
		writeServiceError(w, err)
		return
	}
	// The reply arrives through the activity poll, not here.
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleAssistantStop(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.StopAssistantTurn(r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssistantGate(w http.ResponseWriter, r *http.Request) {
	var req AssistantGateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch req.Decision {
	case "approve", "skip":
	default:
		writeJSONError(w, http.StatusBadRequest, `decision must be "approve" or "skip"`)
		return
	}
	if err := s.svc.ResolveAssistantGate(r.PathValue("gid"), req.Decision == "approve"); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAssistantGateAsk is the agent side of the gate. It blocks while a write
// waits for the person at the pane, so it has no timeout of its own: the gate's
// own deadline is what ends the wait.
func (s *Server) handleAssistantGateAsk(w http.ResponseWriter, r *http.Request) {
	var req AssistantGateAskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	resp, err := s.svc.AskAssistantGate(req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeOptionalJSON decodes a body that may be empty, which is what a POST
// with no options sends.
func decodeOptionalJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil && err.Error() != "EOF" {
		return err
	}
	return nil
}
