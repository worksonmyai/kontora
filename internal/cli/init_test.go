package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestInit_LocalChildExecutionSettings(t *testing.T) {
	path := initTestRepo(t)
	other := initTestRepo(t)
	for _, tc := range []struct {
		name, mode, path, branch string
		missingParent            bool
		wantErr                  bool
	}{
		{name: "separate branch", mode: "agent", path: path, branch: "child-work"},
		{name: "cross repository", mode: "agent", path: other, wantErr: true},
		{name: "integration branch", mode: "agent", path: path, branch: "integration", wantErr: true},
		{name: "passive parent", path: other, branch: "integration"},
		{name: "deleted parent", path: other, missingParent: true},
	} {
		for _, status := range []string{"open", "todo"} {
			t.Run(tc.name+"/"+status, func(t *testing.T) {
				dir := t.TempDir()
				if !tc.missingParent {
					writeTicket(t, dir, "epic.md", fmt.Sprintf("---\nid: epic\nkind: epic\nepic_execution: %q\nstatus: paused\npath: %q\nbranch: integration\n---\n# Epic\n", tc.mode, path))
				}
				before := fmt.Sprintf("---\nid: child\nparent: epic\nstatus: open\npath: %q\nbranch: %q\n---\n# Child\n", path, tc.branch)
				writeTicket(t, dir, "child.md", before)
				cfg := updateTestConfig(dir)
				err := Enable(cfg, "child", EnableOpts{Path: tc.path, Pipeline: "none", Status: status}, io.Discard)
				if tc.wantErr {
					require.ErrorIs(t, err, app.ErrInvalidState)
					after, err := os.ReadFile(filepath.Join(dir, "child.md"))
					require.NoError(t, err)
					assert.Equal(t, before, string(after))
					return
				}
				require.NoError(t, err)
				got, err := ticket.ParseFile(filepath.Join(dir, "child.md"))
				require.NoError(t, err)
				assert.True(t, got.Kontora)
				assert.Equal(t, tc.path, got.Path)
				assert.Equal(t, ticket.Status(status), got.Status)
				assert.Equal(t, tc.branch, got.Branch)
				assert.Empty(t, got.Pipeline)
			})
		}
	}
}

type initRuntime struct {
	app.NoopRuntime
	enqueued []string
}

func (r *initRuntime) Enqueue(t *ticket.Ticket) { r.enqueued = append(r.enqueued, t.ID) }

func TestInit_EpicStartUsesDaemon(t *testing.T) {
	for _, tc := range []struct{ mode, status string }{
		{status: "todo"},
		{mode: "agent", status: "todo"},
		{mode: "agent"},
	} {
		t.Run(fmt.Sprintf("mode=%s/status=%s", tc.mode, tc.status), func(t *testing.T) {
			dir := t.TempDir()
			path := initTestRepo(t)
			cfg := &config.Config{TicketsDir: dir, AutoPickUp: new(false), DefaultAgent: "a", Agents: map[string]config.Agent{"a": {Binary: "true"}}}
			writeTicket(t, dir, "epic.md", fmt.Sprintf("---\nid: epic\nkind: epic\nepic_execution: %q\nstatus: open\nkontora: true\npath: %q\n---\n# Epic\n", tc.mode, path))
			runtime := &initRuntime{}
			svc := app.New(app.Static(cfg), store.NewDiskRepo(dir), runtime)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/tickets/epic/init", r.URL.Path)
				var req web.InitTicketRequest
				if err := json.NewDecoder(r.Body).Decode(&req); !assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, tc.status, req.Status)
				assert.Equal(t, path, req.Path)
				_, err := svc.Init("epic", app.InitRequest{Status: req.Status, Path: req.Path, Agent: req.Agent, Pipeline: req.Pipeline})
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusConflict)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"id": "epic", "status": "todo"})
			}))
			defer srv.Close()
			u, err := url.Parse(srv.URL)
			require.NoError(t, err)
			host, port, err := net.SplitHostPort(u.Host)
			require.NoError(t, err)
			cfg.Web.Host = host
			cfg.Web.Port, err = strconv.Atoi(port)
			require.NoError(t, err)
			var out bytes.Buffer
			require.NoError(t, Enable(cfg, "epi", EnableOpts{Status: tc.status}, &out))
			assert.Contains(t, out.String(), "initialized")
			assert.Equal(t, 1, calls)
			assert.Equal(t, []string{"epic"}, runtime.enqueued)
			got, err := ticket.ParseFile(filepath.Join(dir, "epic.md"))
			require.NoError(t, err)
			assert.True(t, got.IsAgentEpic())
			assert.Equal(t, ticket.StatusTodo, got.Status)
		})
	}
}
