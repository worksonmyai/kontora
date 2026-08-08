package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

// writeTicketAged writes a ticket file and backdates its mtime by ageDays. A
// blank repoPath leaves the path field out of the frontmatter.
func writeTicketAged(t *testing.T, dir, id, status, repoPath string, ageDays int) {
	t.Helper()
	pathField := ""
	if repoPath != "" {
		pathField = fmt.Sprintf("path: %s\n", repoPath)
	}
	content := fmt.Sprintf("---\nid: %s\nkontora: true\nstatus: %s\n%s---\n# %s\n", id, status, pathField, id)
	writeTicket(t, dir, id+".md", content)
	mt := time.Now().AddDate(0, 0, -ageDays)
	require.NoError(t, os.Chtimes(filepath.Join(dir, id+".md"), mt, mt))
}

func TestArchive(t *testing.T) {
	type fixture struct {
		id      string
		status  string
		ageDays int
		path    string
	}
	cases := []struct {
		name string
		// tickets seeded into a temp tickets dir.
		tickets []fixture
		opts    ArchiveOpts
		// wantErr, when set, must appear in the returned error.
		wantErr string
		// wantOut lists substrings expected in stdout.
		wantOut []string
		// wantStatus is the expected on-disk status per ticket id after the run.
		wantStatus map[string]string
		// noWrite asserts no ticket file mtime changed (dry run / rejected run).
		noWrite bool
	}{
		{
			name:       "rejects zero days before touching files",
			tickets:    []fixture{{id: "tst-001", status: "done", ageDays: 90}},
			opts:       ArchiveOpts{Days: 0},
			wantErr:    "positive",
			wantStatus: map[string]string{"tst-001": "done"},
			noWrite:    true,
		},
		{
			name:       "rejects negative days before touching files",
			tickets:    []fixture{{id: "tst-001", status: "done", ageDays: 90}},
			opts:       ArchiveOpts{Days: -30},
			wantErr:    "positive",
			wantStatus: map[string]string{"tst-001": "done"},
			noWrite:    true,
		},
		{
			name: "archives eligible done and cancelled tickets",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
				{id: "tst-cancelled", status: "cancelled", ageDays: 35},
				{id: "tst-recent", status: "done", ageDays: 5},
				{id: "tst-todo", status: "todo", ageDays: 90},
			},
			opts:    ArchiveOpts{Days: 30},
			wantOut: []string{"tst-done", "tst-cancelled", "Archived 2 tickets"},
			wantStatus: map[string]string{
				"tst-done":      "archived",
				"tst-cancelled": "archived",
				"tst-recent":    "done",
				"tst-todo":      "todo",
			},
		},
		{
			name:       "dry run reports without writing",
			tickets:    []fixture{{id: "tst-done", status: "done", ageDays: 40}},
			opts:       ArchiveOpts{Days: 30, DryRun: true},
			wantOut:    []string{"tst-done", "Would archive 1 ticket", "dry run"},
			wantStatus: map[string]string{"tst-done": "done"},
			noWrite:    true,
		},
		{
			name:       "nothing eligible reports zero",
			tickets:    []fixture{{id: "tst-recent", status: "done", ageDays: 5}},
			opts:       ArchiveOpts{Days: 30},
			wantOut:    []string{"Archived 0 tickets"},
			wantStatus: map[string]string{"tst-recent": "done"},
		},
		{
			name: "project filter names the project in the summary",
			tickets: []fixture{
				{id: "tst-a", status: "done", ageDays: 40, path: "/repos/a"},
				{id: "tst-b", status: "done", ageDays: 40, path: "/repos/b"},
			},
			opts:    ArchiveOpts{Days: 30, Project: "alpha"},
			wantOut: []string{"tst-a", "Archived 1 ticket closed for at least 30 days in project alpha"},
			wantStatus: map[string]string{
				"tst-a": "archived",
				"tst-b": "done",
			},
		},
		{
			name:       "unknown project is rejected before touching files",
			tickets:    []fixture{{id: "tst-a", status: "done", ageDays: 40, path: "/repos/a"}},
			opts:       ArchiveOpts{Days: 30, Project: "nope"},
			wantErr:    "unknown project",
			wantStatus: map[string]string{"tst-a": "done"},
			noWrite:    true,
		},
		{
			name: "status filter archives one status and names it",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
				{id: "tst-cancelled", status: "cancelled", ageDays: 40},
			},
			opts:    ArchiveOpts{Days: 30, Status: "cancelled"},
			wantOut: []string{"tst-cancelled", "Archived 1 cancelled ticket closed for at least 30 days"},
			wantStatus: map[string]string{
				"tst-done":      "done",
				"tst-cancelled": "archived",
			},
		},
		{
			name:       "status that is not closed is rejected before touching files",
			tickets:    []fixture{{id: "tst-done", status: "done", ageDays: 40}},
			opts:       ArchiveOpts{Days: 30, Status: "todo"},
			wantErr:    "status must be",
			wantStatus: map[string]string{"tst-done": "done"},
			noWrite:    true,
		},
		{
			name: "path filter archives only that repository's tickets",
			tickets: []fixture{
				{id: "tst-a", status: "done", ageDays: 40, path: "/repos/a"},
				{id: "tst-b", status: "done", ageDays: 40, path: "/repos/b"},
				{id: "tst-none", status: "done", ageDays: 40},
			},
			opts:    ArchiveOpts{Days: 30, Path: "/repos/a"},
			wantOut: []string{"tst-a", "Archived 1 ticket closed for at least 30 days in /repos/a"},
			wantStatus: map[string]string{
				"tst-a":    "archived",
				"tst-b":    "done",
				"tst-none": "done",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := make(map[string]time.Time, len(tc.tickets))
			for _, f := range tc.tickets {
				writeTicketAged(t, dir, f.id, f.status, f.path, f.ageDays)
				info, err := os.Stat(filepath.Join(dir, f.id+".md"))
				require.NoError(t, err)
				before[f.id] = info.ModTime()
			}

			cfg := testConfig(dir)
			cfg.Projects = map[string]config.Project{"alpha": {Path: "/repos/a"}}

			var buf bytes.Buffer
			err := Archive(cfg, &buf, tc.opts)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}

			out := buf.String()
			for _, want := range tc.wantOut {
				assert.Contains(t, out, want)
			}

			for id, want := range tc.wantStatus {
				path := filepath.Join(dir, id+".md")
				data, rerr := os.ReadFile(path)
				require.NoError(t, rerr)
				assert.Contains(t, string(data), "status: "+want, "%s status", id)
				if tc.noWrite {
					info, serr := os.Stat(path)
					require.NoError(t, serr)
					assert.True(t, before[id].Equal(info.ModTime()), "%s mtime must not change", id)
				}
			}
		})
	}
}
