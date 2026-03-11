package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

type viewState int

const (
	viewList viewState = iota
	viewDetail
	viewCreate
)

// Bubble Tea messages
type (
	ticketsRefreshedMsg struct {
		tickets []web.TicketInfo
		running int
	}
	taskUpdatedMsg   web.TicketEvent
	taskDetailMsg    web.TicketInfo
	logsMsg          struct{ content string }
	tickMsg          struct{}
	errMsg           struct{ err error }
	actionResultMsg  struct{ err error }
	fetchLogsRequest struct {
		id    string
		stage string
	}
	configMsg struct {
		config web.ConfigInfo
		err    error
	}
	ticketCreatedMsg struct{ ticket web.TicketInfo }

	// Agent cycling messages
	agentConfigMsg struct {
		agents []string
		err    error
	}
	agentUpdatedMsg struct {
		ticket web.TicketInfo
		err    error
	}
)

type model struct {
	view   viewState
	list   listModel
	detail detailModel
	create createModel
	source Source
	width  int
	height int

	attachTarget string
}

func newModel(src Source) model {
	return model{
		view:   viewList,
		list:   newListModel(),
		source: src,
	}
}

func (m model) Init() tea.Cmd {
	if m.source != nil && m.source.Connected() {
		return m.fetchTasksCmd()
	}
	return tea.Batch(m.fetchTasksCmd(), tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.width = msg.Width
		m.list.height = msg.Height
		m.detail.setSize(msg.Width, msg.Height)
		m.create.setSize(msg.Width, msg.Height)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case ticketsRefreshedMsg:
		m.list.setTickets(msg.tickets, msg.running)
		m.list.connected = m.source.Connected()
		m.list.err = nil
		return m, nil

	case taskUpdatedMsg:
		return m.handleTaskUpdated(web.TicketEvent(msg)), nil

	case taskDetailMsg:
		info := web.TicketInfo(msg)
		m.detail = newDetailModel(info, m.width, m.height)
		m.view = viewDetail
		stage := ""
		if len(info.Stages) > 0 {
			stage = info.Stages[0]
		}
		return m, m.fetchLogsCmd(info.ID, stage)

	case fetchLogsRequest:
		return m, m.fetchLogsCmd(msg.id, msg.stage)

	case logsMsg:
		m.detail.setLogs(msg.content)
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.fetchTasksCmd(), tickCmd())

	case configMsg:
		if msg.err != nil {
			m.list.err = msg.err
			return m, nil
		}
		m.create = newCreateModel(msg.config, m.width, m.height)
		m.view = viewCreate
		return m, m.create.initCmd

	case ticketCreatedMsg:
		m.view = viewList
		return m, m.fetchTasksCmd()

	case errMsg:
		switch m.view {
		case viewList:
			m.list.err = msg.err
		case viewDetail:
			m.detail.err = msg.err
		case viewCreate:
			m.create.err = msg.err
		}
		return m, nil

	case agentConfigMsg:
		return m.handleAgentConfig(msg)

	case agentUpdatedMsg:
		return m.handleAgentUpdated(msg)

	case actionResultMsg:
		if msg.err != nil {
			switch m.view {
			case viewList:
				m.list.err = msg.err
			case viewDetail:
				m.detail.err = msg.err
			case viewCreate:
				m.create.err = msg.err
			}
		}
		return m, m.fetchTasksCmd()
	}

	switch m.view {
	case viewList:
		return m, nil
	case viewDetail:
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd
	case viewCreate:
		var cmd tea.Cmd
		m.create, cmd = m.create.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.view {
	case viewList:
		return m.handleListKey(msg)
	case viewDetail:
		return m.handleDetailKey(msg)
	case viewCreate:
		return m.handleCreateKey(msg)
	}
	return m, nil
}

func (m model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.list.filtering {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	switch {
	case isKey(msg, "q", "ctrl+c"):
		return m, tea.Quit

	case isKey(msg, "esc"):
		if m.list.filter != "" {
			m.list.filter = ""
			m.list.applyFilter()
			return m, nil
		}
		return m, tea.Quit

	case isKey(msg, "enter"):
		if sel := m.list.selected(); sel != nil {
			return m, m.fetchTaskCmd(sel.ID)
		}

	case isKey(msg, "p"):
		if sel := m.list.selected(); sel != nil {
			return m, actionCmd(func() error { return m.source.PauseTicket(sel.ID) })
		}
	case isKey(msg, "r"):
		if sel := m.list.selected(); sel != nil {
			return m, actionCmd(func() error { return m.source.RetryTicket(sel.ID) })
		}
	case isKey(msg, "s"):
		if sel := m.list.selected(); sel != nil {
			return m, actionCmd(func() error { return m.source.SkipStage(sel.ID) })
		}
	case isKey(msg, "a"):
		if sel := m.list.selected(); sel != nil {
			m.attachTarget = sel.ID
			return m, tea.Quit
		}
	case isKey(msg, "n"):
		if !m.source.Connected() {
			m.list.err = errDaemonNotRunning
			return m, nil
		}
		return m, m.fetchConfigCmd()

	default:
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m model) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case isKey(msg, "ctrl+c"):
		return m, tea.Quit

	case isKey(msg, "esc", "q"):
		m.view = viewList
		return m, m.fetchTasksCmd()

	case isKey(msg, "p"):
		return m, actionCmd(func() error { return m.source.PauseTicket(m.detail.ticket.ID) })
	case isKey(msg, "r"):
		return m, actionCmd(func() error { return m.source.RetryTicket(m.detail.ticket.ID) })
	case isKey(msg, "s"):
		return m, actionCmd(func() error { return m.source.SkipStage(m.detail.ticket.ID) })
	case isKey(msg, "a"):
		m.attachTarget = m.detail.ticket.ID
		return m, tea.Quit

	case isKey(msg, "g"):
		st := ticket.Status(m.detail.ticket.Status)
		switch st {
		case ticket.StatusOpen, ticket.StatusTodo, ticket.StatusPaused:
			// allowed
		case ticket.StatusInProgress, ticket.StatusDone, ticket.StatusCancelled:
			return m, nil
		}
		if !m.source.Connected() {
			m.detail.err = errDaemonNotRunning
			return m, nil
		}
		if len(m.detail.agents) == 0 {
			return m, m.fetchConfigForAgentCmd()
		}
		m.detail.agentIdx = (m.detail.agentIdx + 1) % len(m.detail.agents)
		return m, m.setAgentCmd(m.detail.ticket.ID, m.detail.agents[m.detail.agentIdx])

	default:
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd
	}
}

func (m model) handleCreateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case isKey(msg, "ctrl+c"):
		return m, tea.Quit
	case isKey(msg, "esc"):
		m.view = viewList
		return m, nil
	case isKey(msg, "enter"):
		if err := m.create.validate(); err != nil {
			m.create.err = err
			return m, nil
		}
		m.create.err = nil
		return m, m.createTaskCmd(m.create.request())
	default:
		var cmd tea.Cmd
		m.create, cmd = m.create.Update(msg)
		return m, cmd
	}
}

func (m model) View() string {
	switch m.view {
	case viewDetail:
		return m.detail.View()
	case viewCreate:
		return m.create.View()
	case viewList:
		return m.list.View()
	}
	return m.list.View()
}

// Commands

func (m model) fetchTasksCmd() tea.Cmd {
	src := m.source
	return func() tea.Msg {
		tickets, running, err := src.FetchTickets()
		if err != nil {
			return errMsg{err: err}
		}
		return ticketsRefreshedMsg{tickets: tickets, running: running}
	}
}

func (m model) fetchTaskCmd(id string) tea.Cmd {
	src := m.source
	return func() tea.Msg {
		info, err := src.FetchTask(id)
		if err != nil {
			return errMsg{err: err}
		}
		return taskDetailMsg(info)
	}
}

func (m model) fetchLogsCmd(id, stage string) tea.Cmd {
	src := m.source
	return func() tea.Msg {
		content, err := src.FetchLogs(id, stage)
		if err != nil {
			return errMsg{err: err}
		}
		return logsMsg{content: content}
	}
}

func (m model) fetchConfigCmd() tea.Cmd {
	src := m.source
	return func() tea.Msg {
		cfg, err := src.FetchConfig()
		return configMsg{config: cfg, err: err}
	}
}

func (m model) createTaskCmd(req web.CreateTicketRequest) tea.Cmd {
	src := m.source
	return func() tea.Msg {
		info, err := src.CreateTicket(req)
		if err != nil {
			return errMsg{err: err}
		}
		return ticketCreatedMsg{ticket: info}
	}
}

func (m model) handleTaskUpdated(ev web.TicketEvent) model {
	m.list.updateTicket(ev.Ticket)
	if m.view == viewDetail && m.detail.ticket.ID == ev.Ticket.ID {
		m.detail.setTicket(ev.Ticket)
	}
	running := 0
	for _, t := range m.list.tickets {
		if t.Status == string(ticket.StatusInProgress) {
			running++
		}
	}
	m.list.running = running
	return m
}

func (m model) handleAgentConfig(msg agentConfigMsg) (tea.Model, tea.Cmd) {
	if m.view != viewDetail {
		return m, nil
	}
	if msg.err != nil {
		m.detail.err = msg.err
		return m, nil
	}
	// Build cycling list: "" (pipeline default) + all agents
	m.detail.agents = append([]string{""}, msg.agents...)
	// Find current agent in the list
	m.detail.agentIdx = 0
	if m.detail.ticket.AgentOverride {
		for i, a := range m.detail.agents {
			if a == m.detail.ticket.Agent {
				m.detail.agentIdx = i
				break
			}
		}
	}
	// Advance to next
	m.detail.agentIdx = (m.detail.agentIdx + 1) % len(m.detail.agents)
	return m, m.setAgentCmd(m.detail.ticket.ID, m.detail.agents[m.detail.agentIdx])
}

func (m model) handleAgentUpdated(msg agentUpdatedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.detail.err = msg.err
		return m, nil
	}
	m.detail.setTicket(msg.ticket)
	m.list.updateTicket(msg.ticket)
	return m, nil
}

func (m model) fetchConfigForAgentCmd() tea.Cmd {
	src := m.source
	return func() tea.Msg {
		cfg, err := src.FetchConfig()
		if err != nil {
			return agentConfigMsg{err: err}
		}
		return agentConfigMsg{agents: cfg.Agents}
	}
}

func (m model) setAgentCmd(id, agent string) tea.Cmd {
	src := m.source
	return func() tea.Msg {
		err := src.UpdateTicket(id, web.UpdateTicketRequest{Agent: &agent})
		if err != nil {
			return agentUpdatedMsg{err: err}
		}
		info, err := src.FetchTask(id)
		if err != nil {
			return agentUpdatedMsg{err: err}
		}
		return agentUpdatedMsg{ticket: info}
	}
}

func actionCmd(fn func() error) tea.Cmd {
	return func() tea.Msg {
		return actionResultMsg{err: fn()}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}
