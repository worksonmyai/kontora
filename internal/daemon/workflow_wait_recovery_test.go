package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func readWorkflowWaitTestProbeJournal(t *testing.T, h *testHarness) *workflowProbeJournal {
	t.Helper()
	data, err := os.ReadFile(workflowProbePath(h.cfg, "tst-wait"))
	require.NoError(t, err)
	var journal workflowProbeJournal
	require.NoError(t, json.Unmarshal(data, &journal))
	return &journal
}

func saveWorkflowWaitTestTicket(t *testing.T, path string, tkt *ticket.Ticket) {
	t.Helper()
	data, err := tkt.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func TestWorkflowWaitRestartKeepsAbsoluteTiming(t *testing.T) {
	for _, kind := range []string{"timer", "check"} {
		t.Run(kind, func(t *testing.T) {
			h, first, clock, path := newWorkflowWaitTestFixture(t, kind)
			if kind == "check" {
				first.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
					clock.Advance(2 * time.Minute)
					return workflowWaitTestResult(clock, 1), writeWorkflowWaitTestLog(p, "not ready before restart")
				}
				require.True(t, first.runWorkflowProbe(context.Background(), &workflowProbeJob{snapshot: h.readTask("tst-wait.md"), path: path}))
			}
			before := h.readTask("tst-wait.md")
			w := before.GraphState.Wait.Clone()
			due := w.Until
			if kind == "check" {
				due = w.NextPoll
			}
			// Simulate downtime without creating a new relative deadline.
			clock.Advance(time.Minute)
			var agents, probes atomic.Int32
			second := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithFinalSummarySpawner(func(context.Context, FinalSummaryParams) (string, error) { return "summary", nil }), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				if p.Binary == "/bin/sh" {
					probes.Add(1)
					return workflowWaitTestResult(clock, 0), writeWorkflowWaitTestLog(p, "ready after restart")
				}
				agents.Add(1)
				return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "complete", Summary: "resumed after restart"})
			}))
			stop := startWorkflowWaitTestDaemon(t, second)
			require.Eventually(t, func() bool {
				second.mu.Lock()
				defer second.mu.Unlock()
				return second.tickets[before.ID] != nil
			}, 5*time.Second, 10*time.Millisecond)
			requireWorkflowWaitTestIdle(t, second)
			loaded := h.readTask("tst-wait.md")
			require.Equal(t, w, loaded.GraphState.Wait, "startup must not reset requested time, poll count, deadline, or next poll")
			require.Equal(t, before.History, loaded.History)
			require.Zero(t, agents.Load())
			require.Zero(t, probes.Load())
			clock.Advance(due.Sub(clock.Now()))
			second.signalWorkflowWait()
			got := h.waitForStatus("tst-wait.md", ticket.StatusHumanReview, 10*time.Second)
			requireWorkflowWaitTestIdle(t, second)
			stop()
			require.Equal(t, int32(1), agents.Load())
			if kind == "check" {
				require.Equal(t, int32(1), probes.Load(), "do not repeat the pre-restart completed probe")
			} else {
				require.Zero(t, probes.Load())
			}
			require.Len(t, got.History, 2)
			require.Equal(t, before.GraphState.ExecutionID, got.GraphState.ExecutionID)
			require.Equal(t, before.GraphState.VisitID, got.History[1].NodeVisitID)
			require.Equal(t, before.History[0], got.History[0])
			require.NotNil(t, got.History[1].Wake)
			require.Equal(t, w.ID, got.History[1].Wake.WaitID)
		})
	}
}

// Force the real finalization journal to commit before the ticket write, then
// recover in a fresh daemon. Neither an agent nor a required hook may repeat.
func TestWorkflowWaitFrozenSuspensionRecovery(t *testing.T) {
	for _, alreadyCommitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("ticket-committed-%t", alreadyCommitted), func(t *testing.T) {
			h := newHarness(t)
			cfg := workflowWaitTestConfig(t, h)
			clock := newWorkflowWaitTestClock()
			marker := filepath.Join(t.TempDir(), "hook-runs")
			cfg.Hooks = config.Hooks{config.HookStageEnd: {{Run: fmt.Sprintf(`echo hook >> %q; chmod 444 "$KONTORA_TICKET_FILE"`, marker), OnFailure: "pause"}}}
			var calls atomic.Int32
			first := h.newDaemon(cfg, WithWorkflowWaitClock(clock), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls.Add(1)
				return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "waiting", Summary: "waiting for evidence", NextStep: "Read saved evidence.", Wait: &session.WaitRequest{Kind: "timer", Until: clock.Now().Add(time.Hour)}})
			}))
			path := h.writeTicket("tst-wait.md", h.taskMD("tst-wait", "todo", "wait-graph"))
			t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0o644)) })
			first.tickets["tst-wait"] = newTicketState(h.readTask("tst-wait.md"), path)
			first.runTicket(context.Background(), "tst-wait")
			journal, err := loadExecution(cfg, "tst-wait")
			require.NoError(t, err)
			require.Equal(t, "transition", journal.Phase)
			require.NotNil(t, journal.Transition)
			require.NotNil(t, journal.Transition.GraphState.Wait)
			frozen := journal.Transition.GraphState.Clone()
			require.Equal(t, ticket.StatusPaused, journal.Transition.Status)
			require.Equal(t, "waiting", string(journal.Transition.History.RunOutcome))
			require.NoError(t, os.Chmod(path, 0o644))
			if alreadyCommitted {
				committed := h.readTask("tst-wait.md")
				require.NoError(t, committed.SetField("graph_state", frozen.Clone()))
				require.NoError(t, committed.SetField("status", "paused"))
				require.NoError(t, committed.SetField("invocation_id", ""))
				require.NoError(t, committed.SetField("history", []ticket.HistoryEntry{journal.Transition.History}))
				require.NoError(t, committed.SetField("summary", journal.Transition.Summary))
				require.NoError(t, committed.SetField("last_error", journal.Transition.LastError))
				require.NoError(t, committed.SetField("attempt", journal.Transition.Attempt))
				saveWorkflowWaitTestTicket(t, path, committed)
				require.True(t, committedExecutionMatches(committed, journal))
			}
			clock.Advance(20 * time.Minute)
			second := h.newDaemon(cfg, WithWorkflowWaitClock(clock), WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				calls.Add(1)
				return workflowWaitTestResult(clock, 0), fmt.Errorf("recovery must not launch a runner")
			}))
			require.NoError(t, second.initialScan(h.tasksDir))
			got := h.readTask("tst-wait.md")
			require.Equal(t, ticket.StatusPaused, got.Status)
			require.Equal(t, frozen, got.GraphState, "replay must use the frozen wait identity and timestamps")
			require.Empty(t, got.InvocationID)
			require.Len(t, got.History, 1)
			require.Equal(t, int32(1), calls.Load())
			require.NoFileExists(t, executionPath(cfg, got.ID))
			hooks, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Equal(t, "hook\n", string(hooks))
			requireWorkflowWaitTestIdle(t, second)
			for range 3 {
				second.handleFileChanged(path)
				second.reconcileWorkflowWaits(context.Background())
			}
			require.Equal(t, got.GraphState, h.readTask("tst-wait.md").GraphState)
			require.Len(t, h.readTask("tst-wait.md").History, 1)
		})
	}
}

func TestWorkflowWaitProbeResultJournalRecovery(t *testing.T) {
	for _, exit := range []int{0, 1} {
		for _, alreadyCommitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("exit-%d/ticket-committed-%t", exit, alreadyCommitted), func(t *testing.T) {
				h, first, clock, path := newWorkflowWaitTestFixture(t, "check")
				t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0o644)) })
				before := h.readTask("tst-wait.md")
				var calls atomic.Int32
				first.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
					calls.Add(1)
					clock.Advance(2 * time.Minute)
					if err := writeWorkflowWaitTestLog(p, "saved probe evidence ✓"); err != nil {
						return workflowWaitTestResult(clock, exit), err
					}
					return workflowWaitTestResult(clock, exit), os.Chmod(path, 0o444)
				}
				require.False(t, first.runWorkflowProbe(context.Background(), &workflowProbeJob{snapshot: before, path: path}), "ticket-write failure retains the completed probe until journal replay")
				journal := readWorkflowWaitTestProbeJournal(t, h)
				require.Equal(t, "result", journal.Phase)
				require.NotNil(t, journal.After)
				journal.Path = journalTicketAlias(t, path)
				require.NoError(t, saveWorkflowProbe(h.cfg, journal))
				frozen := journal.After.Clone()
				require.Equal(t, 1, frozen.Wait.PollCount)
				require.Equal(t, "saved probe evidence ✓", frozen.Wait.Output)
				require.NoError(t, os.Chmod(path, 0o644))
				if alreadyCommitted {
					committed := h.readTask("tst-wait.md")
					require.NoError(t, committed.SetField("graph_state", frozen.Clone()))
					require.NoError(t, committed.SetField("status", string(journal.Status)))
					require.NoError(t, committed.SetField("last_error", journal.LastError))
					saveWorkflowWaitTestTicket(t, path, committed)
				}
				clock.Advance(time.Minute)
				second := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
					calls.Add(1)
					return workflowWaitTestResult(clock, 0), fmt.Errorf("journal recovery repeated a completed probe")
				}))
				require.NoError(t, second.initialScan(h.tasksDir))
				second.recoverWorkflowProbes()
				got := h.readTask("tst-wait.md")
				require.Equal(t, frozen, got.GraphState)
				require.Equal(t, journal.Status, got.Status)
				require.Equal(t, before.History, got.History, "probe recovery is not an agent invocation")
				require.NoFileExists(t, workflowProbePath(h.cfg, got.ID))
				require.Equal(t, int32(1), calls.Load())
				requireWorkflowWaitTestIdle(t, second)
				for range 3 {
					second.recoverWorkflowProbes()
					second.reconcileWorkflowWaits(context.Background())
				}
				require.Equal(t, frozen, h.readTask("tst-wait.md").GraphState)
				if exit == 0 {
					require.True(t, got.GraphState.Continuation)
					require.NotNil(t, got.GraphState.Wake)
					require.Equal(t, "ready", got.GraphState.Wake.Reason)
					require.Equal(t, []string{got.ID}, second.queuedIDs())
				} else {
					require.Equal(t, before.GraphState.Wait.NextPoll.Add(7*time.Minute), got.GraphState.Wait.NextPoll)
					require.Empty(t, second.workflowProbeJobs, "restart must preserve the future next poll")
				}
			})
		}
	}
}

func TestWorkflowWaitLaunchedProbeRequiresRecovery(t *testing.T) {
	for _, launchCommitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("launch-ticket-committed-%t", launchCommitted), func(t *testing.T) {
			h, _, clock, path := newWorkflowWaitTestFixture(t, "check")
			before := h.readTask("tst-wait.md")
			active := before.GraphState.Clone()
			active.Revision++
			active.Wait.Phase, active.Wait.PollID = "probing", "interrupted-poll"
			w := active.Wait
			journal := &workflowProbeJournal{TicketID: before.ID, ExecutionID: active.ExecutionID, WaitID: w.ID, PollID: w.PollID,
				Revision: active.Revision, Path: path, Repository: w.Repository, Branch: w.Branch, RepoPath: before.Path,
				Fingerprint: active.DefinitionFingerprint, Phase: "launched", Active: active.Clone()}
			require.NoError(t, saveWorkflowProbe(h.cfg, journal))
			if launchCommitted {
				require.NoError(t, before.SetField("graph_state", active))
				saveWorkflowWaitTestTicket(t, path, before)
			}
			var calls atomic.Int32
			second := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				calls.Add(1)
				return workflowWaitTestResult(clock, 0), nil
			}))
			require.NoError(t, second.initialScan(h.tasksDir))
			for range 3 {
				second.recoverWorkflowProbes()
				second.reconcileWorkflowWaits(context.Background())
			}
			got := h.readTask("tst-wait.md")
			require.Equal(t, ticket.StatusPaused, got.Status)
			require.NotNil(t, got.GraphState.Wait)
			require.Equal(t, "recovery_required", got.GraphState.Wait.Phase)
			require.True(t, got.GraphState.Wait.Disarmed)
			require.Equal(t, w.ID, got.GraphState.Wait.ID)
			require.Equal(t, w.Deadline, got.GraphState.Wait.Deadline)
			require.Equal(t, w.NextPoll, got.GraphState.Wait.NextPoll)
			require.False(t, got.GraphState.Continuation)
			require.Nil(t, got.GraphState.Wake)
			require.Zero(t, calls.Load())
			require.Equal(t, 1, second.sem.used, "unknown probe termination retains exactly one slot across repeated scans")
			require.Empty(t, second.workflowProbeJobs)
			require.Error(t, second.RetryTicket(got.ID), "Retry must not infer process termination")
			require.Error(t, second.WorkflowRecover(got.ID, got.GraphState.ExecutionID, got.GraphState.Revision-1))
			require.NoError(t, second.WorkflowRecover(got.ID, got.GraphState.ExecutionID, got.GraphState.Revision))
			ack := h.readTask("tst-wait.md")
			require.Equal(t, "pending", ack.GraphState.Wait.Phase)
			require.True(t, ack.GraphState.Wait.Disarmed, "recovery acknowledgment alone must not launch a replacement")
			require.Equal(t, w.Deadline, ack.GraphState.Wait.Deadline)
			require.Zero(t, second.sem.used)
			require.NoFileExists(t, workflowProbePath(h.cfg, got.ID))
			require.NoError(t, second.RetryTicket(got.ID))
			rearmed := h.readTask("tst-wait.md")
			require.False(t, rearmed.GraphState.Wait.Disarmed)
			require.Equal(t, w.ID, rearmed.GraphState.Wait.ID)
			require.Equal(t, w.Deadline, rearmed.GraphState.Wait.Deadline)
			second.reconcileWorkflowWaits(context.Background())
			require.Len(t, second.workflowProbeJobs, 1)
		})
	}
}

func TestWorkflowWaitUncertainProbeDoesNotLaunchReplacement(t *testing.T) {
	h, d, clock, path := newWorkflowWaitTestFixture(t, "check")
	before := h.readTask("tst-wait.md")
	var calls atomic.Int32
	d.runner = func(_ context.Context, _ RunnerParams) (process.Result, error) {
		calls.Add(1)
		result := workflowWaitTestResult(clock, 0)
		result.Termination = process.TerminationUnknown
		return result, process.ErrTerminationUncertain
	}
	// Direct probe execution bypasses dispatcher admission, so account for the
	// slot it would have reserved. A false return leaves that slot retained.
	d.sem.used++
	require.False(t, d.runWorkflowProbe(context.Background(), &workflowProbeJob{snapshot: before, path: path}))
	clock.Advance(2 * time.Hour)
	for range 3 {
		d.reconcileWorkflowWaits(context.Background())
	}
	got := h.readTask("tst-wait.md")
	require.Equal(t, ticket.StatusPaused, got.Status)
	require.Equal(t, "recovery_required", got.GraphState.Wait.Phase)
	require.True(t, got.GraphState.Wait.Disarmed)
	require.Nil(t, got.GraphState.Wake, "even an expired deadline cannot reuse a slot with uncertain termination")
	require.Equal(t, before.History, got.History)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, 1, d.sem.used)
	require.Empty(t, d.workflowProbeJobs)
	require.Empty(t, d.queuedIDs())
	require.Equal(t, "recovery_required", readWorkflowWaitTestProbeJournal(t, h).Phase)
}

func TestWorkflowWaitProbeDiagnosticIsBoundedAndInvocationQualified(t *testing.T) {
	h, d, clock, path := newWorkflowWaitTestFixture(t, "check")
	before := h.readTask("tst-wait.md")
	var launches []RunnerParams
	// Three-byte runes force the 16 KiB byte boundary through a rune.
	output := ""
	for range 6000 {
		output += "界"
	}
	d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
		launches = append(launches, p)
		return workflowWaitTestResult(clock, 1), writeWorkflowWaitTestLog(p, output)
	}
	for range 2 {
		current := h.readTask("tst-wait.md")
		require.True(t, d.runWorkflowProbe(context.Background(), &workflowProbeJob{snapshot: current, path: path}))
		clock.Advance(5 * time.Minute)
	}
	got := h.readTask("tst-wait.md")
	w := got.GraphState.Wait
	require.Equal(t, 2, w.PollCount)
	require.True(t, w.OutputTruncated)
	require.LessOrEqual(t, len(w.Output), 16*1024)
	require.NotEmpty(t, w.Output)
	require.True(t, utf8.ValidString(w.Output))
	require.Equal(t, before.History, got.History)
	require.Len(t, launches, 2)
	require.NotEqual(t, launches[0].AttemptID, launches[1].AttemptID)
	require.NotEqual(t, launches[0].LogFile, launches[1].LogFile)
	require.Equal(t, launches[1].LogFile, w.LogPath)
	for _, launch := range launches {
		require.Contains(t, launch.LogFile, w.ID)
		require.Contains(t, launch.LogFile, launch.AttemptID)
		require.FileExists(t, launch.LogFile)
	}
}
