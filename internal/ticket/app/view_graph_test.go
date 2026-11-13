package app

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func graphViewFixture() (*config.Config, *ticket.Ticket) {
	graph := &config.WorkflowGraph{Version: 2, Entry: "work", Nodes: []config.WorkflowNode{
		{ID: "unused", Type: "agent", Stage: "code", Agent: "other"},
		{ID: "stop", Type: "end", Status: "paused"},
		{ID: "work", Type: "agent", Stage: "code", Agent: "coder", Verdicts: []string{"ready"}},
		{ID: "again", Type: "loop", Max: 2},
	}, Edges: []config.WorkflowEdge{{From: config.WorkflowPort{Node: "again", Port: "repeat"}, To: "work"}, {From: config.WorkflowPort{Node: "again", Port: "exhausted"}, To: "stop"}}}
	cfg := &config.Config{Pipelines: map[string]config.PipelineDefinition{"graph": {Graph: graph}}}
	tk := &ticket.Ticket{Kontora: true, Pipeline: "graph", Stage: "work", Attempt: 1,
		GraphState: &ticket.GraphState{Version: 1, ExecutionID: "exec", Revision: 7, DefinitionFingerprint: "fp", Visit: 3, VisitID: "exec.3", LoopCounts: map[string]int{"again": 2},
			Transitions: []ticket.GraphTransition{{From: "again", Port: "repeat", To: "work", LoopCount: 1, Revision: 2}, {From: "again", Port: "repeat", To: "work", LoopCount: 2, Revision: 4}}},
		History: []ticket.HistoryEntry{
			{Stage: "work", Template: "code", WorkflowExecutionID: "exec", NodeVisitID: "exec.1", DefinitionFingerprint: "fp", Run: 0},
			{Stage: "work", Template: "code", WorkflowExecutionID: "exec", NodeVisitID: "exec.1", DefinitionFingerprint: "fp", Run: 1},
			{Stage: "work", Template: "code", WorkflowExecutionID: "exec", NodeVisitID: "exec.2", DefinitionFingerprint: "fp", Run: 2},
		}}
	tk.GraphState.PreviousExecutions = []ticket.GraphEvidence{{ExecutionID: "previous", DefinitionFingerprint: "old-fp", NodeTemplates: map[string]string{"work": "old-template"}, LoopLimits: map[string]int{"again": 2}, LoopCounts: map[string]int{"again": 2}, Transitions: []ticket.GraphTransition{{From: "again", Port: "exhausted", To: "stop", Revision: 10}}}}
	return cfg, tk
}

func TestBuildView_GraphTopologyAndExecution(t *testing.T) {
	cfg, tk := graphViewFixture()
	for _, detail := range []bool{false, true} {
		t.Run(map[bool]string{false: "list", true: "detail"}[detail], func(t *testing.T) {
			v := BuildView(cfg, tk, detail)
			require.NotNil(t, v.GraphDefinition)
			require.NotNil(t, v.GraphExecution)
			assert.Empty(t, v.Stages, "graph node order is not a legacy stage sequence")
			assert.Equal(t, "unused", v.GraphDefinition.Nodes[0].ID)
			assert.Equal(t, "work", v.GraphDefinition.Entry)
			assert.Equal(t, "coder", v.Agent)
			assert.Empty(t, v.GraphExecution.CurrentTemplate, "the current visit has no saved template yet")
			assert.Equal(t, "exec.3", v.GraphExecution.VisitID)
			require.Len(t, v.GraphExecution.SelectedEdges, 1)
			assert.Equal(t, 2, v.GraphExecution.SelectedEdges[0].LoopCount)
			require.Len(t, v.GraphExecution.Loops, 1)
			assert.Equal(t, 2, v.GraphExecution.Loops[0].Used)
			assert.False(t, v.GraphExecution.Loops[0].Exhausted, "using the last allowance is not an exhausted transition")
			data, err := json.Marshal(v.GraphExecution)
			require.NoError(t, err)
			if detail {
				assert.Len(t, v.GraphExecution.Transitions, 2)
				assert.Len(t, v.GraphExecution.PreviousExecutions, 1)
				assert.Contains(t, string(data), `"transitions"`)
			} else {
				assert.Empty(t, v.GraphExecution.Transitions)
				assert.Empty(t, v.GraphExecution.PreviousExecutions)
				assert.Empty(t, v.History)
				assert.NotContains(t, string(data), `"transitions"`)
			}
		})
	}
	tk.GraphState.Transitions = append(tk.GraphState.Transitions, ticket.GraphTransition{From: "again", Port: "exhausted", To: "stop", Revision: 7})
	assert.True(t, BuildView(cfg, tk, false).GraphExecution.Loops[0].Exhausted)
}

func TestBuildView_GraphExecutionSurvivesConfigReload(t *testing.T) {
	_, tk := graphViewFixture()
	tk.History = append(tk.History, ticket.HistoryEntry{Stage: "work", Template: "code", WorkflowExecutionID: "exec", NodeVisitID: "exec.3", DefinitionFingerprint: "fp"})
	tk.GraphState.Transitions = append(tk.GraphState.Transitions, ticket.GraphTransition{From: "again", Port: "exhausted", To: "stop", LoopCount: 2, Revision: 7})
	persisted, err := ticket.ParseBytes([]byte("---\nid: saved\nkontora: true\npipeline: graph\nstage: work\nstatus: paused\n---\n# Saved execution\n"))
	require.NoError(t, err)
	require.NoError(t, persisted.SetField("graph_state", tk.GraphState))
	require.NoError(t, persisted.SetField("history", tk.History))
	data, err := persisted.Marshal()
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		reload func(*config.Config)
	}{
		{"template and loop limit changed", func(cfg *config.Config) {
			cfg.Pipelines["graph"].Graph.Nodes[2].Stage = "new-template"
			cfg.Pipelines["graph"].Graph.Nodes[3].Max = 99
		}},
		{"loop replaced", func(cfg *config.Config) {
			cfg.Pipelines["graph"].Graph.Nodes[3].ID = "new-loop"
		}},
		{"pipeline removed", func(cfg *config.Config) { delete(cfg.Pipelines, "graph") }},
		{"pipeline changed to legacy", func(cfg *config.Config) {
			cfg.Pipelines["graph"] = config.PipelineDefinition{Legacy: config.Pipeline{{Stage: "work", Agent: "coder"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := graphViewFixture()
			saved, err := ticket.ParseBytes(data)
			require.NoError(t, err)
			before := BuildView(cfg, saved, true)
			tc.reload(cfg)
			for _, detail := range []bool{false, true} {
				v := BuildView(cfg, saved, detail)
				assert.Equal(t, BuildGraphDefinition(cfg.Pipelines["graph"].Graph), v.GraphDefinition)
				assert.NotEqual(t, before.GraphDefinition, v.GraphDefinition)
				want := *before.GraphExecution
				if !detail {
					want.Transitions = nil
					want.PreviousExecutions = nil
				}
				assert.Equal(t, &want, v.GraphExecution)
				assert.Equal(t, "code", v.GraphExecution.CurrentTemplate)
				assert.Equal(t, []GraphLoopView{{NodeID: "again", Used: 2, Exhausted: true}}, v.GraphExecution.Loops)
				executionJSON, err := json.Marshal(v.GraphExecution)
				require.NoError(t, err)
				assert.NotContains(t, string(executionJSON), `"max"`, "saved execution has no loop limit")
				assert.NotContains(t, string(executionJSON), "new-template")
				assert.NotContains(t, string(executionJSON), "new-loop")
			}
		})
	}
}

func TestBuildView_GraphSavedDisplayEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reload func(*config.Config)
	}{
		{"unchanged", func(*config.Config) {}},
		{"changed nodes", func(cfg *config.Config) {
			cfg.Pipelines["graph"].Graph.Nodes[2].Stage = "new-template"
			cfg.Pipelines["graph"].Graph.Nodes[3].Max = 99
		}},
		{"loop removed", func(cfg *config.Config) {
			cfg.Pipelines["graph"].Graph.Nodes = cfg.Pipelines["graph"].Graph.Nodes[:3]
		}},
		{"pipeline removed", func(cfg *config.Config) { delete(cfg.Pipelines, "graph") }},
		{"pipeline now legacy", func(cfg *config.Config) {
			cfg.Pipelines["graph"] = config.PipelineDefinition{Legacy: config.Pipeline{{Stage: "work", Agent: "coder"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, tk := graphViewFixture()
			tk.GraphState.NodeTemplates = map[string]string{"work": "code", "unused": "code"}
			tk.GraphState.LoopLimits = map[string]int{"again": 2, "future": 3}
			tk.GraphState.LoopCounts["retired"] = 4
			tk.GraphState.Transitions = append(tk.GraphState.Transitions, ticket.GraphTransition{From: "again", Port: "exhausted", To: "stop", LoopCount: 2, Revision: 7})
			tk.History = nil
			persisted, err := ticket.ParseBytes([]byte("---\nid: saved\nkontora: true\npipeline: graph\nstage: work\nstatus: paused\n---\n# Saved execution\n"))
			require.NoError(t, err)
			require.NoError(t, persisted.SetField("graph_state", tk.GraphState))
			data, err := persisted.Marshal()
			require.NoError(t, err)
			saved, err := ticket.ParseBytes(data)
			require.NoError(t, err)
			tc.reload(cfg)
			for _, detail := range []bool{false, true} {
				v := BuildView(cfg, saved, detail)
				require.NotNil(t, v.GraphExecution)
				assert.Equal(t, "code", v.GraphExecution.CurrentTemplate, "saved templates are available before the first visit completes")
				assert.Equal(t, []GraphLoopView{
					{NodeID: "again", Used: 2, Max: 2, Exhausted: true},
					{NodeID: "future", Used: 0, Max: 3},
					{NodeID: "retired", Used: 4},
				}, v.GraphExecution.Loops)
				data, err := json.Marshal(v.GraphExecution.Loops)
				require.NoError(t, err)
				assert.JSONEq(t, `[{"node_id":"again","used":2,"max":2,"exhausted":true},{"node_id":"future","used":0,"max":3,"exhausted":false},{"node_id":"retired","used":4,"exhausted":false}]`, string(data))
				if detail {
					assert.Equal(t, tk.GraphState.PreviousExecutions, v.GraphExecution.PreviousExecutions)
				}
			}
		})
	}
}

func TestBuildView_GraphSavedTemplatePrecedesHistory(t *testing.T) {
	cfg, tk := graphViewFixture()
	tk.GraphState.NodeTemplates = map[string]string{"work": "saved-template"}
	tk.History = append(tk.History, ticket.HistoryEntry{Stage: "work", Template: "history-template", WorkflowExecutionID: "exec", NodeVisitID: "exec.3", DefinitionFingerprint: "fp"})
	assert.Equal(t, "saved-template", BuildView(cfg, tk, true).GraphExecution.CurrentTemplate)
}

func TestBuildView_GraphCurrentTemplateUsesCurrentVisitHistory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(*ticket.HistoryEntry)
		template string
	}{
		{"matching visit", func(*ticket.HistoryEntry) {}, "saved-template"},
		{"different execution", func(h *ticket.HistoryEntry) { h.WorkflowExecutionID = "previous" }, ""},
		{"different visit", func(h *ticket.HistoryEntry) { h.NodeVisitID = "exec.2" }, ""},
		{"different fingerprint", func(h *ticket.HistoryEntry) { h.DefinitionFingerprint = "old-fp" }, ""},
		{"different node", func(h *ticket.HistoryEntry) { h.Stage = "unused" }, ""},
		{"annotation", func(h *ticket.HistoryEntry) { h.Kind = "annotation" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, tk := graphViewFixture()
			row := ticket.HistoryEntry{Stage: "work", Template: "saved-template", WorkflowExecutionID: "exec", NodeVisitID: "exec.3", DefinitionFingerprint: "fp"}
			tc.change(&row)
			tk.History = append(tk.History, row)
			for _, detail := range []bool{false, true} {
				assert.Equal(t, tc.template, BuildView(cfg, tk, detail).GraphExecution.CurrentTemplate)
			}
		})
	}
}

func TestBuildView_GraphVisitsAreNotRunIndexes(t *testing.T) {
	cfg, tk := graphViewFixture()
	v := BuildView(cfg, tk, true)
	for i, want := range []int{1, 2, 1} {
		assert.Equal(t, want, v.History[i].VisitAttempt)
		assert.Equal(t, i, v.History[i].Run, "artifact run indexes remain unchanged")
		assert.Equal(t, "code", v.History[i].Template)
		assert.Equal(t, "fp", v.History[i].DefinitionFingerprint)
	}
	tk.Agent = "override"
	assert.Equal(t, "override", BuildView(cfg, tk, true).Agent)
	tk.Stage = "stop"
	assert.Empty(t, BuildView(cfg, tk, true).Agent, "control nodes do not run the ticket's agent override")
	tk.Stage = "work"
	tk.GraphState.VisitID, tk.GraphState.Visit = "exec.1", 1
	for _, tc := range []struct {
		status  ticket.Status
		attempt int
	}{
		{ticket.StatusPaused, 1}, {ticket.StatusTodo, 2}, {ticket.StatusInProgress, 2}, {ticket.StatusHumanReview, 1},
	} {
		tk.Status = tc.status
		assert.Equal(t, tc.attempt, BuildView(cfg, tk, true).GraphExecution.Attempt, tc.status)
	}
}

func TestBuildView_GraphProjectionIsDetached(t *testing.T) {
	cfg, tk := graphViewFixture()
	v := BuildView(cfg, tk, true)
	v.GraphDefinition.Nodes[2].Verdicts[0] = "changed"
	v.GraphExecution.Transitions[0].To = "changed"
	v.GraphExecution.Loops[0].Used = 99
	v.GraphExecution.PreviousExecutions[0].Transitions[0].To = "changed"
	v.GraphExecution.PreviousExecutions[0].LoopCounts["again"] = 99
	v.GraphExecution.PreviousExecutions[0].NodeTemplates["work"] = "changed"
	v.GraphExecution.PreviousExecutions[0].LoopLimits["again"] = 99
	assert.Equal(t, "old-template", tk.GraphState.PreviousExecutions[0].NodeTemplates["work"])
	assert.Equal(t, 2, tk.GraphState.PreviousExecutions[0].LoopLimits["again"])
	assert.Equal(t, "stop", tk.GraphState.PreviousExecutions[0].Transitions[0].To)
	assert.Equal(t, 2, tk.GraphState.PreviousExecutions[0].LoopCounts["again"])
	assert.Equal(t, "ready", cfg.Pipelines["graph"].Graph.Nodes[2].Verdicts[0])
	assert.Equal(t, "work", tk.GraphState.Transitions[0].To)
	assert.Equal(t, 2, tk.GraphState.LoopCounts["again"])
}

func TestBuildView_LegacyGraphFieldsAbsent(t *testing.T) {
	cfg := &config.Config{Pipelines: map[string]config.PipelineDefinition{"legacy": {Legacy: config.Pipeline{{Stage: "code", Agent: "coder"}, {Stage: "review", Agent: "reviewer"}}}}}
	tk := &ticket.Ticket{Kontora: true, Pipeline: "legacy", Stage: "review", Attempt: 2, History: []ticket.HistoryEntry{{Stage: "code", Run: 3}}}
	v := BuildView(cfg, tk, true)
	assert.Nil(t, v.GraphDefinition)
	assert.Nil(t, v.GraphExecution)
	assert.Equal(t, []string{"code", "review"}, v.Stages)
	assert.Equal(t, "reviewer", v.Agent)
	assert.Equal(t, 2, v.Attempt)
	assert.Equal(t, 3, v.History[0].Run)
	assert.Zero(t, v.History[0].VisitAttempt)
}
