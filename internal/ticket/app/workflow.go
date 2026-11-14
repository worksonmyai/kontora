package app

import (
	"fmt"
	"math"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// ValidateWorkflowIdentity prevents saved graph evidence from being interpreted
// as a legacy pipeline or standalone run. It also applies to execution edits.
func ValidateWorkflowIdentity(cfg *config.Config, t *ticket.Ticket) error {
	definition := cfg.Pipelines[t.Pipeline]
	if !definition.IsGraph() {
		if t.GraphState != nil {
			return fmt.Errorf("%w: graph execution cannot run as a legacy pipeline or standalone ticket; use workflow-restart with a graph pipeline", ErrInvalidState)
		}
		return nil
	}
	if t.IsAgentEpic() || t.AnnotationReturnStatus != "" {
		return fmt.Errorf("%w: graph workflows do not support epic supervisors or annotation runs", ErrInvalidState)
	}
	return nil
}

// ValidateWorkflowReentry checks the shared, disk-visible admission rules.
// The daemon must also check the effective definition, invocation journal and
// termination evidence before starting work. This function never resets state.
func ValidateWorkflowReentry(cfg *config.Config, t *ticket.Ticket) error {
	if err := ValidateWorkflowIdentity(cfg, t); err != nil {
		return err
	}
	definition := cfg.Pipelines[t.Pipeline]
	if !definition.IsGraph() {
		return nil
	}
	if t.InvocationID != "" {
		return fmt.Errorf("%w: workflow invocation requires workflow-recover before re-entry", ErrInvalidState)
	}
	graph := definition.Graph
	if t.GraphState == nil {
		if t.Stage != "" && t.Stage != graph.Entry {
			return fmt.Errorf("%w: a new graph workflow must start at entry %q", ErrInvalidState, graph.Entry)
		}
		return nil
	}
	state := t.GraphState
	if (state.Version != 1 && state.Version != 2) || state.ExecutionID == "" || state.DefinitionFingerprint == "" {
		return fmt.Errorf("%w: invalid graph execution identity", ErrInvalidState)
	}
	if state.Revision == math.MaxUint64 {
		return fmt.Errorf("%w: graph revision overflow", ErrInvalidState)
	}
	if state.Wait != nil {
		if err := validateWorkflowWait(t); err != nil {
			return err
		}
		if state.Wait.Disarmed || state.Wait.Phase != "ready" || !state.Continuation {
			return fmt.Errorf("%w: workflow wait owns the next transition; use retry to re-arm a paused wait", ErrInvalidState)
		}
	}
	node, ok := graph.Node(t.Stage)
	if !ok {
		return fmt.Errorf("%w: unknown graph cursor %q", ErrInvalidState, t.Stage)
	}
	if node.Type == "end" {
		return fmt.Errorf("%w: workflow is at an end node; use workflow-restart", ErrInvalidState)
	}
	for id, count := range state.LoopCounts {
		loop, ok := graph.Node(id)
		if !ok || loop.Type != "loop" || count < 0 || count > loop.Max {
			return fmt.Errorf("%w: invalid graph loop count for %q", ErrInvalidState, id)
		}
	}
	return nil
}

func retryWorkflow(cfg *config.Config, t *ticket.Ticket) error {
	if t.GraphState != nil && t.GraphState.Wait != nil {
		return retryWorkflowWait(cfg, t)
	}
	if err := ValidateWorkflowReentry(cfg, t); err != nil {
		return err
	}
	definition := cfg.Pipelines[t.Pipeline]
	if !definition.IsGraph() {
		return nil
	}
	cursor := t.Stage
	if cursor == "" && t.GraphState == nil {
		cursor = definition.Graph.Entry
	}
	node, ok := definition.Graph.Node(cursor)
	if !ok || (node.Type != "agent" && node.Type != "check") {
		return fmt.Errorf("%w: retry requires an executable graph node; use workflow-restart", ErrInvalidState)
	}
	if t.GraphState == nil {
		return nil
	}
	state := t.GraphState.Clone()
	state.LastResult = nil
	state.Revision++
	return t.SetField("graph_state", state)
}

// PauseWorkflowWait disarms authority without changing its deadline or identity.
// The caller must save the ticket before stopping any live probe.
func PauseWorkflowWait(t *ticket.Ticket) (bool, error) {
	if !t.HasWorkflowWaitAuthority() {
		return false, nil
	}
	state := t.GraphState.Clone()
	if state.Revision == math.MaxUint64 {
		return true, fmt.Errorf("%w: graph revision overflow", ErrInvalidState)
	}
	if state.Wait != nil {
		state.Wait.Disarmed = true
	}
	state.Revision++
	return true, t.SetField("graph_state", state)
}

func cancelWorkflowWait(t *ticket.Ticket) error {
	if !t.HasWorkflowWaitAuthority() {
		return nil
	}
	state := t.GraphState.Clone()
	if state.Revision == math.MaxUint64 {
		return fmt.Errorf("%w: graph revision overflow", ErrInvalidState)
	}
	if state.Wait != nil {
		state.WaitHistory = append(state.WaitHistory, *state.Wait.Clone())
	}
	state.Wait = nil
	state.Continuation = false
	state.Wake = nil
	state.Revision++
	return t.SetField("graph_state", state)
}

func validateWorkflowWait(t *ticket.Ticket) error {
	state := t.GraphState
	w := state.Wait
	validKind := w.Kind == "timer" || w.Kind == "check" || w.Kind == "human" || w.Kind == "deployment"
	needsVisit := w.Kind == "timer" || w.Kind == "check"
	if state.Version != 2 || w.ID == "" || w.ExecutionID != state.ExecutionID ||
		w.NodeID != t.Stage || w.VisitID != state.VisitID || (needsVisit && w.VisitID == "") || !validKind {
		return fmt.Errorf("%w: invalid workflow wait identity; inspect before workflow-recover", ErrInvalidState)
	}
	return nil
}

func retryWorkflowWait(cfg *config.Config, t *ticket.Ticket) error {
	if err := validateWorkflowWait(t); err != nil {
		return err
	}
	w := t.GraphState.Wait
	if w.Phase != "pending" && w.Phase != "ready" {
		return fmt.Errorf("%w: workflow wait requires workflow-recover before retry", ErrInvalidState)
	}
	// Validate the execution independently of the wait's admission barrier.
	identity := t.Clone()
	identity.GraphState.Wait = nil
	identity.GraphState.Continuation = false
	if err := ValidateWorkflowReentry(cfg, identity); err != nil {
		return err
	}
	state := t.GraphState.Clone()
	state.Wait.Disarmed = false
	state.Revision++
	return t.SetField("graph_state", state)
}

func initStage(cfg *config.Config, t *ticket.Ticket, req InitRequest, reinit bool, previousPipeline, previousStage string) error {
	if t.Pipeline == "" {
		return nil
	}
	definition := cfg.Pipelines[t.Pipeline]
	stage := req.Stage
	if definition.IsGraph() {
		if stage == "" {
			stage = definition.Graph.Entry
		}
		if stage != definition.Graph.Entry || (t.GraphState != nil && t.Stage != stage) {
			return fmt.Errorf("%w: graph initialization must start at entry %q; use workflow-restart for an existing execution", ErrInvalidState, definition.Graph.Entry)
		}
	} else if stage == "" {
		stage = definition.Legacy[0].Stage
		if reinit && previousStage != "" && t.Pipeline == previousPipeline {
			stage = previousStage
		}
	}
	return t.SetField("stage", stage)
}
