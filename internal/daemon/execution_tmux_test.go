package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestExecutionRealTmuxAPIRetryReleasesCapacity(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir, err := os.MkdirTemp("", "kyr")
	require.NoError(t, err)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", dir)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run(); _ = os.RemoveAll(dir) })
	out, err := exec.Command("tmux", "-f", "/dev/null", "new-session", "-d", "-s", "unrelated", "-n", "anchor", "--", "sleep", "60").CombinedOutput()
	require.NoError(t, err, "%s", out)
	h := newHarness(t)
	h.cfg.MaxConcurrentAgents = 1
	marker := filepath.Join(t.TempDir(), "first-attempt")
	agent := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(agent, []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$%s\" = tst-retry ] && [ ! -f %q ]; then touch %q; exit 1; fi\nexit 0\n", config.TicketEnvVar, marker, marker)), 0o755))
	agentCfg := h.cfg.Agents["agent1"]
	agentCfg.Binary = agent
	h.cfg.Agents["agent1"] = agentCfg
	d := h.newDaemon(h.cfg, WithRunner(tmuxRunner), WithExecutionPreparer(tmux.EnsureWindowAbsent))
	h.writeTicket("tst-retry.md", h.taskMD("tst-retry", "todo", "one-stage"))
	h.writeTicket("tst-queued.md", h.taskMD("tst-queued", "open", "one-stage"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
	h.waitForStatus("tst-retry.md", ticket.StatusPaused, 10*time.Second)
	waitForAgentsDone(t, d, 5*time.Second)
	windows, err := tmux.ListWindows(h.tmuxSession)
	require.NoError(t, err)
	assert.Contains(t, windows, "tst-retry")
	srv, err := web.New(d, d.broker, "127.0.0.1", 0, "", nil, h.tmuxSession, testLogger(t))
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	for _, target := range []struct{ id, action string }{{"tst-retry", "retry"}, {"tst-queued", "run"}} {
		resp, err := http.Post("http://"+srv.Addr()+"/api/tickets/"+target.id+"/"+target.action, "application/json", nil)
		require.NoError(t, err)
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	}
	h.waitForStatus("tst-retry.md", ticket.StatusDone, 10*time.Second)
	h.waitForStatus("tst-queued.md", ticket.StatusDone, 10*time.Second)
	waitForAgentsDone(t, d, 5*time.Second)
	got := h.readTask("tst-retry.md")
	require.Len(t, got.History, 2)
	assert.Equal(t, 1, got.History[0].ExitCode)
	assert.Zero(t, got.History[1].ExitCode)
	windows, err = tmux.ListWindows(h.tmuxSession)
	require.NoError(t, err)
	for _, window := range windows {
		assert.False(t, strings.HasPrefix(window, "tst-"), "remaining window %q", window)
	}
	d.mu.Lock()
	assert.Empty(t, d.running)
	assert.Empty(t, d.runningBranches)
	assert.Empty(t, d.executions)
	assert.Zero(t, d.sem.used)
	d.mu.Unlock()
}
