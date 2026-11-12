package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestWSURL(t *testing.T) {
	tests := []struct {
		name       string
		base       string
		path       string
		wantPrefix string
	}{
		{"http to ws", "http://daemon:8080", "/ws/terminal/tst-001", "ws://daemon:8080/ws/terminal/tst-001"},
		{"https to wss", "https://daemon:8080", "/ws/terminal/tst-001", "wss://daemon:8080/ws/terminal/tst-001"},
		{"automation run", "https://daemon:8080", "/ws/automations/aut-example/42", "wss://daemon:8080/ws/automations/aut-example/42"},
		{"automation-prefix ticket", "http://daemon:8080", "/ws/terminal/aut-1234", "ws://daemon:8080/ws/terminal/aut-1234"},
		{"base path", "https://daemon:8080/kontora/", "/ws/terminal/tst-001", "wss://daemon:8080/kontora/ws/terminal/tst-001"},
		{"escaped ID", "https://daemon:8080", "/ws/automations/deleted%2Fid%20%3F%23%25/42", "wss://daemon:8080/ws/automations/deleted%2Fid%20%3F%23%25/42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := wsURL(tc.base, tc.path, true, 120, 40)
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

func TestAttachTargetsAndCredentials(t *testing.T) {
	const id = "aut-0123456789abcdef0123456789abcdef"
	targets := []struct {
		name, id, lookup, socket string
		call                     func(context.Context, *Client) error
	}{
		{
			name: "current automation", id: id, lookup: "/api/automations/" + id, socket: "/ws/automations/" + id + "/7",
			call: func(ctx context.Context, c *Client) error { return AttachAutomation(ctx, c, id, 0, false) },
		},
		{
			name: "top-level automation", id: id, lookup: "/api/automations/" + id, socket: "/ws/automations/" + id + "/7",
			call: func(ctx context.Context, c *Client) error { return Attach(ctx, c, id, false) },
		},
		{
			name: "exact deleted automation run", id: id, socket: "/ws/automations/" + id + "/19",
			call: func(ctx context.Context, c *Client) error { return AttachAutomation(ctx, c, id, 19, false) },
		},
		{
			name: "client method", id: id, socket: "/ws/automations/" + id + "/18446744073709551615",
			call: func(_ context.Context, c *Client) error { return c.AttachAutomation(id, ^uint64(0), false) },
		},
		{
			name: "ticket", id: "tst-001", lookup: "/api/tickets/tst-001", socket: "/ws/terminal/tst-001",
			call: func(ctx context.Context, c *Client) error { return Attach(ctx, c, "tst-001", false) },
		},
		{
			name: "automation-prefix ticket", id: "aut-1234", lookup: "/api/tickets/aut-1234", socket: "/ws/terminal/aut-1234",
			call: func(ctx context.Context, c *Client) error { return Attach(ctx, c, "aut-1234", false) },
		},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			for _, credentials := range []struct{ name, bearer, operator string }{
				{"none", "", ""}, {"bearer", "bearer-secret", ""},
				{"operator", "", "operator-secret"}, {"both", "bearer-secret", "operator-secret"},
			} {
				t.Run(credentials.name, func(t *testing.T) {
					var lookups, sockets atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						auth := ""
						if credentials.bearer != "" {
							auth = "Bearer " + credentials.bearer
						}
						assert.Equal(t, auth, r.Header.Get("Authorization"))
						assert.Equal(t, credentials.operator, r.Header.Get(web.OperatorTokenHeader))
						if strings.HasPrefix(r.URL.Path, "/api/") {
							lookups.Add(1)
							assert.Equal(t, target.lookup, r.URL.Path)
							if strings.HasPrefix(r.URL.Path, "/api/automations/") {
								_ = json.NewEncoder(w).Encode(automation.Definition{ID: target.id, Running: true, CurrentRun: &automation.Run{N: 7}})
							} else {
								_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: target.id, Status: "in_progress"})
							}
							return
						}
						sockets.Add(1)
						assert.Equal(t, target.socket, r.URL.Path)
						assert.Equal(t, "0", r.URL.Query().Get("rw"))
						assert.NotEmpty(t, r.URL.Query().Get("cols"))
						assert.NotEmpty(t, r.URL.Query().Get("rows"))
						conn, err := websocket.Accept(w, r, nil)
						if !assert.NoError(t, err) {
							return
						}
						_ = conn.Close(websocket.StatusNormalClosure, "")
					}))
					defer srv.Close()
					c := New(srv.URL, credentials.bearer).WithOperatorToken(credentials.operator)
					require.NoError(t, target.call(t.Context(), c))
					wantLookups := int32(0)
					if target.lookup != "" {
						wantLookups = 1
					}
					assert.Equal(t, wantLookups, lookups.Load())
					assert.EqualValues(t, 1, sockets.Load())
				})
			}
		})
	}
}

func TestAttachAutomationWithoutCurrentRun(t *testing.T) {
	const id = "aut-0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		row  automation.Definition
	}{
		{"not running", automation.Definition{CurrentRun: &automation.Run{N: 7}}},
		{"missing run", automation.Definition{Running: true}},
		{"zero run", automation.Definition{Running: true, CurrentRun: &automation.Run{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "/api/automations/"+id, r.URL.Path)
				_ = json.NewEncoder(w).Encode(tc.row)
			}))
			defer srv.Close()
			err := AttachAutomation(t.Context(), New(srv.URL, ""), id, 0, false)
			require.ErrorContains(t, err, "has no live session")
			assert.EqualValues(t, 1, requests.Load())
		})
	}
}

func TestAttachAutomationDefinitionFailure(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, "/api/automations/deleted", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"definition deleted"}`)
	}))
	defer srv.Close()
	err := AttachAutomation(t.Context(), New(srv.URL, ""), "deleted", 0, false)
	require.ErrorContains(t, err, "definition deleted")
	assert.EqualValues(t, 1, requests.Load())
}

func TestAttachAutomationExactRunHandshake(t *testing.T) {
	const id = "deleted/id ?#%"
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"not found", http.StatusNotFound, "no live terminal session"},
		{"unauthorized", http.StatusUnauthorized, "rejected the token"},
		{"forbidden", http.StatusForbidden, "rejected the token"},
		{"other failure", http.StatusBadGateway, "connecting to remote terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, rw := range []bool{false, true} {
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					assert.Equal(t, "/ws/automations/"+url.PathEscape(id)+"/42", r.URL.EscapedPath())
					assert.Equal(t, map[bool]string{false: "0", true: "1"}[rw], r.URL.Query().Get("rw"))
					w.WriteHeader(tc.status)
				}))
				err := AttachAutomation(t.Context(), New(srv.URL, ""), id, 42, rw)
				require.ErrorContains(t, err, tc.want)
				assert.EqualValues(t, 1, requests.Load())
				srv.Close()
			}
		})
	}
}

func TestAttach_HandshakeFailuresReported(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		operator bool
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
		{name: "ordinary automation-prefix ticket", id: "aut-1234", ticketStatus: "paused", want: `has status "paused"`},
		{name: "automation exact-run socket", id: "aut-0123456789abcdef0123456789abcdef", handshake: http.StatusNotFound, want: "no live terminal session"},
		{name: "automation operator credential", id: "aut-0123456789abcdef0123456789abcdef", operator: true, handshake: http.StatusUnauthorized, want: "rejected the token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.id
			if id == "" {
				id = "tst-001"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.operator {
					assert.Equal(t, "operator-secret", r.Header.Get(web.OperatorTokenHeader))
				}
				if strings.HasPrefix(r.URL.Path, "/api/automations/") {
					require.True(t, automation.IsID(id))
					_ = json.NewEncoder(w).Encode(automation.Definition{ID: id, Running: true, CurrentRun: &automation.Run{N: 7, Phase: "running"}})
					return
				}
				if strings.HasPrefix(r.URL.Path, "/api/tickets/") {
					if tc.ticketStatus == "" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(web.TicketInfo{ID: "tst-001", Status: tc.ticketStatus})
					return
				}
				if automation.IsID(id) {
					assert.Equal(t, "/ws/automations/"+id+"/7", r.URL.Path)
				}
				w.WriteHeader(tc.handshake)
			}))
			defer srv.Close()
			client := New(srv.URL, "wrong")
			if tc.operator {
				client.operatorToken = "operator-secret"
			}
			err := Attach(context.Background(), client, id, false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
