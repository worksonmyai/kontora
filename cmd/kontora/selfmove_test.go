package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A stage agent must not move the ticket it is a stage of: the daemon reads any
// such write as a human override and discards the run's outcome.
func TestSelfMoveRefusal(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		taskID     string
		running    string // KONTORA_TICKET_ID
		wantRefuse bool
	}{
		{name: "no run in progress", verb: "done", taskID: "kon-q88f", running: "", wantRefuse: false},
		{name: "its own ticket", verb: "done", taskID: "kon-q88f", running: "kon-q88f", wantRefuse: true},
		{name: "its own ticket by prefix", verb: "done", taskID: "kon-q", running: "kon-q88f", wantRefuse: true},
		{name: "a bare prefix still matches", verb: "move", taskID: "k", running: "kon-q88f", wantRefuse: true},
		{name: "a schedule on its own ticket", verb: "schedule", taskID: "kon-q88f", running: "kon-q88f", wantRefuse: true},
		{name: "another ticket", verb: "done", taskID: "kon-4b2t", running: "kon-q88f", wantRefuse: false},
		{name: "a longer id is not a prefix", verb: "done", taskID: "kon-q88fx", running: "kon-q88f", wantRefuse: false},
		{name: "no ticket named", verb: "done", taskID: "", running: "kon-q88f", wantRefuse: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selfMoveRefusal(tc.verb, tc.taskID, tc.running, "commit")

			if !tc.wantRefuse {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tc.running, "the refusal names the running ticket, not the prefix typed")
			assert.Contains(t, got, tc.verb)
			assert.Contains(t, got, `stage "commit"`)
			// The point of the refusal is telling the agent what to do instead.
			assert.Contains(t, got, "exit 0")
			assert.Contains(t, got, "kontora note")
		})
	}

	t.Run("an unknown stage still refuses", func(t *testing.T) {
		got := selfMoveRefusal("done", "kon-q88f", "kon-q88f", "")

		assert.Contains(t, got, "a stage")
	})
}

func TestBulkRefusal(t *testing.T) {
	assert.Empty(t, bulkRefusal("archive", ""))
	assert.Contains(t, bulkRefusal("archive", "kon-q88f"), "kon-q88f")
}
