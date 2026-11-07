package ticket

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistoryOutcomeRoundTrip(t *testing.T) {
	for _, outcome := range []RunOutcome{"", RunOutcomeComplete, RunOutcomeBlocked, RunOutcomeProtocolError, RunOutcomeFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			tk, err := ParseBytes([]byte("---\nid: tst-001\nstatus: todo\ncustom: keep\n---\n# Body\n"))
			require.NoError(t, err)
			require.NoError(t, tk.SetField("invocation_id", "active"))
			h := HistoryEntry{Stage: "default", RunOutcome: outcome, OutcomeReason: "needs input", InvocationID: "finished"}
			require.NoError(t, tk.SetField("history", []HistoryEntry{h}))
			data, err := tk.Marshal()
			require.NoError(t, err)
			assert.Contains(t, string(data), "custom: keep")
			parsed, err := ParseBytes(data)
			require.NoError(t, err)
			assert.Equal(t, "active", parsed.InvocationID)
			require.Len(t, parsed.History, 1)
			assert.Equal(t, h, parsed.History[0])
			encoded, err := json.Marshal(h)
			require.NoError(t, err)
			assert.Contains(t, string(encoded), `"outcome_reason":"needs input"`)
			assert.Contains(t, string(encoded), `"invocation_id":"finished"`)
			if outcome == "" {
				assert.NotContains(t, string(data), "run_outcome:")
				assert.NotContains(t, string(encoded), "run_outcome")
			} else {
				assert.Contains(t, string(data), "run_outcome: "+string(outcome))
				assert.Contains(t, string(encoded), `"run_outcome":"`+string(outcome)+`"`)
			}
		})
	}
}

func TestLegacyHistoryOutcomeStaysUnverified(t *testing.T) {
	tk, err := ParseBytes([]byte("---\nid: tst-001\nhistory:\n  - stage: default\n    exit_code: 0\n---\n# Legacy\n"))
	require.NoError(t, err)
	require.Len(t, tk.History, 1)
	assert.Empty(t, tk.History[0].RunOutcome)
	encoded, err := json.Marshal(HistoryEntry{})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "run_outcome")
	assert.NotContains(t, string(encoded), "outcome_reason")
	assert.NotContains(t, string(encoded), "invocation_id")
}
