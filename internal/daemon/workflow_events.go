package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// Event storage is independent of disposable execution logs and of .kontora
// (whose presence changes board ownership). Serializing acceptance and replay
// also makes duplicate requests within one daemon unambiguous. Never enter this
// lock with d.mu held. Board/ticket mutations still use their ordinary locks.
var workflowEventsMu sync.Mutex

type workflowEvent struct {
	Version      int                           `json:"version"`
	Kind         string                        `json:"kind"`
	TicketID     string                        `json:"ticket_id,omitempty"`
	Human        *web.WorkflowReleaseRequest   `json:"human,omitempty"`
	Deployment   *web.DeploymentReleaseRequest `json:"deployment,omitempty"`
	Receipt      *ticket.WorkflowReceipt       `json:"receipt"`
	Applications []*workflowEventApplication   `json:"applications,omitempty"`
}

// Every application is journaled before changing its ticket. A deployment event
// remains available after all current applications finish: historical receipts
// intentionally satisfy waits registered later, including several tickets.
type workflowEventApplication struct {
	TicketID    string                `json:"ticket_id"`
	FilePath    string                `json:"file_path"`
	RepoPath    string                `json:"repo_path"`
	Branch      string                `json:"branch"`
	Pipeline    string                `json:"pipeline"`
	Stage       string                `json:"stage"`
	ExecutionID string                `json:"execution_id"`
	WaitID      string                `json:"wait_id"`
	Revision    uint64                `json:"revision"`
	Fingerprint string                `json:"fingerprint"`
	After       pipeline.GraphAdvance `json:"after"`
	Applied     bool                  `json:"applied,omitempty"`
}

func workflowEventKey(kind, id string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

func (d *Daemon) workflowEventsDir() string {
	return filepath.Join(expandTilde(d.config().TicketsDir), ".workflow-events")
}

func (d *Daemon) workflowEventPath(kind, id string) string {
	return filepath.Join(d.workflowEventsDir(), workflowEventKey(kind, id)+".json")
}

func readWorkflowEvent(path string) (*workflowEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var event workflowEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	if event.Version != 1 || event.Receipt == nil || event.Receipt.ID == "" ||
		(event.Kind != "human" && event.Kind != "deployment") ||
		(event.Kind == "human" && event.Human == nil) || (event.Kind == "deployment" && event.Deployment == nil) {
		return nil, fmt.Errorf("invalid workflow event %s", path)
	}
	if event.Kind == "human" {
		if event.Human.Validate() != nil || event.TicketID == "" || len(event.Applications) != 1 || event.Receipt.ID != workflowEventKey("human", event.Human.RequestID) {
			return nil, fmt.Errorf("invalid human workflow event %s", path)
		}
	} else if event.Deployment.Validate() != nil || event.Receipt.ID != workflowEventKey("deployment", event.Deployment.EventID) || event.Receipt.Repository == "" {
		return nil, fmt.Errorf("invalid deployment workflow event %s", path)
	}
	for _, application := range event.Applications {
		if application == nil || application.After.State == nil || application.TicketID == "" || application.WaitID == "" ||
			application.After.State.ExecutionID != application.ExecutionID || application.After.State.DefinitionFingerprint != application.Fingerprint ||
			application.After.State.Revision == 0 || application.After.State.Revision-1 != application.Revision {
			return nil, fmt.Errorf("invalid workflow event application %s", path)
		}
	}
	return &event, nil
}

func saveWorkflowEvent(path string, event *workflowEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

func workflowReleaseConflict(reason string) error {
	return fmt.Errorf("%w: %s", web.ErrInvalidState, reason)
}

func sameHumanRelease(a, b web.WorkflowReleaseRequest) bool {
	// Operator is audit evidence, not caller-controlled request payload. A retry
	// through another authenticated connection returns the original attribution.
	a.Operator, b.Operator = "", ""
	return a == b
}

func sameDeploymentRelease(a, b web.DeploymentReleaseRequest) bool {
	a.Operator, b.Operator = "", ""
	return a == b
}

func (d *Daemon) checkAcceptedWaitRelease(t *ticket.Ticket, receiptID string) error {
	paths, err := filepath.Glob(filepath.Join(d.workflowEventsDir(), "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		event, err := readWorkflowEvent(path)
		if err != nil {
			return err
		}
		if event.Receipt.ID == receiptID {
			continue
		}
		// Retry invalidates older applications. Their immutable receipts cannot
		// reserve a release at the newly inspected revision.
		for _, application := range event.Applications {
			if application != nil && application.TicketID == t.ID && application.ExecutionID == t.GraphState.ExecutionID && application.WaitID == t.GraphState.Wait.ID && application.Revision == t.GraphState.Revision {
				return workflowReleaseConflict("wait already has an accepted release receipt")
			}
		}
	}
	return nil
}

func (d *Daemon) releaseApplication(t *ticket.Ticket, path string, receipt *ticket.WorkflowReceipt) (*workflowEventApplication, error) {
	s := t.GraphState
	if s == nil || s.Wait == nil || t.Status != ticket.StatusPaused || t.InvocationID != "" {
		return nil, workflowReleaseConflict("release requires a parked declared wait")
	}
	w := s.Wait
	if err := d.checkAcceptedWaitRelease(t, receipt.ID); err != nil {
		return nil, err
	}
	if w.Phase != "pending" || w.Disarmed || (w.Kind != "human" && w.Kind != "deployment") ||
		w.ExecutionID != s.ExecutionID || w.NodeID != t.Stage || w.VisitID != s.VisitID || w.InvocationID != "" {
		return nil, workflowReleaseConflict("wait is disarmed, stale or requires recovery")
	}
	if !w.Deadline.IsZero() && !d.waitNow().Before(w.Deadline) {
		return nil, workflowReleaseConflict("wait deadline has elapsed")
	}
	cfg := d.config()
	graph := cfg.Pipelines[t.Pipeline].Graph
	fingerprint, err := workflowFingerprint(cfg, t, graph)
	if err != nil || fingerprint != s.DefinitionFingerprint {
		return nil, workflowReleaseConflict("workflow definition changed")
	}
	if w.Kind == "deployment" || w.Worktree != "" {
		if err := validateWaitWorkspace(t, w); err != nil {
			return nil, workflowReleaseConflict(err.Error())
		}
	}
	if _, err := loadExecution(cfg, t.ID); !errors.Is(err, os.ErrNotExist) {
		return nil, workflowReleaseConflict("workflow invocation journal must retire before release")
	}
	// The interpreter increments once; patchWorkflowWait also increments before
	// the callback, and installing this frozen state preserves that same revision.
	before := s.Clone()
	before.Wait.Receipt = receipt.Clone()
	after := pipeline.ReleaseGraphWait(graph, t.Stage, before, "released")
	if after.Rejected {
		return nil, workflowReleaseConflict(after.Reason)
	}
	if after.Next != nil {
		after.State.Continuation = true
		after.State.Wake = &ticket.WakeContext{Reason: "released", WaitID: w.ID, Receipt: receipt.Clone()}
	}
	return &workflowEventApplication{TicketID: t.ID, FilePath: path, RepoPath: t.Path, Branch: t.Branch,
		Pipeline: t.Pipeline, Stage: t.Stage, ExecutionID: s.ExecutionID, WaitID: w.ID, Revision: s.Revision,
		Fingerprint: s.DefinitionFingerprint, After: after}, nil
}

// WorkflowRelease acknowledges only after acceptance and the selected graph
// transition are on disk. Reconciliation applies a receipt only at its accepted
// revision. Stale request retries return the stored receipt without advancing.
func (d *Daemon) WorkflowRelease(id string, req web.WorkflowReleaseRequest) (*ticket.WorkflowReceipt, error) {
	if err := req.Validate(); err != nil {
		return nil, workflowReleaseConflict(err.Error())
	}
	workflowEventsMu.Lock()
	defer workflowEventsMu.Unlock()
	d.reconcileControlTransitionsLocked()
	path := d.workflowEventPath("human", req.RequestID)
	if event, err := readWorkflowEvent(path); err == nil {
		if event.Kind != "human" || event.TicketID != id || !sameHumanRelease(*event.Human, req) {
			return nil, workflowReleaseConflict("request_id was already accepted with a different payload")
		}
		d.reconcileWorkflowEvent(path, event)
		return event.Receipt.Clone(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	release, err := d.ordinaryMutationAdmission(id)
	if err != nil {
		return nil, err
	}
	defer release()
	receipt := &ticket.WorkflowReceipt{ID: workflowEventKey("human", req.RequestID), RequestID: req.RequestID,
		Reason: req.Reason, Operator: req.Operator}
	event := &workflowEvent{Version: 1, Kind: "human", TicketID: id, Human: &req, Receipt: receipt}
	// Linearize identity validation and durable acceptance against lifecycle
	// mutations. Application happens afterwards, outside both board and d.mu.
	err = d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := d.workflowActionTicket(repo, id, false)
		if err != nil {
			return err
		}
		t := st.Ticket
		if t.GraphState == nil || t.GraphState.Wait == nil || t.GraphState.ExecutionID != req.ExecutionID ||
			t.GraphState.Wait.ID != req.WaitID || t.GraphState.Revision != req.Revision || t.GraphState.Wait.Kind != "human" {
			return workflowReleaseConflict("stale human release identity or revision")
		}
		receipt.AcceptedAt = d.waitNow().UTC()
		application, err := d.releaseApplication(t, st.FilePath, receipt)
		if err != nil {
			return err
		}
		event.Applications = []*workflowEventApplication{application}
		return saveWorkflowEvent(path, event)
	})
	if err != nil {
		return nil, mapAppError(err)
	}
	d.reconcileWorkflowEvent(path, event)
	d.signalWorkflowWait()
	return receipt.Clone(), nil
}

// DeploymentRelease records immutable local evidence. There is no ticket
// argument, no ref/HEAD substitution for Commit, and no network Git operation.
func (d *Daemon) DeploymentRelease(req web.DeploymentReleaseRequest) (*ticket.WorkflowReceipt, error) {
	receipt, err := d.acceptDeploymentRelease(req)
	if err == nil {
		d.reconcileWorkflowDeployment(context.Background(), d.workflowEventPath("deployment", req.EventID), make(deploymentMatchCache))
		d.signalWorkflowWait()
	}
	return receipt, err
}

func (d *Daemon) acceptDeploymentRelease(req web.DeploymentReleaseRequest) (*ticket.WorkflowReceipt, error) {
	if err := req.Validate(); err != nil {
		return nil, workflowReleaseConflict(err.Error())
	}
	workflowEventsMu.Lock()
	defer workflowEventsMu.Unlock()
	d.reconcileControlTransitionsLocked()
	path := d.workflowEventPath("deployment", req.EventID)
	// An identical retry returns its stored receipt even if the repository has
	// since become unavailable; acceptance must not depend on mutable Git state.
	if event, err := readWorkflowEvent(path); err == nil {
		if event.Kind != "deployment" || !sameDeploymentRelease(*event.Deployment, req) {
			return nil, workflowReleaseConflict("event_id was already accepted with a different payload")
		}
		d.reconcileWorkflowEvent(path, event)
		return event.Receipt.Clone(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := ticket.CheckBoardControl(expandTilde(d.config().TicketsDir)); err != nil {
		return nil, err
	}
	repository, _, err := worktree.DeploymentEvidence(expandTilde(req.Repository), expandTilde(req.Repository))
	if err != nil {
		return nil, workflowReleaseConflict("deployment repository: " + err.Error())
	}
	ok, err := worktree.DeploymentMatches(expandTilde(req.Repository), repository, req.Commit, req.Commit)
	if err != nil || !ok {
		return nil, workflowReleaseConflict(fmt.Sprintf("invalid local deployment commit: %v", err))
	}
	receipt := &ticket.WorkflowReceipt{ID: workflowEventKey("deployment", req.EventID), RequestID: req.EventID,
		Operator: req.Operator, Repository: repository, Commit: req.Commit, Environment: req.Environment, AcceptedAt: d.waitNow().UTC()}
	event := &workflowEvent{Version: 1, Kind: "deployment", Deployment: &req, Receipt: receipt}
	if err := saveWorkflowEvent(path, event); err != nil {
		return nil, err
	}
	d.reconcileWorkflowEvent(path, event)
	d.signalWorkflowWait()
	return receipt.Clone(), nil
}

// reconcileWorkflowEvents is called without d.mu on arrival, registration, and
// startup. Corrupt or unreadable events are skipped here, but block new
// applications when checking for competing receipts.
func (d *Daemon) reconcileWorkflowEvents() {
	d.reconcileWorkflowEventsContext(context.Background())
}

func (d *Daemon) reconcileWorkflowEventsContext(ctx context.Context) {
	d.reconcileControlTransitions()
	paths, err := filepath.Glob(filepath.Join(d.workflowEventsDir(), "*.json"))
	if err != nil {
		return
	}
	// Glob's sorted order supplies deterministic historical matching; receipts
	// prove an occurrence, not which commit is currently deployed.
	cache := make(deploymentMatchCache)
	for _, path := range paths {
		if ctx.Err() != nil {
			return
		}
		workflowEventsMu.Lock()
		event, err := readWorkflowEvent(path)
		if err == nil {
			d.reconcileWorkflowEvent(path, event)
		}
		workflowEventsMu.Unlock()
		if err != nil {
			d.log.Error("read workflow event", "path", path, "err", err)
			continue
		}
		if event.Kind == "deployment" {
			d.reconcileWorkflowDeployment(ctx, path, cache)
		}
	}
}

func (d *Daemon) reconcileWorkflowEvent(path string, event *workflowEvent) {
	for _, application := range event.Applications {
		if application != nil && !application.Applied {
			if err := d.applyWorkflowEvent(event, application); err != nil {
				continue
			}
			application.Applied = true
			if err := saveWorkflowEvent(path, event); err != nil {
				return
			}
		}
	}
}

func workflowEventHasApplication(event *workflowEvent, t *ticket.Ticket) bool {
	for _, application := range event.Applications {
		if application != nil && application.TicketID == t.ID && application.ExecutionID == t.GraphState.ExecutionID &&
			application.WaitID == t.GraphState.Wait.ID && application.Revision == t.GraphState.Revision {
			return true
		}
	}
	return false
}

func workflowEventRetained(t *ticket.Ticket, application *workflowEventApplication, receiptID string) bool {
	if t.GraphState == nil || t.GraphState.ExecutionID != application.ExecutionID {
		return false
	}
	for _, wait := range t.GraphState.WaitHistory {
		if wait.ID == application.WaitID && wait.Receipt != nil && wait.Receipt.ID == receiptID {
			return true
		}
	}
	return false
}

func (d *Daemon) applyWorkflowEvent(event *workflowEvent, application *workflowEventApplication) error {
	var t *ticket.Ticket
	var path string
	err := d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := repo.Get(application.TicketID)
		if err != nil {
			return err
		}
		t, path = st.Ticket.Clone(), st.FilePath
		return nil
	})
	if err != nil {
		return err
	}
	if workflowEventRetained(t, application, event.Receipt.ID) {
		d.enqueueWorkflowContinuation(t)
		return nil
	}
	s := t.GraphState
	if s == nil || s.Wait == nil || !sameJournalTicketPath(application.FilePath, path) || t.Path != application.RepoPath ||
		t.Branch != application.Branch || t.Pipeline != application.Pipeline || t.Stage != application.Stage ||
		s.ExecutionID != application.ExecutionID || s.Wait.ID != application.WaitID || s.Revision != application.Revision ||
		s.DefinitionFingerprint != application.Fingerprint || s.Wait.Disarmed || s.Wait.Phase != "pending" {
		return errExecutionChanged
	}
	saved, err := d.patchWorkflowWait(t, path, func(current *ticket.Ticket) error {
		if current.Status != ticket.StatusPaused || current.GraphState.Wait.Disarmed || current.GraphState.Wait.Phase != "pending" {
			return errExecutionChanged
		}
		if current.GraphState.Wait.Kind == "deployment" || current.GraphState.Wait.Worktree != "" {
			if err := validateWaitWorkspace(current, current.GraphState.Wait); err != nil {
				return err
			}
		}
		after := application.After
		after.State = after.State.Clone()
		// Register the next control wait / completion cleanup marker inside the
		// same fresh ticket mutation, never after enqueueing or acknowledging it.
		return d.finishControlAdvance(current, after)
	})
	if err != nil {
		return err
	}
	d.enqueueWorkflowContinuation(saved)
	d.signalWorkflowWait()
	return nil
}
