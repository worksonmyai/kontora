package daemon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func dispatchFindingsControlGraph() config.PipelineDefinition {
	return config.PipelineDefinition{Graph: &config.WorkflowGraph{
		Version: 2, Entry: "approval",
		Nodes: []config.WorkflowNode{{ID: "approval", Type: "wait", Mode: "human"}, {ID: "done", Type: "end", Status: "done"}},
		Edges: []config.WorkflowEdge{{From: config.WorkflowPort{Node: "approval", Port: "released"}, To: "done"}},
	}}
}

func TestDispatchFindingsControlReservationRevalidatedAtClaim(t *testing.T) {
	for _, change := range []string{"agent entry reload", "ticket pipeline changed", "unchanged definition reload", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.MaxConcurrentAgents = 1
			h.cfg.Pipelines["control"] = dispatchFindingsControlGraph()
			h.cfg = loadWorkflowTestConfig(t, h.cfg)
			var calls atomic.Int32
			usedAtRun := make(chan int, 1)
			var d *Daemon
			d = h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls.Add(1)
				usedAtRun <- poolUsed(d, d.sem)
				now := time.Now()
				var err error
				if p.Env["KONTORA_RESULT_VERSION"] != "" {
					err = publishWorkflowWaitTestResult(p, session.Result{Outcome: "complete", Summary: "Work completed."})
				}
				return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, err
			}))
			path := h.writeTicket("tst-control.md", h.taskMD("tst-control", "todo", "control"))
			task := h.readTask("tst-control.md")
			d.tickets[task.ID] = newTicketState(task, path)
			d.sem.used = 1
			require.True(t, d.workflowControlOnlyLocked(task))
			reservation := dispatchReservation{epic: false, controlConfig: d.config(), daemon: d}
			ctx := context.WithValue(context.Background(), dispatchPoolKey{}, reservation)
			ctx = context.WithValue(ctx, dispatchExplicitKey{}, true)

			switch change {
			case "agent entry reload":
				next := *h.cfg
				next.Pipelines = map[string]config.PipelineDefinition{"control": {Graph: &config.WorkflowGraph{
					Version: 2, Entry: "work",
					Nodes: []config.WorkflowNode{{ID: "work", Type: "agent", Stage: "step1", Agent: "agent1"}, {ID: "done", Type: "end", Status: "done"}, {ID: "stop", Type: "end", Status: "paused"}},
					Edges: []config.WorkflowEdge{{From: config.WorkflowPort{Node: "work", Port: "complete"}, To: "done"}, {From: config.WorkflowPort{Node: "work", Port: "failed"}, To: "stop"}},
				}}}
				d.cfg.Store(loadWorkflowTestConfig(t, &next))
			case "ticket pipeline changed":
				h.writeTicket("tst-control.md", h.taskMD("tst-control", "todo", "one-stage"))
			case "unchanged definition reload":
				d.cfg.Store(loadWorkflowTestConfig(t, h.cfg))
			}

			d.runTicket(ctx, task.ID)
			require.Zero(t, calls.Load(), "a zero-slot pickup must not launch an agent after admission changes")
			require.Equal(t, 1, poolUsed(d, d.sem))
			if change == "unchanged" {
				require.Equal(t, ticket.StatusPaused, h.readTask("tst-control.md").Status)
				require.NotNil(t, h.readTask("tst-control.md").GraphState.Wait)
				return
			}
			require.True(t, d.queued[task.ID], "claim must return the ticket for fresh admission")
			require.Empty(t, d.running)
			require.Equal(t, ticket.StatusTodo, h.readTask("tst-control.md").Status)

			d.mu.Lock()
			d.sem.used--
			d.mu.Unlock()
			runCtx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			wg.Go(func() { d.dispatch(runCtx, &wg) })
			t.Cleanup(func() { cancel(); wg.Wait() })
			want := ticket.StatusDone
			if change == "unchanged definition reload" {
				want = ticket.StatusPaused
			}
			h.waitForStatus("tst-control.md", want, 5*time.Second)
			require.Eventually(t, func() bool { return poolUsed(d, d.sem) == 0 }, time.Second, time.Millisecond)
			if want == ticket.StatusDone {
				require.EqualValues(t, 1, calls.Load())
				require.Equal(t, 1, <-usedAtRun, "fresh admission must reserve the agent's slot")
			}
		})
	}
}

func TestDispatchFindingsControlAfterPoolShrink(t *testing.T) {
	for _, entry := range []string{"human", "end"} {
		t.Run(entry, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.MaxConcurrentAgents = 2
			h.cfg.Pipelines["control"] = dispatchFindingsControlGraph()
			if entry == "end" {
				h.cfg.Pipelines["control"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{
					Version: 2, Entry: "done", Nodes: []config.WorkflowNode{{ID: "done", Type: "end", Status: "done"}},
				}}
			}
			h.cfg = loadWorkflowTestConfig(t, h.cfg)
			started := make(chan string, 3)
			d := h.newDaemon(h.cfg, WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
				started <- p.TicketID
				now := time.Now()
				<-ctx.Done()
				return process.Result{StartedAt: now, ExitedAt: time.Now(), Termination: process.TerminationProcessGroupGone}, nil
			}))
			for _, id := range []string{"tst-worker-a", "tst-worker-b"} {
				h.writeTicket(id+".md", h.taskMD(id, "todo", "one-stage"))
			}
			startWorkflowWaitTestDaemon(t, d)
			for range 2 {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("ordinary workers did not saturate the pool")
				}
			}
			d.mu.Lock()
			next := *d.config()
			next.MaxConcurrentAgents = 1
			d.resizePoolsLocked(next.MaxConcurrentAgents, next.MaxConcurrentEpicSupervisors)
			d.cfg.Store(&next)
			available := d.availablePoolSlotsLocked(d.sem)
			d.mu.Unlock()
			require.Equal(t, -1, available)
			h.writeTicket("tst-control.md", h.taskMD("tst-control", "todo", "control"))
			want := ticket.StatusPaused
			if entry == "end" {
				want = ticket.StatusDone
			}
			got := h.waitForStatus("tst-control.md", want, 5*time.Second)
			if entry == "human" {
				require.NotNil(t, got.GraphState.Wait)
				require.Equal(t, "human", got.GraphState.Wait.Kind)
			}
			require.Equal(t, 2, poolUsed(d, d.sem), "control admission must not release either running worker")
			select {
			case id := <-started:
				t.Fatalf("zero-slot control launched a runner: %s", id)
			default:
			}
		})
	}
}

func TestDispatchFindingsProbeAutomationFairness(t *testing.T) {
	for _, primary := range []string{"none", "ordinary", "managed", "both"} {
		t.Run(primary, func(t *testing.T) {
			h, d, clock, _ := newWorkflowWaitTestFixture(t, "check")
			path := h.writeTicket("tst-second.md", h.taskMD("tst-second", "todo", "wait-graph"))
			d.tickets["tst-second"] = newTicketState(h.readTask("tst-second.md"), path)
			d.runTicket(context.Background(), "tst-second")
			require.NotNil(t, h.readTask("tst-second.md").GraphState.Wait)
			d.reconcileWorkflowWaits(context.Background())
			require.Len(t, d.workflowProbeJobs, 2)

			admitted := make(chan string, 1)
			advance := make(chan struct{})
			turn := func(ctx context.Context, kind string) {
				select {
				case admitted <- kind:
				case <-ctx.Done():
					return
				}
				select {
				case <-advance:
				case <-ctx.Done():
				}
			}
			d.runner = func(ctx context.Context, p RunnerParams) (process.Result, error) {
				kind := "ordinary"
				if p.Binary == "/bin/sh" {
					kind = "probe"
					// Each check outlasts its polling interval. The other check is
					// due again before this check releases its execution slot.
					clock.Advance(6 * time.Minute)
					d.reconcileWorkflowWaits(ctx)
				}
				turn(ctx, kind)
				exit := 0
				if kind == "probe" {
					exit = 1
				}
				return workflowWaitTestResult(clock, exit), nil
			}
			// Keep every competing class backlogged throughout the observation;
			// draining automation first must not count as probe fairness.
			const turns = 12
			for range turns {
				d.automationJobs = append(d.automationJobs, &automationDispatchJob{key: newSessionID(), run: func(ctx context.Context) bool {
					turn(ctx, "automation")
					return true
				}})
			}
			want := map[string]bool{"probe": true, "automation": true}
			if primary == "ordinary" || primary == "both" {
				for i := range turns {
					id := fmt.Sprintf("tst-ordinary-%d", i)
					path := h.writeTicket(id+".md", h.taskMD(id, "todo", "one-stage"))
					d.tickets[id] = newTicketState(h.readTask(id+".md"), path)
					d.enqueueLocked(d.tickets[id].ticket, true)
				}
				want["ordinary"] = true
			}
			if primary == "managed" || primary == "both" {
				d.managedDispatch = &managedDispatch{enabled: true}
				for i := range turns {
					require.NoError(t, d.queueManagedJob(&dispatchJob{objectiveID: "objective", attemptID: fmt.Sprintf("attempt-%d", i), ready: func() bool { return true }, reserve: func() error { return nil }, run: func(ctx context.Context) bool {
						turn(ctx, "managed")
						return true
					}}))
				}
				want["managed"] = true
			}
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			wg.Go(func() { d.dispatch(ctx, &wg) })
			t.Cleanup(func() { cancel(); wg.Wait() })
			seen := make(map[string]int)
			var order []string
			for range turns {
				select {
				case kind := <-admitted:
					seen[kind]++
					order = append(order, kind)
				case <-time.After(5 * time.Second):
					t.Fatalf("dispatch stopped making progress: %v", order)
				}
				advance <- struct{}{}
			}
			for kind := range want {
				require.GreaterOrEqual(t, seen[kind], 2, "%s must retain progress under sustained contention: %v", kind, order)
			}
		})
	}
}

func TestDispatchFindingsQueuedProbeAtCapacity(t *testing.T) {
	h, d, clock, path := newWorkflowWaitTestFixture(t, "check")
	before := h.readTask("tst-wait.md")
	state := before.GraphState.Clone()
	state.Wait.Deadline = clock.Now().Add(3 * time.Minute)
	require.NoError(t, before.SetField("graph_state", state))
	saveWorkflowWaitTestTicket(t, path, before)
	d.tickets[before.ID] = newTicketState(before, path)
	// Represent an unrelated running worker, which must not block deadlines.
	d.sem.used = d.sem.limit
	var calls atomic.Int32
	d.runner = func(context.Context, RunnerParams) (process.Result, error) {
		calls.Add(1)
		return workflowWaitTestResult(clock, 1), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.dispatch(ctx, &wg) })
	t.Cleanup(func() { cancel(); wg.Wait() })

	for range 3 {
		d.reconcileWorkflowWaits(ctx)
		d.mu.Lock()
		queued := len(d.workflowProbeJobs)
		d.mu.Unlock()
		require.Equal(t, 1, queued, "reconciliation must deduplicate the overdue probe")
		require.Equal(t, time.Minute, d.nextWorkflowWait(clock.Now()), "a queued NextPoll must not force 10ms reconciliation")
		require.Zero(t, h.readTask("tst-wait.md").GraphState.Wait.PollCount)
		clock.Advance(time.Minute)
	}
	d.reconcileWorkflowWaits(ctx)
	got := h.readTask("tst-wait.md")
	require.Equal(t, "ready", got.GraphState.Wait.Phase)
	require.Equal(t, "deadline", got.GraphState.Wake.Reason)
	require.Equal(t, time.Minute, d.nextWorkflowWait(clock.Now()))
	require.Zero(t, calls.Load(), "neither probe nor continuation may run without capacity")
	require.Equal(t, d.sem.limit, poolUsed(d, d.sem))
}
