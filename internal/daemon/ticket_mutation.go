package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// Lock order is the board mutation lock, routing admission, then d.mu.
// Callbacks must use the scoped repository, not public daemon methods.
func (d *Daemon) withTicketMutation(fn func(app.MutationRepository) error) error {
	return d.withTicketMutationOrigin(notify.OriginRequest, fn)
}

func (d *Daemon) withTicketMutationOrigin(origin notify.Origin, fn func(app.MutationRepository) error) error {
	dir := expandTilde(d.config().TicketsDir)
	return ticket.WithOfflineWrite(dir, func() error {
		d.mu.Lock()
		defer d.mu.Unlock()
		return fn(&daemonMutationRepository{d: d, disk: store.NewDiskRepo(dir), dir: dir, origin: origin})
	})
}

type daemonMutationRepository struct {
	d      *Daemon
	disk   *store.DiskRepo
	dir    string
	origin notify.Origin
}

func (r *daemonMutationRepository) Resolve(id string) (string, error) {
	if _, err := r.Get(id); err != nil {
		return "", err
	}
	return id, nil
}

func (r *daemonMutationRepository) Get(id string) (*app.StoredTicket, error) {
	st, err := r.disk.Get(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, app.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !ticket.IsCanonicalPath(st.FilePath, st.Ticket.ID) {
		return nil, fmt.Errorf("ticket id does not match destination")
	}
	return st, nil
}

func (r *daemonMutationRepository) List() ([]*app.StoredTicket, error) {
	return r.disk.List()
}

func (r *daemonMutationRepository) Save(st *app.StoredTicket) error {
	if st == nil || st.Ticket == nil {
		return fmt.Errorf("missing ticket")
	}
	if err := ticket.RejectImportedOwnership(st.Ticket); err != nil {
		return err
	}
	if filepath.Clean(filepath.Dir(st.FilePath)) != filepath.Clean(r.dir) || !ticket.IsCanonicalPath(st.FilePath, st.Ticket.ID) {
		return fmt.Errorf("ticket path outside repository")
	}
	current, err := r.Get(st.Ticket.ID)
	if err != nil {
		return err
	}
	if err := ticket.RejectImportedOwnership(current.Ticket); err != nil {
		return err
	}
	if cached, ok := r.d.tickets[st.Ticket.ID]; ok && filepath.Clean(cached.filePath) != filepath.Clean(st.FilePath) {
		return fmt.Errorf("ticket path changed")
	}
	data, err := st.Ticket.Marshal()
	if err != nil {
		return err
	}
	t, err := ticket.ParseBytes(data)
	if err != nil {
		return err
	}
	if t.ID != st.Ticket.ID {
		return fmt.Errorf("ticket id does not match destination")
	}
	if err := ticket.RejectImportedOwnership(t); err != nil {
		return err
	}
	modTime, err := r.d.writeOrdinaryTicketFile(t, st.FilePath, r.origin)
	if err != nil {
		return err
	}
	var previous *ticket.Ticket
	if cached, ok := r.d.tickets[t.ID]; ok {
		previous = cached.ticket
	}
	r.d.tickets[t.ID] = &ticketState{ticket: t, filePath: st.FilePath, modTime: modTime}
	r.d.reconcileDependenciesLocked(t.ID)
	if t.Kind == ticket.KindEpic {
		r.d.rederiveEpicLocked(t.ID, "")
	} else {
		r.d.rederiveEpicMoveLocked(previous, t)
	}
	return nil
}

func (d *Daemon) mutateTicket(id string, mutate func(*ticket.Ticket) error) error {
	if err := d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := repo.Get(id)
		if err != nil {
			return err
		}
		if err := mutate(st.Ticket); err != nil {
			return err
		}
		return repo.Save(st)
	}); err != nil {
		return mapAppError(err)
	}
	d.broadcastTicketUpdateLocking(id)
	return nil
}
