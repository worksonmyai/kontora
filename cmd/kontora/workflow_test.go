package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestWorkflowCommandsUseDaemon(t *testing.T) {
	for _, mode := range []string{"remote", "local"} {
		for _, action := range []string{"restart", "recover"} {
			t.Run(mode+"/"+action, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet && r.URL.Path == "/api/tickets" {
						_, _ = fmt.Fprint(w, `{"tickets":[{"id":"graph-123","title":"Graph"}]}`)
						return
					}
					if r.Method == http.MethodGet {
						w.WriteHeader(http.StatusNotFound)
						_, _ = fmt.Fprint(w, `{"error":"not found"}`)
						return
					}
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "/api/tickets/graph-123/workflow-"+action, r.URL.Path)
					if action == "recover" {
						var body struct {
							ExecutionID string `json:"execution_id"`
							Revision    uint64 `json:"revision"`
						}
						assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
						assert.Equal(t, "execution", body.ExecutionID)
						assert.Equal(t, uint64(18446744073709551615), body.Revision)
					}
					_, _ = fmt.Fprint(w, `{}`)
				}))
				defer srv.Close()
				args := []string{"workflow-" + action, "graph-"}
				if action == "recover" {
					args = append(args, "--execution-id", "execution", "--revision", "18446744073709551615")
				}
				if mode == "remote" {
					args = append(args, "--url", srv.URL)
				} else {
					cfg := filepath.Join(t.TempDir(), "config.yaml")
					// No ticket files exist locally. Even prefix resolution must
					// use the daemon's store rather than guessing from local files.
					content := fmt.Sprintf("tickets_dir: %q\nagents:\n  claude:\n    binary: true\nweb:\n  host: 127.0.0.1\n  port: %d\n", t.TempDir(), srv.Listener.Addr().(*net.TCPAddr).Port)
					require.NoError(t, os.WriteFile(cfg, []byte(content), 0o600))
					args = append(args, "--config", cfg)
				}
				out, err := runCLI(t, nil, args...)
				require.NoError(t, err, out)
				assert.Contains(t, out, "graph-123")
				if action == "restart" {
					assert.Contains(t, out, "workflow restarted")
				} else {
					assert.Contains(t, out, "workflow recovery acknowledged")
				}
			})
		}
	}
}

func TestWorkflowCommandValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"workflow-recover", "graph-1"}, "requires --execution-id and --revision"},
		{[]string{"workflow-recover", "graph-1", "--execution-id", "execution"}, "requires --execution-id and --revision"},
		{[]string{"workflow-recover", "graph-1", "--execution-id", "execution", "--revision", "-1"}, "invalid workflow revision"},
		{[]string{"workflow-restart", "graph-1", "extra"}, "unexpected argument"},
		{[]string{"workflow-restart", "--tickets-dir", "/tmp/unused", "graph-1"}, "flag provided but not defined"},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			out, err := runCLI(t, nil, tc.args...)
			require.Error(t, err)
			assert.Contains(t, out, tc.want)
		})
	}
}

func TestWorkflowRemoteDisplay(t *testing.T) {
	var out bytes.Buffer
	printRemoteTicket(&out, web.TicketInfo{ID: "graph-1", Stage: "write", Pipeline: "graph",
		GraphDefinition: &app.GraphDefinitionView{Version: 2, Entry: "write"},
		GraphExecution: &app.GraphExecutionView{ExecutionID: "execution", Revision: 8, CurrentNode: "write", CurrentTemplate: "code", Visit: 4, VisitID: "execution.4",
			Loops: []app.GraphLoopView{{NodeID: "budget", Used: 3, Max: 3, Exhausted: true}}},
	})
	for _, want := range []string{"node:      write", "template:  code", "execution: execution (revision 8)", "budget 3/3 (exhausted)"} {
		assert.Contains(t, out.String(), want)
	}
	assert.NotContains(t, out.String(), "Stage:")
	out.Reset()
	printRemoteConfig(&out, web.ConfigInfo{PipelineInfos: []web.PipelineInfo{{Name: "graph", GraphDefinition: &app.GraphDefinitionView{Entry: "write", Nodes: []app.GraphNodeView{{ID: "write"}}}}}})
	assert.Contains(t, out.String(), "graph · entry write · 1 nodes")
}
