package web

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func TestGraphDTOThinAndDetail(t *testing.T) {
	cfg := &config.Config{Pipelines: map[string]config.PipelineDefinition{"graph": {Graph: &config.WorkflowGraph{Version: 2, Entry: "review", Nodes: []config.WorkflowNode{{ID: "review", Type: "agent", Stage: "review-template", Agent: "reviewer"}}}}}}
	tk := &ticket.Ticket{Kontora: true, Pipeline: "graph", Stage: "review", GraphState: &ticket.GraphState{Version: 1, ExecutionID: "exec", Revision: 2, DefinitionFingerprint: "fp", Visit: 1, VisitID: "exec.1", Transitions: []ticket.GraphTransition{{From: "review", Port: "complete", To: "done", Revision: 2}}}, History: []ticket.HistoryEntry{{Stage: "review", Run: 4, Template: "review-template", WorkflowExecutionID: "exec", NodeVisitID: "exec.1", DefinitionFingerprint: "fp"}}}
	for _, detail := range []bool{false, true} {
		info := TicketInfoFromView(app.BuildView(cfg, tk, detail))
		data, err := json.Marshal(info)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.Contains(t, decoded, "graph_definition")
		assert.Contains(t, decoded, "graph_execution")
		assert.NotContains(t, decoded, "stages")
		execution := decoded["graph_execution"].(map[string]any)
		assert.Equal(t, "review-template", execution["current_template"])
		if detail {
			assert.Contains(t, execution, "transitions")
			require.Len(t, info.History, 1)
			assert.Equal(t, "exec.1", info.History[0].NodeVisitID)
			assert.Equal(t, 1, info.History[0].VisitAttempt)
			assert.Equal(t, 4, info.History[0].Run)
		} else {
			assert.NotContains(t, execution, "transitions")
			assert.NotContains(t, decoded, "history")
		}
	}
	legacy, err := json.Marshal(TicketInfoFromView(app.View{Stages: []string{"code", "review"}, Stage: "review", Attempt: 2}))
	require.NoError(t, err)
	assert.NotContains(t, string(legacy), "graph_")
	assert.Contains(t, string(legacy), `"stages":["code","review"]`)
}
