package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

// WorkflowRestart requires the daemon: only it can inspect invocation journals
// and release execution reservations safely. There is no disk-only fallback.
func WorkflowRestart(cfg *config.Config, id string) error {
	return LocalClient(cfg).WorkflowRestart(id)
}

func WorkflowRecover(cfg *config.Config, id, executionID string, revision uint64) error {
	return LocalClient(cfg).WorkflowRecover(id, executionID, revision)
}

func ParseWorkflowRecovery(executionID, revision string) (uint64, error) {
	if strings.TrimSpace(executionID) == "" || revision == "" {
		return 0, fmt.Errorf("workflow-recover requires --execution-id and --revision from the inspected execution")
	}
	n, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid workflow revision %q: expected an unsigned 64-bit integer", revision)
	}
	return n, nil
}

// PrintWorkflow renders shared projections without treating definition order as
// progress. Transitions are the actual selected edges, including repetitions.
func PrintWorkflow(w io.Writer, definition *app.GraphDefinitionView, execution *app.GraphExecutionView) {
	if definition == nil && execution == nil {
		return
	}
	if definition != nil {
		fmt.Fprintf(w, "workflow:  graph (entry %s)\n", definition.Entry)
	}
	if execution == nil {
		fmt.Fprintln(w, "execution: not started")
		if definition != nil {
			printWorkflowDefinition(w, definition)
		}
		return
	}
	fmt.Fprintf(w, "execution: %s (revision %d)\n", execution.ExecutionID, execution.Revision)
	fmt.Fprintf(w, "node:      %s\n", execution.CurrentNode)
	if execution.CurrentTemplate != "" {
		fmt.Fprintf(w, "template:  %s\n", execution.CurrentTemplate)
	}
	fmt.Fprintf(w, "visit:     %d (%s)\n", execution.Visit, execution.VisitID)
	fmt.Fprintf(w, "attempt:   %d (within this visit)\n", execution.Attempt+1)
	for _, loop := range execution.Loops {
		note := ""
		if loop.Exhausted {
			note = " (exhausted)"
		}
		limit := "unknown"
		if loop.Max > 0 {
			limit = strconv.Itoa(loop.Max)
		}
		fmt.Fprintf(w, "loop:      %s %d/%s%s\n", loop.NodeID, loop.Used, limit, note)
	}
	for _, previous := range execution.PreviousExecutions {
		fmt.Fprintf(w, "Previous execution: %s\n", previous.ExecutionID)
		for _, edge := range previous.Transitions {
			fmt.Fprintf(w, "  %s.%s -> %s (revision %d)\n", edge.From, edge.Port, edge.To, edge.Revision)
		}
	}
	edges := execution.Transitions
	if len(edges) == 0 {
		edges = execution.SelectedEdges
	}
	if len(edges) > 0 {
		fmt.Fprintln(w, "Transitions:")
		for _, edge := range edges {
			fmt.Fprintf(w, "  %s.%s -> %s (revision %d", edge.From, edge.Port, edge.To, edge.Revision)
			if edge.Port == "repeat" || edge.Port == "exhausted" {
				fmt.Fprintf(w, ", loop count %d", edge.LoopCount)
			}
			fmt.Fprintln(w, ")")
		}
	}
}

func printWorkflowDefinition(w io.Writer, definition *app.GraphDefinitionView) {
	fmt.Fprintln(w, "Nodes (definition, not execution order):")
	for _, node := range definition.Nodes {
		fmt.Fprintf(w, "  %s (%s)", node.ID, node.Type)
		if node.Template != "" {
			fmt.Fprintf(w, " template %s", node.Template)
		}
		if node.Type == "loop" {
			fmt.Fprintf(w, " max %d", node.Max)
		}
		if node.Status != "" {
			fmt.Fprintf(w, " status %s", node.Status)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "Edges (definition):")
	for _, edge := range definition.Edges {
		fmt.Fprintf(w, "  %s.%s -> %s\n", edge.From.Node, edge.From.Port, edge.To)
	}
}
