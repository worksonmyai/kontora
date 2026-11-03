package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
)

func TestPlannerConsumesOnlySnapshotWakes(t *testing.T) {
	store := newControlStore(t)
	assignedControl(t, store)
	controller := objectiveController{store: store}
	deadline := time.Now().Add(time.Hour)
	require.NoError(t, controller.wake("objective", "retry", deadline))
	require.NoError(t, controller.wake("objective", "feedback", time.Time{}))
	admitControl(t, store, "planner", "turn")
	snapshot, err := controller.snapshot("objective", "turn")
	require.NoError(t, err)
	require.Len(t, snapshot.Evidence.Wakes, 1)
	require.Equal(t, "feedback", snapshot.Evidence.Wakes[0].Name)
	controller.planner = func(_ context.Context, s orchestrator.PlannerSnapshot) (plannerOutput, error) {
		data, err := json.Marshal(orchestrator.Decision{Version: 1, TurnID: s.TurnID, SnapshotID: s.ID, Actions: []orchestrator.Proposal{{ID: "wait", Kind: "wait", Wait: &orchestrator.PlannerWait{Event: "feedback"}}}})
		return plannerOutput{Decision: data, TerminalSuccess: true}, err
	}
	require.NoError(t, controller.plan(context.Background(), snapshot))
	pending, err := controller.pending(time.Now())
	require.NoError(t, err)
	require.Empty(t, pending)
	pending, err = controller.pending(deadline)
	require.NoError(t, err)
	require.Equal(t, []string{"objective"}, pending)
}

func TestPlannerSnapshotIncludesOutput(t *testing.T) {
	store := newControlStore(t)
	assignedControl(t, store)
	controller := objectiveController{store: store}
	admitControl(t, store, "worker", "worker-run")
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: "worker", AttemptID: "worker-run", AttemptStatus: orchestrator.AttemptSucceeded, Receipt: "process_group_gone", At: time.Now()})
	artifact, err := store.PutArtifact([]byte("The endpoint requires a token."))
	require.NoError(t, err)
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordResult, Result: orchestrator.Result{ID: "answer", WorkID: "worker", AttemptID: "worker-run", Generation: 1, SpecificationRevision: 1, Artifacts: []orchestrator.ArtifactRef{artifact}, Valid: true}})
	admitControl(t, store, "planner", "turn")
	first, err := controller.snapshot("objective", "turn")
	require.NoError(t, err)
	require.Len(t, first.Evidence.Results, 1)
	require.Equal(t, "The endpoint requires a token.", first.Evidence.Results[0].Information[0].Text)
	require.Len(t, first.Evidence.Attempts, 2)
	second, err := controller.snapshot("objective", "turn")
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: "planner", AttemptID: "turn", AttemptStatus: orchestrator.AttemptSucceeded, Receipt: "planner stopped", At: time.Now()})
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.CreateObjective, Objective: orchestrator.Objective{ID: "other", Assignment: "unrelated", Criteria: []string{"separate output"}, Budget: orchestrator.Budget{MaxAttempts: 2, MaxPlannerTurns: 2, MaxRuntime: time.Hour}}})
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.SetObjectiveStatus, ObjectiveID: "other", Status: orchestrator.ObjectiveActive})
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutWork, Work: orchestrator.WorkItem{ID: "other-planner", ObjectiveID: "other", Kind: orchestrator.WorkPlanner, Specification: "plan"}})
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.AdmitAttempt, Capacity: 2, At: time.Now(), Attempt: orchestrator.Attempt{ID: "other-turn", WorkID: "other-planner", ObjectiveID: "other", SpecificationRevision: 1, Recipe: orchestrator.Recipe{Profile: "profile", ConfigurationRevision: "frozen", Timeout: time.Minute}, Reservation: orchestrator.Reservation{Slots: 1}}})
	other, err := controller.snapshot("other", "other-turn")
	require.NoError(t, err)
	require.Empty(t, other.Evidence.Results)
	require.Len(t, other.Evidence.Attempts, 1)
	changed := first
	changed.Evidence.Results[0].Commit = "different-retained-commit"
	frozen, err := orchestrator.FreezePlannerSnapshot(changed)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, frozen.ID)
}

func TestPlannerIntentPreservesSequence(t *testing.T) {
	store := newControlStore(t)
	assignedControl(t, store)
	admitControl(t, store, "planner", "turn")
	controller := objectiveController{store: store}
	snapshot, err := controller.snapshot("objective", "turn")
	require.NoError(t, err)
	controller.planner = func(_ context.Context, s orchestrator.PlannerSnapshot) (plannerOutput, error) {
		data, err := json.Marshal(orchestrator.Decision{Version: 1, TurnID: s.TurnID, SnapshotID: s.ID, Actions: []orchestrator.Proposal{
			{ID: "z", Kind: "update_work", Work: &orchestrator.PlannerWork{ID: "worker", Specification: "first"}},
			{ID: "a", Kind: "update_work", Work: &orchestrator.PlannerWork{ID: "worker", Specification: "second"}},
		}})
		return plannerOutput{Decision: data, TerminalSuccess: true}, err
	}
	require.NoError(t, controller.plan(context.Background(), snapshot))
	state, err := store.State()
	require.NoError(t, err)
	require.NoError(t, orchestrator.ActionReady(state, "turn/z"))
	require.ErrorIs(t, orchestrator.ActionReady(state, "turn/a"), orchestrator.ErrConflict)
	for i, id := range []string{"turn/z", "turn/a"} {
		data, err := store.ReadArtifact(state.Actions[id].Payload)
		require.NoError(t, err)
		var intent plannerIntent
		require.NoError(t, json.Unmarshal(data, &intent))
		require.Equal(t, i, intent.Sequence)
		require.Equal(t, snapshot.ID, intent.Snapshot.ID)
	}
	require.NoError(t, store.Transact(state.Revision, func(next orchestrator.State) (orchestrator.State, error) {
		return orchestrator.ApplyBatch(next, next.Revision, []orchestrator.Command{
			{Kind: orchestrator.ResolveAction, ExpectedRevision: next.Revision, ActionID: "turn/z", ActionStatus: orchestrator.ActionCompleted, Receipt: "first update committed"},
			{Kind: orchestrator.ReviseWork, ExpectedRevision: next.Revision, WorkID: "worker", Preconditions: state.Actions["turn/z"].Preconditions, Work: orchestrator.WorkItem{Specification: "first"}},
		})
	}))
	afterFirst, err := store.State()
	require.NoError(t, err)
	require.NoError(t, orchestrator.ActionReady(afterFirst, "turn/a"))
}
