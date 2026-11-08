package store

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/worksonmyai/kontora/internal/frontmatter"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"gopkg.in/yaml.v3"
)

var ErrSecondaryLifecycle = errors.New("secondary repository cannot change lifecycle or protected fields")

// Keep the disk repository private so callers cannot bypass restricted Save.
type SecondaryRepo struct {
	disk *DiskRepo
}

var _ app.Repository = (*SecondaryRepo)(nil)
var _ app.MutationRepository = secondaryMutationRepository{}

func NewSecondaryRepo(dir string) *SecondaryRepo {
	return &SecondaryRepo{disk: NewDiskRepo(dir)}
}

func (r *SecondaryRepo) Resolve(idOrPrefix string) (string, error) {
	return r.disk.Resolve(idOrPrefix)
}

func (r *SecondaryRepo) Get(id string) (*app.StoredTicket, error) {
	return r.disk.Get(id)
}

func (r *SecondaryRepo) List() ([]*app.StoredTicket, error) {
	return r.disk.List()
}

func (r *SecondaryRepo) WithMutation(fn func(app.MutationRepository) error) error {
	return r.disk.WithMutation(func(scoped app.MutationRepository) error {
		return fn(secondaryMutationRepository{MutationRepository: scoped})
	})
}

func (r *SecondaryRepo) Save(st *app.StoredTicket) error {
	return r.WithMutation(func(scoped app.MutationRepository) error { return scoped.Save(st) })
}

type secondaryMutationRepository struct {
	app.MutationRepository
}

func (r secondaryMutationRepository) Save(st *app.StoredTicket) error {
	if st == nil || st.Ticket == nil {
		return fmt.Errorf("missing ticket")
	}
	current, err := r.Get(st.Ticket.ID)
	if err != nil {
		return err
	}
	if err := ticket.RejectImportedOwnership(st.Ticket); err != nil {
		return err
	}
	if err := ticket.RejectImportedOwnership(current.Ticket); err != nil {
		return err
	}
	before, err := secondaryProtectedFields(current.Ticket)
	if err != nil {
		return err
	}
	after, err := secondaryProtectedFields(st.Ticket)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, after) {
		return ErrSecondaryLifecycle
	}
	return r.MutationRepository.Save(st)
}

func secondaryProtectedFields(t *ticket.Ticket) ([]byte, error) {
	out, err := t.Marshal()
	if err != nil {
		return nil, err
	}
	fm, _, err := frontmatter.Split(string(out))
	if err != nil {
		return nil, err
	}
	// Decode the serialized document, not the typed Ticket. This includes
	// unknown fields and resolves aliases that reference editable fields.
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(fm), &fields); err != nil {
		return nil, err
	}
	var rawFields map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &rawFields); err != nil {
		return nil, err
	}
	for _, key := range []string{
		"summary", "final_summary", "pipeline", "path", "agent", "branch", "base_branch",
		"notify", "notify_channels", "deps", "links", "parent", "children",
	} {
		delete(fields, key)
		delete(rawFields, key)
	}
	// Keep protected YAML structure as well as resolved alias values. A typed
	// history rewrite must not change tags, comments, or custom field order.
	raw, err := yaml.Marshal(rawFields)
	if err != nil {
		return nil, err
	}
	resolved, err := yaml.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return append(append(raw, 0), resolved...), nil
}
