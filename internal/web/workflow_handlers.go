package web

import (
	"encoding/json"
	"io"
	"net/http"
)

func (s *Server) handleWorkflowRestart(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, s.svc.WorkflowRestart)
}

func (s *Server) handleWorkflowRecover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExecutionID string  `json:"execution_id"`
		Revision    *uint64 `json:"revision"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.ExecutionID == "" || body.Revision == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "execution_id and revision are required"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected one JSON object"})
		return
	}
	s.handleAction(w, r, func(id string) error { return s.svc.WorkflowRecover(id, body.ExecutionID, *body.Revision) })
}
