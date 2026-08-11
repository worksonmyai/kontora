package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestDaemon_ListTickets(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h.writeTicket("tst-l01.md", h.taskMD("tst-l01", "todo", "one-stage"))
	h.writeTicket("tst-l02.md", h.taskMD("tst-l02", "open", "two-stage"))

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	// Wait for tst-l01 to complete (proves daemon is running).
	h.waitForStatus("tst-l01.md", ticket.StatusDone, 10*time.Second)

	// Poll in-memory state — d.tickets is updated after worktree cleanup,
	// which runs after the file is written.
	require.Eventually(t, func() bool {
		info, err := d.GetTicket("tst-l01")
		return err == nil && info.Status == "done"
	}, 5*time.Second, 50*time.Millisecond, "tst-l01 should be done in d.tickets")

	tickets := d.ListTickets()
	require.Len(t, tickets, 2)

	ids := map[string]web.TicketInfo{}
	for _, ti := range tickets {
		ids[ti.ID] = ti
	}

	assert.Equal(t, "done", ids["tst-l01"].Status)
	assert.Equal(t, "open", ids["tst-l02"].Status)
	assert.Equal(t, []string{"step1"}, ids["tst-l01"].Stages)
	assert.Equal(t, []string{"step1", "step2"}, ids["tst-l02"].Stages)

	// UpdatedAt is derived from the markdown file's mtime and should be
	// populated for every ticket the daemon tracks.
	for _, id := range []string{"tst-l01", "tst-l02"} {
		require.NotNil(t, ids[id].UpdatedAt, "%s should have UpdatedAt set", id)
		path := filepath.Join(h.tasksDir, id+".md")
		st, err := os.Stat(path)
		require.NoError(t, err)
		assert.True(t, ids[id].UpdatedAt.Equal(st.ModTime()),
			"%s UpdatedAt %s should match file mtime %s", id, ids[id].UpdatedAt, st.ModTime())
	}

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_GetTicket(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h.writeTicket("tst-g01.md", h.taskMD("tst-g01", "open", "one-stage"))

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	info, err := d.GetTicket("tst-g01")
	require.NoError(t, err)
	assert.Equal(t, "tst-g01", info.ID)
	assert.Equal(t, "open", info.Status)
	assert.Contains(t, info.Body, "Test ticket tst-g01")

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_GetTicket_NotFound(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	_, err := d.GetTicket("nonexistent")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_PauseTicket(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-p01.md", h.taskMD("tst-p01", "todo", "one-stage"))
	h.waitForStatus("tst-p01.md", ticket.StatusInProgress, 5*time.Second)

	err := d.PauseTicket("tst-p01")
	require.NoError(t, err)

	h.waitForStatus("tst-p01.md", ticket.StatusPaused, 5*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_RetryTicket(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-r01.md", h.taskMD("tst-r01", "todo", "one-stage"))
	h.waitForStatus("tst-r01.md", ticket.StatusInProgress, 5*time.Second)

	// Pause first, then retry.
	require.NoError(t, d.PauseTicket("tst-r01"))
	h.waitForStatus("tst-r01.md", ticket.StatusPaused, 5*time.Second)

	require.NoError(t, d.RetryTicket("tst-r01"))

	// Should become running again.
	h.waitForStatus("tst-r01.md", ticket.StatusInProgress, 5*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_SkipStage(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "true")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	cfg.Stages["step2"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-s01.md", h.taskMD("tst-s01", "todo", "two-stage"))
	h.waitForStatus("tst-s01.md", ticket.StatusInProgress, 5*time.Second)

	// Pause first (can't skip a running ticket without pausing).
	require.NoError(t, d.PauseTicket("tst-s01"))
	h.waitForStatus("tst-s01.md", ticket.StatusPaused, 5*time.Second)

	// Skip step1 → step2.
	require.NoError(t, d.SkipStage("tst-s01"))

	// Agent2 is "true" so it should complete quickly.
	h.waitForStatus("tst-s01.md", ticket.StatusDone, 10*time.Second)

	result := h.readTask("tst-s01.md")
	assert.Equal(t, "step2", result.Stage)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_SetStage(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "true")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	cfg.Stages["step2"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Start ticket at step2 by writing it with stage=step2.
	h.writeTicket("tst-ss1.md", fmt.Sprintf(`---
id: tst-ss1
kontora: true
status: paused
pipeline: two-stage
stage: step2
path: %s
created: 2026-01-01T00:00:00Z
---
# Test set-stage
`, h.repoDir))

	// Wait for daemon to discover the ticket.
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-ss1")
		return err == nil
	}, 5*time.Second, 50*time.Millisecond)

	// Set stage back to step1.
	require.NoError(t, d.SetStage("tst-ss1", "step1"))

	// Verify only the stage changed — status and attempt stay untouched.
	result := h.readTask("tst-ss1.md")
	assert.Equal(t, "step1", result.Stage)
	assert.Equal(t, ticket.StatusPaused, result.Status)

	// Invalid stage should return error.
	err := d.SetStage("tst-ss1", "nonexistent")
	assert.ErrorIs(t, err, web.ErrInvalidState)

	// Not found ticket should return error.
	err = d.SetStage("nonexistent", "step1")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_MoveTicket(t *testing.T) {
	cases := []struct {
		name        string
		initial     string
		newStatus   string
		wantErr     error
		checkStatus bool // false when daemon may race the status forward
	}{
		{name: "open to todo", initial: "open", newStatus: "todo"},
		{name: "done to todo", initial: "done", newStatus: "todo"},
		{name: "cancelled to todo", initial: "cancelled", newStatus: "todo"},
		{name: "open to done", initial: "open", newStatus: "done", checkStatus: true},
		{name: "open to cancelled", initial: "open", newStatus: "cancelled", checkStatus: true},
		{name: "invalid status", initial: "open", newStatus: "invalid", wantErr: web.ErrInvalidState},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			ticketID := fmt.Sprintf("tst-m%s", tc.newStatus[:2])
			h.writeTicket(ticketID+".md", h.taskMD(ticketID, tc.initial, "one-stage"))

			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			err := d.MoveTicket(ticketID, tc.newStatus)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				if tc.checkStatus {
					result := h.readTask(ticketID + ".md")
					assert.Equal(t, ticket.Status(tc.newStatus), result.Status)
				}
			}

			cancel()
			require.NoError(t, <-errCh)
		})
	}

	t.Run("open to done sets kontora", func(t *testing.T) {
		h := newHarness(t)
		d := h.newDaemon(h.cfg)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		h.writeTicket("tst-mnk.md", `---
id: tst-mnk
status: open
created: 2026-01-01T00:00:00Z
---
# Test ticket tst-mnk
`)

		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()
		time.Sleep(200 * time.Millisecond)

		require.NoError(t, d.MoveTicket("tst-mnk", "done"))

		result := h.readTask("tst-mnk.md")
		assert.Equal(t, ticket.StatusDone, result.Status)
		assert.True(t, result.Kontora, "kontora should be set after move")

		cancel()
		require.NoError(t, <-errCh)
	})

	t.Run("move to custom status", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Statuses = []string{"review", "qa"}
		d := h.newDaemon(h.cfg)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		h.writeTicket("tst-mcs.md", h.taskMD("tst-mcs", "open", "one-stage"))

		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()
		time.Sleep(200 * time.Millisecond)

		require.NoError(t, d.MoveTicket("tst-mcs", "review"))
		result := h.readTask("tst-mcs.md")
		assert.Equal(t, ticket.Status("review"), result.Status)

		cancel()
		require.NoError(t, <-errCh)
	})

	t.Run("reject unknown status with custom statuses configured", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Statuses = []string{"review", "qa"}
		d := h.newDaemon(h.cfg)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		h.writeTicket("tst-mru.md", h.taskMD("tst-mru", "open", "one-stage"))

		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()
		time.Sleep(200 * time.Millisecond)

		err := d.MoveTicket("tst-mru", "bogus")
		assert.ErrorIs(t, err, web.ErrInvalidState)

		cancel()
		require.NoError(t, <-errCh)
	})
}

func TestDaemon_DeleteTicket(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-del.md", h.taskMD("tst-del", "open", "one-stage"))

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	ch, unsub := d.Subscribe()
	defer unsub()

	err := d.DeleteTicket("tst-del")
	require.NoError(t, err)

	_, statErr := os.Stat(h.tasksDir + "/tst-del.md")
	assert.ErrorIs(t, statErr, os.ErrNotExist)

	_, err = d.GetTicket("tst-del")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)

	select {
	case ev := <-ch:
		assert.Equal(t, "ticket_deleted", ev.Type)
		assert.Equal(t, "tst-del", ev.Ticket.ID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ticket_deleted event")
	}

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_DeleteTicket_RejectsOutsideTicketsDir(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	outsidePath := filepath.Join(t.TempDir(), "tst-outside.md")
	require.NoError(t, os.WriteFile(outsidePath, []byte(h.taskMD("tst-outside", "open", "one-stage")), 0o644))

	tkt, err := ticket.ParseFile(outsidePath)
	require.NoError(t, err)

	d.tickets[tkt.ID] = &ticketState{ticket: tkt, filePath: outsidePath}

	err = d.DeleteTicket(tkt.ID)
	require.ErrorContains(t, err, "outside tickets dir")

	_, statErr := os.Stat(outsidePath)
	require.NoError(t, statErr)
}

func TestDaemon_Subscribe_ReceivesUpdates(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	ch, unsub := d.Subscribe()
	defer unsub()

	h.writeTicket("tst-sub.md", fmt.Sprintf(`---
id: tst-sub
kontora: true
status: todo
pipeline: one-stage
path: %s
created: 2026-01-01T00:00:00Z
summary: what the stage did
last_error: it failed once
last_log: step1.log
history:
  - stage: step1
    agent: agent1
    exit_code: 0
    run: 0
---
# Test ticket tst-sub

Body text the board never renders.

## Notes

**2026-01-01T00:00:00Z**

a note
`, h.repoDir))

	// Should receive at least one event about tst-sub.
	deadline := time.After(10 * time.Second)
	var got web.TicketInfo
	for got.ID == "" {
		select {
		case ev := <-ch:
			if ev.Ticket.ID == "tst-sub" {
				got = ev.Ticket
			}
		case <-deadline:
			t.Fatal("timed out waiting for SSE event")
		}
	}

	assert.Empty(t, got.Body, "ticket_updated must not carry the body")
	// The board sorts human_review by history, and the open detail panel
	// renders the rest of these straight off the event.
	assert.NotEmpty(t, got.History)
	assert.NotEmpty(t, got.Notes)
	assert.Equal(t, "what the stage did", got.Summary)
	assert.Equal(t, "it failed once", got.LastError)
	assert.Equal(t, "step1.log", got.LastLog)

	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"body"`)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_WebServerStarts(t *testing.T) {
	h := newHarness(t)
	enabled := true
	h.cfg.Web = config.Web{Enabled: &enabled, Host: "127.0.0.1", Port: 0}
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-ws1.md", h.taskMD("tst-ws1", "open", "one-stage"))

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// The server started with port 0 — we need to find the actual port.
	// We can't easily get it from the daemon since Server is local to Run().
	// Instead, we verify the daemon doesn't fail to start (which covers
	// the lifecycle test). For full HTTP testing, see below.
	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_WebServerDisabled(t *testing.T) {
	h := newHarness(t)
	disabled := false
	h.cfg.Web = config.Web{Enabled: &disabled}
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Daemon runs fine without web server.
	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_WebServerAPI(t *testing.T) {
	h := newHarness(t)
	enabled := true
	h.cfg.Web = config.Web{Enabled: &enabled, Host: "127.0.0.1", Port: 0}

	// We need to expose the server address somehow. Since the Server is
	// created inside Run(), we'll start the web server ourselves to test
	// the full integration.
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h.writeTicket("tst-api.md", h.taskMD("tst-api", "open", "one-stage"))

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// Create our own web server pointing at the daemon.
	srv := web.New(d, d.broker, "127.0.0.1", 0, "", d.tmuxSession, testLogger(t))
	require.NoError(t, srv.Start())
	defer func() { _ = srv.Shutdown(context.Background()) }()

	addr := srv.Addr()

	// GET /api/tickets
	resp, err := http.Get(fmt.Sprintf("http://%s/api/tickets", addr))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result struct{ Tickets []web.TicketInfo }
	require.NoError(t, json.Unmarshal(body, &result))
	require.Len(t, result.Tickets, 1)
	assert.Equal(t, "tst-api", result.Tickets[0].ID)
	assert.Equal(t, "open", result.Tickets[0].Status)

	// GET /api/tickets/{id}
	resp2, err := http.Get(fmt.Sprintf("http://%s/api/tickets/tst-api", addr))
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// GET /api/tickets/{id} - not found
	resp3, err := http.Get(fmt.Sprintf("http://%s/api/tickets/nonexistent", addr))
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp3.StatusCode)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_UpdateTicket(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-upd.md", `---
id: tst-upd
status: open
pipeline: one-stage
path: ~/old/path
created: 2026-01-01T00:00:00Z
---
# Original body
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	newBody := "# Updated body\n\nNew content.\n"
	newPipeline := "two-stage"
	newPath := "~/new/path"
	err := d.UpdateTicket("tst-upd", web.UpdateTicketRequest{
		Body:     &newBody,
		Pipeline: &newPipeline,
		Path:     &newPath,
	})
	require.NoError(t, err)

	result := h.readTask("tst-upd.md")
	assert.Equal(t, "# Updated body\n\nNew content.\n", result.Body)
	assert.Equal(t, "two-stage", result.Pipeline)
	assert.Equal(t, "~/new/path", result.Path)
	assert.Equal(t, ticket.StatusOpen, result.Status)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_AddNote(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-note.md", `---
id: tst-note
status: open
pipeline: one-stage
path: ~/some/path
created: 2026-01-01T00:00:00Z
---
# Note target
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	require.NoError(t, d.AddNote("tst-note", "blocked on review"))

	result := h.readTask("tst-note.md")
	assert.Contains(t, result.Body, "blocked on review")

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_AddNote_NotFound(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	err := d.AddNote("does-not-exist", "x")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_SetSummary(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-ssum.md", `---
id: tst-ssum
status: open
pipeline: one-stage
path: ~/some/path
created: 2026-01-01T00:00:00Z
---
# Summary target
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	require.NoError(t, d.SetSummary("tst-ssum", "implemented the fix"))

	result := h.readTask("tst-ssum.md")
	assert.Equal(t, "implemented the fix", result.Summary)

	err := d.SetSummary("does-not-exist", "x")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)

	cancel()
	require.NoError(t, <-errCh)
}

func TestNormalizeRemoteURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"scp syntax", "git@github.com:owner/repo.git", "https://github.com/owner/repo"},
		{"https with suffix", "https://github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"https without suffix", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"trailing slash", "https://github.com/owner/repo/", "https://github.com/owner/repo"},
		{"ssh scheme", "ssh://git@github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"ssh port dropped", "ssh://git@git.example.com:2222/owner/repo.git", "https://git.example.com/owner/repo"},
		{"git scheme", "git://github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"newline from git output", "git@github.com:owner/repo.git\n", "https://github.com/owner/repo"},
		{"nested group", "https://gitlab.com/group/sub/repo.git", "https://gitlab.com/group/sub/repo"},
		// The token belongs to the machine, not to everyone looking at the page.
		{"credentials stripped", "https://user:token@github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"local path", "/Users/a/projects/repo", ""},
		{"file url", "file:///Users/a/projects/repo", ""},
		{"windows path", "C:/projects/repo", ""},
		{"javascript url", "javascript:alert(1)//host/repo", ""},
		{"host only", "https://github.com", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeRemoteURL(tc.raw))
		})
	}
}

func TestDaemon_GetChanges(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A ticket whose branch has one commit ahead of main, one with no branch
	// field, and one whose branch does not exist in the repo.
	ticketMD := func(id, branchLine string) string {
		return fmt.Sprintf(`---
id: %s
status: open
pipeline: one-stage
path: %s
%screated: 2026-01-01T00:00:00Z
---
# Changes target %s
`, id, h.repoDir, branchLine, id)
	}
	h.writeTicket("tst-chg.md", ticketMD("tst-chg", "branch: feat-x\n"))
	h.writeTicket("tst-nob.md", ticketMD("tst-nob", ""))
	h.writeTicket("tst-gone.md", ticketMD("tst-gone", "branch: deleted-branch\n"))

	for _, args := range [][]string{
		{"remote", "add", "origin", "git@github.com:owner/repo.git"},
		{"checkout", "-b", "feat-x"},
		{"commit", "--allow-empty", "-m", "add feature"},
		{"checkout", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = h.repoDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	writeFileOnBranch(t, h.repoDir, "feat-x", "a.txt", "one\ntwo\n")

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	changes, err := d.GetChanges("tst-chg")
	require.NoError(t, err)
	assert.Equal(t, "main", changes.Base)
	assert.Equal(t, "feat-x", changes.Branch)
	require.Len(t, changes.Commits, 2)
	assert.Equal(t, "add a.txt", changes.Commits[0].Subject)
	assert.Equal(t, "add feature", changes.Commits[1].Subject)
	assert.NotEmpty(t, changes.Commits[0].SHA)
	require.Len(t, changes.Files, 1)
	assert.Equal(t, web.FileChangeInfo{Path: "a.txt", Added: 2, Deleted: 0}, changes.Files[0])
	assert.Equal(t, "https://github.com/owner/repo", changes.Remote)

	// No branch field: empty payload, not an error.
	changes, err = d.GetChanges("tst-nob")
	require.NoError(t, err)
	assert.Empty(t, changes.Branch)
	assert.Empty(t, changes.Commits)
	assert.Empty(t, changes.Files)

	// Branch missing from the repo: empty payload, not an error.
	changes, err = d.GetChanges("tst-gone")
	require.NoError(t, err)
	assert.Equal(t, "deleted-branch", changes.Branch)
	assert.Empty(t, changes.Commits)
	assert.Empty(t, changes.Files)

	_, err = d.GetChanges("does-not-exist")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)

	cancel()
	require.NoError(t, <-errCh)
}

// writeFileOnBranch commits a file on the given branch and returns to main.
func writeFileOnBranch(t *testing.T, repoDir, branch, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644))
	for _, args := range [][]string{
		{"checkout", branch},
		{"add", name},
		{"commit", "-m", "add " + name},
		{"checkout", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}

func TestDaemon_UpdateTicket_DoneRejects(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-uno.md", `---
id: tst-uno
status: done
kontora: true
pipeline: one-stage
stage: step1
path: `+h.repoDir+`
created: 2026-01-01T00:00:00Z
---
# Done ticket
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	newBody := "should fail"
	err := d.UpdateTicket("tst-uno", web.UpdateTicketRequest{Body: &newBody})
	assert.ErrorIs(t, err, web.ErrInvalidState)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_UpdateTicket_InvalidPipeline(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-uip.md", `---
id: tst-uip
status: open
created: 2026-01-01T00:00:00Z
---
# Ticket
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	badPipeline := "nonexistent-pipeline"
	err := d.UpdateTicket("tst-uip", web.UpdateTicketRequest{Pipeline: &badPipeline})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown pipeline")

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_InitTicket(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Write a non-kontora ticket.
	h.writeTicket("tst-init.md", `---
id: tst-init
status: open
created: 2026-01-01T00:00:00Z
---
# Ticket to init
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Should be visible but not kontora.
	info, err := d.GetTicket("tst-init")
	require.NoError(t, err)
	assert.Equal(t, "open", info.Status)
	assert.False(t, info.Kontora)

	// Init it.
	err = d.InitTicket("tst-init", web.InitTicketRequest{
		Pipeline: "one-stage",
		Path:     h.repoDir,
	})
	require.NoError(t, err)

	// Should now process through to done.
	result := h.waitForStatus("tst-init.md", ticket.StatusDone, 10*time.Second)
	assert.True(t, result.Kontora)
	assert.Equal(t, "one-stage", result.Pipeline)
	assert.Equal(t, h.repoDir, result.Path)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_InitTicket_AlreadyKontora(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-iak.md", h.taskMD("tst-iak", "open", "one-stage"))

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	err := d.InitTicket("tst-iak", web.InitTicketRequest{
		Pipeline: "one-stage",
		Path:     h.repoDir,
	})
	assert.ErrorIs(t, err, web.ErrInvalidState)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_RemoteInit_MovesForeignTicketToTodo(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	// Register a foreign (non-kontora) ticket without running the scheduler, so
	// the assertion observes the post-init state directly rather than the daemon
	// picking it up and racing forward.
	path := h.writeTicket("tst-rinit.md", `---
id: tst-rinit
status: open
created: 2026-01-01T00:00:00Z
---
# Foreign ticket
`)
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets["tst-rinit"] = newTicketState(tk, path)

	srv := web.New(d, d.broker, "127.0.0.1", 0, "", d.tmuxSession, testLogger(t))
	require.NoError(t, srv.Start())
	defer func() { _ = srv.Shutdown(context.Background()) }()

	c := remote.New("http://"+srv.Addr(), "")
	require.NoError(t, c.Init("tst-rinit", web.InitTicketRequest{
		Pipeline: "one-stage",
		Path:     h.repoDir,
	}))

	// The foreign ticket is now kontora-managed, todo, at the first stage.
	result := h.readTask("tst-rinit.md")
	assert.True(t, result.Kontora)
	assert.Equal(t, ticket.StatusTodo, result.Status)
	assert.Equal(t, "one-stage", result.Pipeline)
	assert.Equal(t, "step1", result.Stage)
	assert.Equal(t, h.repoDir, result.Path)
}

func TestDaemon_RawConfig(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	const valid = "agents:\n  claude:\n    binary: claude\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(valid), 0o644))
	d.configPath = cfgPath

	got, err := d.GetRawConfig()
	require.NoError(t, err)
	assert.Equal(t, valid, got)

	// A valid replacement is written to disk and reloaded before the call
	// returns. Live fields take effect; restart-only fields keep their running
	// value (max_concurrent_agents is pinned, branch_prefix is not).
	inMemoryBefore := d.config()
	const updated = "max_concurrent_agents: 7\nbranch_prefix: reloaded\nagents:\n  claude:\n    binary: claude\n"
	require.NoError(t, d.PutRawConfig(updated))

	onDisk, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, updated, string(onDisk))
	assert.NotSame(t, inMemoryBefore, d.config(), "a saved config must be reloaded in memory")
	assert.Equal(t, "reloaded", d.config().BranchPrefix, "live fields take effect immediately")
	assert.Equal(t, h.cfg.MaxConcurrentAgents, d.config().MaxConcurrentAgents,
		"max_concurrent_agents is restart-only and stays pinned")

	// An invalid config is rejected before the write, so the on-disk file and
	// the running config are both left intact.
	afterSave := d.config()
	err = d.PutRawConfig("max_concurrent_agents: 3\nunknown_field: x\n")
	require.ErrorIs(t, err, web.ErrInvalidConfig)
	onDiskAfter, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, updated, string(onDiskAfter), "rejected config must not overwrite the file")
	assert.Same(t, afterSave, d.config(), "rejected config must not change the running config")
}

func TestDaemon_RawConfig_NoPathConfigured(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg) // no WithConfigPath

	_, err := d.GetRawConfig()
	require.ErrorIs(t, err, web.ErrConfigPathNotSet)

	err = d.PutRawConfig("agents:\n  claude:\n    binary: claude\n")
	require.ErrorIs(t, err, web.ErrConfigPathNotSet)
}

func TestDaemon_GetConfig_ReturnsAgents(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	cfg := d.GetConfig()
	assert.Equal(t, []string{"agent1", "agent2"}, cfg.Agents)
}

func TestDaemon_CreateTicket_WithAgent(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	info, err := d.CreateTicket(web.CreateTicketRequest{
		Title:  "Agent ticket",
		Path:   h.repoDir,
		Agent:  "agent1",
		Status: "open",
	})
	require.NoError(t, err)
	assert.Equal(t, "agent1", info.Agent)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_CreateTicket_WithBody(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	info, err := d.CreateTicket(web.CreateTicketRequest{
		Title:  "Body ticket",
		Path:   h.repoDir,
		Body:   "Ticket description.\n\nWith paragraphs.",
		Status: "open",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, info.ID)

	// Verify the file on disk contains the body.
	result := h.readTask(info.ID + ".md")
	assert.Contains(t, result.Body, "Ticket description.")
	assert.Contains(t, result.Body, "With paragraphs.")

	cancel()
	require.NoError(t, <-errCh)
}

// TestDaemon_CreateTicket_FailureClearsReservation: creation reserves the
// ticket path before the CLI writes it. When the write never happens, the
// reservation must go too: the id is handed out again, and the next ticket's
// creation event has to be acted on.
func TestDaemon_CreateTicket_FailureClearsReservation(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	// A path that is not a git repository fails inside the CLI, after the
	// daemon has reserved the file.
	_, err := d.CreateTicket(web.CreateTicketRequest{
		Title:  "Not a repo",
		Path:   t.TempDir(),
		Status: "todo",
	})
	require.Error(t, err)

	d.selfWritesMu.Lock()
	defer d.selfWritesMu.Unlock()
	assert.Empty(t, d.selfWrites, "a failed creation must leave no suppression behind")
}

func TestDaemon_CreateTicket_UnknownAgent(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	_, err := d.CreateTicket(web.CreateTicketRequest{
		Title:  "Bad agent",
		Path:   h.repoDir,
		Agent:  "nonexistent",
		Status: "open",
	})
	require.ErrorIs(t, err, web.ErrUnknownAgent)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_CreateTicket_UnknownPipeline(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	_, err := d.CreateTicket(web.CreateTicketRequest{
		Title:    "Bad pipeline",
		Path:     h.repoDir,
		Pipeline: "nonexistent",
		Status:   "open",
	})
	require.ErrorIs(t, err, web.ErrUnknownPipeline)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_InitTicket_WithAgent(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h.writeTicket("tst-ia.md", `---
id: tst-ia
status: open
created: 2026-01-01T00:00:00Z
---
# Ticket to init with agent
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	err := d.InitTicket("tst-ia", web.InitTicketRequest{
		Pipeline: "one-stage",
		Path:     h.repoDir,
		Agent:    "agent2",
	})
	require.NoError(t, err)

	// The ticket should have the agent field set.
	result := h.readTask("tst-ia.md")
	assert.Equal(t, "agent2", result.Agent)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_InitTicket_UnknownAgent(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-iua.md", `---
id: tst-iua
status: open
created: 2026-01-01T00:00:00Z
---
# Ticket with bad agent
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	err := d.InitTicket("tst-iua", web.InitTicketRequest{
		Pipeline: "one-stage",
		Path:     h.repoDir,
		Agent:    "nonexistent",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown agent")

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_UpdateTicket_Agent(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{"open", "open"},
		{"paused", "paused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("sleep", "sleep")
			cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
			cfg.Stages["step1"] = config.Stage{Prompt: ""}
			d := h.newDaemon(cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			ticketID := "tst-ua" + tc.status[:2]
			h.writeTicket(ticketID+".md", fmt.Sprintf(`---
id: %s
status: %s
kontora: true
pipeline: one-stage
stage: step1
path: %s
created: 2026-01-01T00:00:00Z
---
# Agent update test
`, ticketID, tc.status, h.repoDir))

			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			agent := "agent2"
			err := d.UpdateTicket(ticketID, web.UpdateTicketRequest{Agent: &agent})
			require.NoError(t, err)

			result := h.readTask(ticketID + ".md")
			assert.Equal(t, "agent2", result.Agent)

			// Verify AgentOverride in API response
			info, err := d.GetTicket(ticketID)
			require.NoError(t, err)
			assert.True(t, info.AgentOverride)

			// Clear agent
			empty := ""
			err = d.UpdateTicket(ticketID, web.UpdateTicketRequest{Agent: &empty})
			require.NoError(t, err)

			result = h.readTask(ticketID + ".md")
			assert.Equal(t, "", result.Agent)

			info, err = d.GetTicket(ticketID)
			require.NoError(t, err)
			assert.False(t, info.AgentOverride)

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

func TestDaemon_UpdateTicket_AgentUnknown(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-uau.md", `---
id: tst-uau
status: open
created: 2026-01-01T00:00:00Z
---
# Ticket
`)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	badAgent := "nonexistent"
	err := d.UpdateTicket("tst-uau", web.UpdateTicketRequest{Agent: &badAgent})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown agent")

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_UpdateTicket_InProgressRejects(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-uip.md", h.taskMD("tst-uip", "todo", "one-stage"))
	h.waitForStatus("tst-uip.md", ticket.StatusInProgress, 5*time.Second)

	agent := "agent2"
	err := d.UpdateTicket("tst-uip", web.UpdateTicketRequest{Agent: &agent})
	assert.ErrorIs(t, err, web.ErrInvalidState)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_UpdateTicket_CustomStatus(t *testing.T) {
	h := newHarness(t)
	h.cfg.Statuses = []string{"review", "qa"}
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h.writeTicket("tst-ucs.md", h.taskMD("tst-ucs", "review", "one-stage"))

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	newBody := "# Updated from custom status\n"
	err := d.UpdateTicket("tst-ucs", web.UpdateTicketRequest{Body: &newBody})
	require.NoError(t, err)

	result := h.readTask("tst-ucs.md")
	assert.Equal(t, "# Updated from custom status\n", result.Body)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_UpdateTicket_UnknownStatusRejects(t *testing.T) {
	h := newHarness(t)
	h.cfg.Statuses = []string{"review"}
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Manually write a ticket with a status that is not in the configured custom statuses.
	h.writeTicket("tst-uuk.md", h.taskMD("tst-uuk", "bogus", "one-stage"))

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	newBody := "should fail"
	err := d.UpdateTicket("tst-uuk", web.UpdateTicketRequest{Body: &newBody})
	assert.ErrorIs(t, err, web.ErrInvalidState)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_InitTicket_NotFound(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	err := d.InitTicket("nonexistent", web.InitTicketRequest{
		Pipeline: "one-stage",
		Path:     h.repoDir,
	})
	assert.ErrorIs(t, err, web.ErrTicketNotFound)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_GetConfig_ReturnsProjectsSortedByName(t *testing.T) {
	h := newHarness(t)
	h.cfg.Projects = map[string]config.Project{
		"sigil":   {Path: "~/projects/sigil", Pipeline: "one-stage"},
		"kontora": {Path: h.repoDir, Pipeline: "two-stage", Agent: "agent2"},
	}
	d := h.newDaemon(h.cfg)

	got := d.GetConfig().Projects
	require.Len(t, got, 2)
	assert.Equal(t, web.ProjectInfo{
		Name:         "kontora",
		Path:         h.repoDir,
		ResolvedPath: h.repoDir,
		Pipeline:     "two-stage",
		Agent:        "agent2",
	}, got[0])
	assert.Equal(t, web.ProjectInfo{
		Name:         "sigil",
		Path:         "~/projects/sigil",
		ResolvedPath: config.NormalizeRepoPath("~/projects/sigil"),
		Pipeline:     "one-stage",
	}, got[1])
}

func TestDaemon_CreateTicket_ProjectDefaults(t *testing.T) {
	cases := []struct {
		name         string
		req          web.CreateTicketRequest
		wantPipeline string
		wantAgent    string
	}{
		{
			name:         "blank fields take project defaults",
			req:          web.CreateTicketRequest{Title: "Inherited"},
			wantPipeline: "two-stage",
			wantAgent:    "agent2",
		},
		{
			name:         "explicit values win",
			req:          web.CreateTicketRequest{Title: "Explicit", Pipeline: "one-stage", Agent: "agent1"},
			wantPipeline: "one-stage",
			wantAgent:    "agent1",
		},
		{
			name:      "none opts the pipeline out",
			req:       web.CreateTicketRequest{Title: "Standalone", Pipeline: "none"},
			wantAgent: "agent2",
		},
		{
			name:         "none opts the agent out",
			req:          web.CreateTicketRequest{Title: "No agent", Agent: "none"},
			wantPipeline: "two-stage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Projects = map[string]config.Project{
				"repo": {Path: h.repoDir, Pipeline: "two-stage", Agent: "agent2"},
			}
			d := h.newDaemon(h.cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			req := tc.req
			req.Path = h.repoDir
			req.Status = "open"
			info, err := d.CreateTicket(req)
			require.NoError(t, err)

			created := h.readTask(info.ID + ".md")
			assert.Equal(t, tc.wantPipeline, created.Pipeline)
			assert.Equal(t, tc.wantAgent, created.Agent)

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

func TestDaemon_GetConfig_ExposesMaxRetries(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	var retry web.PipelineInfo
	for _, p := range d.GetConfig().PipelineInfos {
		if p.Name == "retry-stage" {
			retry = p
		}
	}
	require.Equal(t, []string{"step1"}, retry.Stages)
	assert.Equal(t, []int{1}, retry.MaxRetries)
}

func TestDaemon_GetActivity(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	path := h.writeTicket("tst-act.md", `---
id: tst-act
kontora: true
status: paused
pipeline: retry-stage
stage: step1
history:
  - stage: step1
    agent: agent1
    exit_code: 1
    run: 0
  - stage: step1
    agent: agent1
    exit_code: 1
    run: 1
---
# Activity ticket
`)
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets["tst-act"] = newTicketState(tk, path)

	logDir := filepath.Join(h.logsDir, "tst-act")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "step1.log"), []byte("newest run output"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "step1.1.events.json"),
		[]byte(`{"version":1,"agent":"claude","events":[]}`), 0o644))

	t.Run("a run with a sidecar reports source events", func(t *testing.T) {
		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-act", Stage: "step1", Run: 1})
		require.NoError(t, err)
		assert.Equal(t, "events", got.Source)
		assert.Equal(t, 1, got.Run)
		assert.False(t, got.Stale)
		assert.False(t, got.Live)
		require.NotNil(t, got.Tape)
	})

	t.Run("a finished run answers 304 for its own validator", func(t *testing.T) {
		first, err := d.GetActivity(web.ActivityQuery{ID: "tst-act", Stage: "step1", Run: 1})
		require.NoError(t, err)
		require.NotEmpty(t, first.ETag)

		again, err := d.GetActivity(web.ActivityQuery{ID: "tst-act", Stage: "step1", Run: 1, IfNoneMatch: first.ETag})
		require.NoError(t, err)
		assert.True(t, again.NotModified)
		assert.Nil(t, again.Tape, "a 304 must not parse the transcript")
		assert.Equal(t, first.ETag, again.ETag)
	})

	t.Run("an older run without a sidecar is stale plaintext", func(t *testing.T) {
		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-act", Stage: "step1", Run: 0})
		require.NoError(t, err)
		assert.Equal(t, "log", got.Source)
		assert.True(t, got.Stale, "run 0 is not the newest run of step1")
		assert.Equal(t, "newest run output", got.Content)
		assert.Nil(t, got.Tape)
	})

	t.Run("a fallback taken while the stage runs again is stale", func(t *testing.T) {
		running := h.writeTicket("tst-live.md", `---
id: tst-live
kontora: true
status: in_progress
pipeline: retry-stage
stage: step1
history:
  - stage: step1
    agent: agent1
    exit_code: 1
    run: 0
---
# Live ticket
`)
		tk, err := ticket.ParseFile(running)
		require.NoError(t, err)
		d.tickets["tst-live"] = newTicketState(tk, running)

		liveDir := filepath.Join(h.logsDir, "tst-live")
		require.NoError(t, os.MkdirAll(liveDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(liveDir, "step1.log"),
			[]byte("run 0 output, then the live run appended"), 0o644))

		// Run 0 is the newest run in history, but step1 is running again and
		// appending to the same file, so its bytes are not run 0's alone.
		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-live", Stage: "step1", Run: 0})
		require.NoError(t, err)
		assert.Equal(t, "log", got.Source)
		assert.True(t, got.Stale)
		assert.False(t, got.Live, "run 0 has finished; the run in flight is run 1")
	})

	t.Run("an unknown ticket is not found", func(t *testing.T) {
		_, err := d.GetActivity(web.ActivityQuery{ID: "nope", Stage: "step1", Run: 0})
		assert.ErrorIs(t, err, web.ErrTicketNotFound)
	})
}

// TestDaemon_GetActivity_Live covers reads of a run still in flight, which the
// daemon serves from the session JSONL the agent is appending to rather than
// from the sidecar written when the stage exits.
func TestDaemon_GetActivity_Live(t *testing.T) {
	// liveTicket registers a ticket running its first run of step1.
	liveTicket := func(t *testing.T, h *testHarness, id string) *Daemon {
		t.Helper()
		d := h.newDaemon(h.cfg)
		path := h.writeTicket(id+".md", "---\nid: "+id+`
kontora: true
status: in_progress
pipeline: retry-stage
stage: step1
---
# Live ticket
`)
		tk, err := ticket.ParseFile(path)
		require.NoError(t, err)
		d.tickets[id] = newTicketState(tk, path)
		return d
	}

	claudeLines := []string{
		`{"type":"assistant","message":{"model":"m1","content":[{"type":"text","text":"planning"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
	}

	// plantClaudeSession writes the session JSONL a Claude run appends to and
	// returns the config directory holding it alongside the file itself.
	plantClaudeSession := func(t *testing.T, id, sessionID string, lines []string) (claudeDir, file string) {
		t.Helper()
		claudeDir = t.TempDir()
		dir := filepath.Join(claudeDir, "projects", "-worktree-"+id)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		file = filepath.Join(dir, sessionID+".jsonl")
		require.NoError(t, os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
		return claudeDir, file
	}

	// plantClaudeRun plants a session and registers it as the first run of step1.
	plantClaudeRun := func(t *testing.T, d *Daemon, id, sessionID string, lines []string) string {
		t.Helper()
		claudeDir, file := plantClaudeSession(t, id, sessionID, lines)
		d.setLiveRun(id, liveRun{
			stage: "step1",
			run:   0,
			params: RunnerParams{
				SessionID: sessionID,
				Env:       map[string]string{"CLAUDE_CONFIG_DIR": claudeDir},
			},
			startedAt: time.Now(),
		})
		return file
	}

	t.Run("a truncated trailing line yields the complete events only", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv1")
		plantClaudeRun(t, d, "tst-lv1", "sess-1",
			append(slices.Clone(claudeLines), `{"type":"assistant","message":{"content":[{"type":"te`))

		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv1", Stage: "step1", Run: 0})
		require.NoError(t, err)
		assert.Equal(t, "events", got.Source)
		assert.True(t, got.Live)
		require.NotNil(t, got.Tape)
		assert.Equal(t, []string{"model", "text", "tool"}, eventKinds(got.Tape))
		assert.Equal(t, "ok", got.Tape.Events[2].Result)
	})

	t.Run("a pi retry reads its own attempt, not the previous one", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv2")

		dir := piSessionDir(h.cfg, "tst-lv2", "step1")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		previous := filepath.Join(dir, "attempt-1.jsonl")
		require.NoError(t, os.WriteFile(previous,
			[]byte(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"first attempt"}]}}`+"\n"), 0o644))
		hourAgo := time.Now().Add(-time.Hour)
		require.NoError(t, os.Chtimes(previous, hourAgo, hourAgo))

		d.setLiveRun("tst-lv2", liveRun{
			stage:     "step1",
			run:       0,
			params:    RunnerParams{SessionDir: dir},
			startedAt: time.Now(),
		})

		// Until pi creates this attempt's file the newest one in the shared
		// directory belongs to the previous attempt and must not be served.
		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv2", Stage: "step1", Run: 0})
		require.NoError(t, err)
		assert.Equal(t, "events", got.Source)
		assert.True(t, got.Live)
		require.NotNil(t, got.Tape)
		assert.Equal(t, "pi", got.Tape.Agent)
		assert.Empty(t, got.Tape.Events)

		require.NoError(t, os.WriteFile(filepath.Join(dir, "attempt-2.jsonl"),
			[]byte(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"second attempt"}]}}`+"\n"), 0o644))

		got, err = d.GetActivity(web.ActivityQuery{ID: "tst-lv2", Stage: "step1", Run: 0})
		require.NoError(t, err)
		require.NotNil(t, got.Tape)
		require.Len(t, got.Tape.Events, 1)
		assert.Equal(t, "second attempt", got.Tape.Events[0].Text)
	})

	t.Run("a run with no session file yet is an empty live tape", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv3")
		d.setLiveRun("tst-lv3", liveRun{
			stage:     "step1",
			run:       0,
			params:    RunnerParams{SessionID: "sess-missing", Env: map[string]string{"CLAUDE_CONFIG_DIR": t.TempDir()}},
			startedAt: time.Now(),
		})

		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv3", Stage: "step1", Run: 0})
		require.NoError(t, err)
		assert.Equal(t, "events", got.Source)
		assert.True(t, got.Live)
		require.NotNil(t, got.Tape)
		assert.Equal(t, logfmt.TapeVersion, got.Tape.Version)
		assert.Equal(t, "claude", got.Tape.Agent)
		assert.Empty(t, got.Tape.Events)
	})

	t.Run("a run with no session at all serves the live plaintext log", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv4")
		d.setLiveRun("tst-lv4", liveRun{stage: "step1", run: 0, startedAt: time.Now()})

		logDir := filepath.Join(h.logsDir, "tst-lv4")
		require.NoError(t, os.MkdirAll(logDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(logDir, "step1.log"), []byte("direct runner output"), 0o644))

		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv4", Stage: "step1", Run: 0})
		require.NoError(t, err)
		assert.Equal(t, "log", got.Source)
		assert.True(t, got.Live)
		assert.Equal(t, "direct runner output", got.Content)
	})

	t.Run("a run that has not registered yet is live and empty", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv5")

		// Nothing registered and no log on disk: the seconds between the ticket
		// flipping to in_progress and the invocation starting.
		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv5", Stage: "step1", Run: 0})
		require.NoError(t, err)
		assert.Equal(t, "log", got.Source)
		assert.True(t, got.Live)
		assert.Empty(t, got.Content)
	})

	t.Run("a run older than the live one is not live", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv6")
		plantClaudeRun(t, d, "tst-lv6", "sess-6", claudeLines)

		logDir := filepath.Join(h.logsDir, "tst-lv6")
		require.NoError(t, os.MkdirAll(logDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(logDir, "step1.log"), []byte("earlier attempt"), 0o644))

		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv6", Stage: "step1", Run: 1})
		require.NoError(t, err)
		assert.False(t, got.Live, "run 1 is not the run registered as live")
		assert.Equal(t, "log", got.Source)
		assert.True(t, got.Stale)
	})

	t.Run("a rework run does not answer for the stage it interrupts", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv9")

		// Rework runs while the ticket's stage field still names the pipeline
		// stage it interrupted, so a client polling that stage must not be given
		// the rework transcript under its name.
		claudeDir, _ := plantClaudeSession(t, "tst-lv9", "sess-9", claudeLines)
		d.setLiveRun("tst-lv9", liveRun{
			stage: config.ReworkStageName,
			run:   0,
			params: RunnerParams{
				SessionID: "sess-9",
				Env:       map[string]string{"CLAUDE_CONFIG_DIR": claudeDir},
			},
			startedAt: time.Now(),
		})

		logDir := filepath.Join(h.logsDir, "tst-lv9")
		require.NoError(t, os.MkdirAll(logDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(logDir, "step1.log"), []byte("step1 output"), 0o644))

		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv9", Stage: "step1", Run: 0})
		require.NoError(t, err)
		assert.False(t, got.Live, "the run in flight is rework, not step1")
		assert.Equal(t, "log", got.Source)
		assert.Equal(t, "step1 output", got.Content)
		assert.True(t, got.Stale)

		// Asked for by name, the rework run reads live like any other.
		got, err = d.GetActivity(web.ActivityQuery{ID: "tst-lv9", Stage: config.ReworkStageName, Run: 0})
		require.NoError(t, err)
		assert.True(t, got.Live)
		require.NotNil(t, got.Tape)
		assert.Equal(t, []string{"model", "text", "tool"}, eventKinds(got.Tape))
	})

	t.Run("the cursor clamps to the stable prefix and reports its offset", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv7")
		plantClaudeRun(t, d, "tst-lv7", "sess-7", append(slices.Clone(claudeLines),
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/x.go"}}]}}`))

		// The tape is model, text, answered tool, pending tool. Everything from
		// the pending tool on may still be rewritten, so a cursor past it is
		// pulled back.
		got, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv7", Stage: "step1", Run: 0, After: 99})
		require.NoError(t, err)
		assert.Equal(t, 3, got.Offset)
		require.NotNil(t, got.Tape)
		require.Len(t, got.Tape.Events, 1)
		assert.Equal(t, "Read", got.Tape.Events[0].Tool)

		got, err = d.GetActivity(web.ActivityQuery{ID: "tst-lv7", Stage: "step1", Run: 0, After: 1})
		require.NoError(t, err)
		assert.Equal(t, 1, got.Offset)
		assert.Len(t, got.Tape.Events, 3)
	})

	t.Run("the validator changes only when the session file does", func(t *testing.T) {
		h := newHarness(t)
		d := liveTicket(t, h, "tst-lv8")
		file := plantClaudeRun(t, d, "tst-lv8", "sess-8", claudeLines)

		first, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv8", Stage: "step1", Run: 0})
		require.NoError(t, err)
		require.NotEmpty(t, first.ETag)

		again, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv8", Stage: "step1", Run: 0, IfNoneMatch: first.ETag})
		require.NoError(t, err)
		assert.True(t, again.NotModified)
		assert.Nil(t, again.Tape, "a 304 must not parse the transcript")
		assert.Equal(t, first.ETag, again.ETag)

		f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = f.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"more"}]}}` + "\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())

		grown, err := d.GetActivity(web.ActivityQuery{ID: "tst-lv8", Stage: "step1", Run: 0, IfNoneMatch: first.ETag})
		require.NoError(t, err)
		assert.False(t, grown.NotModified)
		assert.NotEqual(t, first.ETag, grown.ETag)
		require.NotNil(t, grown.Tape)
		assert.Equal(t, "more", grown.Tape.Events[len(grown.Tape.Events)-1].Text)
	})
}

func eventKinds(tape *logfmt.Tape) []string {
	out := make([]string, len(tape.Events))
	for i, e := range tape.Events {
		out[i] = e.Kind
	}
	return out
}
