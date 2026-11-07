package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func (d *Daemon) pauseReworkUnlessCancelled(ctx context.Context, t *ticket.Ticket, path string, err error) {
	if ctx.Err() == nil {
		d.pauseExecution(ctx, t, path, err.Error())
	}
}

func reworkInterrupted(root, task context.Context, log *slog.Logger) bool {
	if task.Err() == nil {
		return false
	}
	if root.Err() != nil {
		log.Warn("rework: interrupted by shutdown")
	} else {
		log.Info("rework: interrupted by user")
	}
	return true
}

func (d *Daemon) finalizeRework(ctx, taskCtx context.Context, p spawnAgentParams, run agentRun, hookErr error, guard exitGuardParams) {
	t, ok := d.readTicketForExit(guard)
	if !ok {
		return
	}
	if hookErr != nil && run.Outcome == "complete" {
		run.Outcome, run.OutcomeReason = "failed", hookErr.Error()
	}
	finish := func(current *ticket.Ticket) {
		d.recordValidatedRun(ctx, config.ReworkStageName, p.agentName, current.Pipeline, run)
		// Summary generation must not retain the execution's worker slot.
		if current.Status == ticket.StatusHumanReview {
			d.killTaskWindow(p.ticketID)
			params := finalSummaryParams{log: p.log, cfg: p.cfg, ticketID: p.ticketID, filePath: p.filePath, agentName: p.agentName,
				dir: p.repoPath, tkt: finalSummaryTicket{Title: current.Title(), Body: current.Body}, runs: eligibleFinalSummaryRuns(current), status: current.Status, expected: current.Clone()}
			d.background.Go(func() { d.runFinalSummary(ctx, params) })
		}
	}
	saved, err := d.patchFinalExecution(taskCtx, t, p.filePath, run.Execution, func(current *ticket.Ticket) error {
		summary := runSummary(current.Summary, run.FinalMessage)
		row := ticket.HistoryEntry{Stage: config.ReworkStageName, Agent: p.agentName, Run: p.run,
			Model: run.Model, Effort: run.Effort, ExitCode: run.Result.ExitCode, StartedAt: current.StartedAt, CompletedAt: &run.Result.ExitedAt,
			Summary: summary, SessionKind: run.SessionKind, SessionRef: run.SessionRef, RunOutcome: ticket.RunOutcome(run.Outcome), OutcomeReason: run.OutcomeReason}
		if run.Execution != nil {
			row.InvocationID = run.Execution.Invocation.ID
		}
		_ = current.SetField("history", append(current.History, row))
		_ = current.SetField("summary", summary)
		switch {
		case run.pauses():
			_ = current.SetField("status", string(ticket.StatusPaused))
			_ = current.SetField("last_error", run.OutcomeReason)
		case run.Result.ExitCode != 0:
			_ = current.SetField("status", string(ticket.StatusPaused))
			_ = current.SetField("last_error", fmt.Sprintf("rework agent exited with code %d", run.Result.ExitCode))
		case hookErr != nil:
			_ = current.SetField("status", string(ticket.StatusPaused))
			_ = current.SetField("last_error", hookErr.Error())
			appendSystemNote(current, hookErr.Error())
		default:
			_ = current.SetField("status", string(ticket.StatusHumanReview))
			_ = current.SetField("last_error", "")
		}
		return nil
	}, finish)
	if err != nil {
		p.log.Error("rework: write failed", "phase", "exit", "err", err)
		return
	}
	if run.Execution != nil {
		d.completeFinalization(run.Execution, saved)
	} else {
		finish(saved)
	}
}
