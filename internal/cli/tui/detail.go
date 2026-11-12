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

type logTarget struct {
	stage   string
	run     int
	visit   string
	attempt int
	graph   bool
	label   string
}

func (t logTarget) same(other logTarget) bool {
	// The live label changes when a visit enters history, but its identity does not.
	return t.stage == other.stage && t.run == other.run && t.visit == other.visit &&
		t.attempt == other.attempt && t.graph == other.graph
}

type detailModel struct {
	ticket   web.TicketInfo
	tab      detailTab
	viewport viewport.Model
	width    int
	height   int

	logContent    string
	logStage      int // index into logTargets
	logTargets    []logTarget
	choosingEntry bool

	agents   []string // available agents for cycling (empty = not yet fetched)
	agentIdx int      // current index into agents (0 = pipeline default)

	err        error
	readOnly   bool
	attachable bool
	diagnostic string
}

func newDetailModel(info web.TicketInfo, width, height int) detailModel {
	m := detailModel{
		ticket: info,
		width:  width,
		height: height,
		tab:    tabTicket,
	}
	m.setLogTargets(info)
	m.viewport = viewport.New(width, m.contentHeight())
	m.viewport.SetContent(m.ticketContent())
	return m
}

func (m *detailModel) contentHeight() int {
	// header(3) + metadata(~8) + tabs(1) + separator(1) + status bar(2) = ~15
	extra := 0
	if m.ticket.GraphExecution != nil {
		extra += 3
	}
	if m.choosingEntry {
		extra++
	}
	return max(m.height-15-extra, 3)
}

func (m *detailModel) setSize(w, h int) {
	m.width = w
	m.height = h
	m.viewport.Width = w
	m.viewport.Height = m.contentHeight()
}

func (m *detailModel) setTicket(info web.TicketInfo) {
	old, next := m.ticket.GraphExecution, info.GraphExecution
	if info.IsRunning() || old == nil || next == nil || old.ExecutionID != next.ExecutionID || old.Revision != next.Revision {
		m.choosingEntry = false
	}
	previous, id := m.selectedLogTarget(), m.ticket.ID
	m.ticket = info
	m.setLogTargets(info)
	if previous.graph && isGraph(info) {
		m.logStage = 0
		for i, target := range m.logTargets {
			if target.same(previous) {
				m.logStage = i
				break
			}
		}
	} else if m.logStage >= len(m.logTargets) {
		m.logStage = 0
	}
	if id != info.ID || !previous.same(m.selectedLogTarget()) {
		m.setLogs("")
	}
	if m.tab == tabTicket {
		m.viewport.SetContent(m.ticketContent())
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
		m.viewport.SetContent(m.ticketContent())
		m.viewport.GotoTop()
	case isKey(msg, "2"):
		m.tab = tabLogs
		m.viewport.SetContent(m.logContent)
		m.viewport.GotoTop()
	case isKey(msg, "l"):
		if len(m.logTargets) > 0 {
			previous := m.selectedLogTarget()
			m.logStage = (m.logStage + 1) % len(m.logTargets)
			target, id := m.selectedLogTarget(), m.ticket.ID
			if !previous.same(target) {
				m.setLogs("")
			}
			return m, func() tea.Msg {
				return fetchLogsRequest{id: id, target: target}
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
	if !m.ticket.Kontora {
		header += styleFaint.Render(" · not a kontora ticket")
	}
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
	if !m.ticket.Kontora {
		b.WriteString(styleFaint.Render(" not a kontora ticket"))
		b.WriteByte('\n')
	}
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
		if m.ticket.BaseBranch != "" {
			writeMeta(&b, "base", m.ticket.BaseBranch)
		}
	}

	if x := m.ticket.GraphExecution; x != nil {
		writeMeta(&b, "node", x.CurrentNode)
		writeMeta(&b, "template", x.CurrentTemplate)
		writeMeta(&b, "visit", fmt.Sprintf("%s · attempt %d", x.VisitID, x.Attempt+1))
	}
	if m.choosingEntry && m.ticket.GraphDefinition != nil {
		writeMeta(&b, "restart", m.ticket.GraphDefinition.Entry+" · fresh execution and counters · enter confirm · esc cancel")
	}

	// Stage progress
	if !isGraph(m.ticket) && len(m.ticket.Stages) > 0 {
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

	if m.ticket.ScheduledAt != "" {
		writeMeta(&b, "starts", cli.FormatSchedule(m.ticket.ScheduledAt))
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
		b.WriteString(styleError.Render(fmt.Sprintf(" ⚠ %s", m.ticket.LastError)))
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
	if m.tab == tabLogs && len(m.logTargets) > 0 {
		b.WriteString(styleFaint.Render(fmt.Sprintf("  [%s]", m.selectedLogTarget().label)))
	}
	b.WriteByte('\n')
	b.WriteString(styleFaint.Render(strings.Repeat("─", m.width)))
	b.WriteByte('\n')

	// Viewport content
	b.WriteString(m.viewport.View())
	b.WriteByte('\n')

	// Status bar
	b.WriteString(styledHints(m.hints()))
	if m.diagnostic != "" {
		b.WriteString("\n " + styleFaint.Render(m.diagnostic))
	}

	if m.err != nil {
		b.WriteString("\n " + lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(m.err.Error()))
	}

	return b.String()
}

func (m detailModel) hints() []string {
	var parts []string
	st := ticket.Status(m.ticket.Status)
	if !m.readOnly && m.ticket.Kontora {
		if m.ticket.IsRunning() {
			parts = append(parts, "p pause first to change stages")
		}
		if st == ticket.StatusPaused || st == ticket.StatusDone {
			parts = append(parts, "r retry")
		}
		if isGraph(m.ticket) && !m.ticket.IsRunning() {
			parts = append(parts, "e restart at entry")
		}
		if canSkipStage(m.ticket) {
			parts = append(parts, "s skip")
		}
	}
	if !m.readOnly && !m.ticket.IsRunning() && (st == ticket.StatusOpen || st == ticket.StatusTodo || st == ticket.StatusPaused) {
		parts = append(parts, "g agent")
	}
	if m.attachable {
		parts = append(parts, "a attach")
	}
	return append(parts, "l logs", "1/2 tabs", "esc back")
}

func canSkipStage(t web.TicketInfo) bool {
	return !isGraph(t) && t.Kontora && t.Kind != string(ticket.KindEpic) &&
		!t.IsRunning() && t.Status != string(ticket.StatusDone) && t.Status != string(ticket.StatusCancelled)
}

func isGraph(t web.TicketInfo) bool { return t.GraphDefinition != nil || t.GraphExecution != nil }

func (m *detailModel) setLogTargets(info web.TicketInfo) {
	m.logTargets = nil
	if !isGraph(info) {
		for _, stage := range info.Stages {
			m.logTargets = append(m.logTargets, logTarget{stage: stage, label: stage})
		}
		return
	}
	for _, h := range info.History {
		if h.Imported {
			continue
		}
		m.logTargets = append(m.logTargets, logTarget{
			stage: h.Stage, run: h.Run, visit: h.NodeVisitID, attempt: max(h.VisitAttempt, 1), graph: true,
			label: fmt.Sprintf("%s · visit %s · attempt %d", h.Stage, h.NodeVisitID, max(h.VisitAttempt, 1)),
		})
	}
	if info.IsRunning() && info.Stage != "" {
		run := 0
		for _, h := range info.History {
			if h.Stage == info.Stage {
				run++
			}
		}
		visit := ""
		if info.GraphExecution != nil {
			visit = info.GraphExecution.VisitID
		}
		attempt := info.Attempt
		if info.GraphExecution != nil {
			attempt = info.GraphExecution.Attempt
		}
		m.logTargets = append(m.logTargets, logTarget{
			stage: info.Stage, run: run, visit: visit, attempt: attempt + 1, graph: true,
			label: fmt.Sprintf("%s · visit %s · attempt %d · live", info.Stage, visit, attempt+1),
		})
	}
}

func (m detailModel) selectedLogTarget() logTarget {
	if m.logStage >= 0 && m.logStage < len(m.logTargets) {
		return m.logTargets[m.logStage]
	}
	return logTarget{graph: isGraph(m.ticket)}
}

func (m detailModel) ticketContent() string {
	if !isGraph(m.ticket) {
		return m.ticket.Body
	}
	var b strings.Builder
	b.WriteString(m.ticket.Body + "\n\nWorkflow graph\n")
	if d := m.ticket.GraphDefinition; d != nil {
		fmt.Fprintf(&b, "entry: %s\n", d.Entry)
		for _, n := range d.Nodes {
			fmt.Fprintf(&b, "%s [%s] template=%s\n", n.ID, n.Type, n.Template)
		}
		for _, e := range d.Edges {
			fmt.Fprintf(&b, "%s.%s -> %s\n", e.From.Node, e.From.Port, e.To)
		}
	}
	if x := m.ticket.GraphExecution; x != nil {
		fmt.Fprintf(&b, "execution: %s · revision %d · visit %s\n", x.ExecutionID, x.Revision, x.VisitID)
		if r := x.LastResult; r != nil {
			fmt.Fprintf(&b, "result: %s · visit %s · %s · verdict %s\n", r.NodeID, r.VisitID, r.Outcome, r.Verdict)
		}
		for _, l := range x.Loops {
			limit := "unknown"
			if l.Max > 0 {
				limit = fmt.Sprint(l.Max)
			}
			fmt.Fprintf(&b, "loop %s: %d / %s exhausted=%t\n", l.NodeID, l.Used, limit, l.Exhausted)
		}
		for _, e := range x.Transitions {
			fmt.Fprintf(&b, "r%d: %s.%s -> %s\n", e.Revision, e.From, e.Port, e.To)
		}
		for _, previous := range x.PreviousExecutions {
			fmt.Fprintf(&b, "previous execution: %s\n", previous.ExecutionID)
			for _, e := range previous.Transitions {
				fmt.Fprintf(&b, "r%d: %s.%s -> %s\n", e.Revision, e.From, e.Port, e.To)
			}
		}
	}
	return b.String()
}

func writeMeta(b *strings.Builder, key, value string) {
	if value == "" {
		value = "—"
	}
	fmt.Fprintf(b, " %s  %s\n", styleFaint.Render(fmt.Sprintf("%-10s", key)), value)
}
