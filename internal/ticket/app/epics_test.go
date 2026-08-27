package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
)

const epicFM = "---\nid: epc-100\nstatus: open\nkontora: true\nkind: epic\n---\n# An epic\n"

// An epic is not runnable work, and its status is derived, so every verb that
// would run it or move it by hand is refused at the one layer the CLI, the TUI
// and the daemon all go through.
func TestEpicRefusedByServiceVerbs(t *testing.T) {
	cases := []struct {
		name string
		call func(*Service) error
	}{
		{name: "set status", call: func(s *Service) error { _, err := s.SetStatus("epc-100", ticket.StatusDone); return err }},
		{name: "retry", call: func(s *Service) error { _, err := s.Retry("epc-100"); return err }},
		{name: "skip", call: func(s *Service) error { _, err := s.Skip("epc-100"); return err }},
		{name: "init", call: func(s *Service) error { _, err := s.Init("epc-100", InitRequest{Pipeline: "default"}); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("epc-100", epicFM)
			svc := New(Static(testCfg()), repo, &spyRuntime{})

			err := tc.call(svc)
			require.ErrorIs(t, err, ErrInvalidState)
			assert.Empty(t, repo.saved)
			assert.Equal(t, ticket.StatusOpen, repo.tickets["epc-100"].Ticket.Status)
		})
	}
}

func TestSetParent(t *testing.T) {
	cases := []struct {
		name       string
		tickets    map[string]string
		child      string
		parent     string
		wantErr    error
		wantParent string
	}{
		{
			name: "a ticket is filed under an epic",
			tickets: map[string]string{
				"epc-100": epicFM,
				"tsk-001": "---\nid: tsk-001\nstatus: open\nkontora: true\n---\n# A task\n",
			},
			child: "tsk-001", parent: "epc-100", wantParent: "epc-100",
		},
		{
			name: "the parent must be an epic",
			tickets: map[string]string{
				"tsk-001": "---\nid: tsk-001\nstatus: open\nkontora: true\n---\n# A task\n",
				"tsk-002": "---\nid: tsk-002\nstatus: open\nkontora: true\n---\n# Another\n",
			},
			child: "tsk-001", parent: "tsk-002", wantErr: ErrNotAnEpic,
		},
		{
			name: "epics do not nest",
			tickets: map[string]string{
				"epc-100": epicFM,
				"epc-200": "---\nid: epc-200\nstatus: open\nkontora: true\nkind: epic\n---\n# Another epic\n",
			},
			child: "epc-200", parent: "epc-100", wantErr: ErrNotAnEpic,
		},
		{
			name:    "a ticket cannot parent itself",
			tickets: map[string]string{"epc-100": epicFM},
			child:   "epc-100", parent: "epc-100", wantErr: ErrSelfRelation,
		},
		{
			name: "a parent chain that would close a cycle",
			tickets: map[string]string{
				// A hand-written file can put an epic under a task, which is the
				// only way a two-step parent cycle can be reached.
				"epc-100": "---\nid: epc-100\nstatus: open\nkontora: true\nkind: epic\nparent: tsk-001\n---\n# An epic\n",
				"tsk-001": "---\nid: tsk-001\nstatus: open\nkontora: true\n---\n# A task\n",
			},
			child: "tsk-001", parent: "epc-100", wantErr: ErrRelationCycle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			for id, fm := range tc.tickets {
				repo.add(id, fm)
			}
			svc := New(Static(testCfg()), repo, &spyRuntime{})

			_, err := svc.SetParent(tc.child, tc.parent)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Empty(t, repo.saved)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{tc.child}, repo.saved)
			assert.Equal(t, tc.wantParent, repo.tickets[tc.child].Ticket.Parent)
		})
	}
}

// Clearing a parent writes the child. Clearing one that is not there writes
// nothing, so a repeated call is not a repeated file write.
func TestClearParent(t *testing.T) {
	repo := newMemRepo()
	repo.add("epc-100", epicFM)
	repo.add("tsk-001", "---\nid: tsk-001\nstatus: open\nkontora: true\nparent: epc-100\n---\n# A task\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.ClearParent("tsk-001")
	require.NoError(t, err)
	assert.Empty(t, repo.tickets["tsk-001"].Ticket.Parent)
	assert.Equal(t, []string{"tsk-001"}, repo.saved)

	_, err = svc.ClearParent("tsk-001")
	require.NoError(t, err)
	assert.Equal(t, []string{"tsk-001"}, repo.saved)
}

// The order list writes one file, the epic's, and never a child.
func TestSetChildOrder(t *testing.T) {
	newRepo := func() *memRepo {
		repo := newMemRepo()
		repo.add("epc-100", epicFM)
		repo.add("tsk-001", "---\nid: tsk-001\nstatus: open\nkontora: true\nparent: epc-100\n---\n# One\n")
		repo.add("tsk-002", "---\nid: tsk-002\nstatus: open\nkontora: true\nparent: epc-100\n---\n# Two\n")
		return repo
	}

	t.Run("writes the epic only", func(t *testing.T) {
		repo := newRepo()
		svc := New(Static(testCfg()), repo, &spyRuntime{})
		_, err := svc.SetChildOrder("epc-100", []string{"tsk-002", "tsk-001"})
		require.NoError(t, err)
		assert.Equal(t, []string{"epc-100"}, repo.saved)
		assert.Equal(t, []string{"tsk-002", "tsk-001"}, repo.tickets["epc-100"].Ticket.Children)
	})

	t.Run("the same order writes nothing", func(t *testing.T) {
		repo := newRepo()
		svc := New(Static(testCfg()), repo, &spyRuntime{})
		_, err := svc.SetChildOrder("epc-100", []string{"tsk-002", "tsk-001"})
		require.NoError(t, err)
		_, err = svc.SetChildOrder("epc-100", []string{"tsk-002", "tsk-001"})
		require.NoError(t, err)
		assert.Equal(t, []string{"epc-100"}, repo.saved)
	})

	t.Run("only an epic has an order", func(t *testing.T) {
		repo := newRepo()
		svc := New(Static(testCfg()), repo, &spyRuntime{})
		_, err := svc.SetChildOrder("tsk-001", []string{"tsk-002"})
		require.ErrorIs(t, err, ErrNotAnEpic)
		assert.Empty(t, repo.saved)
	})
}

// An epic has no pipeline because it is not work, so the view must not hand the
// page a default agent and a one-stage ribbon it has no run for.
func TestBuildViewEpicHasNoPipelineData(t *testing.T) {
	tkt, err := ticket.ParseBytes([]byte(epicFM))
	require.NoError(t, err)

	v := BuildView(testCfg(), tkt, true)
	assert.Equal(t, "epic", v.Kind)
	assert.Empty(t, v.Agent)
	assert.Empty(t, v.Stages)
}
