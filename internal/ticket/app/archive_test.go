package app

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
)

// fakeFileInfo is a minimal fs.FileInfo that only carries a mod time.
type fakeFileInfo struct {
	modTime time.Time
}

func (f fakeFileInfo) Name() string       { return "" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return f.modTime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

var archiveNow = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

func TestArchive(t *testing.T) {
	type fixture struct {
		id      string
		status  string
		ageDays int // file mtime = archiveNow - ageDays
	}
	cases := []struct {
		name         string
		tickets      []fixture
		days         int
		dryRun       bool
		statErr      bool
		saveErr      bool
		wantArchived []string
		wantErr      bool
	}{
		{
			name: "eligible done and cancelled are archived",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
				{id: "tst-cancelled", status: "cancelled", ageDays: 30},
			},
			days:         30,
			wantArchived: []string{"tst-cancelled", "tst-done"},
		},
		{
			name: "too-new closed tickets are left unchanged",
			tickets: []fixture{
				{id: "tst-recent", status: "done", ageDays: 5},
			},
			days:         30,
			wantArchived: nil,
		},
		{
			name: "non-closed tickets are never archived",
			tickets: []fixture{
				{id: "tst-todo", status: "todo", ageDays: 90},
				{id: "tst-prog", status: "in_progress", ageDays: 90},
				{id: "tst-paused", status: "paused", ageDays: 90},
				{id: "tst-open", status: "open", ageDays: 90},
			},
			days:         30,
			wantArchived: nil,
		},
		{
			name: "already-archived tickets are skipped",
			tickets: []fixture{
				{id: "tst-arch", status: "archived", ageDays: 90},
			},
			days:         30,
			wantArchived: nil,
		},
		{
			name: "dry run reports without writing",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
			},
			days:         30,
			dryRun:       true,
			wantArchived: []string{"tst-done"},
		},
		{
			name: "stat error fails the run",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
			},
			days:    30,
			statErr: true,
			wantErr: true,
		},
		{
			name: "save error fails the run",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
			},
			days:    30,
			saveErr: true,
			wantErr: true,
		},
		{
			name: "zero days is rejected",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
			},
			days:    0,
			wantErr: true,
		},
		{
			name: "negative days is rejected",
			tickets: []fixture{
				{id: "tst-done", status: "done", ageDays: 40},
			},
			days:    -1,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			mtimes := make(map[string]time.Time, len(tc.tickets))
			for _, f := range tc.tickets {
				repo.add(f.id, fmt.Sprintf("---\nid: %s\nstatus: %s\nkontora: true\n---\n# %s\n", f.id, f.status, f.id))
				mtimes[repo.tickets[f.id].FilePath] = archiveNow.AddDate(0, 0, -f.ageDays)
			}
			if tc.saveErr {
				repo.saveErr = errors.New("disk full")
			}

			stat := func(path string) (fs.FileInfo, error) {
				if tc.statErr {
					return nil, errors.New("stat boom")
				}
				mt, ok := mtimes[path]
				if !ok {
					return nil, fmt.Errorf("no mtime registered for %s", path)
				}
				return fakeFileInfo{modTime: mt}, nil
			}

			rt := &spyRuntime{}
			svc := New(testCfg(), repo, rt)

			result, err := svc.archive(ArchiveOptions{Days: tc.days, DryRun: tc.dryRun}, archiveNow, stat)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantArchived, result.Archived)
			assert.Equal(t, tc.dryRun, result.DryRun)

			archived := make(map[string]bool, len(tc.wantArchived))
			for _, id := range tc.wantArchived {
				archived[id] = true
			}
			for _, f := range tc.tickets {
				gotStatus := repo.tickets[f.id].Ticket.Status
				if archived[f.id] && !tc.dryRun {
					assert.Equal(t, ticket.StatusArchived, gotStatus, "%s should be archived", f.id)
				} else {
					assert.Equal(t, ticket.Status(f.status), gotStatus, "%s should keep its status", f.id)
				}
			}

			if tc.dryRun {
				assert.Empty(t, rt.updated, "dry run must not broadcast updates")
			} else {
				assert.ElementsMatch(t, tc.wantArchived, rt.updated)
			}
		})
	}
}

func TestArchive_PreservesBodyAndUnknownFields(t *testing.T) {
	repo := newMemRepo()
	content := "---\nid: tst-001\nstatus: done\nkontora: true\npriority: high\n---\n# Title\n\nBody paragraph.\n"
	repo.add("tst-001", content)
	mtime := archiveNow.AddDate(0, 0, -40)
	stat := func(string) (fs.FileInfo, error) { return fakeFileInfo{modTime: mtime}, nil }

	svc := New(testCfg(), repo, &spyRuntime{})
	_, err := svc.archive(ArchiveOptions{Days: 30}, archiveNow, stat)
	require.NoError(t, err)

	out, err := repo.tickets["tst-001"].Ticket.Marshal()
	require.NoError(t, err)
	rendered := string(out)
	assert.Contains(t, rendered, "status: archived")
	assert.Contains(t, rendered, "priority: high")
	assert.Contains(t, rendered, "Body paragraph.")
}
