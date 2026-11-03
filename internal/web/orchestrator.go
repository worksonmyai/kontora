package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/worksonmyai/kontora/internal/orchestrator"
)

var ErrOrchestratorUnavailable = errors.New("managed execution is disabled pending ownership, recovery, authority, and integration support")

// ObjectiveRequestError marks rejected client input without classifying storage failures as client errors.
type ObjectiveRequestError struct {
	Err error
}

func (e *ObjectiveRequestError) Error() string { return e.Err.Error() }
func (e *ObjectiveRequestError) Unwrap() error { return e.Err }

// ValidObjectiveID is the shared identity contract for objective API routes and clients.
func ValidObjectiveID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00\r\n\t ")
}

type ObjectiveSnapshot struct {
	BoardID              string                            `json:"board_id"`
	Revision             uint64                            `json:"revision"`
	Objective            orchestrator.Objective            `json:"objective"`
	Plans                []orchestrator.Plan               `json:"plans"`
	Work                 []orchestrator.WorkItem           `json:"work"`
	Attempts             []orchestrator.Attempt            `json:"attempts"`
	Results              []orchestrator.Result             `json:"results"`
	Actions              []orchestrator.Action             `json:"actions"`
	Questions            []orchestrator.Question           `json:"questions"`
	Acceptances          []orchestrator.Acceptance         `json:"acceptances"`
	VerificationPolicies []orchestrator.VerificationPolicy `json:"verification_policies"`
	Reviews              []orchestrator.CombinedReview     `json:"reviews"`
	ExecutionEnabled     bool                              `json:"execution_enabled"`
	DisabledReason       string                            `json:"disabled_reason,omitempty"`
}

type ObjectiveList struct {
	BoardID          string                   `json:"board_id"`
	Revision         uint64                   `json:"revision"`
	Objectives       []orchestrator.Objective `json:"objectives"`
	ExecutionEnabled bool                     `json:"execution_enabled"`
	DisabledReason   string                   `json:"disabled_reason,omitempty"`
}

type ObjectiveControlRequest struct {
	RequestID        string                  `json:"request_id"`
	BoardID          string                  `json:"board_id"`
	ExpectedRevision uint64                  `json:"expected_revision"`
	Operation        string                  `json:"operation"`
	ObjectiveID      string                  `json:"objective_id,omitempty"`
	Objective        *orchestrator.Objective `json:"objective,omitempty"`
	QuestionID       string                  `json:"question_id,omitempty"`
	ActionID         string                  `json:"action_id,omitempty"`
	Binding          string                  `json:"binding,omitempty"`
	Text             string                  `json:"text,omitempty"`
}

type ObjectiveControlResponse struct {
	BoardID     string `json:"board_id"`
	Revision    uint64 `json:"revision"`
	ObjectiveID string `json:"objective_id"`
	Operation   string `json:"operation"`
}

// OrchestratorService is optional; TicketService implementations need not implement it.
// Operator identity comes from transport context, never from ObjectiveControlRequest.
type OrchestratorService interface {
	ListObjectives(context.Context) (ObjectiveList, error)
	GetObjective(context.Context, string) (ObjectiveSnapshot, error)
	ControlObjective(context.Context, string, ObjectiveControlRequest) (ObjectiveControlResponse, error)
}

func (s *Server) handleObjectives(w http.ResponseWriter, r *http.Request) {
	if s.orchestrator == nil {
		objectiveError(w, ErrOrchestratorUnavailable)
		return
	}
	list, err := s.orchestrator.ListObjectives(r.Context())
	if err != nil {
		objectiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleObjective(w http.ResponseWriter, r *http.Request) {
	if s.orchestrator == nil {
		objectiveError(w, ErrOrchestratorUnavailable)
		return
	}
	snapshot, err := s.orchestrator.GetObjective(r.Context(), r.PathValue("id"))
	if err != nil {
		objectiveError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleObjectiveControl(w http.ResponseWriter, r *http.Request) {
	if s.orchestrator == nil {
		objectiveError(w, ErrOrchestratorUnavailable)
		return
	}
	operatorID, ok := OperatorIdentity(r.Context())
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authenticated operator authority is required"})
		return
	}
	var req ObjectiveControlRequest
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !validObjectiveJSON(data) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid objective request"})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid objective request: " + err.Error()})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "objective request must contain one JSON object"})
		return
	}
	if id := r.PathValue("id"); id != "" && req.ObjectiveID != id {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "objective id does not match path"})
		return
	}
	response, err := s.orchestrator.ControlObjective(r.Context(), operatorID, req)
	if err != nil {
		objectiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func objectiveError(w http.ResponseWriter, err error) {
	var status int
	var invalid *ObjectiveRequestError
	switch {
	case errors.Is(err, ErrOrchestratorUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, orchestrator.ErrStale), errors.Is(err, orchestrator.ErrConflict), errors.Is(err, orchestrator.ErrNotReady):
		status = http.StatusConflict
	case errors.Is(err, ErrTicketNotFound):
		status = http.StatusNotFound
	case errors.As(err, &invalid), errors.Is(err, orchestrator.ErrBudget):
		status = http.StatusBadRequest
	default:
		writeServiceError(w, err)
		return
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
