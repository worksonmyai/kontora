package app

import (
	"fmt"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/ticket"
)

// detachedRepository keeps rejected edits away from repository-owned pointers.
type detachedRepository struct{ Repository }

type detachedMutationRepository struct{ MutationRepository }

// WithMutation must wrap its scoped repository so validation reads and saves stay in the transaction.
func (r detachedRepository) WithMutation(fn func(MutationRepository) error) error {
	return r.Repository.WithMutation(func(scoped MutationRepository) error {
		return fn(detachedMutationRepository{scoped})
	})
}

func (r detachedRepository) Get(id string) (*StoredTicket, error) {
	return (detachedMutationRepository{r.Repository}).Get(id)
}

func (r detachedRepository) List() ([]*StoredTicket, error) {
	return (detachedMutationRepository{r.Repository}).List()
}

func (r detachedRepository) Save(st *StoredTicket) error {
	return (detachedMutationRepository{r.Repository}).Save(st)
}

func detached(st *StoredTicket) *StoredTicket {
	if st == nil {
		return nil
	}
	out := *st
	out.Ticket = st.Ticket.Clone()
	return &out
}

func (r detachedMutationRepository) Get(id string) (*StoredTicket, error) {
	st, err := r.MutationRepository.Get(id)
	if err != nil {
		return nil, err
	}
	return detached(st), nil
}

func (r detachedMutationRepository) List() ([]*StoredTicket, error) {
	stored, err := r.MutationRepository.List()
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
	if st == nil || st.Ticket == nil {
		return fmt.Errorf("missing ticket")
	}
	if st.Ticket.HasOwnershipMetadata() {
		return fmt.Errorf("%w: %s", ticket.ErrManaged, st.Ticket.ID)
	}
	if st.FilePath != "" {
		return ticket.CheckBoardControl(filepath.Dir(st.FilePath))
	}
	return nil
}

func (r detachedMutationRepository) Save(st *StoredTicket) error {
	if err := ordinary(st); err != nil {
		return err
	}
	current, err := r.MutationRepository.Get(st.Ticket.ID)
	if err != nil {
		return err
	}
	if err := ordinary(current); err != nil {
		return err
	}
	return r.MutationRepository.Save(detached(st))
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
