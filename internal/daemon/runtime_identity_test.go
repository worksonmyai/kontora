package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func startIdentityDiscovery(t *testing.T, d *Daemon, interval, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.discoverTmuxSocket(ctx, interval, timeout)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("socket discovery did not stop after cancellation")
		}
	})
}

func TestDaemonUsesTheBoardItAdvertises(t *testing.T) {
	for _, tc := range []struct {
		name        string
		environment bool
		parent      bool
	}{
		{name: "retargeted config symlink"},
		{name: "retargeted environment symlink", environment: true},
		{name: "symlink with a parent component", parent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReloadHarness(t)
			const id = "tst-original"
			const originalText = "---\nid: tst-original\nstatus: open\nkontora: true\npipeline: one-stage\nstage: step1\ndeps: [tst-blocker]\n---\n# Original store\n"
			originalFile := h.writeTicket(id+".md", originalText)
			h.writeTicket("tst-blocker.md", "---\nid: tst-blocker\nstatus: open\n---\n# Blocker\n")
			replacement := t.TempDir()
			replacementFile := filepath.Join(replacement, id+".md")
			replacementText := strings.Replace(originalText, "Original store", "Other store", 1)
			require.NoError(t, os.WriteFile(replacementFile, []byte(replacementText), 0o600))
			alias := filepath.Join(replacement, "tickets")
			target, configured := h.tasksDir, alias
			if tc.parent {
				target = filepath.Join(target, "child")
				require.NoError(t, os.Mkdir(target, 0o755))
				configured += "/.."
			}
			require.NoError(t, os.Symlink(target, alias))
			initial := h.yaml(configOpts{})
			if tc.environment {
				t.Setenv(config.TicketsDirEnvVar, configured)
			} else {
				initial = strings.Replace(initial, "tickets_dir: "+h.tasksDir, "tickets_dir: "+configured, 1)
			}
			d := h.newDaemonWithConfig(t, initial)
			companionDir := d.companionDir()
			ctx, cancel := context.WithCancel(t.Context())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			defer func() {
				cancel()
				require.NoError(t, <-errCh)
			}()
			require.Eventually(t, func() bool {
				_, err := d.GetTicket(id)
				return err == nil
			}, 5*time.Second, 10*time.Millisecond)

			if !tc.parent {
				require.NoError(t, os.Remove(alias))
				require.NoError(t, os.Symlink(replacement, alias))
			}
			require.Equal(t, mustEvalDir(t, h.tasksDir), d.GetConfig().TicketsDir)
			assert.Equal(t, companionDir, d.companionDir(), "startup must preserve the companion state directory")
			assert.Equal(t, filepath.Join(companionDir, "assistant"), d.assistantThreadStore(d.config(), "k").Root())
			loaded, err := d.GetTicket(id)
			require.NoError(t, err)
			assert.Equal(t, "Original store", loaded.Title)

			h.writeConfigAtomic(t, strings.Replace(initial, "branch_prefix: kontora", "branch_prefix: reloaded", 1))
			require.NoError(t, d.reloadConfig())
			assert.Equal(t, "reloaded", d.GetConfig().BranchPrefix)
			assert.Equal(t, mustEvalDir(t, h.tasksDir), d.GetConfig().TicketsDir)
			assert.NotContains(t, h.logBuf.String(), "field=tickets_dir")

			at := ticket.FormatSchedule(time.Now().Add(time.Hour))
			require.NoError(t, d.ScheduleTicket(id, web.ScheduleTicketRequest{ScheduledAt: at}))
			scheduled, err := ticket.ParseFile(originalFile)
			require.NoError(t, err)
			assert.Equal(t, at, scheduled.ScheduledAt)
			require.NoError(t, d.RunTicket(id))
			queued, err := ticket.ParseFile(originalFile)
			require.NoError(t, err)
			assert.Equal(t, ticket.StatusTodo, queued.Status)
			assert.Empty(t, queued.ScheduledAt)
			other, err := os.ReadFile(replacementFile)
			require.NoError(t, err)
			assert.Equal(t, replacementText, string(other))
		})
	}
}

func journalTicketAlias(t *testing.T, path string) string {
	t.Helper()
	alias := filepath.Join(t.TempDir(), "board")
	require.NoError(t, os.Symlink(filepath.Dir(path), alias))
	return filepath.Join(alias, filepath.Base(path))
}

func TestGetConfigDoesNotWaitForUnresponsiveTmux(t *testing.T) {
	h := newReloadHarness(t)
	binDir := t.TempDir()
	started := filepath.Join(binDir, "started")
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "tmux"), []byte(`#!/bin/sh
[ "$1" = -N ] && [ "$2" = display-message ] || exit 99
printf ready > "$TEST_TMUX_STARTED"
exec /bin/sleep 30
`), 0o755))
	t.Setenv("PATH", binDir)
	t.Setenv("TEST_TMUX_STARTED", started)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}), WithConfigOverride(func(cfg *config.Config) {
		cfg.Web.Enabled = new(true)
		cfg.Web.Host = "127.0.0.1"
		cfg.Web.Port = 0
	}))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Error("daemon shutdown waited for the socket probe timeout")
		}
	})
	t.Cleanup(stop)
	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, 15*time.Second, 10*time.Millisecond)

	client := &http.Client{Timeout: time.Second}
	addr := d.webAddr.Load()
	require.NotNil(t, addr)
	resp, err := client.Get("http://" + *addr + "/api/config")
	require.NoError(t, err, "mandatory store verification must not wait for optional tmux discovery")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got web.ConfigInfo
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, mustEvalDir(t, h.tasksDir), got.TicketsDir)
	assert.Empty(t, got.TmuxSocket)
	if locked := d.mu.TryLock(); assert.True(t, locked, "socket discovery must not hold the ticket lock") {
		d.mu.Unlock()
	}
	require.NoError(t, d.reloadConfig(), "socket discovery must not hold the reload lock")
	stop()
}

func TestTmuxSocketDiscoveryRetriesAfterFailure(t *testing.T) {
	for _, failure := range []string{"missing session", "unresponsive server"} {
		t.Run(failure, func(t *testing.T) {
			h := newReloadHarness(t)
			socketDir, err := os.MkdirTemp("", "kt")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
			socket := filepath.Join(socketDir, "socket")
			listener, err := net.Listen("unix", socket)
			require.NoError(t, err)
			t.Cleanup(func() { _ = listener.Close() })
			binDir := t.TempDir()
			attempts := filepath.Join(binDir, "attempts")
			ready := filepath.Join(binDir, "ready")
			require.NoError(t, os.WriteFile(filepath.Join(binDir, "tmux"), []byte(`#!/bin/sh
[ "$#" = 6 ] && [ "$1" = -N ] && [ "$2" = display-message ] && [ "$3" = -p ] && [ "$4" = -t ] && [ "$5" = '=kontora-test:' ] && [ "$6" = '#{session_name}:#{socket_path}' ] || exit 99
printf 'attempt\n' >> "$TEST_TMUX_ATTEMPTS"
if [ ! -e "$TEST_TMUX_READY" ]; then
  if [ "$TEST_TMUX_FAILURE" = 'unresponsive server' ]; then exec /bin/sleep 30; fi
  exit 1
fi
printf 'kontora-test:%s\n' "$TEST_TMUX_SOCKET"
`), 0o755))
			t.Setenv("PATH", binDir)
			t.Setenv("TEST_TMUX_ATTEMPTS", attempts)
			t.Setenv("TEST_TMUX_READY", ready)
			t.Setenv("TEST_TMUX_FAILURE", failure)
			t.Setenv("TEST_TMUX_SOCKET", socket)
			d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))
			startIdentityDiscovery(t, d, 10*time.Millisecond, 100*time.Millisecond)
			require.Eventually(t, func() bool {
				data, _ := os.ReadFile(attempts)
				return strings.Count(string(data), "attempt\n") >= 2
			}, 3*time.Second, 10*time.Millisecond, "each failed probe must finish within its bound")
			assert.Empty(t, d.GetConfig().TmuxSocket)

			require.NoError(t, os.WriteFile(ready, nil, 0o600))
			require.Eventually(t, func() bool {
				return d.GetConfig().TmuxSocket == socket
			}, 3*time.Second, 10*time.Millisecond, "a session created after startup must become available")
		})
	}
}
