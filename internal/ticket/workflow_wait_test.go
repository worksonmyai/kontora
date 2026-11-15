package ticket

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func waitWorkflowState() *GraphState {
	state := workflowState()
	state.Version = 2
	at := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	receipt := &WorkflowReceipt{ID: "event-1", RequestID: "request-1", Reason: "Deployed", Operator: "operator", Commit: "abc123", Repository: "/repo", Environment: "production", AcceptedAt: at}
	state.Wait = &WorkflowWait{
		ID: "wait-1", Kind: "check", Phase: "pending", ExecutionID: state.ExecutionID, NodeID: "review", VisitID: state.VisitID, InvocationID: "invocation",
		RequestedAt: at, Until: at.Add(time.Hour), Deadline: at.Add(2 * time.Hour), NextPoll: at.Add(time.Minute), Command: "test -f DONE", Poll: time.Minute,
		PollID: "poll-1", PollCount: 2, Output: "pending 🐱", OutputTruncated: true, LogPath: "/logs/poll-1.log", Next: "Review findings", Worktree: "/worktree",
		Repository: "/repo", Branch: "branch", RequiredCommit: "abc123", Environment: "production", Disarmed: true, Receipt: receipt,
	}
	state.Wake = &WakeContext{Reason: "ready", Output: "ready 🐱", Count: 2, Next: "Review findings", WaitID: "wait-0", Receipt: receipt.Clone()}
	state.WakeCount = 2
	state.WaitHistory = []WorkflowWait{*state.Wait.Clone()}
	state.WaitHistory[0].ID = "wait-0"
	state.Continuation = true
	state.PreviousExecutions[0].Wait = state.Wait.Clone()
	state.PreviousExecutions[0].Wake = state.Wake.Clone()
	state.PreviousExecutions[0].WaitHistory = []WorkflowWait{*state.Wait.Clone()}
	state.PreviousExecutions[0].WakeCount = 2
	state.PreviousExecutions[0].Continuation = true
	return state
}

func TestWaitStateDeepClone(t *testing.T) {
	original := waitWorkflowState()
	before := waitWorkflowState()
	clone := original.Clone()
	require.Equal(t, original, clone)
	clone.Wait.Command = "changed"
	clone.Wait.Receipt.Commit = "changed"
	clone.Wake.Reason = "changed"
	clone.Wake.Receipt.Operator = "changed"
	clone.WaitHistory[0].Output = "changed"
	clone.WaitHistory[0].Receipt.Repository = "changed"
	clone.PreviousExecutions[0].Wait.Receipt.ID = "changed"
	clone.PreviousExecutions[0].Wake.Receipt.RequestID = "changed"
	clone.PreviousExecutions[0].WaitHistory[0].Receipt.Environment = "changed"
	clone.PreviousExecutions[0].WaitHistory[0].Phase = "changed"
	clone.WakeCount++
	clone.Continuation = false
	require.Equal(t, before, original)
	var wait *WorkflowWait
	var wake *WakeContext
	var receipt *WorkflowReceipt
	require.Nil(t, wait.Clone())
	require.Nil(t, wake.Clone())
	require.Nil(t, receipt.Clone())
	for _, state := range []*GraphState{
		{}, {Wait: &WorkflowWait{}}, {Wake: &WakeContext{}}, {WaitHistory: []WorkflowWait{{}}},
		{PreviousExecutions: []GraphEvidence{{Wait: &WorkflowWait{}, Wake: &WakeContext{}, WaitHistory: []WorkflowWait{{}}}}},
	} {
		require.Equal(t, state, state.Clone())
	}
}

func TestWaitStateRetainedEvidence(t *testing.T) {
	previous := waitWorkflowState()
	next := &GraphState{Version: 2, ExecutionID: "next"}
	next.RetainExecution(previous)
	require.Len(t, next.PreviousExecutions, 2)
	retained := &next.PreviousExecutions[1]
	require.Equal(t, previous.Wait, retained.Wait)
	require.Equal(t, previous.Wake, retained.Wake)
	require.Equal(t, previous.WaitHistory, retained.WaitHistory)
	require.Equal(t, previous.WakeCount, retained.WakeCount)
	require.Equal(t, previous.Continuation, retained.Continuation)
	retained.Wait.Receipt.ID = "changed"
	retained.Wake.Receipt.Operator = "changed"
	retained.WaitHistory[0].Receipt.ID = "changed"
	next.PreviousExecutions[0].Wait.Receipt.ID = "changed"
	next.PreviousExecutions[0].Wake.Receipt.Operator = "changed"
	next.PreviousExecutions[0].WaitHistory[0].Receipt.ID = "changed"
	require.Equal(t, waitWorkflowState(), previous)
	require.Nil(t, next.Wait, "historical wait authority must not become active")
	require.Nil(t, next.Wake)
	require.False(t, next.Continuation)
}

func TestWaitStatePersistence(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			state := waitWorkflowState()
			var decoded GraphState
			var data []byte
			var err error
			if format == "yaml" {
				data, err = yaml.Marshal(state)
				require.NoError(t, err)
				require.NoError(t, yaml.Unmarshal(data, &decoded))
			} else {
				data, err = json.Marshal(state)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(data, &decoded))
			}
			require.Equal(t, state, &decoded)
			for _, field := range []string{"wait_history", "wake_count", "continuation", "requested_at", "next_poll", "output_truncated", "required_commit", "accepted_at", "request_id"} {
				require.Contains(t, string(data), field)
			}
			require.NotContains(t, string(data), "WaitHistory")
		})
	}
}

func TestWaitTicketMutationAndClone(t *testing.T) {
	original, err := ParseBytes([]byte("---\nid: graph\nstatus: paused\nstage: review\ncustom: retained\n---\n# Waiting\n"))
	require.NoError(t, err)
	require.NoError(t, original.SetField("graph_state", waitWorkflowState()))
	before, err := original.Marshal()
	require.NoError(t, err)
	parsed, err := ParseBytes(before)
	require.NoError(t, err)
	require.Equal(t, waitWorkflowState(), parsed.GraphState)
	clone := original.Clone()
	clone.GraphState.Wait.Receipt.ID = "changed"
	clone.GraphState.Wake.Receipt.ID = "changed"
	clone.GraphState.WaitHistory[0].Receipt.ID = "changed"
	require.NoError(t, clone.SetField("graph_state", clone.GraphState))
	for _, invalid := range []any{
		map[string]any{"wait": map[string]any{"poll_count": "invalid"}},
		map[string]any{"wait": map[string]any{"deadline": []string{"invalid"}}},
		map[string]any{"wait": map[string]any{"receipt": map[string]any{"accepted_at": []string{"invalid"}}}},
		map[string]any{"wake_count": "invalid"},
		map[string]any{"wake": map[string]any{"count": "invalid"}},
		map[string]any{"wait_history": []any{map[string]any{"poll_count": "invalid"}}},
	} {
		require.Error(t, original.SetField("graph_state", invalid))
		require.Equal(t, waitWorkflowState(), original.GraphState)
	}
	after, err := original.Marshal()
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	require.Contains(t, string(after), "custom: retained")
}

func TestVersionOneWaitFieldsOmitted(t *testing.T) {
	state := workflowState()
	jsonData, err := json.Marshal(state)
	require.NoError(t, err)
	yamlData, err := yaml.Marshal(state)
	require.NoError(t, err)
	for _, data := range [][]byte{jsonData, yamlData} {
		for _, field := range []string{"wait", "wake", "wake_count", "wait_history", "continuation"} {
			require.NotContains(t, string(data), field)
		}
	}
	require.Equal(t, 1, state.Version, "schema addition must not implicitly upgrade old executions")
}
