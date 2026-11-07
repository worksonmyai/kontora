package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/testutil"
	"github.com/worksonmyai/kontora/internal/ticket"
)

type sharedPointerRepo struct{ *memRepo }

func (r sharedPointerRepo) Get(id string) (*StoredTicket, error) { return r.tickets[id], nil }

func (r sharedPointerRepo) WithMutation(fn func(MutationRepository) error) error {
	return r.memRepo.WithMutation(func(MutationRepository) error { return fn(r) })
}

type reenteringRuntime struct {
	NoopRuntime
	svc *Service
	err error
}

func (r *reenteringRuntime) Cancel(string) {
	_, r.err = r.svc.Retry("second")
}

func TestMutationEffectsCanReenterPublicMutation(t *testing.T) {
	repo := newMemRepo()
	repo.add("first", "---\nid: first\nstatus: todo\n---\n# First\n")
	repo.add("second", "---\nid: second\nkontora: true\nstatus: paused\n---\n# Second\n")
	runtime := &reenteringRuntime{}
	svc := New(Static(testCfg()), repo, runtime)
	runtime.svc = svc
	done := make(chan error, 1)
	go func() { _, err := svc.SetStatus("first", ticket.StatusDone); done <- err }()
	select {
	case err := <-done:
		require.NoError(t, err)
		require.NoError(t, runtime.err)
	case <-time.After(5 * time.Second):
		t.Fatal("runtime hook ran under the mutation lock")
	}
	require.Equal(t, ticket.StatusTodo, repo.tickets["second"].Ticket.Status)
}

func TestMutationUsesScopedDecisionAndDoesNotRebindService(t *testing.T) {
	outer, inner := newMemRepo(), newMemRepo()
	outer.add("test", "---\nid: test\nstatus: todo\n---\n# Outer\n")
	inner.add("test", "---\nid: test\nstatus: archived\n---\n# Inner\n")
	repo := transactionTestRepository{MutationRepository: outer, withMutation: func(fn func(MutationRepository) error) error { return fn(inner) }}
	runtime := &spyRuntime{}
	svc := New(Static(testCfg()), repo, runtime)
	_, err := svc.SetStatus("test", ticket.StatusDone)
	require.ErrorIs(t, err, ErrInvalidState)
	require.Empty(t, outer.saved)
	require.Empty(t, inner.saved)
	require.Empty(t, runtime.updated)
	view, err := svc.Get("test", GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "todo", view.Status)
}

func TestMutationPublishesCommittedEffectsWhenScopeReturnsError(t *testing.T) {
	inner := newMemRepo()
	inner.add("test", "---\nid: test\nstatus: todo\n---\n# Test\n")
	failure := fmt.Errorf("transaction completion failed")
	repo := transactionTestRepository{MutationRepository: inner, withMutation: func(fn func(MutationRepository) error) error {
		if err := fn(inner); err != nil {
			return err
		}
		return failure
	}}
	runtime := &spyRuntime{}
	_, err := New(Static(testCfg()), repo, runtime).SetStatus("test", ticket.StatusDone)
	require.ErrorIs(t, err, failure)
	require.Equal(t, ticket.StatusDone, inner.tickets["test"].Ticket.Status)
	require.Equal(t, []string{"test"}, runtime.cancelled)
	require.Equal(t, []string{"test"}, runtime.reconciled)
	require.Equal(t, []string{"test"}, runtime.updated)
}

func TestDependencyDecisionReadsFreshScopedGraph(t *testing.T) {
	repo := newMemRepo()
	repo.add("first", "---\nid: first\n---\n# First\n")
	repo.add("second", "---\nid: second\n---\n# Second\n")
	repo.beforeMutation = func() {
		require.NoError(t, repo.tickets["second"].Ticket.SetField("deps", []string{"first"}))
	}
	runtime := &spyRuntime{}
	_, err := New(Static(testCfg()), repo, runtime).AddDependency("first", "second")
	require.ErrorIs(t, err, ErrRelationCycle)
	require.Empty(t, repo.saved)
	require.Empty(t, runtime.updated)
}

func TestConcurrentDependencyMutationsCannotCreateCycle(t *testing.T) {
	repo := newMemRepo()
	repo.add("first", "---\nid: first\n---\n# First\n")
	repo.add("second", "---\nid: second\n---\n# Second\n")
	svc := New(Static(testCfg()), repo, NoopRuntime{})
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, pair := range [][2]string{{"first", "second"}, {"second", "first"}} {
		go func(from, to string) {
			<-start
			_, err := svc.AddDependency(from, to)
			results <- err
		}(pair[0], pair[1])
	}
	close(start)
	first, second := <-results, <-results
	if first == nil {
		require.ErrorIs(t, second, ErrRelationCycle)
	} else {
		require.ErrorIs(t, first, ErrRelationCycle)
		require.NoError(t, second)
	}
	require.Len(t, repo.saved, 1)
}

func TestLifecycleDecisionsUseFreshScopedState(t *testing.T) {
	for _, operation := range []string{"retry", "skip", "init"} {
		t.Run(operation, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("test", "---\nid: test\nkontora: true\nstatus: open\npipeline: default\nstage: code\n---\n# Test\n")
			repo.beforeMutation = func() { require.NoError(t, repo.tickets["test"].Ticket.SetField("status", "archived")) }
			runtime := &spyRuntime{}
			svc := New(Static(testCfg()), repo, runtime)
			var err error
			switch operation {
			case "retry":
				_, err = svc.Retry("test")
			case "skip":
				_, err = svc.Skip("test")
			case "init":
				_, err = svc.Init("test", InitRequest{})
			}
			require.ErrorIs(t, err, ErrInvalidState)
			require.Empty(t, repo.saved)
			require.Empty(t, runtime.enqueued)
			require.Empty(t, runtime.updated)
		})
	}
}

func TestRetryRejectsExecutionInputsChangedAfterPreparation(t *testing.T) {
	for _, field := range []string{"path", "branch", "base_branch", "parent", "kind", "pipeline", "stage", "agent"} {
		t.Run(field, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("test", "---\nid: test\nkontora: true\nstatus: paused\n---\n# Test\n")
			repo.beforeMutation = func() { require.NoError(t, repo.tickets["test"].Ticket.SetField(field, "changed")) }
			runtime := &spyRuntime{}
			_, err := New(Static(testCfg()), repo, runtime).Retry("test")
			require.ErrorIs(t, err, ErrInvalidState)
			require.Empty(t, repo.saved)
			require.Empty(t, runtime.enqueued)
			require.Empty(t, runtime.updated)
		})
	}
}

func TestInitRejectsFreshEpicMembershipChanges(t *testing.T) {
	path := testutil.InitRepo(t)
	for _, change := range []string{"join", "leave", "branch", "path"} {
		t.Run(change, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("epic", fmt.Sprintf("---\nid: epic\nkind: epic\nstatus: open\npath: %q\n---\n# Epic\n", path))
			repo.add("child", fmt.Sprintf("---\nid: child\nparent: epic\npath: %q\n---\n# Child\n", path))
			if change == "join" {
				require.NoError(t, repo.tickets["child"].Ticket.SetField("parent", ""))
			}
			repo.beforeMutation = func() {
				field, value := "parent", "epic"
				switch change {
				case "leave":
					value = "other"
				case "branch":
					field, value = "branch", "new-branch"
				case "path":
					field, value = "path", "/changed"
				}
				require.NoError(t, repo.tickets["child"].Ticket.SetField(field, value))
			}
			runtime := &spyRuntime{}
			_, err := New(Static(testCfg()), repo, runtime).Init("epic", InitRequest{Status: "todo"})
			require.ErrorIs(t, err, ErrInvalidState)
			require.Empty(t, repo.saved)
			require.Empty(t, runtime.enqueued)
		})
	}
}

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
