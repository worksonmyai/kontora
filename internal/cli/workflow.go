package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
)

// WorkflowRestart requires the daemon: only it can inspect invocation journals
// and release execution reservations safely. There is no disk-only fallback.
func WorkflowRestart(cfg *config.Config, id string) error {
	return LocalClient(cfg).WorkflowRestart(id)
}

func WorkflowRecover(cfg *config.Config, id, executionID string, revision uint64) error {
	return LocalClient(cfg).WorkflowRecover(id, executionID, revision)
}

// Releases never fall back to offline ticket writes: receipt acceptance and
// per-wait replay belong to the daemon, even when it is on localhost.
func WorkflowRelease(cfg *config.Config, id string, req web.WorkflowReleaseRequest) (*ticket.WorkflowReceipt, error) {
	return LocalClient(cfg).WorkflowRelease(id, req)
}

func DeploymentRelease(cfg *config.Config, req web.DeploymentReleaseRequest) (*ticket.WorkflowReceipt, error) {
	return LocalClient(cfg).DeploymentRelease(req)
}

func ParseWorkflowRelease(executionID, waitID, revision, requestID, reason string) (web.WorkflowReleaseRequest, error) {
	req := web.WorkflowReleaseRequest{ExecutionID: executionID, WaitID: waitID, RequestID: requestID, Reason: reason}
	if err := req.Validate(); err != nil {
		return req, err
	}
	if revision == "" {
		return req, fmt.Errorf("release requires --revision from the inspected wait")
	}
	n, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return req, fmt.Errorf("invalid workflow revision %q: expected an unsigned 64-bit integer", revision)
	}
	req.Revision = n
	return req, nil
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
	printWorkflowWait(w, definition, execution)
	printWorkflowTrace(w, execution)
}

// QuoteShellArg prevents expansion when an operator copies a generated command.
func QuoteShellArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func printWorkflowWait(w io.Writer, definition *app.GraphDefinitionView, execution *app.GraphExecutionView) {
	if wait := execution.Wait; wait != nil {
		fmt.Fprintf(w, "Waiting:   %s (%s, wait %s)\n", wait.Kind, wait.Phase, wait.ID)
		if wait.Disarmed {
			fmt.Fprintln(w, "progress:  disarmed; use retry to re-arm after recovery")
		}
		if !wait.Deadline.IsZero() {
			fmt.Fprintf(w, "deadline:  %s\n", wait.Deadline.Format(time.RFC3339Nano))
		}
		if !wait.Until.IsZero() {
			fmt.Fprintf(w, "until:     %s\n", wait.Until.Format(time.RFC3339Nano))
		}
		if !wait.NextPoll.IsZero() {
			fmt.Fprintf(w, "next poll: %s (completed polls %d)\n", wait.NextPoll.Format(time.RFC3339Nano), wait.PollCount)
		}
		fmt.Fprintf(w, "wakes:     %d", execution.WakeCount)
		if definition != nil {
			for _, node := range definition.Nodes {
				if node.ID == execution.CurrentNode && node.Wake != nil {
					fmt.Fprintf(w, "/%d", node.Wake.Max)
				}
			}
		}
		fmt.Fprintln(w)
		if wait.Next != "" {
			fmt.Fprintf(w, "next:      %s\n", wait.Next)
		}
		if wait.Output != "" {
			fmt.Fprintf(w, "output:    %s\n", wait.Output)
		}
		if wait.OutputTruncated {
			fmt.Fprintln(w, "output:    truncated; inspect the probe log")
		}
		if wait.LogPath != "" {
			fmt.Fprintf(w, "probe log: %s\n", wait.LogPath)
		}
		if wait.Kind == "human" {
			fmt.Fprintf(w, "release:   kontora release --execution-id %s --wait-id %s --revision %d --request-id REQUEST --reason REASON TICKET_ID\n", execution.ExecutionID, wait.ID, execution.Revision)
		}
		if wait.Kind == "deployment" {
			fmt.Fprintf(w, "release:   kontora release --repo REPO --environment %s --rev DEPLOYED_FULL_COMMIT --event-id EVENT_ID (required commit %s)\n", QuoteShellArg(wait.Environment), wait.RequiredCommit)
		}
	}
	if execution.Wake != nil {
		fmt.Fprintf(w, "wake:      %s (count %d)\n", execution.Wake.Reason, execution.Wake.Count)
	}
}

func printWorkflowTrace(w io.Writer, execution *app.GraphExecutionView) {
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
