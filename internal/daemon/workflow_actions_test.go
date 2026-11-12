package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func workflowActionFixture(t *testing.T) *resultFixture {
	t.Helper()
	f := newWorkflowRecoveryFixture(t)
	graph := &config.WorkflowGraph{Version: 2, Entry: "step2", Nodes: []config.WorkflowNode{
		{ID: "step2", Type: "agent", Stage: "step1", Agent: "agent1"},
		{ID: "review-loop", Type: "loop", Max: 3},
		{ID: "stop", Type: "end", Status: "paused"},
	}, Edges: []config.WorkflowEdge{
		{From: config.WorkflowPort{Node: "step2", Port: "complete"}, To: "review-loop"},
		{From: config.WorkflowPort{Node: "step2", Port: "failed"}, To: "stop"},
		{From: config.WorkflowPort{Node: "review-loop", Port: "repeat"}, To: "step2"},
		{From: config.WorkflowPort{Node: "review-loop", Port: "exhausted"}, To: "stop"},
	}}
	f.h.cfg.Pipelines[f.tkt.Pipeline] = config.PipelineDefinition{Graph: graph}
	fingerprint, err := workflowFingerprint(f.h.cfg, f.tkt, graph)
	require.NoError(t, err)
	state := f.tkt.GraphState.Clone()
	state.DefinitionFingerprint = fingerprint
	state.VisitID = state.ExecutionID + ".4"
	require.NoError(t, f.tkt.SetField("graph_state", state))
	require.NoError(t, f.tkt.SetField("status", "paused"))
	f.rec.GraphBefore = state.Clone()
	f.rec.Invocation.DefinitionFingerprint = fingerprint
	f.rec.Invocation.NodeVisitID = state.VisitID
	f.rec.Phase = "recovery_required"
	f.write(t)
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	return f
}

func TestWorkflowRecoverAction(t *testing.T) {
	for _, change := range []string{"acknowledge", "stale execution", "stale revision", "changed configuration", "replacement invocation", "live invocation"} {
		t.Run(change, func(t *testing.T) {
			f := workflowActionFixture(t)
			f.d.reserveRecoveredExecutionLocked(f.tkt)
			executionID, revision := f.tkt.GraphState.ExecutionID, f.tkt.GraphState.Revision
			switch change {
			case "stale execution":
				executionID = "old-execution"
			case "stale revision":
				revision--
			case "changed configuration":
				f.h.cfg.SystemPrompt += " changed"
			case "replacement invocation":
				require.NoError(t, f.tkt.SetField("invocation_id", "replacement"))
				f.write(t)
			case "live invocation":
				delete(f.d.recoveredReservations, f.tkt.ID)
			}
			before := f.h.readTask(f.tkt.ID + ".md")
			err := f.d.WorkflowRecover(f.tkt.ID, executionID, revision)
			got := f.h.readTask(f.tkt.ID + ".md")
			if change != "acknowledge" {
				require.ErrorIs(t, err, web.ErrInvalidState)
				assert.Equal(t, before, got)
				assert.Equal(t, 1, f.d.sem.used)
				require.FileExists(t, executionPath(f.h.cfg, got.ID))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, before.GraphState.ExecutionID, got.GraphState.ExecutionID)
			assert.Equal(t, before.GraphState.LoopCounts, got.GraphState.LoopCounts)
			assert.Equal(t, revision+1, got.GraphState.Revision)
			assert.Equal(t, ticket.StatusPaused, got.Status)
			assert.Empty(t, got.InvocationID)
			assert.Empty(t, f.d.runningBranches)
			assert.Empty(t, f.d.recoveredReservations)
			assert.Zero(t, f.d.sem.used)
			require.NoFileExists(t, executionPath(f.h.cfg, got.ID))
			require.ErrorIs(t, f.d.WorkflowRecover(got.ID, executionID, revision), web.ErrInvalidState)
		})
	}
}

func TestWorkflowRecoveryAcknowledgementReplay(t *testing.T) {
	for _, point := range []string{"before ticket save", "after ticket save", "operator move after save"} {
		t.Run(point, func(t *testing.T) {
			f := workflowActionFixture(t)
			f.rec.Phase = "acknowledged"
			f.rec.HookTermination = "pending"
			f.rec.RecoveryState = f.tkt.GraphState.Clone()
			f.rec.RecoveryState.Revision++
			if point != "before ticket save" {
				require.NoError(t, installRecoveryAcknowledgement(f.tkt, f.rec))
				if point == "operator move after save" {
					require.NoError(t, f.tkt.SetField("status", "open"))
				}
				f.write(t)
			}
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			f.d.reserveRecoveredExecutionLocked(f.tkt)
			got := f.reconcile(t)
			assert.Equal(t, f.rec.RecoveryState, got.GraphState)
			if point == "operator move after save" {
				assert.Equal(t, ticket.StatusOpen, got.Status)
			}
			assert.Empty(t, got.InvocationID)
			assert.Empty(t, f.d.runningBranches)
			assert.Zero(t, f.d.sem.used)
			require.NoFileExists(t, executionPath(f.h.cfg, got.ID))
		})
	}
}

func TestWorkflowRestartAction(t *testing.T) {
	for _, mode := range []string{"stopped workflow", "unresolved interruption", "terminal entry"} {
		t.Run(mode, func(t *testing.T) {
			f := workflowActionFixture(t)
			interrupted := mode == "unresolved interruption"
			if mode == "terminal entry" {
				f.h.cfg.Pipelines[f.tkt.Pipeline] = config.PipelineDefinition{Graph: &config.WorkflowGraph{Version: 2, Entry: "done", Nodes: []config.WorkflowNode{{ID: "done", Type: "end", Status: "done"}}}}
			}
			if !interrupted {
				require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
				require.NoError(t, f.tkt.SetField("invocation_id", ""))
				require.NoError(t, f.tkt.SetField("stage", "stop"))
				state := f.tkt.GraphState.Clone()
				state.Transitions = []ticket.GraphTransition{{From: "review-loop", Port: "exhausted", To: "stop", LoopCount: 3, Revision: 7}}
				require.NoError(t, f.tkt.SetField("graph_state", state))
				f.write(t)
			}
			before := f.h.readTask(f.tkt.ID + ".md")
			err := f.d.WorkflowRestart(f.tkt.ID)
			got := f.h.readTask(f.tkt.ID + ".md")
			if interrupted {
				require.ErrorIs(t, err, web.ErrInvalidState)
				assert.Equal(t, before, got)
				return
			}
			require.NoError(t, err)
			assert.NotEqual(t, before.GraphState.ExecutionID, got.GraphState.ExecutionID)
			assert.Empty(t, got.GraphState.LoopCounts)
			if mode == "terminal entry" {
				assert.Equal(t, "done", got.Stage)
				assert.Equal(t, ticket.StatusDone, got.Status)
				assert.NotNil(t, got.CompletedAt)
				assert.Empty(t, f.d.queued)
			} else {
				assert.Equal(t, "step2", got.Stage)
				assert.Equal(t, ticket.StatusTodo, got.Status)
				assert.Equal(t, map[string]string{"step2": "step1"}, got.GraphState.NodeTemplates)
				assert.Equal(t, map[string]int{"review-loop": 3}, got.GraphState.LoopLimits)
			}
			assert.Equal(t, before.History, got.History)
			assert.Equal(t, before.Branch, got.Branch)
			require.Len(t, got.GraphState.PreviousExecutions, 1)
			assert.Equal(t, before.GraphState.Transitions, got.GraphState.PreviousExecutions[0].Transitions)
			assert.NotEqual(t, graphLogPath(f.h.cfg, before), graphLogPath(f.h.cfg, got))
		})
	}
}

func TestWorkflowUnsupportedAutomation(t *testing.T) {
	for _, action := range []string{"annotate", "claim annotate", "park annotate", "rework"} {
		t.Run(action, func(t *testing.T) {
			f := workflowActionFixture(t)
			require.NoError(t, f.tkt.SetField("status", "open"))
			f.write(t)
			f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
			before := f.h.readTask(f.tkt.ID + ".md")
			var err error
			switch action {
			case "annotate":
				err = f.d.StartPlannotatorAnnotate(f.tkt.ID)
			case "claim annotate":
				_, err = f.d.claimAnnotateSession(f.tkt.ID, func() {})
			case "park annotate":
				err = f.d.parkForAnnotation(f.tkt.ID)
			case "rework":
				err = f.d.transitionToRework(f.tkt.ID)
			}
			require.ErrorIs(t, err, web.ErrInvalidState)
			assert.Equal(t, before, f.h.readTask(f.tkt.ID+".md"))
		})
	}
}
