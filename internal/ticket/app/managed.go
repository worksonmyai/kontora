package app

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/ticket"
)

type ManagedMutation struct {
	ID       string
	Expected ticket.ManagedOwnership
	Kind     string
	Fields   map[string]any
	Body     *string
	Note     *ticket.AddNoteOptions
}

type ManagedRepository interface {
	MutateManaged(ManagedMutation) (*StoredTicket, error)
}

func (s *Service) MutateManaged(req ManagedMutation) (*StoredTicket, error) {
	repo, ok := s.repo.(ManagedRepository)
	if !ok {
		return nil, ticket.ErrManaged
	}
	return repo.MutateManaged(req)
}

func (r detachedRepository) MutateManaged(req ManagedMutation) (*StoredTicket, error) {
	repo, ok := r.Repository.(ManagedRepository)
	if !ok {
		return nil, ticket.ErrManaged
	}
	st, err := repo.MutateManaged(req)
	if err != nil {
		return nil, err
	}
	return detached(st), nil
}

// ApplyManagedPatch accepts only the fields whose revision class is explicit.
// Ownership and execution identity always come from durable control state.
func ApplyManagedPatch(current *ticket.Ticket, req ManagedMutation) (*ticket.Ticket, error) {
	out := current.Clone()
	var allowed map[string]bool
	switch req.Kind {
	case "ownership":
		allowed = map[string]bool{}
		if req.Body != nil || req.Note != nil {
			return nil, fmt.Errorf("ownership projection cannot change ticket content")
		}
	case "document":
		allowed = map[string]bool{"summary": true, "final_summary": true}
		if req.Body != nil {
			return nil, fmt.Errorf("body changes require a specification mutation")
		}
	case "specification":
		allowed = map[string]bool{"pipeline": true, "agent": true, "base_branch": true}
	case "attempt":
		allowed = map[string]bool{"status": true, "stage": true, "attempt": true, "started_at": true, "completed_at": true, "history": true, "last_error": true, "last_log": true, "summary": true}
		if req.Body != nil || req.Note != nil {
			return nil, fmt.Errorf("attempt mutation cannot replace the specification or add notes")
		}
	default:
		return nil, fmt.Errorf("unknown managed mutation kind %q", req.Kind)
	}
	for key, value := range req.Fields {
		if !allowed[key] {
			return nil, fmt.Errorf("field %q is not permitted in a %s mutation", key, req.Kind)
		}
		if err := out.SetField(key, value); err != nil {
			return nil, err
		}
	}
	if req.Body != nil {
		out.SetBody(*req.Body)
	}
	if req.Note != nil {
		if _, err := out.AddNote(*req.Note); err != nil {
			return nil, err
		}
	}
	return out, nil
}
