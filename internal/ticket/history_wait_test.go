package ticket

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWaitHistoryRoundTripAndClone(t *testing.T) {
	tk, err := ParseBytes([]byte("---\nid: wait-test\nstatus: paused\n---\n# Wait\n"))
	require.NoError(t, err)
	history := []HistoryEntry{{Stage: "work", RunOutcome: RunOutcomeWaiting, Verdict: "", NextStep: "inspect release", Continuation: true,
		Wake: &WakeContext{Reason: "released", Count: 1, Receipt: &WorkflowReceipt{ID: "wake-receipt"}},
		Wait: &WorkflowWait{ID: "wait", Kind: "check", Output: "pending", Receipt: &WorkflowReceipt{ID: "wait-receipt"}},
	}}
	require.NoError(t, tk.SetField("history", history))
	data, err := tk.Marshal()
	require.NoError(t, err)
	loaded, err := ParseBytes(data)
	require.NoError(t, err)
	require.Equal(t, history, loaded.History)
	cloned := loaded.Clone()
	cloned.History[0].Wake.Receipt.ID = "changed"
	cloned.History[0].Wait.Receipt.ID = "changed"
	cloned.History[0].Wait.Output = "changed"
	require.Equal(t, "wake-receipt", loaded.History[0].Wake.Receipt.ID)
	require.Equal(t, "wait-receipt", loaded.History[0].Wait.Receipt.ID)
	require.Equal(t, "pending", loaded.History[0].Wait.Output)
	encoded, err := json.Marshal(loaded.History[0])
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"run_outcome":"waiting"`)
	require.Contains(t, string(encoded), `"next_step":"inspect release"`)
	require.Contains(t, string(encoded), `"continuation":true`)
}

func TestLegacyHistoryOmitsWaitProvenance(t *testing.T) {
	data, err := json.Marshal(HistoryEntry{Stage: "work"})
	require.NoError(t, err)
	for _, field := range []string{"verdict", "next_step", "wake", "wait", "continuation"} {
		require.NotContains(t, string(data), `"`+field+`":`)
	}
}
