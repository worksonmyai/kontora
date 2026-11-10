package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/tmux"
)

type automationViewerService struct {
	mockTerminalService
	resolve func(string, uint64) (AutomationTerminalTarget, error)
}

func (s *automationViewerService) AutomationTerminal(id string, n uint64) (AutomationTerminalTarget, error) {
	return s.resolve(id, n)
}

func closeAutomationViewerConnection(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.CloseNow(); err != nil {
		assert.ErrorIs(t, err, net.ErrClosed)
	}
}

func viewerTmux(t *testing.T, target AutomationTerminalTarget, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", append([]string{"-S", target.Identity.SocketPath, "-N"}, args...)...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return strings.TrimSuffix(string(out), "\n")
}

func viewerTarget(t *testing.T, base int) AutomationTerminalTarget {
	t.Helper()
	if testing.Short() {
		t.Skip("tmux integration")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir, err := os.MkdirTemp("/tmp", "kav-")
	require.NoError(t, err)
	socket := filepath.Join(dir, "s")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "-N", "kill-server").Run(); _ = os.RemoveAll(dir) })
	cfg := filepath.Join(dir, "tmux.conf")
	require.NoError(t, os.WriteFile(cfg, []byte(fmt.Sprintf("set -g base-index %d\nset -g default-shell /bin/sh\n", base)), 0o600))
	out, err := exec.Command("tmux", "-S", socket, "-f", cfg, "new-session", "-d", "-s", "origin", "-n", "run", "-P", "-F", "#{socket_path}\t#{pid}\t#{start_time}\t#{session_id}\t#{window_id}", "--", "/bin/sh").CombinedOutput()
	require.NoError(t, err, "%s", out)
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	require.Len(t, fields, 5)
	pid, err := strconv.Atoi(fields[1])
	require.NoError(t, err)
	started, err := strconv.ParseInt(fields[2], 10, 64)
	require.NoError(t, err)
	return AutomationTerminalTarget{Session: "origin", Window: "run", Identity: tmux.WindowIdentity{
		SocketPath: fields[0], ServerPID: pid, ServerStartedAt: started, SessionID: fields[3], WindowID: fields[4],
	}}
}

func TestAutomationViewerUsesActualBaseIndex(t *testing.T) {
	for _, base := range []int{0, 1, 7} {
		t.Run(fmt.Sprint(base), func(t *testing.T) {
			target := viewerTarget(t, base)
			view, err := createAutomationViewer(context.Background(), target, 80, 24)
			require.NoError(t, err)
			defer view.close()
			assert.Equal(t, strconv.Itoa(base), view.index)
			assert.Equal(t, fmt.Sprintf("%d\t%s", base, target.Identity.WindowID), viewerTmux(t, target, "list-windows", "-t", view.session, "-F", "#{window_index}\t#{window_id}"))
			// Both sessions must link the agent pane, not a placeholder shell.
			panes := strings.Fields(viewerTmux(t, target, "list-panes", "-a", "-F", "#{pane_id}"))
			require.Len(t, panes, 2)
			assert.Equal(t, panes[0], panes[1])
			view.close()
			assert.Equal(t, "origin", viewerTmux(t, target, "list-sessions", "-F", "#{session_name}"))
			present, err := tmux.InspectWindowIdentity(target.Identity)
			require.NoError(t, err)
			assert.True(t, present)
		})
	}
}

func TestAutomationViewerPreparationFailureCleansUp(t *testing.T) {
	target := viewerTarget(t, 7)
	realTmux, err := exec.LookPath("tmux")
	require.NoError(t, err)
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *link-window*) echo 'link refused' >&2; exit 19;; esac\nexec " + automationTmuxQuote(realTmux) + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = createAutomationViewer(context.Background(), target, 80, 24)
	require.Error(t, err)
	assert.Equal(t, "origin", viewerTmux(t, target, "list-sessions", "-F", "#{session_name}"))
	assert.NotContains(t, viewerTmux(t, target, "list-panes", "-a", "-F", "#{pane_current_command}"), "sleep")
}

func TestAutomationTerminalRefusalBeforeUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name, n    string
		resolveErr error
		status     int
	}{
		{"zero", "0", nil, http.StatusBadRequest},
		{"invalid number", "invalid", nil, http.StatusBadRequest},
		{"missing run", "1", automation.ErrNotFound, http.StatusNotFound},
		{"unavailable", "1", automation.ErrUnavailable, http.StatusServiceUnavailable},
		{"unverified identity", "1", nil, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &automationViewerService{resolve: func(id string, _ uint64) (AutomationTerminalTarget, error) {
				assert.Equal(t, "trusted-id", id)
				return AutomationTerminalTarget{}, tc.resolveErr
			}}
			srv := startTestServer(t, svc, NewSSEBroker(), "", "")
			response, err := http.Get(fmt.Sprintf("http://%s/ws/automations/trusted-id/%s?session=untrusted&window=untrusted&socket=/tmp/evil", srv.Addr(), tc.n))
			require.NoError(t, err)
			defer response.Body.Close()
			assert.Equal(t, tc.status, response.StatusCode)
		})
	}
}

func TestAutomationTerminalAuthDoesNotResolveTarget(t *testing.T) {
	for _, endpoint := range []string{"/api/automations/a/runs/1/terminal", "/ws/automations/a/1"} {
		for _, tc := range []struct {
			name    string
			headers map[string]string
			status  int
		}{
			{"no credentials", nil, http.StatusUnauthorized},
			{"wrong bearer", map[string]string{"Authorization": "Bearer wrong"}, http.StatusUnauthorized},
			{"foreign origin", map[string]string{"Authorization": "Bearer secret", "Origin": "https://evil.example"}, http.StatusForbidden},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				svc := &automationViewerService{resolve: func(string, uint64) (AutomationTerminalTarget, error) {
					calls.Add(1)
					return AutomationTerminalTarget{}, automation.ErrNotFound
				}}
				srv := automationTestServer(t, svc, "secret")
				w := automationRequest(srv, "GET", endpoint, "", tc.headers)
				assert.Equal(t, tc.status, w.Code)
				assert.Zero(t, calls.Load())
			})
		}
	}
}

func TestAutomationTerminalAuthority(t *testing.T) {
	for _, endpoint := range []string{"/api/automations/a/runs/1/terminal", "/ws/automations/a/1"} {
		for _, tc := range []struct {
			name    string
			headers map[string]string
			status  int
			calls   int32
		}{
			{"worker", map[string]string{"Authorization": "Bearer worker-secret"}, http.StatusForbidden, 0},
			{"reader", map[string]string{"Authorization": "Bearer reader-secret"}, http.StatusForbidden, 0},
			{"shared token", map[string]string{"Authorization": "Bearer shared-secret"}, http.StatusUnauthorized, 0},
			{"operator", map[string]string{OperatorTokenHeader: "operator-secret"}, http.StatusNotFound, 1},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				svc := &automationViewerService{resolve: func(id string, n uint64) (AutomationTerminalTarget, error) {
					assert.Equal(t, "a", id)
					assert.Equal(t, uint64(1), n)
					calls.Add(1)
					return AutomationTerminalTarget{}, automation.ErrNotFound
				}}
				srv := automationTestServer(t, svc, "shared-secret", WithAuthority(*testAuthorityConfig()))
				w := automationRequest(srv, "GET", endpoint+"?session=untrusted&window=untrusted", "", tc.headers)
				assert.Equal(t, tc.status, w.Code)
				assert.Equal(t, tc.calls, calls.Load())
			})
		}
	}
}

func TestAutomationViewerWSStopsLinkedRunWithoutLeaking(t *testing.T) {
	for _, base := range []int{1, 7} {
		t.Run(fmt.Sprint(base), func(t *testing.T) {
			target := viewerTarget(t, base)
			svc := &automationViewerService{resolve: func(id string, n uint64) (AutomationTerminalTarget, error) {
				if id != "auto-id" || n != 7 {
					return AutomationTerminalTarget{}, automation.ErrNotFound
				}
				return target, nil
			}}
			srv := startTestServer(t, svc, NewSSEBroker(), "secret", "")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s/ws/automations/auto-id/7?cols=80&rows=24&session=untrusted&window=untrusted", srv.Addr()), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer secret"}}})
			if response != nil && response.Body != nil {
				defer response.Body.Close()
			}
			require.NoError(t, err)
			defer closeAutomationViewerConnection(t, conn)
			done := make(chan error, 1)
			go func() {
				for {
					_, _, err := conn.Read(ctx)
					if err != nil {
						done <- err
						return
					}
				}
			}()
			require.Eventually(t, func() bool {
				return strings.Contains(viewerTmux(t, target, "list-clients", "-F", "#{session_name}"), "kontora-auto-view-")
			}, 3*time.Second, 20*time.Millisecond)
			viewerTmux(t, target, "kill-session", "-t", "=origin")
			// Reused names must not redirect termination from the linked agent window.
			viewerTmux(t, target, "new-session", "-d", "-s", "origin", "-n", "run", "--", "/bin/sleep", "999")
			present, err := tmux.InspectWindowIdentity(target.Identity)
			require.NoError(t, err)
			require.True(t, present)
			require.NoError(t, tmux.KillWindowIdentity(target.Identity))
			select {
			case err := <-done:
				assert.Error(t, err)
			case <-ctx.Done():
				t.Fatal("viewer did not close after termination")
			}
			require.Eventually(t, func() bool {
				return viewerTmux(t, target, "list-sessions", "-F", "#{session_name}") == "origin"
			}, 3*time.Second, 20*time.Millisecond)
		})
	}
}

func TestAutomationViewerInputAndDisconnect(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(fmt.Sprint(writable), func(t *testing.T) {
			target := viewerTarget(t, 7)
			svc := &automationViewerService{resolve: func(string, uint64) (AutomationTerminalTarget, error) { return target, nil }}
			srv := startTestServer(t, svc, NewSSEBroker(), "", "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			rw := "0"
			if writable {
				rw = "1"
			}
			conn, response, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s/ws/automations/a/1?rw=%s", srv.Addr(), rw), nil)
			if response != nil && response.Body != nil {
				defer response.Body.Close()
			}
			require.NoError(t, err)
			defer closeAutomationViewerConnection(t, conn)
			go func() {
				for {
					if _, _, err := conn.Read(ctx); err != nil {
						return
					}
				}
			}()
			require.Eventually(t, func() bool {
				return strings.Contains(viewerTmux(t, target, "list-clients", "-F", "#{session_name}"), "kontora-auto-view-")
			}, time.Second, 20*time.Millisecond)
			marker := filepath.Join(t.TempDir(), "input-was-delivered")
			payload := fmt.Sprintf(`{"type":"input","data":%s}`, strconv.Quote("printf ok > "+automationTmuxQuote(marker)+"\n"))
			require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)))
			if writable {
				require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, time.Second, 20*time.Millisecond)
			} else {
				time.Sleep(150 * time.Millisecond)
				assert.NoFileExists(t, marker)
			}
			require.NoError(t, conn.CloseNow())
			require.Eventually(t, func() bool { return viewerTmux(t, target, "list-sessions", "-F", "#{session_name}") == "origin" }, time.Second, 20*time.Millisecond)
			present, err := tmux.InspectWindowIdentity(target.Identity)
			require.NoError(t, err)
			assert.True(t, present, "disconnect must not kill the agent")
		})
	}
}

func TestAutomationViewerWSRechecksResolvedTarget(t *testing.T) {
	for _, changeAfter := range []int32{1, 2, 3} {
		t.Run(fmt.Sprint(changeAfter), func(t *testing.T) {
			target := viewerTarget(t, 7)
			var calls atomic.Int32
			svc := &automationViewerService{resolve: func(string, uint64) (AutomationTerminalTarget, error) {
				if calls.Add(1) > changeAfter {
					return AutomationTerminalTarget{}, automation.ErrNotFound
				}
				return target, nil
			}}
			srv := startTestServer(t, svc, NewSSEBroker(), "", "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s/ws/automations/a/1", srv.Addr()), nil)
			if response != nil && response.Body != nil {
				defer response.Body.Close()
			}
			require.NoError(t, err)
			defer closeAutomationViewerConnection(t, conn)
			for {
				_, _, err := conn.Read(ctx)
				if err != nil {
					break
				}
			}
			assert.NoError(t, ctx.Err(), "the server must close a stale viewer")
			require.Eventually(t, func() bool { return viewerTmux(t, target, "list-sessions", "-F", "#{session_name}") == "origin" }, time.Second, 20*time.Millisecond)
		})
	}
}
