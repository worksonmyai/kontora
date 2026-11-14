package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestDurableWaitDetailDiagnosticsAndReleaseInstructions(t *testing.T) {
	info := tuiGraphTicket()
	info.History[0].RunOutcome = "waiting"
	info.History[1].VisitAttempt = 1
	info.GraphExecution.WakeCount = 2
	info.GraphExecution.Wait = &ticket.WorkflowWait{ID: "wait", Kind: "human", Phase: "pending", Deadline: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), PollCount: 3, Output: "pending sign-off", Next: "Resume review"}
	info.GraphExecution.WaitHistory = []ticket.WorkflowWait{{ID: "past", Kind: "timer", Receipt: &ticket.WorkflowReceipt{ID: "accepted"}}}
	detail := newDetailModel(info, 160, 70)
	require.True(t, isWaiting(info))
	require.Contains(t, detail.logTargets[0].label, "Waiting")
	require.Contains(t, detail.logTargets[1].label, "attempt 1 · continuation")
	require.Contains(t, detail.View(), "Waiting")
	content := detail.ticketContent()
	for _, expected := range []string{"polls 3", "pending sign-off", "deadline: 2026-01-01", "wake count: 2", "Resume review", "kontora release graph --execution-id exec --wait-id wait", "historical wait: past", "receipt: accepted"} {
		require.Contains(t, content, expected)
	}
	require.NotContains(t, detail.hints(), "r retry")
	source := &graphSource{}
	source.connected = true
	m := newModel(source)
	m.view, m.detail = viewDetail, detail
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	require.Nil(t, cmd)
	require.ErrorContains(t, next.(model).detail.err, "workflow is waiting")
}
