package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/tmux"
)

func TestServer_Index(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "kontora")
}

func TestServer_StartShutdown(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get("http://" + srv.Addr() + "/api/tickets")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	err = srv.Shutdown(context.Background())
	require.NoError(t, err)

	// After shutdown, connections should fail.
	client := &http.Client{Timeout: 100 * time.Millisecond}
	resp2, err := client.Get("http://" + srv.Addr() + "/api/tickets")
	if err == nil {
		resp2.Body.Close()
	}
	assert.Error(t, err)
}

// startTestServer builds a server, starts it on a loopback port and shuts it
// down with the test. The named helpers around the package each vary one
// argument of it.
func startTestServer(t *testing.T, svc TicketService, broker *SSEBroker, token, tmuxSession string) *Server {
	t.Helper()
	srv, err := New(svc, broker, "127.0.0.1", 0, token, nil, tmuxSession, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return startTestServer(t, &mockService{}, NewSSEBroker(), "", tmux.DefaultSessionName)
}
