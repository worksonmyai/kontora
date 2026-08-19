package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relationRepo seeds tickets whose frontmatter carries the given relations.
func relationRepo(t *testing.T, specs ...string) *memRepo {
	t.Helper()
	repo := newMemRepo()
	for _, spec := range specs {
		id, rest, _ := strings.Cut(spec, " ")
		repo.add(id, "---\nid: "+id+"\nstatus: todo\nkontora: true\n"+rest+"\n---\n# "+id+"\n")
	}
	return repo
}

// marshalled returns a ticket's file bytes, which is what a rejected call must
// leave untouched.
func marshalled(t *testing.T, repo *memRepo, id string) string {
	t.Helper()
	out, err := repo.tickets[id].Ticket.Marshal()
	require.NoError(t, err)
	return string(out)
}

func TestAddDependency(t *testing.T) {
	cases := []struct {
		name     string
		specs    []string
		ticketID string
		depID    string
		wantErr  string
		wantDeps []string
		// wantNoop marks a call that succeeds without changing anything, which
		// leaves the scheduler nothing to reconsider.
		wantNoop bool
	}{
		{
			name:     "adds the edge to the dependent only",
			specs:    []string{"a deps: []", "b deps: []"},
			ticketID: "a",
			depID:    "b",
			wantDeps: []string{"b"},
		},
		{
			name:     "repeating an existing edge adds no second entry",
			specs:    []string{"a deps: [b]", "b deps: []"},
			ticketID: "a",
			depID:    "b",
			wantDeps: []string{"b"},
			wantNoop: true,
		},
		{
			name:     "a missing dependency is refused",
			specs:    []string{"a deps: []"},
			ticketID: "a",
			depID:    "ghost",
			wantErr:  "not found",
			wantDeps: nil,
		},
		{
			name:     "a missing ticket is refused",
			specs:    []string{"a deps: []"},
			ticketID: "ghost",
			depID:    "a",
			wantErr:  "not found",
		},
		{
			name:     "a self dependency is refused",
			specs:    []string{"a deps: []"},
			ticketID: "a",
			depID:    "a",
			wantErr:  "cannot be related to itself",
			wantDeps: nil,
		},
		{
			name:     "a direct cycle is refused",
			specs:    []string{"a deps: []", "b deps: [a]"},
			ticketID: "a",
			depID:    "b",
			wantErr:  "dependency cycle",
			wantDeps: nil,
		},
		{
			name:     "a transitive cycle is refused",
			specs:    []string{"a deps: []", "b deps: [c]", "c deps: [a]"},
			ticketID: "a",
			depID:    "b",
			wantErr:  "dependency cycle",
			wantDeps: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := relationRepo(t, tc.specs...)
			var before string
			if _, ok := repo.tickets[tc.ticketID]; ok {
				before = marshalled(t, repo, tc.ticketID)
			}
			rt := &spyRuntime{}
			svc := New(Static(testCfg()), repo, rt)

			_, err := svc.AddDependency(tc.ticketID, tc.depID)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				if before != "" {
					assert.Equal(t, before, marshalled(t, repo, tc.ticketID), "a rejected call must not write the ticket")
				}
				assert.Empty(t, rt.reconciled)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDeps, repo.tickets[tc.ticketID].Ticket.Deps)
			if tc.wantNoop {
				assert.Empty(t, rt.reconciled)
				assert.Equal(t, before, marshalled(t, repo, tc.ticketID))
				return
			}
			assert.Equal(t, []string{tc.ticketID}, rt.reconciled)
		})
	}
}

func TestRemoveDependency(t *testing.T) {
	cases := []struct {
		name     string
		specs    []string
		wantDeps []string
	}{
		{
			name:     "drops the edge",
			specs:    []string{"a deps: [b]", "b deps: []"},
			wantDeps: []string{},
		},
		{
			name:     "keeps the other edges",
			specs:    []string{"a deps: [b, c]", "b deps: []", "c deps: []"},
			wantDeps: []string{"c"},
		},
		{
			name:     "removing an absent edge is a no-op",
			specs:    []string{"a deps: [c]", "b deps: []", "c deps: []"},
			wantDeps: []string{"c"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := relationRepo(t, tc.specs...)
			svc := New(Static(testCfg()), repo, &spyRuntime{})

			_, err := svc.RemoveDependency("a", "b")
			require.NoError(t, err)
			assert.Equal(t, tc.wantDeps, repo.tickets["a"].Ticket.Deps)
		})
	}
}

func TestLink(t *testing.T) {
	cases := []struct {
		name      string
		specs     []string
		related   []string
		runTwice  bool
		wantErr   string
		wantLinks map[string][]string
	}{
		{
			name:      "links both sides",
			specs:     []string{"a links: []", "b links: []"},
			related:   []string{"b"},
			wantLinks: map[string][]string{"a": {"b"}, "b": {"a"}},
		},
		{
			name:      "a repeated link adds no second edge",
			specs:     []string{"a links: []", "b links: []"},
			related:   []string{"b"},
			runTwice:  true,
			wantLinks: map[string][]string{"a": {"b"}, "b": {"a"}},
		},
		{
			name:      "a multi-link connects the first ticket to each other, not a clique",
			specs:     []string{"a links: []", "b links: []", "c links: []"},
			related:   []string{"b", "c"},
			wantLinks: map[string][]string{"a": {"b", "c"}, "b": {"a"}, "c": {"a"}},
		},
		{
			name:      "a self link is refused",
			specs:     []string{"a links: []"},
			related:   []string{"a"},
			wantErr:   "cannot be related to itself",
			wantLinks: map[string][]string{"a": {}},
		},
		{
			name:      "a missing ticket is refused and writes nothing",
			specs:     []string{"a links: []"},
			related:   []string{"ghost"},
			wantErr:   "not found",
			wantLinks: map[string][]string{"a": {}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := relationRepo(t, tc.specs...)
			before := marshalled(t, repo, "a")
			svc := New(Static(testCfg()), repo, &spyRuntime{})

			_, err := svc.Link("a", tc.related...)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Equal(t, before, marshalled(t, repo, "a"), "a rejected call must not write the ticket")
				return
			}
			require.NoError(t, err)
			if tc.runTwice {
				_, err = svc.Link("a", tc.related...)
				require.NoError(t, err)
			}
			for id, want := range tc.wantLinks {
				assert.Equal(t, want, repo.tickets[id].Ticket.Links, "links of %s", id)
			}
		})
	}
}

// A link spans two markdown files, which cannot be written together. The second
// write failing must say so and name both tickets, and a retry must repair the
// side that is missing rather than stopping at the side that is already right.
func TestLink_PartialWriteIsReportedAndRepaired(t *testing.T) {
	repo := relationRepo(t, "a links: []", "b links: []")
	boom := errors.New("disk full")
	repo.saveErrFor = map[string]error{"b": boom}
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Link("a", "b")
	require.Error(t, err)
	assert.ErrorContains(t, err, "a")
	assert.ErrorContains(t, err, "b")
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, []string{"b"}, repo.tickets["a"].Ticket.Links, "the side that was written keeps its link")
	assert.Empty(t, repo.tickets["b"].Ticket.Links, "the side whose save failed is unchanged")

	repo.saveErrFor = nil
	repo.saved = nil
	result, err := svc.Link("a", "b")
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, repo.tickets["b"].Ticket.Links)
	assert.Equal(t, []string{"b"}, repo.tickets["a"].Ticket.Links)
	assert.Equal(t, []string{"b"}, result.Changed, "the repaired call writes only the missing side")
}

func TestUnlink(t *testing.T) {
	cases := []struct {
		name      string
		specs     []string
		related   []string
		wantLinks map[string][]string
	}{
		{
			name:      "removes both sides",
			specs:     []string{"a links: [b]", "b links: [a]"},
			related:   []string{"b"},
			wantLinks: map[string][]string{"a": {}, "b": {}},
		},
		{
			name:      "removing an absent edge is a no-op",
			specs:     []string{"a links: [c]", "b links: []", "c links: [a]"},
			related:   []string{"b"},
			wantLinks: map[string][]string{"a": {"c"}, "b": {}},
		},
		{
			name:      "repairs a one-sided link",
			specs:     []string{"a links: [b]", "b links: []"},
			related:   []string{"b"},
			wantLinks: map[string][]string{"a": {}, "b": {}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := relationRepo(t, tc.specs...)
			svc := New(Static(testCfg()), repo, &spyRuntime{})

			_, err := svc.Unlink("a", tc.related...)
			require.NoError(t, err)
			for id, want := range tc.wantLinks {
				assert.Equal(t, want, repo.tickets[id].Ticket.Links, "links of %s", id)
			}
		})
	}
}

func TestRelations_RequireARelatedID(t *testing.T) {
	repo := relationRepo(t, "a links: []")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Link("a")
	require.ErrorContains(t, err, "at least one related ticket id is required")
}
