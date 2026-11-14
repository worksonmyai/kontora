package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func TestWorkflowContinuationRecoveryRetry(t *testing.T) {
	for _, kind := range []string{"timer", "check"} {
		for _, pause := range []bool{false, true} {
			for _, restart := range []string{"none", "before recovery", "after recovery"} {
				t.Run(fmt.Sprintf("%s/pause-%t/restart-%s", kind, pause, restart), func(t *testing.T) {
					h, d, clock, path := newWorkflowWaitTestFixture(t, kind)
					summary := func(context.Context, FinalSummaryParams) (string, error) { return "recovered workflow", nil }
					d.finalSummarySpawner = summary
					parked := h.readTask("tst-wait.md")
					if kind == "timer" {
						clock.Advance(time.Hour)
						d.reconcileWorkflowWaits(context.Background())
					} else {
						d.runner = func(context.Context, RunnerParams) (process.Result, error) {
							return workflowWaitTestResult(clock, 0), nil
						}
						require.True(t, d.runWorkflowProbe(context.Background(), &workflowProbeJob{snapshot: parked, path: path}))
					}
					ready := h.readTask("tst-wait.md")
					require.Equal(t, ticket.StatusTodo, ready.Status)
					require.True(t, ready.GraphState.Continuation)
					d.runner = func(context.Context, RunnerParams) (process.Result, error) {
						bound, err := loadExecution(h.cfg, ready.ID)
						require.NoError(t, err)
						require.True(t, bound.Continuation, "an intentional wake is not a failure retry")
						active := h.readTask("tst-wait.md")
						require.False(t, active.GraphState.Continuation, "binding consumes pending admission intent")
						require.False(t, bound.GraphBefore.Continuation)
						require.Zero(t, app.BuildView(h.cfg, active, true).GraphExecution.Attempt)
						if pause {
							require.NoError(t, d.PauseTicket(ready.ID))
						}
						result := workflowWaitTestResult(clock, 0)
						result.Interruption = context.Canceled
						return result, nil
					}
					d.runTicket(context.Background(), ready.ID)
					interrupted := h.readTask("tst-wait.md")
					require.Equal(t, ticket.StatusPaused, interrupted.Status)
					require.NotEmpty(t, interrupted.InvocationID)
					rec, err := loadExecution(h.cfg, ready.ID)
					require.NoError(t, err)
					if pause {
						require.Equal(t, rec.GraphBefore.Revision, interrupted.GraphState.Revision, "Pause must preserve the bound invocation's revision")
						require.NoError(t, d.PauseTicket(ready.ID), "repeated Pause must not invalidate a stopped invocation either")
						require.Equal(t, interrupted.GraphState.Revision, h.readTask("tst-wait.md").GraphState.Revision)
					}
					require.Error(t, d.RetryTicket(ready.ID), "Retry must still require recovery acknowledgement")
					if restart == "before recovery" {
						d = h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithFinalSummarySpawner(summary))
						require.NoError(t, d.initialScan(h.tasksDir))
					}
					current := h.readTask("tst-wait.md")
					require.NoError(t, d.WorkflowRecover(current.ID, current.GraphState.ExecutionID, current.GraphState.Revision))
					recovered := h.readTask("tst-wait.md")
					require.True(t, recovered.GraphState.Continuation, "recovery must restore ready-wait admission, not invocation provenance")
					previous := recovered.History[len(recovered.History)-1]
					require.Equal(t, rec.Invocation.ID, previous.InvocationID)
					require.True(t, previous.Continuation, "the interrupted invocation was still an intentional wake")
					require.Equal(t, ticket.RunOutcomeFailed, previous.RunOutcome)
					if pause {
						require.Nil(t, rec.PendingHistory, "operator cancellation exercises recovery without pending result evidence")
						require.Equal(t, -1, previous.ExitCode, "recovery must not invent a successful exit")
					}
					if restart == "after recovery" {
						d = h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithFinalSummarySpawner(summary))
						require.NoError(t, d.initialScan(h.tasksDir))
					}
					require.NoError(t, d.RetryTicket(ready.ID))
					retried := h.readTask("tst-wait.md")
					require.Equal(t, ticket.StatusTodo, retried.Status, "Retry must resume the ready continuation, not park it forever")
					require.Equal(t, ready.GraphState.Wait.ID, retried.GraphState.Wait.ID)
					require.Equal(t, ready.GraphState.Wake, retried.GraphState.Wake)
					for _, detail := range []bool{false, true} {
						queued := app.BuildView(h.cfg, retried, detail)
						assert.False(t, queued.GraphExecution.Continuation)
						assert.Equal(t, 1, queued.GraphExecution.Attempt, "the queued retry must display attempt 2")
					}
					d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
						bound, err := loadExecution(h.cfg, ready.ID)
						require.NoError(t, err)
						assert.False(t, bound.Continuation, "recovery + Retry is a failure retry, not another intentional wake")
						current := h.readTask("tst-wait.md")
						require.False(t, current.GraphState.Continuation)
						for _, detail := range []bool{false, true} {
							active := app.BuildView(h.cfg, current, detail)
							assert.False(t, active.GraphExecution.Continuation)
							assert.Equal(t, 1, active.GraphExecution.Attempt, "the running retry must display attempt 2 (projection is zero-based)")
						}
						return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "complete", Summary: "resumed after recovery"})
					}
					d.runTicket(context.Background(), ready.ID)
					got := h.readTask("tst-wait.md")
					require.Equal(t, ticket.StatusHumanReview, got.Status)
					require.Equal(t, ready.GraphState.ExecutionID, got.GraphState.ExecutionID)
					require.Len(t, got.History, len(recovered.History)+1)
					require.Equal(t, recovered.History, got.History[:len(recovered.History)])
					last := got.History[len(got.History)-1]
					require.Equal(t, ready.GraphState.VisitID, last.NodeVisitID)
					require.Equal(t, ready.GraphState.Wake, last.Wake)
					assert.False(t, last.Continuation, "the retry history must consume a new attempt")
					view := app.BuildView(h.cfg, got, true)
					assert.Equal(t, 2, view.History[len(view.History)-1].VisitAttempt)
					assert.Equal(t, 1, view.History[len(view.History)-2].VisitAttempt, "the interrupted wake must not itself consume a retry")
					require.NoFileExists(t, executionPath(h.cfg, ready.ID))
				})
			}
		}
	}
}

func TestWorkflowContinuationPauseInterruptedBinding(t *testing.T) {
	for _, kind := range []string{"timer", "check"} {
		t.Run(kind, func(t *testing.T) {
			h, d, clock, path := newWorkflowWaitTestFixture(t, kind)
			parked := h.readTask("tst-wait.md")
			ready, err := d.patchWorkflowWait(parked, path, func(current *ticket.Ticket) error {
				return wakeWorkflowAgent(current, "ready")
			})
			require.NoError(t, err)
			require.NoError(t, d.initializeWorkflow(h.cfg, ready, h.cfg.Pipelines[ready.Pipeline].Graph))
			require.NoError(t, ready.SetField("status", string(ticket.StatusInProgress)))
			saveWorkflowWaitTestTicket(t, path, ready)
			d.tickets[ready.ID] = newTicketState(ready.Clone(), path)
			beforeBinding := ready.Clone()
			params := RunnerParams{}
			rec, err := d.bindExecution(ready, spawnAgentParams{cfg: h.cfg, ticketID: ready.ID, filePath: path,
				stageName: ready.Stage, agentName: "agent1", graphState: ready.GraphState.Clone()}, &params, "")
			require.NoError(t, err)
			// The bound journal can reach disk before the ticket's invocation binding.
			saveWorkflowWaitTestTicket(t, path, beforeBinding)
			d.tickets[ready.ID] = newTicketState(beforeBinding.Clone(), path)
			require.NoError(t, d.PauseTicket(ready.ID))
			paused := h.readTask("tst-wait.md")
			require.Empty(t, paused.InvocationID)
			require.Equal(t, rec.GraphBefore.Revision, paused.GraphState.Revision, "Pause must not invalidate an interrupted binding's journal")
			d = h.newDaemon(h.cfg, WithWorkflowWaitClock(clock))
			require.NoError(t, d.initialScan(h.tasksDir))
			current := h.readTask("tst-wait.md")
			require.Equal(t, rec.Invocation.ID, current.InvocationID)
			require.NoError(t, d.WorkflowRecover(current.ID, current.GraphState.ExecutionID, current.GraphState.Revision))
			require.NoError(t, d.RetryTicket(current.ID))
			got := h.readTask("tst-wait.md")
			require.Equal(t, ticket.StatusTodo, got.Status)
			require.Equal(t, beforeBinding.GraphState.Wait, got.GraphState.Wait)
			require.Equal(t, beforeBinding.GraphState.Wake, got.GraphState.Wake)
		})
	}
}

func TestWorkflowParkedPauseWithRetiringInvocationJournal(t *testing.T) {
	h := newHarness(t)
	cfg := workflowWaitTestConfig(t, h)
	clock := newWorkflowWaitTestClock()
	var rec *executionRecord
	d := h.newDaemon(cfg, WithWorkflowWaitClock(clock), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		var err error
		rec, err = loadExecution(cfg, p.TicketID)
		require.NoError(t, err)
		return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "waiting", Summary: "parked", NextStep: "continue", Wait: &session.WaitRequest{Kind: "timer", Until: clock.Now().Add(time.Hour)}})
	}))
	path := h.writeTicket("tst-wait.md", h.taskMD("tst-wait", "todo", "wait-graph"))
	d.tickets["tst-wait"] = newTicketState(h.readTask("tst-wait.md"), path)
	d.runTicket(context.Background(), "tst-wait")
	parked := h.readTask("tst-wait.md")
	require.Empty(t, parked.InvocationID)
	require.NotNil(t, parked.GraphState.Wait)
	rec.Phase = "transition"
	rec.Transition = &executionTransition{Status: parked.Status, Stage: parked.Stage, Attempt: parked.Attempt,
		GraphState: parked.GraphState.Clone(), History: parked.History[len(parked.History)-1]}
	// A committed suspension can still have its predecessor's journal awaiting retirement.
	require.NoError(t, saveExecution(cfg, rec))
	require.NoError(t, d.PauseTicket(parked.ID))
	got := h.readTask("tst-wait.md")
	require.True(t, got.GraphState.Wait.Disarmed, "a predecessor journal must not suppress disarming the new wait")
	require.Equal(t, parked.GraphState.Revision+1, got.GraphState.Revision)
	require.Equal(t, parked.GraphState.Wait.Until, got.GraphState.Wait.Until)
}

func TestWorkflowProbePauseAfterFinalRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exit      int
		uncertain bool
		cancel    bool
	}{
		{name: "ready"},
		{name: "not ready", exit: 1},
		{name: "uncertain", uncertain: true},
		{name: "cancelled", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d, clock, path := newWorkflowWaitTestFixture(t, "check")
			before := h.readTask("tst-wait.md")
			journal := &workflowProbeJournal{TicketID: before.ID, ExecutionID: before.GraphState.ExecutionID, WaitID: before.GraphState.Wait.ID,
				PollID: "pause-race-poll", Path: path, Repository: before.GraphState.Wait.Repository, Branch: before.Branch,
				RepoPath: before.Path, Fingerprint: before.GraphState.DefinitionFingerprint, Phase: "launched"}
			active, err := d.patchWorkflowWait(before, path, func(current *ticket.Ticket) error {
				current.GraphState.Wait.Phase, current.GraphState.Wait.PollID = "probing", journal.PollID
				journal.Active, journal.Revision = current.GraphState.Clone(), current.GraphState.Revision
				return saveWorkflowProbe(h.cfg, journal)
			})
			require.NoError(t, err)
			journal.Result = &workflowProbeResult{ExitCode: tc.exit, Stopped: !tc.uncertain, CompletedAt: clock.Now(), Output: "saved probe evidence"}
			journal.Phase = "stopped"
			require.NoError(t, saveWorkflowProbe(h.cfg, journal))
			snapshot := freshWorkflowProbeTicket(active, path)
			if tc.cancel {
				require.NoError(t, d.MoveTicket(before.ID, "cancelled"))
			} else {
				require.NoError(t, d.PauseTicket(before.ID))
			}
			paused := h.readTask("tst-wait.md")
			err = d.commitWorkflowProbeResult(snapshot, path, journal)
			if tc.cancel {
				require.ErrorIs(t, err, errExecutionChanged)
				require.Equal(t, paused.GraphState, h.readTask("tst-wait.md").GraphState, "a stale probe must not restore a cancelled wait")
				return
			}
			require.NoError(t, err, "Pause between the final read and patch must not strand a stopped probe")
			got := h.readTask("tst-wait.md")
			require.Equal(t, ticket.StatusPaused, got.Status)
			require.True(t, got.GraphState.Wait.Disarmed)
			require.Equal(t, "saved probe evidence", got.GraphState.Wait.Output)
			require.Equal(t, 1, got.GraphState.Wait.PollCount)
			require.Nil(t, got.GraphState.Wake)
			require.False(t, got.GraphState.Continuation)
			require.Equal(t, before.History, got.History)
			if tc.uncertain {
				require.Equal(t, "recovery_required", got.GraphState.Wait.Phase)
				require.NoError(t, d.WorkflowRecover(got.ID, got.GraphState.ExecutionID, got.GraphState.Revision))
			} else {
				require.Equal(t, "pending", got.GraphState.Wait.Phase)
				require.Equal(t, clock.Now().Add(before.GraphState.Wait.Poll), got.GraphState.Wait.NextPoll)
			}
			require.NoError(t, d.RetryTicket(got.ID))
			clock.Advance(5 * time.Minute)
			d.reconcileWorkflowWaits(context.Background())
			require.Len(t, d.workflowProbeJobs, 1, "the same daemon must be able to re-arm the wait")
		})
	}
}

func TestWorkflowProbeRecoveryGuidance(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("cancelled-%t/restart-%t", cancelled, restart), func(t *testing.T) {
				h, d, clock, path := newWorkflowWaitTestFixture(t, "check")
				before := h.readTask("tst-wait.md")
				d.runner = func(context.Context, RunnerParams) (process.Result, error) {
					if cancelled {
						require.NoError(t, d.MoveTicket(before.ID, "cancelled"))
					}
					result := workflowWaitTestResult(clock, 0)
					result.Termination = process.TerminationUnknown
					return result, process.ErrTerminationUncertain
				}
				d.sem.used++
				require.False(t, d.runWorkflowProbe(context.Background(), &workflowProbeJob{snapshot: before, path: path}))
				if restart {
					d = h.newDaemon(h.cfg, WithWorkflowWaitClock(clock))
					require.NoError(t, d.initialScan(h.tasksDir))
					d.recoverWorkflowProbes()
				}
				current := h.readTask("tst-wait.md")
				require.NoError(t, d.WorkflowRecover(current.ID, current.GraphState.ExecutionID, current.GraphState.Revision))
				got := h.readTask("tst-wait.md")
				require.Zero(t, d.sem.used)
				require.NoFileExists(t, workflowProbePath(h.cfg, got.ID))
				if cancelled {
					require.Equal(t, "Cancelled probe recovery acknowledged; the workflow remains cancelled.", got.LastError)
					require.Equal(t, ticket.StatusCancelled, got.Status)
					require.Nil(t, got.GraphState.Wait)
					require.Empty(t, d.queuedIDs())
				} else {
					require.Equal(t, "probe recovery acknowledged; Retry to re-arm the same wait", got.LastError)
					require.Equal(t, ticket.StatusPaused, got.Status)
					require.Equal(t, before.GraphState.Wait.ID, got.GraphState.Wait.ID)
					require.True(t, got.GraphState.Wait.Disarmed)
					require.NoError(t, d.RetryTicket(got.ID))
					require.False(t, h.readTask("tst-wait.md").GraphState.Wait.Disarmed)
				}
			})
		}
	}
}
