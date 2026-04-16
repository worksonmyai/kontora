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

// defaultPlannotatorLookup uses exec.LookPath to verify the plannotator binary
// is installed before we attempt a spawn. Keeping this as a separate hook lets
// tests bypass the real PATH check when using a canned spawner.
func defaultPlannotatorLookup(binary string) error {
	if binary == "" {
		return errors.New("plannotator binary is empty")
	}
	if _, err := exec.LookPath(binary); err != nil {
		return err
	}
	return nil
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

	if t.Status != "review" {
		return web.ErrInvalidState
	}

	repoName, _, err := d.resolvePath(t)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	wtPath := d.worktrees.Path(repoName, id)
	if _, statErr := os.Stat(wtPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return web.ErrPlannotatorWorkdir
		}
		log.Error("plannotator: stat worktree failed", "path", wtPath, "err", statErr)
		return fmt.Errorf("stat worktree: %w", statErr)
	}

	if err := d.plannotatorLookup(d.cfg.Plannotator.Binary); err != nil {
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

	go d.runPlannotator(ctx, log, id, wtPath)

	return nil
}

func (d *Daemon) runPlannotator(ctx context.Context, log *slog.Logger, id, wtPath string) {
	defer func() {
		d.mu.Lock()
		if cancel, ok := d.plannotator[id]; ok {
			cancel()
			delete(d.plannotator, id)
		}
		d.mu.Unlock()
	}()

	params := PlannotatorParams{
		Binary:  d.cfg.Plannotator.Binary,
		Dir:     wtPath,
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

	params := d.buildRunnerParams(agentCfg, stageCfg, args, wtPath, ticketID, config.ReworkStageName, sessionID)
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
		_ = t2.SetField("status", reworkSuccessStatus(d.cfg))
		_ = t2.SetField("last_error", "")
		log.Info("rework completed, routed back to review", "branch", t2.Branch)
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

// reworkSuccessStatus returns the status the ticket should land in after a
// successful rework. Defaults to "review" (a custom status users are expected
// to declare); falls back to paused if that status isn't configured so the
// user notices instead of silently losing the ticket in an unknown state.
func reworkSuccessStatus(cfg *config.Config) string {
	if cfg.IsCustomStatus("review") {
		return "review"
	}
	return string(ticket.StatusPaused)
}
