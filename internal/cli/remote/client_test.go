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

func TestClient_ResolveID(t *testing.T) {
	tickets := []web.TicketInfo{
		{ID: "abc123def"},
		{ID: "xyz999"},
		{ID: "abc"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		encodeList(w, tickets)
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
