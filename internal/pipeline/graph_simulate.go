package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// GraphScenarioEvent describes one synthetic result at the named executable or
// declared wait. It never represents an invocation or a real release receipt.
type GraphScenarioEvent struct {
	Node     string `json:"node"`
	Outcome  string `json:"outcome,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Verdict  string `json:"verdict,omitempty"`
	Wait     string `json:"wait,omitempty"`

	hasCompletion bool
	hasWait       bool
}

func (e *GraphScenarioEvent) UnmarshalJSON(data []byte) error {
	var fields struct {
		Node     string          `json:"node"`
		Outcome  json.RawMessage `json:"outcome"`
		ExitCode json.RawMessage `json:"exit_code"`
		Verdict  json.RawMessage `json:"verdict"`
		Wait     json.RawMessage `json:"wait"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fields); err != nil {
		return err
	}
	event := GraphScenarioEvent{
		Node:          fields.Node,
		hasCompletion: fields.Outcome != nil || fields.ExitCode != nil || fields.Verdict != nil,
		hasWait:       fields.Wait != nil,
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
		dest any
	}{
		{"outcome", fields.Outcome, &event.Outcome},
		{"exit_code", fields.ExitCode, &event.ExitCode},
		{"verdict", fields.Verdict, &event.Verdict},
		{"wait", fields.Wait, &event.Wait},
	} {
		if field.raw != nil {
			if err := json.Unmarshal(field.raw, field.dest); err != nil {
				return fmt.Errorf("%s: %w", field.name, err)
			}
		}
	}
	*e = event
	return nil
}

type GraphScenarioResult struct {
	Cursor      string
	Status      ticket.Status
	Stop        string // end, waiting, missing input, or rejected
	Reason      string
	Transitions []ticket.GraphTransition
}

// SimulateGraph feeds the scenario to the same interpreter used by the daemon.
// The graph should already have passed config validation.
func SimulateGraph(graph *config.WorkflowGraph, events []GraphScenarioEvent) GraphScenarioResult {
	a := simulationEntry(graph)
	var result GraphScenarioResult
	for i := 0; ; i++ {
		result.Transitions = append(result.Transitions, a.Edges...)
		result.Cursor, result.Status, result.Reason = a.Stage, a.Status, a.Reason
		switch {
		case a.Rejected:
			result.Stop = "rejected"
			if i > 0 {
				result.Reason = fmt.Sprintf("event %d: %s", i, a.Reason)
			}
			return result
		case a.Next == nil && a.Wait == nil:
			result.Stop = "end"
			if i < len(events) {
				result.Stop = "rejected"
				result.Reason = fmt.Sprintf("event %d: no input expected after end node %q", i+1, a.Stage)
			}
			return result
		case i == len(events):
			result.Stop = "missing input"
			if a.Wait != nil {
				result.Stop = "waiting"
			}
			return result
		default:
			a = simulationStep(graph, a, events[i])
		}
	}
}

func simulationEntry(graph *config.WorkflowGraph) GraphAdvance {
	return AdvanceGraph(graph, "", &ticket.GraphState{ExecutionID: "offline", DefinitionFingerprint: "offline"}, nil)
}

func simulationStep(graph *config.WorkflowGraph, a GraphAdvance, event GraphScenarioEvent) GraphAdvance {
	// Local rejection returns the incoming advance. Clear its edges so that step is not reported twice.
	a.Edges = nil
	if event.Node != a.Stage {
		return a.pause(fmt.Sprintf("expected node %q, got %q", a.Stage, event.Node))
	}
	s := a.State
	if a.Wait != nil {
		if event.Wait == "" || event.hasCompletion || event.Outcome != "" || event.ExitCode != nil || event.Verdict != "" {
			return a.pause("expected a wait outcome (released or timeout) without completion fields")
		}
		state := *s
		state.Wait = &ticket.WorkflowWait{
			ID: fmt.Sprintf("wait-%d", s.Revision), ExecutionID: s.ExecutionID,
			NodeID: a.Stage, VisitID: s.VisitID, Kind: a.Wait.Mode, Environment: a.Wait.Environment,
		}
		return trimSimulationHistory(ReleaseGraphWait(graph, a.Stage, &state, event.Wait))
	}
	if event.hasWait || event.Wait != "" {
		return a.pause("wait is only valid at a declared wait")
	}
	if a.Next.Type == "check" && event.ExitCode == nil {
		return a.pause("check completion requires exit_code")
	}
	completion := &GraphCompletion{
		NodeID: a.Stage, VisitID: s.VisitID, InvocationID: fmt.Sprintf("run-%d", s.Revision),
		Outcome: event.Outcome, Verdict: event.Verdict,
	}
	if event.ExitCode != nil {
		completion.ExitCode = *event.ExitCode
		if completion.ExitCode < 0 || completion.ExitCode > 255 {
			return a.pause("exit_code must be in 0..255")
		}
	}
	if completion.Outcome == "" {
		completion.Outcome = "complete"
		if completion.ExitCode != 0 {
			completion.Outcome = "failed"
		}
	}
	return trimSimulationHistory(AdvanceGraph(graph, a.Stage, s, completion))
}

// Recorded history does not affect routing. Keep edges in the report rather
// than repeatedly cloning an entire history at each interpreter step.
func trimSimulationHistory(a GraphAdvance) GraphAdvance {
	a.State.Transitions = nil
	a.State.WaitHistory = nil
	return a
}

const GraphPreflightLimit = 4096

type GraphPreflightResult struct {
	Explored     int
	Inconclusive bool
	Diagnostics  []GraphPreflightDiagnostic
}

type GraphPreflightDiagnostic struct {
	Node     string
	Reason   string
	Producer *ticket.GraphResult
	Path     []ticket.GraphTransition
}

// PreflightGraph explores normal synthetic outcomes through the interpreter.
// The graph should already have passed config validation. The budget counts
// interpreter calls, including entering the graph.
func PreflightGraph(graph *config.WorkflowGraph, limit int) GraphPreflightResult {
	var result GraphPreflightResult
	if limit <= 0 {
		result.Inconclusive = true
		return result
	}
	type cursor struct {
		advance GraphAdvance
		path    *preflightPath
	}
	var queue []cursor
	seen := make(map[string]bool)
	reported := make(map[[3]string]bool)
	record := func(a GraphAdvance, parent *preflightPath) {
		result.Explored++
		if a.Rejected {
			producer := a.State.LastResult
			key := [3]string{a.Stage, a.Reason, ""}
			if producer != nil {
				key[2] = producer.NodeID
			}
			if !reported[key] {
				reported[key] = true
				path := &preflightPath{parent: parent, edges: a.Edges}
				result.Diagnostics = append(result.Diagnostics, GraphPreflightDiagnostic{
					Node: a.Stage, Reason: a.Reason, Producer: producer, Path: path.transitions(),
				})
			}
			return
		}
		if a.Next == nil && a.Wait == nil {
			return
		}
		key := preflightCursorKey(a)
		if !seen[key] {
			seen[key] = true
			queue = append(queue, cursor{a, &preflightPath{parent: parent, edges: a.Edges}})
		}
	}
	record(trimSimulationHistory(simulationEntry(graph)), nil)
	if len(queue) == 0 {
		return result
	}
	codesByCheck := representativeCheckCodes(graph)
	for len(queue) > 0 {
		current := queue[0]
		queue[0] = cursor{}
		queue = queue[1:]
		for _, event := range preflightEvents(current.advance, codesByCheck[current.advance.Stage]) {
			if result.Explored >= limit {
				result.Inconclusive = true
				return result
			}
			record(simulationStep(graph, current.advance, event), current.path)
		}
	}
	return result
}

type preflightPath struct {
	parent *preflightPath
	edges  []ticket.GraphTransition
}

func (p *preflightPath) transitions() []ticket.GraphTransition {
	var path []ticket.GraphTransition
	for ; p != nil; p = p.parent {
		for _, edge := range slices.Backward(p.edges) {
			path = append(path, edge)
		}
	}
	slices.Reverse(path)
	return path
}

// Only the cursor, loop counters, and any result crossing a declared wait
// can change future routing. Ignore synthetic identities and revisions.
func preflightCursorKey(a GraphAdvance) string {
	var key strings.Builder
	fmt.Fprintf(&key, "%q", a.Stage)
	for _, id := range slices.Sorted(maps.Keys(a.State.LoopCounts)) {
		if count := a.State.LoopCounts[id]; count != 0 {
			fmt.Fprintf(&key, " %q:%d", id, count)
		}
	}
	if r := a.State.LastResult; r != nil {
		fmt.Fprintf(&key, " result:%q:%q:%q:%d", r.NodeID, r.Outcome, r.Verdict, *r.ExitCode)
	}
	return key.String()
}

func representativeCheckCodes(graph *config.WorkflowGraph) map[string][]int {
	nodes := make(map[string]config.WorkflowNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	targets := make(map[string][]string)
	for _, edge := range graph.Edges {
		targets[edge.From.Node] = append(targets[edge.From.Node], edge.To)
	}
	codesByCheck := make(map[string][]int)
	for _, check := range graph.Nodes {
		if check.Type != "check" {
			continue
		}
		var compared [256]bool
		seen := make(map[string]bool)
		var visit func(string)
		visit = func(id string) {
			if seen[id] {
				return
			}
			seen[id] = true
			node := nodes[id]
			switch node.Type {
			case "condition":
				if node.When != nil && node.When.Field == "exit_code" {
					_, values := node.When.Comparison()
					for _, value := range values {
						if code, ok := value.(int); ok && code > 0 && code < len(compared) {
							compared[code] = true
						}
					}
				}
			case "loop", "wait":
			default:
				return // Stop before another executable replaces the result.
			}
			// Follow every control branch regardless of condition values or loop counts.
			for _, next := range targets[id] {
				visit(next)
			}
		}
		for _, next := range targets[check.ID] {
			visit(next)
		}

		// Zero takes the pass port. Unlisted nonzero codes take the same branches.
		codes := []int{0}
		other := false
		for code := 1; code < len(compared); code++ {
			if compared[code] || !other {
				codes = append(codes, code)
				other = other || !compared[code]
			}
		}
		codesByCheck[check.ID] = codes
	}
	return codesByCheck
}

func preflightEvents(a GraphAdvance, codes []int) []GraphScenarioEvent {
	if a.Wait != nil {
		events := []GraphScenarioEvent{{Node: a.Stage, Wait: "released"}}
		if a.Wait.Timeout.Duration > 0 {
			events = append(events, GraphScenarioEvent{Node: a.Stage, Wait: "timeout"})
		}
		return events
	}
	if a.Next.Type == "check" {
		events := make([]GraphScenarioEvent, 0, len(codes))
		for _, code := range codes {
			events = append(events, GraphScenarioEvent{Node: a.Stage, ExitCode: new(code)})
		}
		return events
	}
	events := []GraphScenarioEvent{{Node: a.Stage, Outcome: "failed", ExitCode: new(1)}}
	if len(a.Next.Verdicts) == 0 {
		return append(events, GraphScenarioEvent{Node: a.Stage})
	}
	for _, verdict := range a.Next.Verdicts {
		if session.GraphVerdictFitsResult(verdict) {
			events = append(events, GraphScenarioEvent{Node: a.Stage, Verdict: verdict})
		}
	}
	return events
}
