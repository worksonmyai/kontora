package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleTerminalWS_NoSession(t *testing.T) {
	svc := &mockTerminalService{hasSession: false}
	srv := startTerminalTestServer(t, svc)

	// Attempt a plain HTTP GET — should get 404 before WebSocket upgrade.
	resp, err := http.Get(fmt.Sprintf("http://%s/ws/terminal/nonexistent", srv.Addr()))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleTerminalWS_SessionExists_UpgradesWebSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in short mode")
	}
	requireTmux(t)

	taskID := "test-term-ws"
	startTmuxWindow(t, "kontora", taskID)

	svc := &mockTerminalService{hasSession: true}
	srv := startTerminalTestServer(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s/ws/terminal/%s?cols=80&rows=24", srv.Addr(), taskID), nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	defer func() { _ = conn.CloseNow() }()

	// Send some output to the tmux window.
	err = exec.Command("tmux", "send-keys", "-t", "=kontora:"+taskID, "echo hello-from-tmux", "Enter").Run()
	require.NoError(t, err)

	// Read until we see the expected output or timeout.
	var received strings.Builder
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()

	for {
		_, data, err := conn.Read(readCtx)
		if err != nil {
			break
		}
		received.Write(data)
		if strings.Contains(received.String(), "hello-from-tmux") {
			break
		}
	}

	assert.Contains(t, received.String(), "hello-from-tmux")
	conn.Close(websocket.StatusNormalClosure, "")
}

// mockTerminalService is a minimal TicketService mock for terminal tests.
type mockTerminalService struct {
	hasSession bool
}

func (m *mockTerminalService) ListTickets() []TicketInfo { return nil }
func (m *mockTerminalService) RunningAgents() int        { return 0 }
func (m *mockTerminalService) GetTicket(_ string) (TicketInfo, error) {
	return TicketInfo{}, ErrTicketNotFound
}
func (m *mockTerminalService) CreateTicket(_ CreateTicketRequest) (TicketInfo, error) {
	return TicketInfo{}, nil
}
func (m *mockTerminalService) GetConfig() ConfigInfo                              { return ConfigInfo{} }
func (m *mockTerminalService) DeleteTicket(_ string) error                        { return nil }
func (m *mockTerminalService) PauseTicket(_ string) error                         { return nil }
func (m *mockTerminalService) RetryTicket(_ string) error                         { return nil }
func (m *mockTerminalService) SkipStage(_ string) error                           { return nil }
func (m *mockTerminalService) SetStage(_ string, _ string) error                  { return nil }
func (m *mockTerminalService) MoveTicket(_ string, _ string) error                { return nil }
func (m *mockTerminalService) InitTicket(_ string, _ InitTicketRequest) error     { return nil }
func (m *mockTerminalService) UpdateTicket(_ string, _ UpdateTicketRequest) error { return nil }
func (m *mockTerminalService) UploadTicket(_ []byte) (TicketInfo, error)          { return TicketInfo{}, nil }
func (m *mockTerminalService) GetLogs(_ string, _ string) (string, error) {
	return "", nil
}
func (m *mockTerminalService) Subscribe() (<-chan TicketEvent, func()) {
	return nil, func() {}
}
func (m *mockTerminalService) HasTerminalSession(_ string) bool { return m.hasSession }

func startTerminalTestServer(t *testing.T, svc TicketService) *Server {
	t.Helper()
	srv := New(svc, NewSSEBroker(), "127.0.0.1", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found, skipping")
	}
}

func startTmuxWindow(t *testing.T, session, window string) {
	t.Helper()
	env := append(os.Environ(), "TERM=xterm")

	// Check whether the session already exists before deciding which command to use.
	// Previously, we tried new-window first and fell back to new-session on any error.
	// That broke when the session existed but new-window failed for another reason
	// (e.g., duplicate window name), because new-session then also failed with
	// "duplicate session".
	hasSession := exec.Command("tmux", "has-session", "-t", "="+session)
	hasSession.Env = env

	if hasSession.Run() != nil {
		cmd := exec.Command("tmux", "new-session", "-d", "-s", session, "-n", window, "-x", "80", "-y", "24")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "tmux new-session failed: %s", out)
	} else {
		// Kill any leftover window with the same name from a previous run.
		_ = exec.Command("tmux", "kill-window", "-t", "="+session+":"+window).Run()

		cmd := exec.Command("tmux", "new-window", "-t", "="+session+":", "-n", window)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "tmux new-window failed: %s", out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-window", "-t", "="+session+":"+window).Run()
	})
}
