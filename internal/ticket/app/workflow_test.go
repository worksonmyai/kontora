package app

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func workflowConfig() *config.Config {
	cfg := testCfg()
	cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{
		Version: 2, Entry: "write",
		Nodes: []config.WorkflowNode{
			{ID: "write", Type: "agent", Stage: "code", Agent: "claude-sonnet"},
			{ID: "test", Type: "check", Command: "true"},
			{ID: "budget", Type: "loop", Max: 3},
			{ID: "stop", Type: "end", Status: "paused"},
		},
	}}
	return cfg
}

func workflowFixture(t *testing.T) (*config.Config, *memRepo, *spyRuntime, *Service) {
	t.Helper()
	cfg := workflowConfig()
	repo := newMemRepo()
	repo.add("graph-1", "---\nid: graph-1\nkontora: true\nstatus: paused\npipeline: graph\nstage: write\nattempt: 2\nlast_error: blocked\nsummary: retained\nhistory:\n  - stage: write\n    summary: previous run\n---\n# Graph\n")
	require.NoError(t, repo.tickets["graph-1"].Ticket.SetField("graph_state", &ticket.GraphState{
		Version: 1, ExecutionID: "execution", Revision: 12, DefinitionFingerprint: "fingerprint",
		Visit: 4, VisitID: "execution.4", LoopCounts: map[string]int{"budget": 3},
		LastResult:  &ticket.GraphResult{NodeID: "write", VisitID: "execution.4", InvocationID: "invocation", Outcome: "blocked"},
		Transitions: []ticket.GraphTransition{{From: "budget", Port: "repeat", To: "write", LoopCount: 3, Revision: 11}},
	}))
	runtime := &spyRuntime{}
	return cfg, repo, runtime, New(Static(cfg), repo, runtime)
}

func TestWorkflowRetryPreservesExecution(t *testing.T) {
	for _, cursor := range []string{"write", "test"} {
		t.Run(cursor, func(t *testing.T) {
			_, repo, runtime, svc := workflowFixture(t)
			require.NoError(t, repo.tickets["graph-1"].Ticket.SetField("stage", cursor))
			before := repo.tickets["graph-1"].Ticket.Clone()
			_, err := svc.Retry("graph-1")
			require.NoError(t, err)
			got := repo.tickets["graph-1"].Ticket
			expected := before.GraphState.Clone()
			expected.LastResult = nil
			expected.Revision++
			assert.Equal(t, expected, got.GraphState)
			assert.Equal(t, cursor, got.Stage)
			assert.Equal(t, ticket.StatusTodo, got.Status)
			assert.Zero(t, got.Attempt)
			assert.Empty(t, got.LastError)
			assert.Equal(t, before.History, got.History)
			assert.Equal(t, before.Summary, got.Summary)
			assert.Equal(t, []string{"graph-1"}, runtime.enqueued)
		})
	}
}

func TestWorkflowControlsRefuseUnsafeChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*ticket.Ticket)
		action  func(*Service) (Result, error)
		message string
	}{
		{"retry end", func(t *ticket.Ticket) { _ = t.SetField("stage", "stop") }, func(s *Service) (Result, error) { return s.Retry("graph-1") }, "workflow-restart"},
		{"retry control", func(t *ticket.Ticket) { _ = t.SetField("stage", "budget") }, func(s *Service) (Result, error) { return s.Retry("graph-1") }, "executable"},
		{"retry unresolved", func(t *ticket.Ticket) { _ = t.SetField("invocation_id", "unfinished") }, func(s *Service) (Result, error) { return s.Retry("graph-1") }, "workflow-recover"},
		{"retry overflow", func(t *ticket.Ticket) {
			state := t.GraphState.Clone()
			state.Revision = math.MaxUint64
			_ = t.SetField("graph_state", state)
		}, func(s *Service) (Result, error) { return s.Retry("graph-1") }, "overflow"},
		{"skip", func(*ticket.Ticket) {}, func(s *Service) (Result, error) { return s.Skip("graph-1") }, "not supported"},
		{"set stage", func(*ticket.Ticket) {}, func(s *Service) (Result, error) { return s.SetStage("graph-1", "test") }, "not supported"},
		{"move end to todo", func(t *ticket.Ticket) { _ = t.SetField("stage", "stop") }, func(s *Service) (Result, error) { return s.SetStatus("graph-1", ticket.StatusTodo) }, "workflow-restart"},
		{"move unresolved to todo", func(t *ticket.Ticket) { _ = t.SetField("invocation_id", "unfinished") }, func(s *Service) (Result, error) { return s.SetStatus("graph-1", ticket.StatusTodo) }, "workflow-recover"},
		{"retry graph as legacy", func(t *ticket.Ticket) { _ = t.SetField("pipeline", "default") }, func(s *Service) (Result, error) { return s.Retry("graph-1") }, "legacy"},
		{"move graph as standalone", func(t *ticket.Ticket) { _ = t.SetField("pipeline", "") }, func(s *Service) (Result, error) { return s.SetStatus("graph-1", ticket.StatusTodo) }, "standalone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repo, runtime, svc := workflowFixture(t)
			tc.change(repo.tickets["graph-1"].Ticket)
			before, err := repo.tickets["graph-1"].Ticket.Marshal()
			require.NoError(t, err)
			_, err = tc.action(svc)
			require.ErrorIs(t, err, ErrInvalidState)
			assert.ErrorContains(t, err, tc.message)
			after, err := repo.tickets["graph-1"].Ticket.Marshal()
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Empty(t, repo.saved)
			assert.Empty(t, runtime.enqueued)
			assert.Empty(t, runtime.updated)
		})
	}
}

func TestWorkflowInitEntryOnly(t *testing.T) {
	for _, tc := range []struct {
		name, stage, status string
		fail                bool
	}{
		{"default entry", "", "todo", false},
		{"explicit entry", "write", "todo", false},
		{"open entry", "", "open", false},
		{"another executable", "test", "todo", true},
		{"end", "stop", "open", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workflowConfig()
			repo := newMemRepo()
			repo.add("new", "---\nid: new\nstatus: open\n---\n# New\n")
			svc := New(Static(cfg), repo, NoopRuntime{})
			_, err := svc.Init("new", InitRequest{Pipeline: "graph", Stage: tc.stage, Status: tc.status})
			if tc.fail {
				require.ErrorIs(t, err, ErrInvalidState)
				assert.ErrorContains(t, err, "entry")
				assert.Empty(t, repo.saved)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "write", repo.tickets["new"].Ticket.Stage)
			assert.Nil(t, repo.tickets["new"].Ticket.GraphState, "the daemon owns execution creation")
		})
	}
}

func TestWorkflowRetryRejectsChangedPreparation(t *testing.T) {
	for _, change := range []string{"revision", "execution", "result", "invocation"} {
		t.Run(change, func(t *testing.T) {
			_, repo, _, svc := workflowFixture(t)
			repo.beforeMutation = func() {
				tkt := repo.tickets["graph-1"].Ticket
				state := tkt.GraphState.Clone()
				switch change {
				case "revision":
					state.Revision++
				case "execution":
					state.ExecutionID = "replacement"
				case "result":
					state.LastResult = nil
				case "invocation":
					require.NoError(t, tkt.SetField("invocation_id", "new-invocation"))
				}
				require.NoError(t, tkt.SetField("graph_state", state))
			}
			_, err := svc.Retry("graph-1")
			require.ErrorIs(t, err, ErrInvalidState)
			assert.ErrorContains(t, err, "execution settings changed")
			assert.Empty(t, repo.saved)
		})
	}
}

func TestWorkflowReentryWithoutState(t *testing.T) {
	cfg := workflowConfig()
	for _, tc := range []struct {
		cursor string
		fail   bool
	}{{"", false}, {"write", false}, {"test", true}, {"stop", true}} {
		t.Run(tc.cursor, func(t *testing.T) {
			tkt := &ticket.Ticket{Pipeline: "graph", Stage: tc.cursor}
			err := ValidateWorkflowReentry(cfg, tkt)
			if tc.fail {
				require.ErrorIs(t, err, ErrInvalidState)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
