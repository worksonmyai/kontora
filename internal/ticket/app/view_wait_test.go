package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestWaitViewPreservesProvenanceAndDetachesEvidence(t *testing.T) {
	cfg := &config.Config{Pipelines: map[string]config.PipelineDefinition{}}
	tk := &ticket.Ticket{Status: ticket.StatusPaused, Stage: "work", GraphState: &ticket.GraphState{ExecutionID: "exec", VisitID: "visit", WakeCount: 2,
		Wait: &ticket.WorkflowWait{ID: "current", Receipt: &ticket.WorkflowReceipt{ID: "current-receipt"}},
		Wake: &ticket.WakeContext{Count: 2}, WaitHistory: []ticket.WorkflowWait{{ID: "past", Receipt: &ticket.WorkflowReceipt{ID: "past-receipt"}}}},
		History: []ticket.HistoryEntry{
			{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "visit", RunOutcome: ticket.RunOutcomeWaiting, NextStep: "continue", Wait: &ticket.WorkflowWait{ID: "past"}},
			{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "visit", Continuation: true, RunOutcome: ticket.RunOutcomeFailed, Wake: &ticket.WakeContext{Count: 1, Receipt: &ticket.WorkflowReceipt{ID: "wake-receipt"}}},
			{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "visit", Verdict: "ok", RunOutcome: ticket.RunOutcomeComplete},
		},
	}
	list := BuildView(cfg, tk, false)
	require.False(t, list.ExecutionActive)
	require.NotNil(t, list.GraphExecution.Wait)
	require.Empty(t, list.GraphExecution.WaitHistory)
	require.Empty(t, list.History)
	detail := BuildView(cfg, tk, true)
	require.Equal(t, []int{1, 1, 2}, []int{detail.History[0].VisitAttempt, detail.History[1].VisitAttempt, detail.History[2].VisitAttempt})
	require.Equal(t, 1, detail.GraphExecution.Attempt)
	require.Equal(t, "continue", detail.History[0].NextStep)
	require.Equal(t, "ok", detail.History[2].Verdict)
	require.True(t, detail.History[1].Continuation)
	detail.GraphExecution.Wait.Receipt.ID = "changed"
	detail.GraphExecution.WaitHistory[0].Receipt.ID = "changed"
	detail.History[1].Wake.Receipt.ID = "changed"
	require.Equal(t, "current-receipt", tk.GraphState.Wait.Receipt.ID)
	require.Equal(t, "past-receipt", tk.GraphState.WaitHistory[0].Receipt.ID)
	require.Equal(t, "wake-receipt", tk.History[1].Wake.Receipt.ID)
}

func TestWaitingContinuationDoesNotAdvanceCurrentAttempt(t *testing.T) {
	tk := &ticket.Ticket{Status: ticket.StatusTodo, GraphState: &ticket.GraphState{ExecutionID: "exec", VisitID: "visit", Continuation: true}, History: []ticket.HistoryEntry{{WorkflowExecutionID: "exec", NodeVisitID: "visit", RunOutcome: ticket.RunOutcomeWaiting}}}
	require.Zero(t, graphVisitAttempt(tk))
	tk.GraphState.Continuation = false
	tk.Status = ticket.StatusInProgress
	tk.GraphState.Wake = &ticket.WakeContext{WaitID: "wait"}
	tk.History[0].Wait = &ticket.WorkflowWait{ID: "wait"}
	require.Zero(t, graphVisitAttempt(tk), "binding consumes the flag but does not create a retry")
	require.True(t, graphVisitContinuation(tk))
	tk.Status = ticket.StatusTodo
	tk.History = append(tk.History, ticket.HistoryEntry{WorkflowExecutionID: "exec", NodeVisitID: "visit", Continuation: true, RunOutcome: ticket.RunOutcomeFailed})
	require.Equal(t, 1, graphVisitAttempt(tk))
}

func TestWaitGraphDefinitionProjection(t *testing.T) {
	graph := &config.WorkflowGraph{Nodes: []config.WorkflowNode{
		{ID: "work", Type: "agent", Wake: &config.WorkflowWake{Max: 3, CheckTimeout: config.Duration{Duration: time.Minute}}},
		{ID: "deploy", Type: "wait", Mode: "deployment", Environment: "production", Timeout: config.Duration{Duration: time.Hour}},
	}}
	view := BuildGraphDefinition(graph)
	require.Equal(t, 3, view.Nodes[0].Wake.Max)
	require.Equal(t, "1m0s", view.Nodes[0].Wake.CheckTimeout)
	require.Equal(t, "deployment", view.Nodes[1].Mode)
	require.Equal(t, "production", view.Nodes[1].Environment)
	require.Equal(t, "1h0m0s", view.Nodes[1].Timeout)
}
