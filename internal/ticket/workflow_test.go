package ticket

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func workflowState() *GraphState {
	exitCode := 0
	return &GraphState{
		Version: 1, ExecutionID: "execution", Revision: 4, DefinitionFingerprint: "fingerprint",
		VisitID: "execution.2", Visit: 2, LoopCounts: map[string]int{"repair": 1},
		NodeTemplates:      map[string]string{"review": "review-template", "implement": "code"},
		LoopLimits:         map[string]int{"repair": 3},
		LastResult:         &GraphResult{NodeID: "review", VisitID: "execution.2", InvocationID: "invocation", Verdict: "changes", ExitCode: &exitCode, Outcome: "complete"},
		Transitions:        []GraphTransition{{From: "review", Port: "complete", To: "approved", Revision: 4}, {From: "repair", Port: "repeat", To: "implement", LoopCount: 1, Revision: 4}},
		PreviousExecutions: []GraphEvidence{{ExecutionID: "previous", DefinitionFingerprint: "previous-definition", NodeTemplates: map[string]string{"review": "old-review"}, LoopLimits: map[string]int{"repair": 3}, LoopCounts: map[string]int{"repair": 3}, Transitions: []GraphTransition{{From: "repair", Port: "exhausted", To: "stop", LoopCount: 3, Revision: 10}}}},
	}
}

func TestGraphStateClone(t *testing.T) {
	var absent *GraphState
	require.Nil(t, absent.Clone())
	original := workflowState()
	before := original.Clone()
	cloned := original.Clone()
	cloned.ExecutionID = "other"
	cloned.NodeTemplates["review"] = "changed"
	cloned.LoopLimits["repair"] = 99
	cloned.PreviousExecutions[0].NodeTemplates["review"] = "changed"
	cloned.PreviousExecutions[0].LoopLimits["repair"] = 99
	cloned.LoopCounts["repair"]++
	cloned.LastResult.Verdict = "approve"
	*cloned.LastResult.ExitCode = 1
	cloned.Transitions[0].To = "other"
	cloned.PreviousExecutions[0].LoopCounts["repair"] = 99
	cloned.PreviousExecutions[0].Transitions[0].To = "other"
	require.Equal(t, before, original)
	for _, state := range []*GraphState{{}, {LastResult: &GraphResult{}}} {
		require.Equal(t, state, state.Clone())
	}
}

func TestWorkflowPersistence(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			state := workflowState()
			var decoded GraphState
			if format == "yaml" {
				data, err := yaml.Marshal(state)
				require.NoError(t, err)
				require.NoError(t, yaml.Unmarshal(data, &decoded))
			} else {
				data, err := json.Marshal(state)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(data, &decoded))
			}
			require.Equal(t, state, &decoded)
			require.NotNil(t, decoded.LastResult.ExitCode)
			require.Zero(t, *decoded.LastResult.ExitCode)
		})
	}
}

func TestTicketWorkflowMutationRoundTrip(t *testing.T) {
	original, err := ParseBytes([]byte("---\nid: graph\nstatus: todo\nstage: review\ncustom: retained\n---\n# Graph\n"))
	require.NoError(t, err)
	require.NoError(t, original.SetField("graph_state", workflowState()))
	history := []HistoryEntry{
		{Stage: "first-review", Template: "review", WorkflowExecutionID: "execution", NodeVisitID: "execution.1", DefinitionFingerprint: "fingerprint", InvocationID: "first"},
		{Stage: "second-review", Template: "review", WorkflowExecutionID: "execution", NodeVisitID: "execution.2", DefinitionFingerprint: "fingerprint", InvocationID: "second"},
	}
	require.NoError(t, original.SetField("history", history))
	before, err := original.Marshal()
	require.NoError(t, err)

	cloned := original.Clone()
	cloned.GraphState.NodeTemplates["review"] = "changed"
	cloned.GraphState.LoopLimits["repair"] = 99
	cloned.GraphState.LoopCounts["repair"] = 99
	*cloned.GraphState.LastResult.ExitCode = 99
	cloned.GraphState.Transitions[0].To = "changed"
	cloned.History[0].NodeVisitID = "changed"
	require.NoError(t, cloned.SetField("graph_state", cloned.GraphState))
	require.NoError(t, cloned.SetField("history", cloned.History))
	after, err := original.Marshal()
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))

	parsed, err := ParseBytes(after)
	require.NoError(t, err)
	require.Equal(t, workflowState(), parsed.GraphState)
	require.Equal(t, history, parsed.History)
	require.Equal(t, "review", parsed.Stage)
	require.Contains(t, string(after), "custom: retained")
	require.Equal(t, "# Graph\n", parsed.Body)
	require.NoError(t, parsed.SetField("summary", "updated"))
	require.Equal(t, workflowState(), parsed.GraphState)
	require.NoError(t, parsed.DeleteField("graph_state"))
	require.Nil(t, parsed.GraphState)
	data, err := parsed.Marshal()
	require.NoError(t, err)
	require.NotContains(t, string(data), "graph_state:")
}

func TestTicketWorkflowMutationRollback(t *testing.T) {
	original, err := ParseBytes([]byte("---\nid: graph\nstage: review\n---\n# Graph\n"))
	require.NoError(t, err)
	require.NoError(t, original.SetField("graph_state", workflowState()))
	before, err := original.Marshal()
	require.NoError(t, err)
	for _, invalid := range []any{
		map[string]any{"version": "invalid"},
		map[string]any{"loop_counts": map[string]any{"repair": "invalid"}},
		map[string]any{"loop_limits": map[string]any{"repair": "invalid"}},
		map[string]any{"node_templates": map[string]any{"review": []string{"invalid"}}},
		map[string]any{"last_result": map[string]any{"exit_code": "invalid"}},
	} {
		require.Error(t, original.SetField("graph_state", invalid))
		require.Equal(t, workflowState(), original.GraphState)
		after, err := original.Marshal()
		require.NoError(t, err)
		require.Equal(t, string(before), string(after))
	}
}

func TestGraphStateRetainsDisplayEvidence(t *testing.T) {
	previous := workflowState()
	next := &GraphState{ExecutionID: "next"}
	next.RetainExecution(previous)
	require.Len(t, next.PreviousExecutions, 2)
	retained := next.PreviousExecutions[1]
	require.Equal(t, previous.NodeTemplates, retained.NodeTemplates)
	require.Equal(t, previous.LoopLimits, retained.LoopLimits)
	require.Equal(t, previous.LoopCounts, retained.LoopCounts)
	retained.NodeTemplates["review"] = "changed"
	retained.LoopLimits["repair"] = 99
	next.PreviousExecutions[0].NodeTemplates["review"] = "changed"
	next.PreviousExecutions[0].LoopLimits["repair"] = 99
	require.Equal(t, workflowState(), previous)
}

func TestGraphStateOlderDisplayEvidenceOmitted(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			var state GraphState
			var data []byte
			var err error
			if format == "yaml" {
				require.NoError(t, yaml.Unmarshal([]byte("execution_id: old\nloop_counts:\n  repair: 2\n"), &state))
				data, err = yaml.Marshal(state)
			} else {
				require.NoError(t, json.Unmarshal([]byte(`{"execution_id":"old","loop_counts":{"repair":2}}`), &state))
				data, err = json.Marshal(state)
			}
			require.NoError(t, err)
			require.Nil(t, state.NodeTemplates)
			require.Nil(t, state.LoopLimits)
			require.Equal(t, 2, state.LoopCounts["repair"])
			require.NotContains(t, string(data), "node_templates")
			require.NotContains(t, string(data), "loop_limits")
		})
	}
}

func TestLegacyWorkflowFieldsOmitted(t *testing.T) {
	data, err := yaml.Marshal(Ticket{History: []HistoryEntry{{Stage: "review"}}})
	require.NoError(t, err)
	for _, field := range []string{"graph_state", "workflow_execution_id", "node_visit_id", "definition_fingerprint", "template:"} {
		require.NotContains(t, string(data), field)
	}
}
