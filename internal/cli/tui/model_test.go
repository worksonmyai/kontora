package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/web"
)

type mockSource struct {
	tickets     []web.TicketInfo
	running     int
	connected   bool
	detail      web.TicketInfo
	logs        string
	actions     []string
	actionErr   error
	config      web.ConfigInfo
	configErr   error
	createdTask web.TicketInfo
	createErr   error
	updateErr   error
}

func (s *mockSource) FetchTickets() ([]web.TicketInfo, int, error) {
	return s.tickets, s.running, nil
}

func (s *mockSource) FetchTask(string) (web.TicketInfo, error) {
	return s.detail, nil
}

func (s *mockSource) FetchLogs(string, string) (string, error) {
	return s.logs, nil
}

func (s *mockSource) PauseTicket(id string) error {
	s.actions = append(s.actions, "pause:"+id)
	return s.actionErr
}

func (s *mockSource) RetryTicket(id string) error {
	s.actions = append(s.actions, "retry:"+id)
	return s.actionErr
}

func (s *mockSource) SkipStage(id string) error {
	s.actions = append(s.actions, "skip:"+id)
	return s.actionErr
}

func (s *mockSource) SetStage(id string, stage string) error {
	s.actions = append(s.actions, "set-stage:"+id+":"+stage)
	return s.actionErr
}

func (s *mockSource) UpdateTicket(id string, _ web.UpdateTicketRequest) error {
	s.actions = append(s.actions, "update:"+id)
	return s.updateErr
}
func (s *mockSource) FetchConfig() (web.ConfigInfo, error) { return s.config, s.configErr }
func (s *mockSource) CreateTicket(req web.CreateTicketRequest) (web.TicketInfo, error) {
	s.actions = append(s.actions, "create:"+req.Title)
	return s.createdTask, s.createErr
}
func (s *mockSource) Subscribe(context.Context) <-chan web.TicketEvent { return nil }
func (s *mockSource) Connected() bool                                  { return s.connected }

func TestModel_WindowSize(t *testing.T) {
	src := &mockSource{tickets: testTickets(), running: 1}
	m := newModel(src)

	result, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model := result.(model)
	assert.Equal(t, 120, model.width)
	assert.Equal(t, 40, model.height)
	assert.Equal(t, 120, model.list.width)
	assert.Equal(t, 40, model.list.height)
}

func TestModel_TasksRefreshed(t *testing.T) {
	src := &mockSource{}
	m := newModel(src)
	m.list.connected = true

	tickets := testTickets()
	result, _ := m.Update(ticketsRefreshedMsg{tickets: tickets, running: 1})
	model := result.(model)
	assert.Len(t, model.list.filtered, 3)
	assert.Equal(t, 1, model.list.running)
}

func TestModel_TaskUpdated(t *testing.T) {
	src := &mockSource{connected: true}
	m := newModel(src)
	m.list.setTickets(testTickets(), 1)

	ev := web.TicketEvent{
		Type:   "ticket_updated",
		Ticket: web.TicketInfo{ID: "tst-001", Title: "Updated", Status: "paused", Kontora: true},
	}
	result, _ := m.Update(taskUpdatedMsg(ev))
	model := result.(model)

	found := false
	for _, ti := range model.list.tickets {
		if ti.ID == "tst-001" {
			assert.Equal(t, "Updated", ti.Title)
			found = true
		}
	}
	assert.True(t, found)
}

func TestModel_NavigateToDetail(t *testing.T) {
	detail := web.TicketInfo{
		ID:     "tst-001",
		Title:  "Test",
		Status: "in_progress",
		Body:   "Ticket body",
		Stages: []string{"plan", "implement"},
		Stage:  "plan",
	}
	src := &mockSource{tickets: testTickets(), running: 1, detail: detail}
	m := newModel(src)
	m.list.setTickets(testTickets(), 1)
	m.width = 100
	m.height = 30

	result, _ := m.Update(taskDetailMsg(detail))
	model := result.(model)
	assert.Equal(t, viewDetail, model.view)
	assert.Equal(t, "tst-001", model.detail.ticket.ID)
	assert.Equal(t, "Ticket body", model.detail.ticket.Body)
}

func TestModel_DetailBackToList(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"esc", tea.KeyMsg{Type: tea.KeyEscape}},
		{"alt+esc", tea.KeyMsg{Type: tea.KeyEscape, Alt: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &mockSource{tickets: testTickets(), running: 1}
			m := newModel(src)
			m.view = viewDetail
			m.detail = newDetailModel(web.TicketInfo{ID: "tst-001"}, 100, 30)

			result, cmd := m.Update(tc.key)
			model := result.(model)
			assert.Equal(t, viewList, model.view)
			require.NotNil(t, cmd)
		})
	}
}

func TestModel_QuitFromList(t *testing.T) {
	src := &mockSource{}
	m := newModel(src)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	assert.NotNil(t, cmd)
}

func TestModel_AttachFromList(t *testing.T) {
	src := &mockSource{tickets: testTickets(), running: 1}
	m := newModel(src)
	m.list.setTickets(testTickets(), 1)

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model := result.(model)
	assert.Equal(t, "tst-002", model.attachTarget) // todo column is first in kanban
	assert.NotNil(t, cmd)                          // tea.Quit
}

func TestModel_CtrlCFromDetail(t *testing.T) {
	src := &mockSource{tickets: testTickets(), running: 1}
	m := newModel(src)
	m.view = viewDetail
	m.detail = newDetailModel(web.TicketInfo{ID: "tst-001"}, 100, 30)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.NotNil(t, cmd)
}

func TestModel_ErrMsg(t *testing.T) {
	src := &mockSource{}
	m := newModel(src)

	result, _ := m.Update(errMsg{err: assert.AnError})
	model := result.(model)
	assert.Equal(t, assert.AnError, model.list.err)
}

func TestModel_ActionResult(t *testing.T) {
	src := &mockSource{tickets: testTickets(), running: 1}
	m := newModel(src)

	result, cmd := m.Update(actionResultMsg{err: assert.AnError})
	model := result.(model)
	assert.Equal(t, assert.AnError, model.list.err)
	assert.NotNil(t, cmd) // fetchTasksCmd
}

func TestModel_NewTaskOpensForm(t *testing.T) {
	cfg := web.ConfigInfo{
		Pipelines: []string{"default", "review"},
		Agents:    []string{"claude"},
	}
	src := &mockSource{tickets: testTickets(), running: 1, connected: true, config: cfg}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.list.setTickets(testTickets(), 1)

	// Press 'n' — should fire fetchConfigCmd
	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mdl := result.(model)
	assert.Equal(t, viewList, mdl.view) // still list until configMsg arrives
	require.NotNil(t, cmd)

	// Simulate configMsg arrival
	result, _ = mdl.Update(configMsg{config: cfg})
	mdl = result.(model)
	assert.Equal(t, viewCreate, mdl.view)
}

func TestModel_NewTaskDisconnected(t *testing.T) {
	src := &mockSource{tickets: testTickets(), running: 1, connected: false}
	m := newModel(src)
	m.list.setTickets(testTickets(), 1)

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	mdl := result.(model)
	assert.Equal(t, viewList, mdl.view)
	assert.ErrorIs(t, mdl.list.err, errDaemonNotRunning)
	assert.Nil(t, cmd)
}

func TestModel_CreateFormEsc(t *testing.T) {
	src := &mockSource{connected: true}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.view = viewCreate
	m.create = newCreateModel(web.ConfigInfo{}, 100, 30)

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	mdl := result.(model)
	assert.Equal(t, viewList, mdl.view)
}

func TestModel_CreateFormSubmit(t *testing.T) {
	created := web.TicketInfo{ID: "new-001", Title: "New Ticket", Status: "todo"}
	src := &mockSource{connected: true, createdTask: created}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.view = viewCreate
	m.create = newCreateModel(web.ConfigInfo{}, 100, 30)
	m.create.fields[fieldTitle].SetValue("New Ticket")
	m.create.fields[fieldPath].SetValue("./myproject")

	// Press enter — should validate and fire createTaskCmd
	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mdl := result.(model)
	assert.Equal(t, viewCreate, mdl.view) // still create until ticketCreatedMsg
	assert.Nil(t, mdl.create.err)
	require.NotNil(t, cmd)

	// Simulate ticketCreatedMsg
	result, cmd = mdl.Update(ticketCreatedMsg{ticket: created})
	mdl = result.(model)
	assert.Equal(t, viewList, mdl.view)
	assert.NotNil(t, cmd) // fetchTasksCmd
}

func TestModel_AgentCycleFromDetail(t *testing.T) {
	cfg := web.ConfigInfo{
		Pipelines: []string{"default"},
		Agents:    []string{"claude", "opus"},
	}
	src := &mockSource{
		tickets:   testTickets(),
		running:   1,
		connected: true,
		config:    cfg,
	}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.view = viewDetail
	m.detail = newDetailModel(web.TicketInfo{
		ID:     "tst-001",
		Status: "todo",
		Agent:  "claude",
	}, 100, 30)

	// First press of 'g' should fetch config (agents not loaded yet)
	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	mdl := result.(model)
	assert.Empty(t, mdl.detail.agents)
	require.NotNil(t, cmd)

	// Simulate config arrival
	result, cmd = mdl.Update(agentConfigMsg{agents: []string{"claude", "opus"}})
	mdl = result.(model)
	assert.Len(t, mdl.detail.agents, 3) // "" + claude + opus
	require.NotNil(t, cmd)              // setAgentCmd
}

func TestModel_AgentCycleDisabledWhenInProgress(t *testing.T) {
	src := &mockSource{connected: true}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.view = viewDetail
	m.detail = newDetailModel(web.TicketInfo{
		ID:     "tst-001",
		Status: "in_progress",
		Agent:  "claude",
	}, 100, 30)

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	mdl := result.(model)
	assert.Empty(t, mdl.detail.agents)
	assert.Nil(t, cmd)
}

func TestModel_AgentCycleDisabledWhenDisconnected(t *testing.T) {
	src := &mockSource{connected: false}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.view = viewDetail
	m.detail = newDetailModel(web.TicketInfo{
		ID:     "tst-001",
		Status: "todo",
		Agent:  "claude",
	}, 100, 30)

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	mdl := result.(model)
	assert.ErrorIs(t, mdl.detail.err, errDaemonNotRunning)
	assert.Nil(t, cmd)
}

func TestModel_AgentUpdatedRefreshesDetail(t *testing.T) {
	src := &mockSource{connected: true}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.view = viewDetail
	m.list.setTickets(testTickets(), 1)
	m.detail = newDetailModel(web.TicketInfo{
		ID:     "tst-001",
		Status: "todo",
		Agent:  "claude",
	}, 100, 30)

	updated := web.TicketInfo{
		ID:            "tst-001",
		Status:        "todo",
		Agent:         "opus",
		AgentOverride: true,
	}
	result, _ := m.Update(agentUpdatedMsg{ticket: updated})
	mdl := result.(model)
	assert.Equal(t, "opus", mdl.detail.ticket.Agent)
	assert.True(t, mdl.detail.ticket.AgentOverride)
}

func TestModel_AgentUpdatedError(t *testing.T) {
	src := &mockSource{connected: true}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.view = viewDetail
	m.detail = newDetailModel(web.TicketInfo{
		ID:     "tst-001",
		Status: "todo",
	}, 100, 30)

	result, _ := m.Update(agentUpdatedMsg{err: assert.AnError})
	mdl := result.(model)
	assert.Equal(t, assert.AnError, mdl.detail.err)
}

func TestModel_CreateFormValidation(t *testing.T) {
	src := &mockSource{connected: true}
	m := newModel(src)
	m.width = 100
	m.height = 30
	m.view = viewCreate
	m.create = newCreateModel(web.ConfigInfo{}, 100, 30)

	// Press enter with empty fields
	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mdl := result.(model)
	assert.Equal(t, viewCreate, mdl.view)
	assert.Error(t, mdl.create.err)
	assert.Nil(t, cmd)
}
