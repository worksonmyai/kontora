package daemon

import (
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
)

func (d *Daemon) isGraphTicket(t *ticket.Ticket) bool {
	return t.GraphState != nil || d.config().Pipelines[t.Pipeline].IsGraph()
}

func (d *Daemon) workflowActionTicket(repo app.MutationRepository, id string, recovery bool) (*app.StoredTicket, error) {
	st, err := repo.Get(id)
	if err != nil {
		return nil, err
	}
	t := st.Ticket
	if !t.Kontora || !d.config().Pipelines[t.Pipeline].IsGraph() || t.IsRunning() || t.Status == ticket.StatusArchived {
		return nil, fmt.Errorf("%w: workflow controls require an initialized, stopped graph ticket", web.ErrInvalidState)
	}
	reservation, retained := d.recoveredReservations[id]
	_, running := d.running[id]
	_, live := d.liveRuns[id]
	_, reviewing := d.plannotator[id]
	// Uncertain termination retains live-run metadata after the runner returns.
	if reviewing || (live || running) && (!recovery || !retained || reservation.finishing) {
		return nil, fmt.Errorf("%w: wait for execution and review to stop", web.ErrInvalidState)
	}
	return st, nil
}

func (d *Daemon) WorkflowRestart(id string) error {
	release, err := d.ordinaryMutationAdmission(id)
	if err != nil {
		return err
	}
	defer release()
	var saved *ticket.Ticket
	err = d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := d.workflowActionTicket(repo, id, false)
		if err != nil {
			return err
		}
		t := st.Ticket
		cfg := d.config()
		if _, err := loadExecution(cfg, id); !errors.Is(err, os.ErrNotExist) || t.InvocationID != "" {
			return fmt.Errorf("%w: acknowledge interrupted workflow recovery before restart", web.ErrInvalidState)
		}
		graph := cfg.Pipelines[t.Pipeline].Graph
		fingerprint, err := workflowFingerprint(cfg, t, graph)
		if err != nil {
			return err
		}
		state := &ticket.GraphState{Version: 1, ExecutionID: newSessionID(), DefinitionFingerprint: fingerprint}
		populateWorkflowDisplay(state, graph)
		state.RetainExecution(t.GraphState)
		after := pipeline.AdvanceGraph(graph, "", state, nil)
		if err := installGraphAdvance(t, after); err != nil {
			return err
		}
		for _, field := range []string{"invocation_id", "started_at", "completed_at", "summary", "final_summary", "last_log", "scheduled_at"} {
			if err := t.DeleteField(field); err != nil {
				return err
			}
		}
		if err := t.SetField("attempt", 0); err != nil {
			return err
		}
		if after.Status == ticket.StatusDone {
			if err := t.SetField("completed_at", time.Now()); err != nil {
				return err
			}
		}
		if err := repo.Save(st); err != nil {
			return err
		}
		saved = t.Clone()
		return nil
	})
	if err != nil {
		return mapAppError(err)
	}
	if saved.Status == ticket.StatusTodo {
		(&daemonRuntime{d: d}).Enqueue(saved)
	}
	(&daemonRuntime{d: d}).ReconcileDependencies(id)
	d.broadcastTicketUpdateLocking(id)
	return nil
}

func (d *Daemon) WorkflowRecover(id, executionID string, revision uint64) error {
	release, err := d.ordinaryMutationAdmission(id)
	if err != nil {
		return err
	}
	defer release()
	err = d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := d.workflowActionTicket(repo, id, true)
		if err != nil {
			return err
		}
		t := st.Ticket
		state := t.GraphState
		if state == nil || executionID == "" || state.ExecutionID != executionID || state.Revision != revision || revision == math.MaxUint64 {
			return fmt.Errorf("%w: stale workflow recovery identity or revision", web.ErrInvalidState)
		}
		cfg := d.config()
		fingerprint, err := workflowFingerprint(cfg, t, cfg.Pipelines[t.Pipeline].Graph)
		if err != nil || fingerprint != state.DefinitionFingerprint {
			return fmt.Errorf("%w: workflow definition changed; restore the execution configuration before recovery", web.ErrInvalidState)
		}
		rec, err := loadExecution(cfg, id)
		if err != nil {
			return fmt.Errorf("%w: read workflow recovery journal: %v", web.ErrInvalidState, err)
		}
		if rec.GraphBefore == nil || rec.Instance != d.instanceName || rec.Invocation.ID != t.InvocationID ||
			rec.Invocation.WorkflowExecutionID != executionID || rec.Invocation.NodeVisitID != state.VisitID ||
			rec.Invocation.DefinitionFingerprint != fingerprint || !executionLifecycleMatches(t, rec) || rec.Phase == "transition" {
			return fmt.Errorf("%w: workflow recovery journal does not match the current execution", web.ErrInvalidState)
		}
		after := state.Clone()
		after.Revision++
		after.LastResult = nil
		rec.RecoveryState = after
		rec.Phase = "acknowledged"
		if err := saveExecution(cfg, rec); err != nil {
			return err
		}
		if err := installRecoveryAcknowledgement(t, rec); err != nil {
			return err
		}
		if err := repo.Save(st); err != nil {
			return err
		}
		if err := os.Remove(executionPath(cfg, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(d.pendingFinalizations, rec.Invocation.ID)
		d.releaseRecoveredExecutionLocked(id)
		return nil
	})
	if err == nil {
		d.broadcastTicketUpdateLocking(id)
	}
	return mapAppError(err)
}

func installRecoveryAcknowledgement(t *ticket.Ticket, rec *executionRecord) error {
	if err := t.SetField("graph_state", rec.RecoveryState.Clone()); err != nil {
		return err
	}
	if err := t.SetField("invocation_id", ""); err != nil {
		return err
	}
	if err := t.SetField("status", string(ticket.StatusPaused)); err != nil {
		return err
	}
	if err := t.SetField("last_error", "workflow recovery acknowledged; retry the node or restart the workflow explicitly"); err != nil {
		return err
	}
	return preserveWorkflowRecoveryHistory(t, rec)
}

func preserveWorkflowRecoveryHistory(t *ticket.Ticket, rec *executionRecord) error {
	if rec.PendingHistory == nil {
		return nil
	}
	for _, h := range t.History {
		if h.InvocationID == rec.Invocation.ID {
			return nil
		}
	}
	// The runner finished, but recovery must not record a successful node transition.
	h := *rec.PendingHistory
	h.RunOutcome = ticket.RunOutcomeFailed
	h.OutcomeReason = "workflow recovery acknowledged without a committed node transition"
	setGraphHistoryProvenance(&h, rec)
	return t.SetField("history", append(t.History, h))
}

func workflowAcknowledgementMatches(t *ticket.Ticket, rec *executionRecord) bool {
	state, after := t.GraphState, rec.RecoveryState
	if state == nil || after == nil || rec.GraphBefore == nil || after.Revision == 0 {
		return false
	}
	identity := t.Clone()
	identity.GraphState = rec.GraphBefore
	return after.Revision-1 == rec.GraphBefore.Revision && after.ExecutionID == rec.GraphBefore.ExecutionID &&
		after.VisitID == rec.GraphBefore.VisitID && after.DefinitionFingerprint == rec.GraphBefore.DefinitionFingerprint &&
		state.ExecutionID == after.ExecutionID && state.VisitID == after.VisitID &&
		state.DefinitionFingerprint == after.DefinitionFingerprint && executionLifecycleMatches(identity, rec) &&
		(t.InvocationID == "" || t.InvocationID == rec.Invocation.ID) &&
		(state.Revision == after.Revision || state.Revision+1 == after.Revision)
}

func (d *Daemon) reconcileWorkflowAcknowledgementLocked(t *ticket.Ticket, path string, rec *executionRecord) bool {
	if rec.Phase != "acknowledged" {
		return false
	}
	state, after := t.GraphState, rec.RecoveryState
	if !workflowAcknowledgementMatches(t, rec) {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	cfg := d.config()
	definition := cfg.Pipelines[t.Pipeline]
	if !definition.IsGraph() {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	fingerprint, err := workflowFingerprint(cfg, t, definition.Graph)
	if err != nil || fingerprint != state.DefinitionFingerprint {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	committed := state.Revision == after.Revision && t.InvocationID == ""
	if !committed && t.Status != ticket.StatusPaused && t.Status != ticket.StatusTodo && !t.IsRunning() {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	before := t.Clone()
	if !committed {
		err = installRecoveryAcknowledgement(t, rec)
	} else {
		// A saved acknowledgement may still lack the pending history.
		err = preserveWorkflowRecoveryHistory(t, rec)
	}
	if err != nil {
		*t = *before
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	if !committed || len(t.History) != len(before.History) {
		if err := d.writeTicketLocked(t, path, notify.OriginRequest); err != nil {
			*t = *before
			d.reserveRecoveredExecutionLocked(t)
			return true
		}
	}
	if err := os.Remove(executionPath(d.config(), t.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	d.releaseRecoveredExecutionLocked(t.ID)
	return true
}
