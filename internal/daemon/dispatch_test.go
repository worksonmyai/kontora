package daemon

import (
	"container/heap"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
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

func TestDispatchPoolsRunIndependently(t *testing.T) {
	for _, firstEpic := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker pool full", true: "supervisor pool full"}[firstEpic], func(t *testing.T) {
			h := newHarness(t)
			h.cfg.MaxConcurrentAgents = 1
			h.cfg.MaxConcurrentEpicSupervisors = 1
			calls := make(chan string, 3)
			d := h.newDaemon(h.cfg, WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
				calls <- p.TicketID
				<-ctx.Done()
				return process.Result{Termination: process.TerminationProcessGroupGone}, ctx.Err()
			}))
			write := func(id string, epic bool) string {
				md := h.taskMD(id, "todo", "one-stage")
				if epic {
					md = strings.Replace(h.epicMD(id, "todo"), "kind: epic\n", "kind: epic\nepic_execution: agent\n", 1)
				}
				return h.writeTicket(id+".md", md)
			}
			write("first", firstEpic)
			stop := startDaemon(t, d)
			defer func() {
				stop()
				require.Empty(t, d.sem, "confirmed worker shutdown releases worker capacity")
				require.Empty(t, d.epicSem, "confirmed supervisor shutdown releases supervisor capacity")
			}()
			select {
			case id := <-calls:
				require.Equal(t, "first", id)
			case <-time.After(5 * time.Second):
				t.Fatal("first pool did not start")
			}
			d.handleFileChanged(write("waiting", firstEpic))
			d.handleFileChanged(write("other", !firstEpic))
			select {
			case id := <-calls:
				require.Equal(t, "other", id)
			case <-time.After(5 * time.Second):
				t.Fatal("full pool blocked the other pool")
			}
			require.Len(t, d.sem, 1)
			require.Len(t, d.epicSem, 1)
			d.mu.Lock()
			waiting := d.queued["waiting"]
			d.mu.Unlock()
			require.True(t, waiting, "saturated pool must leave waiting work queued")
		})
	}
}

func TestEpicUncertainExecutionRetainsOnlySupervisorPool(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConcurrentAgents = 1
	h.cfg.MaxConcurrentEpicSupervisors = 1
	epicMD := func(id string) string {
		return strings.Replace(h.epicMD(id, "todo"), "kind: epic\n", "kind: epic\nepic_execution: agent\n", 1)
	}
	h.writeTicket("uncertain.md", epicMD("uncertain"))
	calls := make(chan string, 3)
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls <- p.TicketID
		result := process.Result{StartedAt: time.Now(), ExitedAt: time.Now()}
		if p.TicketID == "uncertain" {
			return result, process.ErrTerminationUncertain
		}
		result.Termination = process.TerminationProcessGroupGone
		return result, nil
	}))
	stop := startDaemon(t, d)
	defer stop()
	select {
	case id := <-calls:
		require.Equal(t, "uncertain", id)
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not start")
	}
	d.handleFileChanged(h.writeTicket("replacement.md", epicMD("replacement")))
	d.handleFileChanged(h.writeTicket("worker.md", h.taskMD("worker", "todo", "one-stage")))
	select {
	case id := <-calls:
		require.Equal(t, "worker", id, "uncertain supervisor must not admit another supervisor")
	case <-time.After(5 * time.Second):
		t.Fatal("retained supervisor reservation blocked worker capacity")
	}
	h.waitForStatus("worker.md", ticket.StatusDone, 5*time.Second)
	require.Eventually(t, func() bool { return len(d.sem) == 0 }, time.Second, time.Millisecond)
	require.Len(t, d.epicSem, 1)
	d.mu.Lock()
	_, running := d.running["uncertain"]
	_, live := d.liveRuns["uncertain"]
	branches := len(d.runningBranches)
	d.mu.Unlock()
	require.True(t, running)
	require.True(t, live)
	require.Equal(t, 1, branches)
	select {
	case id := <-calls:
		t.Fatalf("replacement admitted after uncertain supervisor termination: %s", id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDispatchPoolSelection(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		workersFull, supervisorsFull bool
		want                         string
	}{
		{name: "oldest across both pools", want: "epic-old"},
		{name: "workers full", workersFull: true, want: "epic-old"},
		{name: "supervisors full", supervisorsFull: true, want: "worker-old"},
		{name: "both full", workersFull: true, supervisorsFull: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			d.sem = make(chan struct{}, 1)
			d.epicSem = make(chan struct{}, 1)
			if tc.workersFull {
				d.sem <- struct{}{}
			}
			if tc.supervisorsFull {
				d.epicSem <- struct{}{}
			}
			for i, id := range []string{"worker-new", "epic-new", "worker-old", "epic-old"} {
				task := &ticket.Ticket{ID: id, Kontora: true, Status: ticket.StatusTodo}
				if i%2 == 1 {
					task.Kind, task.EpicExecution = ticket.KindEpic, "agent"
				}
				d.tickets[id] = &ticketState{ticket: task}
				d.queued[id] = true
				heap.Push(&d.queue, &queueItem{ticketID: id, created: time.Unix(int64(4-i), 0)})
			}
			index := d.readyOrdinaryJobLocked()
			if tc.want == "" {
				require.Equal(t, -1, index)
			} else {
				require.GreaterOrEqual(t, index, 0)
				require.Equal(t, tc.want, d.queue[index].ticketID)
			}
			require.Len(t, d.queue, 4, "full pools must leave tickets queued")
		})
	}
}

func TestDispatchRechecksQueueEligibilityAndMode(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	d.sem = make(chan struct{}, 1)
	d.epicSem = make(chan struct{}, 1)
	d.sem <- struct{}{}
	for _, id := range []string{"missing", "passive", "blocked", "paused", "changed"} {
		task := &ticket.Ticket{ID: id, Kontora: true, Status: ticket.StatusTodo}
		switch id {
		case "missing":
			// A deleted ticket can remain in the heap until the next dispatch.
		case "passive":
			task.Kind = ticket.KindEpic
		case "blocked":
			task.Deps = []string{"missing-dependency"}
		case "paused":
			task.Status = ticket.StatusPaused
		}
		if id != "missing" {
			d.tickets[id] = &ticketState{ticket: task}
		}
		d.queued[id] = true
		heap.Push(&d.queue, &queueItem{ticketID: id})
	}
	d.syncQueueDepthLocked()
	require.Equal(t, -1, d.readyOrdinaryJobLocked())
	require.Len(t, d.queue, 1)
	require.EqualValues(t, 1, d.queueDepth.Load())
	require.Equal(t, map[string]bool{"changed": true}, d.queued)

	changed := d.tickets["changed"].ticket
	changed.Kind, changed.EpicExecution = ticket.KindEpic, "agent"
	require.Equal(t, 0, d.readyOrdinaryJobLocked())
	require.Equal(t, d.epicSem, d.ticketPool(changed))
	workerCtx := context.WithValue(context.Background(), dispatchPoolKey{}, false)
	epicCtx := context.WithValue(context.Background(), dispatchPoolKey{}, true)
	require.False(t, dispatchPoolMatches(workerCtx, changed))
	require.True(t, dispatchPoolMatches(epicCtx, changed))
	changed.Kind, changed.EpicExecution = "", ""
	require.Equal(t, -1, d.readyOrdinaryJobLocked())
	require.False(t, dispatchPoolMatches(epicCtx, changed))
	require.True(t, dispatchPoolMatches(workerCtx, changed))
	require.True(t, dispatchPoolMatches(context.Background(), changed), "direct execution has no dispatcher reservation")
}

func TestDispatchPickupRequeuesChangedPool(t *testing.T) {
	for _, epic := range []bool{false, true} {
		t.Run(map[bool]string{false: "supervisor became worker", true: "worker became supervisor"}[epic], func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			task := &ticket.Ticket{ID: "changed", Kontora: true, Status: ticket.StatusTodo}
			if epic {
				task.Kind, task.EpicExecution = ticket.KindEpic, "agent"
			}
			d.tickets[task.ID] = &ticketState{ticket: task}
			ctx := context.WithValue(context.Background(), dispatchPoolKey{}, !epic)
			d.runTicket(ctx, task.ID)
			require.True(t, d.queued[task.ID], "pickup must retry admission in the current pool")
			require.Len(t, d.queue, 1)
			require.EqualValues(t, 1, d.queueDepth.Load())
			require.Empty(t, d.running, "a mismatched reservation must never claim or spawn work")
			require.Equal(t, ticket.StatusTodo, task.Status)
		})
	}
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
