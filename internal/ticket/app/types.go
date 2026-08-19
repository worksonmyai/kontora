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
	// ReconcileDependencies is called after a change that can make a ticket, or
	// any ticket depending on it, ready or blocked: a relation write or a status
	// transition. The scheduler re-checks the ticket and its dependents against
	// the dependency graph.
	ReconcileDependencies(ticketID string)
}

// NoopRuntime is a RuntimeHooks implementation that does nothing.
// Used by CLI and TUI file mode.
type NoopRuntime struct{}

func (NoopRuntime) Enqueue(*ticket.Ticket)       {}
func (NoopRuntime) Cancel(string)                {}
func (NoopRuntime) BroadcastUpdated(string)      {}
func (NoopRuntime) BroadcastDeleted(string)      {}
func (NoopRuntime) ReconcileDependencies(string) {}

// Result is returned by mutation methods.
type Result struct {
	ID     string
	Status string
	// Blockers are the unresolved dependency ids of a ticket that reached todo
	// but cannot run yet. Empty means nothing holds it back.
	Blockers []string
}

// RelationResult reports what a relation mutation resolved to and which ticket
// files it changed. A repeated call that had nothing to add reports no changes.
type RelationResult struct {
	ID string
	// Related are the ids on the other side of the edge, resolved from whatever
	// prefixes the caller gave, so a confirmation names the same tickets the
	// mutation touched.
	Related []string
	Changed []string
}

// GetOptions controls what is included in a Get response.
type GetOptions struct {
	IncludeBody bool
}

// ListOptions is reserved for future List response options.
type ListOptions struct{}

// InitRequest carries parameters for initializing a ticket.
type InitRequest struct {
	Pipeline string
	Path     string
	Agent    string
	Branch   string // git branch; if empty, the daemon names one from the ticket ID
	Status   string // "open" or "todo"; defaults to "todo"
	Stage    string // starting stage; if empty, defaults to first pipeline stage
}
