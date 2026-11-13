package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestWorkflowRecoverRetainedLiveRun(t *testing.T) {
	for _, mode := range []string{"retained", "active", "finishing", "reviewing", "restart", "stale revision"} {
		t.Run(mode, func(t *testing.T) {
			f := workflowActionFixture(t)
			f.d.reserveRecoveredExecutionLocked(f.tkt)
			f.d.liveRuns[f.tkt.ID] = liveRun{stage: f.tkt.Stage, params: RunnerParams{Env: f.rec.Invocation.Env()}}
			revision := f.tkt.GraphState.Revision
			switch mode {
			case "active":
				delete(f.d.recoveredReservations, f.tkt.ID)
			case "finishing":
				r := f.d.recoveredReservations[f.tkt.ID]
				r.finishing = true
				f.d.recoveredReservations[f.tkt.ID] = r
			case "reviewing":
				f.d.plannotator[f.tkt.ID] = func() {}
			case "stale revision":
				revision--
			}
			before := f.h.readTask(f.tkt.ID + ".md")
			var err error
			if mode == "restart" {
				err = f.d.WorkflowRestart(f.tkt.ID)
			} else {
				err = f.d.WorkflowRecover(f.tkt.ID, f.tkt.GraphState.ExecutionID, revision)
			}
			if mode != "retained" {
				require.ErrorIs(t, err, web.ErrInvalidState)
				assert.Equal(t, before, f.h.readTask(f.tkt.ID+".md"))
				assert.Contains(t, f.d.liveRuns, f.tkt.ID)
				assert.Equal(t, 1, f.d.sem.used)
				require.FileExists(t, executionPath(f.h.cfg, f.tkt.ID))
				return
			}
			require.NoError(t, err)
			assert.Empty(t, f.d.liveRuns)
			assert.Empty(t, f.d.running)
			assert.Empty(t, f.d.runningBranches)
			assert.Empty(t, f.d.recoveredReservations)
			assert.Zero(t, f.d.sem.used)
			require.NoFileExists(t, executionPath(f.h.cfg, f.tkt.ID))
		})
	}
}

func TestWorkflowUncertainAgentRecoveryWithoutRestart(t *testing.T) {
	h := newHarness(t)
	h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{
		Version: 2, Entry: "work", Nodes: []config.WorkflowNode{
			{ID: "work", Type: "agent", Stage: "step1", Agent: "agent1"},
			{ID: "accept", Type: "end", Status: "human_review"},
			{ID: "stop", Type: "end", Status: "paused"},
		}, Edges: []config.WorkflowEdge{
			{From: config.WorkflowPort{Node: "work", Port: "complete"}, To: "accept"},
			{From: config.WorkflowPort{Node: "work", Port: "failed"}, To: "stop"},
		},
	}}
	calls := 0
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls++
		now := time.Now()
		if calls == 1 {
			return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationUnknown}, process.ErrTerminationUncertain
		}
		inv, err := session.InvocationFromEnv(func(key string) string { return p.Env[key] })
		require.NoError(t, err)
		_, err = session.PublishResult(inv.ResultPath, session.Result{Outcome: "complete", Summary: "finished after inspection"}, 2)
		require.NoError(t, err)
		return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, nil
	}))
	path := h.writeTicket("tst-uncertain.md", h.taskMD("tst-uncertain", "todo", "graph"))
	d.tickets["tst-uncertain"] = newTicketState(h.readTask("tst-uncertain.md"), path)
	d.runTicket(context.Background(), "tst-uncertain")
	before := h.readTask("tst-uncertain.md")
	require.Equal(t, ticket.StatusPaused, before.Status)
	require.Contains(t, d.liveRuns, before.ID)
	require.Contains(t, d.recoveredReservations, before.ID)
	require.Equal(t, 1, d.sem.used)
	require.ErrorIs(t, d.RetryTicket(before.ID), web.ErrInvalidState)
	require.NoError(t, d.WorkflowRecover(before.ID, before.GraphState.ExecutionID, before.GraphState.Revision))
	assert.Empty(t, d.liveRuns)
	assert.Empty(t, d.runningBranches)
	assert.Zero(t, d.sem.used)
	require.NoError(t, d.RetryTicket(before.ID))
	d.runTicket(context.Background(), before.ID)
	after := h.readTask("tst-uncertain.md")
	assert.Equal(t, ticket.StatusHumanReview, after.Status)
	assert.Equal(t, before.GraphState.ExecutionID, after.GraphState.ExecutionID)
	require.Len(t, after.History, 1)
	assert.NotEqual(t, before.InvocationID, after.History[0].InvocationID)
	assert.Equal(t, 2, calls)
}

func workflowPendingRecoveryFixture(t *testing.T) *resultFixture {
	t.Helper()
	f := workflowActionFixture(t)
	started := time.Date(2026, time.January, 1, 1, 0, 0, 0, time.UTC)
	run := agentRun{Execution: f.rec, Outcome: "complete", Model: "test-model", Effort: "high",
		FinalMessage: "node completed", SessionKind: "pi", SessionRef: "sessions/node.jsonl",
		Result: process.Result{StartedAt: started, ExitedAt: started.Add(time.Minute), Termination: process.TerminationProcessGroupGone}}
	require.NoError(t, f.d.beginFinalization(f.h.cfg, f.rec, run))
	f.rec.HookTermination = "pending"
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	return f
}

func assertWorkflowRecoveryHistory(t *testing.T, f *resultFixture, got *ticket.Ticket) {
	t.Helper()
	require.Len(t, got.History, 1)
	want := *f.rec.PendingHistory
	want.RunOutcome = ticket.RunOutcomeFailed
	want.OutcomeReason = got.History[0].OutcomeReason
	assert.NotEmpty(t, want.OutcomeReason)
	assert.Equal(t, want, got.History[0])
	assert.Equal(t, f.rec.GraphBefore.ExecutionID, got.History[0].WorkflowExecutionID)
	assert.Equal(t, f.rec.GraphBefore.VisitID, got.History[0].NodeVisitID)
	assert.Greater(t, stageRunIndex(got, got.History[0].Stage), got.History[0].Run)
	assert.Empty(t, got.GraphState.Transitions)
}

func TestWorkflowRecoverPreservesPendingHistory(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending only", true: "already recorded"}[existing], func(t *testing.T) {
			f := workflowPendingRecoveryFixture(t)
			if existing {
				h := *f.rec.PendingHistory
				h.RunOutcome, h.OutcomeReason = ticket.RunOutcomeFailed, "interrupted during stage_end"
				require.NoError(t, f.tkt.SetField("history", []ticket.HistoryEntry{h}))
				f.write(t)
			}
			before := f.reconcile(t)
			require.Equal(t, ticket.StatusPaused, before.Status)
			require.NoError(t, f.d.WorkflowRecover(before.ID, before.GraphState.ExecutionID, before.GraphState.Revision))
			got := f.h.readTask(before.ID + ".md")
			assertWorkflowRecoveryHistory(t, f, got)
			if existing {
				assert.Equal(t, before.History, got.History)
			}
			assert.Zero(t, f.d.sem.used)
			require.NoFileExists(t, executionPath(f.h.cfg, got.ID))
		})
	}
}

func TestWorkflowAcknowledgementReplayPreservesPendingHistory(t *testing.T) {
	for _, point := range []string{"before ticket save", "after ticket save", "saved without history", "operator move without history"} {
		t.Run(point, func(t *testing.T) {
			f := workflowPendingRecoveryFixture(t)
			f.rec.Phase = "acknowledged"
			f.rec.RecoveryState = f.tkt.GraphState.Clone()
			f.rec.RecoveryState.Revision++
			if point != "before ticket save" {
				require.NoError(t, installRecoveryAcknowledgement(f.tkt, f.rec))
				if point != "after ticket save" {
					require.NoError(t, f.tkt.SetField("history", []ticket.HistoryEntry{}))
				}
				if point == "operator move without history" {
					require.NoError(t, f.tkt.SetField("status", "open"))
				}
				f.write(t)
			}
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			f.d.reserveRecoveredExecutionLocked(f.tkt)
			if point != "after ticket save" {
				before := f.h.readTask(f.tkt.ID + ".md")
				require.NoError(t, os.Chmod(f.path, 0o444))
				assert.Equal(t, before, f.reconcile(t))
				assert.Equal(t, executionOf(before), executionOf(f.tkt))
				assert.Equal(t, before.GraphState, f.tkt.GraphState)
				assert.Equal(t, before.History, f.tkt.History)
				assert.Equal(t, 1, f.d.sem.used)
				require.FileExists(t, executionPath(f.h.cfg, f.tkt.ID))
				require.NoError(t, os.Chmod(f.path, 0o644))
			}
			got := f.reconcile(t)
			assertWorkflowRecoveryHistory(t, f, got)
			assert.Equal(t, f.rec.RecoveryState, got.GraphState)
			if point == "operator move without history" {
				assert.Equal(t, ticket.StatusOpen, got.Status)
			}
			assert.Empty(t, got.InvocationID)
			assert.Zero(t, f.d.sem.used)
			require.NoFileExists(t, executionPath(f.h.cfg, got.ID))

			// A crash after the ticket write can leave the acknowledgement journal behind.
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			f.d.reserveRecoveredExecutionLocked(got)
			again := f.reconcile(t)
			assert.Equal(t, got, again)
			require.NoFileExists(t, executionPath(f.h.cfg, got.ID))
		})
	}
}
