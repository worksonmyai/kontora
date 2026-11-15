package pipeline

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func graphEdge(from, port, to string) config.WorkflowEdge {
	return config.WorkflowEdge{From: config.WorkflowPort{Node: from, Port: port}, To: to}
}

func executableGraph(kind string) *config.WorkflowGraph {
	success, failure := "complete", "failed"
	if kind == "check" {
		success, failure = "pass", "fail"
	}
	return &config.WorkflowGraph{
		Version: 2, Entry: "run",
		Nodes: []config.WorkflowNode{{ID: "run", Type: kind, Stage: "shared", Command: "true"}, {ID: "yes", Type: "end", Status: "done"}, {ID: "no", Type: "end", Status: "paused"}},
		Edges: []config.WorkflowEdge{graphEdge("run", success, "yes"), graphEdge("run", failure, "no")},
	}
}

func freshGraphState() *ticket.GraphState {
	return &ticket.GraphState{ExecutionID: "execution", DefinitionFingerprint: "definition"}
}

func completeGraphNode(a GraphAdvance) *GraphCompletion {
	return &GraphCompletion{NodeID: a.Stage, VisitID: a.State.VisitID, InvocationID: "invocation-" + a.State.VisitID, Outcome: "complete"}
}

func graphBytes(t *testing.T, value any) []byte {
	t.Helper()
	data, err := yaml.Marshal(value)
	require.NoError(t, err)
	return data
}

func TestAdvanceGraphExecutable(t *testing.T) {
	for _, tc := range []struct {
		name, kind, outcome, verdict, port string
		exit                               int
		declared                           []string
	}{
		{name: "agent success", kind: "agent", outcome: "complete", port: "complete"},
		{name: "agent nonzero", kind: "agent", outcome: "complete", exit: 1, port: "failed", declared: []string{"approve"}},
		{name: "agent failed outcome", kind: "agent", outcome: "failed", port: "failed"},
		{name: "changes is success", kind: "agent", outcome: "complete", verdict: "changes", declared: []string{"approve", "changes"}, port: "complete"},
		{name: "check zero", kind: "check", outcome: "complete", port: "pass"},
		{name: "check nonzero", kind: "check", outcome: "complete", exit: 7, port: "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := executableGraph(tc.kind)
			graph.Nodes[0].Verdicts = tc.declared
			state := freshGraphState()
			graphBefore, stateBefore := graphBytes(t, graph), state.Clone()
			start := AdvanceGraph(graph, "", state, nil)
			require.Equal(t, ticket.StatusTodo, start.Status)
			require.Equal(t, "run", start.Stage)
			require.Equal(t, uint64(1), start.State.Revision)
			require.Equal(t, uint64(1), start.State.Visit)
			require.Equal(t, "execution.1", start.State.VisitID)
			require.Equal(t, 1, start.State.Version)
			require.Equal(t, stateBefore, state)

			again := AdvanceGraph(graph, start.Stage, start.State, nil)
			require.Equal(t, start.Next, again.Next)
			require.Equal(t, start.State.VisitID, again.State.VisitID)
			require.Equal(t, uint64(2), again.State.Revision)
			completion := completeGraphNode(start)
			completion.Outcome, completion.Verdict, completion.ExitCode = tc.outcome, tc.verdict, tc.exit
			completionBefore := *completion
			result := AdvanceGraph(graph, start.Stage, start.State, completion)
			require.Nil(t, result.Next)
			require.Len(t, result.Edges, 1)
			require.Equal(t, tc.port, result.Edges[0].Port)
			require.Equal(t, uint64(2), result.Edges[0].Revision)
			require.Equal(t, result.Edges, result.State.Transitions)
			require.Equal(t, tc.verdict, result.State.LastResult.Verdict)
			require.Equal(t, tc.exit, *result.State.LastResult.ExitCode)
			require.Equal(t, completionBefore, *completion)
			require.Equal(t, graphBefore, graphBytes(t, graph))
			require.Nil(t, start.State.LastResult)
			require.Empty(t, start.State.Transitions)
			want := ticket.StatusDone
			if tc.port == "fail" || tc.port == "failed" {
				want = ticket.StatusPaused
			}
			require.Equal(t, want, result.Status)
		})
	}
}

func TestAdvanceGraphSafetyPrecedence(t *testing.T) {
	for _, kind := range []string{"agent", "check"} {
		for _, tc := range []struct {
			name   string
			change func(*GraphCompletion)
		}{
			{"blocked", func(c *GraphCompletion) { c.Outcome = "blocked" }},
			{"protocol error", func(c *GraphCompletion) { c.Outcome = "protocol_error" }},
			{"cancelled", func(c *GraphCompletion) { c.Outcome = "cancelled" }},
			{"missing report", func(c *GraphCompletion) { c.Outcome = "" }},
			{"unknown outcome", func(c *GraphCompletion) { c.Outcome = "unknown" }},
			{"interrupted", func(c *GraphCompletion) { c.Interrupted = true }},
			{"timeout", func(c *GraphCompletion) { c.TimedOut = true }},
			{"infrastructure", func(c *GraphCompletion) { c.InfrastructureError = true }},
			{"uncertain termination", func(c *GraphCompletion) { c.TerminationUncertain = true }},
			{"wrong producer", func(c *GraphCompletion) { c.NodeID = "other" }},
			{"wrong visit", func(c *GraphCompletion) { c.VisitID = "execution.0" }},
			{"missing invocation", func(c *GraphCompletion) { c.InvocationID = "" }},
			{"undeclared verdict", func(c *GraphCompletion) { c.Verdict = "surprise" }},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				for _, exit := range []int{0, 1} {
					graph := executableGraph(kind)
					start := AdvanceGraph(graph, "", freshGraphState(), nil)
					completion := completeGraphNode(start)
					completion.ExitCode = exit
					tc.change(completion)
					result := AdvanceGraph(graph, start.Stage, start.State, completion)
					require.Equal(t, ticket.StatusPaused, result.Status)
					require.NotEmpty(t, result.Reason)
					require.Equal(t, "run", result.Stage)
					require.Nil(t, result.Next)
					require.Empty(t, result.Edges)
					require.Nil(t, result.State.LastResult)
				}
			})
		}
	}
}

func conditionGraph(field string, equals any) *config.WorkflowGraph {
	graph := executableGraph("agent")
	graph.Nodes[0].Verdicts = []string{"approve", "changes"}
	graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "condition", Type: "condition", When: &config.WorkflowCondition{Field: field, Equals: equals}})
	graph.Edges[0].To = "condition"
	graph.Edges = append(graph.Edges, graphEdge("condition", "true", "yes"), graphEdge("condition", "false", "no"))
	return graph
}

func persistedResultState() *ticket.GraphState {
	s := freshGraphState()
	s.Version, s.Visit, s.VisitID = 1, 1, "execution.1"
	exit := 0
	s.LastResult = &ticket.GraphResult{NodeID: "run", VisitID: s.VisitID, InvocationID: "invocation", Verdict: "approve", Outcome: "complete", ExitCode: &exit}
	return s
}

func TestAdvanceGraphConditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.WorkflowGraph, *ticket.GraphState)
		port   string
	}{
		{"verdict true", func(_ *config.WorkflowGraph, _ *ticket.GraphState) {}, "true"},
		{"verdict false", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.Verdict = "changes" }, "false"},
		{"missing result", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult = nil }, ""},
		{"missing verdict", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.Verdict = "" }, ""},
		{"undeclared verdict", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.Verdict = "unknown" }, ""},
		{"producer has no declaration", func(g *config.WorkflowGraph, _ *ticket.GraphState) {
			g.Nodes[0].Verdicts = nil
			g.Nodes[3].Verdicts = []string{"approve"}
		}, ""},
		{"stale result", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.VisitID = "execution.0" }, ""},
		{"no invocation", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.InvocationID = "" }, ""},
		{"unknown producer", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.NodeID = "absent" }, ""},
		{"control producer", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.NodeID = "condition" }, ""},
		{"blocked persisted", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.Outcome = "blocked" }, ""},
		{"unknown outcome", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.Outcome = "unknown" }, ""},
		{"failed verdict", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.Outcome = "failed" }, ""},
		{"nonzero agent verdict", func(_ *config.WorkflowGraph, s *ticket.GraphState) { *s.LastResult.ExitCode = 1 }, ""},
		{"missing exit", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.LastResult.ExitCode = nil }, ""},
		{"wrong verdict comparison type", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[3].When.Equals = 0 }, ""},
		{"unknown field", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[3].When.Field = "summary" }, ""},
		{"missing predicate", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[3].When = nil }, ""},
		{"agent exit code forbidden", func(g *config.WorkflowGraph, _ *ticket.GraphState) {
			g.Nodes[3].When = &config.WorkflowCondition{Field: "exit_code", Equals: 0}
		}, ""},
		{"check exit true", func(g *config.WorkflowGraph, s *ticket.GraphState) {
			g.Nodes[0].Type = "check"
			s.LastResult.Verdict = ""
			g.Nodes[3].When = &config.WorkflowCondition{Field: "exit_code", Equals: 0}
		}, "true"},
		{"check exit false", func(g *config.WorkflowGraph, s *ticket.GraphState) {
			g.Nodes[0].Type = "check"
			s.LastResult.Verdict = ""
			*s.LastResult.ExitCode = 5
			g.Nodes[3].When = &config.WorkflowCondition{Field: "exit_code", Equals: 0}
		}, "false"},
		{"string exit comparison", func(g *config.WorkflowGraph, s *ticket.GraphState) {
			g.Nodes[0].Type = "check"
			s.LastResult.Verdict = ""
			g.Nodes[3].When = &config.WorkflowCondition{Field: "exit_code", Equals: "0"}
		}, ""},
		{"float exit comparison", func(g *config.WorkflowGraph, s *ticket.GraphState) {
			g.Nodes[0].Type = "check"
			s.LastResult.Verdict = ""
			g.Nodes[3].When = &config.WorkflowCondition{Field: "exit_code", Equals: float64(0)}
		}, ""},
		{"check cannot report verdict", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[0].Type = "check" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph, state := conditionGraph("verdict", "approve"), persistedResultState()
			tc.change(graph, state)
			before := state.Clone()
			result := AdvanceGraph(graph, "condition", state, nil)
			require.Equal(t, before, state)
			require.Equal(t, state.Visit, result.State.Visit)
			if tc.port == "" {
				require.Equal(t, ticket.StatusPaused, result.Status)
				require.Equal(t, "condition", result.Stage)
				require.Empty(t, result.Edges)
				require.NotEmpty(t, result.Reason)
			} else {
				require.Len(t, result.Edges, 1)
				require.Equal(t, tc.port, result.Edges[0].Port)
			}
		})
	}
}

func TestAdvanceGraphMissingVerdict(t *testing.T) {
	graph := conditionGraph("verdict", "approve")
	start := AdvanceGraph(graph, "", freshGraphState(), nil)
	result := AdvanceGraph(graph, start.Stage, start.State, completeGraphNode(start))
	require.Equal(t, ticket.StatusPaused, result.Status)
	require.Equal(t, "missing agent verdict", result.Reason)
	require.Empty(t, result.Edges)
}

func TestAdvanceGraphSharedResultAndRepeatedTemplates(t *testing.T) {
	graph := conditionGraph("verdict", "approve")
	graph.Nodes = append(graph.Nodes,
		config.WorkflowNode{ID: "condition2", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approve"}},
		config.WorkflowNode{ID: "other-review", Type: "agent", Stage: "shared"},
	)
	graph.Edges[2].To = "condition2"
	graph.Edges = append(graph.Edges, graphEdge("condition2", "true", "other-review"), graphEdge("condition2", "false", "no"), graphEdge("other-review", "complete", "yes"), graphEdge("other-review", "failed", "no"))
	start := AdvanceGraph(graph, "", freshGraphState(), nil)
	completion := completeGraphNode(start)
	completion.Verdict = "approve"
	result := AdvanceGraph(graph, start.Stage, start.State, completion)
	require.Equal(t, "other-review", result.Stage)
	require.Equal(t, start.Next.Stage, result.Next.Stage)
	require.NotEqual(t, start.Next.ID, result.Next.ID)
	require.Equal(t, "execution.2", result.State.VisitID)
	require.Nil(t, result.State.LastResult)
	require.Len(t, result.Edges, 3)
	for _, edge := range result.Edges {
		require.Equal(t, uint64(2), edge.Revision)
	}
	stale := AdvanceGraph(graph, result.Stage, result.State, completion)
	require.Equal(t, ticket.StatusPaused, stale.Status)
	require.Empty(t, stale.Edges)
	start.Next.Verdicts[0] = "changed"
	require.Equal(t, "approve", graph.Nodes[0].Verdicts[0])
}

func TestAdvanceGraphLoopAllowance(t *testing.T) {
	for _, max := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(max), func(t *testing.T) {
			graph := executableGraph("agent")
			graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "repair", Type: "loop", Max: max})
			graph.Edges[0].To = "repair"
			graph.Edges = append(graph.Edges, graphEdge("repair", "repeat", "run"), graphEdge("repair", "exhausted", "yes"))
			current := AdvanceGraph(graph, "", freshGraphState(), nil)
			for repeats := 0; repeats <= max; repeats++ {
				require.NotNil(t, current.Next)
				require.Equal(t, uint64(repeats+1), current.State.Visit)
				before := current.State.Clone()
				next := AdvanceGraph(graph, current.Stage, current.State, completeGraphNode(current))
				require.Equal(t, before, current.State)
				require.Len(t, next.Edges, 2)
				if repeats < max {
					require.Equal(t, "repeat", next.Edges[1].Port)
					require.Equal(t, repeats+1, next.State.LoopCounts["repair"])
					require.Equal(t, repeats+1, next.Edges[1].LoopCount)
				} else {
					require.Equal(t, "exhausted", next.Edges[1].Port)
					require.Equal(t, max, next.State.LoopCounts["repair"])
					require.Equal(t, max, next.Edges[1].LoopCount)
					require.Equal(t, ticket.StatusDone, next.Status)
					require.Contains(t, next.Reason, "exhausted")
					require.Nil(t, next.Next)
				}
				// Reloading state does not replenish the allowance.
				var reloaded ticket.GraphState
				require.NoError(t, yaml.Unmarshal(graphBytes(t, next.State), &reloaded))
				next.State = &reloaded
				current = next
			}
			require.Len(t, current.State.Transitions, 2*(max+1))
		})
	}
}

func TestAdvanceGraphMultipleLoops(t *testing.T) {
	graph := &config.WorkflowGraph{Version: 2, Entry: "a", Nodes: []config.WorkflowNode{
		{ID: "a", Type: "agent", Stage: "shared"}, {ID: "b", Type: "agent", Stage: "shared"},
		{ID: "l1", Type: "loop", Max: 1}, {ID: "l2", Type: "loop", Max: 2}, {ID: "end", Type: "end", Status: "done"},
	}, Edges: []config.WorkflowEdge{
		graphEdge("a", "complete", "l1"), graphEdge("a", "failed", "end"),
		graphEdge("b", "complete", "l2"), graphEdge("b", "failed", "end"),
		graphEdge("l1", "repeat", "b"), graphEdge("l1", "exhausted", "l2"),
		graphEdge("l2", "repeat", "a"), graphEdge("l2", "exhausted", "end"),
	}}
	current := AdvanceGraph(graph, "", freshGraphState(), nil)
	for _, stage := range []string{"a", "b", "a", "a"} {
		require.Equal(t, stage, current.Stage)
		current = AdvanceGraph(graph, current.Stage, current.State, completeGraphNode(current))
	}
	require.Nil(t, current.Next)
	require.Equal(t, ticket.StatusDone, current.Status)
	require.Equal(t, map[string]int{"l1": 1, "l2": 2}, current.State.LoopCounts)
	require.Equal(t, uint64(4), current.State.Visit)
	require.Len(t, current.Edges, 3)
	require.Equal(t, "exhausted", current.Edges[1].Port)
	require.Equal(t, "exhausted", current.Edges[2].Port)
}

func TestAdvanceGraphEntryControlsAndEnds(t *testing.T) {
	for _, status := range []string{"done", "human_review", "paused", "awaiting_release"} {
		t.Run(status, func(t *testing.T) {
			graph := &config.WorkflowGraph{Version: 2, Entry: "loop", Nodes: []config.WorkflowNode{{ID: "loop", Type: "loop", Max: 0}, {ID: "end", Type: "end", Status: status}}, Edges: []config.WorkflowEdge{graphEdge("loop", "exhausted", "end")}}
			result := AdvanceGraph(graph, "", freshGraphState(), nil)
			require.Equal(t, ticket.Status(status), result.Status)
			require.Equal(t, "end", result.Stage)
			require.Nil(t, result.Next)
			require.Zero(t, result.State.Visit)
			require.Empty(t, result.State.VisitID)
			require.Len(t, result.Edges, 1)
		})
	}
}

func TestAdvanceGraphInvalidState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(**config.WorkflowGraph, *string, **ticket.GraphState, **GraphCompletion)
	}{
		{"nil graph", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, _ **GraphCompletion) { *g = nil }},
		{"graph version", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, _ **GraphCompletion) {
			(*g).Version = 99
		}},
		{"nil state", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) { *s = nil }},
		{"missing execution", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).ExecutionID = ""
		}},
		{"missing fingerprint", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).DefinitionFingerprint = ""
		}},
		{"state version", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).Version = 99
		}},
		{"revision overflow", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).Revision = math.MaxUint64
		}},
		{"cursor missing", func(_ **config.WorkflowGraph, stage *string, _ **ticket.GraphState, _ **GraphCompletion) {
			*stage = "absent"
		}},
		{"cursor reset", func(_ **config.WorkflowGraph, stage *string, _ **ticket.GraphState, _ **GraphCompletion) { *stage = "" }},
		{"missing visit", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).Visit = 0
			(*s).VisitID = ""
		}},
		{"visit mismatch", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).VisitID = "execution.2"
		}},
		{"unknown loop", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).LoopCounts = map[string]int{"missing": 1}
		}},
		{"nonloop counter", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).LoopCounts = map[string]int{"run": 1}
		}},
		{"negative counter", func(g **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*g).Nodes = append((*g).Nodes, config.WorkflowNode{ID: "loop", Type: "loop", Max: 1})
			(*s).LoopCounts = map[string]int{"loop": -1}
		}},
		{"excess counter", func(g **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*g).Nodes = append((*g).Nodes, config.WorkflowNode{ID: "loop", Type: "loop", Max: 1})
			(*s).LoopCounts = map[string]int{"loop": 2}
		}},
		{"duplicate node", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, _ **GraphCompletion) {
			(*g).Nodes = append((*g).Nodes, (*g).Nodes[0])
		}},
		{"duplicate edge", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, _ **GraphCompletion) {
			(*g).Edges = append((*g).Edges, (*g).Edges[0])
		}},
		{"missing target", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, _ **GraphCompletion) {
			(*g).Edges[0].To = "missing"
		}},
		{"missing entry", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, _ **GraphCompletion) {
			(*g).Entry = "missing"
		}},
		{"missing port", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, _ **GraphCompletion) {
			(*g).Edges = (*g).Edges[1:]
		}},
		{"unknown kind", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, c **GraphCompletion) {
			(*g).Nodes[0].Type = "unknown"
			*c = nil
		}},
		{"completion on control", func(g **config.WorkflowGraph, _ *string, _ **ticket.GraphState, _ **GraphCompletion) {
			(*g).Nodes[0].Type = "loop"
		}},
		{"already completed", func(_ **config.WorkflowGraph, _ *string, s **ticket.GraphState, _ **GraphCompletion) {
			(*s).LastResult = persistedResultState().LastResult
			(*s).LastResult.Verdict = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := executableGraph("agent")
			start := AdvanceGraph(graph, "", freshGraphState(), nil)
			stage, state, completion := start.Stage, start.State, completeGraphNode(start)
			tc.change(&graph, &stage, &state, &completion)
			before := state.Clone()
			result := AdvanceGraph(graph, stage, state, completion)
			require.Equal(t, ticket.StatusPaused, result.Status)
			require.NotEmpty(t, result.Reason)
			require.Nil(t, result.Next)
			require.Empty(t, result.Edges)
			require.Equal(t, before, state)
		})
	}
}

func TestAdvanceGraphControlSafety(t *testing.T) {
	for _, tc := range []struct {
		name string
		node config.WorkflowNode
	}{
		{name: "entry condition without result", node: config.WorkflowNode{ID: "entry", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approve"}}},
		{name: "negative allowance", node: config.WorkflowNode{ID: "entry", Type: "loop", Max: -1}},
		{name: "missing repeat edge", node: config.WorkflowNode{ID: "entry", Type: "loop", Max: 1}},
		{name: "missing exhausted edge", node: config.WorkflowNode{ID: "entry", Type: "loop", Max: 0}},
		{name: "unknown kind", node: config.WorkflowNode{ID: "entry", Type: "unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := &config.WorkflowGraph{Version: 2, Entry: "entry", Nodes: []config.WorkflowNode{tc.node}}
			result := AdvanceGraph(graph, "", freshGraphState(), nil)
			require.Equal(t, ticket.StatusPaused, result.Status)
			require.NotEmpty(t, result.Reason)
			require.Nil(t, result.Next)
			require.Empty(t, result.Edges)
		})
	}
	for _, status := range []string{"", "todo", "open", "in_progress", "cancelled", "archived", "closed", "next", "bad-status"} {
		t.Run("end/"+status, func(t *testing.T) {
			graph := &config.WorkflowGraph{Version: 2, Entry: "end", Nodes: []config.WorkflowNode{{ID: "end", Type: "end", Status: status}}}
			result := AdvanceGraph(graph, "", freshGraphState(), nil)
			require.Equal(t, ticket.StatusPaused, result.Status)
			require.Equal(t, "invalid end status", result.Reason)
		})
	}
}

func TestAdvanceGraphMissingConditionEdge(t *testing.T) {
	graph := conditionGraph("verdict", "approve")
	graph.Edges = graph.Edges[:2]
	result := AdvanceGraph(graph, "condition", persistedResultState(), nil)
	require.Equal(t, ticket.StatusPaused, result.Status)
	require.Equal(t, "missing condition transition", result.Reason)
	require.Empty(t, result.Edges)
}

func TestAdvanceGraphVisitOverflow(t *testing.T) {
	graph := executableGraph("agent")
	graph.Edges[0].To = "run"
	state := persistedResultState()
	state.LastResult = nil
	state.Visit = math.MaxUint64
	state.VisitID = graphVisitID(state)
	completion := &GraphCompletion{NodeID: "run", VisitID: state.VisitID, InvocationID: "invocation", Outcome: "complete"}
	result := AdvanceGraph(graph, "run", state, completion)
	require.Equal(t, ticket.StatusPaused, result.Status)
	require.Equal(t, "graph visit overflow", result.Reason)
	require.Equal(t, uint64(math.MaxUint64), result.State.Visit)
	require.Nil(t, result.Next)
}

func declaredWaitGraph(mode string) *config.WorkflowGraph {
	node := config.WorkflowNode{ID: "wait", Type: "wait", Mode: mode}
	if mode == "deployment" {
		node.Environment = "production"
	}
	return &config.WorkflowGraph{Version: 2, Entry: "wait", Nodes: []config.WorkflowNode{
		node, {ID: "done", Type: "end", Status: "done"}, {ID: "expired", Type: "end", Status: "human_review"},
	}, Edges: []config.WorkflowEdge{graphEdge("wait", "released", "done")}}
}

// The daemon supplies identity, timing and release evidence, not traversal.
func registerGraphWait(a GraphAdvance) *ticket.GraphState {
	s := a.State.Clone()
	s.Wait = &ticket.WorkflowWait{
		ID: "wait-id", Kind: a.Wait.Mode, Phase: "pending", ExecutionID: s.ExecutionID,
		NodeID: a.Stage, VisitID: s.VisitID, Environment: a.Wait.Environment,
		Receipt: &ticket.WorkflowReceipt{ID: "receipt"},
	}
	return s
}

func TestAdvanceGraphEntryWait(t *testing.T) {
	for _, mode := range []string{"human", "deployment"} {
		t.Run(mode, func(t *testing.T) {
			graph, state := declaredWaitGraph(mode), freshGraphState()
			state.Version = 2
			before := state.Clone()
			graphBefore := graphBytes(t, graph)
			parked := AdvanceGraph(graph, "", state, nil)
			require.Equal(t, ticket.StatusPaused, parked.Status)
			require.Empty(t, parked.Reason)
			require.Equal(t, "wait", parked.Stage)
			require.Equal(t, &graph.Nodes[0], parked.Wait)
			require.Nil(t, parked.Next)
			require.Nil(t, parked.State.Wait)
			require.Nil(t, parked.State.LastResult)
			require.Zero(t, parked.State.Visit)
			require.Empty(t, parked.State.VisitID)
			require.Empty(t, parked.State.Transitions)
			require.Empty(t, parked.State.WaitHistory)
			require.Equal(t, uint64(1), parked.State.Revision)
			require.Equal(t, before, state)
			require.Equal(t, graphBefore, graphBytes(t, graph))

			registered := registerGraphWait(parked)
			registeredBefore := registered.Clone()
			blocked := AdvanceGraph(graph, parked.Stage, registered, nil)
			require.Equal(t, ticket.StatusPaused, blocked.Status)
			require.NotEmpty(t, blocked.Reason)
			require.Nil(t, blocked.Next)
			require.Nil(t, blocked.Wait)
			require.Empty(t, blocked.Edges)
			require.Equal(t, registered.Wait, blocked.State.Wait)

			done := ReleaseGraphWait(graph, parked.Stage, registered, "released")
			require.Equal(t, ticket.StatusDone, done.Status)
			require.Equal(t, "done", done.Stage)
			require.Nil(t, done.Next)
			require.Nil(t, done.Wait)
			require.Nil(t, done.State.Wait)
			require.Zero(t, done.State.Visit)
			require.Nil(t, done.State.LastResult)
			require.Equal(t, []ticket.WorkflowWait{*registered.Wait}, done.State.WaitHistory)
			require.Equal(t, uint64(2), done.State.Revision)
			require.Equal(t, []ticket.GraphTransition{{From: "wait", Port: "released", To: "done", Revision: 2}}, done.Edges)
			require.Equal(t, done.Edges, done.State.Transitions)
			require.Equal(t, registeredBefore, registered)
			done.State.WaitHistory[0].Receipt.ID = "changed"
			require.Equal(t, "receipt", registered.Wait.Receipt.ID)
			parked.Wait.Environment = "changed"
			require.Equal(t, graphBefore, graphBytes(t, graph))
		})
	}
}

func TestReleaseGraphWaitRouting(t *testing.T) {
	for _, tc := range []struct {
		name, port, target string
		status             ticket.Status
	}{
		{"release", "released", "done", ticket.StatusDone},
		{"timeout", "timeout", "expired", ticket.StatusHumanReview},
		{"release through loop", "released", "loop", ticket.StatusDone},
		{"release to another wait", "released", "second", ticket.StatusPaused},
		{"release to executable", "released", "run", ticket.StatusTodo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := declaredWaitGraph("human")
			graph.Nodes[0].Timeout = config.Duration{Duration: time.Hour}
			graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "loop", Type: "loop"}, config.WorkflowNode{ID: "second", Type: "wait", Mode: "human"}, config.WorkflowNode{ID: "run", Type: "agent"})
			graph.Edges[0].To = tc.target
			graph.Edges = append(graph.Edges, graphEdge("wait", "timeout", "expired"), graphEdge("loop", "exhausted", "done"), graphEdge("second", "released", "done"))
			state := freshGraphState()
			state.Version = 2
			parked := AdvanceGraph(graph, "", state, nil)
			require.NotNil(t, parked.Wait)
			result := ReleaseGraphWait(graph, parked.Stage, registerGraphWait(parked), tc.port)
			require.False(t, result.Rejected)
			require.Equal(t, tc.status, result.Status)
			if tc.target == "loop" {
				require.Equal(t, "loop loop exhausted", result.Reason)
			} else {
				require.Empty(t, result.Reason)
			}
			if tc.target == "run" {
				require.Equal(t, "run", result.Next.ID)
				require.Equal(t, uint64(1), result.State.Visit)
			} else {
				require.Nil(t, result.Next)
				require.Zero(t, result.State.Visit)
			}
			require.Len(t, result.State.WaitHistory, 1)
			require.Nil(t, result.State.Wait)
			require.Equal(t, tc.port, result.Edges[0].Port)
			if tc.target == "second" {
				require.Equal(t, "second", result.Wait.ID)
			} else {
				require.Nil(t, result.Wait)
			}
		})
	}
}

func TestAdvanceGraphWaitPreservesResult(t *testing.T) {
	for _, verdict := range []string{"approve", "changes"} {
		t.Run(verdict, func(t *testing.T) {
			graph := conditionGraph("verdict", "approve")
			graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "wait", Type: "wait", Mode: "human"})
			graph.Edges[0].To = "wait"
			graph.Edges = append(graph.Edges, graphEdge("wait", "released", "condition"))
			state := freshGraphState()
			state.Version = 2
			start := AdvanceGraph(graph, "", state, nil)
			completion := completeGraphNode(start)
			completion.Verdict = verdict
			parked := AdvanceGraph(graph, start.Stage, start.State, completion)
			require.Equal(t, ticket.StatusPaused, parked.Status)
			require.Equal(t, "wait", parked.Stage)
			require.Nil(t, parked.Next)
			require.NotNil(t, parked.Wait)
			require.Equal(t, start.State.VisitID, parked.State.VisitID)
			require.Equal(t, verdict, parked.State.LastResult.Verdict)
			require.Len(t, parked.Edges, 1)
			registered := registerGraphWait(parked)
			result := ReleaseGraphWait(graph, parked.Stage, registered, "released")
			require.Equal(t, registered.LastResult, result.State.LastResult)
			require.Equal(t, start.State.Visit, result.State.Visit)
			require.Len(t, result.Edges, 2)
			if verdict == "approve" {
				require.Equal(t, ticket.StatusDone, result.Status)
				require.Equal(t, "true", result.Edges[1].Port)
			} else {
				require.Equal(t, ticket.StatusPaused, result.Status)
				require.Equal(t, "false", result.Edges[1].Port)
			}
		})
	}
}

func TestReleaseGraphWaitInvalidState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.WorkflowGraph, *string, *ticket.GraphState, *string)
	}{
		{"missing wait", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.Wait = nil }},
		{"missing wait ID", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.Wait.ID = "" }},
		{"wrong execution", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.Wait.ExecutionID = "old" }},
		{"wrong node", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.Wait.NodeID = "other" }},
		{"wrong visit", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) {
			s.Wait.VisitID = "execution.1"
		}},
		{"wrong kind", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.Wait.Kind = "timer" }},
		{"agent invocation", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) {
			s.Wait.InvocationID = "invocation"
		}},
		{"wrong environment", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) {
			s.Wait.Environment = "other"
		}},
		{"wrong cursor", func(_ *config.WorkflowGraph, stage *string, _ *ticket.GraphState, _ *string) { *stage = "done" }},
		{"empty cursor", func(_ *config.WorkflowGraph, stage *string, _ *ticket.GraphState, _ *string) { *stage = "" }},
		{"unknown cursor", func(_ *config.WorkflowGraph, stage *string, _ *ticket.GraphState, _ *string) { *stage = "unknown" }},
		{"legacy state", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.Version = 1 }},
		{"unsupported state", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.Version = 3 }},
		{"revision overflow", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.Revision = math.MaxUint64 }},
		{"missing execution", func(_ *config.WorkflowGraph, _ *string, s *ticket.GraphState, _ *string) { s.ExecutionID = "" }},
		{"invalid port", func(_ *config.WorkflowGraph, _ *string, _ *ticket.GraphState, port *string) { *port = "complete" }},
		{"timeout disabled", func(_ *config.WorkflowGraph, _ *string, _ *ticket.GraphState, port *string) { *port = "timeout" }},
		{"missing release edge", func(g *config.WorkflowGraph, _ *string, _ *ticket.GraphState, _ *string) { g.Edges = nil }},
		{"missing timeout edge", func(g *config.WorkflowGraph, _ *string, _ *ticket.GraphState, _ *string) {
			g.Nodes[0].Timeout.Duration = time.Hour
		}},
		{"unexpected timeout edge", func(g *config.WorkflowGraph, _ *string, _ *ticket.GraphState, _ *string) {
			g.Edges = append(g.Edges, graphEdge("wait", "timeout", "expired"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph, state := declaredWaitGraph("human"), freshGraphState()
			state.Version = 2
			parked := AdvanceGraph(graph, "", state, nil)
			state, stage, port := registerGraphWait(parked), parked.Stage, "released"
			tc.change(graph, &stage, state, &port)
			before := state.Clone()
			result := ReleaseGraphWait(graph, stage, state, port)
			require.True(t, result.Rejected)
			require.Equal(t, ticket.StatusPaused, result.Status)
			require.NotEmpty(t, result.Reason)
			require.Nil(t, result.Next)
			require.Nil(t, result.Wait)
			require.Empty(t, result.Edges)
			require.Empty(t, result.State.WaitHistory)
			require.Equal(t, before.Wait, result.State.Wait)
			require.Equal(t, before, state)
		})
	}
}

func TestAdvanceGraphWaitSafety(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.WorkflowGraph, *ticket.GraphState)
	}{
		{"legacy state", func(_ *config.WorkflowGraph, s *ticket.GraphState) { s.Version = 1 }},
		{"unknown mode", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[0].Mode = "timer" }},
		{"human environment", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[0].Environment = "production" }},
		{"deployment environment missing", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[0].Mode = "deployment" }},
		{"negative timeout", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[0].Timeout.Duration = -time.Second }},
		{"missing release edge", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Edges = nil }},
		{"missing timeout edge", func(g *config.WorkflowGraph, _ *ticket.GraphState) { g.Nodes[0].Timeout.Duration = time.Hour }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph, state := declaredWaitGraph("human"), freshGraphState()
			state.Version = 2
			tc.change(graph, state)
			result := AdvanceGraph(graph, "", state, nil)
			require.Equal(t, ticket.StatusPaused, result.Status)
			require.NotEmpty(t, result.Reason)
			require.Nil(t, result.Next)
			require.Nil(t, result.Wait)
			require.Empty(t, result.Edges)
			require.Zero(t, result.State.Visit)
		})
	}
	for _, kind := range []string{"agent", "check"} {
		for _, completion := range []bool{false, true} {
			t.Run(fmt.Sprintf("active wait cannot bypass/%s/completion=%t", kind, completion), func(t *testing.T) {
				graph, state := executableGraph(kind), freshGraphState()
				state.Version = 2
				start := AdvanceGraph(graph, "", state, nil)
				// Even malformed wait metadata must never permit normal pickup.
				start.State.Wait = &ticket.WorkflowWait{}
				var c *GraphCompletion
				if completion {
					c = completeGraphNode(start)
				}
				result := AdvanceGraph(graph, start.Stage, start.State, c)
				require.Equal(t, ticket.StatusPaused, result.Status)
				require.NotEmpty(t, result.Reason)
				require.Nil(t, result.Next)
				require.Empty(t, result.Edges)
				require.Nil(t, result.State.LastResult)
			})
		}
	}
}

func TestAdvanceGraphContinuation(t *testing.T) {
	for _, kind := range []string{"agent", "check"} {
		t.Run(kind, func(t *testing.T) {
			graph, state := executableGraph(kind), freshGraphState()
			graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "other", Type: kind})
			if kind == "agent" {
				graph.Nodes[0].Wake = &config.WorkflowWake{Max: 3}
			}
			graph.Edges[0].To = "other"
			state.Version = 2
			start := AdvanceGraph(graph, "", state, nil)
			start.State.Wake = &ticket.WakeContext{Reason: "ready", Count: 2, WaitID: "wait-id", Next: "Continue"}
			start.State.WakeCount = 2
			start.State.Continuation = true
			start.State.WaitHistory = []ticket.WorkflowWait{{ID: "previous-wait"}}
			before := start.State.Clone()
			resumed := AdvanceGraph(graph, start.Stage, start.State, nil)
			require.Equal(t, ticket.StatusTodo, resumed.Status)
			require.Equal(t, start.Next, resumed.Next)
			require.Equal(t, start.State.Visit, resumed.State.Visit)
			require.Equal(t, start.State.VisitID, resumed.State.VisitID)
			require.Equal(t, start.State.Wake, resumed.State.Wake)
			require.Equal(t, 2, resumed.State.WakeCount)
			require.True(t, resumed.State.Continuation)
			require.Equal(t, before, start.State)
			if kind == "agent" {
				resumed.Next.Wake.Max = 99
				require.Equal(t, 3, graph.Nodes[0].Wake.Max)
			}
			next := AdvanceGraph(graph, resumed.Stage, resumed.State, completeGraphNode(resumed))
			require.Equal(t, "other", next.Stage)
			require.NotNil(t, next.Next)
			require.Equal(t, start.State.Visit+1, next.State.Visit)
			require.Nil(t, next.State.Wake)
			require.Zero(t, next.State.WakeCount)
			require.False(t, next.State.Continuation)
			require.Equal(t, start.State.WaitHistory, next.State.WaitHistory)
		})
	}
}

func TestAdvanceGraphStateVersionCompatibility(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			graph, state := executableGraph("agent"), freshGraphState()
			state.Version = version
			start := AdvanceGraph(graph, "", state, nil)
			want := max(version, 1)
			require.Equal(t, want, start.State.Version)
			require.Equal(t, ticket.StatusTodo, start.Status)
			result := AdvanceGraph(graph, start.Stage, start.State, completeGraphNode(start))
			require.Equal(t, want, result.State.Version)
			require.Equal(t, ticket.StatusDone, result.Status)
			require.Nil(t, result.State.Wait)
			require.Nil(t, result.State.Wake)
			require.Zero(t, result.State.WakeCount)
			require.False(t, result.State.Continuation)
			require.Empty(t, result.State.WaitHistory)
		})
	}
}

func TestAdvanceGraphRuntimeControlCycle(t *testing.T) {
	graph := &config.WorkflowGraph{Version: 2, Entry: "loop", Nodes: []config.WorkflowNode{{ID: "loop", Type: "loop", Max: 100}}, Edges: []config.WorkflowEdge{graphEdge("loop", "repeat", "loop"), graphEdge("loop", "exhausted", "loop")}}
	result := AdvanceGraph(graph, "", freshGraphState(), nil)
	require.Equal(t, ticket.StatusPaused, result.Status)
	require.Contains(t, result.Reason, "traversal limit")
	require.LessOrEqual(t, len(result.Edges), len(graph.Nodes)+1)
}
