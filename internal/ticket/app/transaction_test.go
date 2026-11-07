package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

type transactionTestRepository struct {
	MutationRepository
	withMutation func(func(MutationRepository) error) error
}

func (r transactionTestRepository) WithMutation(fn func(MutationRepository) error) error {
	return r.withMutation(fn)
}

func TestDetachedRepositoryWithMutationUsesDetachedScope(t *testing.T) {
	outer := newMemRepo()
	outer.add("outside", "---\nid: outside\n---\n# Outside\n")
	outer.tickets["outside"].FilePath = ""
	inner := newMemRepo()
	inner.add("inside", "---\nid: inside\ndeps: [original]\ncustom: [preserved]\n---\n# Inside\n")
	inner.tickets["inside"].FilePath = ""
	original := inner.tickets["inside"].Ticket
	calls := 0
	r := detachedRepository{transactionTestRepository{
		MutationRepository: outer,
		withMutation: func(fn func(MutationRepository) error) error {
			calls++
			return fn(sharedPointerRepo{inner})
		},
	}}
	err := r.WithMutation(func(scoped MutationRepository) error {
		id, err := scoped.Resolve("ins")
		require.NoError(t, err)
		require.Equal(t, "inside", id)
		st, err := scoped.Get(id)
		require.NoError(t, err)
		require.NotSame(t, original, st.Ticket)
		require.NoError(t, st.Ticket.SetField("deps", []string{"changed"}))
		require.NoError(t, st.Ticket.SetField("custom", []string{"changed"}))
		require.Equal(t, []string{"original"}, original.Deps)
		before, err := original.Marshal()
		require.NoError(t, err)
		require.Contains(t, string(before), "preserved")

		listed, err := scoped.List()
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.NotSame(t, original, listed[0].Ticket)
		require.NoError(t, listed[0].Ticket.SetField("deps", []string{"listed"}))
		require.Equal(t, []string{"original"}, original.Deps)

		// The outer adapter must not be rebound to the scoped repository.
		_, err = r.Get("outside")
		require.NoError(t, err)
		require.NoError(t, scoped.Save(st))
		require.NotSame(t, st, inner.tickets[id])
		require.NotSame(t, st.Ticket, inner.tickets[id].Ticket)
		require.NoError(t, st.Ticket.SetField("deps", []string{"after save"}))
		fresh, err := scoped.Get(id)
		require.NoError(t, err)
		require.Equal(t, []string{"changed"}, fresh.Ticket.Deps)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, []string{"inside"}, inner.saved)
	require.Empty(t, outer.saved)
}

func TestDetachedRepositoryWithMutationRetainsOrdinaryChecks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *memRepo, *StoredTicket)
	}{
		{"incoming ownership", func(t *testing.T, _ *memRepo, st *StoredTicket) {
			require.NoError(t, st.Ticket.SetField("managed", map[string]any{}))
		}},
		{"current ownership", func(t *testing.T, repo *memRepo, _ *StoredTicket) {
			require.NoError(t, repo.tickets["test"].Ticket.SetField("managed", map[string]any{}))
		}},
		{"current null ownership marker", func(t *testing.T, repo *memRepo, _ *StoredTicket) {
			parsed, err := ticket.ParseBytes([]byte("---\nid: test\nmanaged: null\n---\n# Managed\n"))
			require.NoError(t, err)
			repo.tickets["test"].Ticket = parsed
		}},
		{"incoming board control", func(t *testing.T, _ *memRepo, st *StoredTicket) {
			st.FilePath = controlledTicketPath(t)
		}},
		{"current board control", func(t *testing.T, repo *memRepo, _ *StoredTicket) {
			repo.tickets["test"].FilePath = controlledTicketPath(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := newMemRepo()
			inner.add("test", "---\nid: test\n---\n# Test\n")
			inner.tickets["test"].FilePath = ""
			st, err := inner.Get("test")
			require.NoError(t, err)
			tc.setup(t, inner, st)
			r := detachedRepository{transactionTestRepository{
				MutationRepository: newMemRepo(),
				withMutation: func(fn func(MutationRepository) error) error {
					return fn(sharedPointerRepo{inner})
				},
			}}
			err = r.WithMutation(func(scoped MutationRepository) error { return scoped.Save(st) })
			require.ErrorIs(t, err, ticket.ErrManaged)
			require.Empty(t, inner.saved)
		})
	}
}

func controlledTicketPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".kontora"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora", "authority.json"), []byte("{}"), 0o600))
	return filepath.Join(dir, "test.md")
}

func TestDetachedRepositoryWithMutationPropagatesErrors(t *testing.T) {
	failure := errors.New("transaction failed")
	r := detachedRepository{transactionTestRepository{
		withMutation: func(func(MutationRepository) error) error { return failure },
	}}
	require.ErrorIs(t, r.WithMutation(func(MutationRepository) error {
		t.Fatal("callback ran despite admission failure")
		return nil
	}), failure)

	inner := newMemRepo()
	inner.add("test", "---\nid: test\n---\n# Test\n")
	inner.tickets["test"].FilePath = ""
	r = detachedRepository{inner}
	require.ErrorIs(t, r.WithMutation(func(MutationRepository) error { return failure }), failure)
	inner.saveErr = failure
	require.ErrorIs(t, r.WithMutation(func(scoped MutationRepository) error {
		st, err := scoped.Get("test")
		require.NoError(t, err)
		return scoped.Save(st)
	}), failure)
	require.Empty(t, inner.saved)

	require.ErrorIs(t, r.WithMutation(func(scoped MutationRepository) error {
		return scoped.Save(&StoredTicket{Ticket: &ticket.Ticket{ID: "missing"}})
	}), ErrNotFound)
}
