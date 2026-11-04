package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/companion"
)

type CompanionInfo struct {
	Enabled     bool             `json:"enabled"`
	Name        string           `json:"name"`
	AIAvailable bool             `json:"ai_available"`
	Hint        string           `json:"hint,omitempty"`
	Health      string           `json:"health"`
	Error       string           `json:"error,omitempty"`
	Revision    uint64           `json:"revision"`
	CaughtUp    uint64           `json:"caught_up"`
	ObservedAt  time.Time        `json:"observed_at"`
	Items       []companion.Item `json:"items"`
}

type CompanionMessageRequest struct {
	ThreadID  string   `json:"thread_id,omitempty"`
	Kind      string   `json:"kind"`
	Text      string   `json:"text"`
	TicketIDs []string `json:"ticket_ids,omitempty"`
}

type CompanionEvidenceInfo struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	CapturedAt  time.Time         `json:"captured_at"`
	WindowStart time.Time         `json:"window_start"`
	WindowEnd   time.Time         `json:"window_end"`
	Revision    uint64            `json:"revision"`
	Sources     map[string]string `json:"sources"`
	Omissions   []string          `json:"omissions"`
	Outdated    bool              `json:"outdated"`
}

func decodeCompanionJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 40<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return &json.SyntaxError{}
	}
	return nil
}

func (s *Server) handleCompanion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.CompanionSnapshot())
}

func (s *Server) handleCompanionRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision *uint64 `json:"revision"`
		Read     *bool   `json:"read"`
	}
	if err := decodeCompanionJSON(w, r, &req); err != nil || req.Revision == nil || req.Read == nil {
		writeJSONError(w, http.StatusBadRequest, "revision and read are required")
		return
	}
	info, err := s.svc.ReadCompanionItem(r.PathValue("id"), *req.Revision, *req.Read)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleCompanionCatchUp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Revision *uint64 `json:"revision"`
	}
	if err := decodeCompanionJSON(w, r, &req); err != nil || req.Revision == nil {
		writeJSONError(w, http.StatusBadRequest, "revision is required")
		return
	}
	info, err := s.svc.CatchUpCompanion(*req.Revision)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleCompanionMessage(w http.ResponseWriter, r *http.Request) {
	var req CompanionMessageRequest
	if err := decodeCompanionJSON(w, r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid companion request")
		return
	}
	if strings.TrimSpace(req.Text) == "" || len(req.Text) > assistantTextMax || len(req.TicketIDs) > 5 {
		writeJSONError(w, http.StatusBadRequest, "text must be non-blank and at most 32 KiB; ticket_ids must contain no more than five IDs")
		return
	}
	switch req.Kind {
	case "briefing", "investigation", "followup":
	default:
		writeJSONError(w, http.StatusBadRequest, "kind must be briefing, investigation or followup")
		return
	}
	info, err := s.svc.PostCompanionMessage(req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, info)
}
