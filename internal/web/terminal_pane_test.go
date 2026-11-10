package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/tmux"
)

func TestPaneTerminal_HistoryAndSnapshotCrossing(t *testing.T) {
	p := newPaneTerminalTest(t)
	ready, gate := p.file("seeded"), p.file("cross-snapshot")
	p.respawn("i=1; while [ \"$i\" -le 3000 ]; do printf 'line-%d\\n' \"$i\"; i=$((i+1)); done; " +
		": > " + paneShellQuote(ready) + "; " + paneWaitFile(gate) +
		"printf 'marker-xyz\\nstream-finished\\n'; exec sleep 60")
	p.waitFile(ready)
	// Wait for tmux to parse the seed, not just for the producer to write it.
	require.Eventually(t, func() bool {
		out, err := exec.Command("tmux", "capture-pane", "-p", "-S", "-5000", "-t", p.target).Output()
		return err == nil && strings.Contains(string(out), "line-3000")
	}, 5*time.Second, 10*time.Millisecond)

	conn := p.dial(80, 24, false)
	// Release immediately after Dial, before reading the reset/snapshot. The
	// browser must receive the marker whether it comes from the snapshot or live output.
	p.touch(gate)
	p.firstReset(conn, 80, 24)
	var received strings.Builder
	p.readThrough(conn, &received, "stream-finished")
	p.command("kill-window", "-t", p.target)
	p.readNormalClose(conn, &received)
	assert.Contains(t, received.String(), "line-1\r\n")
	assert.Contains(t, received.String(), "line-3000\r\n")
	assert.Equal(t, 1, strings.Count(received.String(), "marker-xyz"))
}

func TestPaneTerminal_QueuedOutputOnExit(t *testing.T) {
	for _, tt := range []struct {
		name    string
		message clientMsg
	}{
		{"no commands", clientMsg{}},
		{"resize during drain", clientMsg{Type: "resize", Cols: 90, Rows: 30}},
		{"input during drain", clientMsg{Type: "input", Data: "late input"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newPaneTerminalTest(t)
			gate, done := p.file("write-go"), p.file("written")
			p.respawn(paneWaitFile(gate) +
				"dd if=/dev/zero bs=1024 count=768 2>/dev/null | tr '\\000' x; printf '\\r\\nFINAL-MARKER\\r\\n'; : > " +
				paneShellQuote(done) + "; exec sleep 60")
			conn := p.dial(80, 24, true)
			p.firstReset(conn, 80, 24)
			p.touch(gate)
			p.waitFile(done)
			// Hold socket reads until tmux has parsed all output and the window exits.
			require.Eventually(t, func() bool {
				return strings.Contains(p.command("capture-pane", "-p", "-t", p.target), "FINAL-MARKER")
			}, 3*time.Second, 10*time.Millisecond)
			p.command("kill-window", "-t", p.target)
			time.Sleep(100 * time.Millisecond)
			if tt.message.Type != "" {
				p.send(conn, tt.message)
				time.Sleep(100 * time.Millisecond)
			}
			var received strings.Builder
			p.readNormalClose(conn, &received)
			assert.Contains(t, received.String(), "FINAL-MARKER")
			assert.Equal(t, 768*1024, strings.Count(received.String(), "x"))
		})
	}
}

func TestPaneTerminal_WindowIsolationAndCleanup(t *testing.T) {
	p := newPaneTerminalTest(t)
	gate, otherGate := p.file("agent-go"), p.file("other-go")
	p.respawn(paneWaitFile(gate) + "printf 'agent-visible\\n'; exec sleep 60")
	p.command("new-window", "-d", "-t", "="+p.session+":", "-n", "other-agent",
		paneShellCommand(paneWaitFile(otherGate)+"printf 'other-agent-private\\n'; exec sleep 60"))
	conn := p.dial(80, 24, false)
	p.firstReset(conn, 80, 24)

	viewer := p.command("list-clients", "-F", "#{client_session}", "-f", "#{m:kontora-view-*,#{client_session}}")
	require.True(t, strings.HasPrefix(viewer, "kontora-view-"), "viewer: %q", viewer)
	windowID := p.command("display-message", "-p", "-t", p.target, "#{window_id}")
	requireTmuxOutput(t, windowID, "list-windows", "-t", "="+viewer+":", "-F", "#{window_id}")

	p.touch(otherGate)
	requireTmuxOutput(t, "other-agent-private", "capture-pane", "-p", "-t", tmux.WindowTarget(p.session, "other-agent"))
	p.touch(gate)
	var received strings.Builder
	p.readThrough(conn, &received, "agent-visible")
	p.command("kill-window", "-t", p.target)
	p.readNormalClose(conn, &received)
	assert.NotContains(t, received.String(), "other-agent-private")
	// The other agent keeps the server alive, so an empty result proves the
	// viewer session was removed rather than merely losing its client.
	requireTmuxOutput(t, "", "list-sessions", "-F", "#{session_name}", "-f", "#{m:kontora-view-*,#{session_name}}")
}

func TestPaneTerminal_StalledBrowserDoesNotHoldAgent(t *testing.T) {
	p := newPaneTerminalTest(t)
	gate, done := p.file("write-go"), p.file("done")
	p.respawn(paneWaitFile(gate) +
		"dd if=/dev/zero bs=1024 count=5120 2>/dev/null | tr '\\000' x; : > " +
		paneShellQuote(done) + "; exec sleep 60")
	_ = p.dial(80, 24, false) // Deliberately never read even the initial reset.
	requireTmuxOutput(t, "1", "list-clients", "-F", "#{client_control_mode}", "-f", "#{m:kontora-view-*,#{client_session}}")
	p.touch(gate)
	require.Eventually(t, func() bool {
		_, err := os.Stat(done)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "5 MB producer was held by a browser that never reads")
}

func TestPaneTerminal_SecondViewerAndSizeReclaim(t *testing.T) {
	p := newPaneTerminalTest(t)
	gate := p.file("survive-disconnect")
	p.respawn(paneWaitFile(gate) + "printf 'first-viewer-still-live\\n'; exec sleep 60")
	first := p.dial(200, 50, false)
	p.firstReset(first, 200, 50)
	requireTmuxOutput(t, "200 50", "display-message", "-p", "-t", p.target, "#{pane_width} #{pane_height}")

	second := p.dial(120, 30, false)
	p.firstReset(second, 120, 30)
	requireTmuxOutput(t, "120 30", "display-message", "-p", "-t", p.target, "#{pane_width} #{pane_height}")
	p.readReset(first, 120, 30)

	p.send(first, clientMsg{Type: "resize", Cols: 210, Rows: 55})
	p.readReset(first, 210, 55)
	requireTmuxOutput(t, "210 55", "display-message", "-p", "-t", p.target, "#{pane_width} #{pane_height}")

	// Another viewer leaving can emit window-close even though the shared
	// agent window survives. It must not close this viewer's stream.
	require.NoError(t, second.CloseNow())
	requireTmuxOutput(t, "1", "list-clients", "-F", "#{client_control_mode}", "-f", "#{m:kontora-view-*,#{client_session}}")
	p.touch(gate)
	var received strings.Builder
	p.readThrough(first, &received, "first-viewer-still-live")
}

func TestPaneTerminal_Input(t *testing.T) {
	for _, tt := range []struct {
		name  string
		rw    bool
		input string
	}{
		{"read-write exact UTF-8 and punctuation", true, "héllo;\"'\n"},
		{"read-write chunked multiline paste", true, strings.Repeat("héllo;\"' \n", 80)},
		{"read-only drops input", false, "héllo;\"'\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newPaneTerminalTest(t)
			captured, ready := p.file("captured"), p.file("ready")
			p.respawn(": > " + paneShellQuote(captured) + "; : > " + paneShellQuote(ready) +
				"; exec cat > " + paneShellQuote(captured))
			p.waitFile(ready)
			conn := p.dial(80, 24, tt.rw)
			p.firstReset(conn, 80, 24)
			wantReadOnly := "1"
			if tt.rw {
				wantReadOnly = "0"
			}
			requireTmuxOutput(t, wantReadOnly, "list-clients", "-F", "#{client_readonly}", "-f", "#{m:kontora-view-*,#{client_session}}")
			p.send(conn, clientMsg{Type: "input", Data: tt.input})
			// A later resize/reset is a protocol barrier: the handler has read
			// the input message before we inspect the receiving program's file.
			p.send(conn, clientMsg{Type: "resize", Cols: 90, Rows: 25})
			p.readReset(conn, 90, 25)
			if tt.rw {
				require.Eventually(t, func() bool {
					data, err := os.ReadFile(captured)
					return err == nil && string(data) == tt.input
				}, 3*time.Second, 10*time.Millisecond, "input must arrive byte-for-byte")
			} else {
				assert.Never(t, func() bool {
					data, err := os.ReadFile(captured)
					return err != nil || len(data) != 0
				}, 300*time.Millisecond, 10*time.Millisecond, "read-only viewer delivered input")
			}
		})
	}
}

// All tmux commands below use the private server installed by isolateTmux.
// The initial placeholder is replaced with a newly created pane so the history
// limit is installed BEFORE the pane's grid is allocated (not after seeding).
type paneTerminalTest struct {
	t                   *testing.T
	session, id, target string
	dir                 string
	srv                 *Server
	ctx                 context.Context
}

func newPaneTerminalTest(t *testing.T) *paneTerminalTest {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping tmux integration test in short mode")
	}
	requireTmux(t)
	isolateTmux(t)
	p := &paneTerminalTest{t: t, session: uniqueSession(t), id: "agent", dir: t.TempDir()}
	startTmuxWindow(t, p.session, "__init") // -f /dev/null: no user tmux config.
	p.target = tmux.WindowTarget(p.session, p.id)
	p.command("set-option", "-g", "default-shell", "/bin/sh")
	p.command("set-option", "-g", "history-limit", "6000")
	p.command("new-window", "-d", "-t", "="+p.session+":", "-n", p.id, paneShellCommand("exec sleep 60"))
	p.command("kill-window", "-t", tmux.WindowTarget(p.session, "__init"))
	p.srv = startTerminalTestServer(t, &mockTerminalService{hasSession: true}, p.session)
	var cancel context.CancelFunc
	p.ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return p
}

func (p *paneTerminalTest) command(args ...string) string {
	p.t.Helper()
	out, err := exec.Command("tmux", args...).CombinedOutput()
	require.NoError(p.t, err, "tmux %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func paneShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func paneShellCommand(script string) string {
	return "exec /bin/sh -c " + paneShellQuote("stty -echo; "+script)
}

func paneWaitFile(path string) string {
	return "while [ ! -f " + paneShellQuote(path) + " ]; do sleep 0.01; done; "
}

func (p *paneTerminalTest) respawn(script string) {
	p.t.Helper()
	p.command("respawn-pane", "-k", "-t", p.target, paneShellCommand(script))
}

func (p *paneTerminalTest) file(name string) string { return filepath.Join(p.dir, name) }

func (p *paneTerminalTest) touch(path string) {
	p.t.Helper()
	require.NoError(p.t, os.WriteFile(path, nil, 0600))
}

func (p *paneTerminalTest) waitFile(path string) {
	p.t.Helper()
	require.Eventually(p.t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "waiting for %s", path)
}

func (p *paneTerminalTest) dial(cols, rows int, rw bool) *websocket.Conn {
	p.t.Helper()
	url := fmt.Sprintf("ws://%s/ws/terminal/%s?stream=pane&cols=%d&rows=%d", p.srv.Addr(), p.id, cols, rows)
	if rw {
		url += "&rw=1"
	}
	conn, resp, err := websocket.Dial(p.ctx, url, nil)
	require.NoError(p.t, err)
	if resp != nil && resp.Body != nil {
		p.t.Cleanup(func() { _ = resp.Body.Close() })
	}
	p.t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func (p *paneTerminalTest) send(conn *websocket.Conn, msg clientMsg) {
	p.t.Helper()
	data, err := json.Marshal(msg)
	require.NoError(p.t, err)
	require.NoError(p.t, conn.Write(p.ctx, websocket.MessageText, data))
}

func (p *paneTerminalTest) firstReset(conn *websocket.Conn, cols, rows int) {
	p.t.Helper()
	typ, data, err := conn.Read(p.ctx)
	require.NoError(p.t, err)
	require.Equal(p.t, websocket.MessageText, typ, "first frame must be a reset, not output")
	require.JSONEq(p.t, fmt.Sprintf(`{"type":"reset","cols":%d,"rows":%d}`, cols, rows), string(data))
}

func (p *paneTerminalTest) readReset(conn *websocket.Conn, cols, rows int) {
	p.t.Helper()
	for {
		typ, data, err := conn.Read(p.ctx)
		require.NoError(p.t, err, "waiting for reset %dx%d", cols, rows)
		if typ == websocket.MessageText {
			var msg clientMsg
			require.NoError(p.t, json.Unmarshal(data, &msg))
			if msg.Type == "reset" && msg.Cols == cols && msg.Rows == rows {
				return
			}
		}
	}
}

func (p *paneTerminalTest) readThrough(conn *websocket.Conn, received *strings.Builder, marker string) {
	p.t.Helper()
	for !strings.Contains(received.String(), marker) {
		typ, data, err := conn.Read(p.ctx)
		require.NoError(p.t, err, "waiting for %q", marker)
		if typ == websocket.MessageBinary {
			received.Write(data)
		}
	}
}

func (p *paneTerminalTest) readNormalClose(conn *websocket.Conn, received *strings.Builder) {
	p.t.Helper()
	for {
		typ, data, err := conn.Read(p.ctx)
		if err != nil {
			assert.Equal(p.t, websocket.StatusNormalClosure, websocket.CloseStatus(err), "close error: %v", err)
			return
		}
		if typ == websocket.MessageBinary {
			received.Write(data)
		}
	}
}
