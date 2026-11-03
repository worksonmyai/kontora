package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func newControlStore(t *testing.T) *controlstore.Store {
	t.Helper()
	store, err := controlstore.Open(t.TempDir(), "board")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func controlApply(t *testing.T, store *controlstore.Store, command orchestrator.Command) {
	t.Helper()
	state, err := store.State()
	require.NoError(t, err)
	command.ExpectedRevision = state.Revision
	require.NoError(t, store.Apply(command))
}

func assignedControl(t *testing.T, store *controlstore.Store) {
	t.Helper()
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.CreateObjective, Objective: orchestrator.Objective{ID: "objective", Assignment: "implement", Criteria: []string{"checked output"}, Budget: orchestrator.Budget{MaxAttempts: 20, MaxPlannerTurns: 10, MaxRuntime: time.Hour}}})
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.SetObjectiveStatus, ObjectiveID: "objective", Status: orchestrator.ObjectiveActive})
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutWork, Work: orchestrator.WorkItem{ID: "planner", ObjectiveID: "objective", Kind: orchestrator.WorkPlanner, Specification: "plan"}})
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutWork, Work: orchestrator.WorkItem{ID: "worker", ObjectiveID: "objective", Kind: orchestrator.WorkJob, Specification: "implement"}})
}

func admitControl(t *testing.T, store *controlstore.Store, workID, attemptID string) {
	t.Helper()
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.AdmitAttempt, Capacity: 1, At: time.Now(), Attempt: orchestrator.Attempt{ID: attemptID, WorkID: workID, ObjectiveID: "objective", SpecificationRevision: 1, Recipe: orchestrator.Recipe{Profile: "profile", ConfigurationRevision: "frozen", Timeout: time.Minute}, Reservation: orchestrator.Reservation{Slots: 1}}})
}

func TestControllerEvents(t *testing.T) {
	store := newControlStore(t)
	controller := objectiveController{store: store}
	pending, err := controller.pending(time.Now())
	require.NoError(t, err)
	require.Empty(t, pending)
	require.Error(t, controller.wake("unassigned", "feedback", time.Time{}))
	assignedControl(t, store)
	deadline := time.Now().Add(time.Hour)
	require.NoError(t, controller.wake("objective", "result", deadline))
	require.NoError(t, controller.wake("objective", "result", deadline))
	state, err := store.State()
	require.NoError(t, err)
	require.Len(t, state.Actions, 1)
	pending, err = controller.pending(time.Now())
	require.NoError(t, err)
	require.Empty(t, pending)
	require.NoError(t, store.Snapshot())
	pending, err = controller.pending(deadline)
	require.NoError(t, err)
	require.Equal(t, []string{"objective"}, pending)
	require.NoError(t, controller.wake("objective", "result", time.Time{}))
	pending, err = controller.pending(time.Now())
	require.NoError(t, err)
	require.Equal(t, []string{"objective"}, pending)
}

func TestControllerProposals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		stale    bool
		invalid  bool
		valid    bool
	}{
		{name: "valid", terminal: true, valid: true},
		{name: "missing terminal success"},
		{name: "stale snapshot", terminal: true, stale: true},
		{name: "whole proposal rejected", terminal: true, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			assignedControl(t, store)
			controller := objectiveController{store: store, profiles: []string{"profile"}}
			require.NoError(t, controller.wake("objective", "assignment", time.Time{}))
			admitControl(t, store, "planner", "turn")
			snapshot, err := controller.snapshot("objective", "turn")
			require.NoError(t, err)
			controller.planner = func(_ context.Context, snapshot orchestrator.PlannerSnapshot) (plannerOutput, error) {
				decision := orchestrator.Decision{Version: 1, TurnID: snapshot.TurnID, SnapshotID: snapshot.ID, Actions: []orchestrator.Proposal{{ID: "1", Kind: "schedule", Target: "worker", Profile: "profile"}}}
				if tc.invalid {
					decision.Actions = append(decision.Actions, orchestrator.Proposal{ID: "2", Kind: "schedule", Target: "foreign", Profile: "profile"})
				}
				data, err := json.Marshal(decision)
				return plannerOutput{Decision: data, TerminalSuccess: tc.terminal}, err
			}
			if tc.stale {
				require.NoError(t, controller.wake("objective", "feedback", time.Time{}))
			}
			err = controller.plan(context.Background(), snapshot)
			state, stateErr := store.State()
			require.NoError(t, stateErr)
			if tc.valid {
				require.NoError(t, err)
				action := state.Actions["turn/1"]
				require.Equal(t, "schedule", action.Kind)
				require.Equal(t, orchestrator.SpecificationRevision(1), action.Preconditions.SpecificationRevision)
				data, err := store.ReadArtifact(action.Payload)
				require.NoError(t, err)
				var intent plannerIntent
				require.NoError(t, json.Unmarshal(data, &intent))
				require.Equal(t, snapshot.ID, intent.Snapshot.ID)
				require.Equal(t, 0, intent.Sequence)
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.ReviseWork, WorkID: "worker", Work: orchestrator.WorkItem{Specification: "changed"}, Preconditions: orchestrator.Preconditions{DocumentRevision: 1, SpecificationRevision: 1}})
				changed, err := store.State()
				require.NoError(t, err)
				require.ErrorIs(t, orchestrator.ActionReady(changed, action.ID), orchestrator.ErrStale)
			} else {
				require.Error(t, err)
				require.Empty(t, state.Actions["turn/1"].ID)
			}
		})
	}
}

func TestAuthorityPrecedesOrdinaryRecovery(t *testing.T) {
	h := newHarness(t)
	path := h.writeTicket("kon-owned.md", "---\nid: kon-owned\nstatus: in_progress\nkontora: true\n---\n# owned\n")
	store, err := controlstore.OpenBoard(h.tasksDir, controlstore.Authority{BoardID: "board", OwnerID: "owner", StateDir: filepath.Join(t.TempDir(), "state")})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	d := h.newDaemon(h.cfg)
	require.ErrorContains(t, d.Run(context.Background()), "managed execution remains disabled")
	content, err := ticket.ParseFile(path)
	require.NoError(t, err)
	require.Equal(t, "in_progress", string(content.Status))
}
