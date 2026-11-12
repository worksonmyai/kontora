package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestCustomActivityControls(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, view := range []viewState{viewList, viewDetail} {
			t.Run(fmt.Sprintf("active=%t/view=%d", active, view), func(t *testing.T) {
				src := &mockSource{connected: true}
				m := newModel(src)
				info := testDetailTicket()
				info.Status, info.ExecutionActive = "awaiting_data", active
				m.view = view
				m.list.customStatuses = []string{"awaiting_data"}
				m.list.width, m.list.height = 160, 40
				m.list.setTickets([]web.TicketInfo{info}, 0)
				m.detail = newDetailModel(info, 160, 40)
				assert.Equal(t, !active, canSkipStage(info))
				updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
				if active {
					require.Nil(t, cmd)
					assert.Contains(t, updated.View(), "pause first")
					assert.Empty(t, src.actions)
				} else {
					require.NotNil(t, cmd)
					require.NoError(t, cmd().(actionResultMsg).err)
					assert.Equal(t, []string{"skip:tst-001"}, src.actions)
				}
				if view == viewDetail && active {
					m.detail.agents = []string{"", "claude"}
					_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
					assert.Nil(t, cmd)
				}
			})
		}
	}
}

func TestCustomActivityColumnsAndSameStatusEvent(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	active := web.TicketInfo{ID: "active", Title: "Active", Status: "awaiting_data", Kontora: true, ExecutionActive: true, StartedAt: &started}
	parked := web.TicketInfo{ID: "parked", Title: "Parked", Status: "awaiting_data", Kontora: true, StartedAt: &started}
	passive := web.TicketInfo{ID: "epic", Status: "in_progress", Kontora: true, Kind: "epic"}
	secondary := web.TicketInfo{ID: "secondary", Status: "awaiting_data", Kontora: true, Source: "other", ExecutionActive: true}
	m := newModel(&mockSource{connected: true})
	m.list.setTickets([]web.TicketInfo{active, parked, passive, secondary}, 0)
	m.countRunning()
	assert.Equal(t, 1, m.list.running)
	assert.Empty(t, m.list.customStatuses, "fallback must not alter configured destinations")
	require.Len(t, m.list.columns, 2)
	assert.Equal(t, "awaiting_data", string(m.list.columns[1].status))
	assert.Len(t, m.list.columns[1].tickets, 3)
	assert.NotEqual(t, "—", ticketDuration(active))
	assert.Equal(t, "—", ticketDuration(parked))
	assert.Equal(t, "—", ticketDuration(passive))

	m.list.customStatuses = []string{"awaiting_data"}
	m.view = viewDetail
	m.detail = newDetailModel(active, 160, 40)
	active.ExecutionActive = false
	m, _ = m.handleTaskUpdated(web.TicketEvent{Type: "ticket_updated", Ticket: active})
	assert.False(t, m.detail.ticket.ExecutionActive)
	assert.False(t, m.detail.ticket.IsRunning())
	assert.Equal(t, "awaiting_data", m.detail.ticket.Status)
	assert.Equal(t, 0, m.list.running)
	assert.Equal(t, "—", ticketDuration(m.detail.ticket))
}

func TestFileSourceCustomActivity(t *testing.T) {
	dir := t.TempDir()
	for id, fields := range map[string]string{
		"active": "status: awaiting_data\nrunning_status: awaiting_data\n",
		"parked": "status: awaiting_data\n",
		"epic":   "status: in_progress\nkind: epic\n",
	} {
		content := "---\nid: " + id + "\nkontora: true\n" + fields + "---\n# Work\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), []byte(content), 0o644))
	}
	src := &fileSource{cfg: &config.Config{TicketsDir: dir, LogsDir: t.TempDir()}}
	infos, running, err := src.FetchTickets()
	require.NoError(t, err)
	assert.Equal(t, 1, running)
	require.Len(t, infos, 3)
	for _, info := range infos {
		assert.Equal(t, info.ID == "active", info.ExecutionActive)
		detail, err := src.FetchTask(info.ID)
		require.NoError(t, err)
		assert.Equal(t, info.ExecutionActive, detail.ExecutionActive)
		assert.Equal(t, info.Status, detail.Status)
	}
}
