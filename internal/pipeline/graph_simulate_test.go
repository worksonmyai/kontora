package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestPreflightMixedProducersCrossWait(t *testing.T) {
	graph := &config.WorkflowGraph{Version: 2, Entry: "review",
		Nodes: []config.WorkflowNode{
			{ID: "review", Type: "agent", Verdicts: []string{"approved", "changes_requested"}},
			{ID: "branch", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approved"}},
			{ID: "tests", Type: "check"},
			{ID: "gate", Type: "wait", Mode: "human", Timeout: config.Duration{Duration: time.Hour}},
			{ID: "by-verdict", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approved"}},
			{ID: "by-code", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", Equals: 1}},
			{ID: "end", Type: "end", Status: "done"},
		}, Edges: []config.WorkflowEdge{
			graphEdge("review", "complete", "branch"), graphEdge("review", "failed", "gate"),
			graphEdge("branch", "true", "gate"), graphEdge("branch", "false", "tests"),
			graphEdge("tests", "pass", "gate"), graphEdge("tests", "fail", "gate"),
			graphEdge("gate", "released", "by-verdict"), graphEdge("gate", "timeout", "by-code"),
			graphEdge("by-verdict", "true", "end"), graphEdge("by-verdict", "false", "end"),
			graphEdge("by-code", "true", "end"), graphEdge("by-code", "false", "end"),
		},
	}
	report := PreflightGraph(graph, GraphPreflightLimit)
	require.False(t, report.Inconclusive)
	require.Len(t, report.Diagnostics, 3)
	var got [][]string
	for _, diagnostic := range report.Diagnostics {
		require.NotNil(t, diagnostic.Producer)
		require.NotEmpty(t, diagnostic.Path)
		require.Equal(t, graph.Entry, diagnostic.Path[0].From)
		require.Equal(t, diagnostic.Node, diagnostic.Path[len(diagnostic.Path)-1].To)
		row := []string{diagnostic.Producer.NodeID, diagnostic.Producer.Outcome, diagnostic.Node}
		for _, edge := range diagnostic.Path {
			row = append(row, edge.Port)
		}
		got = append(got, row)
	}
	require.ElementsMatch(t, [][]string{
		{"review", "failed", "by-verdict", "failed", "released"},
		{"review", "failed", "by-code", "failed", "timeout"},
		{"tests", "complete", "by-verdict", "complete", "false", "pass", "released"},
	}, got)

	for _, tc := range []struct {
		name   string
		events []GraphScenarioEvent
		port   string
	}{
		{"approved survives release", []GraphScenarioEvent{{Node: "review", Verdict: "approved"}, {Node: "gate", Wait: "released"}}, "true"},
		{"failed check survives timeout", []GraphScenarioEvent{{Node: "review", Verdict: "changes_requested"}, {Node: "tests", ExitCode: new(1)}, {Node: "gate", Wait: "timeout"}}, "true"},
		{"passed check survives timeout", []GraphScenarioEvent{{Node: "review", Verdict: "changes_requested"}, {Node: "tests", ExitCode: new(0)}, {Node: "gate", Wait: "timeout"}}, "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := SimulateGraph(graph, tc.events)
			require.Equal(t, "end", result.Stop)
			require.Equal(t, "end", result.Cursor)
			require.Equal(t, tc.port, result.Transitions[len(result.Transitions)-1].Port)
		})
	}
}

func TestPreflightExitCodeDomainAndCap(t *testing.T) {
	graph := &config.WorkflowGraph{Version: 2, Entry: "check", Nodes: []config.WorkflowNode{
		{ID: "check", Type: "check"},
		{ID: "check-code", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code"}},
		{ID: "needs-verdict", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approved"}},
		{ID: "end", Type: "end", Status: "done"},
	}, Edges: []config.WorkflowEdge{
		graphEdge("check", "pass", "check-code"), graphEdge("check", "fail", "check-code"),
		graphEdge("check-code", "true", "needs-verdict"), graphEdge("check-code", "false", "end"),
		graphEdge("needs-verdict", "true", "end"), graphEdge("needs-verdict", "false", "end"),
	}}
	for _, code := range []int{-1, 0, 1, 127, 255, 256} {
		graph.Nodes[1].When.Equals = code
		report := PreflightGraph(graph, GraphPreflightLimit)
		require.False(t, report.Inconclusive)
		if code < 0 || code > 255 {
			require.Empty(t, report.Diagnostics, "out-of-range comparison %d must not create a path", code)
			continue
		}
		require.Len(t, report.Diagnostics, 1)
		require.Equal(t, code, *report.Diagnostics[0].Producer.ExitCode)
		require.Equal(t, "true", report.Diagnostics[0].Path[1].Port)
	}
	graph.Nodes[1].When.Equals = 0
	capped := PreflightGraph(graph, 2)
	require.True(t, capped.Inconclusive)
	require.Equal(t, 2, capped.Explored)
	require.Len(t, capped.Diagnostics, 1, "keep witnesses already found before hitting the cap")
	complete := PreflightGraph(graph, 3)
	require.False(t, complete.Inconclusive, "the last available step exactly completed exploration")
	require.Equal(t, 3, complete.Explored)
}

func TestPreflightVerdictResultSize(t *testing.T) {
	const prefix, suffix = `{"outcome":"complete","summary":"x","verdict":"`, `"}`
	for _, tc := range []struct {
		name, tail, encoded string
		extra               int
	}{
		{name: "plain at limit"},
		{name: "plain above limit", extra: 1},
		{name: "escaped quote at limit", tail: `"`, encoded: `\"`},
		{name: "escaped quote above limit", tail: `"`, encoded: `\"`, extra: 1},
		{name: "escaped control above limit", tail: "\x01", encoded: `\u0001`, extra: 1},
		{name: "unescaped UTF-8 at limit", tail: "\u2028<", encoded: "\u2028<"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			padding := strings.Repeat("v", session.MaxResultBytes-len(prefix)-len(suffix)-len(tc.encoded)+tc.extra)
			verdict := padding + tc.tail
			graph := &config.WorkflowGraph{Version: 2, Entry: "review", Nodes: []config.WorkflowNode{
				{ID: "review", Type: "agent", Verdicts: []string{"other", verdict}},
				{ID: "choose", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: verdict}},
				{ID: "needs-code", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", Equals: 0}},
				{ID: "end", Type: "end", Status: "done"},
			}, Edges: []config.WorkflowEdge{
				graphEdge("review", "complete", "choose"), graphEdge("review", "failed", "end"),
				graphEdge("choose", "true", "needs-code"), graphEdge("choose", "false", "end"),
				graphEdge("needs-code", "true", "end"), graphEdge("needs-code", "false", "end"),
			}}
			data := []byte(prefix + padding + tc.encoded + suffix)
			result, err := session.DecodeResult(data, session.GraphResultVersion)
			report := PreflightGraph(graph, GraphPreflightLimit)
			require.False(t, report.Inconclusive)
			if tc.extra == 0 {
				require.NoError(t, err)
				require.Equal(t, verdict, result.Verdict)
				require.Len(t, report.Diagnostics, 1)
				require.Equal(t, "needs-code", report.Diagnostics[0].Node)
				require.Equal(t, "true", report.Diagnostics[0].Path[1].Port)
			} else {
				require.ErrorContains(t, err, "result exceeds")
				require.Empty(t, report.Diagnostics, "an unreportable verdict must not create a witness")
			}
		})
	}
}

func TestGraphSimulationEntryStateVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		node config.WorkflowNode
		want int
	}{
		{"plain", config.WorkflowNode{Type: "agent"}, 1},
		{"wake enabled", config.WorkflowNode{Type: "agent", Wake: &config.WorkflowWake{Max: 1}}, 2},
		{"declared wait", config.WorkflowNode{Type: "wait", Mode: "human"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.node.ID = "entry"
			graph := &config.WorkflowGraph{Version: 2, Entry: "entry",
				Nodes: []config.WorkflowNode{tc.node, {ID: "end", Type: "end", Status: "done"}},
			}
			if tc.node.Type == "wait" {
				graph.Edges = []config.WorkflowEdge{graphEdge("entry", "released", "end")}
			}
			a := simulationEntry(graph)
			require.False(t, a.Rejected, a.Reason)
			require.Equal(t, tc.want, a.State.Version)
			if tc.node.Type == "wait" {
				result := SimulateGraph(graph, []GraphScenarioEvent{{Node: "entry", Wait: "released"}})
				require.Equal(t, "end", result.Stop)
				legacy := AdvanceGraph(graph, "", &ticket.GraphState{Version: 1, ExecutionID: "old", DefinitionFingerprint: "old"}, nil)
				require.True(t, legacy.Rejected)
				require.Equal(t, 1, legacy.State.Version)
				preflight := PreflightGraph(graph, GraphPreflightLimit)
				require.Empty(t, preflight.Diagnostics)
				require.Equal(t, 2, preflight.Explored, "do not invent a timeout when none is declared")
			}
		})
	}
}
