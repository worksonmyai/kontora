package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestSecondaryDependencyResolution(t *testing.T) {
	for _, tc := range []struct {
		name, status, primaryStatus string
		duplicate                   bool
		wantReady                   bool
		wantStatus                  string
	}{
		{name: "done", status: "done", wantReady: true, wantStatus: "done"},
		{name: "archived", status: "archived", wantReady: true, wantStatus: "archived"},
		{name: "review", status: "human_review", wantReady: true, wantStatus: "human_review"},
		{name: "cancelled", status: "cancelled", wantReady: true, wantStatus: "cancelled"},
		{name: "unfinished", status: "todo", wantStatus: "todo"},
		{name: "primary unresolved wins", status: "done", primaryStatus: "open", wantStatus: "open"},
		{name: "primary resolved wins", status: "open", primaryStatus: "done", wantReady: true, wantStatus: "done"},
		{name: "ambiguous", status: "done", duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			h.cfg.SecondaryTicketsDirs = []string{h.tasksDir, dir, h.tasksDir}
			h.writeTicket("dep-a.md", h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"))
			if tc.primaryStatus != "" {
				h.writeTicket("dep-b.md", h.depTaskMD("dep-b", tc.primaryStatus, "2026-01-01T00:00:00Z"))
			}
			data := h.depTaskMD("dep-b", tc.status, "2026-01-01T00:00:00Z", "source-local")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "dep-b.md"), []byte(data), 0o644))
			if tc.duplicate {
				other := t.TempDir()
				h.cfg.SecondaryTicketsDirs = append(h.cfg.SecondaryTicketsDirs, other)
				require.NoError(t, os.WriteFile(filepath.Join(other, "dep-b.md"), []byte(data), 0o644))
			}
			d := h.newDaemon(h.cfg)
			stop, err := d.startSecondary(context.Background())
			require.NoError(t, err)
			defer stop()
			require.NoError(t, d.initialScan(h.tasksDir))

			info, err := d.GetTicket("dep-a")
			require.NoError(t, err)
			assert.Equal(t, tc.wantReady, info.Ready)
			assert.Equal(t, tc.wantStatus, info.Deps[0].Status)
			if tc.wantReady {
				assert.Equal(t, []string{"dep-a"}, d.queuedIDs())
			} else {
				assert.Empty(t, d.queuedIDs())
				assert.Equal(t, []string{"dep-b"}, info.Blockers)
			}
			chain, err := d.GetChain("dep-a")
			require.NoError(t, err)
			var prerequisite web.ChainNode
			for _, node := range chain.Nodes {
				if node.ID == "dep-b" {
					prerequisite = node
				}
			}
			assert.Equal(t, "dep-b", prerequisite.ID)
			assert.Equal(t, tc.wantStatus == "", prerequisite.Missing)
			assert.Equal(t, tc.wantStatus, prerequisite.Status)
			assert.Equal(t, tc.wantReady, prerequisite.Resolved)
			if tc.wantStatus != "" {
				assert.Equal(t, "Test ticket dep-b", prerequisite.Title)
			}
			if tc.primaryStatus == "" && !tc.duplicate {
				assert.NotEmpty(t, prerequisite.Source)
				assert.Equal(t, prerequisite.Source, info.Deps[0].Source)
				assert.Zero(t, prerequisite.WaitsOn.Total)
				assert.Len(t, d.tickets, 1)
			} else {
				assert.Empty(t, prerequisite.Source)
			}
		})
	}
}

func TestSecondaryDependencyRefresh(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		remove, unavailable bool
		wantReady           bool
	}{
		{name: "completion queues", before: "open", after: "done", wantReady: true},
		{name: "reopening removes queued work", before: "done", after: "open"},
		{name: "deletion blocks", before: "done", remove: true},
		{name: "unavailable source blocks", before: "done", unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			h.cfg.SecondaryTicketsDirs = []string{h.tasksDir, dir, h.tasksDir}
			h.writeTicket("dep-a.md", h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"))
			path := filepath.Join(dir, "dep-b.md")
			write := func(status string) {
				require.NoError(t, os.WriteFile(path, []byte(h.depTaskMD("dep-b", status, "2026-01-01T00:00:00Z")), 0o644))
			}
			write(tc.before)
			d := h.newDaemon(h.cfg)
			sources, err := h.cfg.SecondarySources()
			require.NoError(t, err)
			source := sources[0].ID
			catalog := &secondaryCatalog{source: web.TicketSourceInfo{ID: source, Path: dir}}
			catalog.observer.observe = func() (map[string]web.TicketInfo, error) {
				return d.observeAndPublishSecondary(source, dir)
			}
			d.secondary = map[string]*secondaryCatalog{source: catalog}
			d.refreshSecondary(source)
			require.NoError(t, d.initialScan(h.tasksDir))
			if tc.remove {
				require.NoError(t, os.Remove(path))
			} else if !tc.unavailable {
				write(tc.after)
			}
			if tc.unavailable {
				d.setSecondaryUnavailable(source, errors.New("source offline"))
			} else {
				d.refreshSecondary(source)
			}
			info, err := d.GetTicket("dep-a")
			require.NoError(t, err)
			assert.Equal(t, tc.wantReady, info.Ready)
			if tc.wantReady {
				assert.Equal(t, []string{"dep-a"}, d.queuedIDs())
			} else {
				assert.Empty(t, d.queuedIDs())
			}
		})
	}
}

func TestSecondaryDependencyWatcherStartsPrimaryOnly(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.cfg.SecondaryTicketsDirs = []string{h.tasksDir, dir, h.tasksDir}
	h.writeTicket("dep-a.md", h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"))
	path := filepath.Join(dir, "dep-b.md")
	require.NoError(t, os.WriteFile(path, []byte(h.depTaskMD("dep-b", "open", "2026-01-01T00:00:00Z")), 0o644))
	secondaryTodo := []byte(h.depTaskMD("secondary-todo", "todo", "2026-01-01T00:00:00Z"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secondary-todo.md"), secondaryTodo, 0o644))
	launched := make(chan string, 4)
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		launched <- p.TicketID
		publishCompleteResult(t, p)
		return process.Result{Termination: process.TerminationProcessGroupGone}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	require.Eventually(t, func() bool {
		info, err := d.GetTicket("dep-a")
		return err == nil && !info.Ready
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte(h.depTaskMD("dep-b", "done", "2026-01-01T00:00:00Z")), 0o644))
	select {
	case id := <-launched:
		assert.Equal(t, "dep-a", id)
	case <-time.After(5 * time.Second):
		t.Fatal("secondary completion did not start the primary dependent")
	}
	cancel()
	require.NoError(t, <-errCh)
	assert.Empty(t, launched)
	data, err := os.ReadFile(filepath.Join(dir, "secondary-todo.md"))
	require.NoError(t, err)
	assert.Equal(t, secondaryTodo, data)
}

func TestExecutionClaimRechecksSecondaryDependency(t *testing.T) {
	for _, tc := range []struct {
		name, fresh string
		remove      bool
		wantClaim   bool
	}{
		{name: "done", fresh: "done", wantClaim: true},
		{name: "reopened before watcher", fresh: "open"},
		{name: "deleted before watcher", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			h.cfg.SecondaryTicketsDirs = []string{h.tasksDir, dir, h.tasksDir}
			h.writeTicket("dep-a.md", h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"))
			path := filepath.Join(dir, "dep-b.md")
			require.NoError(t, os.WriteFile(path, []byte(h.depTaskMD("dep-b", "done", "2026-01-01T00:00:00Z")), 0o644))
			d := h.newDaemon(h.cfg)
			sources, err := h.cfg.SecondarySources()
			require.NoError(t, err)
			source := sources[0].ID
			catalog := &secondaryCatalog{source: web.TicketSourceInfo{ID: source, Path: dir}}
			catalog.observer.observe = func() (map[string]web.TicketInfo, error) {
				return d.observeAndPublishSecondary(source, dir)
			}
			d.secondary = map[string]*secondaryCatalog{source: catalog}
			d.refreshSecondary(source)
			require.NoError(t, d.initialScan(h.tasksDir))
			require.Equal(t, []string{"dep-a"}, d.queuedIDs())
			if tc.remove {
				require.NoError(t, os.Remove(path))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(h.depTaskMD("dep-b", tc.fresh, "2026-01-01T00:00:00Z")), 0o644))
			}
			ctx, cancel, _, filePath, branchKey, err := d.claimExecution(context.Background(), h.cfg, "dep-a", testLogger(t))
			if tc.wantClaim {
				require.NoError(t, err)
				d.finishOrdinaryRun(ctx, cancel, "dep-a", branchKey, filePath, testLogger(t))
			} else {
				require.ErrorIs(t, err, errExecutionChanged)
				assert.Empty(t, d.running)
			}
		})
	}
}
