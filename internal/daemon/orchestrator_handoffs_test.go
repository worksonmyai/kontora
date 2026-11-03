package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
)

func TestManagedHandoffs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		question bool
		confirm  bool
	}{
		{name: "cancel confirmed", confirm: true},
		{name: "cancel unconfirmed"},
		{name: "question confirmed", question: true, confirm: true},
		{name: "question unconfirmed", question: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			assignedControl(t, store)
			admitControl(t, store, "worker", "attempt")
			execution := managedExecution{store: store}
			done := make(chan struct{})
			cancel := context.CancelFunc(func() {
				state, err := store.State()
				require.NoError(t, err)
				want := orchestrator.AttemptCancelling
				terminal := orchestrator.AttemptCancelled
				if tc.question {
					want = orchestrator.AttemptParking
					terminal = orchestrator.AttemptParked
				}
				require.Equal(t, want, state.Attempts["attempt"].Status, "intent must be committed before cancellation")
				if tc.confirm {
					controlApply(t, store, orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: "worker", AttemptID: "attempt", AttemptStatus: terminal, At: time.Now(), Receipt: "process group gone"})
				}
				close(done)
			})
			var err error
			if tc.question {
				err = execution.question(orchestrator.Question{ID: "q", ObjectiveID: "objective", WorkID: "worker", AttemptID: "attempt", SpecificationRevision: 1, Prompt: "continue?"}, cancel, done)
			} else {
				err = execution.cancel("worker", "attempt", "operator cancellation", cancel, done)
			}
			if tc.confirm {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
