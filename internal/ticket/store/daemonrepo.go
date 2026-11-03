package store

import (
	"fmt"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

// DaemonRepoCallbacks provides the daemon-specific operations that a
// DaemonRepo delegates to. The daemon constructs these with closures that
// handle mutex locking and internal state coordination.
type DaemonRepoCallbacks struct {
	ManagedMutation func(app.ManagedMutation) (*app.StoredTicket, error)
	// PathLookup returns the file path for a ticket ID.
	// Implementations are responsible for their own synchronization.
	PathLookup func(id string) (string, error)

	// WriteTicket writes a ticket to disk (handles recordSelfWrite).
	WriteTicket func(t *ticket.Ticket, path string) error

	// AfterSave is called after a successful write to update daemon
	// in-memory state with the mutated ticket.
	AfterSave func(id string, st *app.StoredTicket)

	// ListTickets returns detached snapshots of all tracked tickets.
	// Implementations must clone tickets while holding their cache mutex.
	ListTickets func() []*app.StoredTicket
}

// DaemonRepo implements Repository backed by daemon in-memory state,
// with disk re-reads for mutation safety.
type DaemonRepo struct {
	cb DaemonRepoCallbacks
}

// NewDaemonRepo creates a DaemonRepo with the given callbacks.
func NewDaemonRepo(cb DaemonRepoCallbacks) *DaemonRepo {
	return &DaemonRepo{cb: cb}
}

func (r *DaemonRepo) MutateManaged(req app.ManagedMutation) (*app.StoredTicket, error) {
	if r.cb.ManagedMutation == nil {
		return nil, ticket.ErrManaged
	}
	return r.cb.ManagedMutation(req)
}

func (r *DaemonRepo) Resolve(idOrPrefix string) (string, error) {
	_, err := r.cb.PathLookup(idOrPrefix)
	if err != nil {
		return "", err
	}
	return idOrPrefix, nil
}

func (r *DaemonRepo) Get(id string) (*app.StoredTicket, error) {
	path, err := r.cb.PathLookup(id)
	if err != nil {
		return nil, err
	}
	t, err := ticket.ParseFile(path)
	if err != nil {
		return nil, fmt.Errorf("re-reading ticket %s: %w", id, err)
	}
	return &app.StoredTicket{Ticket: t, FilePath: path}, nil
}

func (r *DaemonRepo) List() ([]*app.StoredTicket, error) {
	return r.cb.ListTickets(), nil
}

func (r *DaemonRepo) Save(st *app.StoredTicket) error {
	return ticket.WithOfflineWrite(filepath.Dir(st.FilePath), func() error {
		if err := ticket.RejectImportedOwnership(st.Ticket); err != nil {
			return err
		}
		path, err := r.cb.PathLookup(st.Ticket.ID)
		if err != nil {
			return err
		}
		if filepath.Clean(path) != filepath.Clean(st.FilePath) {
			return fmt.Errorf("ticket path changed")
		}
		cloned := *st
		cloned.Ticket = st.Ticket.Clone()
		if err := r.cb.WriteTicket(cloned.Ticket, cloned.FilePath); err != nil {
			return err
		}
		r.cb.AfterSave(cloned.Ticket.ID, &cloned)
		return nil
	})
}
