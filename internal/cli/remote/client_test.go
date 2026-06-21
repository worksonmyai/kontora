package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/web"
)

func encodeList(w http.ResponseWriter, tickets []web.TicketInfo) {
	_ = json.NewEncoder(w).Encode(struct {
		Tickets       []web.TicketInfo `json:"tickets"`
		RunningAgents int              `json:"running_agents"`
	}{Tickets: tickets, RunningAgents: 0})
}

func TestClient_BearerHeaderInjected(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		encodeList(w, nil)
	}))
	defer srv.Close()

	c := New(srv.URL, "secret")
	_, _, err := c.ListTickets()
	require.NoError(t, err)
	assert.Equal(t, "Bearer secret", gotAuth)
}

func TestClient_NoTokenNoHeader(t *testing.T) {
	var hasAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth = r.Header["Authorization"]
		encodeList(w, nil)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, _, err := c.ListTickets()
	require.NoError(t, err)
	assert.False(t, hasAuth, "no Authorization header expected when token is empty")
}

func TestClient_BearerOnPostAction(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "tst-001"})
	}))
	defer srv.Close()

	c := New(srv.URL, "secret")
	require.NoError(t, c.Run("tst-001"))
	assert.Equal(t, "Bearer secret", gotAuth)
	assert.Equal(t, "/api/tickets/tst-001/run", gotPath)
}

func TestClient_DeleteTicket(t *testing.T) {
	var gotMethod, gotPath, gotConfirm, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotConfirm = r.Header.Get("X-Kontora-Confirm")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "secret")
	require.NoError(t, c.DeleteTicket("tst-001"))
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/api/tickets/tst-001", gotPath)
	assert.Equal(t, "delete-ticket-file", gotConfirm)
	assert.Equal(t, "Bearer secret", gotAuth)
}

func TestClient_DeleteTicket_ErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "ticket not found"})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	err := c.DeleteTicket("missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ticket not found")
}

func TestClient_Init(t *testing.T) {
	var gotPath string
	var gotReq web.InitTicketRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "tst-001"})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	req := web.InitTicketRequest{Pipeline: "two-stage", Path: "/repo", Agent: "claude"}
	require.NoError(t, c.Init("tst-001", req))
	assert.Equal(t, "/api/tickets/tst-001/init", gotPath)
	assert.Equal(t, req, gotReq)
}

func TestClient_RawConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/config/raw", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]string{"content": "tickets_dir: ~/x\n"})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	got, err := c.RawConfig()
	require.NoError(t, err)
	assert.Equal(t, "tickets_dir: ~/x\n", got)
}

func TestClient_PutRawConfig(t *testing.T) {
	var gotMethod, gotPath, gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotContent = body.Content
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	require.NoError(t, c.PutRawConfig("foo: bar\n"))
	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/api/config/raw", gotPath)
	assert.Equal(t, "foo: bar\n", gotContent)
}

func TestClient_PutRawConfig_ValidationErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid config: bad agent"})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	err := c.PutRawConfig("nonsense")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid config")
}

func TestClient_ResolveID(t *testing.T) {
	tickets := []web.TicketInfo{
		{ID: "abc123def"},
		{ID: "xyz999"},
		{ID: "abc"},
		{ID: "pre-b"},
		{ID: "pre-a"},
		{ID: "hidden-123-visible"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tickets":
			encodeList(w, tickets)
		case "/api/tickets/hidden-1":
			_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "hidden-1"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "ticket not found"})
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")

	t.Run("exact match wins over prefix", func(t *testing.T) {
		id, err := c.ResolveID("abc")
		require.NoError(t, err)
		assert.Equal(t, "abc", id)
	})

	t.Run("prefix match", func(t *testing.T) {
		id, err := c.ResolveID("xyz")
		require.NoError(t, err)
		assert.Equal(t, "xyz999", id)
	})

	t.Run("prefix match is deterministic", func(t *testing.T) {
		id, err := c.ResolveID("pre-")
		require.NoError(t, err)
		assert.Equal(t, "pre-a", id)
	})

	t.Run("exact hidden ticket wins over visible prefix", func(t *testing.T) {
		id, err := c.ResolveID("hidden-1")
		require.NoError(t, err)
		assert.Equal(t, "hidden-1", id)
	})

	t.Run("no match errors", func(t *testing.T) {
		_, err := c.ResolveID("nope")
		assert.Error(t, err)
	})
}

func TestClient_CancelAndDoneMapToMove(t *testing.T) {
	var gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotStatus = body.Status
		_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "tst-001"})
	}))
	defer srv.Close()

	c := New(srv.URL, "")

	require.NoError(t, c.Cancel("tst-001"))
	assert.Equal(t, "cancelled", gotStatus)

	require.NoError(t, c.Done("tst-001"))
	assert.Equal(t, "done", gotStatus)
}

func TestClient_NoteSendsText(t *testing.T) {
	var gotText, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotText = body.Text
		_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "tst-001"})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	require.NoError(t, c.Note("tst-001", "blocked on review"))
	assert.Equal(t, "/api/tickets/tst-001/note", gotPath)
	assert.Equal(t, "blocked on review", gotText)
}

func TestClient_ServerErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown pipeline \"bogus\""})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	err := c.Run("tst-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown pipeline")
}

func TestClient_UnaryRequestTimesOutReadingBody(t *testing.T) {
	oldTimeout := unaryRequestTimeout
	unaryRequestTimeout = 20 * time.Millisecond
	t.Cleanup(func() { unaryRequestTimeout = oldTimeout })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	started := time.Now()
	_, _, err := c.ListTickets()
	require.Error(t, err)
	assert.Less(t, time.Since(started), time.Second)
}

func TestClient_SSEUnauthorizedStopsReconnecting(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, "wrong")
	ch := c.Subscribe(context.Background())

	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel should close, not deliver events")
	case <-time.After(2 * time.Second):
		t.Fatal("sseLoop did not stop after a 401")
	}
	assert.Equal(t, int32(1), hits.Load(), "a 401 must not trigger a reconnect")
}

func TestClient_SSEStreamEOFBacksOff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := New(srv.URL, "")
	ch := c.Subscribe(ctx)

	require.Eventually(t, func() bool { return hits.Load() == 1 }, time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), hits.Load(), "a closed 200 stream should back off before reconnecting")

	cancel()
	select {
	case _, ok := <-ch:
		assert.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("sseLoop did not stop after context cancellation")
	}
}
