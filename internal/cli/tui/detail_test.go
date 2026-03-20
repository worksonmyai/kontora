package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/worksonmyai/kontora/internal/web"
)

func testDetailTicket() web.TicketInfo {
	return web.TicketInfo{
		ID:       "tst-001",
		Title:    "Test Ticket",
		Status:   "in_progress",
		Pipeline: "default",
		Path:     "~/projects/test",
		Branch:   "kontora/tst-001",
		Stage:    "implement",
		Agent:    "claude",
		Stages:   []string{"plan", "implement", "review"},
		Body:     "This is the ticket body.\nLine 2.\nLine 3.",
	}
}

func TestDetailModel_TabSwitching(t *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)
	assert.Equal(t, tabTicket, m.tab)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	assert.Equal(t, tabLogs, m.tab)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	assert.Equal(t, tabTicket, m.tab)
}

func TestDetailModel_SetLogs(t *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)
	m.setLogs("log content here")
	assert.Equal(t, "log content here", m.logContent)

	// Switch to logs tab to see the content
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	assert.Equal(t, tabLogs, m.tab)
}

func TestDetailModel_LogCycle(t *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)
	assert.Equal(t, 0, m.logStage)

	// Cycle through stages
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 1, m.logStage) // implement
	assert.NotNil(t, cmd)          // should request log fetch

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 2, m.logStage) // review

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 0, m.logStage) // wraps back to plan
}

func TestDetailModel_LogCycleEmpty(t *testing.T) {
	info := web.TicketInfo{ID: "tst-001", Status: "open"}
	m := newDetailModel(info, 100, 30)

	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 0, m.logStage)
	assert.Nil(t, cmd) // no stages, no fetch
}

func TestDetailModel_ScrollKeys(_ *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)

	// These shouldn't panic
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	_ = m
}

func TestDetailModel_SetTask(t *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)

	updated := testDetailTicket()
	updated.Status = "paused"
	updated.Title = "Updated Title"
	m.setTicket(updated)

	assert.Equal(t, "paused", m.ticket.Status)
	assert.Equal(t, "Updated Title", m.ticket.Title)
}

func TestDetailModel_SetTaskClampsLogStage(t *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)
	// Advance logStage to last stage (index 2 of 3 stages)
	m.logStage = 2

	// Update with fewer stages — logStage must be clamped
	updated := testDetailTicket()
	updated.Stages = []string{"plan"}
	m.setTicket(updated)

	assert.Equal(t, 0, m.logStage)
	assert.Len(t, m.logStages, 1)
}

func TestDetailModel_SetTaskClampsLogStageToZeroWhenEmpty(t *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)
	m.logStage = 2

	updated := testDetailTicket()
	updated.Stages = nil
	m.setTicket(updated)

	assert.Equal(t, 0, m.logStage)
}

func TestDetailModel_SetSize(t *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)
	m.setSize(120, 40)
	assert.Equal(t, 120, m.width)
	assert.Equal(t, 40, m.height)
	assert.Equal(t, 120, m.viewport.Width)
}

func TestDetailModel_View(t *testing.T) {
	m := newDetailModel(testDetailTicket(), 100, 30)
	view := m.View()

	assert.Contains(t, view, "tst-001")
	assert.Contains(t, view, "in_progress")
	assert.Contains(t, view, "Test Ticket")
	assert.Contains(t, view, "default")     // pipeline
	assert.Contains(t, view, "[implement]") // current stage bracketed
	assert.Contains(t, view, "esc back")
	assert.Contains(t, view, "p pause")
}

func TestDetailModel_ViewShowsAgent(t *testing.T) {
	info := testDetailTicket()
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.Contains(t, view, "agent")
	assert.Contains(t, view, "claude")
}

func TestDetailModel_ViewAgentOverride(t *testing.T) {
	info := testDetailTicket()
	info.AgentOverride = true
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.Contains(t, view, "claude")
	assert.Contains(t, view, "(override)")
}

func TestDetailModel_ViewNoOverrideLabel(t *testing.T) {
	info := testDetailTicket()
	info.AgentOverride = false
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.Contains(t, view, "claude")
	assert.NotContains(t, view, "(override)")
}

func TestDetailModel_ViewStatusBarShowsAgentKey(t *testing.T) {
	info := testDetailTicket()
	info.Status = "todo"
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.Contains(t, view, "g agent")
}

func TestDetailModel_ViewStatusBarHidesAgentKeyInProgress(t *testing.T) {
	info := testDetailTicket()
	info.Status = "in_progress"
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.NotContains(t, view, "g agent")
}

func TestDetailModel_ViewStatusBarHidesAgentKeyDone(t *testing.T) {
	info := testDetailTicket()
	info.Status = "done"
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.NotContains(t, view, "g agent")
}

func TestDetailModel_ViewDoneTask(t *testing.T) {
	info := testDetailTicket()
	info.Status = "done"
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.Contains(t, view, "r retry")
	assert.NotContains(t, view, "p pause")
}

func TestDetailModel_ViewSimpleTask(t *testing.T) {
	info := web.TicketInfo{
		ID:      "sim-001",
		Title:   "Simple Ticket",
		Status:  "done",
		Kontora: true,
		Path:    "~/projects/test",
		Branch:  "kontora/sim-001",
		Stage:   "default",
		Stages:  []string{"default"},
		Body:    "Simple ticket body.",
	}
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.Contains(t, view, "branch")
	assert.Contains(t, view, "kontora/sim-001")
	assert.Equal(t, []string{"default"}, m.logStages)
}

func TestDetailModel_ViewSimpleTaskEmptyBranch(t *testing.T) {
	info := web.TicketInfo{
		ID:      "sim-002",
		Title:   "Simple No Branch",
		Status:  "todo",
		Kontora: true,
		Stages:  []string{"default"},
	}
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.Contains(t, view, "branch")
	assert.Contains(t, view, "—") // empty branch shows dash
}

func TestDetailModel_ViewLastError(t *testing.T) {
	info := testDetailTicket()
	info.Status = "paused"
	info.LastError = "agent exited with code 1 (stage: implement)"
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.Contains(t, view, "agent exited with code 1 (stage: implement)")
}

func TestDetailModel_ViewNoLastError(t *testing.T) {
	info := testDetailTicket()
	info.LastError = ""
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.NotContains(t, view, "⚠")
}

func TestDetailModel_ViewNonKontoraNoBranch(t *testing.T) {
	info := web.TicketInfo{
		ID:     "ext-001",
		Title:  "External Ticket",
		Status: "open",
	}
	m := newDetailModel(info, 100, 30)
	view := m.View()

	assert.NotContains(t, view, "branch")
}
