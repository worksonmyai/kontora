package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestRunSurfacesDaemonErrorWithoutReachabilityPrefix(t *testing.T) {
	ticketsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ticketsDir, "abc123.md"), []byte("# Test\n"), 0o644))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tickets/abc123/run", r.URL.Path)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid state transition"})
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)

	err = Run(&config.Config{
		TicketsDir: ticketsDir,
		Web: config.Web{
			Host: host,
			Port: port,
		},
	}, "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid state transition")
	assert.NotContains(t, err.Error(), "daemon not reachable")
}
