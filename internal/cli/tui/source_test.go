package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestNewSource_APIMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ticketsResponse{Tickets: []web.TicketInfo{}, RunningAgents: 0})
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: &http.Client{Timeout: 2 * time.Second}}
	assert.True(t, src.Connected())
}

func TestNewSource_FileMode(t *testing.T) {
	cfg := &config.Config{
		Web:        config.Web{Host: "127.0.0.1", Port: 19999},
		TicketsDir: t.TempDir(),
		Agents:     map[string]config.Agent{"claude": {Binary: "true"}},
	}
	src := newSource(cfg)
	assert.False(t, src.Connected())
}

func TestAPISource_FetchTickets(t *testing.T) {
	want := []web.TicketInfo{
		{ID: "tst-001", Title: "Test", Status: "in_progress"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ticketsResponse{Tickets: want, RunningAgents: 1})
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: srv.Client()}
	tickets, running, err := src.FetchTickets()
	require.NoError(t, err)
	assert.Equal(t, 1, running)
	assert.Equal(t, "tst-001", tickets[0].ID)
}

func TestAPISource_FetchTask(t *testing.T) {
	want := web.TicketInfo{ID: "tst-001", Title: "Test", Status: "in_progress", Body: "hello"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tickets/tst-001" {
			_ = json.NewEncoder(w).Encode(want)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: srv.Client()}

	info, err := src.FetchTask("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "hello", info.Body)

	_, err = src.FetchTask("missing")
	assert.Error(t, err)
}

func TestAPISource_Actions(t *testing.T) {
	var called string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = r.URL.Path
		_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "tst-001"})
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: srv.Client()}

	tests := []struct {
		name   string
		action func(string) error
		path   string
	}{
		{"pause", src.PauseTicket, "/api/tickets/tst-001/pause"},
		{"retry", src.RetryTicket, "/api/tickets/tst-001/retry"},
		{"skip", src.SkipStage, "/api/tickets/tst-001/skip"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.action("tst-001")
			require.NoError(t, err)
			assert.Equal(t, tc.path, called)
		})
	}
}

func TestAPISource_Subscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		info := web.TicketInfo{ID: "tst-001", Status: "in_progress"}
		data, _ := json.Marshal(info)
		fmt.Fprintf(w, "event: ticket_updated\ndata: %s\n\n", data)
		flusher.Flush()
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch := src.Subscribe(ctx)
	select {
	case ev := <-ch:
		assert.Equal(t, "ticket_updated", ev.Type)
		assert.Equal(t, "tst-001", ev.Ticket.ID)
	case <-ctx.Done():
		t.Fatal("timeout waiting for SSE event")
	}
}

func TestFileSource_FetchTickets(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001", "todo", "Test ticket 1")
	writeTicket(t, dir, "tst-002", "in_progress", "Test ticket 2")

	cfg := &config.Config{
		TicketsDir: dir,
		Agents:     map[string]config.Agent{"claude": {Binary: "true"}},
	}
	src := &fileSource{cfg: cfg}

	tickets, running, err := src.FetchTickets()
	require.NoError(t, err)
	assert.Equal(t, 1, running)
	assert.Len(t, tickets, 2)
}

func TestFileSource_FetchTask(t *testing.T) {
	dir := t.TempDir()
	writeTicket(t, dir, "tst-001", "todo", "Test ticket")

	cfg := &config.Config{
		TicketsDir: dir,
		Agents:     map[string]config.Agent{"claude": {Binary: "true"}},
	}
	src := &fileSource{cfg: cfg}

	info, err := src.FetchTask("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "Test ticket", info.Title)

	_, err = src.FetchTask("missing")
	assert.Error(t, err)
}

func TestAPISource_FetchConfig(t *testing.T) {
	want := web.ConfigInfo{
		Pipelines: []string{"default", "review"},
		Agents:    []string{"claude", "aider"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/config" {
			_ = json.NewEncoder(w).Encode(want)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: srv.Client()}
	cfg, err := src.FetchConfig()
	require.NoError(t, err)
	assert.Equal(t, []string{"default", "review"}, cfg.Pipelines)
	assert.Equal(t, []string{"claude", "aider"}, cfg.Agents)
}

func TestAPISource_CreateTicket(t *testing.T) {
	want := web.TicketInfo{ID: "new-001", Title: "Test Ticket", Status: "todo"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tickets" && r.Method == http.MethodPost {
			var req web.CreateTicketRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "Test Ticket", req.Title)
			assert.Equal(t, "mypath", req.Path)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(want)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: srv.Client()}
	info, err := src.CreateTicket(web.CreateTicketRequest{
		Title:  "Test Ticket",
		Path:   "mypath",
		Status: "todo",
	})
	require.NoError(t, err)
	assert.Equal(t, "new-001", info.ID)
	assert.Equal(t, "Test Ticket", info.Title)
}

func TestAPISource_UpdateTicket(t *testing.T) {
	var receivedMethod string
	var receivedPath string
	var receivedAgent *string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		var req web.UpdateTicketRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			receivedAgent = req.Agent
		}
		_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "tst-001", Agent: "opus"})
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: srv.Client()}
	agent := "opus"
	err := src.UpdateTicket("tst-001", web.UpdateTicketRequest{Agent: &agent})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, receivedMethod)
	assert.Equal(t, "/api/tickets/tst-001", receivedPath)
	require.NotNil(t, receivedAgent)
	assert.Equal(t, "opus", *receivedAgent)
}

func TestAPISource_UpdateTicket_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown agent"})
	}))
	defer srv.Close()

	src := &apiSource{base: srv.URL, client: srv.Client()}
	agent := "bad"
	err := src.UpdateTicket("tst-001", web.UpdateTicketRequest{Agent: &agent})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown agent")
}

func TestFileSource_ActionsUnavailable(t *testing.T) {
	cfg := &config.Config{TicketsDir: t.TempDir()}
	src := &fileSource{cfg: cfg}

	assert.ErrorIs(t, src.UpdateTicket("x", web.UpdateTicketRequest{}), errDaemonNotRunning)
	assert.ErrorIs(t, src.PauseTicket("x"), errDaemonNotRunning)
	assert.ErrorIs(t, src.RetryTicket("x"), errDaemonNotRunning)
	assert.ErrorIs(t, src.SkipStage("x"), errDaemonNotRunning)
	assert.Nil(t, src.Subscribe(context.Background()))

	_, err := src.FetchConfig()
	assert.ErrorIs(t, err, errDaemonNotRunning)

	_, err = src.CreateTicket(web.CreateTicketRequest{Title: "x", Path: "y"})
	assert.ErrorIs(t, err, errDaemonNotRunning)
}

func TestFileSource_FetchLogs(t *testing.T) {
	ticketsDir := t.TempDir()
	logsDir := t.TempDir()
	writeTicket(t, ticketsDir, "sim-001", "done", "Simple task")

	logDir := filepath.Join(logsDir, "sim-001")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "default.log"), []byte("log output"), 0o644))

	cfg := &config.Config{TicketsDir: ticketsDir, LogsDir: logsDir}
	src := &fileSource{cfg: cfg}

	content, err := src.FetchLogs("sim-001", "default")
	require.NoError(t, err)
	assert.Equal(t, "log output", content)

	content, err = src.FetchLogs("sim-001", "")
	require.NoError(t, err)
	assert.Equal(t, "log output", content)
}

func TestFileSource_FetchLogsHistoryFallback(t *testing.T) {
	ticketsDir := t.TempDir()
	logsDir := t.TempDir()
	writeTicket(t, ticketsDir, "sim-002", "done", "No log files")

	cfg := &config.Config{TicketsDir: ticketsDir, LogsDir: logsDir}
	src := &fileSource{cfg: cfg}

	// Empty stage falls back to history.
	content, err := src.FetchLogs("sim-002", "")
	require.NoError(t, err)
	assert.Contains(t, content, "no logs found")

	// Named stage also falls back when the log file is missing.
	// This is the path the TUI takes: simple tickets have Stages=["default"],
	// so the initial log fetch requests stage="default".
	content, err = src.FetchLogs("sim-002", "default")
	require.NoError(t, err)
	assert.Contains(t, content, "no logs found")
}

func TestFileSource_BuildFileTicketInfoSimpleTask(t *testing.T) {
	ticketsDir := t.TempDir()
	logsDir := t.TempDir()
	writeTicket(t, ticketsDir, "sim-001", "done", "Simple task")

	logDir := filepath.Join(logsDir, "sim-001")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "default.log"), []byte("log"), 0o644))

	cfg := &config.Config{TicketsDir: ticketsDir, LogsDir: logsDir}
	src := &fileSource{cfg: cfg}

	info, err := src.FetchTask("sim-001")
	require.NoError(t, err)
	assert.Equal(t, []string{"default"}, info.Stages)
	assert.True(t, info.Kontora)
}

func TestFileSource_BuildFileTicketInfoSimpleTaskNoLogs(t *testing.T) {
	ticketsDir := t.TempDir()
	logsDir := t.TempDir()
	writeTicket(t, ticketsDir, "sim-001", "todo", "Simple task")

	cfg := &config.Config{TicketsDir: ticketsDir, LogsDir: logsDir}
	src := &fileSource{cfg: cfg}

	info, err := src.FetchTask("sim-001")
	require.NoError(t, err)
	assert.Equal(t, []string{"default"}, info.Stages)
}

func writeTicket(t *testing.T, dir, id, status, title string) {
	t.Helper()
	content := fmt.Sprintf("---\nid: %s\nstatus: %s\nkontora: true\n---\n# %s\n", id, status, title)
	err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(content), 0o644)
	require.NoError(t, err)
}
