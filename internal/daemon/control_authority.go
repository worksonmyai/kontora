package daemon

import (
	"errors"
	"fmt"
	"os"

	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// Until authority and integration are installed, a registered board must not enter ordinary recovery.
func discoverControlAuthority(dir string) error {
	authority, err := controlstore.Discover(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("discover orchestrator authority: %w", err)
	}
	control, err := controlstore.OpenBoard(dir, authority)
	if err != nil {
		return fmt.Errorf("load orchestrator authority: %w", err)
	}
	if err := control.Close(); err != nil {
		return err
	}
	return fmt.Errorf("%w: board %s has durable orchestrator authority; managed execution remains disabled pending authority and integration support", ticket.ErrManaged, authority.BoardID)
}

func (d *Daemon) ordinaryMutationAllowed(id string) error {
	if err := ticket.CheckBoardControl(expandTilde(d.config().TicketsDir)); err != nil {
		return fmt.Errorf("%w: %w", web.ErrInvalidState, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkTransferAdmissionLocked(id); err != nil {
		return mapAppError(err)
	}
	if ts, ok := d.tickets[id]; ok && ts.ticket.HasOwnershipMetadata() {
		return fmt.Errorf("%w: %w", web.ErrInvalidState, ticket.ErrManaged)
	}
	return nil
}
