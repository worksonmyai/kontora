package tui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/web"
)

func testTickets() []web.TicketInfo {
	return []web.TicketInfo{
		{ID: "tst-001", Title: "In progress ticket", Status: "in_progress", Kontora: true, Stage: "plan", Agent: "claude"},
		{ID: "tst-002", Title: "Todo ticket", Status: "todo", Kontora: true, Stage: "implement", Agent: "claude"},
		{ID: "tst-003", Title: "Done ticket", Status: "done", Kontora: true, Stage: "review", Agent: "claude"},
	}
}

func TestListModel_CursorNavigation(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets(testTickets(), 1)

	// With testTickets: todo(1), in_progress(1), done(1 hidden)
	// Columns: todo, in_progress — colIdx=0, rowIdx=0
	assert.Equal(t, 0, m.colIdx)
	assert.Equal(t, 0, m.rowIdx)

	// j does nothing in a single-item column
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	assert.Equal(t, 0, m.rowIdx)

	// l moves to next column
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 1, m.colIdx)

	// Can't go past last column
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 1, m.colIdx)

	// h moves back
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	assert.Equal(t, 0, m.colIdx)

	// Can't go before first column
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	assert.Equal(t, 0, m.colIdx)
}

func TestListModel_Selected(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets(testTickets(), 1)

	// First column (todo), first ticket
	sel := m.selected()
	require.NotNil(t, sel)
	assert.Equal(t, "tst-002", sel.ID) // todo ticket

	// Move to in_progress column
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	sel = m.selected()
	require.NotNil(t, sel)
	assert.Equal(t, "tst-001", sel.ID) // in_progress ticket
}

func TestListModel_EmptySelected(t *testing.T) {
	m := newListModel()
	assert.Nil(t, m.selected())
}

func TestListModel_Filter(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets(testTickets(), 1)

	assert.Len(t, m.filtered, 3)

	// Enter filter mode
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	assert.True(t, m.filtering)

	// Type "progress"
	for _, r := range "progress" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	assert.Equal(t, "progress", m.filter)
	assert.Len(t, m.filtered, 1)
	assert.Equal(t, "tst-001", m.filtered[0].ID)

	// Only one column (in_progress)
	assert.Len(t, m.columns, 1)
	assert.Equal(t, "in_progress", string(m.columns[0].status))

	// Esc clears filter
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, m.filtering)
	assert.Equal(t, "", m.filter)
	assert.Len(t, m.filtered, 3)
}

func TestListModel_FilterBackspace(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets(testTickets(), 1)
	m.filtering = true
	m.filter = "progress"
	m.applyFilter()

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	assert.Equal(t, "progres", m.filter)
}

func TestListModel_SortOrder(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	tasks := []web.TicketInfo{
		{ID: "tst-003", Title: "Done", Status: "done", Kontora: true},
		{ID: "tst-001", Title: "In Progress", Status: "in_progress", Kontora: true},
		{ID: "tst-002", Title: "Todo", Status: "todo", Kontora: true},
	}
	m.setTickets(tasks, 1)

	// Filtered still sorted by status rank
	assert.Equal(t, "tst-001", m.filtered[0].ID)
	assert.Equal(t, "tst-002", m.filtered[1].ID)
	assert.Equal(t, "tst-003", m.filtered[2].ID)
}

func TestListModel_VirtualScroll(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 10 // very small — forces scrolling

	tasks := make([]web.TicketInfo, 0, 20)
	for i := range 20 {
		tasks = append(tasks, web.TicketInfo{
			ID:      fmt.Sprintf("tst-%03d", i),
			Title:   fmt.Sprintf("Ticket %d", i),
			Status:  "todo",
			Kontora: true,
		})
	}
	m.setTickets(tasks, 0)

	// All in one "todo" column
	require.Len(t, m.columns, 1)

	for range 15 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	assert.Equal(t, 15, m.rowIdx)
	assert.True(t, m.columns[0].offset > 0)
}

func TestListModel_GoToTopBottom(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30

	tasks := []web.TicketInfo{
		{ID: "tst-001", Title: "A", Status: "todo", Kontora: true},
		{ID: "tst-002", Title: "B", Status: "todo", Kontora: true},
		{ID: "tst-003", Title: "C", Status: "todo", Kontora: true},
	}
	m.setTickets(tasks, 0)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	assert.Equal(t, 2, m.rowIdx)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	assert.Equal(t, 0, m.rowIdx)
}

func TestListModel_UpdateTicket(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets(testTickets(), 1)

	m.updateTicket(web.TicketInfo{ID: "tst-001", Title: "Updated", Status: "paused", Kontora: true})
	for _, ti := range m.filtered {
		if ti.ID == "tst-001" {
			assert.Equal(t, "Updated", ti.Title)
			assert.Equal(t, "paused", ti.Status)
		}
	}

	m.updateTicket(web.TicketInfo{ID: "tst-004", Title: "New task", Status: "todo", Kontora: true})
	assert.Len(t, m.filtered, 4)
}

func TestListModel_NonKontoraVisibility(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets([]web.TicketInfo{
		{ID: "ext-001", Title: "External open", Status: "open", Kontora: false},
		{ID: "ext-002", Title: "External todo", Status: "todo", Kontora: false},
		{ID: "kon-001", Title: "Kontora todo", Status: "todo", Kontora: true},
	}, 0)

	assert.Len(t, m.filtered, 2)
	assert.Equal(t, "kon-001", m.filtered[0].ID)
	assert.Equal(t, "ext-001", m.filtered[1].ID)

	m.updateTicket(web.TicketInfo{ID: "ext-001", Title: "External open", Status: "done", Kontora: false})
	assert.Len(t, m.filtered, 1)
	assert.Equal(t, "kon-001", m.filtered[0].ID)
}

func TestListModel_CtrlCInFilterMode(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets(testTickets(), 1)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	assert.True(t, m.filtering)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.NotNil(t, cmd)
}

func TestListModel_View(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.connected = true
	m.setTickets(testTickets(), 1)

	view := m.View()
	assert.Contains(t, view, "kontora")
	assert.Contains(t, view, "1 running")
	assert.Contains(t, view, "connected")
	assert.Contains(t, view, "tst-001")
	assert.Contains(t, view, "In progress ticket")
	assert.Contains(t, view, "Todo")
	assert.Contains(t, view, "In Progress")
	assert.Contains(t, view, "│")
}

func TestListModel_BuildColumns(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets([]web.TicketInfo{
		{ID: "a", Status: "in_progress", Kontora: true},
		{ID: "b", Status: "todo", Kontora: true},
		{ID: "c", Status: "paused", Kontora: true},
		{ID: "d", Status: "todo", Kontora: true},
		{ID: "e", Status: "done", Kontora: true},
	}, 1)

	// Done hidden by default
	assert.Len(t, m.columns, 3) // todo, in_progress, paused
	assert.Equal(t, "todo", string(m.columns[0].status))
	assert.Equal(t, "in_progress", string(m.columns[1].status))
	assert.Equal(t, "paused", string(m.columns[2].status))
	assert.Len(t, m.columns[0].tickets, 2) // b, d
}

func TestListModel_ShowDoneToggle(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets([]web.TicketInfo{
		{ID: "a", Status: "in_progress", Kontora: true},
		{ID: "b", Status: "done", Kontora: true},
		{ID: "c", Status: "cancelled", Kontora: true},
	}, 1)

	assert.Len(t, m.columns, 1) // in_progress only

	// Toggle done
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	assert.True(t, m.showDone)
	assert.Len(t, m.columns, 3) // in_progress, done, cancelled

	// Toggle back
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	assert.False(t, m.showDone)
	assert.Len(t, m.columns, 1)
}

func TestListModel_ColumnNavigation(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets([]web.TicketInfo{
		{ID: "a", Status: "todo", Kontora: true},
		{ID: "b", Status: "todo", Kontora: true},
		{ID: "c", Status: "todo", Kontora: true},
		{ID: "d", Status: "in_progress", Kontora: true},
	}, 1)

	// todo column has 3 tickets, in_progress has 1
	assert.Equal(t, 0, m.colIdx)

	// Navigate down within todo
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	assert.Equal(t, 2, m.rowIdx)

	// Switch to in_progress column — rowIdx clamped to 0
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 1, m.colIdx)
	assert.Equal(t, 0, m.rowIdx)

	// Switch back — rowIdx stays at 0 (clamped)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	assert.Equal(t, 0, m.colIdx)
	assert.Equal(t, 0, m.rowIdx)
}

func TestListModel_ColumnClampOnSwitch(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets([]web.TicketInfo{
		{ID: "a", Status: "todo", Kontora: true},
		{ID: "b", Status: "in_progress", Kontora: true},
		{ID: "c", Status: "in_progress", Kontora: true},
		{ID: "d", Status: "in_progress", Kontora: true},
	}, 3)

	// Move to in_progress column
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	// Navigate to bottom of in_progress
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	assert.Equal(t, 2, m.rowIdx)

	// Switch to todo (only 1 ticket) — rowIdx clamped to 0
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	assert.Equal(t, 0, m.rowIdx)
}

func TestListModel_EmptyColumnsHidden(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets([]web.TicketInfo{
		{ID: "a", Status: "in_progress", Kontora: true},
	}, 1)

	// Only in_progress column should exist
	assert.Len(t, m.columns, 1)
	assert.Equal(t, "in_progress", string(m.columns[0].status))
}

func TestListModel_NoTasks(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets(nil, 0)

	assert.Nil(t, m.selected())
	assert.Len(t, m.columns, 0)

	view := m.View()
	assert.Contains(t, view, "no tickets")
}

func TestListModel_FilterEmptiesColumn(t *testing.T) {
	m := newListModel()
	m.width = 100
	m.height = 30
	m.setTickets([]web.TicketInfo{
		{ID: "a", Title: "alpha", Status: "todo", Kontora: true},
		{ID: "b", Title: "beta", Status: "in_progress", Kontora: true},
	}, 1)

	assert.Len(t, m.columns, 2)

	// Move to in_progress column
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 1, m.colIdx)

	// Filter to only "alpha" — in_progress column disappears
	m.filtering = true
	m.filter = "alpha"
	m.applyFilter()

	assert.Len(t, m.columns, 1)
	assert.Equal(t, 0, m.colIdx) // clamped
	sel := m.selected()
	require.NotNil(t, sel)
	assert.Equal(t, "a", sel.ID)
}
