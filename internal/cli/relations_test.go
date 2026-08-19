package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

// relationFixture writes the named tickets and returns a config pointing at them.
func relationFixture(t *testing.T, ids ...string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	for _, id := range ids {
		content := "---\nid: " + id + "\nstatus: todo\nkontora: true\ndeps: []\nlinks: []\n---\n# " + id + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), []byte(content), 0o644))
	}
	return &config.Config{TicketsDir: dir}
}

func readTicket(t *testing.T, cfg *config.Config, id string) *ticket.Ticket {
	t.Helper()
	tk, err := ticket.ParseFile(filepath.Join(cfg.TicketsDir, id+".md"))
	require.NoError(t, err)
	return tk
}

func TestRelationCommands(t *testing.T) {
	cases := []struct {
		name string
		// run applies the verbs under test, in order.
		run       func(t *testing.T, cfg *config.Config) error
		wantErr   string
		wantDeps  map[string][]string
		wantLinks map[string][]string
	}{
		{
			name: "dep writes only the dependent",
			run: func(_ *testing.T, cfg *config.Config) error {
				_, err := Dep(cfg, "tst-001", "tst-002")
				return err
			},
			wantDeps: map[string][]string{"tst-001": {"tst-002"}, "tst-002": {}},
		},
		{
			name: "dep resolves both ends from a prefix",
			run: func(_ *testing.T, cfg *config.Config) error {
				_, err := Dep(cfg, "tst-001", "tst-002")
				return err
			},
			wantDeps: map[string][]string{"tst-001": {"tst-002"}},
		},
		{
			name: "undep drops the edge",
			run: func(t *testing.T, cfg *config.Config) error {
				_, err := Dep(cfg, "tst-001", "tst-002")
				require.NoError(t, err)
				_, err = Undep(cfg, "tst-001", "tst-002")
				return err
			},
			wantDeps: map[string][]string{"tst-001": {}},
		},
		{
			name: "dep refuses a cycle and leaves both files alone",
			run: func(t *testing.T, cfg *config.Config) error {
				_, err := Dep(cfg, "tst-002", "tst-001")
				require.NoError(t, err)
				_, err = Dep(cfg, "tst-001", "tst-002")
				return err
			},
			wantErr:  "dependency cycle",
			wantDeps: map[string][]string{"tst-001": {}, "tst-002": {"tst-001"}},
		},
		{
			name: "dep refuses a self relation",
			run: func(_ *testing.T, cfg *config.Config) error {
				_, err := Dep(cfg, "tst-001", "tst-001")
				return err
			},
			wantErr:  "cannot be related to itself",
			wantDeps: map[string][]string{"tst-001": {}},
		},
		{
			name: "dep refuses a missing ticket",
			run: func(_ *testing.T, cfg *config.Config) error {
				_, err := Dep(cfg, "tst-001", "ghost")
				return err
			},
			wantErr:  "not found",
			wantDeps: map[string][]string{"tst-001": {}},
		},
		{
			name: "link writes both sides and repeats without a second edge",
			run: func(t *testing.T, cfg *config.Config) error {
				_, err := Link(cfg, "tst-001", []string{"tst-002"})
				require.NoError(t, err)
				_, err = Link(cfg, "tst-001", []string{"tst-002"})
				return err
			},
			wantLinks: map[string][]string{"tst-001": {"tst-002"}, "tst-002": {"tst-001"}},
		},
		{
			name: "a multi-link does not build a clique",
			run: func(_ *testing.T, cfg *config.Config) error {
				_, err := Link(cfg, "tst-001", []string{"tst-002", "tst-003"})
				return err
			},
			wantLinks: map[string][]string{
				"tst-001": {"tst-002", "tst-003"},
				"tst-002": {"tst-001"},
				"tst-003": {"tst-001"},
			},
		},
		{
			name: "unlink removes both sides",
			run: func(t *testing.T, cfg *config.Config) error {
				_, err := Link(cfg, "tst-001", []string{"tst-002"})
				require.NoError(t, err)
				_, err = Unlink(cfg, "tst-001", []string{"tst-002"})
				return err
			},
			wantLinks: map[string][]string{"tst-001": {}, "tst-002": {}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := relationFixture(t, "tst-001", "tst-002", "tst-003")

			err := tc.run(t, cfg)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}

			for id, want := range tc.wantDeps {
				assert.Equal(t, want, readTicket(t, cfg, id).Deps, "deps of %s", id)
			}
			for id, want := range tc.wantLinks {
				assert.Equal(t, want, readTicket(t, cfg, id).Links, "links of %s", id)
			}
		})
	}
}

// The confirmation a relation verb prints names the tickets it changed, so the
// result has to carry the resolved ids rather than the prefixes that were typed.
func TestRelationResultCarriesResolvedIDs(t *testing.T) {
	cases := []struct {
		name string
		run  func(cfg *config.Config) (app.RelationResult, error)
		want []string
	}{
		{
			name: "dep",
			run:  func(cfg *config.Config) (app.RelationResult, error) { return Dep(cfg, "tst-a", "tst-d") },
			want: []string{"tst-def"},
		},
		{
			name: "link",
			run: func(cfg *config.Config) (app.RelationResult, error) {
				return Link(cfg, "tst-a", []string{"tst-d", "tst-g"})
			},
			want: []string{"tst-def", "tst-ghi"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := relationFixture(t, "tst-abc", "tst-def", "tst-ghi")
			res, err := tc.run(cfg)
			require.NoError(t, err)
			assert.Equal(t, "tst-abc", res.ID)
			assert.Equal(t, tc.want, res.Related)
		})
	}
}
