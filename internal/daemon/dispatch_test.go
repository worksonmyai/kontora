package daemon

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestSharedDispatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ordinary  int
		uncertain bool
		slots     int
	}{
		{name: "capacity one"},
		{name: "mixed FIFO fairness", ordinary: 2},
		{name: "unconfirmed stop retains capacity", uncertain: true},
		{name: "multiple slots released", slots: 2},
		{name: "unconfirmed stop retains multiple slots", uncertain: true, slots: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			slots := max(1, tc.slots)
			h.cfg.MaxConcurrentAgents = slots
			d := h.newDaemon(h.cfg)
			d.managedDispatch = &managedDispatch{enabled: true}
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			wg.Go(func() { d.scheduler(ctx, &wg) })
			defer func() { cancel(); wg.Wait() }()
			time.Sleep(20 * time.Millisecond)
			require.Empty(t, d.sem, "an empty queue must not reserve capacity")
			d.mu.Lock()
			for i := range tc.ordinary {
				heap.Push(&d.queue, &queueItem{ticketID: string(rune('x' + i)), enqueuedAt: time.Now()})
			}
			d.mu.Unlock()
			admitted := make(chan string, 3)
			stopped := make(chan struct{})
			makeJob := func(objective, id string) *dispatchJob {
				return &dispatchJob{objectiveID: objective, attemptID: id, slots: slots, ready: func() bool { return true }, reserve: func() error { return nil }, run: func(context.Context) bool {
					admitted <- id
					if id == "planner" && tc.uncertain {
						close(stopped)
						return false
					}
					return true
				}}
			}
			require.NoError(t, d.queueManagedJob(makeJob("a", "planner")))
			require.NoError(t, d.queueManagedJob(makeJob("a", "worker")))
			select {
			case id := <-admitted:
				require.Equal(t, "planner", id)
			case <-time.After(3 * time.Second):
				t.Fatal("planner was starved")
			}
			if tc.uncertain {
				<-stopped
				select {
				case id := <-admitted:
					t.Fatalf("replacement admitted after uncertain stop: %s", id)
				case <-time.After(50 * time.Millisecond):
				}
				require.Len(t, d.sem, slots)
			} else {
				select {
				case id := <-admitted:
					require.Equal(t, "worker", id)
				case <-time.After(3 * time.Second):
					t.Fatal("worker was starved")
				}
				require.Eventually(t, func() bool { return len(d.sem) == 0 }, time.Second, time.Millisecond)
			}
		})
	}
}

func TestOrdinaryUncertainExecutionRetainsReservations(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConcurrentAgents = 1
	h.writeTicket("ordinary.md", h.taskMD("ordinary", "todo", "one-stage"))
	returned := make(chan struct{})
	d := h.newDaemon(h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
		close(returned)
		return process.Result{StartedAt: time.Now(), ExitedAt: time.Now()}, process.ErrTerminationUncertain
	}))
	d.managedDispatch = &managedDispatch{enabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary runner did not return")
	}
	admitted := make(chan struct{}, 1)
	require.NoError(t, d.queueManagedJob(&dispatchJob{objectiveID: "objective", attemptID: "replacement", ready: func() bool { return true }, reserve: func() error { return nil }, run: func(context.Context) bool {
		admitted <- struct{}{}
		return true
	}}))
	select {
	case <-admitted:
		t.Fatal("replacement admitted after uncertain ordinary termination")
	case <-time.After(100 * time.Millisecond):
	}
	require.Len(t, d.sem, 1)
	d.mu.Lock()
	_, running := d.running["ordinary"]
	branches := len(d.runningBranches)
	_, live := d.liveRuns["ordinary"]
	d.mu.Unlock()
	require.True(t, running)
	require.Equal(t, 1, branches)
	require.True(t, live)
}

func TestSharedDispatchMultiSlot(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConcurrentAgents = 3
	for _, id := range []string{"ordinary-a", "ordinary-b"} {
		h.writeTicket(id+".md", h.taskMD(id, "todo", "one-stage"))
	}
	ordinaryStarted := make(chan string, 2)
	ordinaryRelease := map[string]chan struct{}{"ordinary-a": make(chan struct{}), "ordinary-b": make(chan struct{})}
	d := h.newDaemon(h.cfg, WithRunner(func(ctx context.Context, params RunnerParams) (process.Result, error) {
		started := time.Now()
		ordinaryStarted <- params.TicketID
		select {
		case <-ordinaryRelease[params.TicketID]:
		case <-ctx.Done():
		}
		return process.Result{StartedAt: started, ExitedAt: time.Now(), Termination: process.TerminationProcessGroupGone}, nil
	}))
	d.managedDispatch = &managedDispatch{enabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	for range 2 {
		select {
		case <-ordinaryStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("ordinary worker did not start")
		}
	}
	store := newControlStore(t)
	assignedControl(t, store)
	managedStarted := make(chan struct{}, 1)
	managedRelease := make(chan struct{})
	execution := managedExecution{store: store, logsDir: t.TempDir(), capacity: 3, runner: func(ctx context.Context, _ RunnerParams) (process.Result, error) {
		managedStarted <- struct{}{}
		select {
		case <-managedRelease:
		case <-ctx.Done():
		}
		return process.Result{Termination: process.TerminationProcessGroupGone}, nil
	}}
	recipe, err := execution.freeze("profile", time.Minute, frozenCommand{Binary: "worker", Directory: t.TempDir()})
	require.NoError(t, err)
	attempt := orchestrator.Attempt{ID: "managed", WorkID: "worker", ObjectiveID: "objective", SpecificationRevision: 1, Recipe: recipe, Reservation: orchestrator.Reservation{Slots: 2}}
	require.NoError(t, d.queueManagedJob(&dispatchJob{objectiveID: "objective", attemptID: "managed", slots: attempt.Reservation.Slots, ready: func() bool { return true }, reserve: func() error { return execution.reserve(attempt, nil) }, run: func(ctx context.Context) bool { return execution.execute(ctx, "managed") }}))
	select {
	case <-managedStarted:
		t.Fatal("two-slot attempt admitted alongside two ordinary workers at capacity three")
	case <-time.After(100 * time.Millisecond):
	}
	state, err := store.State()
	require.NoError(t, err)
	require.Empty(t, state.Attempts)
	close(ordinaryRelease["ordinary-a"])
	select {
	case <-managedStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("two-slot attempt did not start after ordinary capacity was released")
	}
	require.Len(t, d.sem, 3)
	close(managedRelease)
	require.Eventually(t, func() bool { return len(d.sem) == 1 }, 5*time.Second, time.Millisecond)
	state, err = store.State()
	require.NoError(t, err)
	require.False(t, state.Attempts["managed"].Reservation.Held)
	close(ordinaryRelease["ordinary-b"])
	require.Eventually(t, func() bool { return len(d.sem) == 0 }, 5*time.Second, time.Millisecond)
}

func TestManagedDispatchSlotRefusal(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConcurrentAgents = 2
	d := h.newDaemon(h.cfg)
	d.managedDispatch = &managedDispatch{enabled: true}
	job := &dispatchJob{objectiveID: "objective", attemptID: "too-large", slots: 3, ready: func() bool { return true }, reserve: func() error { return nil }, run: func(context.Context) bool { return true }}
	require.Error(t, d.queueManagedJob(job))
	job.slots = 2
	job.attemptID = "refused"
	job.reserve = func() error { return errors.New("durable admission refused") }
	job.run = func(context.Context) bool { t.Error("refused job ran"); return true }
	require.NoError(t, d.queueManagedJob(job))
	admitted := make(chan struct{}, 1)
	require.NoError(t, d.queueManagedJob(&dispatchJob{objectiveID: "objective", attemptID: "next", slots: 2, ready: func() bool { return true }, reserve: func() error { return nil }, run: func(context.Context) bool {
		admitted <- struct{}{}
		return true
	}}))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.dispatch(ctx, &wg) })
	defer func() { cancel(); wg.Wait() }()
	select {
	case <-admitted:
	case <-time.After(5 * time.Second):
		t.Fatal("refused admission leaked slots")
	}
	require.Eventually(t, func() bool { return len(d.sem) == 0 }, time.Second, time.Millisecond)
}

func TestManagedDispatchSelection(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	require.ErrorContains(t, d.queueManagedJob(&dispatchJob{}), "disabled")
	d.managedDispatch = &managedDispatch{enabled: true, lastObjective: "a", active: map[string]*dispatchJob{"old": {planner: true}}}
	d.managedDispatch.queue = []*dispatchJob{
		{objectiveID: "b", planner: true, ready: func() bool { return true }},
		{objectiveID: "a", ready: func() bool { return true }},
		{objectiveID: "b", ready: func() bool { return true }},
	}
	require.Equal(t, 2, d.readyManagedJobLocked())
	d.managedDispatch.queue[2].ready = func() bool { return false }
	require.Equal(t, 1, d.readyManagedJobLocked())
}
