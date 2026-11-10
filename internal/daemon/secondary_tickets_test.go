package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestSecondaryObservation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		secondary bool
	}{
		{name: "primary aliases only"},
		{name: "primary aliases and secondaries", secondary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			missing := filepath.Join(t.TempDir(), "missing")
			cfg := h.cfg
			primaryLink := filepath.Join(t.TempDir(), "primary-link")
			require.NoError(t, os.Symlink(h.tasksDir, primaryLink))
			cfg.SecondaryTicketsDirs = []string{h.tasksDir, primaryLink}
			wantSources := 0
			if tc.secondary {
				cfg.SecondaryTicketsDirs = append(cfg.SecondaryTicketsDirs, dir, missing)
				wantSources = 2
			}
			h.writeTicket("primary.md", h.taskMD("primary", "open", "one-stage"))
			contents := map[string][]byte{}
			for _, tc := range []struct{ id, status, extra string }{
				{"queued", "todo", ""},
				{"running", "in_progress", "claimed_by: test-instance\n"},
				{"scheduled", "open", "scheduled_at: 2020-01-01T00:00:00Z\n"},
				{"epic", "in_progress", "kind: epic\nepic_execution: agent\n"},
				{"archived", "archived", ""},
				{"foreign", "outside", "kontora: false\n"},
			} {
				path := filepath.Join(dir, tc.id+".md")
				enrollment := "kontora: true\n"
				if tc.id == "foreign" {
					enrollment = ""
				}
				data := []byte(fmt.Sprintf("---\nid: %s\nstatus: %s\n%spipeline: one-stage\n%s---\n# %s\n", tc.id, tc.status, enrollment, tc.extra, tc.id))
				require.NoError(t, os.WriteFile(path, data, 0o644))
				contents[path] = data
			}
			var launches atomic.Int32
			d := h.newDaemon(cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				launches.Add(1)
				return process.Result{}, nil
			}))
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			require.Eventually(t, func() bool {
				sources := d.ListTicketSources()
				if len(sources) != wantSources || len(d.ListTickets(web.ListTicketsOptions{})) != 1 {
					return false
				}
				for _, source := range sources {
					if !source.Available && source.Error == "" {
						return false
					}
				}
				return true
			}, 5*time.Second, 10*time.Millisecond)
			sources := d.ListTicketSources()
			listedPrimary := d.ListTickets(web.ListTicketsOptions{})
			require.Len(t, listedPrimary, 1)
			assert.Equal(t, "primary", listedPrimary[0].ID)
			var source string
			for _, s := range sources {
				primary, err := config.DirectoryIdentity(h.tasksDir)
				require.NoError(t, err)
				assert.NotEqual(t, primary, s.Path, "primary must not appear in secondary inventory")
				if same, _ := config.SameDirectory(s.Path, dir); same {
					source = s.ID
					assert.True(t, s.Available)
				} else {
					assert.False(t, s.Available)
					assert.NotEmpty(t, s.Error)
				}
			}
			if tc.secondary {
				require.NotEmpty(t, source)
				listed, err := d.ListSecondaryTickets(source)
				require.NoError(t, err)
				assert.Len(t, listed, 6)
				require.Eventually(t, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return len(d.tickets) == 1 }, time.Second, 10*time.Millisecond)
				for path, data := range contents {
					actual, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, data, actual)
				}
				path := filepath.Join(dir, "queued.md")
				require.NoError(t, os.WriteFile(path, []byte("---\nid: queued\nstatus: paused\n---\n# Changed\n"), 0o644))
				require.Eventually(t, func() bool {
					d.secondaryMu.Lock()
					defer d.secondaryMu.Unlock()
					c := d.secondary[source]
					return c != nil && c.tickets["queued"] != nil && c.tickets["queued"].ticket.Status == ticket.StatusPaused
				}, 5*time.Second, 10*time.Millisecond)
				require.NoError(t, os.Remove(path))
				require.Eventually(t, func() bool {
					d.secondaryMu.Lock()
					defer d.secondaryMu.Unlock()
					return d.secondary[source].tickets["queued"] == nil
				}, 5*time.Second, 10*time.Millisecond)
			}
			cancel()
			require.NoError(t, <-errCh)
			assert.Zero(t, launches.Load())
			_, err := os.Stat(missing)
			assert.True(t, os.IsNotExist(err))
			entries, err := os.ReadDir(h.wtDir)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestSecondaryEditingIsSourceLocal(t *testing.T) {
	for _, status := range []string{"open", "todo", "in_progress", "done", "archived", "custom"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			h.cfg.SecondaryTicketsDirs = []string{dir}
			primary := h.writeTicket("same.md", "---\nid: same\nstatus: open\n---\n# Primary\n")
			primaryBefore, err := os.ReadFile(primary)
			require.NoError(t, err)
			path := filepath.Join(dir, "same.md")
			require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("---\nid: same\nstatus: %s\nstage: step1\nattempt: 3\ninvocation_id: retained\nscheduled_at: 2020-01-01T00:00:00Z\nhistory:\n - {stage: step1, summary: preserved}\n---\n# Secondary\n", status)), 0o644))
			d := h.newDaemon(h.cfg)
			stop, err := d.startSecondary(context.Background())
			require.NoError(t, err)
			defer stop()
			source := d.ListTicketSources()[0].ID
			description := "# Edited secondary\n"
			require.NoError(t, d.UpdateSecondaryTicket(source, "same", web.UpdateTicketRequest{Description: &description}))
			require.NoError(t, d.MutateSecondaryTicket(source, "same", web.SecondaryMutation{Action: web.SecondaryAddNote, Text: "Source-only note", Author: "person"}))
			st, err := ticket.ParseFile(path)
			require.NoError(t, err)
			assert.Equal(t, ticket.Status(status), st.Status)
			assert.Equal(t, "step1", st.Stage)
			assert.Equal(t, 3, st.Attempt)
			assert.Equal(t, "retained", st.InvocationID)
			assert.Equal(t, "2020-01-01T00:00:00Z", st.ScheduledAt)
			require.Len(t, st.History, 1)
			assert.Equal(t, "preserved", st.History[0].Summary)
			assert.Contains(t, st.Body, "Source-only note")
			primaryAfter, err := os.ReadFile(primary)
			require.NoError(t, err)
			assert.Equal(t, primaryBefore, primaryAfter)
			info, err := d.GetSecondaryTicket(source, "same")
			require.NoError(t, err)
			assert.Equal(t, source, info.Source)
			assert.True(t, info.CanEdit)
			assert.False(t, info.CanControl)
			assert.False(t, info.CanClaim)
			assert.Empty(t, d.ListTickets(web.ListTicketsOptions{}))
		})
	}
}

func TestSecondaryClaim(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		conflict    bool
	}{
		{name: "idle"},
		{name: "running", extra: "status: in_progress\n", conflict: true},
		{name: "invocation", extra: "invocation_id: active\n", conflict: true},
		{name: "annotation", extra: "annotation_return_status: open\n", conflict: true},
		{name: "stale claim", extra: "claimed_by: old-daemon\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			h.cfg.SecondaryTicketsDirs = []string{dir}
			status := "status: open\n"
			if tc.name == "running" {
				status = ""
			}
			path := filepath.Join(dir, "claim.md")
			data := []byte("---\nid: claim\n" + status + tc.extra + "stage: step1\nattempt: 4\nscheduled_at: 2020-01-01T00:00:00Z\nstarted_at: 2020-01-01T00:00:00Z\nhistory:\n - {stage: step1, summary: imported}\ncustom: kept\n---\n# Claim me\n")
			require.NoError(t, os.WriteFile(path, data, 0o644))
			d := h.newDaemon(h.cfg)
			stop, err := d.startSecondary(context.Background())
			require.NoError(t, err)
			defer stop()
			source := d.ListTicketSources()[0].ID
			info, err := d.GetSecondaryTicket(source, "claim")
			require.NoError(t, err)
			got, err := d.ClaimSecondaryTicket(source, "claim", info.Revision)
			if tc.conflict {
				require.Error(t, err)
				actual, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, data, actual)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, got.Source)
			assert.Equal(t, "open", got.Status)
			assert.Empty(t, got.Stage)
			assert.Empty(t, got.ClaimedBy)
			assert.Empty(t, got.ScheduledAt)
			assert.Nil(t, got.StartedAt)
			assert.Zero(t, got.Attempt)
			require.Len(t, got.History, 1)
			assert.True(t, got.History[0].Imported)
			_, err = os.Stat(path)
			assert.True(t, os.IsNotExist(err))
			d.mu.Lock()
			assert.Empty(t, d.running)
			assert.Empty(t, d.queue)
			d.mu.Unlock()
		})
	}
}

func TestSecondaryAssistantRefusal(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg, WithTurnSpawner(func(context.Context, TurnParams) (string, error) {
		t.Error("secondary context spawned a turn")
		return "", nil
	}))
	err := d.PostAssistantMessage("missing-thread", web.AssistantMessageRequest{Text: "inspect this", Source: "secondary-folder"})
	assert.ErrorIs(t, err, web.ErrInvalidState)
}
