package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

type detailTab int

const (
	tabTicket detailTab = iota
	tabLogs
)

type detailModel struct {
	ticket   web.TicketInfo
	tab      detailTab
	viewport viewport.Model
	width    int
	height   int

	logContent string
	logStage   int // index into ticket.Stages
	logStages  []string

	agents   []string // available agents for cycling (empty = not yet fetched)
	agentIdx int      // current index into agents (0 = pipeline default)

	err error
}

func newDetailModel(info web.TicketInfo, width, height int) detailModel {
	m := detailModel{
		ticket: info,
		width:  width,
		height: height,
		tab:    tabTicket,
	}
	m.logStages = info.Stages
	m.viewport = viewport.New(width, m.contentHeight())
	m.viewport.SetContent(info.Body)
	return m
}

func (m *detailModel) contentHeight() int {
	// header(3) + metadata(~8) + tabs(1) + separator(1) + status bar(2) = ~15
	return max(m.height-15, 3)
}

func (m *detailModel) setSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.Width = w
	m.viewport.Height = m.contentHeight()
}

func (m *detailModel) setTicket(info web.TicketInfo) {
	m.ticket = info
	m.logStages = info.Stages
	if m.logStage >= len(m.logStages) {
		m.logStage = 0
	}
	if m.tab == tabTicket {
		m.viewport.SetContent(info.Body)
	}
}

func (m *detailModel) setLogs(content string) {
	m.logContent = content
	if m.tab == tabLogs {
		m.viewport.SetContent(content)
	}
}

func (m detailModel) Update(msg tea.Msg) (detailModel, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		return m.updateKeys(keyMsg)
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m detailModel) updateKeys(msg tea.KeyMsg) (detailModel, tea.Cmd) {
	switch {
	case isKey(msg, "1"):
		m.tab = tabTicket
		m.viewport.SetContent(m.ticket.Body)
		m.viewport.GotoTop()
	case isKey(msg, "2"):
		m.tab = tabLogs
		m.viewport.SetContent(m.logContent)
		m.viewport.GotoTop()
	case isKey(msg, "l"):
		if len(m.logStages) > 0 {
			m.logStage = (m.logStage + 1) % len(m.logStages)
			return m, func() tea.Msg {
				return fetchLogsRequest{id: m.ticket.ID, stage: m.logStages[m.logStage]}
			}
		}
	case isKey(msg, "j", "down"):
		m.viewport.ScrollDown(1)
	case isKey(msg, "k", "up"):
		m.viewport.ScrollUp(1)
	case isKey(msg, "d"):
		m.viewport.HalfPageDown()
	case isKey(msg, "u"):
		m.viewport.HalfPageUp()
	}
	return m, nil
}

func (m detailModel) View() string {
	var b strings.Builder

	// Header: ID + status + back hint
	header := fmt.Sprintf(" %s · %s", m.ticket.ID, styledStatus(m.ticket.Status))
	right := "esc back"
	available := max(0, m.width-lipgloss.Width(right)-1)
	header = padRight(header, available) + styleFaint.Render(right)
	b.WriteString(styleHeader.Render(header))
	b.WriteByte('\n')
	b.WriteString(styleFaint.Render(strings.Repeat("─", m.width)))
	b.WriteByte('\n')

	// Title
	b.WriteString(styleHeader.Render(" " + m.ticket.Title))
	b.WriteByte('\n')
	b.WriteByte('\n')

	// Metadata
	writeMeta(&b, "pipeline", m.ticket.Pipeline)
	writeMeta(&b, "path", m.ticket.Path)
	if m.ticket.Agent != "" {
		agentLabel := m.ticket.Agent
		if m.ticket.AgentOverride {
			agentLabel += styleFaint.Render(" (override)")
		}
		writeMeta(&b, "agent", agentLabel)
	}
	if m.ticket.Kontora {
		writeMeta(&b, "branch", m.ticket.Branch)
	}

	// Stage progress
	if len(m.ticket.Stages) > 0 {
		var stages []string
		for _, s := range m.ticket.Stages {
			if s == m.ticket.Stage {
				stages = append(stages, styleCyan.Render("["+s+"]"))
			} else {
				stages = append(stages, styleFaint.Render(s))
			}
		}
		writeMeta(&b, "stage", strings.Join(stages, styleFaint.Render(" → ")))
	}

	dur := ticketDuration(m.ticket)
	if dur != "—" {
		writeMeta(&b, "in progress", dur)
	} else if m.ticket.StartedAt != nil {
		writeMeta(&b, "started", cli.FormatTimestamp(m.ticket.StartedAt))
	}
	if m.ticket.Attempt > 0 {
		writeMeta(&b, "attempt", fmt.Sprintf("%d", m.ticket.Attempt))
	}
	if m.ticket.LastError != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(fmt.Sprintf(" ⚠ %s", m.ticket.LastError)))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	// Tabs
	b.WriteString(" ")
	tabs := []struct {
		name string
		tab  detailTab
	}{
		{"ticket", tabTicket},
		{"logs", tabLogs},
	}
	for i, t := range tabs {
		if i > 0 {
			b.WriteString("  ")
		}
		label := fmt.Sprintf("%d:%s", i+1, t.name)
		if m.tab == t.tab {
			b.WriteString(styleActiveTab.Render(label))
		} else {
			b.WriteString(styleTab.Render(label))
		}
	}

	// Log stage indicator
	if m.tab == tabLogs && len(m.logStages) > 0 {
		b.WriteString(styleFaint.Render(fmt.Sprintf("  [%s]", m.logStages[m.logStage])))
	}
	b.WriteByte('\n')
	b.WriteString(styleFaint.Render(strings.Repeat("─", m.width)))
	b.WriteByte('\n')

	// Viewport content
	b.WriteString(m.viewport.View())
	b.WriteByte('\n')

	// Status bar
	var parts []string
	st := ticket.Status(m.ticket.Status)
	if st == ticket.StatusInProgress {
		parts = append(parts, "p pause")
	}
	if st == ticket.StatusPaused || st == ticket.StatusDone {
		parts = append(parts, "r retry")
	}
	if st != ticket.StatusDone && st != ticket.StatusCancelled {
		parts = append(parts, "s skip")
	}
	if st == ticket.StatusOpen || st == ticket.StatusTodo || st == ticket.StatusPaused {
		parts = append(parts, "g agent")
	}
	parts = append(parts, "a attach", "l logs", "1/2 tabs", "esc back")
	b.WriteString(styledHints(parts))

	if m.err != nil {
		b.WriteString("\n " + lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(m.err.Error()))
	}

	return b.String()
}

func writeMeta(b *strings.Builder, key, value string) {
	if value == "" {
		value = "—"
	}
	fmt.Fprintf(b, " %s  %s\n", styleFaint.Render(fmt.Sprintf("%-10s", key)), value)
}
