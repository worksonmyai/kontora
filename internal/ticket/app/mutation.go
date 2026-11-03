package app

import (
	"fmt"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/ticket"
)

// detachedRepository keeps rejected edits away from repository-owned pointers.
type detachedRepository struct{ Repository }

func detached(st *StoredTicket) *StoredTicket {
	if st == nil {
		return nil
	}
	out := *st
	out.Ticket = st.Ticket.Clone()
	return &out
}

func (r detachedRepository) Get(id string) (*StoredTicket, error) {
	st, err := r.Repository.Get(id)
	if err != nil {
		return nil, err
	}
	return detached(st), nil
}

func (r detachedRepository) List() ([]*StoredTicket, error) {
	stored, err := r.Repository.List()
	if err != nil {
		return nil, err
	}
	out := make([]*StoredTicket, len(stored))
	for i, st := range stored {
		out[i] = detached(st)
	}
	return out, nil
}

func ordinary(st *StoredTicket) error {
	if st.Ticket.HasOwnershipMetadata() {
		return fmt.Errorf("%w: %s", ticket.ErrManaged, st.Ticket.ID)
	}
	if st.FilePath != "" {
		return ticket.CheckBoardControl(filepath.Dir(st.FilePath))
	}
	return nil
}

func (r detachedRepository) Save(st *StoredTicket) error {
	if err := ordinary(st); err != nil {
		return err
	}
	current, err := r.Repository.Get(st.Ticket.ID)
	if err != nil {
		return err
	}
	if err := ordinary(current); err != nil {
		return err
	}
	return r.Repository.Save(detached(st))
}

func (s *Service) mutationTicket(id string) (*StoredTicket, error) {
	st, err := s.repo.Get(id)
	if err != nil {
		return nil, err
	}
	if err := ordinary(st); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Service) ordinaryParticipants(ids ...string) error {
	for _, id := range ids {
		if id == "" {
			continue
		}
		st, err := s.repo.Get(id)
		if err != nil {
			return err
		}
		if err := ordinary(st); err != nil {
			return err
		}
	}
	return nil
}
