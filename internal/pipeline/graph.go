package pipeline

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

type GraphCompletion struct {
	NodeID, VisitID, InvocationID, Outcome, Verdict                  string
	ExitCode                                                         int
	Interrupted, TimedOut, InfrastructureError, TerminationUncertain bool
}

type GraphAdvance struct {
	Stage string
	State *ticket.GraphState
	Next  *config.WorkflowNode
	// Wait is a declared control wait for the daemon to register. Traversal does
	// not assign a wait identity or any timing or workspace evidence.
	Wait     *config.WorkflowNode
	Status   ticket.Status
	Reason   string
	Rejected bool `json:"rejected,omitempty"`
	Edges    []ticket.GraphTransition
}

func AdvanceGraph(graph *config.WorkflowGraph, stage string, state *ticket.GraphState, completion *GraphCompletion) GraphAdvance {
	a := GraphAdvance{Stage: stage, State: state.Clone(), Status: ticket.StatusTodo}
	if reason := a.validateInput(graph); reason != "" {
		return a.pause(reason)
	}
	entering := stage == ""
	node, reason := a.prepareCursor(graph, completion)
	if reason != "" {
		return a.pause(reason)
	}
	if a.State.Wait != nil {
		return a.pause("graph has an active wait; release or wake it before advancing")
	}
	if completion != nil {
		if reason := a.complete(graph, node, completion, entering); reason != "" {
			return a.pause(reason)
		}
	} else if graphExecutable(node) && !entering {
		if a.State.Visit == 0 || a.State.LastResult != nil {
			return a.pause("invalid executable visit state")
		}
		a.Next = cloneWorkflowNode(node)
		return a
	}
	return a.traverse(graph)
}

// ReleaseGraphWait follows an explicitly accepted release or timeout of a
// registered declared wait. The caller owns release authority, timing and
// evidence; this interpreter only validates identity and chooses graph edges.
func ReleaseGraphWait(graph *config.WorkflowGraph, stage string, state *ticket.GraphState, port string) GraphAdvance {
	a := GraphAdvance{Stage: stage, State: state.Clone(), Status: ticket.StatusTodo}
	if reason := a.validateInput(graph); reason != "" {
		return a.pause(reason)
	}
	node, ok := graph.Node(stage)
	if !ok || node.Type != "wait" {
		return a.pause("release requires a declared wait cursor")
	}
	if reason := validateDeclaredWait(graph, node); reason != "" {
		return a.pause(reason)
	}
	s := a.State
	w := s.Wait
	if w == nil || w.ID == "" || w.ExecutionID != s.ExecutionID || w.NodeID != node.ID || w.VisitID != s.VisitID || w.Kind != node.Mode || w.InvocationID != "" || w.Environment != node.Environment {
		return a.pause("release does not match the current declared wait")
	}
	if port != "released" && (port != "timeout" || node.Timeout.Duration <= 0) {
		return a.pause("invalid declared wait release port")
	}
	if !a.follow(graph, node.ID, port, 0) {
		return a.pause("missing declared wait transition")
	}
	s.WaitHistory = append(s.WaitHistory, *w)
	s.Wait = nil
	return a.traverse(graph)
}

func validateDeclaredWait(graph *config.WorkflowGraph, node config.WorkflowNode) string {
	if (node.Mode != "human" && node.Mode != "deployment") || node.Timeout.Duration < 0 ||
		(node.Mode == "human" && node.Environment != "") ||
		(node.Mode == "deployment" && strings.TrimSpace(node.Environment) == "") {
		return "invalid declared wait configuration"
	}
	if _, ok := graph.Target(node.ID, "released"); !ok {
		return "missing declared wait release transition"
	}
	_, timeout := graph.Target(node.ID, "timeout")
	if timeout != (node.Timeout.Duration > 0) {
		return "invalid declared wait timeout transition"
	}
	return ""
}

func (a GraphAdvance) pause(reason string) GraphAdvance {
	a.Rejected = true
	a.Next = nil
	a.Wait = nil
	a.Status = ticket.StatusPaused
	a.Reason = reason
	return a
}

func (a *GraphAdvance) validateInput(graph *config.WorkflowGraph) string {
	s := a.State
	if s == nil {
		return "missing graph state"
	}
	if s.Revision == math.MaxUint64 {
		return "graph revision overflow"
	}
	s.Revision++
	if graph == nil || graph.Version != 2 {
		return "unsupported graph definition"
	}
	if s.ExecutionID == "" || s.DefinitionFingerprint == "" {
		return "missing graph execution identity"
	}
	// Persisted state versions are independent of the definition version.
	// The daemon selects version 2 for wait-enabled executions; unchanged
	// executions continue to initialize as version 1.
	if s.Version == 0 && a.Stage == "" {
		s.Version = 1
	}
	if s.Version != 1 && s.Version != 2 {
		return "unsupported graph state version"
	}
	if reason := validateGraphStructure(graph); reason != "" {
		return reason
	}
	return validateGraphState(graph, s)
}

func validateGraphStructure(graph *config.WorkflowGraph) string {
	ids := make(map[string]bool, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if node.ID == "" || ids[node.ID] {
			return "invalid graph node identity"
		}
		ids[node.ID] = true
	}
	if !ids[graph.Entry] {
		return "missing graph entry"
	}
	ports := make(map[config.WorkflowPort]bool, len(graph.Edges))
	for _, edge := range graph.Edges {
		if ports[edge.From] || !ids[edge.From.Node] || !ids[edge.To] {
			return "invalid graph edge"
		}
		ports[edge.From] = true
	}
	return ""
}

func validateGraphState(graph *config.WorkflowGraph, s *ticket.GraphState) string {
	if s.WakeCount < 0 {
		return "invalid graph wake count"
	}
	if s.Version == 1 && (s.Wait != nil || s.Wake != nil || s.WakeCount != 0 || len(s.WaitHistory) != 0 || s.Continuation) {
		return "wait metadata requires graph state version 2"
	}
	for id, count := range s.LoopCounts {
		node, ok := graph.Node(id)
		if !ok || node.Type != "loop" || count < 0 || count > node.Max {
			return "invalid graph loop count"
		}
	}
	if (s.Visit == 0 && s.VisitID != "") || (s.Visit > 0 && s.VisitID != graphVisitID(s)) {
		return "invalid graph visit identity"
	}
	if s.LastResult != nil {
		return validateGraphResult(graph, s)
	}
	return ""
}

func (a *GraphAdvance) prepareCursor(graph *config.WorkflowGraph, completion *GraphCompletion) (config.WorkflowNode, string) {
	if a.Stage == "" {
		s := a.State
		if s.Visit != 0 || s.LastResult != nil || len(s.LoopCounts) != 0 || len(s.Transitions) != 0 || completion != nil || s.Wait != nil || s.Wake != nil || s.WakeCount != 0 || len(s.WaitHistory) != 0 || s.Continuation {
			return config.WorkflowNode{}, "empty cursor with existing execution evidence"
		}
		a.Stage = graph.Entry
	}
	node, ok := graph.Node(a.Stage)
	if !ok {
		return config.WorkflowNode{}, "unknown graph cursor"
	}
	return node, ""
}

func (a *GraphAdvance) complete(graph *config.WorkflowGraph, node config.WorkflowNode, completion *GraphCompletion, entering bool) string {
	s := a.State
	if !graphExecutable(node) || entering || s.Visit == 0 || completion.NodeID != node.ID || completion.VisitID != s.VisitID || completion.InvocationID == "" {
		return "completion does not match the current node visit"
	}
	if s.LastResult != nil {
		return "executable cursor already has a result"
	}
	if reason := graphCompletionSafety(completion); reason != "" {
		return reason
	}
	exitCode := completion.ExitCode
	s.LastResult = &ticket.GraphResult{
		NodeID: node.ID, VisitID: s.VisitID, InvocationID: completion.InvocationID,
		Outcome: completion.Outcome, Verdict: completion.Verdict, ExitCode: &exitCode,
	}
	if reason := validateGraphResult(graph, s); reason != "" {
		s.LastResult = nil
		return reason
	}
	port := "complete"
	if node.Type == "check" {
		port = "pass"
		if exitCode != 0 {
			port = "fail"
		}
	} else if exitCode != 0 || completion.Outcome == "failed" {
		port = "failed"
	}
	if !a.follow(graph, node.ID, port, 0) {
		return "missing executable transition"
	}
	return ""
}

func graphCompletionSafety(completion *GraphCompletion) string {
	// Safety evidence takes precedence over exit codes and verdicts.
	switch {
	case completion.TerminationUncertain:
		return "executable termination is uncertain"
	case completion.Interrupted:
		return "executable interrupted"
	case completion.TimedOut:
		return "executable timed out"
	case completion.InfrastructureError:
		return "executable infrastructure error"
	case completion.Outcome == "blocked":
		return "executable blocked"
	case completion.Outcome == "protocol_error":
		return "executable protocol error"
	}
	return ""
}

func (a GraphAdvance) traverse(graph *config.WorkflowGraph) GraphAdvance {
	s := a.State
	for steps := 0; steps <= len(graph.Nodes); steps++ {
		node, ok := graph.Node(a.Stage)
		if !ok {
			return a.pause("unknown graph cursor")
		}
		switch node.Type {
		case "agent", "check":
			if s.Visit == math.MaxUint64 {
				return a.pause("graph visit overflow")
			}
			s.Visit++
			s.VisitID = graphVisitID(s)
			s.LastResult = nil
			s.Wake = nil
			s.WakeCount = 0
			s.Continuation = false
			a.Next = cloneWorkflowNode(node)
			return a
		case "wait":
			if s.Version != 2 {
				return a.pause("declared wait requires graph state version 2")
			}
			if reason := validateDeclaredWait(graph, node); reason != "" {
				return a.pause(reason)
			}
			a.Status = ticket.StatusPaused
			a.Wait = cloneWorkflowNode(node)
			return a
		case "condition":
			port, reason := graphCondition(graph, node, s)
			if reason != "" {
				return a.pause(reason)
			}
			if !a.follow(graph, node.ID, port, 0) {
				return a.pause("missing condition transition")
			}
		case "loop":
			if reason := a.advanceLoop(graph, node); reason != "" {
				return a.pause(reason)
			}
		case "end":
			status := ticket.Status(node.Status)
			if status != ticket.StatusDone && status != ticket.StatusHumanReview && status != ticket.StatusPaused && !ticket.IsCustomRunningStatus(status) {
				return a.pause("invalid end status")
			}
			a.Status = status
			return a
		default:
			return a.pause("unsupported graph node kind")
		}
	}
	return a.pause("graph control traversal limit exceeded")
}

func (a *GraphAdvance) advanceLoop(graph *config.WorkflowGraph, node config.WorkflowNode) string {
	if node.Max < 0 {
		return "invalid loop allowance"
	}
	s := a.State
	count := s.LoopCounts[node.ID]
	port := "exhausted"
	if count < node.Max {
		port = "repeat"
		count++
	}
	if !a.follow(graph, node.ID, port, count) {
		return "missing loop transition"
	}
	if s.LoopCounts == nil {
		s.LoopCounts = make(map[string]int)
	}
	s.LoopCounts[node.ID] = count
	if port == "exhausted" {
		a.Reason = fmt.Sprintf("loop %s exhausted", node.ID)
	}
	return ""
}

func (a *GraphAdvance) follow(graph *config.WorkflowGraph, from, port string, loopCount int) bool {
	to, ok := graph.Target(from, port)
	if !ok {
		return false
	}
	edge := ticket.GraphTransition{From: from, Port: port, To: to, LoopCount: loopCount, Revision: a.State.Revision}
	a.Edges = append(a.Edges, edge)
	a.State.Transitions = append(a.State.Transitions, edge)
	a.Stage = to
	return true
}

func graphVisitID(s *ticket.GraphState) string {
	return fmt.Sprintf("%s.%d", s.ExecutionID, s.Visit)
}

func graphExecutable(node config.WorkflowNode) bool {
	return node.Type == "agent" || node.Type == "check"
}

func cloneWorkflowNode(node config.WorkflowNode) *config.WorkflowNode {
	node.Verdicts = slices.Clone(node.Verdicts)
	if node.Wake != nil {
		wake := *node.Wake
		node.Wake = &wake
	}
	return &node
}

func validateGraphResult(graph *config.WorkflowGraph, s *ticket.GraphState) string {
	r := s.LastResult
	if s.Visit == 0 || r.VisitID != s.VisitID || r.InvocationID == "" {
		return "missing or stale executable result identity"
	}
	producer, ok := graph.Node(r.NodeID)
	if !ok || !graphExecutable(producer) {
		return "result producer is not executable"
	}
	if r.ExitCode == nil || (r.Outcome != "complete" && r.Outcome != "failed") {
		return "invalid executable result"
	}
	if producer.Type == "check" {
		if r.Verdict != "" || (r.Outcome == "failed" && *r.ExitCode == 0) {
			return "invalid check result"
		}
		return ""
	}
	if r.Verdict != "" && !slices.Contains(producer.Verdicts, r.Verdict) {
		return "undeclared agent verdict"
	}
	if r.Outcome == "complete" && *r.ExitCode == 0 && len(producer.Verdicts) > 0 && r.Verdict == "" {
		return "missing agent verdict"
	}
	return ""
}

func graphCondition(graph *config.WorkflowGraph, node config.WorkflowNode, s *ticket.GraphState) (string, string) {
	if node.When == nil || s.LastResult == nil {
		return "", "condition requires an executable result"
	}
	if reason := validateGraphResult(graph, s); reason != "" {
		return "", reason
	}
	r := s.LastResult
	producer, _ := graph.Node(r.NodeID)
	var equal bool
	switch node.When.Field {
	case "verdict":
		expected, ok := node.When.Equals.(string)
		if !ok || producer.Type != "agent" || r.Verdict == "" || r.Outcome != "complete" || *r.ExitCode != 0 {
			return "", "condition requires a successful declared agent verdict"
		}
		equal = r.Verdict == expected
	case "exit_code":
		expected, ok := node.When.Equals.(int)
		if !ok || producer.Type != "check" || r.ExitCode == nil {
			return "", "condition requires a check exit code and integer comparison"
		}
		equal = *r.ExitCode == expected
	default:
		return "", "unsupported condition field"
	}
	if equal {
		return "true", ""
	}
	return "false", ""
}
