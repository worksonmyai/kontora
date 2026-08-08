package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

// writeTicketAged writes a ticket file and backdates its mtime by ageDays. A
// blank repoPath leaves the path field out of the frontmatter, and a blank
// title falls back to the ID as the markdown heading.
func writeTicketAged(t *testing.T, dir, id, status, repoPath, title string, ageDays int) {
	t.Helper()
	pathField := ""
	if repoPath != "" {
		pathField = fmt.Sprintf("path: %s\n", repoPath)
	}
	if title == "" {
		title = id
	}
	content := fmt.Sprintf("---\nid: %s\nkontora: true\nstatus: %s\n%s---\n# %s\n", id, status, pathField, title)
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
		title   string
	}
	cases := []struct {
		name string
		// tickets seeded into a temp tickets dir.
		tickets []fixture
		opts    ArchiveOpts
		// stdin answers the confirmation prompt. An empty string stands for a
		// run with no terminal attached.
		stdin string
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
			name: "archives eligible done and cancelled tickets after confirmation",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
				{id: "tst-cancelled", status: "cancelled", ageDays: 35},
				{id: "tst-recent", status: "done", ageDays: 5},
				{id: "tst-todo", status: "todo", ageDays: 90},
			},
			opts:    ArchiveOpts{Days: 30},
			stdin:   "y\n",
			wantOut: []string{"tst-done", "tst-cancelled", "Archive 2 tickets?", "Archived 2 tickets"},
			wantStatus: map[string]string{
				"tst-done":      "archived",
				"tst-cancelled": "archived",
				"tst-recent":    "done",
				"tst-todo":      "todo",
			},
		},
		{
			name: "listing shows the title and repository path",
			tickets: []fixture{
				{id: "tst-a", status: "done", ageDays: 40, path: "/repos/a", title: "Fix the flaky watcher test"},
			},
			opts:       ArchiveOpts{Days: 30},
			stdin:      "n\n",
			wantOut:    []string{"TITLE", "PATH", "Fix the flaky watcher test", "/repos/a"},
			wantStatus: map[string]string{"tst-a": "done"},
			noWrite:    true,
		},
		{
			name:       "a declined prompt changes nothing",
			tickets:    []fixture{{id: "tst-done", status: "done", ageDays: 40}},
			opts:       ArchiveOpts{Days: 30},
			stdin:      "n\n",
			wantOut:    []string{"tst-done", "Cancelled"},
			wantStatus: map[string]string{"tst-done": "done"},
			noWrite:    true,
		},
		{
			name:       "an empty answer changes nothing",
			tickets:    []fixture{{id: "tst-done", status: "done", ageDays: 40}},
			opts:       ArchiveOpts{Days: 30},
			stdin:      "\n",
			wantOut:    []string{"Cancelled"},
			wantStatus: map[string]string{"tst-done": "done"},
			noWrite:    true,
		},
		{
			name:       "yes skips the prompt",
			tickets:    []fixture{{id: "tst-done", status: "done", ageDays: 40}},
			opts:       ArchiveOpts{Days: 30, Yes: true},
			wantOut:    []string{"Archived 1 ticket"},
			wantStatus: map[string]string{"tst-done": "archived"},
		},
		{
			name:       "without a terminal the run asks for yes instead of writing",
			tickets:    []fixture{{id: "tst-done", status: "done", ageDays: 40}},
			opts:       ArchiveOpts{Days: 30},
			wantErr:    "pass --yes",
			wantStatus: map[string]string{"tst-done": "done"},
			noWrite:    true,
		},
		{
			name:       "dry run lists without writing or prompting",
			tickets:    []fixture{{id: "tst-done", status: "done", ageDays: 40}},
			opts:       ArchiveOpts{Days: 30, DryRun: true},
			wantOut:    []string{"tst-done", "1 ticket closed for at least 30 days", "Dry run"},
			wantStatus: map[string]string{"tst-done": "done"},
			noWrite:    true,
		},
		{
			name:       "nothing eligible reports it and does not prompt",
			tickets:    []fixture{{id: "tst-recent", status: "done", ageDays: 5}},
			opts:       ArchiveOpts{Days: 30},
			wantOut:    []string{"No tickets closed for at least 30 days"},
			wantStatus: map[string]string{"tst-recent": "done"},
		},
		{
			name: "project filter names the project in the summary",
			tickets: []fixture{
				{id: "tst-a", status: "done", ageDays: 40, path: "/repos/a"},
				{id: "tst-b", status: "done", ageDays: 40, path: "/repos/b"},
			},
			opts:    ArchiveOpts{Days: 30, Project: "alpha", Yes: true},
			wantOut: []string{"tst-a", "1 ticket closed for at least 30 days in project alpha"},
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
			opts:    ArchiveOpts{Days: 30, Status: "cancelled", Yes: true},
			wantOut: []string{"tst-cancelled", "1 cancelled ticket closed for at least 30 days", "Archived 1 cancelled ticket"},
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
			opts:    ArchiveOpts{Days: 30, Path: "/repos/a", Yes: true},
			wantOut: []string{"tst-a", "1 ticket closed for at least 30 days in /repos/a"},
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
				writeTicketAged(t, dir, f.id, f.status, f.path, f.title, f.ageDays)
				info, err := os.Stat(filepath.Join(dir, f.id+".md"))
				require.NoError(t, err)
				before[f.id] = info.ModTime()
			}

			cfg := testConfig(dir)
			cfg.Projects = map[string]config.Project{"alpha": {Path: "/repos/a"}}

			opts := tc.opts
			if tc.stdin != "" {
				opts.In = strings.NewReader(tc.stdin)
			}

			var buf bytes.Buffer
			err := Archive(cfg, &buf, opts)
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
