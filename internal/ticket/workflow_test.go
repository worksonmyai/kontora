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
		LastResult:  &GraphResult{NodeID: "review", VisitID: "execution.2", InvocationID: "invocation", Verdict: "changes", ExitCode: &exitCode, Outcome: "complete"},
		Transitions: []GraphTransition{{From: "review", Port: "complete", To: "approved", Revision: 4}, {From: "repair", Port: "repeat", To: "implement", LoopCount: 1, Revision: 4}},
	}
}

func TestGraphStateClone(t *testing.T) {
	var absent *GraphState
	require.Nil(t, absent.Clone())
	original := workflowState()
	before := original.Clone()
	cloned := original.Clone()
	cloned.ExecutionID = "other"
	cloned.LoopCounts["repair"]++
	cloned.LastResult.Verdict = "approve"
	*cloned.LastResult.ExitCode = 1
	cloned.Transitions[0].To = "other"
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
		map[string]any{"last_result": map[string]any{"exit_code": "invalid"}},
	} {
		require.Error(t, original.SetField("graph_state", invalid))
		require.Equal(t, workflowState(), original.GraphState)
		after, err := original.Marshal()
		require.NoError(t, err)
		require.Equal(t, string(before), string(after))
	}
}

func TestLegacyWorkflowFieldsOmitted(t *testing.T) {
	data, err := yaml.Marshal(Ticket{History: []HistoryEntry{{Stage: "review"}}})
	require.NoError(t, err)
	for _, field := range []string{"graph_state", "workflow_execution_id", "node_visit_id", "definition_fingerprint", "template:"} {
		require.NotContains(t, string(data), field)
	}
}
