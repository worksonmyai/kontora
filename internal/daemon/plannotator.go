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
	"slices"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/hook"
	"github.com/worksonmyai/kontora/internal/metrics"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// Sentinel strings emitted by `plannotator review` on stdout. Plannotator
// signals the user's choice through one of these exact lines (see
// apps/hook/server/index.ts in the plannotator repo); anything else is treated
// as feedback for the rework stage.
const (
	plannotatorApprovedMarker  = "Code review completed — no changes requested."
	plannotatorCancelledMarker = "Review session closed without feedback."
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

// defaultPlannotatorSpawner runs plannotator as a subprocess and returns
// stdout. The process does not share stdout with the daemon so that the
// annotation blob remains intact.
func defaultPlannotatorSpawner(ctx context.Context, params PlannotatorParams) (string, error) {
	var stdout bytes.Buffer
	if _, err := process.Run(ctx, process.RunParams{
		Binary:  params.Binary,
		Args:    params.Args,
		Dir:     params.Dir,
		Timeout: params.Timeout,
		Stdout:  &stdout,
		Env:     envPairs(withCommonPath(params.Env)),
	}); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// StartPlannotatorReview spawns a plannotator subprocess against the ticket's
// worktree. The user's choice is signalled by plannotator's stdout: the
// approved/cancelled marker strings leave the ticket parked in human_review,
// while any other output is treated as feedback and parks the ticket at
// stage=rework, status=todo for the scheduler to pick up.
//
// Synchronous errors:
//   - web.ErrTicketNotFound: unknown ticket
//   - web.ErrPlannotatorInFlight: a previous invocation is still running
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
		return fmt.Errorf("%w: ticket %s is in status %s, review needs %s", web.ErrInvalidState, id, t.Status, ticket.StatusHumanReview)
	}

	repoName, repoPath, err := d.resolvePath(t)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	cfg := d.config()
	branch := ticketBranch(cfg, t)
	// Snapshot the base for the same reason as the branch: the review runs in a
	// goroutine and the ticket file can change while it is running.
	base := ticketBase(t)
	reviewPath := d.worktrees.Path(repoName, id) + ".plannotator"

	binaryPath, err := d.plannotatorLookup(cfg.Plannotator.Binary)
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

	// The whole run uses the settings the review started with, so a reload
	// mid-review cannot change the timeout or the reviews directory.
	go d.runPlannotator(ctx, log, cfg.Plannotator, id, binaryPath, repoPath, branch, base, reviewPath)

	return nil
}

// releasePlannotator ends a ticket's Plannotator session and replaces the pickup
// the open session cost it. runTicket drops such a pickup rather than queueing
// behind a session that may stay open for as long as its timeout allows, so
// without this the ticket would sit in todo until something else moved it.
//
// A ticket that is running needs no offer, and its run owns the cached ticket
// this would otherwise read.
func (d *Daemon) releasePlannotator(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cancel, ok := d.plannotator[id]; ok {
		cancel()
		delete(d.plannotator, id)
	}
	_, deferred := d.plannotatorDeferred[id]
	delete(d.plannotatorDeferred, id)
	if !deferred {
		return
	}
	if _, running := d.running[id]; running {
		return
	}
	if ts, ok := d.tickets[id]; ok && ts.ticket.Status == ticket.StatusTodo {
		d.enqueue(ts.ticket)
	}
}

func (d *Daemon) runPlannotator(ctx context.Context, log *slog.Logger, pcfg config.Plannotator, id, binaryPath, repoPath, branch, base, reviewPath string) {
	defer d.releasePlannotator(id)

	reviewWt, cleanup, err := setupPlannotatorWorktree(log, repoPath, branch, base, reviewPath)
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
		Args:    []string{"review"},
		Dir:     reviewWt,
		Env:     map[string]string{"PLANNOTATOR_REMOTE": "0"},
		Timeout: pcfg.Timeout.Duration,
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

	switch strings.TrimSpace(stdout) {
	case "", plannotatorApprovedMarker:
		log.Info("plannotator: approved")
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeApproved,
		})
		return
	case plannotatorCancelledMarker:
		log.Info("plannotator: cancelled by user")
		d.broker.Broadcast(web.TicketEvent{
			Type:    "plannotator_finished",
			Ticket:  web.TicketInfo{ID: id},
			Outcome: web.PlannotatorOutcomeCancelled,
		})
		return
	}

	reviewsDir := config.ExpandTilde(pcfg.ReviewsDir)
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
	reviewFile := filepath.Join(reviewsDir, id+".md")
	if wErr := os.WriteFile(reviewFile, []byte(stdout), 0o644); wErr != nil {
		log.Error("plannotator: write review file failed", "err", wErr, "path", reviewFile)
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
		_ = os.Remove(reviewFile)
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
	// A reviewer chose rework, so this transition is a request.
	if err := d.writeTicket(t2, filePath, notify.OriginRequest); err != nil {
		return fmt.Errorf("write ticket: %w", err)
	}

	d.mu.Lock()
	d.setTicketState(id, t2, filePath)
	d.enqueue(t2)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// runReworkStage executes the built-in rework stage: use the last-known agent
// for the ticket (or the config default), spawn it with the rework prompt, and
// on success route the ticket back to status=review.
func (d *Daemon) runReworkStage(ctx, taskCtx context.Context, cfg *config.Config, log *slog.Logger, ticketID string, t *ticket.Ticket, filePath string) {
	agentName := d.reworkAgent(cfg, t)
	agentCfg, ok := cfg.Agents[agentName]
	if !ok {
		log.Error("rework: unknown agent", "agent", agentName)
		d.pauseTicket(t, filePath, fmt.Sprintf("rework: unknown agent %q", agentName))
		return
	}

	binaryPath, err := d.agentLookup(agentCfg.Binary)
	if err != nil {
		log.Error("rework: agent binary lookup failed", "binary", agentCfg.Binary, "err", err)
		// spawnAgentRun records a failed run for the same case, so this path does
		// too: a rework stage broken at the config level must not be invisible in
		// kontora.stage.runs.
		d.recordReworkRun(taskCtx, agentName, t.Pipeline, process.Result{}, err, 0)
		d.pauseTicket(t, filePath, fmt.Sprintf("rework: agent binary unavailable: %s", err))
		return
	}

	// Set status=in_progress, started_at, and claim for this instance.
	now := time.Now()
	if err := d.editTicket(t, filePath, notify.OriginDaemon, func() error {
		_ = t.SetField("status", string(ticket.StatusInProgress))
		_ = t.SetField("started_at", now.Format(time.RFC3339))
		_ = t.SetField("claimed_by", d.instanceName)
		return nil
	}); err != nil {
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

	branch := ticketBranch(cfg, t)
	wtPath, created, err := d.worktrees.Create(worktree.CreateOpts{
		RepoPath: repoPath, RepoName: repoName, TaskID: ticketID,
		Branch: branch, Base: ticketBase(t),
	})
	if err != nil {
		log.Error("rework: create worktree failed", "err", err)
		d.pauseTicket(t, filePath, "rework: create worktree failed: "+err.Error())
		return
	}
	// The rework stage builds its worktree itself rather than through
	// prepareWorktreeForAgent, so it fires the lifecycle hooks itself too.
	reworkHookCtx := hook.Context{
		TicketID:   ticketID,
		TicketFile: filePath,
		Worktree:   wtPath,
		RepoPath:   repoPath,
		Branch:     branch,
		Stage:      config.ReworkStageName,
		Agent:      agentName,
	}
	if !d.runWorktreeCreatedHooks(taskCtx, cfg, log, t, filePath, reworkHookCtx, created) {
		return
	}
	if err := d.editTicket(t, filePath, notify.OriginDaemon, func() error {
		_ = t.SetField("branch", branch)
		// Rework runs like any other stage from here on: the per-run summary
		// belongs to this run only, and the ticket-level one is stale until this
		// run has been folded into it.
		_ = t.SetField("summary", "")
		_ = t.SetField("final_summary", "")
		return nil
	}); err != nil {
		log.Error("rework: write branch failed", "err", err)
		return
	}

	stageCfg := cfg.Stages[config.ReworkStageName]
	rendered, err := d.renderTicketPrompt(cfg, stageCfg.Prompt, t, filePath, wtPath)
	if err != nil {
		log.Error("rework: render prompt failed", "err", err)
		d.pauseTicket(t, filePath, "rework: render prompt failed: "+err.Error())
		return
	}
	if rendered != "" {
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, true, "")
	}

	// Rework spawns its agent here rather than through runAgentOnce, so the
	// stage model and effort are resolved on this path too.
	model := stageCfg.Model.For(agentName, agentCfg)
	effort := stageCfg.Effort.For(agentName, agentCfg)
	effModel, effEffort := agentCfg.Effective(model, effort)
	args, settingsFile, sessionID, err := buildAgentArgs(agentCfg, rendered, stageBrief(cfg.SystemPrompt, ticketID, config.ReworkStageName), tmux.ChannelName(d.tmuxSession, ticketID), "", model, effort, "", nil, false)
	if err != nil {
		log.Error("rework: build agent args failed", "err", err)
		d.pauseTicket(t, filePath, "rework: build agent args failed: "+err.Error())
		return
	}
	if settingsFile != "" {
		defer os.Remove(settingsFile)
	}

	stageStartCtx := reworkHookCtx
	stageStartCtx.Event = config.HookStageStart
	if hookErr := d.runHooks(taskCtx, cfg, log, stageStartCtx); hookErr != nil {
		// The stage is over before the agent ran. Every other way this path ends
		// records a run, so a rework blocked by its own hook must not be the one
		// case missing from kontora.stage.runs.
		d.recordReworkRun(taskCtx, agentName, t.Pipeline, process.Result{}, hookErr, 0)
		if taskCtx.Err() == nil {
			d.pauseTicket(t, filePath, hookErr.Error())
		}
		return
	}

	params := d.buildRunnerParams(cfg, agentCfg, stageCfg, binaryPath, args, wtPath, ticketID,
		agentName, config.ReworkStageName, config.ReworkStageName, sessionID, checkpointSetup{})
	runIndex := stageRunIndex(t, config.ReworkStageName)
	// Rework always opens a session of its own, so nothing in it belongs to an
	// earlier run and the scope carries no prior usage.
	scope := runScope{startedAt: time.Now()}
	d.setLiveRun(ticketID, liveRun{stage: config.ReworkStageName, agent: agentName, run: runIndex, params: params, startedAt: scope.startedAt})
	// The built-in rework stage calls the runner directly and never reaches
	// spawnAgentRun, so it records its own stage run. Without this every
	// built-in rework run would be invisible.
	started := time.Now()
	result, runnerErr := d.runner(taskCtx, params)
	d.clearLiveRun(ticketID)
	d.recordReworkRun(taskCtx, agentName, t.Pipeline, result, runnerErr, time.Since(started))
	stageEndCtx := reworkHookCtx
	stageEndCtx.Event = config.HookStageEnd
	stageEndCtx.ExitCode = &result.ExitCode

	if runnerErr != nil && taskCtx.Err() == nil {
		log.Error("rework: runner failed", "err", runnerErr)
		d.killTaskWindow(ticketID)
		// The stage_start hooks have run, so their counterpart runs on this path
		// too. The pause belongs to the runner: a hook failure here only reaches
		// the log.
		if hookErr := d.runHooks(taskCtx, cfg, log, stageEndCtx); hookErr != nil {
			log.Warn("rework: stage_end hooks failed after a failed run", "err", hookErr)
		}
		d.pauseTicket(t, filePath, "rework: runner failed: "+runnerErr.Error())
		return
	}

	usage, usageComplete := d.materializeAgentLogs(log, params, stageEventsPath(cfg, ticketID, config.ReworkStageName, runIndex), scope)
	d.recordTokens(taskCtx, config.ReworkStageName, agentName, usage, usageComplete)
	sessionKind, sessionRef := runSessionRef(cfg, ticketID, params, scope.startedAt)

	if taskCtx.Err() != nil {
		if ctx.Err() != nil {
			log.Warn("rework: interrupted by shutdown")
			return
		}
		log.Info("rework: interrupted by user")
		return
	}

	hookErr := d.runHooks(taskCtx, cfg, log, stageEndCtx)
	if taskCtx.Err() != nil {
		log.Info("rework: interrupted during stage_end hooks")
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

	// If another instance claimed the ticket while the rework agent ran, discard
	// the result: write nothing and keep the worktree.
	if d.claimedElsewhere(t2) {
		log.Info("rework: discarding exit result, claimed by another instance", "claimed_by", t2.ClaimedBy)
		d.killTaskWindow(ticketID)
		return
	}

	summary := runSummary(t2.Summary, finalAssistantMessage(log, params, scope.startedAt))
	history := t2.History
	history = append(history, ticket.HistoryEntry{
		Stage:       config.ReworkStageName,
		Agent:       agentName,
		Model:       effModel,
		Effort:      effEffort,
		ExitCode:    result.ExitCode,
		Run:         runIndex,
		StartedAt:   t2.StartedAt,
		CompletedAt: &result.ExitedAt,
		Summary:     summary,
		SessionKind: sessionKind,
		SessionRef:  sessionRef,
	})
	_ = t2.SetField("history", history)
	_ = t2.SetField("summary", summary)

	switch {
	case result.ExitCode != 0:
		_ = t2.SetField("status", string(ticket.StatusPaused))
		_ = t2.SetField("last_error", fmt.Sprintf("rework agent exited with code %d", result.ExitCode))
		log.Warn("rework paused", "exit_code", result.ExitCode)
	case hookErr != nil:
		// The rework itself succeeded, so the hook decides the outcome alone.
		_ = t2.SetField("status", string(ticket.StatusPaused))
		_ = t2.SetField("last_error", hookErr.Error())
		appendSystemNote(t2, hookErr.Error())
	default:
		_ = t2.SetField("status", string(ticket.StatusHumanReview))
		_ = t2.SetField("last_error", "")
		log.Info("rework completed, routed back to human_review", "branch", t2.Branch)
		d.killTaskWindow(ticketID)
	}

	if err := d.writeTicket(t2, filePath, notify.OriginDaemon); err != nil {
		log.Error("rework: write failed", "phase", "exit", "err", err)
		return
	}

	d.mu.Lock()
	d.setTicketState(ticketID, t2, filePath)
	d.broadcastTicketUpdate(ticketID)
	d.mu.Unlock()

	// Rework is the successful end of a review round, so the ticket-level
	// summary is regenerated from the history this run just extended. It runs
	// on the daemon's context, off this goroutine, for the reasons in
	// startFinalSummary.
	if result.ExitCode == 0 && hookErr == nil {
		params := finalSummaryParams{
			log:       log,
			cfg:       cfg,
			ticketID:  ticketID,
			filePath:  filePath,
			agentName: agentName,
			dir:       repoPath,
			tkt:       finalSummaryTicket{Title: t2.Title(), Body: t2.Body},
			runs:      eligibleFinalSummaryRuns(t2),
			status:    t2.Status,
		}
		d.background.Go(func() { d.runFinalSummary(ctx, params) })
	}
}

// recordReworkRun reports the built-in rework stage's run, matching what
// recordStageRun reports for every stage that goes through spawnAgentRun. It
// defaults the same way too: only a run that reached the agent and came back
// clean is a success, so a path that ends before the runner is a failure rather
// than a silent success.
//
// The rework stage does not go through handleAgentExit, so it produces no
// kontora.stage.transitions; its outcome is the whole record of what it did.
func (d *Daemon) recordReworkRun(taskCtx context.Context, agentName, pipelineName string, result process.Result, runnerErr error, elapsed time.Duration) {
	outcome := metrics.OutcomeFailure
	switch {
	case taskCtx.Err() != nil:
		outcome = metrics.OutcomeCancelled
	case runnerErr == nil && result.ExitCode == 0:
		outcome = metrics.OutcomeSuccess
	}
	d.metrics.StageRun(taskCtx, metrics.StageAttrs{
		Stage:    config.ReworkStageName,
		Agent:    agentName,
		Pipeline: pipelineName,
		Outcome:  outcome,
		ExitCode: result.ExitCode,
	}, elapsed)
}

// reworkAgent picks the agent to use for the rework stage. Priority:
//  1. explicit agent override on the ticket
//  2. the agent recorded on the most recent history entry
//  3. the default agent from config
func (d *Daemon) reworkAgent(cfg *config.Config, t *ticket.Ticket) string {
	if t.Agent != "" {
		return t.Agent
	}
	for _, h := range slices.Backward(t.History) {
		if a := h.Agent; a != "" {
			return a
		}
	}
	return cfg.DefaultAgent
}

// setupPlannotatorWorktree creates a disposable detached worktree at the
// merge-base of the ticket branch and the ticket's base branch, or the repo's
// default branch when unset, then applies
// the branch's diff on top as unstaged changes. Plannotator's default
// "unstaged" view then shows everything the agent committed without the
// daemon having to touch the ticket branch itself.
//
// All git operations run against repoPath with an explicit branch ref, so the
// ticket's normal working worktree does not need to exist on disk — only the
// branch itself does. Returns the path to the review worktree and a cleanup
// function that removes it. The cleanup is safe to call once, regardless of
// outcome.
func setupPlannotatorWorktree(log *slog.Logger, repoPath, branch, base, reviewPath string) (string, func(), error) {
	baseRef, err := worktree.ResolveBase(repoPath, base)
	if err != nil {
		return "", nil, fmt.Errorf("resolve base branch: %w", err)
	}

	mergeBase, err := runGit(repoPath, "merge-base", baseRef, branch)
	if err != nil {
		return "", nil, fmt.Errorf("merge-base: %w", err)
	}
	mergeBase = strings.TrimSpace(mergeBase)
	if mergeBase == "" {
		return "", nil, errors.New("merge-base returned empty sha")
	}

	if err := os.MkdirAll(filepath.Dir(reviewPath), 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir review parent: %w", err)
	}
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

	diff, err := runGit(repoPath, "diff", "--binary", mergeBase+".."+branch)
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
