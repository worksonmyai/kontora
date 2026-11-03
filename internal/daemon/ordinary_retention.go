package daemon

import (
	"context"
	"log/slog"

	"github.com/worksonmyai/kontora/internal/ticket"
)

func (d *Daemon) finishOrdinaryRun(ctx context.Context, cancel context.CancelFunc, ticketID, branchKey, filePath string, log *slog.Logger) {
	cancelled := ctx.Err() != nil
	cancel()
	if ordinaryReservationRetained(ctx) {
		return
	}
	d.mu.Lock()
	delete(d.running, ticketID)
	if branchKey != "" && d.runningBranches[branchKey] == ticketID {
		delete(d.runningBranches, branchKey)
	}
	d.mu.Unlock()
	if cancelled {
		if current, err := ticket.ParseFile(filePath); err == nil && isTerminalOverride(current.Status) {
			d.cleanupWorktree(log, current)
		}
	}
}
