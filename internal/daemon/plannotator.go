package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// stageInPipeline reports whether a stage name appears in a pipeline definition.
func stageInPipeline(p config.Pipeline, stage string) bool {
	for _, step := range p {
		if step.Stage == stage {
			return true
		}
	}
	return false
}

// defaultPlannotatorLookup resolves the plannotator binary to an absolute
// path via process.LookupBinary — see there for fallback semantics.
func defaultPlannotatorLookup(binary string) (string, error) {
	return process.LookupBinary(binary)
}

// defaultPlannotatorSpawner runs `plannotator review` as a subprocess and
// returns stdout. The process does not share stdout with the daemon so that
// the annotation blob remains intact.
func defaultPlannotatorSpawner(ctx context.Context, params PlannotatorParams) (string, error) {
	var stdout bytes.Buffer
	env := make([]string, 0, len(params.Env))
	for k, v := range params.Env {
		env = append(env, k+"="+v)
	}
	if _, err := process.Run(ctx, process.RunParams{
		Binary:  params.Binary,
		Args:    []string{"review"},
		Dir:     params.Dir,
		Timeout: params.Timeout,
		Stdout:  &stdout,
		Env:     env,
	}); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// StartPlannotatorReview spawns a plannotator subprocess against the ticket's
// worktree. When non-empty feedback is returned, the daemon parks the ticket
// at stage=rework, status=todo and lets the scheduler pick it up.
//
// Synchronous errors:
//   - web.ErrTicketNotFound: unknown ticket
//   - web.ErrPlannotatorInFlight: a previous invocation is still running
//   - web.ErrPlannotatorWorkdir: the worktree does not exist
//   - web.ErrPlannotatorBinary: the plannotator binary is not on PATH
//
// Anything else goes over SSE as a plannotator_finished(outcome=error).
func (d *Daemon) StartPlannotatorReview(id string) error {
	log := d.ticketLog(id)

	if !ticket.IsSafeID(id) {
		return fmt.Errorf("%w: unsafe ticket id", web.ErrTicketNotFound)
	}

	d.mu.Lock()
	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}
	t := ts.ticket
	d.mu.Unlock()

	if t.Status != ticket.StatusHumanReview {
		return web.ErrInvalidState
	}

	_, repoPath, err := d.resolvePath(t)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	branch := d.ticketBranch(t)
	wtPath, err := worktree.FindWorktreeForBranch(repoPath, branch)
	if err != nil {
		log.Error("plannotator: find worktree failed", "branch", branch, "err", err)
		return fmt.Errorf("find worktree: %w", err)
	}
	if wtPath == "" {
		return web.ErrPlannotatorWorkdir
	}
	log.Info("located worktree for branch", "path", wtPath, "branch", branch)

	binaryPath, err := d.plannotatorLookup(d.cfg.Plannotator.Binary)
	if err != nil {
		return fmt.Errorf("%w: %s", web.ErrPlannotatorBinary, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	if _, running := d.plannotator[id]; running {
		d.mu.Unlock()
		cancel()
		return web.ErrPlannotatorInFlight
	}
	d.plannotator[id] = cancel
	d.mu.Unlock()

	d.broker.Broadcast(web.TicketEvent{
		Type:   "plannotator_started",
		Ticket: web.TicketInfo{ID: id},
	})

	go d.runPlannotator(ctx, log, id, binaryPath, repoPath, wtPath)

	return nil
}

func (d *Daemon) runPlannotator(ctx context.Context, log *slog.Logger, id, binaryPath, repoPath, wtPath string) {
	defer func() {
		d.mu.Lock()
		if cancel, ok := d.plannotator[id]; ok {
			cancel()
			delete(d.plannotator, id)
		}
		d.mu.Unlock()
	}()

	reviewWt, cleanup, err := setupPlannotatorWorktree(log, repoPath, wtPath)
	if err != nil {
		log.Error("plannotator: setup review worktree failed", "err", err)
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeError,
			Message: "setup review worktree: " + err.Error(),
		})
		return
	}
	defer cleanup()

	params := PlannotatorParams{
		Binary:  binaryPath,
		Dir:     reviewWt,
		Env:     map[string]string{"PLANNOTATOR_REMOTE": "0"},
		Timeout: d.cfg.Plannotator.Timeout.Duration,
	}

	stdout, err := d.plannotatorSpawner(ctx, params)
	if err != nil {
		log.Error("plannotator: spawn failed", "err", err)
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeError,
			Message: err.Error(),
		})
		return
	}

	if strings.TrimSpace(stdout) == "" {
		log.Info("plannotator: approved (empty feedback)")
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeApproved,
		})
		return
	}

	reviewsDir := config.ExpandTilde(d.cfg.Plannotator.ReviewsDir)
	if mkErr := os.MkdirAll(reviewsDir, 0o755); mkErr != nil {
		log.Error("plannotator: mkdir reviews_dir failed", "err", mkErr)
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeError,
			Message: "mkdir reviews_dir: " + mkErr.Error(),
		})
		return
	}
	reviewPath := filepath.Join(reviewsDir, id+".md")
	if wErr := os.WriteFile(reviewPath, []byte(stdout), 0o644); wErr != nil {
		log.Error("plannotator: write review file failed", "err", wErr, "path", reviewPath)
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeError,
			Message: "write review: " + wErr.Error(),
		})
		return
	}

	if tErr := d.transitionToRework(id); tErr != nil {
		log.Error("plannotator: transition to rework failed", "err", tErr)
		// Best-effort cleanup: remove the orphaned review file.
		_ = os.Remove(reviewPath)
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeError,
			Message: "transition: " + tErr.Error(),
		})
		return
	}

	log.Info("plannotator: feedback captured, ticket moved to rework", "bytes", len(stdout))
	d.broker.Broadcast(web.TicketEvent{
		Type:    "plannotator_finished",
		Ticket:  web.TicketInfo{ID: id},
		Outcome: web.PlannotatorOutcomeRework,
	})
}

// transitionToRework sets the ticket's stage=rework, status=todo and enqueues
// it via the existing scheduler path. Keeps lock discipline similar to
// SetStage/Retry.
func (d *Daemon) transitionToRework(id string) error {
	d.mu.Lock()
	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}
	filePath := ts.filePath
	d.mu.Unlock()

	// Re-read from disk to avoid racing with other mutators.
	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return fmt.Errorf("re-read ticket: %w", err)
	}
	if err := t2.SetField("stage", config.ReworkStageName); err != nil {
		return fmt.Errorf("set stage: %w", err)
	}
	if err := t2.SetField("status", string(ticket.StatusTodo)); err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	if err := t2.SetField("attempt", 0); err != nil {
		return fmt.Errorf("reset attempt: %w", err)
	}
	if err := t2.SetField("last_error", ""); err != nil {
		return fmt.Errorf("clear last_error: %w", err)
	}
	if err := d.writeTicket(t2, filePath); err != nil {
		return fmt.Errorf("write ticket: %w", err)
	}

	d.mu.Lock()
	d.tickets[id] = &ticketState{ticket: t2, filePath: filePath}
	d.enqueue(t2)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// runReworkStage executes the built-in rework stage: use the last-known agent
// for the ticket (or the config default), spawn it with the rework prompt, and
// on success route the ticket back to status=review.
func (d *Daemon) runReworkStage(ctx, taskCtx context.Context, log *slog.Logger, ticketID string, t *ticket.Ticket, filePath string) {
	agentName := d.reworkAgent(t)
	agentCfg, ok := d.cfg.Agents[agentName]
	if !ok {
		log.Error("rework: unknown agent", "agent", agentName)
		d.pauseTicket(t, filePath, fmt.Sprintf("rework: unknown agent %q", agentName))
		return
	}

	binaryPath, err := d.agentLookup(agentCfg.Binary)
	if err != nil {
		log.Error("rework: agent binary lookup failed", "binary", agentCfg.Binary, "err", err)
		d.pauseTicket(t, filePath, fmt.Sprintf("rework: agent binary unavailable: %s", err))
		return
	}

	// Set status=in_progress, started_at.
	now := time.Now()
	_ = t.SetField("status", string(ticket.StatusInProgress))
	_ = t.SetField("started_at", now.Format(time.RFC3339))
	if err := d.writeTicket(t, filePath); err != nil {
		log.Error("rework: write failed", "phase", "pickup", "err", err)
		return
	}
	log.Info("rework picked up", "agent", agentName)
	d.broadcastTicketUpdateLocking(ticketID)

	if err := taskCtx.Err(); err != nil {
		log.Info("rework cancelled before worktree creation")
		return
	}

	repoName, repoPath, err := d.resolvePath(t)
	if err != nil {
		log.Error("rework: resolve path failed", "err", err)
		d.pauseTicket(t, filePath, "rework: resolve path failed: "+err.Error())
		return
	}

	branch := d.ticketBranch(t)
	wtPath, _, err := d.worktrees.Create(repoPath, repoName, ticketID, branch)
	if err != nil {
		log.Error("rework: create worktree failed", "err", err)
		d.pauseTicket(t, filePath, "rework: create worktree failed: "+err.Error())
		return
	}
	_ = t.SetField("branch", branch)
	if err := d.writeTicket(t, filePath); err != nil {
		log.Error("rework: write branch failed", "err", err)
		return
	}

	stageCfg := d.cfg.Stages[config.ReworkStageName]
	rendered, err := d.renderTicketPrompt(stageCfg.Prompt, t, filePath, wtPath)
	if err != nil {
		log.Error("rework: render prompt failed", "err", err)
		d.pauseTicket(t, filePath, "rework: render prompt failed: "+err.Error())
		return
	}
	if rendered != "" {
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, true)
	}

	args, settingsFile, sessionID, err := buildAgentArgs(agentCfg, rendered, tmux.ChannelName(ticketID))
	if err != nil {
		log.Error("rework: build agent args failed", "err", err)
		d.pauseTicket(t, filePath, "rework: build agent args failed: "+err.Error())
		return
	}
	if settingsFile != "" {
		defer os.Remove(settingsFile)
	}

	params := d.buildRunnerParams(agentCfg, stageCfg, binaryPath, args, wtPath, ticketID, config.ReworkStageName, sessionID)
	result, runnerErr := d.runner(taskCtx, params)
	if runnerErr != nil && taskCtx.Err() == nil {
		log.Error("rework: runner failed", "err", runnerErr)
		d.killTaskWindow(ticketID)
		d.pauseTicket(t, filePath, "rework: runner failed: "+runnerErr.Error())
		return
	}

	d.materializeAgentLogs(log, params)

	if taskCtx.Err() != nil {
		if ctx.Err() != nil {
			log.Warn("rework: interrupted by shutdown")
			return
		}
		log.Info("rework: interrupted by user")
		return
	}

	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		log.Error("rework: re-read failed after agent exit", "err", err)
		return
	}
	if d.isUserOverride(t2.Status) {
		log.Info("rework: user override during execution", "status", t2.Status)
		return
	}

	history := t2.History
	history = append(history, ticket.HistoryEntry{
		Stage:       config.ReworkStageName,
		Agent:       agentName,
		ExitCode:    result.ExitCode,
		StartedAt:   t2.StartedAt,
		CompletedAt: &result.ExitedAt,
	})
	_ = t2.SetField("history", history)

	if result.ExitCode == 0 {
		_ = t2.SetField("status", string(ticket.StatusHumanReview))
		_ = t2.SetField("last_error", "")
		log.Info("rework completed, routed back to human_review", "branch", t2.Branch)
		d.killTaskWindow(ticketID)
	} else {
		_ = t2.SetField("status", string(ticket.StatusPaused))
		_ = t2.SetField("last_error", fmt.Sprintf("rework agent exited with code %d", result.ExitCode))
		log.Warn("rework paused", "exit_code", result.ExitCode)
	}

	if err := d.writeTicket(t2, filePath); err != nil {
		log.Error("rework: write failed", "phase", "exit", "err", err)
		return
	}

	d.mu.Lock()
	d.tickets[ticketID] = &ticketState{ticket: t2, filePath: filePath}
	d.broadcastTicketUpdate(ticketID)
	d.mu.Unlock()
}

// reworkAgent picks the agent to use for the rework stage. Priority:
//  1. explicit agent override on the ticket
//  2. the agent recorded on the most recent history entry
//  3. the default agent from config
func (d *Daemon) reworkAgent(t *ticket.Ticket) string {
	if t.Agent != "" {
		return t.Agent
	}
	for i := len(t.History) - 1; i >= 0; i-- {
		if a := t.History[i].Agent; a != "" {
			return a
		}
	}
	return d.cfg.DefaultAgent
}

// setupPlannotatorWorktree creates a disposable detached worktree at the
// merge-base of the ticket branch and the repo's default branch, then applies
// the branch's diff on top as unstaged changes. Plannotator's default
// "unstaged" view then shows everything the agent committed without the
// daemon having to touch the ticket branch itself.
//
// Returns the path to the review worktree and a cleanup function that removes
// it. The cleanup is safe to call once, regardless of outcome.
func setupPlannotatorWorktree(log *slog.Logger, repoPath, branchWtPath string) (string, func(), error) {
	defaultBranch, err := worktree.DetectDefaultBranch(repoPath)
	if err != nil {
		return "", nil, fmt.Errorf("detect default branch: %w", err)
	}

	mergeBase, err := runGit(branchWtPath, "merge-base", defaultBranch, "HEAD")
	if err != nil {
		return "", nil, fmt.Errorf("merge-base: %w", err)
	}
	mergeBase = strings.TrimSpace(mergeBase)
	if mergeBase == "" {
		return "", nil, errors.New("merge-base returned empty sha")
	}

	reviewPath := branchWtPath + ".plannotator"
	// Clean up any leftover from a prior crashed run before asking git to create the worktree.
	if _, err := os.Stat(reviewPath); err == nil {
		_, _ = runGit(repoPath, "worktree", "remove", "--force", reviewPath)
		_ = os.RemoveAll(reviewPath)
	}

	if _, err := runGit(repoPath, "worktree", "add", "--detach", reviewPath, mergeBase); err != nil {
		return "", nil, fmt.Errorf("worktree add: %w", err)
	}

	cleanup := func() {
		if _, err := runGit(repoPath, "worktree", "remove", "--force", reviewPath); err != nil {
			log.Warn("plannotator: worktree remove failed, falling back to rm -rf", "err", err, "path", reviewPath)
			_ = os.RemoveAll(reviewPath)
		}
	}

	diff, err := runGit(branchWtPath, "diff", "--binary", mergeBase+"..HEAD")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git diff: %w", err)
	}

	if strings.TrimSpace(diff) != "" {
		applyCmd := exec.Command("git", "apply")
		applyCmd.Dir = reviewPath
		applyCmd.Stdin = strings.NewReader(diff)
		if out, err := applyCmd.CombinedOutput(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("git apply: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}

	return reviewPath, cleanup, nil
}

// runGit runs git in dir and returns stdout. On failure the error includes
// stderr so operators can diagnose without turning on debug logging.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}
