package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/web"
)

// ObjectiveService exposes durable controls without enabling managed admission.
// The board owner must hold the authority lock before installing this service.
type ObjectiveService struct {
	store *controlstore.Store
}

func NewObjectiveService(store *controlstore.Store) (*ObjectiveService, error) {
	if store == nil {
		return nil, fmt.Errorf("objective store is required")
	}
	return &ObjectiveService{store: store}, nil
}

var _ web.OrchestratorService = (*ObjectiveService)(nil)

func (s *ObjectiveService) ListObjectives(ctx context.Context) (web.ObjectiveList, error) {
	if err := ctx.Err(); err != nil {
		return web.ObjectiveList{}, err
	}
	state, err := s.store.State()
	if err != nil {
		return web.ObjectiveList{}, err
	}
	list := web.ObjectiveList{BoardID: state.BoardID, Revision: state.Revision, Objectives: []orchestrator.Objective{}, DisabledReason: web.ErrOrchestratorUnavailable.Error()}
	for _, objective := range state.Objectives {
		list.Objectives = append(list.Objectives, objective)
	}
	slices.SortFunc(list.Objectives, func(a, b orchestrator.Objective) int { return strings.Compare(a.ID, b.ID) })
	return list, nil
}

func (s *ObjectiveService) GetObjective(ctx context.Context, id string) (web.ObjectiveSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return web.ObjectiveSnapshot{}, err
	}
	state, err := s.store.State()
	if err != nil {
		return web.ObjectiveSnapshot{}, err
	}
	objective, ok := state.Objectives[id]
	if !ok {
		return web.ObjectiveSnapshot{}, web.ErrTicketNotFound
	}
	out := web.ObjectiveSnapshot{BoardID: state.BoardID, Revision: state.Revision, Objective: objective, DisabledReason: web.ErrOrchestratorUnavailable.Error(), Plans: []orchestrator.Plan{}, Work: []orchestrator.WorkItem{}, Attempts: []orchestrator.Attempt{}, Results: []orchestrator.Result{}, Actions: []orchestrator.Action{}, Questions: []orchestrator.Question{}, Acceptances: []orchestrator.Acceptance{}, VerificationPolicies: []orchestrator.VerificationPolicy{}, Reviews: []orchestrator.CombinedReview{}}
	for _, p := range state.Plans {
		if p.ObjectiveID == id {
			out.Plans = append(out.Plans, p)
		}
	}
	for _, w := range state.Work {
		if w.ObjectiveID == id {
			out.Work = append(out.Work, w)
		}
	}
	for _, a := range state.Attempts {
		if a.ObjectiveID == id {
			out.Attempts = append(out.Attempts, a)
		}
	}
	for _, r := range state.Results {
		if state.Work[r.WorkID].ObjectiveID == id {
			out.Results = append(out.Results, r)
		}
	}
	for _, a := range state.Actions {
		if a.ObjectiveID == id {
			out.Actions = append(out.Actions, a)
		}
	}
	for _, q := range state.Questions {
		if q.ObjectiveID == id {
			out.Questions = append(out.Questions, q)
		}
	}
	for _, a := range state.Acceptances {
		if a.ObjectiveID == id {
			out.Acceptances = append(out.Acceptances, a)
		}
	}
	for _, p := range state.VerificationPolicies {
		if p.ObjectiveID == id {
			out.VerificationPolicies = append(out.VerificationPolicies, p)
		}
	}
	for _, r := range state.Reviews {
		if r.ObjectiveID == id {
			out.Reviews = append(out.Reviews, r)
		}
	}
	slices.SortFunc(out.VerificationPolicies, func(a, b orchestrator.VerificationPolicy) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Reviews, func(a, b orchestrator.CombinedReview) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Plans, func(a, b orchestrator.Plan) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Work, func(a, b orchestrator.WorkItem) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Attempts, func(a, b orchestrator.Attempt) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Results, func(a, b orchestrator.Result) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Actions, func(a, b orchestrator.Action) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Questions, func(a, b orchestrator.Question) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.Acceptances, func(a, b orchestrator.Acceptance) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (s *ObjectiveService) ControlObjective(ctx context.Context, operatorID string, req web.ObjectiveControlRequest) (web.ObjectiveControlResponse, error) {
	if err := ctx.Err(); err != nil {
		return web.ObjectiveControlResponse{}, err
	}
	if strings.TrimSpace(operatorID) == "" || req.BoardID == "" || req.RequestID == "" {
		return web.ObjectiveControlResponse{}, &web.ObjectiveRequestError{Err: fmt.Errorf("operator, board, and request identity are required")}
	}
	if len(req.RequestID) > 256 {
		return web.ObjectiveControlResponse{}, &web.ObjectiveRequestError{Err: fmt.Errorf("request identity exceeds 256 bytes")}
	}
	data, err := json.Marshal(req)
	if err != nil {
		return web.ObjectiveControlResponse{}, err
	}
	if len(data) > 1<<20 {
		return web.ObjectiveControlResponse{}, &web.ObjectiveRequestError{Err: fmt.Errorf("objective request exceeds limit")}
	}
	body, err := s.store.ExecuteRequest(req.RequestID, operatorID, data, req.ExpectedRevision, func(state orchestrator.State) (orchestrator.State, []byte, error) {
		if req.BoardID != state.BoardID {
			return state, nil, orchestrator.ErrConflict
		}
		commands, objectiveID, err := objectiveCommands(state, operatorID, req)
		if err != nil {
			return state, nil, &web.ObjectiveRequestError{Err: err}
		}
		next, err := orchestrator.ApplyBatch(state, state.Revision, commands)
		if err != nil {
			return state, nil, &web.ObjectiveRequestError{Err: err}
		}
		response, err := json.Marshal(web.ObjectiveControlResponse{BoardID: state.BoardID, Revision: next.Revision, ObjectiveID: objectiveID, Operation: req.Operation})
		return next, response, err
	})
	if err != nil {
		return web.ObjectiveControlResponse{}, err
	}
	var response web.ObjectiveControlResponse
	err = json.Unmarshal(body, &response)
	return response, err
}

func objectiveCommands(state orchestrator.State, operatorID string, req web.ObjectiveControlRequest) ([]orchestrator.Command, string, error) {
	id := req.ObjectiveID
	command := orchestrator.Command{ExpectedRevision: state.Revision, ObjectiveID: id, OperatorID: operatorID, At: time.Now().UTC()}
	if req.Operation != "assign" && state.Objectives[id].ID == "" {
		return nil, id, web.ErrTicketNotFound
	}
	switch req.Operation {
	case "assign":
		if req.Objective == nil || id == "" || req.Objective.ID != id || len(req.Objective.Criteria) == 0 || !req.Objective.Policy.RequireCommitApproval || !req.Objective.Policy.RequireAcceptance {
			return nil, id, fmt.Errorf("Draft assignment requires an objective whose nonempty id matches objective_id. Criteria, commit approval, and human acceptance are required.") //nolint:revive,staticcheck // Keep the operator-facing diagnostic as complete sentences.
		}
		if !web.ValidObjectiveID(id) {
			return nil, id, fmt.Errorf("invalid objective id")
		}
		command.Kind, command.Objective = orchestrator.CreateObjective, *req.Objective
	case "pause", "cancel":
		if strings.TrimSpace(req.Text) == "" {
			return nil, id, fmt.Errorf("control reason is required")
		}
		command.Kind, command.Status, command.Reason = orchestrator.SetObjectiveStatus, orchestrator.ObjectivePaused, req.Text
		if req.Operation == "cancel" {
			command.Status = orchestrator.ObjectiveCancelled
		}
	case "resume", "release", "accept":
		return nil, id, web.ErrOrchestratorUnavailable
	case "answer":
		q := state.Questions[req.QuestionID]
		if q.ObjectiveID != id {
			return nil, id, orchestrator.ErrConflict
		}
		command.Kind, command.QuestionID, command.Answer = orchestrator.AnswerQuestion, q.ID, req.Text
	case "approve":
		a := state.Actions[req.ActionID]
		if a.ObjectiveID != id || req.Binding != orchestrator.ActionBinding(a) {
			return nil, id, orchestrator.ErrStale
		}
		command.Kind, command.ActionID = orchestrator.ApproveAction, a.ID
		command.Approval = &orchestrator.Approval{OperatorID: operatorID, Binding: req.Binding, ApprovedAt: command.At}
	case "feedback":
		if strings.TrimSpace(req.Text) == "" {
			return nil, id, fmt.Errorf("feedback is required")
		}
		o := state.Objectives[id]
		if o.Status == orchestrator.ObjectiveAccepted || o.Status == orchestrator.ObjectiveCancelled {
			return nil, id, orchestrator.ErrConflict
		}
		command.Kind = orchestrator.PutWork
		command.Work = orchestrator.WorkItem{ID: "feedback/" + req.RequestID, ObjectiveID: id, Kind: orchestrator.WorkJob, Specification: req.Text, AcceptanceRequired: true}
	default:
		return nil, id, fmt.Errorf("unknown objective operation %q", req.Operation)
	}
	return []orchestrator.Command{command}, id, nil
}
