package daemon

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func (d *Daemon) runCheckNode(ctx context.Context, cfg *config.Config, t *ticket.Ticket, node config.WorkflowNode, rec *executionRecord, worktree string) agentRun {
	run := agentRun{Execution: rec, Outcome: "failed", ForcePause: true,
		Result: process.Result{ExitCode: -1, ExitedAt: time.Now(), Termination: process.TerminationNotStarted}}
	params := RunnerParams{Binary: "/bin/sh", Args: []string{"-c", node.Command}, Dir: worktree,
		Timeout: node.Timeout.Duration, TicketID: t.ID, SessionName: d.tmuxSession,
		LogFile: graphLogPath(cfg, t), RequireTermination: true,
		Env: agentEnv(cfg, config.Agent{}, d.configPath, "", node.ID, t.ID)}
	maps.Copy(params.Env, rec.Invocation.Env())
	rec.Phase = "running"
	if err := saveExecution(cfg, rec); err != nil {
		run.OutcomeReason = "persist check launch: " + err.Error()
		return run
	}
	result, err := d.runner(ctx, params)
	run.Result = result
	rec.RunnerTermination = result.Termination
	if errors.Is(err, process.ErrTerminationUncertain) || result.Termination == process.TerminationUnknown {
		run.OutcomeReason = "check termination is unconfirmed; inspect execution before recovery"
		d.retainUncertainExecution(ctx, t, rec)
		return run
	}
	switch {
	case ctx.Err() != nil || result.Interruption != nil:
		run.OutcomeReason = "check interrupted; inspect execution before recovery"
	case err != nil:
		run.OutcomeReason = "check launch or runner failed: " + err.Error()
	case result.Termination == process.TerminationNotStarted:
		run.OutcomeReason = "check did not start"
	default:
		run.ForcePause = false
		run.Outcome = "complete"
		if result.ExitCode != 0 {
			run.Outcome = "failed"
		}
	}
	return run
}
