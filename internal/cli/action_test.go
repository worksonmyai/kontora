package cli

import (
	"encoding/json"
	"fmt"
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
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func TestLocalStageActions(t *testing.T) {
	for _, operation := range []string{"skip", "set-stage"} {
		for _, status := range []string{"in_progress", "paused"} {
			t.Run(operation+"/"+status, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "abc123.md")
				before := []byte(fmt.Sprintf("---\nid: abc123\nkontora: true\nstatus: %s\npipeline: test\nstage: code\nattempt: 3\ncustom_field: preserved\n---\n# Body\n", status))
				require.NoError(t, os.WriteFile(path, before, 0o644))
				cfg := &config.Config{TicketsDir: dir, Pipelines: map[string]config.PipelineDefinition{
					"test": {Legacy: config.Pipeline{{Stage: "code"}, {Stage: "review"}}},
				}}
				var err error
				if operation == "skip" {
					err = Skip(cfg, "abc")
				} else {
					err = SetStage(cfg, "abc", "review")
				}
				after, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				if status == "in_progress" {
					require.ErrorIs(t, err, app.ErrInvalidState)
					assert.ErrorContains(t, err, "pause first")
					assert.Equal(t, before, after)
					return
				}
				require.NoError(t, err)
				stored, err := ticket.ParseBytes(after)
				require.NoError(t, err)
				assert.Equal(t, "review", stored.Stage)
				assert.Contains(t, string(after), "custom_field: preserved")
				assert.Equal(t, "# Body\n", stored.Body)
				if operation == "set-stage" {
					assert.Equal(t, ticket.StatusPaused, stored.Status)
					assert.Equal(t, 3, stored.Attempt)
				} else {
					assert.Equal(t, ticket.StatusTodo, stored.Status)
					assert.Zero(t, stored.Attempt)
				}
			})
		}
	}
}

func TestRunSurfacesDaemonErrorWithoutReachabilityPrefix(t *testing.T) {
	ticketsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ticketsDir, "abc123.md"),
		[]byte("---\nid: abc123\nstatus: todo\nkontora: true\n---\n# Test\n"), 0o644))

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

	_, err = Run(&config.Config{
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
