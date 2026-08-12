package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/hook"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// runHooks runs the hooks configured for hc.Event and returns the failure of
// the first one whose on_failure resolved to pause, or nil. A hook set to warn
// is logged and does not stop the sequence.
//
// The ticket is left untouched: what a failure does to it differs by event, so
// each call site applies its own policy to the returned error.
func (d *Daemon) runHooks(ctx context.Context, cfg *config.Config, log *slog.Logger, hc hook.Context) error {
	hooks := cfg.HooksFor(hc.RepoPath, hc.Event)
	if len(hooks) == 0 {
		return nil
	}
	hc.Project, _, _ = cfg.ProjectFor(hc.RepoPath)

	specs := make([]hook.Spec, len(hooks))
	for i, h := range hooks {
		specs[i] = hook.Spec{
			Name:    h.Name,
			Run:     h.Run,
			Timeout: h.TimeoutOrDefault(),
			Fatal:   h.Fatal(hc.Event),
		}
	}

	out, closeOut := openHookLog(cfg, log, hc.TicketID)
	defer closeOut()

	log.Info("running hooks", "event", hc.Event, "count", len(specs))
	err := hook.Run(ctx, specs, hc, out, func(_ hook.Spec, warn error) {
		log.Warn("hook failed, continuing", "event", hc.Event, "err", warn)
	})
	if err != nil && ctx.Err() == nil {
		log.Error("hook failed", "event", hc.Event, "err", err)
	}
	return err
}

// runWorktreeCreatedHooks fires the worktree_created event for the worktree the
// caller is about to run a stage in, and reports whether it may proceed. It
// fires for a worktree the caller has just created, and for one an earlier
// pickup left unprepared; a worktree whose hooks have finished is left alone.
//
// A failure under a pause policy removes that worktree before pausing the
// ticket, so a retry creates it again and runs the hook again rather than
// reusing a half-prepared one. When the removal fails — the worktree is dirty,
// or git refuses it — the unprepared marker stays on disk instead, so the retry
// runs the hooks again on the worktree that is still there. A cancelled ticket
// is left to the caller's own cancellation handling.
func (d *Daemon) runWorktreeCreatedHooks(taskCtx context.Context, cfg *config.Config, log *slog.Logger,
	t *ticket.Ticket, filePath string, hc hook.Context, created bool,
) bool {
	hc.Event = config.HookWorktreeCreated
	if len(cfg.HooksFor(hc.RepoPath, hc.Event)) == 0 {
		return true
	}
	if !created {
		if !worktreeUnprepared(cfg, hc.TicketID, hc.Worktree) {
			return true
		}
		log.Warn("worktree left unprepared by an earlier run, running its hooks again", "path", hc.Worktree)
	}

	// The marker is planted before the hooks rather than after they fail: a
	// daemon that dies mid-hook leaves the same half-prepared worktree behind as
	// one whose hook exited non-zero.
	d.markWorktreeUnprepared(cfg, log, hc.TicketID, hc.Worktree)
	hookErr := d.runHooks(taskCtx, cfg, log, hc)
	if hookErr == nil {
		d.clearWorktreeUnprepared(cfg, log, hc.TicketID)
		return true
	}
	if taskCtx.Err() != nil {
		return false
	}

	reason := hookErr.Error()
	err := d.worktrees.RemoveAt(hc.RepoPath, hc.Worktree)
	d.logRemoveResult(log, err)
	if err == nil {
		d.clearWorktreeUnprepared(cfg, log, hc.TicketID)
	} else {
		reason += fmt.Sprintf(" (worktree kept at %s: %s; its hooks run again on the next pickup)", hc.Worktree, err)
	}
	d.pauseTicket(t, filePath, reason)
	return false
}

// runStageEndHooksAfterFailure pairs the stage_start hooks of a run that ends
// without reaching handleAgentExit — a runner failure, or an agent error behind
// a clean exit. Without it a stage_start hook that starts something has nothing
// to stop it on exactly the paths where the ticket is left paused.
//
// The ticket is already being paused for the run's own failure, so a failure
// here only reaches the log: it cannot make the outcome worse, and it must not
// replace the reason the run failed.
func (d *Daemon) runStageEndHooksAfterFailure(taskCtx context.Context, p spawnAgentParams, attempt agentAttempt) {
	if p.annotation || taskCtx.Err() != nil {
		return
	}
	exitCode := attempt.run.Result.ExitCode
	hookErr := d.runHooks(taskCtx, p.cfg, p.log, hook.Context{
		Event:      config.HookStageEnd,
		TicketID:   p.ticketID,
		TicketFile: p.filePath,
		Worktree:   p.wtPath,
		RepoPath:   p.repoPath,
		Branch:     p.branch,
		Stage:      p.stageName,
		Agent:      p.agentName,
		ExitCode:   &exitCode,
	})
	if hookErr != nil {
		p.log.Warn("stage_end hooks failed after a failed run", "stage", p.stageName, "err", hookErr)
	}
}

// hookLogPath is where every hook of a ticket appends its output. It sits in a
// directory of its own rather than beside the stage logs: matchFailurePatterns
// scans a stage log for the agent's failure regexes, and a stage named "hooks"
// would otherwise write to the same file hook output goes to.
func hookLogPath(cfg *config.Config, ticketID string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), ticketID, "hooks", "hooks.log")
}

// openHookLog returns the writer hook output is appended to and a function that
// closes it. A log that cannot be opened costs the output, not the run.
func openHookLog(cfg *config.Config, log *slog.Logger, ticketID string) (io.Writer, func()) {
	path := hookLogPath(cfg, ticketID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Warn("hook log directory unavailable", "path", path, "err", err)
		return io.Discard, func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Warn("hook log unavailable", "path", path, "err", err)
		return io.Discard, func() {}
	}
	return f, func() { _ = f.Close() }
}

// unpreparedMarkerPath names the worktree whose worktree_created hooks have not
// finished. It holds the worktree path, so a marker left over from a worktree
// that has since been replaced does not make the new one look unprepared.
//
// It lives in logs_dir next to the resume records for the same reason those do:
// it points at a machine-local directory, and it must survive a daemon restart.
func unpreparedMarkerPath(cfg *config.Config, ticketID string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), ticketID, "worktree.unprepared")
}

// worktreeUnprepared reports whether wtPath is the worktree an earlier run left
// with its worktree_created hooks unfinished.
func worktreeUnprepared(cfg *config.Config, ticketID, wtPath string) bool {
	data, err := os.ReadFile(unpreparedMarkerPath(cfg, ticketID))
	return err == nil && strings.TrimSpace(string(data)) == wtPath
}

// markWorktreeUnprepared records that wtPath is being prepared. A marker that
// cannot be written costs the retry contract, not the run, so it is logged and
// the hooks go ahead.
func (d *Daemon) markWorktreeUnprepared(cfg *config.Config, log *slog.Logger, ticketID, wtPath string) {
	path := unpreparedMarkerPath(cfg, ticketID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Warn("unprepared marker not written", "path", path, "err", err)
		return
	}
	if err := os.WriteFile(path, []byte(wtPath), 0o644); err != nil {
		log.Warn("unprepared marker not written", "path", path, "err", err)
	}
}

func (d *Daemon) clearWorktreeUnprepared(cfg *config.Config, log *slog.Logger, ticketID string) {
	path := unpreparedMarkerPath(cfg, ticketID)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("unprepared marker not cleared", "path", path, "err", err)
	}
}
