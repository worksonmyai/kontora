package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Only the wait scheduler uses this clock. Daemon startup, watchers, and test
// timeouts still use real time, so advancing a wait never masks a stuck Run.
type workflowWaitTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*workflowWaitTestTimer]time.Time
}

type workflowWaitTestTimer struct {
	clock *workflowWaitTestClock
	ch    chan time.Time
}

func newWorkflowWaitTestClock() *workflowWaitTestClock {
	return &workflowWaitTestClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), timers: make(map[*workflowWaitTestTimer]time.Time)}
}
func (c *workflowWaitTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *workflowWaitTestClock) NewTimer(delay time.Duration) WorkflowWaitTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &workflowWaitTestTimer{clock: c, ch: make(chan time.Time, 1)}
	if delay <= 0 {
		t.ch <- c.now
	} else {
		c.timers[t] = c.now.Add(delay)
	}
	return t
}
func (t *workflowWaitTestTimer) C() <-chan time.Time { return t.ch }
func (t *workflowWaitTestTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	_, active := t.clock.timers[t]
	delete(t.clock.timers, t)
	return active
}
func (c *workflowWaitTestClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
	for timer, at := range c.timers {
		if !c.now.Before(at) {
			timer.ch <- c.now
			delete(c.timers, timer)
		}
	}
}

func workflowWaitTestConfig(t *testing.T, h *testHarness) *config.Config {
	t.Helper()
	h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
	h.cfg.MaxConcurrentAgents = 1
	h.cfg.Pipelines["wait-graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{
		Version: 2, Entry: "work",
		Nodes: []config.WorkflowNode{
			{ID: "work", Type: "agent", Stage: "step1", Agent: "agent1", Wake: &config.WorkflowWake{Max: 3, CheckTimeout: config.Duration{Duration: 23 * time.Second}}},
			{ID: "accept", Type: "end", Status: "human_review"},
			{ID: "stop", Type: "end", Status: "paused"},
		},
		Edges: []config.WorkflowEdge{
			{From: config.WorkflowPort{Node: "work", Port: "complete"}, To: "accept"},
			{From: config.WorkflowPort{Node: "work", Port: "failed"}, To: "stop"},
		},
	}}
	h.cfg = loadWorkflowTestConfig(t, h.cfg)
	return h.cfg
}

func workflowWaitTestResult(clock *workflowWaitTestClock, exit int) process.Result {
	now := clock.Now()
	return process.Result{ExitCode: exit, StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}
}

func publishWorkflowWaitTestResult(p RunnerParams, report session.Result) error {
	inv, err := session.InvocationFromEnv(func(key string) string { return p.Env[key] })
	if err != nil {
		return err
	}
	_, err = session.PublishResult(inv.ResultPath, report, inv.Version)
	return err
}

func writeWorkflowWaitTestLog(p RunnerParams, text string) error {
	if err := os.MkdirAll(filepath.Dir(p.LogFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.LogFile, []byte(text), 0o600)
}

func startWorkflowWaitTestDaemon(t *testing.T, d *Daemon) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("daemon Run did not stop after cancellation")
		}
	})
	t.Cleanup(stop)
	return stop
}

func awaitWorkflowWaitTestTicket(t *testing.T, h *testHarness, id string, predicate func(*ticket.Ticket) bool) *ticket.Ticket {
	t.Helper()
	var got *ticket.Ticket
	require.Eventually(t, func() bool {
		parsed, err := ticket.ParseFile(filepath.Join(h.tasksDir, id+".md"))
		if err != nil {
			return false
		}
		got = parsed
		return predicate(parsed)
	}, 10*time.Second, 10*time.Millisecond, "ticket %s never reached expected wait state", id)
	return got
}

func requireWorkflowWaitTestIdle(t *testing.T, d *Daemon) {
	t.Helper()
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.sem.used == 0 && len(d.running) == 0 && len(d.workflowProbes) == 0
	}, 5*time.Second, 10*time.Millisecond, "idle waits must not retain an execution slot")
}

// Both acceptance paths run the real lifecycle and dispatcher, with no real
// agent or probe processes. Capacity is one, including condition probes.
func TestWorkflowWaitReleasesCapacity(t *testing.T) {
	for _, kind := range []string{"timer", "check"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			cfg := workflowWaitTestConfig(t, h)
			cfg.Notifications.DefaultStatuses = []string{"paused", "waiting"}
			clock := newWorkflowWaitTestClock()
			requested := clock.Now()
			request := &session.WaitRequest{Kind: "timer", Until: requested.Add(time.Hour)}
			if kind == "check" {
				request = &session.WaitRequest{Kind: "check", Command: "test -f READY", Poll: 5 * time.Minute, Deadline: requested.Add(24 * time.Hour)}
			}
			var agentCalls, probeCalls atomic.Int32
			var mu sync.Mutex
			var agents, probes []RunnerParams
			var summaries summarySpawns
			d := h.newDaemon(cfg, WithWorkflowWaitClock(clock), WithFinalSummarySpawner(summaries.spawner("wait summary")), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				if p.Binary == "/bin/sh" {
					mu.Lock()
					probes = append(probes, p)
					mu.Unlock()
					n := probeCalls.Add(1)
					if n == 1 {
						// Poll intervals are measured from completion, not launch.
						clock.Advance(2 * time.Minute)
						return workflowWaitTestResult(clock, 1), writeWorkflowWaitTestLog(p, "still pending")
					}
					return workflowWaitTestResult(clock, 0), writeWorkflowWaitTestLog(p, "deployment ready: ✓")
				}
				if p.TicketID != "tst-wait" {
					return workflowWaitTestResult(clock, 0), nil // legacy ticket
				}
				mu.Lock()
				agents = append(agents, p)
				mu.Unlock()
				report := session.Result{Outcome: "complete", Summary: "continuation finished"}
				if agentCalls.Add(1) == 1 {
					report = session.Result{Outcome: "waiting", Summary: "external work started", NextStep: "Read evidence and report the deployment.", Wait: request}
				}
				return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, report)
			}))
			h.writeTicket("tst-wait.md", h.taskMD("tst-wait", "todo", "wait-graph"))
			stop := startWorkflowWaitTestDaemon(t, d)
			parked := awaitWorkflowWaitTestTicket(t, h, "tst-wait", func(tkt *ticket.Ticket) bool {
				return tkt.Status == ticket.StatusPaused && tkt.GraphState != nil && tkt.GraphState.Wait != nil && (kind != "check" || tkt.GraphState.Wait.PollCount == 1)
			})
			requireWorkflowWaitTestIdle(t, d)
			require.Nil(t, parked.Notify.Statuses)
			require.Equal(t, 2, parked.GraphState.Version)
			require.Len(t, parked.History, 1)
			require.Equal(t, "waiting", string(parked.History[0].RunOutcome))
			require.Equal(t, "Read evidence and report the deployment.", parked.History[0].NextStep)
			require.NotNil(t, parked.History[0].Wait)
			require.Equal(t, parked.GraphState.Wait.ID, parked.History[0].Wait.ID)
			require.Equal(t, "work", parked.Stage)
			require.Nil(t, parked.GraphState.LastResult, "suspension must not follow a completion edge")
			require.Empty(t, parked.GraphState.Transitions)
			require.Empty(t, parked.InvocationID)
			require.NoFileExists(t, executionPath(cfg, parked.ID))
			require.Empty(t, summaries.all(), "suspension must not run a final-summary agent")
			w := parked.GraphState.Wait
			require.Equal(t, requested, w.RequestedAt)
			require.DirExists(t, w.Worktree)
			d.mu.Lock()
			holder, reserved := d.branchHolderLocked(w.Repository + "\x00" + w.Branch)
			d.mu.Unlock()
			require.True(t, reserved, "parking must retain a branch-only reservation")
			require.Equal(t, parked.ID, holder)
			if kind == "check" {
				require.Equal(t, requested.Add(7*time.Minute), w.NextPoll)
				require.Equal(t, "still pending", w.Output)
			}

			// This ticket cannot run unless the first wait released the only slot.
			h.writeTicket("tst-other.md", h.taskMD("tst-other", "todo", "one-stage"))
			h.waitForStatus("tst-other.md", ticket.StatusDone, 10*time.Second)
			requireWorkflowWaitTestIdle(t, d)
			require.Equal(t, int32(1), agentCalls.Load())
			if kind == "check" {
				clock.Advance(5*time.Minute - time.Nanosecond)
				d.reconcileWorkflowWaits(context.Background())
				require.Equal(t, int32(1), probeCalls.Load(), "pending must not immediately replay a missed poll")
				clock.Advance(time.Nanosecond)
			} else {
				clock.Advance(time.Hour)
			}
			d.signalWorkflowWait()
			got := h.waitForStatus("tst-wait.md", ticket.StatusHumanReview, 10*time.Second)
			h.waitForFinalSummary("tst-wait.md", "wait summary", 5*time.Second)
			requireWorkflowWaitTestIdle(t, d)
			for range 3 {
				d.reconcileWorkflowWaits(context.Background())
			}
			stop()
			require.Nil(t, got.Notify.Statuses)
			require.Equal(t, int32(2), agentCalls.Load(), "one wake must start exactly one continuation")
			require.Len(t, got.History, 2, "probes must not fabricate agent history")
			require.Equal(t, parked.GraphState.ExecutionID, got.GraphState.ExecutionID)
			require.Equal(t, parked.History[0].NodeVisitID, got.History[1].NodeVisitID)
			require.NotEqual(t, got.History[0].InvocationID, got.History[1].InvocationID)
			require.Equal(t, 0, got.Attempt, "a wake is not a failure retry")
			require.Equal(t, 1, got.GraphState.WakeCount)
			require.True(t, got.History[1].Continuation, "history must distinguish an intentional continuation from a retry")
			require.Len(t, got.GraphState.WaitHistory, 1)
			require.Equal(t, w.ID, got.GraphState.WaitHistory[0].ID)
			require.Empty(t, got.GraphState.LoopCounts)
			require.Len(t, summaries.all(), 1)
			reason := "timer"
			if kind == "check" {
				reason = "ready"
			}
			require.NotNil(t, got.History[1].Wake)
			require.Equal(t, reason, got.History[1].Wake.Reason)
			require.Equal(t, w.Next, got.History[1].Wake.Next)
			require.Equal(t, 1, got.History[1].Wake.Count)
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, agents, 2)
			require.Equal(t, "3", agents[0].Env[session.ResultVersionEnvVar])
			require.NotEqual(t, agents[0].LogFile, agents[1].LogFile)
			require.NotContains(t, strings.Join(agents[0].Args, "\n"), "Workflow wake handoff")
			prompt := strings.Join(agents[1].Args, "\n")
			require.Contains(t, prompt, "Workflow wake handoff")
			require.Contains(t, prompt, "Wake count: 1")
			require.Contains(t, prompt, w.ID)
			require.Contains(t, prompt, reason)
			require.Contains(t, prompt, w.Next, "existing stage prompts must receive the automatic wake handoff")
			if kind == "check" {
				require.Len(t, probes, 2)
				require.NotEqual(t, probes[0].AttemptID, probes[1].AttemptID)
				require.NotEqual(t, probes[0].LogFile, probes[1].LogFile)
				require.Contains(t, prompt, "deployment ready: ✓")
				for _, probe := range probes {
					require.Equal(t, []string{"-c", request.Command}, probe.Args)
					require.Equal(t, 23*time.Second, probe.Timeout)
					require.Equal(t, w.Worktree, probe.Dir)
					require.True(t, probe.RequireTermination)
					require.False(t, probe.Interactive)
					require.Empty(t, probe.Env[session.InvocationIDEnvVar])
				}
			} else {
				require.Empty(t, probes)
			}
		})
	}
}

func TestWorkflowWaitSuspensionRequiresPermissionAndSuccessfulHooks(t *testing.T) {
	for _, mode := range []string{"required hook fails", "unsupported wait"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			cfg := workflowWaitTestConfig(t, h)
			clock := newWorkflowWaitTestClock()
			if mode == "required hook fails" {
				cfg.Hooks = config.Hooks{config.HookStageEnd: {{Run: "exit 7", OnFailure: "pause"}}}
			} else {
				cfg.Pipelines["wait-graph"].Graph.Nodes[0].Wake = nil
			}
			var calls atomic.Int32
			d := h.newDaemon(cfg, WithWorkflowWaitClock(clock), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls.Add(1)
				// Bypass the publisher's validation to test daemon rejection of an
				// unsupported runner artifact, not merely client-side validation.
				payload := fmt.Sprintf(`{"outcome":"waiting","summary":"started","next_step":"resume","wait":{"kind":"timer","until":%q}}`, clock.Now().Add(time.Hour).Format(time.RFC3339))
				return workflowWaitTestResult(clock, 0), os.WriteFile(p.Env[session.ResultFileEnvVar], []byte(payload), 0o600)
			}))
			path := h.writeTicket("tst-refuse.md", h.taskMD("tst-refuse", "todo", "wait-graph"))
			d.tickets["tst-refuse"] = newTicketState(h.readTask("tst-refuse.md"), path)
			d.runTicket(context.Background(), "tst-refuse")
			got := h.readTask("tst-refuse.md")
			require.Equal(t, ticket.StatusPaused, got.Status)
			require.Nil(t, got.GraphState.Wait)
			require.False(t, got.GraphState.Continuation)
			require.Empty(t, got.GraphState.Transitions)
			require.Len(t, got.History, 1)
			if mode == "required hook fails" {
				require.Equal(t, "failed", string(got.History[0].RunOutcome))
				require.Contains(t, got.LastError, "7")
			} else {
				require.Equal(t, "protocol_error", string(got.History[0].RunOutcome))
			}
			clock.Advance(2 * time.Hour)
			d.reconcileWorkflowWaits(context.Background())
			require.Equal(t, int32(1), calls.Load())
			require.Empty(t, d.queuedIDs())
		})
	}
}

// A direct initial invocation gives the recovery tests a real persisted wait,
// real invocation history, and a real reserved Git worktree without running a
// dispatcher. Subsequent probes/reconciliation can then stop at exact boundaries.
func newWorkflowWaitTestFixture(t *testing.T, kind string) (*testHarness, *Daemon, *workflowWaitTestClock, string) {
	t.Helper()
	h := newHarness(t)
	cfg := workflowWaitTestConfig(t, h)
	clock := newWorkflowWaitTestClock()
	request := &session.WaitRequest{Kind: "timer", Until: clock.Now().Add(time.Hour)}
	if kind == "check" {
		request = &session.WaitRequest{Kind: "check", Command: "test -f READY", Poll: 5 * time.Minute, Deadline: clock.Now().Add(time.Hour)}
	}
	d := h.newDaemon(cfg, WithWorkflowWaitClock(clock), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "waiting", Summary: "started external work", NextStep: "Read saved evidence and continue.", Wait: request})
	}))
	path := h.writeTicket("tst-wait.md", h.taskMD("tst-wait", "todo", "wait-graph"))
	d.tickets["tst-wait"] = newTicketState(h.readTask("tst-wait.md"), path)
	d.runTicket(context.Background(), "tst-wait")
	got := h.readTask("tst-wait.md")
	require.Equal(t, ticket.StatusPaused, got.Status)
	require.NotNil(t, got.GraphState.Wait)
	require.Empty(t, got.InvocationID)
	require.NoFileExists(t, executionPath(cfg, got.ID))
	return h, d, clock, path
}

func TestWorkflowWaitProbeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		exit         int
		err          error
		interruption error
		deadline     bool
	}{
		{name: "pending", exit: 1},
		{name: "ready", reason: "ready"},
		{name: "command error", exit: 7, reason: "error"},
		{name: "runner failure", err: fmt.Errorf("launch failed"), reason: "error"},
		{name: "confirmed timeout", interruption: context.DeadlineExceeded, reason: "error"},
		{name: "deadline wins ready result", reason: "deadline", deadline: true},
		{name: "deadline wins pending result", exit: 1, reason: "deadline", deadline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d, clock, _ := newWorkflowWaitTestFixture(t, "check")
			before := h.readTask("tst-wait.md")
			d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
				if tc.deadline {
					clock.Advance(time.Hour)
				} else {
					clock.Advance(2 * time.Minute)
				}
				require.NoError(t, writeWorkflowWaitTestLog(p, "probe diagnostic ✓"))
				result := workflowWaitTestResult(clock, tc.exit)
				result.Interruption = tc.interruption
				return result, tc.err
			}
			for range 3 {
				d.reconcileWorkflowWaits(context.Background())
			}
			require.Len(t, d.workflowProbeJobs, 1, "only one queued probe per wait")
			job := d.workflowProbeJobs[0]
			d.workflowProbeJobs = nil
			require.True(t, d.runWorkflowProbe(context.Background(), job))
			got := h.readTask("tst-wait.md")
			require.Len(t, got.History, 1)
			require.Equal(t, before.History, got.History)
			require.Equal(t, 1, got.GraphState.Wait.PollCount)
			require.Equal(t, "probe diagnostic ✓", got.GraphState.Wait.Output)
			require.NoFileExists(t, workflowProbePath(h.cfg, got.ID))
			require.Empty(t, got.InvocationID)
			if tc.reason == "" {
				require.Equal(t, ticket.StatusPaused, got.Status)
				require.Equal(t, "pending", got.GraphState.Wait.Phase)
				require.Equal(t, clock.Now().Add(5*time.Minute), got.GraphState.Wait.NextPoll)
				require.Nil(t, got.GraphState.Wake)
			} else {
				require.Equal(t, ticket.StatusTodo, got.Status)
				require.Equal(t, "ready", got.GraphState.Wait.Phase)
				require.True(t, got.GraphState.Continuation)
				require.Equal(t, tc.reason, got.GraphState.Wake.Reason)
				require.Equal(t, before.GraphState.Wait.Next, got.GraphState.Wake.Next)
				require.Equal(t, 1, got.GraphState.Wake.Count)
			}
		})
	}
}

func TestWorkflowWaitReadyContinuationSurvivesDependencyBlocking(t *testing.T) {
	h, _, clock, path := newWorkflowWaitTestFixture(t, "timer")
	h.cfg.AutoPickUp = new(false)
	parked := h.readTask("tst-wait.md")
	require.NoError(t, parked.SetField("deps", []string{"tst-dep"}))
	data, err := parked.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	h.writeTicket("tst-dep.md", h.taskMD("tst-dep", "open", "one-stage"))
	var calls atomic.Int32
	d := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithFinalSummarySpawner(func(context.Context, FinalSummaryParams) (string, error) { return "summary", nil }), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls.Add(1)
		return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "complete", Summary: "continued"})
	}))
	stop := startWorkflowWaitTestDaemon(t, d)
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.tickets["tst-dep"] != nil && d.tickets["tst-wait"] != nil
	}, 5*time.Second, 10*time.Millisecond)
	clock.Advance(time.Hour)
	d.signalWorkflowWait()
	ready := awaitWorkflowWaitTestTicket(t, h, "tst-wait", func(tkt *ticket.Ticket) bool {
		return tkt.Status == ticket.StatusTodo && tkt.GraphState.Continuation
	})
	require.Equal(t, "ready", ready.GraphState.Wait.Phase)
	require.Equal(t, "timer", ready.GraphState.Wake.Reason)
	require.Zero(t, calls.Load(), "dependencies still gate explicit continuations")
	require.NoError(t, d.MoveTicket("tst-dep", "done"))
	got := h.waitForStatus("tst-wait.md", ticket.StatusHumanReview, 10*time.Second)
	requireWorkflowWaitTestIdle(t, d)
	stop()
	require.Equal(t, int32(1), calls.Load(), "dependency resolution must preserve continuation intent with auto pickup disabled")
	require.Len(t, got.History, 2)
	require.Equal(t, parked.GraphState.ExecutionID, got.GraphState.ExecutionID)
}

func TestWorkflowWaitLateProbeCannotResurrectCancellation(t *testing.T) {
	h, d, clock, path := newWorkflowWaitTestFixture(t, "check")
	started, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	d.runner = func(context.Context, RunnerParams) (process.Result, error) {
		close(started)
		<-release // deliberately return a late success, even after cancellation
		return workflowWaitTestResult(clock, 0), nil
	}
	done := make(chan bool, 1)
	snapshot := h.readTask("tst-wait.md")
	go func() {
		defer close(exited)
		done <- d.runWorkflowProbe(context.Background(), &workflowProbeJob{snapshot: snapshot, path: path})
	}()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("probe goroutine did not stop")
		}
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never launched")
	}
	require.NoError(t, d.MoveTicket("tst-wait", "cancelled"))
	cancelled := h.readTask("tst-wait.md")
	unblock()
	select {
	case stopped := <-done:
		require.True(t, stopped)
	case <-time.After(5 * time.Second):
		t.Fatal("late probe did not finish")
	}
	for range 3 {
		d.reconcileWorkflowWaits(context.Background())
	}
	got := h.readTask("tst-wait.md")
	require.Equal(t, ticket.StatusCancelled, got.Status)
	require.Equal(t, cancelled.GraphState, got.GraphState, "stale callbacks must not mutate cancelled wait evidence")
	require.Equal(t, cancelled.History, got.History)
	require.False(t, got.GraphState.Continuation)
	require.Empty(t, d.queuedIDs())
	require.Empty(t, d.workflowProbeJobs)
}

func TestWorkflowWaitDeadlineCancelsProbeBeforeContinuation(t *testing.T) {
	h, d, clock, path := newWorkflowWaitTestFixture(t, "check")
	started, cancelled, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	d.runner = func(ctx context.Context, _ RunnerParams) (process.Result, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release // cancellation alone is not proof that the probe stopped
		return workflowWaitTestResult(clock, 0), nil
	}
	snapshot := h.readTask("tst-wait.md")
	done := make(chan bool, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(exited)
		done <- d.runWorkflowProbe(ctx, &workflowProbeJob{snapshot: snapshot, path: path})
	}()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		cancel()
		unblock()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("probe goroutine did not stop")
		}
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never launched")
	}
	clock.Advance(time.Hour)
	d.reconcileWorkflowWaits(context.Background())
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("expired deadline did not cancel active probe")
	}
	beforeTermination := h.readTask("tst-wait.md")
	require.Equal(t, ticket.StatusPaused, beforeTermination.Status)
	require.False(t, beforeTermination.GraphState.Continuation)
	require.Nil(t, beforeTermination.GraphState.Wake)
	unblock()
	select {
	case stopped := <-done:
		require.True(t, stopped)
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not finish")
	}
	got := h.readTask("tst-wait.md")
	require.Equal(t, ticket.StatusTodo, got.Status)
	require.NotNil(t, got.GraphState.Wake)
	require.Equal(t, "deadline", got.GraphState.Wake.Reason, "deadline takes precedence over a late exit zero")
	require.True(t, got.GraphState.Continuation)
	require.Equal(t, 1, got.GraphState.WakeCount)
}

func TestWorkflowWaitTimeReconciliationDoesNotRequireCapacity(t *testing.T) {
	for _, kind := range []string{"timer", "check"} {
		t.Run(kind, func(t *testing.T) {
			h, d, clock, _ := newWorkflowWaitTestFixture(t, kind)
			before := h.readTask("tst-wait.md")
			if kind == "check" {
				// Keep the next poll beyond the deadline to isolate deadline
				// scheduling from ordinary-pool probe admission.
				require.NoError(t, d.mutateTicket(before.ID, func(current *ticket.Ticket) error {
					state := current.GraphState.Clone()
					state.Wait.NextPoll = state.Wait.Deadline.Add(time.Hour)
					return current.SetField("graph_state", state)
				}))
			}
			d.sem.used = d.sem.limit // an unrelated execution owns the only slot
			clock.Advance(time.Hour - 2*time.Second)
			require.Equal(t, 2*time.Second, d.nextWorkflowWait(clock.Now()))
			d.reconcileWorkflowWaits(context.Background())
			require.Equal(t, ticket.StatusPaused, h.readTask("tst-wait.md").Status)
			clock.Advance(2 * time.Second)
			d.reconcileWorkflowWaits(context.Background())
			got := h.readTask("tst-wait.md")
			require.Equal(t, ticket.StatusTodo, got.Status)
			require.True(t, got.GraphState.Continuation)
			require.NotNil(t, got.GraphState.Wake)
			reason := "timer"
			if kind == "check" {
				reason = "deadline"
			}
			require.Equal(t, reason, got.GraphState.Wake.Reason)
			require.Equal(t, before.GraphState.Wait.Next, got.GraphState.Wake.Next)
			require.Equal(t, before.History, got.History)
			require.Empty(t, d.workflowProbeJobs, "an expired check must not launch a final probe")
			require.Equal(t, 1, d.sem.used, "reconciling time must neither acquire nor release another execution's slot")
		})
	}
}

func TestWorkflowWaitPauseRetryPreservesDeadline(t *testing.T) {
	h, d, clock, _ := newWorkflowWaitTestFixture(t, "check")
	before := h.readTask("tst-wait.md")
	require.NoError(t, d.PauseTicket(before.ID))
	paused := h.readTask("tst-wait.md")
	require.True(t, paused.GraphState.Wait.Disarmed)
	require.Error(t, d.RunTicket(before.ID))
	require.Error(t, d.MoveTicket(before.ID, "todo"))
	clock.Advance(2 * time.Hour)
	d.reconcileWorkflowWaits(context.Background())
	require.Empty(t, d.workflowProbeJobs)
	require.Equal(t, paused.GraphState, h.readTask("tst-wait.md").GraphState)
	require.NoError(t, d.RetryTicket(before.ID))
	rearmed := h.readTask("tst-wait.md")
	require.False(t, rearmed.GraphState.Wait.Disarmed)
	require.Equal(t, before.GraphState.Wait.ID, rearmed.GraphState.Wait.ID)
	require.Equal(t, before.GraphState.Wait.RequestedAt, rearmed.GraphState.Wait.RequestedAt)
	require.Equal(t, before.GraphState.Wait.Deadline, rearmed.GraphState.Wait.Deadline)
	d.reconcileWorkflowWaits(context.Background())
	got := h.readTask("tst-wait.md")
	require.Equal(t, ticket.StatusTodo, got.Status)
	require.NotNil(t, got.GraphState.Wake)
	require.Equal(t, "deadline", got.GraphState.Wake.Reason)
	require.Empty(t, d.workflowProbeJobs)
}
