package pipeline

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func commandConditionSimulationGraph() *config.WorkflowGraph {
	return &config.WorkflowGraph{Version: 2, Entry: "check", Nodes: []config.WorkflowNode{
		{ID: "check", Type: "check"},
		{ID: "probe", Type: "condition", Command: "exit 1"},
		{ID: "by-code", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", Equals: 42}},
		{ID: "needs-verdict", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approved"}},
		{ID: "yes", Type: "end", Status: "done"},
		{ID: "no", Type: "end", Status: "paused"},
	}, Edges: []config.WorkflowEdge{
		graphEdge("check", "pass", "no"), graphEdge("check", "fail", "probe"),
		graphEdge("probe", "true", "by-code"), graphEdge("probe", "false", "no"),
		graphEdge("by-code", "true", "needs-verdict"), graphEdge("by-code", "false", "yes"),
		graphEdge("needs-verdict", "true", "yes"), graphEdge("needs-verdict", "false", "no"),
	}}
}

func TestSimulateGraphCommandCondition(t *testing.T) {
	graph := commandConditionSimulationGraph()
	for _, tc := range []struct {
		name                 string
		code                 *int
		cursor, stop, reason string
		status               ticket.Status
		ports                []string
	}{
		{name: "no event", cursor: "probe", stop: "missing input", status: ticket.StatusTodo},
		{name: "true", code: new(0), cursor: "yes", stop: "end", status: ticket.StatusDone, ports: []string{"true", "false"}},
		{name: "false", code: new(1), cursor: "no", stop: "end", status: ticket.StatusPaused, ports: []string{"false"}},
		{name: "other exit", code: new(2), cursor: "probe", stop: "rejected", reason: "event 2: condition probe exited with code 2", status: ticket.StatusPaused},
		{name: "negative exit", code: new(-1), cursor: "probe", stop: "rejected", reason: "event 2: condition probe exited with code -1", status: ticket.StatusPaused},
		{name: "exit above check range", code: new(256), cursor: "probe", stop: "rejected", reason: "event 2: condition probe exited with code 256", status: ticket.StatusPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []GraphScenarioEvent{{Node: "check", ExitCode: new(1)}}
			if tc.code != nil {
				events = append(events, GraphScenarioEvent{Node: "probe", ExitCode: tc.code})
			}
			report := SimulateGraph(graph, events)
			require.Equal(t, tc.cursor, report.Cursor)
			require.Equal(t, tc.stop, report.Stop)
			require.Equal(t, tc.status, report.Status)
			require.Equal(t, tc.reason, report.Reason)
			var ports []string
			for _, edge := range report.Transitions {
				ports = append(ports, edge.Port)
			}
			require.Equal(t, append([]string{"fail"}, tc.ports...), ports)
			if len(tc.ports) != 0 {
				edge := report.Transitions[1]
				require.Equal(t, "probe", edge.From)
				require.NotEmpty(t, edge.EvaluationID)
			}
		})
	}
}

func TestPreflightGraphCommandCondition(t *testing.T) {
	report := PreflightGraph(commandConditionSimulationGraph(), GraphPreflightLimit)
	require.False(t, report.Inconclusive)
	require.Equal(t, 8, report.Explored) // Entry, three check results, then two exits for each of the two failing checks.
	require.Len(t, report.Diagnostics, 1)
	diagnostic := report.Diagnostics[0]
	require.Equal(t, "needs-verdict", diagnostic.Node)
	require.NotNil(t, diagnostic.Producer)
	require.Equal(t, "check", diagnostic.Producer.NodeID)
	require.Equal(t, new(42), diagnostic.Producer.ExitCode)
	require.Len(t, diagnostic.Path, 3)
	require.Equal(t, "probe", diagnostic.Path[1].From)
	require.Equal(t, "true", diagnostic.Path[1].Port)
}

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
	for _, tc := range []struct {
		name  string
		when  config.WorkflowCondition
		codes []int
	}{
		{"in", config.WorkflowCondition{Field: "exit_code", In: []any{3, 4}}, []int{0, 1, 3, 4}},
		{"not_equals", config.WorkflowCondition{Field: "exit_code", NotEquals: 3}, []int{0, 1, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph.Nodes[1].When = &tc.when
			require.Equal(t, tc.codes, representativeCheckCodes(graph)["check"])
			report := PreflightGraph(graph, GraphPreflightLimit)
			require.False(t, report.Inconclusive)
			require.Equal(t, len(tc.codes)+1, report.Explored)
		})
	}
	graph.Nodes[1].When = &config.WorkflowCondition{Field: "exit_code", Equals: 0}
	capped := PreflightGraph(graph, 2)
	require.True(t, capped.Inconclusive)
	require.Equal(t, 2, capped.Explored)
	require.Len(t, capped.Diagnostics, 1, "keep witnesses already found before hitting the cap")
	complete := PreflightGraph(graph, 3)
	require.False(t, complete.Inconclusive, "the last available step exactly completed exploration")
	require.Equal(t, 3, complete.Explored)
}

func TestPreflightExitCodeListAfterRetryingChecks(t *testing.T) {
	var values []any
	for code := 128; code <= 255; code++ {
		values = append(values, code)
	}
	graph := &config.WorkflowGraph{Version: 2, Nodes: []config.WorkflowNode{
		{ID: "final", Type: "check"},
		{ID: "by-code", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", In: values}},
		{ID: "end", Type: "end", Status: "done"},
	}, Edges: []config.WorkflowEdge{
		graphEdge("final", "pass", "by-code"), graphEdge("final", "fail", "by-code"),
		graphEdge("by-code", "true", "end"), graphEdge("by-code", "false", "end"),
	}}
	next := "final"
	for i := 4; i > 0; i-- {
		check := fmt.Sprintf("check-%d", i)
		retry := "retry-" + check
		graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: check, Type: "check"},
			config.WorkflowNode{ID: retry, Type: "loop", Max: 1})
		graph.Edges = append(graph.Edges, graphEdge(check, "pass", next), graphEdge(check, "fail", retry),
			graphEdge(retry, "repeat", check), graphEdge(retry, "exhausted", "end"))
		next = check
	}
	graph.Entry = next

	report := PreflightGraph(graph, GraphPreflightLimit)
	require.False(t, report.Inconclusive, "only the final check should try the codes in the list")
	require.Empty(t, report.Diagnostics)
}

func TestPreflightExitCodeConsumersAcrossControls(t *testing.T) {
	graph := &config.WorkflowGraph{Version: 2, Entry: "check", Nodes: []config.WorkflowNode{
		{ID: "check", Type: "check"},
		{ID: "retry", Type: "loop", Max: 1},
		{ID: "gate", Type: "wait", Mode: "human", Timeout: config.Duration{Duration: time.Hour}},
		{ID: "choose", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", Equals: 7}},
		{ID: "by-release", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", In: []any{42}}},
		{ID: "by-timeout", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", Equals: 43}},
		{ID: "release-verdict", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approved"}},
		{ID: "timeout-verdict", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approved"}},
		{ID: "end", Type: "end", Status: "done"},
	}, Edges: []config.WorkflowEdge{
		graphEdge("check", "pass", "end"), graphEdge("check", "fail", "retry"),
		graphEdge("retry", "repeat", "check"), graphEdge("retry", "exhausted", "gate"),
		graphEdge("gate", "released", "choose"), graphEdge("gate", "timeout", "by-timeout"),
		graphEdge("choose", "true", "end"), graphEdge("choose", "false", "by-release"),
		graphEdge("by-release", "true", "release-verdict"), graphEdge("by-release", "false", "end"),
		graphEdge("by-timeout", "true", "timeout-verdict"), graphEdge("by-timeout", "false", "end"),
		graphEdge("release-verdict", "true", "end"), graphEdge("release-verdict", "false", "end"),
		graphEdge("timeout-verdict", "true", "end"), graphEdge("timeout-verdict", "false", "end"),
	}}
	report := PreflightGraph(graph, GraphPreflightLimit)
	require.False(t, report.Inconclusive)
	require.Len(t, report.Diagnostics, 2)
	codes := make(map[string]int)
	for _, diagnostic := range report.Diagnostics {
		require.NotNil(t, diagnostic.Producer)
		require.Equal(t, "check", diagnostic.Producer.NodeID)
		require.NotNil(t, diagnostic.Producer.ExitCode)
		codes[diagnostic.Node] = *diagnostic.Producer.ExitCode
	}
	require.Equal(t, map[string]int{"release-verdict": 42, "timeout-verdict": 43}, codes)
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
