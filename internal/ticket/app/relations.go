package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/ticket"
)

// ErrRelationCycle is returned when a dependency would close a cycle.
var ErrRelationCycle = errors.New("dependency cycle")

// ErrSelfRelation is returned when both ends of a relation are the same ticket.
var ErrSelfRelation = errors.New("self relation")

// resolveRelation resolves both ends of a relation and rejects a ticket related
// to itself. Every participant is resolved before any file is written, so a
// rejected call leaves the store untouched.
func (s *Service) resolveRelation(ticketID string, relatedIDs []string) (string, []string, error) {
	if len(relatedIDs) == 0 {
		return "", nil, fmt.Errorf("at least one related ticket id is required")
	}
	resolved, err := s.repo.Resolve(ticketID)
	if err != nil {
		return "", nil, err
	}
	var related []string
	for _, id := range relatedIDs {
		other, err := s.repo.Resolve(id)
		if err != nil {
			return "", nil, err
		}
		if other == resolved {
			return "", nil, fmt.Errorf("%w: ticket %s cannot be related to itself", ErrSelfRelation, resolved)
		}
		if !slices.Contains(related, other) {
			related = append(related, other)
		}
	}
	return resolved, related, nil
}

// index builds the id-keyed ticket set the dependency functions read. The
// tickets in it are the store's own, so callers must not mutate them.
func (s *Service) index() (map[string]*ticket.Ticket, error) {
	stored, err := s.repo.List()
	if err != nil {
		return nil, err
	}
	tickets := make([]*ticket.Ticket, 0, len(stored))
	for _, st := range stored {
		tickets = append(tickets, st.Ticket)
	}
	return ticket.Index(tickets), nil
}

// AddDependency records that ticketID waits on dependencyID. It writes only the
// dependent ticket: the reverse edge is derived, not stored. Adding an edge the
// ticket already has succeeds without a second entry.
func (s *Service) AddDependency(ticketID, dependencyID string) (RelationResult, error) {
	resolved, related, err := s.resolveRelation(ticketID, []string{dependencyID})
	if err != nil {
		return RelationResult{}, err
	}
	depID := related[0]

	st, err := s.repo.Get(resolved)
	if err != nil {
		return RelationResult{}, err
	}
	if slices.Contains(st.Ticket.Deps, depID) {
		return RelationResult{ID: resolved, Related: related}, nil
	}

	index, err := s.index()
	if err != nil {
		return RelationResult{}, err
	}
	// The new edge points from the dependent to its dependency, so it closes a
	// cycle exactly when the dependency already reaches the dependent.
	if path := ticket.DependencyPath(index, depID, resolved); path != nil {
		return RelationResult{}, fmt.Errorf("%w: %s would close %s", ErrRelationCycle, resolved, strings.Join(append([]string{resolved}, path...), " -> "))
	}

	if err := st.Ticket.SetField("deps", append(slices.Clone(st.Ticket.Deps), depID)); err != nil {
		return RelationResult{}, fmt.Errorf("setting deps: %w", err)
	}
	if err := s.repo.Save(st); err != nil {
		return RelationResult{}, err
	}

	s.runtime.ReconcileDependencies(resolved)
	s.runtime.BroadcastUpdated(resolved)
	return RelationResult{ID: resolved, Related: related, Changed: []string{resolved}}, nil
}

// RemoveDependency drops dependencyID from ticketID's deps. Removing an edge
// that is not there succeeds and writes nothing.
func (s *Service) RemoveDependency(ticketID, dependencyID string) (RelationResult, error) {
	resolved, related, err := s.resolveRelation(ticketID, []string{dependencyID})
	if err != nil {
		return RelationResult{}, err
	}
	depID := related[0]

	st, err := s.repo.Get(resolved)
	if err != nil {
		return RelationResult{}, err
	}
	deps := slices.DeleteFunc(slices.Clone(st.Ticket.Deps), func(id string) bool { return id == depID })
	if len(deps) == len(st.Ticket.Deps) {
		return RelationResult{ID: resolved, Related: related}, nil
	}

	if err := st.Ticket.SetField("deps", deps); err != nil {
		return RelationResult{}, fmt.Errorf("setting deps: %w", err)
	}
	if err := s.repo.Save(st); err != nil {
		return RelationResult{}, err
	}

	s.runtime.ReconcileDependencies(resolved)
	s.runtime.BroadcastUpdated(resolved)
	return RelationResult{ID: resolved, Related: related, Changed: []string{resolved}}, nil
}

// Link records a symmetric "related" edge between ticketID and each of
// relatedIDs. It links the first ticket to each later one and does not link the
// later ones to each other.
func (s *Service) Link(ticketID string, relatedIDs ...string) (RelationResult, error) {
	return s.mutateLinks(ticketID, relatedIDs, true)
}

// Unlink removes the symmetric edge between ticketID and each of relatedIDs.
// Removing an edge that is not there succeeds and writes nothing.
func (s *Service) Unlink(ticketID string, relatedIDs ...string) (RelationResult, error) {
	return s.mutateLinks(ticketID, relatedIDs, false)
}

// mutateLinks adds or removes symmetric link edges. Two markdown files cannot be
// written atomically, so every participant is read and validated first and the
// saves happen one at a time. A failure part-way names the tickets already
// written, and a repeated call repairs whichever side is missing rather than
// stopping at the side that is already correct.
func (s *Service) mutateLinks(ticketID string, relatedIDs []string, add bool) (RelationResult, error) {
	verb := "linking"
	if !add {
		verb = "unlinking"
	}

	resolved, related, err := s.resolveRelation(ticketID, relatedIDs)
	if err != nil {
		return RelationResult{}, err
	}

	stored := make(map[string]*StoredTicket, len(related)+1)
	for _, id := range append([]string{resolved}, related...) {
		st, err := s.repo.Get(id)
		if err != nil {
			return RelationResult{}, err
		}
		stored[id] = st
	}

	changed := map[string]bool{}
	for _, other := range related {
		if applyLink(stored[resolved].Ticket, other, add) {
			changed[resolved] = true
		}
		if applyLink(stored[other].Ticket, resolved, add) {
			changed[other] = true
		}
	}

	// The ticket named on the command line is written first, so a partial
	// failure leaves the edge on the side the caller asked about.
	order := append([]string{resolved}, related...)
	var saved []string
	for _, id := range order {
		if !changed[id] {
			continue
		}
		if err := setLinks(stored[id].Ticket); err != nil {
			return RelationResult{}, err
		}
		if err := s.repo.Save(stored[id]); err != nil {
			return RelationResult{ID: resolved, Related: related, Changed: saved},
				fmt.Errorf("%s %s and %s: saving %s: %w (already changed: %s)",
					verb, resolved, strings.Join(related, ", "), id, err, joinOrNone(saved))
		}
		saved = append(saved, id)
		s.runtime.BroadcastUpdated(id)
	}

	return RelationResult{ID: resolved, Related: related, Changed: saved}, nil
}

// applyLink adds or removes one id in a ticket's in-memory links, reporting
// whether the list changed.
func applyLink(t *ticket.Ticket, other string, add bool) bool {
	if add {
		if slices.Contains(t.Links, other) {
			return false
		}
		t.Links = append(slices.Clone(t.Links), other)
		return true
	}
	links := slices.DeleteFunc(slices.Clone(t.Links), func(id string) bool { return id == other })
	if len(links) == len(t.Links) {
		return false
	}
	t.Links = links
	return true
}

// setLinks writes the in-memory links back into the frontmatter node, which is
// what Marshal encodes.
func setLinks(t *ticket.Ticket) error {
	if err := t.SetField("links", t.Links); err != nil {
		return fmt.Errorf("setting links: %w", err)
	}
	return nil
}

func joinOrNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}
