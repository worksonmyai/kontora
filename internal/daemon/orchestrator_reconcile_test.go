package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
)

type fakeControlInspector struct {
	execution executionObservation
	effect    effectObservation
	err       error
}

func (f fakeControlInspector) Attempt(context.Context, orchestrator.Attempt) (executionObservation, error) {
	return f.execution, f.err
}
func (f fakeControlInspector) Action(context.Context, orchestrator.Action) (effectObservation, error) {
	return f.effect, f.err
}

func TestControlReconciliationPreservesStopIntent(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind orchestrator.CommandKind
		want orchestrator.AttemptStatus
	}{
		{name: "cancel", kind: orchestrator.CancelAttempt, want: orchestrator.AttemptCancelled},
		{name: "park", kind: orchestrator.ParkAttempt, want: orchestrator.AttemptParked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			assignedControl(t, store)
			admitControl(t, store, "worker", "attempt")
			controlApply(t, store, orchestrator.Command{Kind: tc.kind, WorkID: "worker", AttemptID: "attempt", Reason: "operator request"})
			require.NoError(t, reconcileControl(context.Background(), store, fakeControlInspector{err: errors.New("temporarily unavailable")}))
			state, err := store.State()
			require.NoError(t, err)
			require.True(t, state.Attempts["attempt"].Reservation.Held)
			require.Equal(t, "operator request", state.Attempts["attempt"].CancellationReason)
			require.NoError(t, reconcileControl(context.Background(), store, fakeControlInspector{execution: executionObservation{Stopped: true, Evidence: "exact attempt group absent"}}))
			state, err = store.State()
			require.NoError(t, err)
			require.Equal(t, tc.want, state.Attempts["attempt"].Status)
			require.False(t, state.Attempts["attempt"].Reservation.Held)
			require.Equal(t, "operator request", state.Attempts["attempt"].CancellationReason)
		})
	}
}

func TestControlReconciliationParkedEffectEvidence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		observation effectObservation
		want        orchestrator.ActionStatus
	}{
		{name: "exact receipt", observation: effectObservation{Completed: true, Receipt: "exact effect observed"}, want: orchestrator.ActionCompleted},
		{name: "retry is not completion evidence", observation: effectObservation{SafeToRetry: true}, want: orchestrator.ActionParked},
		{name: "missing receipt", observation: effectObservation{Completed: true}, want: orchestrator.ActionParked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			assignedControl(t, store)
			controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordAction, Action: orchestrator.Action{ID: "effect", ObjectiveID: "objective", Kind: "projection", Preconditions: orchestrator.Preconditions{WorkID: "worker", DocumentRevision: 1, SpecificationRevision: 1}}})
			require.NoError(t, reconcileControl(context.Background(), store, fakeControlInspector{err: errors.New("temporarily unavailable")}))
			controlApply(t, store, orchestrator.Command{Kind: orchestrator.ReviseWork, WorkID: "worker", Work: orchestrator.WorkItem{Specification: "changed work"}, Preconditions: orchestrator.Preconditions{DocumentRevision: 1, SpecificationRevision: 1}})
			require.NoError(t, reconcileControl(context.Background(), store, fakeControlInspector{effect: tc.observation}))
			state, err := store.State()
			require.NoError(t, err)
			require.Equal(t, tc.want, state.Actions["effect"].Status)
			require.Error(t, orchestrator.ActionReady(state, "effect"), "evidence must not authorize replay")
			if tc.want == orchestrator.ActionCompleted {
				require.Equal(t, tc.observation.Receipt, state.Actions["effect"].Receipt)
			} else {
				require.Empty(t, state.Actions["effect"].Receipt)
			}
		})
	}
}

func TestControlReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inspector fakeControlInspector
		held      bool
		action    orchestrator.ActionStatus
	}{
		{name: "ambiguous", held: true, action: orchestrator.ActionParked},
		{name: "inspection failure", held: true, action: orchestrator.ActionParked, inspector: fakeControlInspector{err: errors.New("inspection failed")}},
		{name: "already completed", action: orchestrator.ActionCompleted, inspector: fakeControlInspector{execution: executionObservation{Stopped: true, Evidence: "exact group absent"}, effect: effectObservation{Completed: true, Receipt: "exact effect observed"}}},
		{name: "safe pending", action: orchestrator.ActionPending, inspector: fakeControlInspector{execution: executionObservation{Stopped: true, Evidence: "exact group absent"}, effect: effectObservation{SafeToRetry: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			assignedControl(t, store)
			admitControl(t, store, "worker", "attempt")
			controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordAction, Action: orchestrator.Action{ID: "effect", ObjectiveID: "objective", Kind: "projection"}})
			require.NoError(t, reconcileControl(context.Background(), store, tc.inspector))
			state, err := store.State()
			require.NoError(t, err)
			require.Equal(t, tc.held, state.Attempts["attempt"].Reservation.Held)
			require.Equal(t, tc.action, state.Actions["effect"].Status)
			if tc.held {
				require.Equal(t, orchestrator.AttemptUncertain, state.Attempts["attempt"].Status)
			}
		})
	}
}
