package daemon

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestInitializeWorkflowDisplayEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
		saved    bool
		change   func(*config.Config, *config.WorkflowGraph)
	}{
		{name: "new execution"},
		{name: "matching older execution", existing: true},
		{name: "matching saved evidence", existing: true, saved: true},
		{name: "older execution changed template", existing: true, change: func(_ *config.Config, g *config.WorkflowGraph) { g.Nodes[0].Stage = "step2" }},
		{name: "older execution changed loop limit", existing: true, change: func(_ *config.Config, g *config.WorkflowGraph) { g.Nodes[3].Max = 99 }},
		{name: "older execution changed prompt", existing: true, change: func(cfg *config.Config, _ *config.WorkflowGraph) {
			cfg.Stages["step1"] = config.Stage{Prompt: "changed"}
		}},
		{name: "saved execution changed template", existing: true, saved: true, change: func(_ *config.Config, g *config.WorkflowGraph) { g.Nodes[0].Stage = "step2" }},
		{name: "saved execution changed loop limit", existing: true, saved: true, change: func(_ *config.Config, g *config.WorkflowGraph) { g.Nodes[3].Max = 99 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := workflowTestGraph()
			graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "check", Type: "check", Command: "private-check-command"})
			cfg := &config.Config{LogsDir: t.TempDir(),
				Stages:      map[string]config.Stage{"step1": {Prompt: "private-prompt"}},
				Agents:      map[string]config.Agent{"agent1": {Binary: "private-binary"}, "agent2": {Binary: "private-binary"}},
				Environment: map[string]string{"SECRET": "private-environment"},
			}
			tk, err := ticket.ParseBytes([]byte("---\nid: display\nstatus: todo\n---\n# Workflow\n"))
			require.NoError(t, err)
			fingerprint, err := workflowFingerprint(cfg, tk, graph)
			require.NoError(t, err)
			if tc.existing {
				require.NoError(t, tk.SetField("stage", "implement"))
				state := &ticket.GraphState{Version: 1, ExecutionID: "exec", DefinitionFingerprint: fingerprint,
					Visit: 1, VisitID: "exec.1", Revision: 7, LoopCounts: map[string]int{"revise": 2},
					Transitions: []ticket.GraphTransition{{From: "revise", Port: "repeat", To: "implement", LoopCount: 2, Revision: 7}}}
				if tc.saved {
					state.NodeTemplates = map[string]string{"implement": "saved-template"}
					state.LoopLimits = map[string]int{"revise": 5}
				}
				require.NoError(t, tk.SetField("graph_state", state))
			}
			before := tk.GraphState.Clone()
			if tc.change != nil {
				tc.change(cfg, graph)
			}
			d := &Daemon{graphWorkflowsReady: true}
			err = d.initializeWorkflow(cfg, tk, graph)
			if tc.change != nil {
				require.ErrorIs(t, err, errWorkflowNoSpawn)
				require.Equal(t, ticket.StatusPaused, tk.Status)
				require.Contains(t, tk.LastError, "workflow definition changed")
				require.Equal(t, before, tk.GraphState, "unmatched executions must not adopt display evidence")
			} else {
				require.NoError(t, err)
				require.Equal(t, "implement", tk.Stage)
				if tc.saved {
					require.Equal(t, before.NodeTemplates, tk.GraphState.NodeTemplates)
					require.Equal(t, before.LoopLimits, tk.GraphState.LoopLimits)
				} else {
					require.Equal(t, map[string]string{"implement": "step1", "review": "step1"}, tk.GraphState.NodeTemplates)
					require.Equal(t, map[string]int{"revise": 3}, tk.GraphState.LoopLimits)
				}
				if tc.existing {
					require.Equal(t, before.LoopCounts, tk.GraphState.LoopCounts)
					require.Equal(t, before.Transitions, tk.GraphState.Transitions)
					require.Equal(t, before.VisitID, tk.GraphState.VisitID)
				}
			}
			data, err := json.Marshal(tk.GraphState)
			require.NoError(t, err)
			require.NotContains(t, string(data), "private-")
			data, err = tk.Marshal()
			require.NoError(t, err)
			reloaded, err := ticket.ParseBytes(data)
			require.NoError(t, err)
			require.Equal(t, tk.GraphState, reloaded.GraphState)
		})
	}
}
