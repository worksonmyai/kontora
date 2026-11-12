package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/web"
)

func TestLogSelectionClearsContentUntilMatchingResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		info web.TicketInfo
	}{
		{name: "graph", info: tuiGraphTicket()},
		{name: "legacy", info: testDetailTicket()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &graphSource{}
			src.logs = "legacy transcript"
			m := newModel(src)
			m.width, m.height = 160, 60
			m, initialFetch := m.openDetail(tc.info)
			initial := initialFetch().(logsMsg)
			initial.content = "first target bytes"
			applyReconcileMsg(&m, initial)
			applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
			require.Contains(t, m.detail.viewport.View(), initial.content)

			request := applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
			require.NotNil(t, request)
			assert.Empty(t, m.detail.logContent)
			assert.NotContains(t, m.detail.viewport.View(), initial.content)
			fetch := applyReconcileMsg(&m, request())
			require.NotNil(t, fetch)
			current := fetch().(logsMsg)
			current.content = "second target bytes"
			applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
			ticketView := m.detail.viewport.View()
			applyReconcileMsg(&m, initial)
			assert.Empty(t, m.detail.logContent, "an overtaken response must not populate the cache")
			applyReconcileMsg(&m, current)
			assert.Equal(t, current.content, m.detail.logContent)
			assert.Equal(t, ticketView, m.detail.viewport.View())
			assert.NotContains(t, m.detail.viewport.View(), current.content)
			applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
			assert.Contains(t, m.detail.viewport.View(), current.content)
			assert.NotContains(t, m.detail.viewport.View(), initial.content)

			m.view = viewList
			current.content = "response after leaving detail"
			applyReconcileMsg(&m, current)
			assert.NotEqual(t, current.content, m.detail.logContent)
			other := tc.info
			other.ID = "another-ticket"
			m, _ = m.openDetail(other)
			m.detail.logStage = 1
			applyReconcileMsg(&m, current)
			assert.Empty(t, m.detail.logContent, "another ticket must not receive the old transcript")
		})
	}
}

func TestLegacyLogsKeepStageFetchAndEmptyStageFallback(t *testing.T) {
	for _, stages := range [][]string{{"plan", "implement"}, nil} {
		info := testDetailTicket()
		info.Stages = stages
		m := newModel(&mockSource{logs: "legacy stage bytes"})
		m, fetch := m.openDetail(info)
		require.NotNil(t, fetch)
		response := fetch().(logsMsg)
		assert.False(t, response.target.graph)
		if len(stages) > 0 {
			assert.Equal(t, stages[0], response.target.stage)
		} else {
			assert.Empty(t, response.target.stage)
		}
		applyReconcileMsg(&m, response)
		assert.Equal(t, "legacy stage bytes", m.detail.logContent)
		m.detail.setTicket(info)
		assert.Equal(t, "legacy stage bytes", m.detail.logContent)
	}
}
