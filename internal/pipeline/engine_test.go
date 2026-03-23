package pipeline

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// testPipeline returns a 4-stage pipeline: plan → code → review → commit.
// plan: on_failure=pause
// code: on_failure=retry, max_retries=2
// review: on_failure=back
// commit: on_failure=pause, on_success=done
func testPipeline() config.Pipeline {
	return config.Pipeline{
		{Stage: "plan", Agent: "opus", OnSuccess: "next", OnFailure: "pause"},
		{Stage: "code", Agent: "sonnet", OnSuccess: "next", OnFailure: "retry", MaxRetries: 2},
		{Stage: "review", Agent: "sonnet", OnSuccess: "next", OnFailure: "back"},
		{Stage: "commit", Agent: "sonnet", OnSuccess: "done", OnFailure: "pause"},
	}
}

func testTicket(stage string, status ticket.Status, attempt int) *ticket.Ticket {
	startedAt := ""
	if status == ticket.StatusInProgress {
		startedAt = "started_at: 2026-03-05T10:00:00Z"
	}

	yaml := fmt.Sprintf(`---
id: test-001
status: %s
pipeline: default
path: ~/projects/myapp
stage: %s
attempt: %d
%s
created: 2026-03-05T09:00:00Z
---
# Test ticket
`, status, stage, attempt, startedAt)

	t, err := ticket.ParseBytes([]byte(yaml))
	if err != nil {
		panic(fmt.Sprintf("testTicket: %v", err))
	}
	return t
}

func TestEvaluate(t *testing.T) {
	pipeline := testPipeline()
	ts := time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)

	tests := []struct {
		name             string
		ticket           *ticket.Ticket
		ev               Event
		wantKind         ActionKind
		wantErr          bool
		wantFields       map[string]string
		wantSpawnAgent   string
		wantSpawnStage   string
		wantHistory      bool
		wantHistoryStage string
		wantHistoryAgent string
		wantHistoryExit  int
	}{
		{
			name:           "pickup spawns correct agent",
			ticket:         testTicket("plan", ticket.StatusTodo, 0),
			ev:             Event{Kind: EventPickedUp},
			wantKind:       ActionSpawn,
			wantFields:     map[string]string{"status": "in_progress"},
			wantSpawnAgent: "opus",
			wantSpawnStage: "plan",
		},
		{
			name:    "pickup wrong status errors",
			ticket:  testTicket("plan", ticket.StatusInProgress, 0),
			ev:      Event{Kind: EventPickedUp},
			wantErr: true,
		},
		{
			name:    "pickup unknown stage errors",
			ticket:  testTicket("unknown", ticket.StatusTodo, 0),
			ev:      Event{Kind: EventPickedUp},
			wantErr: true,
		},
		{
			name:             "advance to next stage",
			ticket:           testTicket("plan", ticket.StatusInProgress, 0),
			ev:               Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts},
			wantKind:         ActionAdvance,
			wantFields:       map[string]string{"status": "todo", "stage": "code", "attempt": "0"},
			wantHistory:      true,
			wantHistoryStage: "plan",
			wantHistoryAgent: "opus",
			wantHistoryExit:  0,
		},
		{
			name:             "pipeline completion",
			ticket:           testTicket("commit", ticket.StatusInProgress, 0),
			ev:               Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts},
			wantKind:         ActionComplete,
			wantFields:       map[string]string{"status": "done"},
			wantHistory:      true,
			wantHistoryStage: "commit",
			wantHistoryAgent: "sonnet",
			wantHistoryExit:  0,
		},
		{
			name:             "retry first attempt",
			ticket:           testTicket("code", ticket.StatusInProgress, 0),
			ev:               Event{Kind: EventAgentExited, ExitCode: 1, Timestamp: ts},
			wantKind:         ActionRetry,
			wantFields:       map[string]string{"status": "todo", "attempt": "1"},
			wantHistory:      true,
			wantHistoryStage: "code",
			wantHistoryAgent: "sonnet",
			wantHistoryExit:  1,
		},
		{
			name:             "retry second attempt",
			ticket:           testTicket("code", ticket.StatusInProgress, 1),
			ev:               Event{Kind: EventAgentExited, ExitCode: 1, Timestamp: ts},
			wantKind:         ActionRetry,
			wantFields:       map[string]string{"status": "todo", "attempt": "2"},
			wantHistory:      true,
			wantHistoryStage: "code",
			wantHistoryAgent: "sonnet",
			wantHistoryExit:  1,
		},
		{
			name:             "retry exhausted pauses",
			ticket:           testTicket("code", ticket.StatusInProgress, 2),
			ev:               Event{Kind: EventAgentExited, ExitCode: 1, Timestamp: ts},
			wantKind:         ActionPause,
			wantFields:       map[string]string{"status": "paused"},
			wantHistory:      true,
			wantHistoryStage: "code",
			wantHistoryAgent: "sonnet",
			wantHistoryExit:  1,
		},
		{
			name:             "back to previous stage",
			ticket:           testTicket("review", ticket.StatusInProgress, 0),
			ev:               Event{Kind: EventAgentExited, ExitCode: 1, Timestamp: ts},
			wantKind:         ActionBack,
			wantFields:       map[string]string{"status": "todo", "stage": "code", "attempt": "0"},
			wantHistory:      true,
			wantHistoryStage: "review",
			wantHistoryAgent: "sonnet",
			wantHistoryExit:  1,
		},
		{
			name:             "pause on failure",
			ticket:           testTicket("plan", ticket.StatusInProgress, 0),
			ev:               Event{Kind: EventAgentExited, ExitCode: 1, Timestamp: ts},
			wantKind:         ActionPause,
			wantFields:       map[string]string{"status": "paused"},
			wantHistory:      true,
			wantHistoryStage: "plan",
			wantHistoryAgent: "opus",
			wantHistoryExit:  1,
		},
		{
			name:             "history preserves exit code",
			ticket:           testTicket("code", ticket.StatusInProgress, 0),
			ev:               Event{Kind: EventAgentExited, ExitCode: 137, Timestamp: ts},
			wantKind:         ActionRetry,
			wantHistory:      true,
			wantHistoryStage: "code",
			wantHistoryAgent: "sonnet",
			wantHistoryExit:  137,
		},
		{
			name:    "agent exited wrong status errors",
			ticket:  testTicket("plan", ticket.StatusTodo, 0),
			ev:      Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, err := Evaluate(tt.ticket, pipeline, tt.ev)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			assert.Equal(t, tt.wantKind, action.Kind)

			if tt.wantFields != nil {
				fields := make(map[string]string)
				for _, f := range action.Fields {
					fields[f.Key] = fmt.Sprint(f.Value)
				}
				for k, want := range tt.wantFields {
					got, ok := fields[k]
					if !ok {
						t.Errorf("missing field %q in action fields", k)
						continue
					}
					assert.Equal(t, want, got, "field %q", k)
				}
			}

			if tt.wantSpawnAgent != "" {
				require.NotNil(t, action.Spawn)
				assert.Equal(t, tt.wantSpawnAgent, action.Spawn.Agent)
				assert.Equal(t, tt.wantSpawnStage, action.Spawn.Stage)
			}

			if tt.wantHistory {
				require.NotNil(t, action.History)
				assert.Equal(t, tt.wantHistoryStage, action.History.Stage)
				assert.Equal(t, tt.wantHistoryAgent, action.History.Agent)
				assert.Equal(t, tt.wantHistoryExit, action.History.ExitCode)
				require.NotNil(t, action.History.CompletedAt)
				assert.True(t, action.History.CompletedAt.Equal(ts), "History.CompletedAt = %v, want %v", action.History.CompletedAt, ts)
			}
		})
	}
}

func TestEvaluatePickupSetsStartedAt(t *testing.T) {
	pipeline := testPipeline()
	tk := testTicket("plan", ticket.StatusTodo, 0)
	ts := time.Date(2026, 3, 5, 10, 30, 0, 0, time.UTC)

	action, err := Evaluate(tk, pipeline, Event{Kind: EventPickedUp, Timestamp: ts})
	require.NoError(t, err)

	for _, f := range action.Fields {
		if f.Key == "started_at" {
			got, ok := f.Value.(time.Time)
			require.True(t, ok, "started_at value is %T, want time.Time", f.Value)
			assert.True(t, got.Equal(ts), "started_at = %v, want %v", got, ts)
			return
		}
	}
	t.Error("started_at field not found in action fields")
}

func TestEvaluateCompleteSetsCompletedAt(t *testing.T) {
	pipeline := testPipeline()
	tk := testTicket("commit", ticket.StatusInProgress, 0)
	ts := time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)

	action, err := Evaluate(tk, pipeline, Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts})
	require.NoError(t, err)

	for _, f := range action.Fields {
		if f.Key == "completed_at" {
			return
		}
	}
	t.Error("completed_at field not found in action fields for pipeline completion")
}

func TestEvaluateHistoryStartedAtFromTask(t *testing.T) {
	pipeline := testPipeline()
	tk := testTicket("plan", ticket.StatusInProgress, 0)
	ts := time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)

	action, err := Evaluate(tk, pipeline, Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts})
	require.NoError(t, err)

	require.NotNil(t, action.History)
	assert.NotNil(t, action.History.StartedAt, "History.StartedAt should come from ticket.StartedAt")
}

func TestEvaluateHistoryNilStartedAt(t *testing.T) {
	pipeline := testPipeline()
	// Ticket without started_at (hand-edited)
	yaml := `---
id: test-002
status: in_progress
pipeline: default
path: ~/projects/myapp
stage: plan
attempt: 0
created: 2026-03-05T09:00:00Z
---
# Test ticket
`
	tk, err := ticket.ParseBytes([]byte(yaml))
	require.NoError(t, err)

	ts := time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)
	action, err := Evaluate(tk, pipeline, Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts})
	require.NoError(t, err)

	require.NotNil(t, action.History)
	// Nil StartedAt should be tolerated
	assert.Nil(t, action.History.StartedAt, "History.StartedAt should be nil for hand-edited ticket")
}
