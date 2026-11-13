package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
)

func tuiGraphTicket() web.TicketInfo {
	return web.TicketInfo{ID: "graph", Title: "Graph work", Kontora: true, Status: "paused", Stage: "work", Body: "ticket body",
		GraphDefinition: &app.GraphDefinitionView{Version: 2, Entry: "work", Nodes: []app.GraphNodeView{{ID: "unused", Type: "agent", Template: "code"}, {ID: "work", Type: "agent", Template: "code"}}},
		GraphExecution:  &app.GraphExecutionView{ExecutionID: "exec", CurrentNode: "work", CurrentTemplate: "code", Visit: 2, VisitID: "exec.2", Loops: []app.GraphLoopView{{NodeID: "again", Used: 1, Max: 2}}, Transitions: []ticket.GraphTransition{{From: "again", Port: "repeat", To: "work", Revision: 4}}},
		History:         []web.HistoryInfo{{Stage: "work", Run: 0, NodeVisitID: "exec.1", VisitAttempt: 1}, {Stage: "work", Run: 1, NodeVisitID: "exec.1", VisitAttempt: 2}, {Stage: "work", Run: 2, NodeVisitID: "exec.2", VisitAttempt: 1}}}
}

func TestGraphDetailActualExecutionAndVisitNavigation(t *testing.T) {
	info := tuiGraphTicket()
	m := newDetailModel(info, 160, 60)
	assert.Contains(t, m.View(), "exec.2 · attempt 1")
	assert.Contains(t, m.ticketContent(), "again.repeat -> work")
	assert.Contains(t, m.ticketContent(), "loop again: 1 / 2")
	m.ticket.GraphExecution.Loops[0].Max = 0
	assert.Contains(t, m.ticketContent(), "loop again: 1 / unknown")
	assert.NotContains(t, m.View(), "unused ->")
	assert.False(t, canSkipStage(info))
	require.Len(t, m.logTargets, 3)
	for i, target := range m.logTargets {
		assert.Equal(t, "work", target.stage)
		assert.Equal(t, i, target.run)
	}
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	request := cmd().(fetchLogsRequest)
	assert.Equal(t, 1, request.target.run)
	assert.True(t, request.target.graph)
	assert.Contains(t, m.selectedLogTarget().label, "visit exec.1 · attempt 2")
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	assert.Equal(t, 2, cmd().(fetchLogsRequest).target.run)
	assert.Contains(t, m.selectedLogTarget().label, "visit exec.2 · attempt 1")
}

type graphSource struct {
	mockSource
	calls    []int
	restarts int
}

func (s *graphSource) FetchRunLogs(_, _ string, run int) (string, error) {
	s.calls = append(s.calls, run)
	return "visit-specific transcript", nil
}
func (s *graphSource) RestartWorkflow(string) error { s.restarts++; return nil }

func TestGraphTUIRestartUsesConfiguredEntryAndRequiresConfirmation(t *testing.T) {
	src := &graphSource{}
	src.connected = true
	m := newModel(src)
	m.view = viewDetail
	m.detail = newDetailModel(tuiGraphTicket(), 160, 60)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	require.Nil(t, cmd)
	m = next.(model)
	assert.True(t, m.detail.choosingEntry)
	assert.Contains(t, m.View(), "fresh execution and counters")
	assert.Zero(t, src.restarts)
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	require.NoError(t, cmd().(actionResultMsg).err)
	assert.Equal(t, 1, src.restarts)
	assert.False(t, next.(model).detail.choosingEntry)
}

func TestGraphTUIFetchesExactRunAndDropsOvertakenLogResponse(t *testing.T) {
	src := &graphSource{}
	src.connected = true
	m := newModel(src)
	m, cmd := m.openDetail(tuiGraphTicket())
	require.NotNil(t, cmd)
	response := cmd().(logsMsg)
	assert.Equal(t, []int{0}, src.calls)
	m.detail.logStage = 2
	next, _ := m.Update(response)
	assert.Empty(t, next.(model).detail.logContent)
	response = m.fetchLogsCmd(m.detail.ticket.ID, m.detail.selectedLogTarget())().(logsMsg)
	next, _ = m.Update(response)
	assert.Equal(t, "visit-specific transcript", next.(model).detail.logContent)
}

func TestGraphCreatePreviewUsesEntryNotFirstDefinitionNode(t *testing.T) {
	info := tuiGraphTicket()
	m := newCreateModel(web.ConfigInfo{Pipelines: []string{"repair"}, PipelineInfos: []web.PipelineInfo{{Name: "repair", GraphDefinition: info.GraphDefinition}}}, 160, 50)
	m.fields[fieldPipeline].SetValue("repair")
	assert.Contains(t, m.View(), "configured entry: work")
	assert.Equal(t, "repair", m.request().Pipeline)
}

func TestGraphFileLogsNeverSubstituteSharedStageLog(t *testing.T) {
	dir := t.TempDir()
	ticketsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ticketsDir, "graph.md"), []byte(`---
id: graph
kontora: true
status: paused
stage: work
graph_state:
  execution_id: exec
  visit_id: exec.2
  visit: 2
history:
  - {stage: work, run: 0, node_visit_id: exec.1}
  - {stage: work, run: 1, node_visit_id: exec.1}
  - {stage: work, run: 2, node_visit_id: exec.2}
---
# Graph
`), 0600))
	src := &fileSource{cfg: &config.Config{LogsDir: dir, TicketsDir: ticketsDir}}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "graph"), 0700))
	require.NoError(t, os.WriteFile(session.LogPath(dir, "graph", "work"), []byte("wrong visit"), 0600))
	missing, err := src.FetchRunLogs("graph", "work", 1)
	require.NoError(t, err)
	assert.Contains(t, missing, "No structured record")
	assert.NotContains(t, missing, "wrong visit")
	require.NoError(t, os.WriteFile(session.GraphLogPath(dir, "graph", "work", "exec.2"), []byte("exact check visit"), 0600))
	exact, err := src.FetchRunLogs("graph", "work", 2)
	require.NoError(t, err)
	assert.Equal(t, "exact check visit", exact)
	require.NoError(t, os.WriteFile(session.GraphLogPath(dir, "graph", "work", "exec.1"), []byte("later retry"), 0600))
	stale, err := src.FetchRunLogs("graph", "work", 0)
	require.NoError(t, err)
	assert.Contains(t, stale, "No exact transcript")
	assert.NotContains(t, stale, "later retry\n")
	data, err := json.Marshal(logfmt.Tape{Version: 1, Events: []logfmt.Event{{Kind: "text", Text: "second visit"}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(session.EventsPath(dir, "graph", "work", 2), data, 0600))
	content, err := src.FetchRunLogs("graph", "work", 2)
	require.NoError(t, err)
	assert.Contains(t, content, "second visit")
}
