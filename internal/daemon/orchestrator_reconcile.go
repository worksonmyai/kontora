package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
)

type executionObservation struct {
	Stopped  bool
	Evidence string
}

type effectObservation struct {
	Completed   bool
	SafeToRetry bool
	Receipt     string
}

type controlInspector interface {
	Attempt(context.Context, orchestrator.Attempt) (executionObservation, error)
	Action(context.Context, orchestrator.Action) (effectObservation, error)
}

func reconcileControl(ctx context.Context, store *controlstore.Store, inspect controlInspector) error {
	if inspect == nil {
		return errors.New("managed recovery requires an execution and effect inspector")
	}
	state, err := store.State()
	if err != nil {
		return err
	}
	for id, attempt := range state.Attempts {
		if !attempt.Reservation.Held {
			continue
		}
		observation, inspectErr := inspect.Attempt(ctx, attempt)
		err := transactControl(store, func(latest orchestrator.State) (orchestrator.State, error) {
			current, ok := latest.Attempts[id]
			if !ok || current.WorkID != attempt.WorkID || !current.Reservation.Held || latest.Work[current.WorkID].ActiveAttemptID != id {
				return orchestrator.State{}, orchestrator.ErrStale
			}
			command := orchestrator.Command{ExpectedRevision: latest.Revision, WorkID: current.WorkID, AttemptID: id}
			if inspectErr == nil && observation.Stopped && observation.Evidence != "" {
				command.Kind = orchestrator.StopAttempt
				command.AttemptStatus = stoppedAttemptStatus(current, false)
				command.Receipt = observation.Evidence
				command.At = time.Now()
				command.Usage.UnknownCost = true
			} else {
				command.Kind = orchestrator.UncertainAttempt
				command.Reason = "startup could not establish termination of exact attempt"
				if inspectErr != nil {
					command.Reason += "; " + inspectErr.Error()
				}
			}
			return orchestrator.Apply(latest, command)
		})
		if err != nil {
			return fmt.Errorf("reconcile attempt %s: %w", id, err)
		}
	}
	state, err = store.State()
	if err != nil {
		return err
	}
	for id, action := range state.Actions {
		if action.Status != orchestrator.ActionPending && action.Status != orchestrator.ActionParked || action.Kind == "planner_event" {
			continue
		}
		observation, inspectErr := inspect.Action(ctx, action)
		completed := inspectErr == nil && observation.Completed && observation.Receipt != ""
		if !completed && (action.Status == orchestrator.ActionParked || inspectErr == nil && !observation.Completed && observation.SafeToRetry) {
			continue
		}
		err := transactControl(store, func(latest orchestrator.State) (orchestrator.State, error) {
			command := orchestrator.Command{Kind: orchestrator.ResolveAction, ExpectedRevision: latest.Revision, ActionID: id, ActionStatus: orchestrator.ActionParked, Reason: "startup found an ambiguous effect"}
			if completed {
				command.ActionStatus = orchestrator.ActionCompleted
				command.Receipt = observation.Receipt
			}
			return orchestrator.Apply(latest, command)
		})
		if err != nil {
			return fmt.Errorf("reconcile action %s: %w", id, err)
		}
	}
	return nil
}
