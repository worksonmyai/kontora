package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/metrics"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// executionRecord survives independently of optional conversation resume records.
type executionRecord struct {
	Invocation           session.Invocation   `json:"invocation"`
	PreviousInvocationID *string              `json:"previous_invocation_id,omitempty"`
	EpicID               string               `json:"epic_id"`
	Instance             string               `json:"instance"`
	Worktree             string               `json:"worktree"`
	Pipeline             string               `json:"pipeline"`
	TicketStage          string               `json:"ticket_stage"`
	Attempt              int                  `json:"attempt"`
	Branch               string               `json:"branch"`
	RepoPath             string               `json:"repo_path"`
	RepoCommonDir        string               `json:"repo_common_dir,omitempty"`
	Phase                string               `json:"phase"`
	Agent                string               `json:"agent"`
	Run                  int                  `json:"run"`
	PendingHistory       *ticket.HistoryEntry `json:"pending_history,omitempty"`
	Transition           *executionTransition `json:"transition,omitempty"`
}

type executionTransition struct {
	Status      ticket.Status       `json:"status"`
	Stage       string              `json:"stage"`
	Attempt     int                 `json:"attempt"`
	CompletedAt *time.Time          `json:"completed_at,omitempty"`
	LastError   string              `json:"last_error"`
	Summary     string              `json:"summary"`
	History     ticket.HistoryEntry `json:"history"`
}

func executionPath(cfg *config.Config, id string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), id, "invocation.json")
}

func saveExecution(cfg *config.Config, rec *executionRecord) error {
	path := executionPath(cfg, rec.Invocation.TicketID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o600)
}

func loadExecution(cfg *config.Config, id string) (*executionRecord, error) {
	data, err := os.ReadFile(executionPath(cfg, id))
	if err != nil {
		return nil, err
	}
	var rec executionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.Invocation.TicketID != id || rec.Invocation.ID == "" || rec.EpicID == "" {
		return nil, fmt.Errorf("invalid active invocation")
	}
	if _, err := session.InvocationFromEnv(func(key string) string { return rec.Invocation.Env()[key] }); err != nil {
		return nil, err
	}
	expected, err := filepath.Abs(filepath.Join(expandTilde(cfg.LogsDir), id, "results", rec.Invocation.ID+".json"))
	if err != nil {
		return nil, err
	}
	if rec.Invocation.ResultPath != expected {
		return nil, fmt.Errorf("invalid invocation artifact path")
	}
	return &rec, nil
}

func (d *Daemon) acceptObservedTicket(t *ticket.Ticket, path string) bool {
	if t.ID == "" {
		return false
	}
	if !ticket.IsCanonicalPath(path, t.ID) {
		d.log.Warn("ignoring non-canonical ticket file", "path", path, "id", t.ID)
		return false
	}
	if t.HasOwnershipMetadata() {
		d.log.Error("external managed ticket requires its owning daemon", "ticket", t.ID)
		return false
	}
	return true
}

func sameObservedFile(before, after os.FileInfo, beforeErr, afterErr error) bool {
	return beforeErr == nil && afterErr == nil && os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func (d *Daemon) supervision(t *ticket.Ticket, annotation bool) (string, error) {
	if annotation {
		return "", nil
	}
	if t.IsAgentEpic() {
		return t.ID, nil
	}
	if t.Parent == "" {
		return "", nil
	}
	if !ticket.IsSafeID(t.Parent) {
		return "", fmt.Errorf("read supervision scope: unsafe parent ID")
	}
	parent, err := ticket.ParseFile(filepath.Join(expandTilde(d.config().TicketsDir), t.Parent+".md"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read supervision scope: %w", err)
	}
	if parent.ID == t.Parent && parent.IsAgentEpic() {
		return parent.ID, nil
	}
	return "", nil
}

func (d *Daemon) bindExecution(t *ticket.Ticket, p spawnAgentParams, params *RunnerParams, epicID string) (*executionRecord, error) {
	for _, key := range []string{"KONTORA_RESULT_VERSION", "KONTORA_INVOCATION_ID", "KONTORA_RESULT_TICKET", "KONTORA_RESULT_STAGE", "KONTORA_RESULT_FILE"} {
		params.Env[key] = ""
	}
	if epicID == "" {
		return nil, nil
	}
	if err := validateSupervisedAgent(p.agentCfg); err != nil {
		return nil, err
	}
	id := newSessionID()
	resultPath, err := filepath.Abs(filepath.Join(expandTilde(p.cfg.LogsDir), p.ticketID, "results", id+".json"))
	if err != nil {
		return nil, err
	}
	commonDir, err := worktree.CommonDir(expandTilde(t.Path))
	if err != nil {
		return nil, err
	}
	inv := session.Invocation{Version: 1, ID: id, TicketID: p.ticketID, Stage: p.stageName, ResultPath: resultPath}
	rec := &executionRecord{Invocation: inv, EpicID: epicID, Instance: d.instanceName,
		Worktree: p.wtPath, Pipeline: t.Pipeline, TicketStage: t.Stage, Attempt: t.Attempt,
		Branch: t.Branch, RepoPath: t.Path, RepoCommonDir: commonDir, Phase: "preparing", Agent: p.agentName, Run: p.run}
	expected := executionOf(t)
	err = d.withTicketMutationOrigin(notify.OriginDaemon, func(repo app.MutationRepository) error {
		st, err := repo.Get(t.ID)
		if err != nil {
			return err
		}
		current := st.Ticket
		claim := d.executions[t.ID]
		if st.FilePath != p.filePath || current.Status != ticket.StatusInProgress || d.claimedElsewhere(current) || current.HasOwnershipMetadata() ||
			executionOf(current) != expected || (claim != nil && claim.expected != expected) {
			return errExecutionChanged
		}
		// The previous identity distinguishes an interrupted binding from a different invocation.
		rec.PreviousInvocationID = new(current.InvocationID)
		if err := saveExecution(p.cfg, rec); err != nil {
			return err
		}
		if err := current.SetField("invocation_id", id); err != nil {
			return err
		}
		if err := repo.Save(st); err != nil {
			return err
		}
		*t = *current
		if claim != nil {
			// Binding rotates only the invocation, not the daemon-local claim.
			claim.expected = executionOf(current)
		}
		// A runner may start only after the ticket binding and launch commit are written.
		rec.Phase = "running"
		return saveExecution(p.cfg, rec)
	})
	if err != nil {
		return nil, err
	}
	maps.Copy(params.Env, inv.Env())
	params.RequireTermination = true
	params.TerminalReported = func() bool { return terminalReported(inv.ResultPath) }
	return rec, nil
}

func (d *Daemon) invocationScope(t *ticket.Ticket, p spawnAgentParams) (string, error) {
	if p.supervisionResolved {
		return p.supervisionID, nil
	}
	if !p.annotation && t.InvocationID != "" {
		rec, err := loadExecution(p.cfg, t.ID)
		if err == nil && rec.Phase == "recovered" && rec.Invocation.ID == t.InvocationID && rec.Instance == d.instanceName && rec.Pipeline == t.Pipeline && rec.TicketStage == t.Stage {
			return rec.EpicID, nil
		}
	}
	return d.supervision(t, p.annotation)
}

func terminalReported(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}

func classifyExecution(rec *executionRecord, run *agentRun, failure string) {
	if rec == nil {
		return
	}
	run.Execution = rec
	report, err := session.ReadResult(rec.Invocation.ResultPath)
	run.Reported = !errors.Is(err, os.ErrNotExist)
	switch {
	case err == nil && report.Outcome == "blocked":
		run.Outcome, run.OutcomeReason = "blocked", report.Summary+" Next step: "+report.NextStep
	case failure != "":
		run.Outcome, run.OutcomeReason = "failed", failure
	case run.Result.ExitCode != 0:
		run.Outcome, run.OutcomeReason = "failed", fmt.Sprintf("agent exited with code %d", run.Result.ExitCode)
	case err != nil:
		run.Outcome = "protocol_error"
		if errors.Is(err, os.ErrNotExist) {
			run.OutcomeReason = "result protocol: missing current-invocation report"
		} else {
			run.OutcomeReason = "result protocol: " + err.Error()
		}
	default:
		run.Outcome, run.OutcomeReason = "complete", report.Summary
	}
}

func executionLifecycleMatches(t *ticket.Ticket, rec *executionRecord) bool {
	return t.Stage == rec.TicketStage && t.Pipeline == rec.Pipeline && t.Attempt == rec.Attempt &&
		t.Branch == rec.Branch && t.Path == rec.RepoPath && !t.HasOwnershipMetadata()
}

func executionMatches(t *ticket.Ticket, rec *executionRecord) bool {
	return t.InvocationID == rec.Invocation.ID && t.Status == ticket.StatusInProgress && executionLifecycleMatches(t, rec)
}

func preparingExecutionMatches(t *ticket.Ticket, rec *executionRecord) bool {
	return rec.Phase == "preparing" && rec.PreviousInvocationID != nil &&
		t.InvocationID == *rec.PreviousInvocationID && executionLifecycleMatches(t, rec)
}

func committedExecutionMatches(t *ticket.Ticket, rec *executionRecord) bool {
	tr := rec.Transition
	if rec.Phase != "transition" || tr == nil || t.InvocationID != "" || t.ID != rec.Invocation.TicketID ||
		t.Pipeline != rec.Pipeline || t.Branch != rec.Branch || t.Path != rec.RepoPath ||
		t.Status != tr.Status || t.Stage != tr.Stage || t.Attempt != tr.Attempt ||
		!reflect.DeepEqual(t.CompletedAt, tr.CompletedAt) || t.LastError != tr.LastError || t.Summary != tr.Summary {
		return false
	}
	matches := 0
	for _, h := range t.History {
		if h.InvocationID == rec.Invocation.ID {
			if !reflect.DeepEqual(h, tr.History) {
				return false
			}
			matches++
		}
	}
	return matches == 1
}

func (d *Daemon) beginFinalization(cfg *config.Config, rec *executionRecord, runs ...agentRun) error {
	if rec == nil {
		return nil
	}
	rec.Phase = "finalizing"
	if len(runs) != 0 {
		run := runs[0]
		started, ended := run.Result.StartedAt, run.Result.ExitedAt
		rec.PendingHistory = &ticket.HistoryEntry{Stage: rec.Invocation.Stage, Agent: rec.Agent, Run: rec.Run,
			StartedAt: &started, CompletedAt: &ended, ExitCode: run.Result.ExitCode, Model: run.Model, Effort: run.Effort,
			Summary: runSummary("", run.FinalMessage), SessionKind: run.SessionKind, SessionRef: run.SessionRef,
			RunOutcome: ticket.RunOutcome(run.Outcome), OutcomeReason: run.OutcomeReason, InvocationID: rec.Invocation.ID}
	}
	return saveExecution(cfg, rec)
}

func persistTransition(cfg *config.Config, rec *executionRecord, t *ticket.Ticket) error {
	if rec == nil {
		return nil
	}
	var row ticket.HistoryEntry
	for _, h := range t.History {
		if h.InvocationID == rec.Invocation.ID {
			row = h
			break
		}
	}
	if row.InvocationID == "" {
		return fmt.Errorf("finalization has no invocation history")
	}
	rec.Transition = &executionTransition{Status: t.Status, Stage: t.Stage, Attempt: t.Attempt,
		CompletedAt: t.CompletedAt, LastError: t.LastError, Summary: t.Summary, History: row}
	rec.Phase = "transition"
	return saveExecution(cfg, rec)
}

func retireExecution(cfg *config.Config, rec *executionRecord) {
	if rec != nil {
		_ = os.Remove(executionPath(cfg, rec.Invocation.TicketID))
	}
}

func appendRunHistory(t *ticket.Ticket, run agentRun, agent, stage string, index int) {
	if run.Execution == nil {
		return
	}
	id := run.Execution.Invocation.ID
	for _, h := range t.History {
		if h.InvocationID == id {
			return
		}
	}
	started, ended := run.Result.StartedAt, run.Result.ExitedAt
	if started.IsZero() && t.StartedAt != nil {
		started = *t.StartedAt
	}
	if ended.IsZero() {
		ended = time.Now()
	}
	h := ticket.HistoryEntry{Stage: stage, Agent: agent, Run: index, Model: run.Model, Effort: run.Effort,
		ExitCode: run.Result.ExitCode, StartedAt: &started, CompletedAt: &ended,
		Summary: runSummary(t.Summary, run.FinalMessage), SessionKind: run.SessionKind, SessionRef: run.SessionRef,
		RunOutcome: ticket.RunOutcome(run.Outcome), OutcomeReason: run.OutcomeReason, InvocationID: id}
	_ = t.SetField("history", append(t.History, h))
}

func (d *Daemon) reconcileRetainedExecutionLocked(t *ticket.Ticket, path string) bool {
	if _, retained := d.recoveredReservations[t.ID]; !retained {
		return false
	}
	d.reconcileExecutionLocked(t, path)
	d.tickets[t.ID] = newTicketState(t, path)
	if t.Status == ticket.StatusTodo && *d.config().AutoPickUp {
		d.enqueue(t)
	}
	return true
}

// reconcileExecutionLocked is shared by startup and stale self-claim recovery.
func (d *Daemon) reconcileExecutionLocked(t *ticket.Ticket, path string) bool {
	cfg := d.config()
	fresh, readErr := ticket.ParseFile(path)
	if readErr != nil {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	*t = *fresh
	if d.recoveredReservations[t.ID].finishing || d.claimedElsewhere(t) || t.HasOwnershipMetadata() {
		return true
	}
	rec, err := loadExecution(cfg, t.ID)
	if (t.Status != ticket.StatusInProgress || !t.Kontora) && errors.Is(err, os.ErrNotExist) {
		d.reconcileLegacyOverrideLocked(t)
		return true
	}
	if errors.Is(err, os.ErrNotExist) && t.InvocationID == "" {
		epicID, scopeErr := d.supervision(t, t.AnnotationReturnStatus != "")
		if scopeErr == nil && epicID == "" {
			return false
		}
		if scopeErr != nil {
			if d.confirmRecoveryTerminationLocked(t) {
				d.pauseRecoveryLocked(t, path, scopeErr.Error())
			}
			return true
		}
		rec = &executionRecord{Invocation: session.Invocation{TicketID: t.ID}, EpicID: epicID, Instance: d.instanceName,
			TicketStage: t.Stage, Pipeline: t.Pipeline, Attempt: t.Attempt, Branch: t.Branch, RepoPath: t.Path, Phase: "legacy"}
		err = nil
	}
	if err != nil {
		if d.confirmRecoveryTerminationLocked(t) {
			d.pauseRecoveryLocked(t, path, "invocation recovery: "+err.Error())
		}
		return true
	}
	if rec.Instance != d.instanceName || d.claimedElsewhere(t) {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	if t.InvocationID != "" && t.InvocationID != rec.Invocation.ID && !preparingExecutionMatches(t, rec) {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	if rec.Phase == "recovered" && t.Status == ticket.StatusTodo && t.InvocationID == rec.Invocation.ID &&
		t.Stage == rec.TicketStage && t.Pipeline == rec.Pipeline && t.Attempt == rec.Attempt &&
		t.Branch == rec.Branch && t.Path == rec.RepoPath {
		d.releaseRecoveredExecutionLocked(t.ID)
		return true
	}
	if !d.confirmRecoveryTerminationLocked(t) {
		return true
	}
	return d.finishRecoveredExecutionLocked(t, path, rec)
}

func (d *Daemon) reconcileLegacyOverrideLocked(t *ticket.Ticket) {
	_, retained := d.recoveredReservations[t.ID]
	epicID, scopeErr := d.supervision(t, t.AnnotationReturnStatus != "")
	// Operator state does not prove that a journal-less supervised runner stopped.
	if (retained || epicID != "" || scopeErr != nil) && !d.confirmRecoveryTerminationLocked(t) {
		return
	}
	d.releaseRecoveredExecutionLocked(t.ID)
}

func (d *Daemon) finishRecoveredExecutionLocked(t *ticket.Ticket, path string, rec *executionRecord) bool {
	cfg := d.config()
	fresh, readErr := ticket.ParseFile(path)
	if readErr != nil {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	*t = *fresh
	if d.claimedElsewhere(t) || t.HasOwnershipMetadata() {
		return true
	}
	if committedExecutionMatches(t, rec) {
		d.completeRecoveredExecutionLocked(t, rec)
		return true
	}
	if !executionMatches(t, rec) && !preparingExecutionMatches(t, rec) {
		// An older journal can precede the first ticket binding. Never apply its reports.
		if rec.Phase == "running" && t.InvocationID == "" && t.Status == ticket.StatusInProgress && executionLifecycleMatches(t, rec) {
			_ = t.SetField("status", string(ticket.StatusTodo))
		}
		// Operator edits win, but only confirmed termination permits resource reuse.
		if t.InvocationID == rec.Invocation.ID {
			_ = t.SetField("invocation_id", "")
		}
		if err := d.writeTicketLocked(t, path, notify.OriginObserved); err != nil {
			d.reserveRecoveredExecutionLocked(t)
			return true
		}
		retireExecution(cfg, rec)
		delete(d.pendingFinalizations, rec.Invocation.ID)
		d.releaseRecoveredExecutionLocked(t.ID)
		return true
	}
	if rec.Phase == "transition" && rec.Transition == nil {
		d.pauseRecoveryLocked(t, path, "invocation recovery: missing transition; inspect before Retry")
		return true
	}
	if !d.applyRecoveredExecutionLocked(t, rec) {
		return true
	}
	if rec.Phase != "recovered" {
		_ = t.SetField("invocation_id", "")
	}
	if err := d.writeTicketLocked(t, path, notify.OriginObserved); err != nil {
		d.ticketLog(t.ID).Error("invocation recovery: write failed", "err", err)
		_ = t.SetField("status", string(ticket.StatusInProgress))
		_ = t.SetField("invocation_id", rec.Invocation.ID)
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	d.completeRecoveredExecutionLocked(t, rec)
	return true
}

func (d *Daemon) completeRecoveredExecutionLocked(t *ticket.Ticket, rec *executionRecord) {
	cfg := d.config()
	if rec.Phase != "recovered" {
		if finish := d.takeFinalizationLocked(rec); finish != nil {
			d.reserveRecoveredExecutionLocked(t)
			reservation := d.recoveredReservations[t.ID]
			reservation.finishing = true
			d.recoveredReservations[t.ID] = reservation
			current := t.Clone()
			d.background.Go(func() {
				finish(current)
				d.mu.Lock()
				defer d.mu.Unlock()
				retireExecution(cfg, rec)
				d.releaseRecoveredExecutionLocked(current.ID)
			})
			return
		}
		if rec.Phase == "transition" && t.Status == ticket.StatusDone && !t.IsAgentEpic() {
			d.removeWorktreeAt(d.ticketLog(t.ID), rec.RepoPath, rec.Worktree)
		}
		retireExecution(cfg, rec)
	}
	d.releaseRecoveredExecutionLocked(t.ID)
}

func (d *Daemon) applyRecoveredExecutionLocked(t *ticket.Ticket, rec *executionRecord) bool {
	cfg := d.config()
	switch rec.Phase {
	case "preparing":
		// No runner could start before the launch commit. Ignore reports for this identity.
		if t.Status == ticket.StatusInProgress {
			_ = t.SetField("status", string(ticket.StatusTodo))
		}
	case "legacy":
		_ = t.SetField("status", string(ticket.StatusTodo))
	case "transition":
		tr := rec.Transition
		_ = t.SetField("status", string(tr.Status))
		_ = t.SetField("stage", tr.Stage)
		_ = t.SetField("attempt", tr.Attempt)
		if tr.CompletedAt == nil {
			_ = t.DeleteField("completed_at")
		} else {
			_ = t.SetField("completed_at", tr.CompletedAt.Format(time.RFC3339))
		}
		_ = t.SetField("last_error", tr.LastError)
		_ = t.SetField("summary", tr.Summary)
		found := false
		for _, h := range t.History {
			if h.InvocationID == rec.Invocation.ID {
				found = true
				break
			}
		}
		if !found {
			_ = t.SetField("history", append(t.History, tr.History))
		}
	case "finalizing":
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", "invocation recovery: hook effects are unknown; inspect before Retry")
		if rec.PendingHistory != nil {
			h := *rec.PendingHistory
			h.RunOutcome = ticket.RunOutcomeFailed
			h.OutcomeReason = t.LastError
			found := false
			for _, existing := range t.History {
				if existing.InvocationID == rec.Invocation.ID {
					found = true
					break
				}
			}
			if !found {
				_ = t.SetField("history", append(t.History, h))
			}
		}
	case "running", "recovered":
		run := agentRun{}
		classifyExecution(rec, &run, "")
		// No runner status survived the crash. Use the existing unknown-status sentinel.
		run.Result.ExitCode = -1
		if run.Outcome == "blocked" || run.Outcome == "protocol_error" && run.Reported {
			_ = t.SetField("status", string(ticket.StatusPaused))
			_ = t.SetField("last_error", run.OutcomeReason)
			appendRunHistory(t, run, rec.Agent, rec.Invocation.Stage, rec.Run)
		} else {
			// A report alone does not prove that validation and shutdown completed.
			_ = t.SetField("status", string(ticket.StatusTodo))
			rec.Phase = "recovered"
			if err := saveExecution(cfg, rec); err != nil {
				d.reserveRecoveredExecutionLocked(t)
				_ = t.SetField("status", string(ticket.StatusInProgress))
				return false
			}
		}
	default:
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", "invocation recovery: invalid finalization phase")
	}
	return true
}

func (d *Daemon) pauseRecoveryLocked(t *ticket.Ticket, path, reason string) {
	current, err := ticket.ParseFile(path)
	if err != nil {
		d.reserveRecoveredExecutionLocked(t)
		return
	}
	if current.Status != ticket.StatusInProgress || d.claimedElsewhere(current) || current.InvocationID != t.InvocationID || current.Stage != t.Stage || current.HasOwnershipMetadata() {
		*t = *current
		return
	}
	*t = *current
	_ = t.SetField("status", string(ticket.StatusPaused))
	_ = t.SetField("last_error", reason)
	if err := d.writeTicketLocked(t, path, notify.OriginObserved); err != nil {
		_ = t.SetField("status", string(ticket.StatusInProgress))
		d.reserveRecoveredExecutionLocked(t)
		return
	}
	d.releaseRecoveredExecutionLocked(t.ID)
}

func (d *Daemon) confirmRecoveryTerminationLocked(t *ticket.Ticket) bool {
	windows, err := d.windows.list(d.tmuxSession)
	if err == nil && slices.Contains(windows, t.ID) {
		err = d.windows.kill(d.tmuxSession, t.ID)
		if err == nil {
			windows, err = d.windows.list(d.tmuxSession)
		}
	}
	if err == nil && !slices.Contains(windows, t.ID) {
		return true
	}
	d.ticketLog(t.ID).Error("invocation recovery: termination unconfirmed; reservation retained", "err", err)
	d.reserveRecoveredExecutionLocked(t)
	return false
}

type recoveredReservation struct {
	pool      chan struct{}
	token     bool
	branchKey string
	finishing bool
}

func (d *Daemon) reserveUnparsedExecutionLocked(path string) {
	id := strings.TrimSuffix(filepath.Base(path), ".md")
	if !ticket.IsSafeID(id) || !ticket.IsCanonicalPath(path, id) {
		return
	}
	rec, err := loadExecution(d.config(), id)
	if err != nil || rec.Instance != d.instanceName {
		return
	}
	// The journal protects resources, but an unreadable ticket cannot authorize termination or writes.
	d.reserveRecoveredExecutionLocked(&ticket.Ticket{ID: id})
}

func (d *Daemon) reserveRecoveredExecutionLocked(t *ticket.Ticket, transferredToken ...bool) {
	if _, exists := d.running[t.ID]; exists {
		return
	}
	pool := d.ticketPool(t)
	repoPath, branch, commonDir := t.Path, ticketBranch(d.config(), t), ""
	if rec, err := loadExecution(d.config(), t.ID); err == nil {
		repoPath, branch, commonDir = rec.RepoPath, rec.Branch, rec.RepoCommonDir
		pool = d.sem
		if rec.EpicID == rec.Invocation.TicketID {
			pool = d.epicSem
		}
	}
	if commonDir == "" {
		commonDir, _ = worktree.CommonDir(expandTilde(repoPath))
	}
	transferred := len(transferredToken) != 0 && transferredToken[0]
	reservation := recoveredReservation{pool: pool, token: transferred || len(pool) < cap(pool)}
	if commonDir != "" {
		reservation.branchKey = commonDir + "\x00" + branch
		if _, exists := d.runningBranches[reservation.branchKey]; !exists {
			d.runningBranches[reservation.branchKey] = t.ID
		}
	}
	if reservation.token && !transferred {
		pool <- struct{}{}
	}
	if d.recoveredReservations == nil {
		d.recoveredReservations = make(map[string]recoveredReservation)
	}
	d.recoveredReservations[t.ID] = reservation
	d.running[t.ID] = func() {
		d.background.Go(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if _, retained := d.recoveredReservations[t.ID]; !retained {
				return
			}
			if ts, ok := d.tickets[t.ID]; ok {
				current := ts.ticket.Clone()
				d.reconcileExecutionLocked(current, ts.filePath)
				d.setTicketState(t.ID, current, ts.filePath)
				if current.Status == ticket.StatusTodo && *d.config().AutoPickUp {
					d.enqueue(current)
				}
			}
		})
	}
}

func (d *Daemon) retainUncertainExecution(ctx context.Context, t *ticket.Ticket, rec *executionRecord) {
	retainOrdinaryReservation(ctx)
	if rec != nil {
		d.retainFinalizationReservation(ctx, t)
	}
}

func (d *Daemon) retainFinalizationReservation(ctx context.Context, t *ticket.Ticket) {
	retainOrdinaryReservation(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, t.ID)
	_, transferred := ctx.Value(dispatchPoolKey{}).(bool)
	d.reserveRecoveredExecutionLocked(t, transferred)
}

func (d *Daemon) branchHolderLocked(key string) (string, bool) {
	if holder, exists := d.runningBranches[key]; exists {
		return holder, true
	}
	for id, reservation := range d.recoveredReservations {
		if reservation.branchKey == key {
			return id, true
		}
	}
	return "", false
}

func (d *Daemon) releaseRecoveredExecutionLocked(id string) {
	reservation, exists := d.recoveredReservations[id]
	if !exists {
		return
	}
	delete(d.recoveredReservations, id)
	delete(d.executions, id)
	delete(d.running, id)
	delete(d.liveRuns, id)
	for key, holder := range d.runningBranches {
		if holder == id {
			delete(d.runningBranches, key)
		}
	}
	if reservation.token {
		<-reservation.pool
	}
	d.queueCond.Broadcast()
}

// Recovery can discover more executions than the configured channel can hold.
func (d *Daemon) availablePoolSlotsLocked(pool chan struct{}) int {
	available := cap(pool) - len(pool)
	for _, reservation := range d.recoveredReservations {
		if reservation.pool == pool && !reservation.token {
			available--
		}
	}
	return available
}

// patchFinalExecution journals and commits the final transition inside the same
// claim-checked mutation. A stale exit must not leave a replayable transition.
func (d *Daemon) patchFinalExecution(ctx context.Context, snapshot *ticket.Ticket, path string, rec *executionRecord, patch func(*ticket.Ticket) error, finish func(*ticket.Ticket)) (*ticket.Ticket, error) {
	saved, err := d.patchExecution(ctx, snapshot, path, func(current *ticket.Ticket) error {
		if rec != nil && !executionMatches(current, rec) {
			return errExecutionChanged
		}
		if err := patch(current); err != nil {
			return err
		}
		if err := persistTransition(d.config(), rec, current); err != nil {
			return err
		}
		if rec != nil {
			_ = current.SetField("invocation_id", "")
			d.rememberFinalizationLocked(rec, finish)
		}
		return nil
	})
	if err != nil && rec != nil && rec.Phase == "transition" && !errors.Is(err, errExecutionChanged) {
		d.retainFinalizationReservation(ctx, snapshot)
	}
	return saved, err
}

func (d *Daemon) rememberFinalization(rec *executionRecord, finish func(*ticket.Ticket)) {
	if rec == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rememberFinalizationLocked(rec, finish)
}

func (d *Daemon) rememberFinalizationLocked(rec *executionRecord, finish func(*ticket.Ticket)) {
	if d.pendingFinalizations == nil {
		d.pendingFinalizations = make(map[string]func(*ticket.Ticket))
	}
	d.pendingFinalizations[rec.Invocation.ID] = finish
}

func (d *Daemon) takeFinalizationLocked(rec *executionRecord) func(*ticket.Ticket) {
	finish := d.pendingFinalizations[rec.Invocation.ID]
	delete(d.pendingFinalizations, rec.Invocation.ID)
	return finish
}

func (d *Daemon) completeFinalization(rec *executionRecord, t *ticket.Ticket) {
	if rec == nil {
		return
	}
	d.mu.Lock()
	finish := d.takeFinalizationLocked(rec)
	d.mu.Unlock()
	if finish != nil {
		finish(t)
	}
	d.mu.Lock()
	retireExecution(d.config(), rec)
	d.mu.Unlock()
}

func (d *Daemon) recordValidatedRun(ctx context.Context, stage, agent, pipelineName string, run agentRun) {
	if run.Execution == nil {
		return
	}
	outcome := run.Outcome
	if outcome == "complete" {
		outcome = metrics.OutcomeSuccess
	}
	if outcome == "failed" {
		outcome = metrics.OutcomeFailure
	}
	if ctx.Err() != nil {
		outcome = metrics.OutcomeCancelled
	}
	elapsed := run.Result.ExitedAt.Sub(run.Result.StartedAt)
	elapsed = max(elapsed, 0)
	d.metrics.StageRun(ctx, metrics.StageAttrs{Stage: stage, Agent: agent, Pipeline: pipelineName,
		Outcome: outcome, ExitCode: run.Result.ExitCode}, elapsed)
}

func (d *Daemon) recordStoppedInvocation(root, ctx context.Context, path, agent, stage string, index int, run agentRun) {
	if run.Execution == nil || root.Err() != nil || ctx.Err() == nil || ordinaryReservationRetained(ctx) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := ticket.ParseFile(path)
	if err != nil || t.InvocationID != run.Execution.Invocation.ID || d.claimedElsewhere(t) || t.HasOwnershipMetadata() {
		return
	}
	run.Outcome, run.OutcomeReason = "failed", "execution cancelled by operator"
	appendRunHistory(t, run, agent, stage, index)
	_ = t.SetField("invocation_id", "")
	if err := d.writeTicketLocked(t, path, notify.OriginDaemon); err != nil {
		return
	}
	retireExecution(d.config(), run.Execution)
	d.setTicketState(t.ID, t, path)
	d.recordValidatedRun(ctx, stage, agent, t.Pipeline, run)
}

func evaluateAgentExit(t *ticket.Ticket, p *handleExitParams, hookErr error) (pipeline.Action, error) {
	if hookErr != nil && p.runData.Execution != nil && p.runData.Outcome == "complete" {
		p.runData.Outcome, p.runData.OutcomeReason, p.runData.ForcePause = "failed", hookErr.Error(), true
	}
	ev := outcomeEvent(p.runData)
	ev.ExitCode, ev.Timestamp = p.result.ExitCode, p.result.ExitedAt
	return pipeline.Evaluate(t, p.pipelineCfg, ev)
}

func setAgentExitError(t *ticket.Ticket, p handleExitParams, action pipeline.Action) {
	switch {
	case p.runData.pauses():
		_ = t.SetField("last_error", p.runData.OutcomeReason)
	case action.Kind == pipeline.ActionPause, action.Kind == pipeline.ActionPark && p.result.ExitCode != 0:
		_ = t.SetField("last_error", fmt.Sprintf("agent exited with code %d (stage: %s)", p.result.ExitCode, p.stageName))
	default:
		_ = t.SetField("last_error", "")
	}
}

func applySimpleExit(t *ticket.Ticket, run agentRun, hookErr, completionErr error) {
	switch {
	case run.pauses():
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", run.OutcomeReason)
	case run.Result.ExitCode != 0:
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", fmt.Sprintf("agent exited with code %d", run.Result.ExitCode))
	case hookErr != nil:
		// A paused ticket still needs the summary of the work the agent finished.
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", hookErr.Error())
		_ = t.SetField("summary", runSummary(t.Summary, run.FinalMessage))
		appendSystemNote(t, hookErr.Error())
	case completionErr != nil:
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", completionErr.Error())
		appendSystemNote(t, completionErr.Error())
	case t.IsAgentEpic():
		_ = t.SetField("status", string(ticket.StatusHumanReview))
		_ = t.SetField("stage", "")
		_ = t.SetField("last_error", "")
	default:
		_ = t.SetField("status", string(ticket.StatusDone))
		_ = t.SetField("last_error", "")
		_ = t.SetField("summary", runSummary(t.Summary, run.FinalMessage))
		ended := run.Result.ExitedAt
		if ended.IsZero() {
			ended = time.Now()
		}
		_ = t.SetField("completed_at", ended.Format(time.RFC3339))
	}
}

func (run agentRun) pauses() bool {
	return run.Outcome == "blocked" || run.Outcome == "protocol_error" || run.ForcePause
}

func outcomeEvent(run agentRun) pipeline.Event {
	ev := pipeline.Event{Kind: pipeline.EventAgentExited, ExitCode: run.Result.ExitCode, Timestamp: run.Result.ExitedAt,
		RunOutcome: run.Outcome, OutcomeReason: run.OutcomeReason, ForcePause: run.ForcePause}
	if run.Execution != nil {
		ev.InvocationID = run.Execution.Invocation.ID
	}
	return ev
}
