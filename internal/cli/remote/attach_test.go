package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/web"
)

func TestWSURL(t *testing.T) {
	tests := []struct {
		name       string
		base       string
		wantPrefix string
	}{
		{"http to ws", "http://daemon:8080", "ws://daemon:8080/ws/terminal/tst-001"},
		{"https to wss", "https://daemon:8080", "wss://daemon:8080/ws/terminal/tst-001"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := wsURL(tc.base, "tst-001", true, 120, 40)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(u, tc.wantPrefix), "got %q", u)
			assert.Contains(t, u, "rw=1")
			assert.Contains(t, u, "cols=120")
			assert.Contains(t, u, "rows=40")
		})
	}
}

func TestRunBridge_Framing(t *testing.T) {
	var gotType, gotData string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()

		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m termMsg
		_ = json.Unmarshal(data, &m)
		gotType, gotData = m.Type, m.Data

		// Echo the keystrokes back as a binary output frame, then close.
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("echo:"+m.Data))
	}))
	defer srv.Close()

	conn, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/terminal/tst-001", nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var out strings.Builder
	var mu sync.Mutex
	err = runBridge(context.Background(), conn, strings.NewReader("hello"), &out, &mu, true)
	require.NoError(t, err)

	assert.Equal(t, "input", gotType)
	assert.Equal(t, "hello", gotData)
	assert.Equal(t, "echo:hello", out.String())
}

func TestRunBridge_ReadOnlyDoesNotForwardInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("out"))
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	conn, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/terminal/tst-001", nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var out strings.Builder
	var mu sync.Mutex
	in := &trackingReader{}
	err = runBridge(context.Background(), conn, in, &out, &mu, false)
	require.NoError(t, err)
	assert.Equal(t, "out", out.String())
	assert.False(t, in.used.Load(), "read-only bridge must not read stdin")
}

// trackingReader records whether Read was ever called.
type trackingReader struct{ used atomic.Bool }

func (r *trackingReader) Read([]byte) (int, error) {
	r.used.Store(true)
	return 0, io.EOF
}

func TestRunBridge_AbnormalCloseSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Drop the session with a non-normal status so the client must report it.
		conn.Close(websocket.StatusInternalError, "boom")
	}))
	defer srv.Close()

	conn, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/terminal/tst-001", nil)
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var out strings.Builder
	var mu sync.Mutex
	err = runBridge(context.Background(), conn, strings.NewReader(""), &out, &mu, true)
	require.Error(t, err)
}

func TestAttach_HandshakeFailuresReported(t *testing.T) {
	cases := []struct {
		name string
		// ticketStatus is what GET /api/tickets/{id} answers; empty means the
		// ticket lookup itself fails with the handshake status.
		ticketStatus string
		handshake    int
		want         string
	}{
		{
			name:         "auth rejection names the token",
			ticketStatus: "in_progress",
			handshake:    http.StatusUnauthorized,
			want:         "rejected the token",
		},
		{
			name:         "no live session says so",
			ticketStatus: "in_progress",
			handshake:    http.StatusNotFound,
			want:         "no live terminal session",
		},
		{
			name:         "a ticket that is not running never dials",
			ticketStatus: "paused",
			handshake:    http.StatusOK,
			want:         `has status "paused", must be in_progress to attach`,
		},
		{
			name:      "an unknown ticket is named",
			handshake: http.StatusNotFound,
			want:      "not found on the daemon",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/tickets/") {
					if tc.ticketStatus == "" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "tst-001", Status: tc.ticketStatus})
					return
				}
				w.WriteHeader(tc.handshake)
			}))
			defer srv.Close()

			err := Attach(context.Background(), New(srv.URL, "wrong"), "tst-001", false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
