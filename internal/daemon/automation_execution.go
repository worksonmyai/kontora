package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/pricing"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/prompt"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/stats"
	"github.com/worksonmyai/kontora/internal/tmux"
)

func (d *Daemon) executeAutomation(execution *automationExecution) (stopped bool) {
	run := execution.run
	defer func() {
		execution.cancel()
		if stopped {
			if saved, err := d.automationStore.GetRun(run.AutomationID, run.N); err == nil {
				run = saved
			}
			d.finalizeAutomationArtifacts(run)
			d.releaseAutomationRepoLock(run)
		}
		d.automationMu.Lock()
		if stopped && d.automationExecutions[run.AutomationID] == execution {
			delete(d.automationExecutions, run.AutomationID)
		}
		d.automationMu.Unlock()
		d.automationChanged()
	}()
	current, err := d.automationStore.GetRun(run.AutomationID, run.N)
	if err != nil {
		return false
	}
	run = current
	if run.Phase == "terminal" {
		return true
	}
	if run.Phase != "queued" && (run.Phase != "stopping" || !run.StartedAt.IsZero()) {
		return d.failAutomationRun(run, errors.New("run already crossed the launch boundary"), true)
	}
	if execution.ctx.Err() != nil || run.Phase == "stopping" {
		return d.cancelUnlaunchedAutomation(run)
	}
	launch, err := readAutomationLaunch(run)
	if err != nil {
		return d.failAutomationRun(run, err, false)
	}
	ctx, cancel := context.WithTimeout(execution.ctx, launch.Timeout)
	defer cancel()
	if err := d.acquireAutomationRepoLock(ctx, run, launch); err != nil {
		return d.failAutomationRun(run, automationContextError(ctx, run, err), errors.Is(err, process.ErrTerminationUncertain))
	}
	updated, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
		if r.Phase != "queued" || r.StopReason != "" || ctx.Err() != nil {
			return errors.Join(automation.ErrConflict, ctx.Err())
		}
		r.Phase, r.StartedAt = "preparing", time.Now()
		r.Session, r.Window = fmt.Sprintf("kontora-auto-%s-%d", r.AutomationID, r.N), "run"
		return nil
	})
	if err != nil {
		return d.failAutomationRun(run, automationContextError(ctx, run, err), false)
	}
	run = updated
	if err := d.automationEvent(run, "lifecycle", fmt.Sprintf("automation %s · run #%d · trigger %s", run.AutomationID, run.N, run.Trigger)); err != nil {
		return d.failAutomationRun(run, err, false)
	}
	prepared, err := d.prepareAutomationWorkspace(ctx, run, launch)
	run = prepared
	if err != nil {
		return d.failAutomationRun(run, automationContextError(ctx, run, err), errors.Is(err, process.ErrTerminationUncertain))
	}
	if err := d.runAutomationHooks(ctx, launch, run, config.HookStageStart, nil, ""); err != nil {
		return d.failAutomationRun(run, automationContextError(ctx, run, err), errors.Is(err, process.ErrTerminationUncertain))
	}
	rendered, err := d.renderAutomationPrompt(run)
	if err != nil {
		return d.failAutomationRun(run, err, false)
	}
	params, err := d.automationRunnerParams(ctx, run, launch, rendered)
	if err != nil {
		return d.failAutomationRun(run, automationContextError(ctx, run, err), false)
	}
	if _, err := os.Lstat(run.ResultPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return d.failAutomationRun(run, errors.New("result artifact already exists or cannot be inspected before launch"), false)
	}
	if err := ctx.Err(); err != nil {
		return d.failAutomationRun(run, automationContextError(ctx, run, err), false)
	}
	if err := d.automationEvent(run, "lifecycle", fmt.Sprintf("tmux session %s · %s · model %s · prompt %d chars", run.Session, run.Spec.Agent, launch.Model, len([]rune(rendered)))); err != nil {
		return d.failAutomationRun(run, err, false)
	}
	callbackDone := d.automationRunnerCallbacks(ctx, cancel, run, launch, &params)
	d.automationActive.Add(1)
	result, runnerErr := d.runner(ctx, params)
	d.automationActive.Add(-1)
	callbackDone()
	return d.completeAutomationRun(ctx, run, launch, params, result, runnerErr)
}

func (d *Daemon) completeAutomationRun(ctx context.Context, run automation.Run, launch automationLaunch, params RunnerParams, result process.Result, runnerErr error) bool {
	if errors.Is(runnerErr, process.ErrTerminationUncertain) || !automationTerminationConfirmed(result.Termination) {
		return d.failAutomationRun(run, errors.Join(runnerErr, process.ErrTerminationUncertain), true)
	}
	materialized := params
	materialized.LogFile = launch.Paths.Agent
	d.materializeAgentLogs(d.log, materialized, launch.Paths.Events, runScope{startedAt: run.StartedAt})
	outcome, summary := automationResult(ctx, run, result, runnerErr)
	reason, _, detected := d.detectAgentError(launch.Agent.config(), materialized, 0, run.StartedAt)
	if !detected {
		reason, _, detected = d.detectAgentError(launch.Agent.config(), params, 0, run.StartedAt)
	}
	if detected {
		outcome, summary = "failed", reason
	}
	logicalExit := result.ExitCode
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(result.Interruption, context.DeadlineExceeded) {
		logicalExit = 124
	}
	hookErr := d.runAutomationHooks(ctx, launch, run, config.HookStageEnd, &logicalExit, outcome)
	if errors.Is(hookErr, process.ErrTerminationUncertain) {
		return d.failAutomationRun(run, hookErr, true)
	}
	if hookErr != nil {
		outcome, summary = "failed", automationContextError(ctx, run, hookErr).Error()
	}
	cost := automationRunCost(launch.Paths.Events, launch.Model)
	updated, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
		if r.Phase == "terminal" {
			return automation.ErrConflict
		}
		// A stop can arrive after the runner exits or while the end hook runs.
		if r.StopReason != "" {
			outcome, summary, logicalExit = "failed", r.StopReason, 143
		} else if ctx.Err() != nil {
			outcome, summary = "failed", automationContextError(ctx, run, ctx.Err()).Error()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				logicalExit = 124
			}
		}
		r.Phase, r.Outcome, r.Result, r.CompletedAt = "terminal", outcome, summary, time.Now()
		r.ExitCode, r.ObservedExitCode, r.Cost = new(logicalExit), new(result.ExitCode), cost
		r.Termination = string(result.Termination)
		return nil
	})
	if err != nil {
		return false
	}
	run = updated
	kind := "success"
	if outcome != "complete" {
		kind = "error"
	}
	if err := d.automationEvent(run, kind, fmt.Sprintf("exit %d · %s · %s", logicalExit, time.Since(run.StartedAt).Round(time.Second), summary)); err != nil {
		d.log.Error("automation log finalization failed", "err", err)
	}
	return true
}

func automationTerminationConfirmed(evidence process.TerminationEvidence) bool {
	return evidence == process.TerminationNotStarted || evidence == process.TerminationProcessGroupGone || evidence == process.TerminationWindowGone
}

func automationContextError(ctx context.Context, run automation.Run, cause error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("timeout after %s: %w", run.Spec.Timeout, cause)
	}
	return cause
}

func (d *Daemon) cancelUnlaunchedAutomation(run automation.Run) bool {
	_, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
		if r.Phase == "terminal" {
			return nil
		}
		reason := r.StopReason
		if reason == "" {
			reason = "cancelled before launch"
		}
		r.Phase, r.Outcome, r.Result, r.CompletedAt = "terminal", "skipped", reason, time.Now()
		return nil
	})
	return err == nil
}

func (d *Daemon) automationRunnerParams(ctx context.Context, run automation.Run, launch automationLaunch, rendered string) (RunnerParams, error) {
	agent := launch.Agent.config()
	brief := fmt.Sprintf("You are running automation %s, run %d. Do not assume a ticket or epic exists. Before ending, publish an explicit result with kontora result --input-file <absolute JSON file> %s. The JSON must contain outcome (complete or blocked), summary, and next_step when blocked. Follow the configured agent permissions. A successful process exit does not prove that the task is complete.", run.AutomationID, run.N, run.AutomationID)
	model, effort := launch.Model, launch.Effort
	if agent.Kind() == "" {
		model, effort = "", ""
	}
	args, settings, sessionID, err := buildAgentArgs(agent, rendered+"\n\n"+brief, "", tmux.ChannelName(run.Session, run.Window), "", model, effort, "", nil, false)
	if err != nil {
		return RunnerParams{}, err
	}
	if settings != "" {
		name := "agent-settings.json"
		if agent.IsPi() {
			name = "agent-extension.mjs"
		}
		path := filepath.Join(launch.Paths.Dir, name)
		data, readErr := os.ReadFile(settings)
		if readErr == nil {
			readErr = durableConfigWrite(path, data, 0o600)
		}
		_ = os.Remove(settings)
		if readErr != nil {
			return RunnerParams{}, readErr
		}
		for i := range args {
			if args[i] == settings {
				args[i] = path
			}
		}
	}
	env := automationExecutionEnv(launch)
	inv := session.Invocation{Version: session.ResultVersion, ID: run.InvocationID, AutomationID: run.AutomationID, RunNumber: run.N, ResultPath: run.ResultPath}
	maps.Copy(env, inv.Env())
	sessionDir := ""
	if agent.IsPi() {
		sessionDir = filepath.Join(launch.Paths.Dir, "pi-sessions")
		if err := privateAutomationDirectory(sessionDir); err != nil {
			return RunnerParams{}, err
		}
		args = append(args, "--session-dir", sessionDir)
	}
	_, err = d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
		if r.Phase != "preparing" || r.StopReason != "" || ctx.Err() != nil {
			return errors.Join(automation.ErrConflict, ctx.Err())
		}
		r.SessionID, r.SessionDir = sessionID, sessionDir
		return nil
	})
	if err != nil {
		return RunnerParams{}, err
	}
	return RunnerParams{Binary: launch.Executable, Args: args, Dir: run.Worktree, Timeout: launch.Timeout, TicketID: run.Window, SessionName: run.Session, LogFile: launch.Paths.Capture, Interactive: agent.IsClaude(), SessionID: sessionID, SessionDir: sessionDir, Env: env, RequireTermination: true, StableWindowIdentity: true, TerminalReported: func() bool { return terminalReported(inv.ResultPath) }}, nil
}

func (d *Daemon) automationRunnerCallbacks(ctx context.Context, cancel context.CancelFunc, run automation.Run, launch automationLaunch, params *RunnerParams) func() {
	var mu sync.Mutex
	finished := false
	params.OnWindowCreated = func(identity tmux.WindowIdentity) error {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return automation.ErrConflict
		}
		if identity.WindowID == "" {
			return errors.New("runner supplied no window identity")
		}
		data, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		if err := durableConfigWrite(launch.Paths.Terminal, data, 0o600); err != nil {
			return err
		}
		_, err = d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
			if r.Phase != "preparing" && r.Phase != "running" && r.Phase != "stopping" {
				return automation.ErrConflict
			}
			r.WindowID = identity.WindowID
			return nil
		})
		return err
	}
	params.OnReady = func() {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		_, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
			if r.Phase != "preparing" || r.StopReason != "" || ctx.Err() != nil {
				return errors.Join(automation.ErrConflict, ctx.Err())
			}
			r.Phase = "running"
			return nil
		})
		if err != nil {
			cancel()
			return
		}
		d.automationChanged()
	}
	return func() { mu.Lock(); finished = true; mu.Unlock() }
}

func automationResult(ctx context.Context, run automation.Run, result process.Result, runnerErr error) (string, string) {
	switch {
	case runnerErr != nil:
		return "failed", automationContextError(ctx, run, runnerErr).Error()
	case result.Interruption != nil:
		return "failed", automationContextError(ctx, run, result.Interruption).Error()
	case ctx.Err() != nil:
		return "failed", automationContextError(ctx, run, ctx.Err()).Error()
	case result.Termination == process.TerminationNotStarted:
		return "failed", "agent did not start"
	case result.ExitCode != 0:
		return "failed", fmt.Sprintf("agent exited with code %d", result.ExitCode)
	}
	report, err := session.ReadResult(run.ResultPath)
	if err != nil {
		return "failed", "result protocol: " + err.Error()
	}
	if report.Outcome != "complete" {
		return "failed", report.Summary + " · " + report.NextStep
	}
	return "complete", report.Summary
}

func (d *Daemon) finalizeAutomationArtifacts(run automation.Run) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := d.cleanupAutomationWorkspace(cleanupCtx, run); err != nil {
		d.log.Warn("automation worktree retained", "automation", run.AutomationID, "err", err)
	}
	cancel()
	if run.LogPath == "" {
		return
	}
	if _, raw, err := d.AutomationLog(run.AutomationID, run.N); err == nil {
		if err := durableConfigWrite(run.LogPath, []byte(raw), 0o600); err != nil {
			d.log.Error("automation composed log failed", "err", err)
		}
	}
}

func (d *Daemon) failAutomationRun(run automation.Run, cause error, uncertain bool) bool {
	phase := "terminal"
	if uncertain {
		phase = "recovery_required"
	}
	saved, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
		if r.Phase == "terminal" {
			return nil
		}
		r.Phase, r.Result = phase, cause.Error()
		if !uncertain {
			r.Outcome, r.CompletedAt = "failed", time.Now()
			if r.StopReason != "" {
				r.Result = r.StopReason
				r.ExitCode = new(143)
			}
		}
		return nil
	})
	if err != nil {
		d.log.Error("saving automation failure", "err", err)
		return false
	}
	if uncertain {
		if err := d.retainAutomationRepoLock(saved); err != nil {
			d.log.Error("retaining automation checkout lease", "automation", saved.AutomationID, "err", err)
		}
	}
	if saved.ResultPath != "" {
		if err := d.automationEvent(saved, "error", saved.Result); err != nil {
			d.log.Warn("writing automation failure log", "err", err)
		}
	}
	d.log.Warn("automation run failed", "automation", run.AutomationID, "run", run.N, "err", cause)
	return !uncertain
}

func automationRunCost(path, model string) *string {
	metadata, err := stats.ReadSidecarMetadata(path)
	if err != nil {
		return nil
	}
	if metadata.Model != "" {
		model = metadata.Model
	}
	_, amount, priced := calculateRunCost(model, metadata.Usage, pricing.Resolve)
	if !priced {
		return nil
	}
	return new(amount.String())
}

func (d *Daemon) renderAutomationPrompt(run automation.Run) (string, error) {
	runContext := func(r automation.Run) prompt.AutomationRunContext {
		location, err := time.LoadLocation(r.Spec.Timezone)
		if err != nil {
			location = time.UTC
		}
		return prompt.AutomationRunContext{Number: r.N, N: r.N, InvocationID: r.InvocationID, Branch: r.Branch, Date: r.StartedAt.In(location).Format("2006-01-02"), Trigger: r.Trigger, ScheduledFor: r.ScheduledFor, AcceptedAt: r.AcceptedAt, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt, Outcome: r.Outcome, Result: r.Result, ExitCode: r.ExitCode, Cost: r.Cost}
	}
	data := prompt.AutomationData{Automation: prompt.AutomationContext{ID: run.AutomationID, Key: run.Key, Name: run.Spec.Name, Prompt: run.Spec.Prompt, Path: run.Spec.Path, Agent: run.Spec.Agent, Model: run.Spec.Model, Schedule: run.Spec.Schedule, Timezone: run.Spec.Timezone, Workspace: run.Spec.Workspace, Overlap: run.Spec.Overlap, Timeout: run.Spec.Timeout, Enabled: run.Spec.Enabled}, Run: runContext(run)}
	runs, err := d.automationStore.Runs(run.AutomationID)
	if err != nil {
		return "", err
	}
	for _, previous := range runs {
		if previous.N < run.N && previous.Phase == "terminal" && previous.Outcome != "skipped" && !previous.StartedAt.IsZero() {
			data.LastRun = runContext(previous)
			break
		}
	}
	return prompt.RenderAutomation(run.Spec.Prompt, data, run.Worktree)
}
