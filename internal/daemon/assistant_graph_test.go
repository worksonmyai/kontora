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
		"decide [condition verdict equals approved]",
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

func TestDocumentedGraphConfigs(t *testing.T) {
	for _, tc := range []struct{ path, heading string }{
		{"../../docs/configuration.md", "### Workflow graphs"},
		{"../cli/skills/pipelines.md", "## Workflow graphs"},
	} {
		t.Run(tc.path, func(t *testing.T) {
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
			assert.Equal(t, "tests", graph.Entry)
			assert.Contains(t, assistantBoard(cfg)[0], "budget.repeat -> fix-code")
		})
	}
}
