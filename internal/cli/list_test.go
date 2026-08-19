package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

// listFixture writes one ticket per spec line of the form
// "<id> <status> [dep=<id>] [path=<path>]".
func listFixture(t *testing.T, specs ...string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		TicketsDir:   dir,
		DefaultAgent: "claude-sonnet",
		Agents:       map[string]config.Agent{"claude-sonnet": {Binary: "claude"}},
		Projects: map[string]config.Project{
			"alpha": {Path: "/tmp/alpha"},
			"beta":  {Path: "/tmp/beta"},
		},
	}
	for i, spec := range specs {
		fields := strings.Fields(spec)
		id, status := fields[0], fields[1]
		dep, path := "[]", "/tmp/alpha"
		for _, f := range fields[2:] {
			switch {
			case strings.HasPrefix(f, "dep="):
				dep = "[" + strings.TrimPrefix(f, "dep=") + "]"
			case strings.HasPrefix(f, "path="):
				path = strings.TrimPrefix(f, "path=")
			}
		}
		// Distinct creation times keep the ready and blocked order stable.
		content := "---\nid: " + id + "\nkontora: true\nstatus: " + status +
			"\npath: " + path + "\ndeps: " + dep +
			"\ncreated: 2026-01-0" + string(rune('1'+i)) + "T00:00:00Z\n---\n# Title " + id + "\n\nbody of " + id + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), []byte(content), 0o644))
	}
	return cfg
}

func listIDs(t *testing.T, cfg *config.Config, opts ListOpts) []string {
	t.Helper()
	opts.JSON = true
	var buf bytes.Buffer
	require.NoError(t, List(cfg, &buf, opts))
	var items []ListItem
	require.NoError(t, json.Unmarshal(buf.Bytes(), &items))
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

func TestListFilters(t *testing.T) {
	specs := []string{
		"tst-run in_progress",
		"tst-ready todo",
		"tst-block todo dep=tst-open",
		"tst-open open",
		"tst-done done",
		"tst-closed closed",
		"tst-arch archived",
		"tst-beta todo path=/tmp/beta",
	}

	cases := []struct {
		name    string
		opts    ListOpts
		wantIDs []string
	}{
		{
			name:    "the default hides closed and archived",
			wantIDs: []string{"tst-run", "tst-beta", "tst-block", "tst-ready", "tst-open"},
		},
		{
			name:    "closed adds done and legacy closed but not archived",
			opts:    ListOpts{ShowClosed: true},
			wantIDs: []string{"tst-run", "tst-beta", "tst-block", "tst-ready", "tst-open", "tst-done", "tst-closed"},
		},
		{
			name:    "archived adds only archived",
			opts:    ListOpts{ShowArchived: true},
			wantIDs: []string{"tst-run", "tst-beta", "tst-block", "tst-ready", "tst-open", "tst-arch"},
		},
		{
			name:    "status selects one status",
			opts:    ListOpts{Status: "open"},
			wantIDs: []string{"tst-open"},
		},
		{
			// Naming a hidden status is asking to see it, so neither of these
			// needs its own --closed or --archived.
			name:    "status selects a closed status on its own",
			opts:    ListOpts{Status: "done"},
			wantIDs: []string{"tst-done"},
		},
		{
			name:    "status selects the legacy closed status on its own",
			opts:    ListOpts{Status: "closed"},
			wantIDs: []string{"tst-closed"},
		},
		{
			name:    "status selects archived on its own",
			opts:    ListOpts{Status: "archived"},
			wantIDs: []string{"tst-arch"},
		},
		{
			name:    "project selects by configured project",
			opts:    ListOpts{Project: "beta"},
			wantIDs: []string{"tst-beta"},
		},
		{
			name:    "path selects by repository path",
			opts:    ListOpts{Path: "/tmp/beta"},
			wantIDs: []string{"tst-beta"},
		},
		{
			name: "ready selects unblocked todo tickets in scheduler order",
			opts: ListOpts{Ready: true},
			// tst-ready was created before tst-beta.
			wantIDs: []string{"tst-ready", "tst-beta"},
		},
		{
			name:    "blocked selects the waiting todo tickets",
			opts:    ListOpts{Blocked: true},
			wantIDs: []string{"tst-block"},
		},
		{
			name:    "limit cuts the result",
			opts:    ListOpts{Ready: true, Limit: 1},
			wantIDs: []string{"tst-ready"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := listFixture(t, specs...)
			assert.Equal(t, tc.wantIDs, listIDs(t, cfg, tc.opts))
		})
	}
}

func TestListJSON(t *testing.T) {
	cfg := listFixture(t, "tst-block todo dep=tst-open", "tst-open open")

	var buf bytes.Buffer
	require.NoError(t, List(cfg, &buf, ListOpts{JSON: true, Status: "todo"}))

	var raw []map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &raw), "stdout must parse as JSON")
	require.Len(t, raw, 1)

	got := raw[0]
	assert.NotContains(t, got, "body", "a listing must not carry the ticket body")
	for _, key := range []string{"id", "title", "status", "path", "project", "deps", "links", "ready", "blockers", "created_at"} {
		assert.Contains(t, got, key)
	}
	assert.Equal(t, "tst-block", got["id"])
	assert.Equal(t, "Title tst-block", got["title"])
	assert.Equal(t, "alpha", got["project"])
	assert.Equal(t, false, got["ready"])
	assert.Equal(t, []any{"tst-open"}, got["blockers"])
	assert.Equal(t, []any{"tst-open"}, got["deps"])
	assert.Equal(t, []any{}, got["links"], "an empty relation list is an array, not null")
}

func TestListJSONEmptyIsAnArray(t *testing.T) {
	cfg := listFixture(t)

	var buf bytes.Buffer
	require.NoError(t, List(cfg, &buf, ListOpts{JSON: true}))
	assert.Equal(t, "[]\n", buf.String())
}

func TestListRejectsContradictoryFilters(t *testing.T) {
	cfg := listFixture(t, "tst-001 todo")

	var buf bytes.Buffer
	err := List(cfg, &buf, ListOpts{Ready: true, Blocked: true})
	require.ErrorContains(t, err, "--ready and --blocked")
}

func TestListTableCarriesBlockers(t *testing.T) {
	cfg := listFixture(t, "tst-block todo dep=tst-open", "tst-open open")

	var buf bytes.Buffer
	require.NoError(t, List(cfg, &buf, ListOpts{Blocked: true}))

	out := buf.String()
	assert.Contains(t, out, "BLOCKED BY")
	assert.Contains(t, out, "tst-open")
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "Title tst-block")
}

// A missing dependency blocks: nothing on disk says that work is finished.
func TestListBlockedByMissingDependency(t *testing.T) {
	cfg := listFixture(t, "tst-block todo dep=ghost")
	assert.Equal(t, []string{"tst-block"}, listIDs(t, cfg, ListOpts{Blocked: true}))
	assert.Empty(t, listIDs(t, cfg, ListOpts{Ready: true}))
}

// An archived dependency is closed, so it releases its dependents even though a
// default listing hides it.
func TestListArchivedDependencyIsResolved(t *testing.T) {
	cfg := listFixture(t, "tst-ready todo dep=tst-arch", "tst-arch archived")
	assert.Equal(t, []string{"tst-ready"}, listIDs(t, cfg, ListOpts{Ready: true}))
}
