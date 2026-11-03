package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
)

type plannerEvent struct {
	Name    string    `json:"name"`
	RetryAt time.Time `json:"retry_at,omitzero"`
}

type plannerIntent struct {
	Snapshot         orchestrator.PlannerSnapshot `json:"snapshot"`
	Sequence         int                          `json:"sequence"`
	PreviousActionID string                       `json:"previous_action_id,omitempty"`
	Proposal         orchestrator.Proposal        `json:"proposal"`
}

type plannerOutput struct {
	Decision        []byte
	TerminalSuccess bool
}

type plannerTurn func(context.Context, orchestrator.PlannerSnapshot) (plannerOutput, error)

type objectiveController struct {
	store    *controlstore.Store
	profiles []string
	planner  plannerTurn
}

func controlID(prefix string) string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(id[:])
}

func (c *objectiveController) wake(objectiveID, event string, retryAt time.Time) error {
	if event == "" {
		return errors.New("planner event is required")
	}
	state, err := c.store.State()
	if err != nil {
		return err
	}
	objective, ok := state.Objectives[objectiveID]
	if !ok || objective.Status != orchestrator.ObjectiveActive {
		return errors.New("objective is not assigned and active")
	}
	data, err := json.Marshal(plannerEvent{Name: event, RetryAt: retryAt})
	if err != nil {
		return err
	}
	artifact, err := c.store.PutArtifact(data)
	if err != nil {
		return err
	}
	duplicate := ""
	for id, action := range state.Actions {
		if action.ObjectiveID != objectiveID || action.Kind != "planner_event" || action.Status != orchestrator.ActionPending {
			continue
		}
		existing, err := c.store.ReadArtifact(action.Payload)
		if err != nil {
			return err
		}
		var pending plannerEvent
		if err := json.Unmarshal(existing, &pending); err != nil {
			return err
		}
		if pending.Name == event {
			if pending.RetryAt.IsZero() || !retryAt.IsZero() && !retryAt.Before(pending.RetryAt) {
				return nil
			}
			duplicate = id
			break
		}
	}
	return c.store.Transact(state.Revision, func(next orchestrator.State) (orchestrator.State, error) {
		if duplicate != "" {
			action := next.Actions[duplicate]
			action.Status = orchestrator.ActionCompleted
			action.Receipt = "superseded by earlier wake"
			next.Actions[duplicate] = action
		}
		id := controlID("event-")
		next.Actions[id] = orchestrator.Action{ID: id, ObjectiveID: objectiveID, Kind: "planner_event", Payload: artifact, Status: orchestrator.ActionPending}
		next.Revision++
		return next, nil
	})
}

func (c *objectiveController) pending(now time.Time) ([]string, error) {
	state, err := c.store.State()
	if err != nil {
		return nil, err
	}
	objectives := map[string]bool{}
	for _, action := range state.Actions {
		objective, ok := state.Objectives[action.ObjectiveID]
		if !ok || objective.Status != orchestrator.ObjectiveActive || action.Kind != "planner_event" || action.Status != orchestrator.ActionPending {
			continue
		}
		data, err := c.store.ReadArtifact(action.Payload)
		if err != nil {
			return nil, err
		}
		var event plannerEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		if event.RetryAt.IsZero() || !now.Before(event.RetryAt) {
			objectives[action.ObjectiveID] = true
		}
	}
	ids := make([]string, 0, len(objectives))
	for id := range objectives {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (c *objectiveController) snapshot(objectiveID, turnID string) (orchestrator.PlannerSnapshot, error) {
	state, err := c.store.State()
	if err != nil {
		return orchestrator.PlannerSnapshot{}, err
	}
	objective, ok := state.Objectives[objectiveID]
	if !ok || objective.Status != orchestrator.ObjectiveActive {
		return orchestrator.PlannerSnapshot{}, errors.New("objective is not active")
	}
	snapshot := orchestrator.PlannerSnapshot{TurnID: turnID, ObjectiveID: objectiveID, Assignment: objective.Assignment, Criteria: slices.Clone(objective.Criteria), CriteriaRevision: objective.CriteriaRevision, PolicyRevision: objective.Policy.Revision, Budget: objective.Budget, Usage: objective.Usage, Revision: state.Revision, Repositories: objective.Policy.Repositories, Profiles: slices.Clone(c.profiles), Work: map[string]orchestrator.PlannerWork{}}
	attempt, admitted := state.Attempts[turnID]
	if !admitted || attempt.ObjectiveID != objectiveID || !attempt.Reservation.Held || attempt.Status != orchestrator.AttemptRunning || state.Work[attempt.WorkID].Kind != orchestrator.WorkPlanner {
		return snapshot, errors.New("planner turn requires an admitted attempt")
	}
	snapshot.RemainingActions = 64
	for _, plan := range state.Plans {
		if plan.ObjectiveID == objectiveID && plan.Revision == objective.CurrentPlan {
			snapshot.RequiredResults = slices.Clone(plan.RequiredResults)
		}
	}
	for id, work := range state.Work {
		if work.ObjectiveID != objectiveID || work.Kind == orchestrator.WorkPlanner {
			continue
		}
		snapshot.Work[id] = orchestrator.PlannerWork{ID: id, Specification: work.Specification, Dependencies: slices.Clone(work.Dependencies)}
	}
	snapshot.Evidence, err = c.evidence(state, objectiveID, time.Now())
	if err != nil {
		return snapshot, err
	}
	return orchestrator.FreezePlannerSnapshot(snapshot)
}

// plan records proposals only. Applying effects requires the later authority and integration paths.
func (c *objectiveController) plan(ctx context.Context, snapshot orchestrator.PlannerSnapshot) error {
	if c.planner == nil {
		return errors.New("planner adapter is not configured")
	}
	current, err := c.store.State()
	if err != nil {
		return err
	}
	attempt, admitted := current.Attempts[snapshot.TurnID]
	if !admitted || attempt.ObjectiveID != snapshot.ObjectiveID || !attempt.Reservation.Held || attempt.Status != orchestrator.AttemptRunning || current.Work[attempt.WorkID].Kind != orchestrator.WorkPlanner || current.Revision != snapshot.Revision {
		return errors.New("planner turn is not the current admitted attempt")
	}
	output, err := c.planner(ctx, snapshot)
	if err != nil {
		return err
	}
	if !output.TerminalSuccess {
		return errors.New("planner did not report terminal success")
	}
	decision, err := orchestrator.DecodeDecision(output.Decision, snapshot, time.Now())
	if err != nil {
		return err
	}
	intents := make([]orchestrator.Action, 0, len(decision.Actions))
	previous := ""
	work := maps.Clone(current.Work)
	for sequence, proposal := range decision.Actions {
		data, err := json.Marshal(plannerIntent{Snapshot: snapshot, Sequence: sequence, PreviousActionID: previous, Proposal: proposal})
		if err != nil {
			return err
		}
		artifact, err := c.store.PutArtifact(data)
		if err != nil {
			return err
		}
		preconditions := orchestrator.Preconditions{PreviousActionID: previous}
		target := proposal.Target
		if proposal.Kind == "update_work" {
			target = proposal.Work.ID
		}
		if targetWork, ok := work[target]; ok {
			preconditions.WorkID = targetWork.ID
			preconditions.DocumentRevision = targetWork.DocumentRevision
			preconditions.SpecificationRevision = targetWork.SpecificationRevision
			preconditions.AttemptID = targetWork.ActiveAttemptID
		}
		id := snapshot.TurnID + "/" + proposal.ID
		intents = append(intents, orchestrator.Action{ID: id, ObjectiveID: snapshot.ObjectiveID, Kind: proposal.Kind, Payload: artifact, Preconditions: preconditions, Status: orchestrator.ActionPending, RequiresApproval: proposal.Kind == "request_approval"})
		previous = id
		if proposal.Kind == "update_work" {
			updated := work[proposal.Work.ID]
			updated.DocumentRevision++
			updated.SpecificationRevision++
			work[updated.ID] = updated
		}
	}
	return c.store.Transact(snapshot.Revision, func(next orchestrator.State) (orchestrator.State, error) {
		objective, ok := next.Objectives[snapshot.ObjectiveID]
		if !ok || objective.Status != orchestrator.ObjectiveActive {
			return next, errors.New("objective no longer active")
		}
		for _, intent := range intents {
			if _, exists := next.Actions[intent.ID]; exists {
				return next, fmt.Errorf("duplicate action %s", intent.ID)
			}
		}
		for _, intent := range intents {
			next.Actions[intent.ID] = intent
		}
		if snapshot.Evidence != nil {
			for _, wake := range snapshot.Evidence.Wakes {
				event := next.Actions[wake.ID]
				if event.ObjectiveID != snapshot.ObjectiveID || event.Kind != "planner_event" || event.Status != orchestrator.ActionPending {
					return next, orchestrator.ErrStale
				}
				event.Status = orchestrator.ActionCompleted
				event.Receipt = snapshot.TurnID
				next.Actions[wake.ID] = event
			}
		}
		next.Revision++
		return next, nil
	})
}
