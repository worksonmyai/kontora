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

	type testCase struct {
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
		wantFieldsAbsent []string
		pipeline         config.Pipeline
	}
	tests := []testCase{
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
			name:             "complete report advances only current stage",
			ticket:           testTicket("plan", ticket.StatusInProgress, 0),
			ev:               Event{Kind: EventAgentExited, RunOutcome: "complete", OutcomeReason: "plan ready", InvocationID: "inv-complete", Timestamp: ts},
			wantKind:         ActionAdvance,
			wantFields:       map[string]string{"status": "todo", "stage": "code", "attempt": "0"},
			wantFieldsAbsent: []string{"completed_at"},
			wantHistory:      true,
			wantHistoryStage: "plan",
			wantHistoryAgent: "opus",
		},
		{
			name:             "complete report finishes last stage",
			ticket:           testTicket("commit", ticket.StatusInProgress, 0),
			ev:               Event{Kind: EventAgentExited, RunOutcome: "complete", Timestamp: ts},
			wantKind:         ActionComplete,
			wantFields:       map[string]string{"status": "done"},
			wantHistory:      true,
			wantHistoryStage: "commit",
			wantHistoryAgent: "sonnet",
		},
		{
			name:             "complete report cannot override runner failure",
			ticket:           testTicket("code", ticket.StatusInProgress, 1),
			ev:               Event{Kind: EventAgentExited, ExitCode: 1, RunOutcome: "complete", Timestamp: ts},
			wantKind:         ActionRetry,
			wantFields:       map[string]string{"status": "todo", "attempt": "2"},
			wantHistory:      true,
			wantHistoryStage: "code",
			wantHistoryAgent: "sonnet",
			wantHistoryExit:  1,
		},
		{
			name:    "agent exited wrong status errors",
			ticket:  testTicket("plan", ticket.StatusTodo, 0),
			ev:      Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts},
			wantErr: true,
		},
	}

	// Semantic failures retain every existing failure policy, even with exit zero.
	for _, tc := range []struct {
		stage    string
		attempt  int
		kind     ActionKind
		fields   map[string]string
		pipeline config.Pipeline
	}{
		{"plan", 0, ActionPause, map[string]string{"status": "paused"}, pipeline},
		{"code", 0, ActionRetry, map[string]string{"status": "todo", "attempt": "1"}, pipeline},
		{"code", 1, ActionRetry, map[string]string{"status": "todo", "attempt": "2"}, pipeline},
		{"code", 2, ActionPause, map[string]string{"status": "paused"}, pipeline},
		{"review", 1, ActionBack, map[string]string{"status": "todo", "stage": "code", "attempt": "0"}, pipeline},
		{"commit", 1, ActionPause, map[string]string{"status": "paused"}, pipeline},
		{"code", 1, ActionPark, map[string]string{"status": "needs_fix"}, testPipelineWithCustomStatuses()},
	} {
		idx, err := stageIndex(tc.pipeline, tc.stage)
		require.NoError(t, err)
		for _, exitCode := range []int{0, 1} {
			tests = append(tests, testCase{
				name:             fmt.Sprintf("failed %s attempt %d policy %s exit %d", tc.stage, tc.attempt, tc.pipeline[idx].OnFailure, exitCode),
				ticket:           testTicket(tc.stage, ticket.StatusInProgress, tc.attempt),
				ev:               Event{Kind: EventAgentExited, ExitCode: exitCode, RunOutcome: "failed", OutcomeReason: "validation failed", InvocationID: "inv-failed", Timestamp: ts},
				wantKind:         tc.kind,
				wantFields:       tc.fields,
				wantFieldsAbsent: []string{"completed_at"},
				wantHistory:      true,
				wantHistoryStage: tc.stage,
				wantHistoryAgent: tc.pipeline[idx].Agent,
				wantHistoryExit:  exitCode,
				pipeline:         tc.pipeline,
			})
			// Neither blocked nor protocol errors enter the failure policy,
			// even when the runner failed or retries have been exhausted.
			for _, disposition := range []struct {
				outcome    string
				forcePause bool
			}{{"blocked", false}, {"protocol_error", false}, {"failed", true}} {
				tt := tests[len(tests)-1]
				tt.name = fmt.Sprintf("%s forced=%t %s attempt %d policy %s exit %d", disposition.outcome, disposition.forcePause, tc.stage, tc.attempt, tc.pipeline[idx].OnFailure, exitCode)
				tt.ev.RunOutcome = disposition.outcome
				tt.ev.ForcePause = disposition.forcePause
				tt.ev.OutcomeReason = "requires operator action"
				tt.ev.InvocationID = "inv-" + disposition.outcome
				tt.wantKind = ActionPause
				tt.wantFields = map[string]string{"status": "paused", "last_error": tt.ev.OutcomeReason}
				tt.wantFieldsAbsent = []string{"stage", "attempt", "completed_at"}
				tests = append(tests, tt)
			}
		}
	}

	for _, status := range []string{"in_progress", "awaiting_data", "todo", "done", "closed", "next"} {
		custom := testPipeline()
		custom[0].RunningStatus = status
		tests = append(tests, testCase{
			name:   "pickup running status " + status,
			ticket: testTicket("plan", ticket.StatusTodo, 0),
			ev:     Event{Kind: EventPickedUp}, pipeline: custom,
			wantErr:  status != "in_progress" && status != "awaiting_data",
			wantKind: ActionSpawn, wantFields: map[string]string{"status": status},
			wantSpawnAgent: "opus", wantSpawnStage: "plan",
		})
	}
	for _, tc := range tests {
		if tc.ev.Kind != EventAgentExited || !tc.ticket.IsRunning() {
			continue
		}
		custom := tc
		custom.name += " under custom active status"
		custom.ticket = tc.ticket.Clone()
		require.NoError(t, custom.ticket.SetRunningStatus("awaiting_data"))
		tests = append(tests, custom)
	}
	tests = append(tests, testCase{
		name: "parked custom cannot exit", ticket: testTicket("plan", "awaiting_data", 0),
		ev: Event{Kind: EventAgentExited}, wantErr: true,
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := pipeline
			if tt.pipeline != nil {
				p = tt.pipeline
			}
			action, err := Evaluate(tt.ticket, p, tt.ev)
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

			for _, key := range tt.wantFieldsAbsent {
				for _, field := range action.Fields {
					assert.NotEqual(t, key, field.Key, "unexpected field update")
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
				assert.Equal(t, tt.ev.RunOutcome, string(action.History.RunOutcome))
				assert.Equal(t, tt.ev.OutcomeReason, action.History.OutcomeReason)
				assert.Equal(t, tt.ev.InvocationID, action.History.InvocationID)
				assert.Equal(t, tt.ticket.StartedAt, action.History.StartedAt)
				require.NotNil(t, action.History.CompletedAt)
				assert.True(t, action.History.CompletedAt.Equal(ts), "History.CompletedAt = %v, want %v", action.History.CompletedAt, ts)
			}
		})
	}
}

// testPipelineWithCustomStatuses returns a pipeline using custom statuses.
func testPipelineWithCustomStatuses() config.Pipeline {
	return config.Pipeline{
		{Stage: "plan", Agent: "opus", OnSuccess: "next", OnFailure: "pause"},
		{Stage: "code", Agent: "sonnet", OnSuccess: "review", OnFailure: "needs_fix"},
	}
}

func TestEvaluateCustomStatusOnSuccess(t *testing.T) {
	pipeline := testPipelineWithCustomStatuses()
	tk := testTicket("code", ticket.StatusInProgress, 0)
	ts := time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)

	action, err := Evaluate(tk, pipeline, Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts})
	require.NoError(t, err)

	assert.Equal(t, ActionPark, action.Kind)
	fields := make(map[string]string)
	for _, f := range action.Fields {
		fields[f.Key] = fmt.Sprint(f.Value)
	}
	assert.Equal(t, "review", fields["status"])
	_, hasCompletedAt := fields["completed_at"]
	assert.False(t, hasCompletedAt)
	require.NotNil(t, action.History)
	assert.Equal(t, "code", action.History.Stage)
}

func TestEvaluateCustomStatusOnFailure(t *testing.T) {
	pipeline := testPipelineWithCustomStatuses()
	tk := testTicket("code", ticket.StatusInProgress, 0)
	ts := time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)

	action, err := Evaluate(tk, pipeline, Event{Kind: EventAgentExited, ExitCode: 1, Timestamp: ts})
	require.NoError(t, err)

	assert.Equal(t, ActionPark, action.Kind)
	fields := make(map[string]string)
	for _, f := range action.Fields {
		fields[f.Key] = fmt.Sprint(f.Value)
	}
	assert.Equal(t, "needs_fix", fields["status"])
	require.NotNil(t, action.History)
	assert.Equal(t, 1, action.History.ExitCode)
}

func TestEvaluateLastStageCustomOnSuccess(t *testing.T) {
	pipeline := testPipelineWithCustomStatuses()
	tk := testTicket("code", ticket.StatusInProgress, 0)
	ts := time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)

	action, err := Evaluate(tk, pipeline, Event{Kind: EventAgentExited, ExitCode: 0, Timestamp: ts})
	require.NoError(t, err)
	assert.Equal(t, ActionPark, action.Kind)
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
