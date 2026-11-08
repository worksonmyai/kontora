package daemon

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// Callers hold d.mu. Pending destinations are not ordinary tickets until recovery commits them.
func (d *Daemon) checkTransferAdmissionLocked(id string) error {
	if id == "" {
		return nil
	}
	if d.transferBlocked[id] {
		return fmt.Errorf("%w: ticket %s has an unfinished transfer", app.ErrInvalidState, id)
	}
	return d.checkPendingTransfer(id)
}

func (d *Daemon) checkPendingTransfer(id string) error {
	pending, err := store.HasPendingTransfer(expandTilde(d.config().TicketsDir), id)
	if err != nil {
		return fmt.Errorf("%w: checking transfer for %s: %v", app.ErrInvalidState, id, err)
	}
	if pending {
		return fmt.Errorf("%w: ticket %s has an unfinished transfer", app.ErrInvalidState, id)
	}
	return nil
}
