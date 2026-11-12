package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/web"
)

func TestGraphLogsRefreshTracksTargetIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		update   func(*web.TicketInfo)
		selected int
		retained bool
	}{
		{
			name: "unchanged target",
			update: func(info *web.TicketInfo) {
				info.Title = "Updated title"
				info.GraphExecution.Revision++
			},
			selected: 1, retained: true,
		},
		{
			name: "target moved in history",
			update: func(info *web.TicketInfo) {
				info.History = info.History[1:]
			},
			selected: 0, retained: true,
		},
		{
			name: "different visit at the same index and run",
			update: func(info *web.TicketInfo) {
				info.History[1].NodeVisitID = "new-execution.1"
			},
		},
		{
			name: "different attempt at the same index and run",
			update: func(info *web.TicketInfo) {
				info.History[1].VisitAttempt++
			},
		},
		{
			name: "no targets remain",
			update: func(info *web.TicketInfo) {
				info.History = nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := tuiGraphTicket()
			m := newDetailModel(info, 160, 60)
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
			m.setLogs("second attempt bytes")
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
			tc.update(&info)
			m.setTicket(info)
			assert.Equal(t, tc.selected, m.logStage)
			if tc.retained {
				assert.Equal(t, "second attempt bytes", m.logContent)
				assert.Contains(t, m.viewport.View(), "second attempt bytes")
				assert.Contains(t, m.View(), "visit exec.1 · attempt 2")
			} else {
				assert.Empty(t, m.logContent)
				assert.NotContains(t, m.viewport.View(), "second attempt bytes")
			}
		})
	}
}

func TestGraphLogsCompletedLiveVisitKeepsMatchingContent(t *testing.T) {
	info := tuiGraphTicket()
	info.ExecutionActive = true
	info.GraphExecution.VisitID = "exec.3"
	info.GraphExecution.Attempt = 1
	m := newDetailModel(info, 160, 60)
	for range 3 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	}
	m.setLogs("completed live attempt bytes")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	info.ExecutionActive = false
	info.History = append(info.History, web.HistoryInfo{Stage: "work", Run: 3, NodeVisitID: "exec.3", VisitAttempt: 2})
	m.setTicket(info)
	assert.Equal(t, 3, m.logStage)
	assert.Equal(t, "completed live attempt bytes", m.logContent)
	assert.Contains(t, m.View(), "visit exec.3 · attempt 2]")
	assert.Contains(t, m.viewport.View(), m.logContent)
}

func TestGraphLogsRejectReplacedTargetAsyncMessages(t *testing.T) {
	for _, replacement := range []string{"visit", "attempt"} {
		t.Run(replacement, func(t *testing.T) {
			src := &graphSource{}
			m := newModel(src)
			info := tuiGraphTicket()
			m, fetch := m.openDetail(info)
			oldResponse := fetch().(logsMsg)
			oldResponse.content = "old target bytes"
			oldRequest := fetchLogsRequest{id: oldResponse.id, target: oldResponse.target}
			applyReconcileMsg(&m, oldResponse)
			if replacement == "visit" {
				info.History[0].NodeVisitID = "new-execution.1"
			} else {
				info.History[0].VisitAttempt++
			}
			m.detail.setTicket(info)
			require.Zero(t, m.detail.logStage)
			require.Empty(t, m.detail.logContent)
			assert.Nil(t, applyReconcileMsg(&m, oldRequest), "do not fetch a removed target")
			applyReconcileMsg(&m, oldResponse)
			assert.Empty(t, m.detail.logContent)
			applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
			assert.NotContains(t, m.detail.View(), oldResponse.content)
			current := m.fetchLogsCmd(info.ID, m.detail.selectedLogTarget())().(logsMsg)
			applyReconcileMsg(&m, current)
			assert.Equal(t, current.content, m.detail.logContent)
		})
	}
}

func TestGraphLogsUncertainTerminationClearsRemovedLiveTarget(t *testing.T) {
	for _, tab := range []detailTab{tabLogs, tabTicket} {
		t.Run(map[detailTab]string{tabLogs: "logs tab", tabTicket: "ticket tab"}[tab], func(t *testing.T) {
			info := tuiGraphTicket()
			info.ExecutionActive = true
			info.GraphExecution.VisitID = "exec.3"
			src := &graphSource{}
			m := newModel(src)
			m.width, m.height = 160, 60
			m, _ = m.openDetail(info)
			for range 2 {
				runReconcileCmd(&m, applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}))
			}
			request := applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
			fetch := applyReconcileMsg(&m, request())
			require.NotNil(t, fetch)
			live := fetch().(logsMsg)
			live.content = "bytes from uncertain live visit"
			applyReconcileMsg(&m, live)
			applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
			require.Contains(t, m.detail.View(), "visit exec.3 · attempt 1 · live")
			require.Contains(t, m.detail.viewport.View(), live.content)
			if tab == tabTicket {
				applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
			}

			// Uncertain termination stops execution without appending a history entry.
			info.ExecutionActive = false
			m.detail.setTicket(info)
			assert.Zero(t, m.detail.logStage)
			assert.Empty(t, m.detail.logContent)
			assert.NotContains(t, m.detail.viewport.View(), live.content)
			applyReconcileMsg(&m, live)
			assert.Empty(t, m.detail.logContent, "a late live response must not refill the historical selection")
			applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
			assert.Contains(t, m.detail.View(), "visit exec.1 · attempt 1")
			assert.NotContains(t, m.detail.View(), live.content)
		})
	}
}
