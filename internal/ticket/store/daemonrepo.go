package store

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

// DaemonRepoCallbacks provides the daemon-specific operations that a
// DaemonRepo delegates to. The daemon constructs these with closures that
// handle mutex locking and internal state coordination.
type DaemonRepoCallbacks struct {
	// PathLookup returns the file path for a ticket ID.
	// Implementations are responsible for their own synchronization.
	PathLookup func(id string) (string, error)

	// WriteTicket writes a ticket to disk (handles recordSelfWrite).
	WriteTicket func(t *ticket.Ticket, path string) error

	// AfterSave is called after a successful write to update daemon
	// in-memory state with the mutated ticket.
	AfterSave func(id string, st *app.StoredTicket)

	// ListTickets returns all tracked tickets from daemon state.
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
	if err := r.cb.WriteTicket(st.Ticket, st.FilePath); err != nil {
		return err
	}
	r.cb.AfterSave(st.Ticket.ID, st)
	return nil
}
