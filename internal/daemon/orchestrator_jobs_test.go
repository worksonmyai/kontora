package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestManagedExecution(t *testing.T) {
	for _, tc := range []struct {
		name              string
		kind              orchestrator.WorkKind
		leases            []orchestrator.Lease
		evidence          process.TerminationEvidence
		parking           bool
		secretEnvironment bool
		timeout           bool
		want              orchestrator.AttemptStatus
		held              bool
	}{
		{name: "ticketless job", kind: orchestrator.WorkJob, evidence: process.TerminationProcessGroupGone, want: orchestrator.AttemptSucceeded},
		{name: "operator secrets removed before freezing", kind: orchestrator.WorkJob, secretEnvironment: true, evidence: process.TerminationProcessGroupGone, want: orchestrator.AttemptSucceeded},
		{name: "leased job", kind: orchestrator.WorkJob, leases: []orchestrator.Lease{{Key: "repo", WorkID: "job"}}, evidence: process.TerminationProcessGroupGone, want: orchestrator.AttemptSucceeded},
		{name: "accounted summary", kind: orchestrator.WorkSummary, evidence: process.TerminationProcessGroupGone, want: orchestrator.AttemptSucceeded},
		{name: "timeout with exit zero", kind: orchestrator.WorkJob, evidence: process.TerminationProcessGroupGone, timeout: true, want: orchestrator.AttemptFailed},
		{name: "no worker started", kind: orchestrator.WorkJob, evidence: process.TerminationNotStarted, want: orchestrator.AttemptFailed},
		{name: "question stopped", kind: orchestrator.WorkJob, evidence: process.TerminationProcessGroupGone, parking: true, want: orchestrator.AttemptParked},
		{name: "question still alive", kind: orchestrator.WorkJob, parking: true, want: orchestrator.AttemptParking, held: true},
		{name: "tmux absence is insufficient", kind: orchestrator.WorkJob, evidence: process.TerminationWindowGone, want: orchestrator.AttemptUncertain, held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			assignedControl(t, store)
			controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutWork, Work: orchestrator.WorkItem{ID: "job", ObjectiveID: "objective", Kind: tc.kind, Specification: "perform work"}})
			execution := managedExecution{store: store, logsDir: t.TempDir(), capacity: 1}
			command := frozenCommand{Binary: "worker", Directory: t.TempDir(), Args: []string{"frozen"}}
			if tc.secretEnvironment {
				execution.operatorSecretKeys = []string{"OPERATOR_ALIAS"}
				command.Environment = map[string]string{"KONTORA_TOKEN": "shared-secret", "OPERATOR_ALIAS": "operator-secret", "SAFE": "value"}
			}
			recipe, err := execution.freeze("worker-"+string(tc.kind), time.Minute, command)
			require.NoError(t, err)
			if tc.secretEnvironment {
				data, err := store.ReadArtifact(recipe.Artifacts[0])
				require.NoError(t, err)
				require.NotContains(t, string(data), "shared-secret")
				require.NotContains(t, string(data), "operator-secret")
				var frozen frozenCommand
				require.NoError(t, json.Unmarshal(data, &frozen))
				require.Equal(t, map[string]string{"SAFE": "value"}, frozen.Environment)
			}
			require.NoError(t, execution.reserve(orchestrator.Attempt{ID: "attempt", WorkID: "job", ObjectiveID: "objective", SpecificationRevision: 1, Recipe: recipe, Reservation: orchestrator.Reservation{Slots: 1}}, tc.leases))
			execution.runner = func(_ context.Context, params RunnerParams) (process.Result, error) {
				require.Equal(t, "attempt", params.AttemptID)
				require.Empty(t, params.TicketID)
				require.Equal(t, []string{"frozen"}, params.Args)
				require.True(t, params.ReplaceEnv)
				if tc.secretEnvironment {
					require.Equal(t, map[string]string{"SAFE": "value"}, params.Env)
				}
				require.Contains(t, params.LogFile, "attempts/attempt/output.log")
				if tc.parking {
					controlApply(t, store, orchestrator.Command{Kind: orchestrator.AskQuestion, Question: orchestrator.Question{ID: "question", ObjectiveID: "objective", WorkID: "job", AttemptID: "attempt", SpecificationRevision: 1, Prompt: "continue?"}})
				}
				result := process.Result{ExitCode: 0, Termination: tc.evidence}
				if tc.timeout {
					result.Interruption = context.DeadlineExceeded
				}
				return result, nil
			}
			require.Equal(t, !tc.held, execution.execute(context.Background(), "attempt"))
			state, err := store.State()
			require.NoError(t, err)
			require.Equal(t, tc.want, state.Attempts["attempt"].Status)
			require.Equal(t, "worker-"+string(tc.kind), state.Attempts["attempt"].Recipe.Profile)
			if !tc.held {
				require.Empty(t, state.Leases)
			}
			require.Equal(t, tc.held, state.Attempts["attempt"].Reservation.Held)
			require.Equal(t, uint64(1), state.Objectives["objective"].Usage.Attempts)
			require.Empty(t, state.Results, "exit zero must not manufacture a usable result")
		})
	}
}

func TestManagedConcurrentCompletions(t *testing.T) {
	const workers = 12
	store := newControlStore(t)
	assignedControl(t, store)
	for i := range workers {
		id := fmt.Sprintf("job-%d", i)
		controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutWork, Work: orchestrator.WorkItem{ID: id, ObjectiveID: "objective", Kind: orchestrator.WorkJob, Specification: "work"}})
	}
	h := newHarness(t)
	h.cfg.MaxConcurrentAgents = workers
	d := h.newDaemon(h.cfg)
	d.managedDispatch = &managedDispatch{enabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.dispatch(ctx, &wg) })
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		wg.Wait()
	}()
	started := make(chan struct{}, workers)
	execution := managedExecution{store: store, logsDir: t.TempDir(), capacity: workers, runner: func(context.Context, RunnerParams) (process.Result, error) {
		started <- struct{}{}
		<-release
		return process.Result{ExitCode: 0, Termination: process.TerminationProcessGroupGone}, nil
	}}
	recipe, err := execution.freeze("profile", time.Minute, frozenCommand{Binary: "worker", Directory: t.TempDir()})
	require.NoError(t, err)
	for i := range workers {
		id := fmt.Sprintf("job-%d", i)
		attempt := orchestrator.Attempt{ID: id, WorkID: id, ObjectiveID: "objective", SpecificationRevision: 1, Recipe: recipe, Reservation: orchestrator.Reservation{Slots: 1}}
		require.NoError(t, d.queueManagedJob(&dispatchJob{objectiveID: "objective", attemptID: id, slots: attempt.Reservation.Slots, ready: func() bool { return true }, reserve: func() error { return execution.reserve(attempt, nil) }, run: func(ctx context.Context) bool { return execution.execute(ctx, id) }}))
	}
	for range workers {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("worker was not admitted")
		}
	}
	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, func() bool { return len(d.sem) == 0 }, 5*time.Second, time.Millisecond)
	state, err := store.State()
	require.NoError(t, err)
	for _, attempt := range state.Attempts {
		require.False(t, attempt.Reservation.Held)
		require.Equal(t, orchestrator.AttemptSucceeded, attempt.Status)
	}
	d.mu.Lock()
	active := len(d.managedDispatch.active)
	d.mu.Unlock()
	require.Zero(t, active)
}

func TestManagedSpawnFailure(t *testing.T) {
	store := newControlStore(t)
	assignedControl(t, store)
	execution := managedExecution{store: store, logsDir: t.TempDir(), capacity: 1, runner: DirectRunner}
	recipe, err := execution.freeze("profile", time.Minute, frozenCommand{Binary: "/no/such/worker", Directory: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, execution.reserve(orchestrator.Attempt{ID: "attempt", WorkID: "worker", ObjectiveID: "objective", SpecificationRevision: 1, Recipe: recipe, Reservation: orchestrator.Reservation{Slots: 1}}, nil))
	require.True(t, execution.execute(context.Background(), "attempt"))
	state, err := store.State()
	require.NoError(t, err)
	require.Equal(t, orchestrator.AttemptFailed, state.Attempts["attempt"].Status)
	require.Equal(t, string(process.TerminationNotStarted), state.Attempts["attempt"].TerminationEvidence)
	require.False(t, state.Attempts["attempt"].Reservation.Held)
	admitControl(t, store, "worker", "replacement")
}

func TestManagedTimeoutOutcome(t *testing.T) {
	store := newControlStore(t)
	assignedControl(t, store)
	execution := managedExecution{store: store, logsDir: t.TempDir(), capacity: 1, runner: DirectRunner}
	recipe, err := execution.freeze("profile", 100*time.Millisecond, frozenCommand{Binary: "/bin/sh", Directory: t.TempDir(), Args: []string{"-c", `trap 'exit 0' TERM; while :; do :; done`}})
	require.NoError(t, err)
	require.NoError(t, execution.reserve(orchestrator.Attempt{ID: "attempt", WorkID: "worker", ObjectiveID: "objective", SpecificationRevision: 1, Recipe: recipe, Reservation: orchestrator.Reservation{Slots: 1}}, nil))
	stopped := execution.execute(context.Background(), "attempt")
	state, err := store.State()
	require.NoError(t, err)
	attempt := state.Attempts["attempt"]
	require.NotEqual(t, orchestrator.AttemptSucceeded, attempt.Status)
	if stopped {
		require.Equal(t, orchestrator.AttemptFailed, attempt.Status)
		require.False(t, attempt.Reservation.Held)
	} else {
		require.Equal(t, orchestrator.AttemptUncertain, attempt.Status)
		require.True(t, attempt.Reservation.Held)
	}
}

func TestManagedCompletionWithoutDurableStore(t *testing.T) {
	store := newControlStore(t)
	assignedControl(t, store)
	execution := managedExecution{store: store, logsDir: t.TempDir(), capacity: 1}
	recipe, err := execution.freeze("profile", time.Minute, frozenCommand{Binary: "worker", Directory: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, execution.reserve(orchestrator.Attempt{ID: "attempt", WorkID: "worker", ObjectiveID: "objective", SpecificationRevision: 1, Recipe: recipe, Reservation: orchestrator.Reservation{Slots: 1}}, nil))
	execution.runner = func(context.Context, RunnerParams) (process.Result, error) {
		require.NoError(t, store.Close())
		return process.Result{Termination: process.TerminationProcessGroupGone}, nil
	}
	require.False(t, execution.execute(context.Background(), "attempt"), "stopping without durable acknowledgement must retain shared capacity")
}

func TestManagedStaleCompletion(t *testing.T) {
	store := newControlStore(t)
	assignedControl(t, store)
	admitControl(t, store, "worker", "old")
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: "worker", AttemptID: "old", AttemptStatus: orchestrator.AttemptCancelled, At: time.Now(), Receipt: "confirmed"})
	admitControl(t, store, "worker", "new")
	execution := managedExecution{store: store, runner: func(context.Context, RunnerParams) (process.Result, error) {
		t.Fatal("stale attempt spawned")
		return process.Result{}, errors.New("unexpected")
	}}
	require.False(t, execution.execute(context.Background(), "old"))
	require.ErrorIs(t, execution.transition("worker", "old", orchestrator.CancelAttempt, "late cancel"), orchestrator.ErrStale)
	state, err := store.State()
	require.NoError(t, err)
	require.Equal(t, "new", state.Work["worker"].ActiveAttemptID)
	require.True(t, state.Attempts["new"].Reservation.Held)
}
