package web

import (
	"bytes"
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
	srv := startTerminalTestServer(t, svc, uniqueSession(t))

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

	// A non-default session name also proves the handler attaches to the
	// session the server was given rather than probing "kontora".
	session := uniqueSession(t)
	taskID := "test-term-ws"
	startTmuxWindow(t, session, taskID)

	svc := &mockTerminalService{hasSession: true}
	srv := startTerminalTestServer(t, svc, session)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s/ws/terminal/%s?cols=80&rows=24", srv.Addr(), taskID), nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	defer func() { _ = conn.CloseNow() }()

	// Send some output to the tmux window.
	err = exec.Command("tmux", "send-keys", "-t", "="+session+":"+taskID, "echo hello-from-tmux", "Enter").Run()
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

func TestClampDim(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"below the cap", 240, 240},
		{"at the cap", maxTerminalDim, maxTerminalDim},
		{"above the cap", maxTerminalDim + 1, maxTerminalDim},
		// uint16(65536) is 0, which would leave tmux and the pty with no columns.
		{"past uint16", 65536, maxTerminalDim},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clampDim(tt.in))
		})
	}
}

// stubWriter records the payload of every websocket write, and blocks until the
// write context expires once blocked is set.
type stubWriter struct {
	blocked bool
	writes  [][]byte
}

func (w *stubWriter) Write(ctx context.Context, _ websocket.MessageType, p []byte) error {
	if w.blocked {
		<-ctx.Done()
		return ctx.Err()
	}
	w.writes = append(w.writes, append([]byte(nil), p...))
	return nil
}

func TestPipeOutput(t *testing.T) {
	tests := []struct {
		name         string
		payload      []byte
		blocked      bool
		writeTimeout time.Duration
		wantWrites   [][]byte
	}{
		{
			name:         "burst filling the read buffer becomes one frame",
			payload:      bytes.Repeat([]byte("x"), ptyReadBufSize),
			writeTimeout: time.Second,
			wantWrites:   [][]byte{bytes.Repeat([]byte("x"), ptyReadBufSize)},
		},
		{
			name:         "echo-sized output is forwarded without waiting",
			payload:      []byte("ls\r\n"),
			writeTimeout: time.Second,
			wantWrites:   [][]byte{[]byte("ls\r\n")},
		},
		{
			name:         "a client that stops reading is dropped at the deadline",
			payload:      []byte("hello"),
			blocked:      true,
			writeTimeout: 50 * time.Millisecond,
			wantWrites:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			w := &stubWriter{blocked: tt.blocked}

			done := make(chan struct{})
			go func() {
				defer close(done)
				srv.pipeOutput(context.Background(), w, bytes.NewReader(tt.payload), "tst-001", tt.writeTimeout)
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("pipeOutput did not return; a stalled client would hold the pty read loop")
			}
			assert.Equal(t, tt.wantWrites, w.writes)
		})
	}
}

// mockTerminalService is a minimal TicketService mock for terminal tests.
type mockTerminalService struct {
	hasSession bool
}

func (m *mockTerminalService) ListTickets(ListTicketsOptions) []TicketInfo { return nil }
func (m *mockTerminalService) RunningAgents() int                          { return 0 }
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
func (m *mockTerminalService) RunTicket(_ string) error                           { return nil }
func (m *mockTerminalService) SkipStage(_ string) error                           { return nil }
func (m *mockTerminalService) SetStage(_ string, _ string) error                  { return nil }
func (m *mockTerminalService) MoveTicket(_ string, _ string) error                { return nil }
func (m *mockTerminalService) AddNote(_ string, _ string) error                   { return nil }
func (m *mockTerminalService) SetSummary(_ string, _ string) error                { return nil }
func (m *mockTerminalService) GetChanges(_ string) (ChangesInfo, error)           { return ChangesInfo{}, nil }
func (m *mockTerminalService) InitTicket(_ string, _ InitTicketRequest) error     { return nil }
func (m *mockTerminalService) AddDependency(_ string, _ string) error             { return nil }
func (m *mockTerminalService) RemoveDependency(_ string, _ string) error          { return nil }
func (m *mockTerminalService) LinkTickets(_ string, _ []string) error             { return nil }
func (m *mockTerminalService) UnlinkTickets(_ string, _ []string) error           { return nil }
func (m *mockTerminalService) UpdateTicket(_ string, _ UpdateTicketRequest) error { return nil }
func (m *mockTerminalService) UploadTicket(_ []byte) (TicketInfo, error)          { return TicketInfo{}, nil }
func (m *mockTerminalService) GetLogs(_ string, _ string) (string, error) {
	return "", nil
}
func (m *mockTerminalService) GetActivity(ActivityQuery) (ActivityInfo, error) {
	return ActivityInfo{}, nil
}
func (m *mockTerminalService) GetStats(StatsQuery) (StatsInfo, error) { return StatsInfo{}, nil }
func (m *mockTerminalService) GetRawConfig() (string, error)          { return "", nil }
func (m *mockTerminalService) PutRawConfig(_ string) error            { return nil }
func (m *mockTerminalService) Subscribe() (<-chan TicketEvent, func()) {
	return nil, func() {}
}
func (m *mockTerminalService) HasTerminalSession(_ string) bool { return m.hasSession }
func (m *mockTerminalService) StartPlannotatorReview(_ string) error {
	return nil
}
func (m *mockTerminalService) StartPlannotatorAnnotate(_ string) error {
	return nil
}

// uniqueSession names a tmux session no other test binary or running daemon
// can share, so these tests never attach to or tear down real agent windows.
func uniqueSession(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("kontora-webtest-%d-%s", os.Getpid(), strings.ToLower(t.Name()))
}

func startTerminalTestServer(t *testing.T, svc TicketService, tmuxSession string) *Server {
	t.Helper()
	srv := New(svc, NewSSEBroker(), "127.0.0.1", 0, "", tmuxSession, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	newSession := exec.Command("tmux", "new-session", "-d", "-s", session, "-n", window, "-x", "80", "-y", "24")
	newSession.Env = append(os.Environ(), "TERM=xterm")
	out, err := newSession.CombinedOutput()
	require.NoError(t, err, "failed to create tmux session: %s", out)

	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", "="+session).Run()
	})
}
