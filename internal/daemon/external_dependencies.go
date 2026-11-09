package daemon

import (
	"errors"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

type externalDependency struct {
	ticket *ticket.Ticket
	source string
}

// Primary namesakes take precedence; duplicate secondary IDs cannot identify a prerequisite.
// Must be called with d.mu held.
func (d *Daemon) dependencyLocked(id string) (*ticket.Ticket, string) {
	if ts := d.tickets[id]; ts != nil {
		return ts.ticket, ""
	}
	dep := d.externalDependencies[id]
	return dep.ticket, dep.source
}

func (d *Daemon) reconcileSecondaryDependencies() {
	d.mu.Lock()
	defer d.mu.Unlock()

	next := make(map[string]externalDependency)
	ambiguous := make(map[string]bool)
	d.secondaryMu.Lock()
	for source, catalog := range d.secondary {
		if !catalog.source.Available {
			continue
		}
		for id, ts := range catalog.tickets {
			if _, exists := next[id]; exists || ambiguous[id] {
				delete(next, id)
				ambiguous[id] = true
				continue
			}
			// External prerequisites are leaves: their relations belong to their own folder.
			next[id] = externalDependency{
				ticket: &ticket.Ticket{ID: id, Status: ts.ticket.Status, Body: "# " + ts.ticket.Title() + "\n"},
				source: source,
			}
		}
	}
	d.secondaryMu.Unlock()

	old := d.externalDependencies
	d.externalDependencies = next
	changed := make(map[string]bool)
	for id := range old {
		changed[id] = true
	}
	for id := range next {
		changed[id] = true
	}
	for id := range changed {
		before, after := old[id], next[id]
		if d.tickets[id] != nil || sameExternalDependency(before, after) {
			continue
		}
		d.reconcileDependenciesLocked(id)
		for _, dependent := range d.dependentsLocked(id) {
			d.broadcastTicketUpdate(dependent)
		}
	}
}

// Admission re-reads secondary files so watcher lag cannot authorize a run.
func secondaryDependencyResolved(cfg *config.Config, id string) bool {
	sources, err := cfg.SecondarySources()
	if err != nil {
		return false
	}
	var found *ticket.Ticket
	for _, source := range sources {
		st, err := secondaryStored(source.Path, id)
		if errors.Is(err, web.ErrTicketNotFound) {
			continue
		}
		if err != nil || found != nil {
			return false
		}
		found = st.Ticket
	}
	return found != nil && ticket.IsDependencyResolved(found.Status)
}

func sameExternalDependency(a, b externalDependency) bool {
	if a.ticket == nil || b.ticket == nil {
		return a.ticket == b.ticket
	}
	return a.source == b.source && a.ticket.Status == b.ticket.Status && a.ticket.Body == b.ticket.Body
}
