package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

type workflowProbe struct {
	waitID, pollID string
	branchKey      string
	cancel         context.CancelFunc
	retained       bool
}
type workflowProbeJob struct {
	snapshot *ticket.Ticket
	path     string
}
type workflowProbeJournal struct {
	Snapshot    *ticket.Ticket       `json:"-"`
	TicketID    string               `json:"ticket_id"`
	ExecutionID string               `json:"execution_id"`
	WaitID      string               `json:"wait_id"`
	PollID      string               `json:"poll_id"`
	Revision    uint64               `json:"revision"`
	Path        string               `json:"path"`
	Repository  string               `json:"repository"`
	Branch      string               `json:"branch"`
	RepoPath    string               `json:"repo_path"`
	Fingerprint string               `json:"fingerprint"`
	Phase       string               `json:"phase"`
	Active      *ticket.GraphState   `json:"active"`
	After       *ticket.GraphState   `json:"after,omitempty"`
	Result      *workflowProbeResult `json:"result,omitempty"`
	Status      ticket.Status        `json:"status,omitempty"`
	LastError   string               `json:"last_error,omitempty"`
}

type workflowProbeResult struct {
	ExitCode    int       `json:"exit_code"`
	Stopped     bool      `json:"stopped"`
	Failed      bool      `json:"failed"`
	Interrupted bool      `json:"interrupted"`
	CompletedAt time.Time `json:"completed_at"`
	Output      string    `json:"output"`
	Truncated   bool      `json:"truncated"`
	LogPath     string    `json:"log_path"`
}

func workflowProbePath(cfg *config.Config, id string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), id, "workflow-probe.json")
}
func saveWorkflowProbe(cfg *config.Config, j *workflowProbeJournal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	path := workflowProbePath(cfg, j.TicketID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}
func (d *Daemon) workflowProbeActiveLocked(id string) bool { _, ok := d.workflowProbes[id]; return ok }
func (d *Daemon) cancelWorkflowProbeLocked(id string) {
	if p := d.workflowProbes[id]; p != nil && p.cancel != nil {
		p.cancel()
	}
}
func (d *Daemon) queueWorkflowProbe(t *ticket.Ticket, path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.workflowProbes == nil {
		d.workflowProbes = make(map[string]*workflowProbe)
	}
	if d.workflowProbeActiveLocked(t.ID) {
		return
	}
	for _, job := range d.workflowProbeJobs {
		if job.snapshot.ID == t.ID {
			return
		}
	}
	d.workflowProbeJobs = append(d.workflowProbeJobs, &workflowProbeJob{snapshot: t.Clone(), path: path})
	d.queueCond.Broadcast()
}
func (d *Daemon) runWorkflowProbe(ctx context.Context, job *workflowProbeJob) bool {
	cfg := d.config()
	snapshot := job.snapshot
	w := workflowWait(snapshot)
	if w == nil {
		return true
	}
	pollID := newSessionID()
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.mu.Lock()
	if d.workflowProbes == nil {
		d.workflowProbes = make(map[string]*workflowProbe)
	}
	if d.workflowProbeActiveLocked(snapshot.ID) {
		d.mu.Unlock()
		return true
	}
	p := &workflowProbe{waitID: w.ID, pollID: pollID, cancel: cancel, branchKey: w.Repository + "\x00" + w.Branch}
	d.workflowProbes[snapshot.ID] = p
	d.mu.Unlock()
	stopped := true
	defer func() {
		d.mu.Lock()
		if stopped && d.workflowProbes[snapshot.ID] == p {
			delete(d.workflowProbes, snapshot.ID)
		}
		d.mu.Unlock()
		d.signalWorkflowWait()
	}()
	journal := &workflowProbeJournal{TicketID: snapshot.ID, ExecutionID: snapshot.GraphState.ExecutionID, WaitID: w.ID, PollID: pollID,
		Path: job.path, Repository: w.Repository, Branch: w.Branch, RepoPath: snapshot.Path, Fingerprint: snapshot.GraphState.DefinitionFingerprint, Phase: "launched"}
	launched, err := d.patchWorkflowWait(snapshot, job.path, func(t *ticket.Ticket) error {
		current := t.GraphState.Wait
		if current.Disarmed || current.Phase != "pending" || d.waitNow().Before(current.NextPoll) {
			return errExecutionChanged
		}
		if err := validateWaitWorkspace(t, current); err != nil {
			return err
		}
		current.Phase, current.PollID = "probing", pollID
		journal.Revision = t.GraphState.Revision
		journal.Active = t.GraphState.Clone()
		return saveWorkflowProbe(cfg, journal)
	})
	if err != nil {
		return true
	}
	snapshot, w = launched, launched.GraphState.Wait
	timeout := workflowProbeTimeout(cfg, snapshot)
	logPath := filepath.Join(expandTilde(cfg.LogsDir), snapshot.ID, "waits", w.ID, pollID+".log")
	result, runErr := d.runner(probeCtx, RunnerParams{Binary: "/bin/sh", Args: []string{"-c", w.Command}, Dir: w.Worktree, Timeout: timeout,
		TicketID: snapshot.ID, AttemptID: pollID, SessionName: d.tmuxSession, RequireTermination: true, LogFile: logPath,
		Env: agentEnv(cfg, config.Agent{}, d.configPath, "", snapshot.Stage, snapshot.ID)})
	uncertain := errors.Is(runErr, process.ErrTerminationUncertain) || result.Termination == process.TerminationUnknown
	output, truncated := workflowProbeOutput(logPath)
	if output == "" && runErr != nil {
		output = runErr.Error()
	}
	journal.Result = &workflowProbeResult{ExitCode: result.ExitCode, Stopped: !uncertain, Failed: runErr != nil || result.Interruption != nil || result.Termination == process.TerminationNotStarted, Interrupted: ctx.Err() != nil, CompletedAt: d.waitNow(), Output: output, Truncated: truncated, LogPath: logPath}
	journal.Phase = "stopped"
	if uncertain {
		journal.Phase = "recovery_required"
	}
	if saveErr := saveWorkflowProbe(cfg, journal); saveErr != nil {
		d.pauseWorkflowProbeRecovery(snapshot, job.path, "cannot persist probe termination; inspect processes before workflow-recover")
		stopped = false
		d.mu.Lock()
		p.retained = true
		d.mu.Unlock()
		return false
	}
	snapshot = freshWorkflowProbeTicket(snapshot, job.path)
	err = d.commitWorkflowProbeResult(snapshot, job.path, journal)
	if uncertain || (err != nil && !errors.Is(err, errExecutionChanged)) {
		if err != nil {
			d.pauseWorkflowProbeRecovery(snapshot, job.path, "workflow probe finalization requires process inspection and workflow-recover")
		}
		stopped = false
		d.mu.Lock()
		p.retained = true
		d.mu.Unlock()
		return false
	}
	if err == nil {
		_ = os.Remove(workflowProbePath(cfg, snapshot.ID))
	}
	return true
}
func (d *Daemon) commitWorkflowProbeResult(snapshot *ticket.Ticket, path string, journal *workflowProbeJournal) error {
	for {
		journal.Active, journal.Revision = snapshot.GraphState.Clone(), snapshot.GraphState.Revision
		_, err := d.patchWorkflowWait(snapshot, path, func(t *ticket.Ticket) error {
			current := t.GraphState.Wait
			if current.PollID != journal.PollID || current.Phase != "probing" {
				return errExecutionChanged
			}
			if err := d.applyWorkflowProbeResult(t, journal.Result); err != nil {
				return err
			}
			journal.Phase, journal.After, journal.Status, journal.LastError = "result", t.GraphState.Clone(), t.Status, t.LastError
			if !journal.Result.Stopped {
				journal.Phase = "recovery_required"
			}
			return saveWorkflowProbe(d.config(), journal)
		})
		if !errors.Is(err, errExecutionChanged) {
			return err
		}
		// Pause can change the revision after the final read without changing the poll that owns this result.
		next := freshWorkflowProbeTicket(snapshot, path)
		if next.GraphState.Revision <= snapshot.GraphState.Revision || next.GraphState.Wait.PollID != journal.PollID || next.GraphState.Wait.Phase != "probing" {
			return err
		}
		snapshot = next
	}
}

func workflowProbeTimeout(cfg *config.Config, t *ticket.Ticket) time.Duration {
	if graph := cfg.Pipelines[t.Pipeline].Graph; graph != nil {
		if node, ok := graph.Node(t.Stage); ok && node.Wake != nil && node.Wake.CheckTimeout.Duration > 0 {
			return node.Wake.CheckTimeout.Duration
		}
	}
	return time.Minute
}

func freshWorkflowProbeTicket(snapshot *ticket.Ticket, path string) *ticket.Ticket {
	current, err := ticket.ParseFile(path)
	if err != nil || current.GraphState == nil || current.GraphState.Wait == nil {
		return snapshot
	}
	if current.GraphState.ExecutionID != snapshot.GraphState.ExecutionID || current.GraphState.Wait.ID != snapshot.GraphState.Wait.ID || current.Path != snapshot.Path || current.Branch != snapshot.Branch || current.Pipeline != snapshot.Pipeline || current.GraphState.DefinitionFingerprint != snapshot.GraphState.DefinitionFingerprint {
		return snapshot
	}
	return current
}

func (d *Daemon) applyWorkflowProbeResult(t *ticket.Ticket, r *workflowProbeResult) error {
	s, w := t.GraphState, t.GraphState.Wait
	w.Output, w.OutputTruncated, w.LogPath = r.Output, r.Truncated, r.LogPath
	w.PollCount++
	switch {
	case !r.Stopped:
		w.Phase, w.Disarmed = "recovery_required", true
		if err := t.SetField("graph_state", s); err != nil {
			return err
		}
		return t.SetField("last_error", "workflow probe termination is unconfirmed; inspect processes before workflow-recover")
	case w.Disarmed || r.Interrupted:
		w.Phase, w.PollID = "pending", ""
		w.NextPoll = r.CompletedAt.Add(w.Poll)
	case !w.Deadline.IsZero() && !r.CompletedAt.Before(w.Deadline):
		return d.applyWaitReason(t, "deadline")
	case r.Failed:
		return d.applyWaitReason(t, "error")
	case r.ExitCode == 1:
		w.Phase, w.PollID = "pending", ""
		w.NextPoll = r.CompletedAt.Add(w.Poll)
	case r.ExitCode == 0:
		return d.applyWaitReason(t, "ready")
	default:
		return d.applyWaitReason(t, "error")
	}
	return t.SetField("graph_state", s)
}
func (d *Daemon) pauseWorkflowProbeRecovery(snapshot *ticket.Ticket, path, reason string) {
	_ = d.withTicketMutationOrigin(notify.OriginDaemon, func(repo app.MutationRepository) error {
		st, err := repo.Get(snapshot.ID)
		if err != nil {
			return err
		}
		t := st.Ticket
		if t.GraphState == nil || t.GraphState.Wait == nil || snapshot.GraphState == nil || snapshot.GraphState.Wait == nil || st.FilePath != path || t.GraphState.ExecutionID != snapshot.GraphState.ExecutionID || t.GraphState.Wait.ID != snapshot.GraphState.Wait.ID {
			return errExecutionChanged
		}
		s := t.GraphState.Clone()
		s.Revision++
		s.Wait.Phase, s.Wait.Disarmed = "recovery_required", true
		_ = t.SetField("graph_state", s)
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", reason)
		return repo.Save(st)
	})
}

func workflowProbeOutput(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16*1024+1))
	if err != nil {
		return "", false
	}
	truncated := len(data) > 16*1024
	if truncated {
		data = data[:16*1024]
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
	}
	output := strings.ToValidUTF8(string(data), "�")
	if len(output) > 16*1024 {
		output = output[:16*1024]
		for !utf8.ValidString(output) {
			output = output[:len(output)-1]
		}
		truncated = true
	}
	return output, truncated
}
func (d *Daemon) recoverWorkflowProbes() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, ts := range d.tickets {
		data, err := os.ReadFile(workflowProbePath(d.config(), id))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		t := ts.ticket
		w := workflowWait(t)
		var j workflowProbeJournal
		decoded := err == nil && json.Unmarshal(data, &j) == nil && j.TicketID == id && j.Active != nil && j.Active.Wait != nil
		valid := decoded && t.GraphState != nil && j.ExecutionID == t.GraphState.ExecutionID && j.Path == ts.filePath && j.RepoPath == t.Path && j.Branch == t.Branch && j.Fingerprint == t.GraphState.DefinitionFingerprint
		if w == nil {
			if decoded && j.Result != nil && j.Result.Stopped {
				_ = os.Remove(workflowProbePath(d.config(), id))
				continue
			}
			if decoded {
				d.retainRecoveredProbeLocked(id, j.Active.Wait, j.PollID)
			}
			continue
		}
		valid = valid && j.WaitID == w.ID && j.Repository == w.Repository
		if valid && d.replayWorkflowProbeLocked(t, ts.filePath, &j) {
			continue
		}
		state := t.GraphState.Clone()
		if valid && j.Active != nil && state.Revision+1 == j.Revision {
			state = j.Active.Clone()
		}
		state.Wait.Phase, state.Wait.Disarmed = "recovery_required", true
		state.Revision++
		_ = t.SetField("graph_state", state)
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", "workflow probe journal requires process inspection and workflow-recover")
		_ = d.writeTicketLocked(t, ts.filePath, notify.OriginDaemon)
		d.retainRecoveredProbeLocked(id, w, j.PollID)
	}
}
func (d *Daemon) replayWorkflowProbeLocked(t *ticket.Ticket, path string, j *workflowProbeJournal) bool {
	w := t.GraphState.Wait
	if j.Result != nil && j.Result.Stopped && j.After == nil && w.PollID == j.PollID && w.Phase == "probing" && d.validateWaitProgress(t, w) == nil {
		next := t.Clone()
		state := next.GraphState.Clone()
		state.Revision++
		_ = next.SetField("graph_state", state)
		if d.applyWorkflowProbeResult(next, j.Result) == nil {
			j.Active, j.Revision = t.GraphState.Clone(), t.GraphState.Revision
			j.After, j.Status, j.LastError, j.Phase = next.GraphState.Clone(), next.Status, next.LastError, "result"
			if saveWorkflowProbe(d.config(), j) == nil && d.writeTicketLocked(next, path, notify.OriginDaemon) == nil {
				*t = *next
				_ = os.Remove(workflowProbePath(d.config(), t.ID))
				return true
			}
		}
	}
	if j.After == nil {
		return false
	}
	if reflect.DeepEqual(t.GraphState, j.After) && t.Status == j.Status && t.LastError == j.LastError {
		if j.Phase != "recovery_required" {
			_ = os.Remove(workflowProbePath(d.config(), t.ID))
			return true
		}
	} else if reflect.DeepEqual(t.GraphState, j.Active) {
		_ = t.SetField("graph_state", j.After.Clone())
		_ = t.SetField("status", string(j.Status))
		_ = t.SetField("last_error", j.LastError)
		if d.writeTicketLocked(t, path, notify.OriginDaemon) == nil && j.Phase != "recovery_required" {
			_ = os.Remove(workflowProbePath(d.config(), t.ID))
			return true
		}
	}
	return false
}

func (d *Daemon) retainRecoveredProbeLocked(id string, w *ticket.WorkflowWait, pollID string) {
	if d.workflowProbes == nil {
		d.workflowProbes = make(map[string]*workflowProbe)
	}
	if !d.workflowProbeActiveLocked(id) {
		d.workflowProbes[id] = &workflowProbe{waitID: w.ID, pollID: pollID, retained: true, branchKey: w.Repository + "\x00" + w.Branch}
		d.sem.used++
	}
}
func (d *Daemon) recoverWorkflowProbeLocked(t *ticket.Ticket, path, executionID string, revision uint64) (bool, error) {
	w := workflowWait(t)
	p := d.workflowProbes[t.ID]
	cancelled := w == nil && p != nil && p.retained
	if !cancelled && (w == nil || w.Phase != "recovery_required") {
		return false, nil
	}
	if t.GraphState.ExecutionID != executionID || t.GraphState.Revision != revision {
		return true, fmt.Errorf("stale probe recovery identity")
	}
	state := t.GraphState.Clone()
	state.Revision++
	if !cancelled {
		state.Wait.Phase, state.Wait.Disarmed, state.Wait.PollID = "pending", true, ""
	}
	_ = t.SetField("graph_state", state)
	reason := "probe recovery acknowledged; Retry to re-arm the same wait"
	if cancelled {
		reason = "Cancelled probe recovery acknowledged; the workflow remains cancelled."
	}
	_ = t.SetField("last_error", reason)
	if err := d.writeTicketLocked(t, path, notify.OriginRequest); err != nil {
		return true, err
	}
	if err := os.Remove(workflowProbePath(d.config(), t.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	if p := d.workflowProbes[t.ID]; p != nil && p.retained {
		d.sem.used--
		delete(d.workflowProbes, t.ID)
		d.queueCond.Broadcast()
	}
	d.signalWorkflowWait()
	return true, nil
}
