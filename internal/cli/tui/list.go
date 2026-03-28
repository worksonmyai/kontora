package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

const cardHeight = 4 // ID+dur, title, stage·agent, blank

// columnOrder defines the display order for status columns.
var columnOrder = []ticket.Status{
	ticket.StatusTodo,
	ticket.StatusInProgress,
	ticket.StatusPaused,
	ticket.StatusOpen,
}

var doneStatuses = []ticket.Status{
	ticket.StatusDone,
	ticket.StatusCancelled,
}

var statusTitles = map[ticket.Status]string{
	ticket.StatusTodo:       "Todo",
	ticket.StatusInProgress: "In Progress",
	ticket.StatusPaused:     "Paused",
	ticket.StatusOpen:       "Open",
	ticket.StatusDone:       "Done",
	ticket.StatusCancelled:  "Cancelled",
}

type column struct {
	status  ticket.Status
	tickets []web.TicketInfo
	offset  int
}

type listModel struct {
	tickets        []web.TicketInfo
	filtered       []web.TicketInfo
	columns        []column
	running        int
	customStatuses []string

	colIdx   int
	rowIdx   int
	showDone bool

	height int
	width  int

	filtering bool
	filter    string

	connected bool
	err       error
}

func newListModel() listModel {
	return listModel{height: 20, width: 80}
}

func (m *listModel) setTickets(tickets []web.TicketInfo, running int) {
	m.tickets = filterVisible(tickets)
	m.running = running
	sortTickets(m.tickets)
	m.applyFilter()
}

func (m *listModel) updateTicket(info web.TicketInfo) {
	visible := ticketVisible(info)
	for i, t := range m.tickets {
		if t.ID == info.ID {
			if !visible {
				m.tickets = slices.Delete(m.tickets, i, i+1)
			} else {
				m.tickets[i] = info
			}
			sortTickets(m.tickets)
			m.applyFilter()
			return
		}
	}
	if visible {
		m.tickets = append(m.tickets, info)
		sortTickets(m.tickets)
		m.applyFilter()
	}
}

func sortTickets(tasks []web.TicketInfo) {
	slices.SortFunc(tasks, func(a, b web.TicketInfo) int {
		ra := cli.StatusRank(ticket.Status(a.Status))
		rb := cli.StatusRank(ticket.Status(b.Status))
		if ra != rb {
			return ra - rb
		}
		ta := ticketSortTime(a)
		tb := ticketSortTime(b)
		if c := tb.Compare(ta); c != 0 {
			return c
		}
		if a.Title != b.Title {
			return strings.Compare(a.Title, b.Title)
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func ticketSortTime(t web.TicketInfo) time.Time {
	if t.Status == string(ticket.StatusInProgress) && t.StartedAt != nil {
		return *t.StartedAt
	}
	return derefTimePtr(t.CreatedAt)
}

func ticketVisible(t web.TicketInfo) bool {
	return t.Kontora || t.Status == string(ticket.StatusOpen)
}

func filterVisible(tickets []web.TicketInfo) []web.TicketInfo {
	result := make([]web.TicketInfo, 0, len(tickets))
	for _, t := range tickets {
		if ticketVisible(t) {
			result = append(result, t)
		}
	}
	return result
}

func derefTimePtr(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func (m *listModel) applyFilter() {
	if m.filter == "" {
		m.filtered = m.tickets
	} else {
		q := strings.ToLower(m.filter)
		m.filtered = nil
		for _, t := range m.tickets {
			haystack := strings.ToLower(
				t.ID + " " + t.Title + " " + t.Status + " " + t.Stage + " " + t.Agent + " " + t.Path,
			)
			if strings.Contains(haystack, q) {
				m.filtered = append(m.filtered, t)
			}
		}
	}
	m.buildColumns()
	m.clampCursor()
}

func (m *listModel) buildColumns() {
	oldOffsets := make(map[ticket.Status]int, len(m.columns))
	for _, col := range m.columns {
		oldOffsets[col.status] = col.offset
	}

	m.columns = nil

	order := slices.Clone(columnOrder)
	for _, cs := range m.customStatuses {
		order = append(order, ticket.Status(cs))
	}
	if m.showDone {
		order = append(order, doneStatuses...)
	}

	for _, status := range order {
		var tickets []web.TicketInfo
		for _, t := range m.filtered {
			if ticket.Status(t.Status) == status {
				tickets = append(tickets, t)
			}
		}
		if len(tickets) > 0 {
			off := oldOffsets[status]
			if maxOff := len(tickets) - m.visibleCardRows(); maxOff > 0 {
				off = min(off, maxOff)
			} else {
				off = 0
			}
			m.columns = append(m.columns, column{status: status, tickets: tickets, offset: off})
		}
	}
}

func (m *listModel) clampCursor() {
	if len(m.columns) == 0 {
		m.colIdx = 0
		m.rowIdx = 0
		return
	}
	if m.colIdx >= len(m.columns) {
		m.colIdx = len(m.columns) - 1
	}
	if m.colIdx < 0 {
		m.colIdx = 0
	}
	col := m.columns[m.colIdx]
	if m.rowIdx >= len(col.tickets) {
		m.rowIdx = max(0, len(col.tickets)-1)
	}
	if m.rowIdx < 0 {
		m.rowIdx = 0
	}
	m.clampColumnOffset(m.colIdx)
}

func (m *listModel) clampColumnOffset(ci int) {
	if ci < 0 || ci >= len(m.columns) {
		return
	}
	col := &m.columns[ci]
	rows := m.visibleCardRows()

	if m.rowIdx < col.offset {
		col.offset = m.rowIdx
	}
	if m.rowIdx >= col.offset+rows {
		col.offset = m.rowIdx - rows + 1
	}
	if col.offset < 0 {
		col.offset = 0
	}
}

func (m *listModel) visibleCardRows() int {
	// header(1) + separator(1) + col header(1) + col separator(1) + status bar(2) = 6
	return max(1, (m.height-6)/cardHeight)
}

func (m *listModel) columnWidth() int {
	n := len(m.columns)
	if n == 0 {
		return m.width
	}
	return max(20, (m.width-(n-1)*3)/n)
}

func (m *listModel) selected() *web.TicketInfo {
	if len(m.columns) == 0 || m.colIdx >= len(m.columns) {
		return nil
	}
	col := m.columns[m.colIdx]
	if m.rowIdx < 0 || m.rowIdx >= len(col.tickets) {
		return nil
	}
	return &m.columns[m.colIdx].tickets[m.rowIdx]
}

func (m listModel) Update(msg tea.Msg) (listModel, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		if m.filtering {
			return m.updateFilter(keyMsg)
		}
		return m.updateNormal(keyMsg)
	}
	return m, nil
}

func (m listModel) updateFilter(msg tea.KeyMsg) (listModel, tea.Cmd) {
	switch {
	case isKey(msg, "ctrl+c"):
		return m, tea.Quit
	case isKey(msg, "esc"):
		m.filtering = false
		m.filter = ""
		m.applyFilter()
	case isKey(msg, "enter"):
		m.filtering = false
	case isKey(msg, "backspace"):
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
			m.applyFilter()
		}
	default:
		r := msg.String()
		if len(r) == 1 {
			m.filter += r
			m.applyFilter()
		}
	}
	return m, nil
}

func (m listModel) updateNormal(msg tea.KeyMsg) (listModel, tea.Cmd) {
	switch {
	case isKey(msg, "h", "left"):
		if m.colIdx > 0 {
			m.colIdx--
			col := m.columns[m.colIdx]
			if m.rowIdx >= len(col.tickets) {
				m.rowIdx = max(0, len(col.tickets)-1)
			}
			m.clampColumnOffset(m.colIdx)
		}
	case isKey(msg, "l", "right"):
		if m.colIdx < len(m.columns)-1 {
			m.colIdx++
			col := m.columns[m.colIdx]
			if m.rowIdx >= len(col.tickets) {
				m.rowIdx = max(0, len(col.tickets)-1)
			}
			m.clampColumnOffset(m.colIdx)
		}
	case isKey(msg, "j", "down"):
		if len(m.columns) > 0 && m.colIdx < len(m.columns) {
			col := m.columns[m.colIdx]
			if m.rowIdx < len(col.tickets)-1 {
				m.rowIdx++
				m.clampColumnOffset(m.colIdx)
			}
		}
	case isKey(msg, "k", "up"):
		if m.rowIdx > 0 {
			m.rowIdx--
			m.clampColumnOffset(m.colIdx)
		}
	case isKey(msg, "g"):
		m.rowIdx = 0
		m.clampColumnOffset(m.colIdx)
	case isKey(msg, "G"):
		if len(m.columns) > 0 && m.colIdx < len(m.columns) {
			m.rowIdx = max(0, len(m.columns[m.colIdx].tickets)-1)
			m.clampColumnOffset(m.colIdx)
		}
	case isKey(msg, "d"):
		m.showDone = !m.showDone
		m.buildColumns()
		m.clampCursor()
	case isKey(msg, "/"):
		m.filtering = true
		m.filter = ""
	}
	return m, nil
}

func (m listModel) View() string {
	var b strings.Builder

	// Header
	header := " " + styleBrand.Render("kontora")
	if m.running > 0 {
		header += styleFaint.Render(" · ") + styleRunning.Render(fmt.Sprintf("%d running", m.running))
	}
	if m.connected {
		header += styleFaint.Render(" · ") + styleConnected.Render("connected")
	} else {
		header += styleFaint.Render(" · ") + styleDisconnected.Render("disconnected")
	}
	if m.filtering {
		filterStr := styleFilter.Render("/" + m.filter + "▌")
		header = padRight(header, m.width-lipgloss.Width(filterStr)-1) + filterStr
	}
	b.WriteString(styleHeader.Render(header))
	b.WriteByte('\n')

	// Full-width separator
	b.WriteString(styleFaint.Render(strings.Repeat("─", m.width)))
	b.WriteByte('\n')

	if len(m.columns) == 0 {
		b.WriteString("  no tickets\n")
		for range m.height - 6 {
			b.WriteByte('\n')
		}
	} else {
		colW := m.columnWidth()
		nCols := len(m.columns)

		// Column headers
		headers := make([]string, 0, nCols)
		for ci, col := range m.columns {
			title := statusTitles[col.status]
			if title == "" {
				title = string(col.status)
			}
			h := fmt.Sprintf("  %s (%d)", title, len(col.tickets))

			h = truncateStr(h, colW)
			h = padRight(h, colW)
			if ci == m.colIdx {
				h = styleColHeaderActive.Render(h)
			} else {
				h = styleColHeaderInactive.Render(h)
			}
			headers = append(headers, h)
		}
		b.WriteString(strings.Join(headers, " │ "))
		b.WriteByte('\n')

		// Column separator
		seps := make([]string, 0, nCols)
		for range nCols {
			seps = append(seps, strings.Repeat("─", colW))
		}
		b.WriteString(styleFaint.Render(strings.Join(seps, "─┼─")))
		b.WriteByte('\n')

		// Card area
		visRows := m.visibleCardRows()
		for row := range visRows {
			for line := range cardHeight {
				var parts []string
				for ci, col := range m.columns {
					idx := col.offset + row
					isSelected := ci == m.colIdx && idx == m.rowIdx
					if idx < len(col.tickets) {
						parts = append(parts, renderCardLine(col.tickets[idx], colW, isSelected, line))
					} else {
						parts = append(parts, padRight("", colW))
					}
				}
				b.WriteString(strings.Join(parts, " │ "))
				b.WriteByte('\n')
			}
		}
	}

	// Status bar
	b.WriteByte('\n')
	b.WriteString(styledHints([]string{
		"h/l columns", "j/k navigate", "enter detail", "d done", "/ filter",
		"n new", "p pause", "r retry", "s skip", "a attach",
		"q quit",
	}))

	if m.err != nil {
		b.WriteString("\n " + styleError.Render(m.err.Error()))
	}

	return b.String()
}

func renderCardLine(t web.TicketInfo, colW int, selected bool, line int) string {
	var content string
	switch line {
	case 0: // ID + duration
		dur := ticketDuration(t)
		id := t.ID
		if dur != "—" {
			id += " " + dur
		}
		content = "  " + truncateStr(id, colW-2)
	case 1: // Title
		content = "  " + truncateStr(t.Title, colW-2)
	case 2: // Stage · Agent
		stage := t.Stage
		if stage == "" {
			stage = "—"
		}
		agent := t.Agent
		if agent == "" {
			agent = "—"
		}
		content = "  " + truncateStr(stage+" · "+agent, colW-2)
	case 3: // Blank
		content = ""
	}
	content = padRight(content, colW)
	if selected {
		return styleSelected.Render(content)
	}
	st := ticket.Status(t.Status)
	if st == ticket.StatusDone || st == ticket.StatusCancelled {
		return styleFaint.Render(content)
	}
	if line == 0 {
		if s, ok := statusStyle[st]; ok {
			return s.Render(content)
		}
		return styleCustomStatus.Render(content)
	}
	if line == 1 {
		return styleCardTitle.Render(content)
	}
	return content
}

func ticketDuration(t web.TicketInfo) string {
	if ticket.Status(t.Status) == ticket.StatusInProgress && t.StartedAt != nil {
		return cli.FormatDuration(time.Since(*t.StartedAt))
	}
	return "—"
}

func truncateStr(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}

func padRight(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}
