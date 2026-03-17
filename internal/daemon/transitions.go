package daemon

import (
	"fmt"
	"time"

	"github.com/worksonmyai/kontora/internal/ticket"
)

// applyTicketSnapshot reconciles a freshly parsed ticket file with daemon
// state. This is used both for watcher-driven file changes and for daemon
// writes that intentionally bypass the watcher via self-write suppression.
func (d *Daemon) applyTicketSnapshot(path string, t *ticket.Ticket) {
	log := d.ticketLog(t.ID)

	var (
		cancel       func()
		cancelReason string
		killWindow   bool
		cleanup      *ticket.Ticket
	)

	d.mu.Lock()
	prev, known := d.tickets[t.ID]
	ns := &ticketState{ticket: t, filePath: path}
	// Preserve lastError when the ticket stays paused (e.g. user edited notes
	// but didn't retry). Rebuilding from disk would otherwise lose it since
	// lastError is in-memory only.
	if known && prev.lastError != "" && prev.ticket.Status == ticket.StatusPaused && t.Status == ticket.StatusPaused {
		ns.lastError = prev.lastError
	}
	d.tickets[t.ID] = ns

	if t.Status != ticket.StatusTodo || !t.Kontora {
		d.removeQueuedLocked(t.ID)
	}

	if t.Kontora {
		switch t.Status { //nolint:exhaustive
		case ticket.StatusTodo:
			shouldQueue := !known || prev.ticket.Status != ticket.StatusTodo || (!d.queued[t.ID] && d.running[t.ID] == nil)
			if shouldQueue {
				if runningCancel, ok := d.running[t.ID]; ok {
					cancel = runningCancel
					cancelReason = "status changed to todo"
				}
				d.removeQueuedLocked(t.ID)
				d.enqueue(t)
				killWindow = true
				if !known {
					log.Info("new ticket", "pipeline", t.Pipeline)
				} else {
					log.Info("enqueuing", "previous_status", string(prev.ticket.Status), "pipeline", t.Pipeline, "role", t.Role)
				}
			}

		case ticket.StatusPaused:
			if runningCancel, ok := d.running[t.ID]; ok {
				cancel = runningCancel
				cancelReason = "user set paused"
			}

		case ticket.StatusOpen:
			if runningCancel, ok := d.running[t.ID]; ok {
				cancel = runningCancel
				cancelReason = "status changed to open"
			}
			killWindow = known && prev.ticket.Status != t.Status

		case ticket.StatusCancelled:
			if runningCancel, ok := d.running[t.ID]; ok {
				cancel = runningCancel
				cancelReason = "user set cancelled"
			}
			killWindow = true
			cleanup = t

		case ticket.StatusDone:
			if runningCancel, ok := d.running[t.ID]; ok {
				cancel = runningCancel
				cancelReason = "status changed to done"
			}
			killWindow = true
			cleanup = t
		}
	}

	d.broadcastTicketUpdate(t.ID)
	d.mu.Unlock()

	if cancel != nil {
		log.Info("killing agent", "reason", cancelReason)
		cancel()
	}
	if killWindow {
		d.killTaskWindow(t.ID)
	}
	if cleanup != nil {
		go d.cleanupWorktree(log, cleanup)
	}
}

func (d *Daemon) writeAndApplyTicket(t *ticket.Ticket, path string) error {
	if err := d.writeTicket(t, path); err != nil {
		return err
	}
	d.applyTicketSnapshot(path, t)
	return nil
}

func resetForQueue(t *ticket.Ticket) error {
	if err := t.SetField("attempt", 0); err != nil {
		return fmt.Errorf("setting attempt: %w", err)
	}
	if err := t.SetField("status", string(ticket.StatusTodo)); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}
	if err := t.SetField("started_at", nil); err != nil {
		return fmt.Errorf("clearing started_at: %w", err)
	}
	if err := t.SetField("completed_at", nil); err != nil {
		return fmt.Errorf("clearing completed_at: %w", err)
	}
	return nil
}

func resetForOpen(t *ticket.Ticket) error {
	if err := t.SetField("status", string(ticket.StatusOpen)); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}
	if err := t.SetField("attempt", 0); err != nil {
		return fmt.Errorf("setting attempt: %w", err)
	}
	if err := t.SetField("started_at", nil); err != nil {
		return fmt.Errorf("clearing started_at: %w", err)
	}
	if err := t.SetField("completed_at", nil); err != nil {
		return fmt.Errorf("clearing completed_at: %w", err)
	}
	return nil
}

func markDone(t *ticket.Ticket, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if err := t.SetField("status", string(ticket.StatusDone)); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}
	if err := t.SetField("completed_at", at); err != nil {
		return fmt.Errorf("setting completed_at: %w", err)
	}
	return nil
}

func markCancelled(t *ticket.Ticket) error {
	if err := t.SetField("status", string(ticket.StatusCancelled)); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}
	return nil
}
