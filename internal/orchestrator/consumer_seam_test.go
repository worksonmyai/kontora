package orchestrator_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/daemon"
	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
)

func TestObserveInvalidatesLiveConsumer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deleted     bool
		independent bool
	}{
		{name: "external edit"},
		{name: "external deletion", deleted: true},
		{name: "independent consumer after edit", independent: true},
		{name: "independent consumer after deletion", deleted: true, independent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tst-001.md")
			require.NoError(t, os.WriteFile(path, []byte("---\nid: tst-001\nkind: epic\nstatus: open\nkontora: true\n---\n# Producer\n"), 0o600))
			authority := controlstore.Authority{Version: 1, BoardID: "board", OwnerID: "owner", StateDir: t.TempDir()}
			managed, err := daemon.OpenManagedTickets(dir, authority, daemon.ManagedTicketHooks{Reserve: func(_ string, commit func() error) error { return commit() }})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, managed.Close()) })
			state := func() orchestrator.State {
				t.Helper()
				s, err := managed.Control.State()
				require.NoError(t, err)
				return s
			}
			apply := func(c orchestrator.Command) {
				t.Helper()
				c.ExpectedRevision = state().Revision
				require.NoError(t, managed.Control.Apply(c))
			}
			apply(orchestrator.Command{Kind: orchestrator.CreateObjective, Objective: orchestrator.Objective{ID: "o", Assignment: "produce and consume", Criteria: []string{"retained output"}, Budget: orchestrator.Budget{MaxAttempts: 10, MaxRuntime: time.Hour}}})
			_, err = managed.Enroll("tst-001", "producer", "o", state().Revision)
			require.NoError(t, err)
			apply(orchestrator.Command{Kind: orchestrator.SetObjectiveStatus, ObjectiveID: "o", Status: orchestrator.ObjectiveActive})
			now := time.Now()
			admit := func(workID, attemptID string) {
				t.Helper()
				w := state().Work[workID]
				apply(orchestrator.Command{Kind: orchestrator.AdmitAttempt, At: now, Capacity: 1, Attempt: orchestrator.Attempt{ID: attemptID, WorkID: workID, ObjectiveID: "o", SpecificationRevision: w.SpecificationRevision, Inputs: w.Inputs, Recipe: orchestrator.Recipe{Profile: "worker", ConfigurationRevision: "frozen", Timeout: time.Minute}, Reservation: orchestrator.Reservation{Slots: 1}}})
			}
			admit("producer", "produce")
			apply(orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: "producer", AttemptID: "produce", AttemptStatus: orchestrator.AttemptSucceeded, Receipt: "exact producer stopped", At: now.Add(time.Second)})
			apply(orchestrator.Command{Kind: orchestrator.RecordResult, Result: orchestrator.Result{ID: "r", WorkID: "producer", AttemptID: "produce", Generation: 1, SpecificationRevision: 1, Commit: "retained", Valid: true}})
			apply(orchestrator.Command{Kind: orchestrator.PutWork, Work: orchestrator.WorkItem{ID: "consumer", ObjectiveID: "o", Kind: orchestrator.WorkJob, Specification: "consume retained output", Inputs: []orchestrator.ResultRef{{ResultID: "r", WorkID: "producer", Generation: 1}}}})
			admit("consumer", "consume")
			if tc.independent {
				apply(orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: "consumer", AttemptID: "consume", AttemptStatus: orchestrator.AttemptSucceeded, Receipt: "consumer stopped", At: now.Add(time.Second)})
				apply(orchestrator.Command{Kind: orchestrator.RecordResult, Result: orchestrator.Result{ID: "historical", WorkID: "consumer", AttemptID: "consume", Generation: 1, SpecificationRevision: 1, Inputs: state().Work["consumer"].Inputs, Commit: "old output", Valid: true}})
				apply(orchestrator.Command{Kind: orchestrator.ReviseWork, WorkID: "consumer", Preconditions: orchestrator.Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: orchestrator.WorkItem{Specification: "independent work"}})
				admit("consumer", "independent")
			}
			if tc.deleted {
				require.NoError(t, os.Remove(path))
			} else {
				require.NoError(t, os.WriteFile(path, []byte("external edit\n"), 0o600))
			}
			require.ErrorContains(t, managed.Observe("tst-001"), "diverged")
			s := state()
			require.False(t, s.Results["r"].Valid)
			require.NotEmpty(t, s.Work["producer"].Divergence)
			if tc.independent {
				require.Empty(t, s.Work["consumer"].Divergence)
				require.Equal(t, orchestrator.AttemptRunning, s.Attempts["independent"].Status)
				require.True(t, s.Attempts["independent"].Reservation.Held)
				return
			}
			require.NotEmpty(t, s.Work["consumer"].Divergence)
			require.Equal(t, orchestrator.AttemptCancelling, s.Attempts["consume"].Status)
			require.True(t, s.Attempts["consume"].Reservation.Held)
			require.ErrorIs(t, orchestrator.Ready(s, "consumer"), orchestrator.ErrNotReady)
			require.NoError(t, managed.Close())
			managed, err = daemon.OpenManagedTickets(dir, authority, daemon.ManagedTicketHooks{})
			require.NoError(t, err)
			require.False(t, state().Results["r"].Valid)
			apply(orchestrator.Command{Kind: orchestrator.UncertainAttempt, WorkID: "consumer", AttemptID: "consume", Reason: "inspection temporarily unavailable"})
			require.Equal(t, orchestrator.AttemptCancelling, state().Attempts["consume"].Status)
			_, err = orchestrator.Apply(state(), orchestrator.Command{Kind: orchestrator.StopAttempt, ExpectedRevision: state().Revision, WorkID: "consumer", AttemptID: "consume", AttemptStatus: orchestrator.AttemptSucceeded, Receipt: "late success", At: now.Add(time.Second)})
			require.ErrorIs(t, err, orchestrator.ErrConflict)
			apply(orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: "consumer", AttemptID: "consume", AttemptStatus: orchestrator.AttemptCancelled, Receipt: "exact consumer stopped", At: now.Add(time.Second)})
			require.False(t, state().Attempts["consume"].Reservation.Held)
			require.Equal(t, orchestrator.AttemptCancelled, state().Attempts["consume"].Status)
		})
	}
}
