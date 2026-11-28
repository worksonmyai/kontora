package daemon

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestAssistantBoardDescribesGraph(t *testing.T) {
	graph := &config.WorkflowGraph{
		Version: 2, Entry: "review-a",
		Nodes: []config.WorkflowNode{
			{ID: "review-a", Type: "agent", Stage: "review", Agent: "claude", Verdicts: []string{"changes", "approved"}},
			{ID: "decide", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approved"}},
			{ID: "failed", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", NotEquals: 0}},
			{ID: "repairable", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", In: []any{1, 2}}},
			{ID: "phrase", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", In: []any{`needs "review"`}}},
			{ID: "words", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", In: []any{"needs", "review"}}},
			{ID: "newlines", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", NotEquals: "ready\nretry"}},
			{ID: "budget", Type: "loop", Max: 2},
			{ID: "tests", Type: "check", Command: "go test ./...", Timeout: config.Duration{Duration: time.Minute}},
			{ID: "finished", Type: "end", Status: "human_review"},
		},
		Edges: []config.WorkflowEdge{
			{From: config.WorkflowPort{Node: "review-a", Port: "complete"}, To: "decide"},
			{From: config.WorkflowPort{Node: "decide", Port: "false"}, To: "budget"},
			{From: config.WorkflowPort{Node: "budget", Port: "repeat"}, To: "tests"},
		},
	}
	cfg := &config.Config{Pipelines: map[string]config.PipelineDefinition{
		"graph":  {Graph: graph},
		"legacy": {Legacy: config.Pipeline{{Stage: "plan"}, {Stage: "code"}}},
	}}
	lines := assistantBoard(cfg)
	require.Len(t, lines, 1)
	for _, want := range []string{
		"graph (graph v2 entry=review-a; nodes (unordered):",
		`review-a [agent stage=review agent=claude verdicts=["approved" "changes"]]`,
		`decide [condition verdict equals "approved"]`,
		"failed [condition exit_code not_equals 0]",
		"repairable [condition exit_code in [1, 2]]",
		`phrase [condition verdict in ["needs \"review\""]]`,
		`words [condition verdict in ["needs", "review"]]`,
		`newlines [condition verdict not_equals "ready\nretry"]`,
		"budget [loop max=2]",
		`tests [check command="go test ./..." timeout=1m0s]`,
		"finished [end status=human_review]",
		"edges: budget.repeat -> tests, decide.false -> budget, review-a.complete -> decide",
		"legacy (plan -> code)",
	} {
		assert.Contains(t, lines[0], want)
	}
	assert.NotContains(t, lines[0], "review-a -> decide")

	slices.Reverse(graph.Nodes)
	slices.Reverse(graph.Edges)
	slices.Reverse(graph.Nodes[len(graph.Nodes)-1].Verdicts)
	assert.Equal(t, lines, assistantBoard(cfg), "declaration order is not traversal order")
}

func TestAssistantGraphDescribesCommandCondition(t *testing.T) {
	graph := &config.WorkflowGraph{Version: 2, Entry: "tests", Nodes: []config.WorkflowNode{
		{ID: "ready", Type: "condition", Command: "test -f \"ready\"", Timeout: config.Duration{Duration: 3 * time.Second}},
	}}
	assert.Contains(t, assistantGraph(graph), `ready [condition command="test -f \"ready\"" timeout=3s]`)
}

func TestDocumentedGraphConfigs(t *testing.T) {
	for _, tc := range []struct{ path, heading, entry string }{
		{"../../docs/configuration.md", "### Workflow graphs", "tests"},
		{"../../docs/configuration.md", "### Command conditions", "write-notes"},
		{"../cli/skills/pipelines.md", "## Workflow graphs", "tests"},
	} {
		t.Run(tc.path+"/"+strings.TrimLeft(tc.heading, "# "), func(t *testing.T) {
			data, err := os.ReadFile(tc.path)
			require.NoError(t, err)
			_, section, ok := strings.Cut(string(data), tc.heading+"\n")
			require.True(t, ok)
			_, example, ok := strings.Cut(section, "```yaml\n")
			require.True(t, ok)
			example, _, ok = strings.Cut(example, "\n```")
			require.True(t, ok)
			cfg, err := config.LoadReader(strings.NewReader(example))
			require.NoError(t, err)
			graph := cfg.Pipelines[cfg.DefaultPipeline].Graph
			require.NotNil(t, graph)
			assert.Equal(t, tc.entry, graph.Entry)
			if tc.heading == "### Command conditions" {
				condition, ok := graph.Node("has-notes")
				require.True(t, ok)
				assert.True(t, condition.CommandCondition())
				assert.Equal(t, "test -s CHANGELOG.md", condition.Command)
				assert.Equal(t, time.Minute, condition.Timeout.Duration)
			} else {
				assert.Contains(t, assistantBoard(cfg)[0], "budget.repeat -> fix-code")
			}
		})
	}
}
