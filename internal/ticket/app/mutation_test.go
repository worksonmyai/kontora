package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

type sharedPointerRepo struct{ *memRepo }

func (r sharedPointerRepo) Get(id string) (*StoredTicket, error) { return r.tickets[id], nil }

func TestMutationRefusalDoesNotChangeSharedPointers(t *testing.T) {
	repo := newMemRepo()
	repo.add("ordinary", "---\nid: ordinary\nkontora: true\nstatus: open\n---\n# Ordinary\n")
	repo.tickets["ordinary"].FilePath = ""
	repo.saveErr = fmt.Errorf("stale storage revision")
	runtime := &spyRuntime{}
	svc := New(Static(testCfg()), sharedPointerRepo{repo}, runtime)
	original := repo.tickets["ordinary"].Ticket
	before, err := original.Marshal()
	require.NoError(t, err)
	_, err = svc.SetStatus("ordinary", ticket.StatusDone)
	require.Error(t, err)
	after, err := original.Marshal()
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, ticket.StatusOpen, original.Status)
	require.Empty(t, runtime.cancelled)
	require.Empty(t, runtime.updated)
}

func TestRelationsRefuseEveryManagedParticipantBeforeSaving(t *testing.T) {
	for _, operation := range []string{"link", "dependency", "remove dependency", "parent", "clear parent", "child order"} {
		t.Run(operation, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("ordinary", "---\nid: ordinary\nparent: managed\ndeps: [managed]\n---\n# Ordinary\n")
			repo.add("managed", "---\nid: managed\nkind: epic\nmanaged: {}\n---\n# Managed\n")
			for _, st := range repo.tickets {
				st.FilePath = ""
			}
			runtime := &spyRuntime{}
			svc := New(Static(testCfg()), sharedPointerRepo{repo}, runtime)
			var err error
			switch operation {
			case "link":
				_, err = svc.Link("ordinary", "managed")
			case "dependency":
				_, err = svc.AddDependency("ordinary", "managed")
			case "remove dependency":
				_, err = svc.RemoveDependency("ordinary", "managed")
			case "parent":
				_, err = svc.SetParent("ordinary", "managed")
			case "clear parent":
				_, err = svc.ClearParent("ordinary")
			case "child order":
				_, err = svc.SetChildOrder("managed", []string{"ordinary"})
			}
			require.ErrorIs(t, err, ticket.ErrManaged)
			require.Empty(t, repo.saved)
			require.Empty(t, runtime.updated)
			require.Empty(t, repo.tickets["ordinary"].Ticket.Links)
		})
	}
}
