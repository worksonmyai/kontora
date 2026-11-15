package app

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestReview17DeclaredReleaseCountsFirstInvocationOfNewVisit(t *testing.T) {
	cfg := &config.Config{}
	tk := &ticket.Ticket{Stage: "work", Status: ticket.StatusTodo,
		GraphState: &ticket.GraphState{ExecutionID: "exec", VisitID: "new-visit", Continuation: true},
		History: []ticket.HistoryEntry{
			{Stage: "work", WorkflowExecutionID: "old-exec", NodeVisitID: "new-visit"},
			{Stage: "approval", WorkflowExecutionID: "exec", NodeVisitID: "wait-visit", Kind: "wait_release"},
			{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "new-visit", Kind: "annotation"},
		},
	}
	require.Zero(t, BuildView(cfg, tk, true).GraphExecution.Attempt)
	tk.GraphState.Continuation = false
	tk.History = append(tk.History, ticket.HistoryEntry{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "new-visit", Continuation: true, RunOutcome: ticket.RunOutcomeBlocked})
	for _, tc := range []struct {
		status  ticket.Status
		attempt int
	}{
		{ticket.StatusPaused, 0},
		{ticket.StatusTodo, 1},
		{ticket.StatusInProgress, 1},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			tk.Status = tc.status
			for _, detail := range []bool{false, true} {
				v := BuildView(cfg, tk, detail)
				require.Equal(t, tc.attempt, v.GraphExecution.Attempt)
				if detail {
					require.Equal(t, 1, v.History[3].VisitAttempt)
				}
			}
		})
	}
	tk.Status = ticket.StatusPaused
	tk.History = append(tk.History, ticket.HistoryEntry{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "new-visit", RunOutcome: ticket.RunOutcomeBlocked})
	v := BuildView(cfg, tk, true)
	require.Equal(t, 1, v.GraphExecution.Attempt)
	require.Equal(t, 2, v.History[4].VisitAttempt)

	tk.Status = ticket.StatusTodo
	tk.GraphState.Continuation = true
	tk.History[4].RunOutcome = ticket.RunOutcomeWaiting
	tk.History = append(tk.History, ticket.HistoryEntry{Stage: "work", WorkflowExecutionID: "exec", NodeVisitID: "new-visit", Continuation: true, RunOutcome: ticket.RunOutcomeWaiting})
	v = BuildView(cfg, tk, true)
	require.Equal(t, 1, v.GraphExecution.Attempt, "same-visit continuations do not consume attempts")
	require.Equal(t, 2, v.History[5].VisitAttempt)
}
