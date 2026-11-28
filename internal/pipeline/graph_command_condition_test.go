package pipeline

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func commandConditionNode(id string) config.WorkflowNode {
	return config.WorkflowNode{ID: id, Type: "condition", Command: "test -f ready", Timeout: config.Duration{Duration: 5 * time.Second}}
}

func commandConditionGraph() *config.WorkflowGraph {
	g := executableGraph("check")
	g.Nodes = append(g.Nodes, commandConditionNode("probe"))
	g.Edges[1].To = "probe"
	g.Edges = append(g.Edges, graphEdge("probe", "true", "yes"), graphEdge("probe", "false", "no"))
	return g
}

func completeCommandConditionProducer(t *testing.T, g *config.WorkflowGraph, state *ticket.GraphState) GraphAdvance {
	t.Helper()
	start := AdvanceGraph(g, "", state, nil)
	require.NotNil(t, start.Next)
	completion := completeGraphNode(start)
	completion.ExitCode = 7 // Deliberately different from the command's branch codes.
	return AdvanceGraph(g, start.Stage, start.State, completion)
}

func reachCommandCondition(t *testing.T, g *config.WorkflowGraph, state *ticket.GraphState) GraphAdvance {
	t.Helper()
	pending := completeCommandConditionProducer(t, g, state)
	require.False(t, pending.Rejected, pending.Reason)
	require.Equal(t, "probe", pending.Stage)
	require.NotNil(t, pending.Condition)
	return pending
}

func TestAdvanceGraphCommandConditionPickup(t *testing.T) {
	g := commandConditionGraph()
	pending := reachCommandCondition(t, g, freshGraphState())
	require.Equal(t, ticket.StatusTodo, pending.Status)
	require.Nil(t, pending.Next)
	require.Nil(t, pending.Wait)
	require.Equal(t, commandConditionNode("probe"), *pending.Condition)
	require.Equal(t, []ticket.GraphTransition{{From: "run", Port: "fail", To: "probe", Revision: 2}}, pending.Edges)
	require.Equal(t, pending.Edges, pending.State.Transitions)
	require.Equal(t, uint64(1), pending.State.Visit)
	require.Equal(t, "execution.1", pending.State.VisitID)
	exit := 7
	require.Equal(t, &ticket.GraphResult{NodeID: "run", VisitID: "execution.1", InvocationID: "invocation-execution.1", Outcome: "complete", ExitCode: &exit}, pending.State.LastResult)

	before := pending.State.Clone()
	pickedUp := AdvanceGraph(g, pending.Stage, pending.State, nil)
	require.False(t, pickedUp.Rejected, pickedUp.Reason)
	require.Equal(t, ticket.StatusTodo, pickedUp.Status)
	require.Equal(t, pending.Stage, pickedUp.Stage)
	require.Equal(t, pending.Condition, pickedUp.Condition)
	require.Nil(t, pickedUp.Next)
	require.Nil(t, pickedUp.Wait)
	require.Empty(t, pickedUp.Edges)
	require.Equal(t, before, pending.State)
	before.Revision++
	require.Equal(t, before, pickedUp.State)
}

func TestResolveGraphConditionExitCodes(t *testing.T) {
	for _, tc := range []struct {
		exit         int
		port, target string
		status       ticket.Status
	}{
		{0, "true", "yes", ticket.StatusDone},
		{1, "false", "no", ticket.StatusPaused},
		{2, "", "probe", ticket.StatusPaused},
	} {
		t.Run(fmt.Sprintf("exit_%d", tc.exit), func(t *testing.T) {
			g := commandConditionGraph()
			pending := reachCommandCondition(t, g, freshGraphState())
			before := pending.State.Clone()
			result := ResolveGraphCondition(g, pending.Stage, pending.State, GraphConditionResult{NodeID: "probe", EvaluationID: "eval-42", ExitCode: tc.exit})
			require.Equal(t, before, pending.State)
			if tc.exit == 2 {
				requireRejectedCommandCondition(t, result, tc.target, "condition probe exited with code 2", before)
				return
			}
			require.False(t, result.Rejected, result.Reason)
			require.Equal(t, tc.target, result.Stage)
			require.Equal(t, tc.status, result.Status)
			require.Empty(t, result.Reason)
			require.Nil(t, result.Next)
			require.Nil(t, result.Condition)
			require.Nil(t, result.Wait)
			before.Revision++
			edge := ticket.GraphTransition{From: "probe", Port: tc.port, To: tc.target, Revision: before.Revision, EvaluationID: "eval-42"}
			require.Equal(t, []ticket.GraphTransition{edge}, result.Edges)
			before.Transitions = append(before.Transitions, edge)
			require.Equal(t, before, result.State)

			var reloaded ticket.GraphState
			require.NoError(t, yaml.Unmarshal(graphBytes(t, result.State), &reloaded))
			require.Equal(t, before, &reloaded)
		})
	}
}

func requireRejectedCommandCondition(t *testing.T, result GraphAdvance, stage, reason string, before *ticket.GraphState) {
	t.Helper()
	require.True(t, result.Rejected)
	require.Equal(t, ticket.StatusPaused, result.Status)
	require.Equal(t, stage, result.Stage)
	require.Equal(t, reason, result.Reason)
	require.Nil(t, result.Next)
	require.Nil(t, result.Condition)
	require.Nil(t, result.Wait)
	require.Empty(t, result.Edges)
	want := before.Clone()
	want.Revision++
	require.Equal(t, want, result.State)
}

func TestResolveGraphConditionSafetyOrder(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		completion   GraphConditionResult
	}{
		{"uncertain termination", "termination is uncertain", GraphConditionResult{TerminationUncertain: true, Interrupted: true, TimedOut: true, InfrastructureError: true}},
		{"interrupted", "interrupted", GraphConditionResult{Interrupted: true, TimedOut: true, InfrastructureError: true}},
		{"timeout", "timed out", GraphConditionResult{TimedOut: true, InfrastructureError: true}},
		{"infrastructure", "infrastructure error", GraphConditionResult{InfrastructureError: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := commandConditionGraph()
			pending := reachCommandCondition(t, g, freshGraphState())
			before := pending.State.Clone()
			for _, exit := range []int{0, 1, 2} {
				t.Run(fmt.Sprintf("exit_%d", exit), func(t *testing.T) {
					completion := tc.completion
					completion.NodeID, completion.EvaluationID, completion.ExitCode = "probe", "eval-42", exit
					result := ResolveGraphCondition(g, pending.Stage, pending.State, completion)
					requireRejectedCommandCondition(t, result, pending.Stage, "condition probe "+tc.reason, before)
					require.Equal(t, before, pending.State)
				})
			}
		})
	}
}

func TestResolveGraphConditionCursor(t *testing.T) {
	const mismatch = "result does not match the current command condition"
	for _, tc := range []struct{ name, stage, nodeID, evaluationID string }{
		{"wrong cursor kind", "run", "run", "eval-42"},
		{"wrong result node", "probe", "run", "eval-42"},
		{"missing evaluation ID", "probe", "probe", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := commandConditionGraph()
			pending := reachCommandCondition(t, g, freshGraphState())
			before := pending.State.Clone()
			result := ResolveGraphCondition(g, tc.stage, pending.State, GraphConditionResult{NodeID: tc.nodeID, EvaluationID: tc.evaluationID})
			requireRejectedCommandCondition(t, result, tc.stage, mismatch, before)
			require.Equal(t, before, pending.State)
		})
	}
}

func TestResolveGraphConditionActiveWait(t *testing.T) {
	g := commandConditionGraph()
	g.Nodes = append(g.Nodes, config.WorkflowNode{ID: "gate", Type: "wait", Mode: "human"})
	g.Edges[1].To = "gate" // run.fail
	g.Edges = append(g.Edges, graphEdge("gate", "released", "probe"))
	parked := completeCommandConditionProducer(t, g, freshGraphState())
	require.NotNil(t, parked.Wait)
	registered := registerGraphWait(parked)
	before := registered.Clone()

	// Even a caller supplying a matching command cursor cannot bypass a registered wait.
	result := ResolveGraphCondition(g, "probe", registered, GraphConditionResult{NodeID: "probe", EvaluationID: "eval-42"})
	requireRejectedCommandCondition(t, result, "probe", "graph has an active wait; release or wake it before advancing", before)
	require.Equal(t, before, registered)
}

func TestResolveGraphConditionContinuation(t *testing.T) {
	for _, tc := range []struct {
		node     config.WorkflowNode
		status   ticket.Status
		preserve bool
	}{
		{config.WorkflowNode{ID: "next", Type: "agent"}, ticket.StatusTodo, true},
		{commandConditionNode("next"), ticket.StatusTodo, true},
		{config.WorkflowNode{ID: "next", Type: "wait", Mode: "human"}, ticket.StatusPaused, false},
		{config.WorkflowNode{ID: "next", Type: "end", Status: "done"}, ticket.StatusDone, false},
	} {
		t.Run(tc.node.Type, func(t *testing.T) {
			g := commandConditionGraph()
			g.Nodes = append(g.Nodes, tc.node)
			g.Edges[2].To = tc.node.ID // probe.true
			switch tc.node.Type {
			case "agent":
				g.Edges = append(g.Edges, graphEdge("next", "complete", "yes"), graphEdge("next", "failed", "no"))
			case "condition":
				g.Edges = append(g.Edges, graphEdge("next", "true", "yes"), graphEdge("next", "false", "no"))
			case "wait":
				g.Edges = append(g.Edges, graphEdge("next", "released", "yes"))
			}
			state := freshGraphState()
			state.Version = 2
			pending := reachCommandCondition(t, g, state)
			pending.State.Continuation = true
			pending.State.Wake = &ticket.WakeContext{Reason: "ready", WaitID: "prior-wait", Receipt: &ticket.WorkflowReceipt{ID: "receipt"}}
			pending.State.WaitHistory = []ticket.WorkflowWait{{ID: "prior-wait"}}
			before := pending.State.Clone()
			result := ResolveGraphCondition(g, pending.Stage, pending.State, GraphConditionResult{NodeID: "probe", EvaluationID: "eval-42"})
			require.False(t, result.Rejected, result.Reason)
			require.Equal(t, tc.node.ID, result.Stage)
			require.Equal(t, tc.status, result.Status)
			require.Equal(t, tc.preserve, result.State.Continuation)
			require.Equal(t, before.WaitHistory, result.State.WaitHistory)
			require.Equal(t, before, pending.State)
			if tc.preserve {
				require.Equal(t, before.Wake, result.State.Wake)
				retry := AdvanceGraph(g, result.Stage, result.State, nil)
				require.False(t, retry.Rejected, retry.Reason)
				require.Empty(t, retry.Edges)
				require.True(t, retry.State.Continuation)
				require.Equal(t, before.Wake, retry.State.Wake)
			} else {
				require.Nil(t, result.State.Wake)
			}
			switch tc.node.Type {
			case "agent":
				require.Equal(t, &tc.node, result.Next)
				require.Equal(t, before.Visit+1, result.State.Visit)
				require.Nil(t, result.State.LastResult)
			case "condition":
				require.Equal(t, &tc.node, result.Condition)
				require.Equal(t, before.VisitID, result.State.VisitID)
				require.Equal(t, before.LastResult, result.State.LastResult)
			case "wait":
				require.Equal(t, &tc.node, result.Wait)
				require.Nil(t, result.State.Wait, "the interpreter does not register waits")
			}
		})
	}
}

func TestResolveGraphConditionPreservesProducerForDeclarativeCondition(t *testing.T) {
	g := commandConditionGraph()
	g.Nodes = append(g.Nodes, config.WorkflowNode{ID: "by-exit", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", Equals: 7}})
	g.Edges[2].To = "by-exit"
	g.Edges = append(g.Edges, graphEdge("by-exit", "true", "yes"), graphEdge("by-exit", "false", "no"))
	pending := reachCommandCondition(t, g, freshGraphState())
	result := ResolveGraphCondition(g, pending.Stage, pending.State, GraphConditionResult{NodeID: "probe", EvaluationID: "eval-42", ExitCode: 0})
	require.False(t, result.Rejected, result.Reason)
	require.Equal(t, ticket.StatusDone, result.Status)
	require.Equal(t, pending.State.LastResult, result.State.LastResult)
	require.Equal(t, []ticket.GraphTransition{
		{From: "probe", Port: "true", To: "by-exit", Revision: 3, EvaluationID: "eval-42"},
		{From: "by-exit", Port: "true", To: "yes", Revision: 3},
	}, result.Edges, "the declarative condition must compare the original check's 7, not the command condition's 0")
	require.Equal(t, append(pending.State.Clone().Transitions, result.Edges...), result.State.Transitions)
}

func TestResolveGraphConditionPreviousHandoff(t *testing.T) {
	g := commandConditionGraph()
	g.Nodes = append(g.Nodes, config.WorkflowNode{ID: "repair", Type: "agent", Handoff: config.WorkflowHandoffPrevious})
	g.Edges[2].To = "repair"
	g.Edges = append(g.Edges, graphEdge("repair", "complete", "yes"), graphEdge("repair", "failed", "no"))
	pending := reachCommandCondition(t, g, freshGraphState())
	want := &ticket.GraphHandoff{
		ExecutionID: "execution", NodeID: "run", VisitID: "execution.1", InvocationID: "invocation-execution.1",
		Check: &ticket.GraphCheckHandoff{ExitCode: 7},
	}
	require.Equal(t, want, pending.State.Handoff)
	result := ResolveGraphCondition(g, pending.Stage, pending.State, GraphConditionResult{NodeID: "probe", EvaluationID: "eval-42"})
	require.False(t, result.Rejected, result.Reason)
	require.NotNil(t, result.Next)
	require.Equal(t, "repair", result.Next.ID)
	require.Equal(t, "execution.2", result.State.VisitID)
	require.Nil(t, result.State.LastResult)
	require.Equal(t, want, pending.State.Handoff)
	want.DestinationVisitID = result.State.VisitID
	require.Equal(t, want, result.State.Handoff)
}
