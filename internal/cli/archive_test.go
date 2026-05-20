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
)

// writeTicketAged writes a ticket file and backdates its mtime by ageDays.
func writeTicketAged(t *testing.T, dir, id, status string, ageDays int) {
	t.Helper()
	content := fmt.Sprintf("---\nid: %s\nkontora: true\nstatus: %s\n---\n# %s\n", id, status, id)
	writeTicket(t, dir, id+".md", content)
	mt := time.Now().AddDate(0, 0, -ageDays)
	require.NoError(t, os.Chtimes(filepath.Join(dir, id+".md"), mt, mt))
}

func TestArchive(t *testing.T) {
	type fixture struct {
		id      string
		status  string
		ageDays int
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
			tickets:    []fixture{{"tst-001", "done", 90}},
			opts:       ArchiveOpts{Days: 0},
			wantErr:    "positive",
			wantStatus: map[string]string{"tst-001": "done"},
			noWrite:    true,
		},
		{
			name:       "rejects negative days before touching files",
			tickets:    []fixture{{"tst-001", "done", 90}},
			opts:       ArchiveOpts{Days: -30},
			wantErr:    "positive",
			wantStatus: map[string]string{"tst-001": "done"},
			noWrite:    true,
		},
		{
			name: "archives eligible done and cancelled tickets",
			tickets: []fixture{
				{"tst-done", "done", 40},
				{"tst-cancelled", "cancelled", 35},
				{"tst-recent", "done", 5},
				{"tst-todo", "todo", 90},
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
			tickets:    []fixture{{"tst-done", "done", 40}},
			opts:       ArchiveOpts{Days: 30, DryRun: true},
			wantOut:    []string{"tst-done", "Would archive 1 ticket", "dry run"},
			wantStatus: map[string]string{"tst-done": "done"},
			noWrite:    true,
		},
		{
			name:       "nothing eligible reports zero",
			tickets:    []fixture{{"tst-recent", "done", 5}},
			opts:       ArchiveOpts{Days: 30},
			wantOut:    []string{"Archived 0 tickets"},
			wantStatus: map[string]string{"tst-recent": "done"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := make(map[string]time.Time, len(tc.tickets))
			for _, f := range tc.tickets {
				writeTicketAged(t, dir, f.id, f.status, f.ageDays)
				info, err := os.Stat(filepath.Join(dir, f.id+".md"))
				require.NoError(t, err)
				before[f.id] = info.ModTime()
			}

			var buf bytes.Buffer
			err := Archive(testConfig(dir), &buf, tc.opts)
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
