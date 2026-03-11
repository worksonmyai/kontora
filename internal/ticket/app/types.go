package app

import (
	"github.com/worksonmyai/kontora/internal/ticket"
)

// Repository abstracts ticket storage. Implementations handle disk-based
// and daemon-state-based backends.
type Repository interface {
	Resolve(idOrPrefix string) (string, error)
	Get(id string) (*StoredTicket, error)
	List() ([]*StoredTicket, error)
	Save(st *StoredTicket) error
}

// StoredTicket pairs a parsed ticket with its storage path.
type StoredTicket struct {
	Ticket   *ticket.Ticket
	FilePath string
}

// RuntimeHooks lets the service trigger daemon-specific side effects
// (enqueue, cancel, broadcast) without embedding daemon internals.
type RuntimeHooks interface {
	Enqueue(t *ticket.Ticket)
	Cancel(ticketID string)
	BroadcastUpdated(ticketID string)
	BroadcastDeleted(ticketID string)
}

// NoopRuntime is a RuntimeHooks implementation that does nothing.
// Used by CLI and TUI file mode.
type NoopRuntime struct{}

func (NoopRuntime) Enqueue(*ticket.Ticket)      {}
func (NoopRuntime) Cancel(string)                {}
func (NoopRuntime) BroadcastUpdated(string)      {}
func (NoopRuntime) BroadcastDeleted(string)      {}

// Result is returned by mutation methods.
type Result struct {
	ID     string
	Status string
}

// GetOptions controls what is included in a Get response.
type GetOptions struct {
	IncludeBody bool
}

// ListOptions controls filtering for List.
type ListOptions struct {
	IncludeNonKontora bool
}

// InitRequest carries parameters for initializing a ticket.
type InitRequest struct {
	Pipeline string
	Path     string
	Agent    string
	Status   string // "open" or "todo"; defaults to "todo"
	Role     string // starting stage; if empty, defaults to first pipeline stage
}
