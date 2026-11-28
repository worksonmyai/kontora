package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/worktree"
)

type workflowConditionJournal struct {
	TicketID      string                         `json:"ticket_id"`
	ExecutionID   string                         `json:"execution_id"`
	NodeID        string                         `json:"node_id"`
	EvaluationID  string                         `json:"evaluation_id"`
	Revision      uint64                         `json:"revision"`
	Fingerprint   string                         `json:"fingerprint"`
	Path          string                         `json:"path"`
	RepoPath      string                         `json:"repo_path"`
	Branch        string                         `json:"branch"`
	Worktree      string                         `json:"worktree"`
	Instance      string                         `json:"instance"`
	LogPath       string                         `json:"log_path"`
	Phase         string                         `json:"phase"`
	Window        *tmux.WindowIdentity           `json:"window,omitempty"`
	Result        *pipeline.GraphConditionResult `json:"result,omitempty"`
	LogIncomplete bool                           `json:"log_incomplete,omitempty"`
}

func workflowConditionPath(cfg *config.Config, id string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), id, "workflow-condition.json")
}

func workflowConditionJournalExists(cfg *config.Config, id string) bool {
	_, err := os.Lstat(workflowConditionPath(cfg, id))
	return !errors.Is(err, os.ErrNotExist)
}

func saveWorkflowCondition(cfg *config.Config, j *workflowConditionJournal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	path := workflowConditionPath(cfg, j.TicketID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

func loadWorkflowCondition(cfg *config.Config, id string) (*workflowConditionJournal, error) {
	data, err := os.ReadFile(workflowConditionPath(cfg, id))
	if err != nil {
		return nil, err
	}
	var j workflowConditionJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	if j.TicketID != id || j.ExecutionID == "" || j.NodeID == "" || j.EvaluationID == "" ||
		(j.Phase != "launched" && j.Phase != "stopped" && j.Phase != "recovery_required" && j.Phase != "acknowledged") ||
		(j.Phase == "stopped" && (j.Result == nil || j.Result.NodeID != j.NodeID || j.Result.EvaluationID != j.EvaluationID || j.Result.TerminationUncertain)) {
		return nil, errors.New("invalid workflow condition journal")
	}
	return &j, nil
}

func removeWorkflowCondition(cfg *config.Config, id string) error {
	if err := os.Remove(workflowConditionPath(cfg, id)); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (d *Daemon) runGraphCondition(root, task context.Context, cfg *config.Config, log *slog.Logger, t *ticket.Ticket, path string, node config.WorkflowNode) {
	repoPath := expandTilde(t.Path)
	wtPath, err := worktree.FindWorktreeForBranch(repoPath, t.Branch)
	if err != nil {
		d.pauseExecution(task, t, path, fmt.Sprintf("condition %s: could not look up the ticket worktree: %v. Fix the lookup error before retrying.", node.ID, err))
		return
	}
	if wtPath == "" {
		d.pauseExecution(task, t, path, fmt.Sprintf("condition %s requires the ticket worktree; use workflow-restart", node.ID))
		return
	}
	id := fmt.Sprintf("%020d-%s", time.Now().UnixNano(), newSessionID())
	logPath, err := filepath.Abs(filepath.Join(expandTilde(cfg.LogsDir), t.ID, "conditions", node.ID+"."+id+".log"))
	if err != nil {
		d.pauseExecution(task, t, path, fmt.Sprintf("condition %s: resolve log path: %v", node.ID, err))
		return
	}
	j := &workflowConditionJournal{TicketID: t.ID, ExecutionID: t.GraphState.ExecutionID, NodeID: node.ID,
		EvaluationID: id, Revision: t.GraphState.Revision, Fingerprint: t.GraphState.DefinitionFingerprint,
		Path: path, RepoPath: t.Path, Branch: t.Branch, Worktree: wtPath, Instance: d.instanceName, LogPath: logPath, Phase: "launched"}
	env := d.workflowConditionEnv(cfg, t, node.ID, repoPath, wtPath)
	if err := d.recordConditionLaunch(task, cfg, t, j); err != nil {
		if !errors.Is(err, errExecutionChanged) && !errors.Is(err, app.ErrNotFound) {
			d.pauseExecution(task, t, path, fmt.Sprintf("condition %s: persist launch: %v", node.ID, err))
		}
		return
	}
	result, runErr := d.runner(task, RunnerParams{Binary: "/bin/sh", Args: []string{"-c", node.Command}, Dir: wtPath,
		Timeout: node.Timeout.Duration, TicketID: t.ID, SessionName: d.tmuxSession, RequireTermination: true,
		LogFile: logPath, WaitForLog: true, Env: env, OnWindowCreated: func(window tmux.WindowIdentity) error {
			j.Window = &window
			return saveWorkflowCondition(cfg, j)
		}})

	if result.LogCaptureError != nil {
		log.Warn("workflow condition log capture incomplete", "node", node.ID, "log", logPath, "err", result.LogCaptureError)
	}
	r := conditionRunnerResult(root, task, j, result, runErr)
	if r == nil {
		return
	}
	_, logErr := os.Stat(logPath)
	j.LogIncomplete = result.LogCaptureError != nil || logErr != nil
	j.Result, j.Phase = r, "stopped"
	if r.TerminationUncertain {
		j.Phase = "recovery_required"
	}
	saveErr := saveWorkflowCondition(cfg, j)
	if r.TerminationUncertain {
		d.retainFinalizationReservation(task, t)
		d.pauseExecution(context.WithoutCancel(task), t, path, conditionRecoveryReason(j))
	}
	if saveErr != nil {
		if !r.TerminationUncertain {
			d.retainFinalizationReservation(task, t)
		}
		log.Error("persist condition result", "node", node.ID, "err", saveErr)
	}
	if r.TerminationUncertain || saveErr != nil {
		return
	}
	if r.InfrastructureError {
		log.Error("run workflow condition", "node", node.ID, "termination", result.Termination, "err", runErr)
	}
	graph := cfg.Pipelines[t.Pipeline].Graph
	saved, err := d.patchExecution(context.WithoutCancel(task), t, path, func(current *ticket.Ticket) error {
		if task.Err() != nil {
			return errExecutionChanged
		}
		return d.commitWorkflowCondition(current, graph, j)
	})
	if err != nil && !errors.Is(err, errExecutionChanged) && !errors.Is(err, app.ErrNotFound) {
		d.retainConditionFinalization(task, t)
		log.Error("commit condition result", "node", node.ID, "err", err)
		return
	}
	if err := removeWorkflowCondition(cfg, t.ID); err != nil {
		d.retainConditionFinalization(task, t)
		log.Error("remove condition journal", "node", node.ID, "err", err)
		return
	}
	if saved != nil {
		p := spawnAgentParams{cfg: cfg, log: log, ticketID: t.ID, filePath: path, repoPath: repoPath, wtPath: wtPath}
		d.finishWorkflowExit(root, saved, p, node, agentRun{})
		d.signalWorkflowWait()
	}
}

func conditionRunnerResult(root, task context.Context, j *workflowConditionJournal, result process.Result, runErr error) *pipeline.GraphConditionResult {
	r := &pipeline.GraphConditionResult{NodeID: j.NodeID, EvaluationID: j.EvaluationID, ExitCode: result.ExitCode}
	preserveWorkflowInterruption(&result, runErr)
	switch {
	case errors.Is(runErr, process.ErrTerminationUncertain) || result.Termination == process.TerminationUnknown:
		r.TerminationUncertain = true
	case root.Err() != nil:
		return nil
	case task.Err() != nil || errors.Is(result.Interruption, context.Canceled):
		r.Interrupted = true
	case errors.Is(result.Interruption, context.DeadlineExceeded):
		r.TimedOut = true
	case runErr != nil || result.Termination == process.TerminationNotStarted:
		r.InfrastructureError = true
	}
	return r
}

func (d *Daemon) recordConditionLaunch(ctx context.Context, cfg *config.Config, t *ticket.Ticket, j *workflowConditionJournal) error {
	return d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := repo.Get(t.ID)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || st.FilePath != j.Path || !d.executionMatchesLocked(ctx, t, st.Ticket) {
			return errExecutionChanged
		}
		return saveWorkflowCondition(cfg, j)
	})
}

func (d *Daemon) commitWorkflowCondition(t *ticket.Ticket, graph *config.WorkflowGraph, j *workflowConditionJournal) error {
	after := pipeline.ResolveGraphCondition(graph, t.Stage, t.GraphState, *j.Result)
	if after.Rejected {
		after.Reason = fmt.Sprintf("%s (%s)", after.Reason, conditionLogDescription(j))
		if j.Result.InfrastructureError {
			after.Reason += ". See the daemon log for the runner error."
		}
	}
	return d.finishControlAdvance(t, after)
}

func (d *Daemon) workflowConditionEnv(cfg *config.Config, t *ticket.Ticket, nodeID, repoPath, wtPath string) map[string]string {
	env := agentEnv(cfg, config.Agent{}, d.configPath, "", nodeID, t.ID)
	// After merging global environment, blank inherited or configured invocation bindings.
	for key := range (session.Invocation{}).Env() {
		env[key] = ""
	}
	base := ticketBase(t)
	if base == "" {
		base, _ = worktree.DetectDefaultBranch(repoPath)
	}
	maps.Copy(env, map[string]string{
		config.AgentEnvVar: "", config.StageEnvVar: nodeID, config.TicketEnvVar: t.ID,
		"KONTORA_WORKTREE": wtPath, "KONTORA_REPO_PATH": repoPath, "KONTORA_BRANCH": t.Branch, "KONTORA_BASE_BRANCH": base,
		session.WorkflowExecutionIDEnvVar: t.GraphState.ExecutionID,
		"KONTORA_LAST_NODE":               "", "KONTORA_LAST_OUTCOME": "", "KONTORA_LAST_EXIT_CODE": "", "KONTORA_LAST_VERDICT": "", "KONTORA_LAST_LOG": "",
	})
	if r := t.GraphState.LastResult; r != nil {
		env["KONTORA_LAST_NODE"], env["KONTORA_LAST_OUTCOME"], env["KONTORA_LAST_VERDICT"] = r.NodeID, r.Outcome, r.Verdict
		if r.ExitCode != nil {
			env["KONTORA_LAST_EXIT_CODE"] = strconv.Itoa(*r.ExitCode)
		}
		env["KONTORA_LAST_LOG"], _ = filepath.Abs(session.GraphLogPath(expandTilde(cfg.LogsDir), t.ID, r.NodeID, r.VisitID, r.InvocationID))
	}
	return env
}

func conditionLogDescription(j *workflowConditionJournal) string {
	if j.LogIncomplete {
		return "expected log path: " + j.LogPath
	}
	return "log: " + j.LogPath
}

func conditionRecoveryReason(j *workflowConditionJournal) string {
	if j.Result != nil && !j.Result.TerminationUncertain {
		return fmt.Sprintf("condition %s stopped, but the workflow definition changed; use workflow-recover to acknowledge termination and discard the result (%s)", j.NodeID, conditionLogDescription(j))
	}
	return fmt.Sprintf("condition %s termination is unconfirmed; inspect processes before workflow-recover (%s)", j.NodeID, conditionLogDescription(j))
}

func (j *workflowConditionJournal) matches(t *ticket.Ticket, path string) bool {
	s := t.GraphState
	return s != nil && s.ExecutionID == j.ExecutionID && s.Revision == j.Revision && t.Stage == j.NodeID &&
		t.InvocationID == "" && sameJournalTicketPath(j.Path, path) && t.Path == j.RepoPath && t.Branch == j.Branch
}

func (d *Daemon) requireConditionRecoveryLocked(cfg *config.Config, t *ticket.Ticket, path string, j *workflowConditionJournal) {
	j.Phase = "recovery_required"
	if err := saveWorkflowCondition(cfg, j); err != nil {
		d.ticketLog(t.ID).Error("persist condition recovery", "err", err)
	}
	d.reserveRecoveredExecutionLocked(t)
	if j.matches(t, path) {
		d.pauseConditionRecoveryLocked(t, path, conditionRecoveryReason(j))
	}
}

func (d *Daemon) reconcileWorkflowConditionLocked(cfg *config.Config, t *ticket.Ticket, path string) bool {
	graph := cfg.Pipelines[t.Pipeline].Graph
	var commandCursor bool
	if graph != nil {
		node, _ := graph.Node(t.Stage)
		commandCursor = node.CommandCondition()
	}
	j, err := loadWorkflowCondition(cfg, t.ID)
	if errors.Is(err, os.ErrNotExist) && !workflowConditionJournalExists(cfg, t.ID) {
		if t.GraphState == nil || !t.IsRunning() || !commandCursor || t.InvocationID != "" {
			return false
		}
		d.resumeRecoveredConditionLocked(cfg, t, path)
		return true
	}
	if err != nil {
		d.pauseConditionRecoveryLocked(t, path, fmt.Sprintf("Cannot read condition journal %s: %v. The slot stays reserved. workflow-recover cannot acknowledge an unreadable journal.", workflowConditionPath(cfg, t.ID), err))
		return true
	}
	if j.Instance != d.instanceName {
		d.reserveRecoveredExecutionLocked(t)
		return true
	}
	matches := j.matches(t, path)
	switch j.Phase {
	case "acknowledged":
		if err := d.finishConditionAcknowledgementLocked(cfg, t, path, j); err != nil {
			d.ticketLog(t.ID).Error("finish condition acknowledgement", "err", err)
		}
		return true
	case "recovery_required":
		d.reserveRecoveredExecutionLocked(t)
		if matches {
			d.pauseConditionRecoveryLocked(t, path, conditionRecoveryReason(j))
		} else {
			d.ticketLog(t.ID).Warn("older condition journal still holds a reservation; inspect it before workflow-recover", "node", j.NodeID, "evaluation", j.EvaluationID)
		}
		return true
	case "launched":
		d.recoverLaunchedConditionLocked(cfg, t, path, j, graph, commandCursor)
		return true
	}
	s := t.GraphState
	committed := s != nil && s.ExecutionID == j.ExecutionID && (s.Revision > j.Revision ||
		slices.ContainsFunc(s.Transitions, func(e ticket.GraphTransition) bool { return e.EvaluationID == j.EvaluationID }))
	if matches && t.IsRunning() && !committed {
		if !commandCursor {
			d.requireConditionRecoveryLocked(cfg, t, path, j)
			return true
		}
		fingerprint, err := workflowFingerprint(cfg, t, graph)
		if err != nil || fingerprint != j.Fingerprint || fingerprint != s.DefinitionFingerprint {
			d.requireConditionRecoveryLocked(cfg, t, path, j)
			return true
		}
		next := t.Clone()
		if err := d.commitWorkflowCondition(next, graph, j); err != nil {
			d.ticketLog(t.ID).Error("recover condition result", "err", err)
			d.retryConditionFinalizationLocked(t)
			return true
		}
		if err := d.writeRecoveredConditionLocked(t, next, path); err != nil {
			d.retryConditionFinalizationLocked(t)
			return true
		}
		committed = true
	}
	if d.finishRecoveredConditionLocked(cfg, t) == nil && committed && t.Status == ticket.StatusTodo {
		d.enqueue(t)
	}
	return true
}

func (d *Daemon) recoverLaunchedConditionLocked(cfg *config.Config, t *ticket.Ticket, path string, j *workflowConditionJournal, graph *config.WorkflowGraph, commandCursor bool) {
	if j.Window != nil {
		present, err := d.windows.inspectIdentity(*j.Window)
		if err == nil && present {
			err = d.windows.killIdentity(*j.Window)
			if err == nil {
				present, err = d.windows.inspectIdentity(*j.Window)
			}
		}
		if err != nil || present {
			d.ticketLog(t.ID).Error("confirm workflow condition termination", "node", j.NodeID, "err", err)
			d.requireConditionRecoveryLocked(cfg, t, path, j)
			return
		}
	}
	// Tmux does not start the command until the journal has its physical window ID.
	// Disk edits can arrive while tmux confirms termination.
	current, err := ticket.ParseFile(path)
	if err != nil {
		d.retryConditionFinalizationLocked(t)
		return
	}
	*t = *current
	if d.claimedElsewhere(t) || t.HasOwnershipMetadata() {
		return
	}
	matches := j.matches(t, path)
	if matches && t.IsRunning() {
		if commandCursor {
			fingerprint, err := workflowFingerprint(cfg, t, graph)
			if err == nil && fingerprint == j.Fingerprint && fingerprint == t.GraphState.DefinitionFingerprint {
				d.resumeRecoveredConditionLocked(cfg, t, path)
				return
			}
		}
		next := t.Clone()
		_ = next.SetField("status", string(ticket.StatusPaused))
		_ = next.SetField("last_error", fmt.Sprintf("condition %s stopped after the workflow definition changed; restore the definition before retrying or use workflow-restart", j.NodeID))
		if err := d.writeRecoveredConditionLocked(t, next, path); err != nil {
			d.retryConditionFinalizationLocked(t)
			return
		}
	}
	if d.finishRecoveredConditionLocked(cfg, t) == nil && matches && t.Status == ticket.StatusTodo {
		d.enqueue(t)
	}
}

func (d *Daemon) resumeRecoveredConditionLocked(cfg *config.Config, t *ticket.Ticket, path string) {
	next := t.Clone()
	_ = next.SetField("status", string(ticket.StatusTodo))
	_ = next.SetField("last_error", "")
	if err := d.writeRecoveredConditionLocked(t, next, path); err != nil {
		d.retryConditionFinalizationLocked(t)
		return
	}
	if d.finishRecoveredConditionLocked(cfg, t) == nil {
		d.enqueue(t)
	}
}

func (d *Daemon) writeRecoveredConditionLocked(t, next *ticket.Ticket, path string) error {
	if err := d.writeTicketLocked(next, path, notify.OriginDaemon); err != nil {
		d.ticketLog(t.ID).Error("write condition recovery", "err", err)
		return err
	}
	*t = *next
	return nil
}

func (d *Daemon) finishRecoveredConditionLocked(cfg *config.Config, t *ticket.Ticket) error {
	if err := removeWorkflowCondition(cfg, t.ID); err != nil {
		d.ticketLog(t.ID).Error("remove condition journal", "err", err)
		d.retryConditionFinalizationLocked(t)
		return err
	}
	d.releaseRecoveredExecutionLocked(t.ID)
	d.signalWorkflowWait()
	return nil
}

func (d *Daemon) finishConditionAcknowledgementLocked(cfg *config.Config, t *ticket.Ticket, path string, j *workflowConditionJournal) error {
	if !d.claimedElsewhere(t) && j.matches(t, path) && (t.Status == ticket.StatusPaused || t.IsRunning()) {
		next := t.Clone()
		next.GraphState.Revision++
		if err := next.SetField("graph_state", next.GraphState); err != nil {
			return err
		}
		_ = next.SetField("status", string(ticket.StatusPaused))
		_ = next.SetField("last_error", "Condition recovery acknowledged. No work was started.")
		if err := d.writeRecoveredConditionLocked(t, next, path); err != nil {
			d.retryConditionFinalizationLocked(t)
			return err
		}
		d.setTicketState(t.ID, t, path)
		d.reconcileDependenciesLocked(t.ID)
	}
	return d.finishRecoveredConditionLocked(cfg, t)
}

func (d *Daemon) retainConditionFinalization(ctx context.Context, t *ticket.Ticket) {
	d.retainFinalizationReservation(ctx, t)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, retained := d.recoveredReservations[t.ID]; retained {
		d.retryConditionFinalizationLocked(t)
	}
}

func (d *Daemon) retryConditionFinalizationLocked(t *ticket.Ticket) {
	d.reserveRecoveredExecutionLocked(t)
	if reservation, ok := d.recoveredReservations[t.ID]; ok {
		reservation.conditionFinalization = true
		d.recoveredReservations[t.ID] = reservation
	}
}

func (d *Daemon) reconcileConditionFinalizations() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, reservation := range d.recoveredReservations {
		if !reservation.conditionFinalization {
			continue
		}
		if ts := d.tickets[id]; ts != nil {
			current, err := ticket.ParseFile(ts.filePath)
			if err != nil {
				continue
			}
			reservation.conditionFinalization = false
			d.recoveredReservations[id] = reservation
			d.reconcileRetainedExecutionLocked(current, ts.filePath)
			if executionOf(current) != executionOf(ts.ticket) {
				d.broadcastTicketUpdate(id)
			}
		}
	}
}

func (d *Daemon) pauseConditionRecoveryLocked(t *ticket.Ticket, path, reason string) {
	d.reserveRecoveredExecutionLocked(t)
	if t.IsRunning() {
		d.pauseRecoveryLocked(t, path, reason, true)
	} else if t.Status == ticket.StatusTodo || t.Status == ticket.StatusPaused && t.LastError != reason {
		next := t.Clone()
		_ = next.SetField("status", string(ticket.StatusPaused))
		_ = next.SetField("last_error", reason)
		_ = d.writeRecoveredConditionLocked(t, next, path)
	}
}
