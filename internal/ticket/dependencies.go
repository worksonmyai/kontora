package ticket

import "slices"

// resolvedDependencyStatuses are the statuses that release the tickets which
// depend on a ticket. Human review is in the set because the work itself is
// finished: whatever the reviewer decides, the dependents can be worked on
// against what it produced, and holding them until someone signs off serialises
// the chain on a person. Legacy closed is in the set for reading only; kontora
// never writes it.
var resolvedDependencyStatuses = map[Status]bool{
	StatusHumanReview:  true,
	StatusDone:         true,
	StatusCancelled:    true,
	StatusArchived:     true,
	StatusLegacyClosed: true,
}

// IsDependencyResolved reports whether a ticket in this status stops blocking
// the tickets that depend on it. Every other status, including a custom one,
// blocks.
func IsDependencyResolved(s Status) bool { return resolvedDependencyStatuses[s] }

// Index keys tickets by ID for the dependency functions below. A later ticket
// with an ID an earlier one already used is dropped, so a caller that passes
// duplicates gets the first.
func Index(tickets []*Ticket) map[string]*Ticket {
	index := make(map[string]*Ticket, len(tickets))
	for _, t := range tickets {
		if t == nil || t.ID == "" {
			continue
		}
		if _, ok := index[t.ID]; !ok {
			index[t.ID] = t
		}
	}
	return index
}

// Readiness classifies one ticket against the dependency graph. It is derived
// on every read and never written to a ticket file.
type Readiness struct {
	// Ready is true when no dependency blocks the ticket.
	Ready bool
	// Blockers are the dependency IDs that do not resolve, in the order the
	// ticket lists them.
	Blockers []string
	// Missing are the blockers that name no ticket in the index. They are a
	// subset of Blockers: an ID nothing on disk answers blocks, because there
	// is no way to tell whether that work is finished.
	Missing []string
}

// Classify reports whether a ticket's dependencies are all resolved, and which
// ones are not. It looks at deps alone: whether the ticket is a kontora ticket,
// and whether its own status makes it a scheduling candidate, is the caller's
// question.
func Classify(t *Ticket, index map[string]*Ticket) Readiness {
	r := Readiness{Ready: true}
	for _, dep := range t.Deps {
		if dep == "" {
			continue
		}
		// A self-dependency can only ever block, and naming the ticket its own
		// blocker is how a hand-edited file reports it.
		dt, ok := index[dep]
		if !ok {
			r.Ready = false
			r.Blockers = append(r.Blockers, dep)
			r.Missing = append(r.Missing, dep)
			continue
		}
		if !IsDependencyResolved(dt.Status) {
			r.Ready = false
			r.Blockers = append(r.Blockers, dep)
		}
	}
	return r
}

// DependencyPath returns the chain of IDs from `from` to `to` along deps edges,
// starting at from and ending at to. It returns nil when to is not reachable,
// and a one-element path when from and to are the same ID.
func DependencyPath(index map[string]*Ticket, from, to string) []string {
	if from == to {
		return []string{from}
	}
	visited := map[string]bool{}
	var walk func(id string) []string
	walk = func(id string) []string {
		if visited[id] {
			return nil
		}
		visited[id] = true
		t, ok := index[id]
		if !ok {
			return nil
		}
		for _, dep := range t.Deps {
			if dep == to {
				return []string{id, to}
			}
			if rest := walk(dep); rest != nil {
				return append([]string{id}, rest...)
			}
		}
		return nil
	}
	return walk(from)
}

// Cycle returns the dependency cycle a ticket sits on, as the chain of IDs from
// the ticket back to itself, or nil when it sits on none.
func Cycle(index map[string]*Ticket, id string) []string {
	t, ok := index[id]
	if !ok {
		return nil
	}
	for _, dep := range t.Deps {
		if dep == id {
			return []string{id, id}
		}
		if path := DependencyPath(index, dep, id); path != nil {
			return append([]string{id}, path...)
		}
	}
	return nil
}

// Dependents returns the IDs of the tickets whose deps name id, sorted.
func Dependents(index map[string]*Ticket, id string) []string {
	var ids []string
	for other, t := range index {
		if slices.Contains(t.Deps, id) {
			ids = append(ids, other)
		}
	}
	slices.Sort(ids)
	return ids
}
