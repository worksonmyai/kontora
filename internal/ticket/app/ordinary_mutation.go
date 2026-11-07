package app

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/ticket"
)

// scopedRepository adapts a transaction reader/writer to a call-local Service.
// Nested transactions are a programming error; no shared Service field changes.
type scopedRepository struct{ MutationRepository }

func (scopedRepository) WithMutation(func(MutationRepository) error) error {
	return fmt.Errorf("nested application mutation")
}

// preparationRepository permits the ordinary init code to build a proposal,
// without publishing a write or running any runtime effects.
type preparationRepository struct{ Repository }

func (preparationRepository) Save(*StoredTicket) error { return nil }

func mutate[T any](s *Service, fn func(*Service) (T, error)) (T, error) {
	var result T
	effects := &mutationEffects{}
	err := s.repo.WithMutation(func(repo MutationRepository) error {
		scoped := &Service{cfg: s.cfg, repo: scopedRepository{repo}, runtime: effects}
		var err error
		result, err = fn(scoped)
		return err
	})
	// Successful saves are not rolled back if a later save or scope completion fails.
	for _, effect := range effects.pending {
		effect(s.runtime)
	}
	return result, err
}

type mutationEffects struct{ pending []func(RuntimeHooks) }

func (e *mutationEffects) Enqueue(t *ticket.Ticket) {
	snapshot := t.Clone()
	e.pending = append(e.pending, func(r RuntimeHooks) { r.Enqueue(snapshot) })
}
func (e *mutationEffects) Cancel(id string) {
	e.pending = append(e.pending, func(r RuntimeHooks) { r.Cancel(id) })
}
func (e *mutationEffects) BroadcastUpdated(id string) {
	e.pending = append(e.pending, func(r RuntimeHooks) { r.BroadcastUpdated(id) })
}
func (e *mutationEffects) BroadcastDeleted(id string) {
	e.pending = append(e.pending, func(r RuntimeHooks) { r.BroadcastDeleted(id) })
}
func (e *mutationEffects) ReconcileDependencies(id string) {
	e.pending = append(e.pending, func(r RuntimeHooks) { r.ReconcileDependencies(id) })
}
