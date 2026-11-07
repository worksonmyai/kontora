package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/web"
)

func TestStageSkipRequiresPause(t *testing.T) {
	for _, view := range []viewState{viewList, viewDetail} {
		for _, status := range []string{"in_progress", "paused"} {
			t.Run(map[viewState]string{viewList: "list", viewDetail: "detail"}[view]+"/"+status, func(t *testing.T) {
				src := &mockSource{connected: true}
				m := newModel(src)
				info := testDetailTicket()
				info.Status = status
				m.view = view
				m.list.width, m.list.height = 160, 40
				m.list.setTickets([]web.TicketInfo{info}, 0)
				m.detail = newDetailModel(info, 160, 40)
				rendered := m.View()
				if status == "in_progress" {
					assert.NotContains(t, rendered, "s skip")
					assert.Contains(t, rendered, "pause first")
				} else {
					assert.Contains(t, rendered, "s skip")
				}
				updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
				if status == "in_progress" {
					require.Nil(t, cmd)
					assert.Contains(t, updated.View(), "pause first and wait for the agent to stop")
					assert.Empty(t, src.actions)
				} else {
					require.NotNil(t, cmd)
					result := cmd().(actionResultMsg)
					require.NoError(t, result.err)
					assert.Equal(t, []string{"skip:tst-001"}, src.actions)
				}
			})
		}
	}
}
