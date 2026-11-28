package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/stats"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestPrintStatsOutcomes(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		outcome string
		exit    int
		counts  []string
	}{
		{name: "legacy runner failure", exit: 7, counts: []string{"1", "0", "0", "0", "0", "1", "1"}},
		{name: "legacy clean exit", counts: []string{"1", "0", "0", "0", "0", "1", "0"}},
		{name: "unrecognized outcome", outcome: "future", counts: []string{"1", "0", "0", "0", "0", "1", "0"}},
		{name: "failed outcome and runner", outcome: "failed", exit: 7, counts: []string{"1", "0", "0", "0", "1", "0", "1"}},
		{name: "complete", outcome: "complete", counts: []string{"1", "1", "0", "0", "0", "0", "0"}},
		{name: "blocked", outcome: "blocked", counts: []string{"1", "0", "1", "0", "0", "0", "0"}},
		{name: "protocol error", outcome: "protocol_error", counts: []string{"1", "0", "0", "1", "0", "0", "0"}},
		{name: "semantic failure", outcome: "failed", counts: []string{"1", "0", "0", "0", "1", "0", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := stats.Compute([]stats.Ticket{{ID: "kon-stats", History: []stats.Run{{
				Stage: "code", StartedAt: &now, CompletedAt: &now, RunOutcome: tc.outcome, ExitCode: tc.exit,
			}}}}, stats.Options{Now: now, Days: 7})
			var out bytes.Buffer
			require.NoError(t, PrintStats(info, &out))
			lines := strings.Split(out.String(), "\n")
			var header, row []string
			for _, line := range lines {
				fields := strings.Fields(line)
				if len(fields) > 0 && fields[0] == "STAGE" {
					header = fields
				}
				if len(fields) > 0 && fields[0] == "code" {
					row = fields
				}
			}
			require.Equal(t, []string{"STAGE", "RUNS", "COMPLETE", "BLOCKED", "PROTOCOL_ERROR", "FAILED", "UNVERIFIED", "RUNNER_FAILED", "P50", "P90", "TOKENS"}, header)
			require.Len(t, row, len(header))
			require.Equal(t, tc.counts, row[1:8])
		})
	}
}

func TestLocalClient(t *testing.T) {
	board, other := t.TempDir(), t.TempDir()
	for _, tc := range []struct {
		name, host, advertised string
		disabled, ipv6, closed bool
		status                 int
		body, cause            string
		wantError              []string
	}{
		{name: "web disabled", host: "127.0.0.1", disabled: true, wantError: []string{"web.enabled is false"}},
		{name: "IPv4 wildcard", host: "0.0.0.0"},
		{name: "IPv6 wildcard", host: "::", ipv6: true},
		{name: "bracketed IPv6 wildcard", host: "[::]", ipv6: true},
		{name: "other board", host: "127.0.0.1", advertised: other, wantError: []string{"does not match requested board", board, other}},
		{name: "unauthorized", host: "127.0.0.1", status: http.StatusUnauthorized, body: `{"error":"unauthorized: set KONTORA_TOKEN"}`, wantError: []string{"returned HTTP 401", "Set web.token in the local config and retry", "--url with --token", "Explicit remote mode does not check local tickets_dir"}},
		{name: "not found", host: "127.0.0.1", status: http.StatusNotFound, cause: "HTTP 404"},
		{name: "invalid config response", host: "127.0.0.1", body: "{", cause: "unexpected EOF"},
		{name: "connection refused", host: "127.0.0.1", closed: true, cause: "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			advertised := tc.advertised
			if advertised == "" {
				advertised = board
			}
			var connections, requests atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "GET /api/config", r.Method+" "+r.URL.Path)
				if tc.status == http.StatusUnauthorized {
					assert.Empty(t, r.Header.Get("Authorization"))
				} else {
					assert.Equal(t, "Bearer local-token", r.Header.Get("Authorization"))
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				switch {
				case tc.body != "":
					_, _ = fmt.Fprint(w, tc.body)
				case tc.status == 0:
					_ = json.NewEncoder(w).Encode(web.ConfigInfo{TicketsDir: advertised})
				}
			}))
			defer srv.Close()
			if tc.ipv6 {
				listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp6", "[::1]:0")
				if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL) {
					t.Skipf("IPv6 loopback is unavailable: %v", err)
				}
				require.NoError(t, err)
				require.NoError(t, srv.Listener.Close())
				srv.Listener = listener
			}
			srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			srv.Start()
			address := srv.Listener.Addr().String()
			port := srv.Listener.Addr().(*net.TCPAddr).Port
			if tc.closed {
				srv.Close()
			}
			token := "local-token"
			if tc.status == http.StatusUnauthorized {
				token = ""
			}
			cfg := &config.Config{TicketsDir: board, Web: config.Web{
				Host: tc.host, Port: port, Token: token, Enabled: new(!tc.disabled),
			}}
			client, err := LocalClient(cfg)
			switch {
			case tc.cause != "":
				require.ErrorContains(t, err, "cannot verify daemon board: GET "+srv.URL+"/api/config: ")
				assert.ErrorContains(t, err, tc.cause)
				assert.Equal(t, 1, strings.Count(err.Error(), address))
				assert.Nil(t, client)
			case len(tc.wantError) > 0:
				require.Error(t, err)
				for _, want := range tc.wantError {
					assert.ErrorContains(t, err, want)
				}
				assert.Nil(t, client)
			default:
				require.NoError(t, err)
				assert.Equal(t, srv.URL, client.Base())
			}
			if tc.disabled || tc.closed {
				assert.Zero(t, connections.Load())
				assert.Zero(t, requests.Load())
				return
			}
			assert.EqualValues(t, 1, requests.Load())
			if tc.advertised != "" || tc.status == http.StatusUnauthorized {
				assert.ErrorContains(t, err, srv.URL)
				assert.NotContains(t, err.Error(), "KONTORA_TOKEN")
			}
		})
	}
}

func TestLocalClientRefusesNonLoopback(t *testing.T) {
	const port = 8080
	for _, tc := range []struct{ host, address string }{
		{"100.64.0.10", "100.64.0.10"},
		{"2001:db8::1", "[2001:db8::1]"},
		{"[2001:db8::1]", "[2001:db8::1]"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			client, err := LocalClient(&config.Config{Web: config.Web{Host: tc.host, Port: port}})
			require.ErrorContains(t, err, "web.host "+strconv.Quote(tc.host))
			assert.ErrorContains(t, err, "--url http://"+tc.address+":8080 with --token")
			assert.ErrorContains(t, err, "Explicit remote mode does not check local tickets_dir")
			assert.Nil(t, client)
		})
	}
}
