package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

type workflowControlTransition struct {
	TicketID       string             `json:"ticket_id"`
	Path           string             `json:"path"`
	RepoPath       string             `json:"repo_path"`
	Branch         string             `json:"branch"`
	Pipeline       string             `json:"pipeline"`
	Stage          string             `json:"stage"`
	ExecutionID    string             `json:"execution_id"`
	WaitID         string             `json:"wait_id"`
	BeforeRevision uint64             `json:"before_revision"`
	After          *ticket.GraphState `json:"after"`
	AfterStage     string             `json:"after_stage"`
	Status         ticket.Status      `json:"status"`
	CompletedAt    *time.Time         `json:"completed_at,omitempty"`
	LastError      string             `json:"last_error"`
}

func workflowControlPath(cfg *config.Config, id string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), id, "workflow-control.json")
}
func (d *Daemon) persistControlTransition(before, after *ticket.Ticket) error {
	w := workflowWait(before)
	if w == nil || (w.Kind != "human" && w.Kind != "deployment") || before.InvocationID != "" || before.GraphState.Revision == 0 || after.GraphState.ExecutionID != before.GraphState.ExecutionID {
		return nil
	}
	j := workflowControlTransition{TicketID: before.ID, Path: filepath.Join(expandTilde(d.config().TicketsDir), before.ID+".md"), RepoPath: before.Path, Branch: before.Branch, Pipeline: before.Pipeline, Stage: before.Stage, ExecutionID: before.GraphState.ExecutionID, WaitID: w.ID, BeforeRevision: before.GraphState.Revision - 1, After: after.GraphState.Clone(), AfterStage: after.Stage, Status: after.Status, CompletedAt: after.CompletedAt, LastError: after.LastError}
	path := workflowControlPath(d.config(), before.ID)
	if data, readErr := os.ReadFile(path); readErr == nil {
		var previous workflowControlTransition
		if json.Unmarshal(data, &previous) != nil {
			return fmt.Errorf("unreadable workflow control transition")
		}
		current, parseErr := ticket.ParseFile(j.Path)
		if parseErr != nil {
			return parseErr
		}
		if !reflect.DeepEqual(current.GraphState, previous.After) && previous.ExecutionID == j.ExecutionID && previous.WaitID == j.WaitID {
			return fmt.Errorf("workflow control transition awaits reconciliation")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}
func (j *workflowControlTransition) matches(t *ticket.Ticket, path string) bool {
	return path == j.Path && t.Path == j.RepoPath && t.Branch == j.Branch && t.Pipeline == j.Pipeline && t.GraphState != nil && t.GraphState.ExecutionID == j.ExecutionID
}

func (d *Daemon) reconcileControlTransitions() {
	workflowEventsMu.Lock()
	defer workflowEventsMu.Unlock()
	d.reconcileControlTransitionsLocked()
}
func (d *Daemon) reconcileControlTransitionsLocked() {
	paths, err := filepath.Glob(filepath.Join(expandTilde(d.config().LogsDir), "*", "workflow-control.json"))
	if err != nil {
		return
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var j workflowControlTransition
		if json.Unmarshal(data, &j) != nil || !ticket.IsSafeID(j.TicketID) || j.After == nil || path != workflowControlPath(d.config(), j.TicketID) {
			continue
		}
		var saved *ticket.Ticket
		err = d.withTicketMutationOrigin(notify.OriginDaemon, func(repo app.MutationRepository) error {
			st, err := repo.Get(j.TicketID)
			if err != nil {
				return err
			}
			t := st.Ticket
			if !j.matches(t, st.FilePath) {
				return errExecutionChanged
			}
			if reflect.DeepEqual(t.GraphState, j.After) && t.Stage == j.AfterStage && t.Status == j.Status {
				saved = t.Clone()
				return nil
			}
			w := workflowWait(t)
			if w == nil || w.ID != j.WaitID || t.Stage != j.Stage || t.GraphState.Revision != j.BeforeRevision || t.HasOwnershipMetadata() || d.claimedElsewhere(t) || t.IsRunning() || t.InvocationID != "" {
				return errExecutionChanged
			}
			if err := d.validateWaitProgress(t, w); err != nil {
				return err
			}
			if err := t.SetField("graph_state", j.After.Clone()); err != nil {
				return err
			}
			_ = t.SetField("stage", j.AfterStage)
			_ = t.SetField("status", string(j.Status))
			_ = t.SetField("last_error", j.LastError)
			if j.CompletedAt != nil {
				_ = t.SetField("completed_at", j.CompletedAt)
			}
			if err := repo.Save(st); err != nil {
				return err
			}
			saved = t.Clone()
			return nil
		})
		if err == nil || errors.Is(err, errExecutionChanged) {
			_ = os.Remove(path)
			if saved != nil {
				d.enqueueWorkflowContinuation(saved)
				d.broadcastTicketUpdateLocking(saved.ID)
			}
		}
	}
}
