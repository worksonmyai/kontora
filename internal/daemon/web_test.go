package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
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
	cfg.Roles["step1"] = config.Role{Prompt: ""}
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
	cfg.Roles["step1"] = config.Role{Prompt: ""}
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
	cfg.Roles["step1"] = config.Role{Prompt: ""}
	cfg.Roles["step2"] = config.Role{Prompt: ""}
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
	assert.Equal(t, "step2", result.Role)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_SetStage(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "true")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
	cfg.Roles["step1"] = config.Role{Prompt: ""}
	cfg.Roles["step2"] = config.Role{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Start ticket at step2 by writing it with role=step2.
	h.writeTicket("tst-ss1.md", fmt.Sprintf(`---
id: tst-ss1
kontora: true
status: paused
pipeline: two-stage
role: step2
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

	// Verify only the role changed — status and attempt stay untouched.
	result := h.readTask("tst-ss1.md")
	assert.Equal(t, "step1", result.Role)
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

	h.writeTicket("tst-sub.md", h.taskMD("tst-sub", "todo", "one-stage"))

	// Should receive at least one event about tst-sub.
	deadline := time.After(10 * time.Second)
	var received bool
	for !received {
		select {
		case ev := <-ch:
			if ev.Ticket.ID == "tst-sub" {
				received = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for SSE event")
		}
	}

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
	srv := web.New(d, d.broker, "127.0.0.1", 0, testLogger(t))
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
role: step1
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
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown agent")

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
			cfg.Roles["step1"] = config.Role{Prompt: ""}
			d := h.newDaemon(cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			ticketID := "tst-ua" + tc.status[:2]
			h.writeTicket(ticketID+".md", fmt.Sprintf(`---
id: %s
status: %s
kontora: true
pipeline: one-stage
role: step1
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
	cfg.Roles["step1"] = config.Role{Prompt: ""}
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

func TestDaemon_Summarize_NotConfigured(t *testing.T) {
	h := newHarness(t)
	// Ensure no summarizer is configured (default).
	d := h.newDaemon(h.cfg)

	_, err := d.Summarize("any-id", "")
	assert.ErrorIs(t, err, web.ErrSummarizerNotConfigured)
}

func TestDaemon_Summarize_TicketNotFound(t *testing.T) {
	h := newHarness(t)
	h.cfg.Summarizer = &config.Summarizer{Binary: "echo"}
	h.cfg.Summarizer.Timeout.Duration = 30 * time.Second
	d := h.newDaemon(h.cfg)

	_, err := d.Summarize("nonexistent", "")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)
}

func TestDaemon_GetConfig_SummarizerConfigured(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	cfg := d.GetConfig()
	assert.False(t, cfg.SummarizerConfigured)

	// With summarizer configured.
	h2 := newHarness(t)
	h2.cfg.Summarizer = &config.Summarizer{Binary: "echo"}
	d2 := h2.newDaemon(h2.cfg)

	cfg2 := d2.GetConfig()
	assert.True(t, cfg2.SummarizerConfigured)
}
