package ticket

import (
	"slices"
	"strings"
)

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

// EpicChildren returns the tickets whose parent names the epic, ordered by the
// epic's children list first and then by created, oldest first. A child the
// list does not name sorts after every child it does; ties break on ID so the
// order is stable.
func EpicChildren(index map[string]*Ticket, epicID string) []*Ticket {
	epic, ok := index[epicID]
	if !ok {
		return nil
	}
	rank := make(map[string]int, len(epic.Children))
	for i, id := range epic.Children {
		if _, seen := rank[id]; !seen {
			rank[id] = i
		}
	}
	var children []*Ticket
	for _, t := range index {
		if t.ID != epicID && t.Parent == epicID {
			children = append(children, t)
		}
	}
	slices.SortFunc(children, func(a, b *Ticket) int {
		ra, oka := rank[a.ID]
		rb, okb := rank[b.ID]
		switch {
		case oka && okb && ra != rb:
			return ra - rb
		case oka != okb:
			if oka {
				return -1
			}
			return 1
		}
		if c := compareCreated(a, b); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return children
}

// compareCreated orders two tickets oldest first. A ticket with no created
// timestamp sorts after one that has it.
func compareCreated(a, b *Ticket) int {
	switch {
	case a.Created == nil && b.Created == nil:
		return 0
	case a.Created == nil:
		return 1
	case b.Created == nil:
		return -1
	}
	return a.Created.Compare(*b.Created)
}

// epicActiveStatuses are the child statuses that make an epic in_progress: the
// work is under way, or waiting on a person who is looking at finished work.
var epicActiveStatuses = map[Status]bool{
	StatusTodo:        true,
	StatusInProgress:  true,
	StatusPaused:      true,
	StatusHumanReview: true,
}

// DeriveEpicStatus computes an epic's status from its children. Archived
// children are ignored entirely: they are off the board and out of the meter.
// An epic with no children reads open, which is also what an epic reads while
// any child is still open.
func DeriveEpicStatus(index map[string]*Ticket, epicID string) Status {
	var done, cancelled, other int
	for _, c := range EpicChildren(index, epicID) {
		switch {
		case c.Status == StatusArchived:
			continue
		case epicActiveStatuses[c.Status]:
			return StatusInProgress
		case c.Status == StatusDone || c.Status == StatusLegacyClosed:
			done++
		case c.Status == StatusCancelled:
			cancelled++
		default:
			other++
		}
	}
	switch {
	case other > 0 || done+cancelled == 0:
		return StatusOpen
	case done > 0:
		return StatusDone
	default:
		return StatusCancelled
	}
}
