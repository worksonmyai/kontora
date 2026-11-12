package remote

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/web"
)

type automationContract struct {
	name, method, path, body, response string
	status                             int
	call                               func(*Client) (any, error)
	want                               any
}

func automationContracts(t *testing.T) []automationContract {
	t.Helper()
	const id = "deleted/id ?#%"
	const revision = ^uint64(0)
	const n = ^uint64(0)
	path := "/api/automations/" + url.PathEscape(id)
	runPath := path + "/runs/18446744073709551615"
	spec := automation.Spec{
		Name: "Daily", Prompt: "first\nsecond", Path: "/repo with spaces", Agent: "agent",
		Model: "", Schedule: "0 9 * * *", Timezone: "Europe/Stockholm", Workspace: "repo",
		Overlap: "queue", Timeout: "1h", Enabled: false,
	}
	specJSON, err := json.Marshal(spec)
	require.NoError(t, err)
	definition := automation.Definition{ID: id, Spec: spec, Revision: revision, NextRuns: []time.Time{}}
	definitionJSON, err := json.Marshal(definition)
	require.NoError(t, err)
	run := automation.Run{AutomationID: id, N: n, Phase: "stopping", Result: "termination requested"}
	runJSON, err := json.Marshal(run)
	require.NoError(t, err)
	preview := AutomationPreviewResponse{Human: "Every day at 09:00", Occurrences: []time.Time{time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}}
	previewJSON, err := json.Marshal(preview)
	require.NoError(t, err)
	logs := AutomationLogsResponse{Lines: []automation.LogLine{{At: preview.Occurrences[0], Source: "agent", Kind: "text", Text: "first\nsecond"}}, Raw: "first\nsecond\r\n"}
	logsJSON, err := json.Marshal(logs)
	require.NoError(t, err)
	return []automationContract{
		{
			name: "list", method: "GET", path: "/api/automations", status: http.StatusOK,
			response: `{"available":true,"definitions":[]}`,
			call:     func(c *Client) (any, error) { return c.Automations() },
			want:     web.AutomationsResponse{Available: true, Definitions: []automation.Definition{}},
		},
		{
			name: "unavailable list", method: "GET", path: "/api/automations", status: http.StatusOK,
			response: `{"available":false,"definitions":[],"error":"automation storage unavailable"}`,
			call:     func(c *Client) (any, error) { return c.Automations() },
			want:     web.AutomationsResponse{Definitions: []automation.Definition{}, Error: "automation storage unavailable"},
		},
		{
			name: "definition", method: "GET", path: path, status: http.StatusOK, response: string(definitionJSON),
			call: func(c *Client) (any, error) { return c.Automation(id) }, want: definition,
		},
		{
			name: "create", method: "POST", path: "/api/automations", status: http.StatusCreated,
			body: `{"request_id":"stable-create-id","key":"daily","definition":` + string(specJSON) + `}`, response: string(definitionJSON),
			call: func(c *Client) (any, error) { return c.CreateAutomation("stable-create-id", "daily", spec) }, want: definition,
		},
		{
			name: "update", method: "PUT", path: path, status: http.StatusOK,
			body: `{"revision":18446744073709551615,"definition":` + string(specJSON) + `}`, response: string(definitionJSON),
			call: func(c *Client) (any, error) { return c.UpdateAutomation(id, revision, spec) }, want: definition,
		},
		{
			name: "delete", method: "DELETE", path: path, status: http.StatusNoContent,
			body: `{"revision":18446744073709551615}`,
			call: func(c *Client) (any, error) { return nil, c.DeleteAutomation(id, revision) },
		},
		{
			name: "pause", method: "POST", path: path + "/pause", status: http.StatusOK,
			body: `{"revision":18446744073709551615}`, response: string(definitionJSON),
			call: func(c *Client) (any, error) { return c.PauseAutomation(id, revision) }, want: definition,
		},
		{
			name: "resume", method: "POST", path: path + "/resume", status: http.StatusOK,
			body: `{"revision":18446744073709551615}`, response: string(definitionJSON),
			call: func(c *Client) (any, error) { return c.ResumeAutomation(id, revision) }, want: definition,
		},
		{
			name: "run", method: "POST", path: path + "/run", status: http.StatusAccepted,
			body: `{"request_id":"stable-run-id"}`, response: string(runJSON),
			call: func(c *Client) (any, error) { return c.RunAutomation(id, "stable-run-id") }, want: run,
		},
		{
			name: "stop", method: "POST", path: path + "/stop", status: http.StatusAccepted,
			body: `{"n":18446744073709551615}`, response: string(runJSON),
			call: func(c *Client) (any, error) { return c.StopAutomation(id, n) }, want: run,
		},
		{
			name: "history", method: "GET", path: path + "/runs?limit=17&offset=23", status: http.StatusOK,
			response: `{"runs":[` + string(runJSON) + `],"total":41}`,
			call:     func(c *Client) (any, error) { return c.AutomationRuns(id, 23, 17) },
			want:     web.AutomationRunsResponse{Runs: []automation.Run{run}, Total: 41},
		},
		{
			name: "empty history", method: "GET", path: path + "/runs?limit=20&offset=0", status: http.StatusOK,
			response: `{"runs":[],"total":0}`,
			call:     func(c *Client) (any, error) { return c.AutomationRuns(id, 0, 20) },
			want:     web.AutomationRunsResponse{Runs: []automation.Run{}, Total: 0},
		},
		{
			name: "preview", method: "POST", path: "/api/automations/preview", status: http.StatusOK,
			body: `{"schedule":"0 9 * * *","timezone":"Europe/Stockholm"}`, response: string(previewJSON),
			call: func(c *Client) (any, error) { return c.PreviewAutomation(spec.Schedule, spec.Timezone) }, want: preview,
		},
		{
			name: "logs", method: "GET", path: runPath + "/log", status: http.StatusOK, response: string(logsJSON),
			call: func(c *Client) (any, error) { return c.AutomationLogs(id, n) }, want: logs,
		},
		{
			name: "empty logs", method: "GET", path: runPath + "/log", status: http.StatusOK, response: `{"lines":[],"raw":""}`,
			call: func(c *Client) (any, error) { return c.AutomationLogs(id, n) }, want: AutomationLogsResponse{Lines: []automation.LogLine{}},
		},
		{
			name: "raw logs", method: "GET", path: runPath + "/log?raw=1", status: http.StatusOK, response: "\x00first\r\nsecond\n\n",
			call: func(c *Client) (any, error) { return c.RawAutomationLogs(id, n) }, want: []byte("\x00first\r\nsecond\n\n"),
		},
		{
			name: "terminal", method: "GET", path: runPath + "/terminal", status: http.StatusOK,
			response: `{"session":"session","window":"@42","identity":{"socket_path":"/tmp/tmux","window_id":"@42","session_id":"$1","server_pid":123,"server_started_at":456}}`,
			call:     func(c *Client) (any, error) { return c.AutomationTerminal(id, n) },
		},
	}
}

func TestAutomationClientContracts(t *testing.T) {
	for _, tc := range automationContracts(t) {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, tc.method, r.Method)
				assert.Equal(t, tc.path, r.URL.RequestURI())
				assert.Equal(t, "Bearer bearer-secret", r.Header.Get("Authorization"))
				assert.Equal(t, "operator-secret", r.Header.Get(web.OperatorTokenHeader))
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				if tc.body == "" {
					assert.Empty(t, body)
				} else {
					assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
					assert.JSONEq(t, tc.body, string(body))
					if strings.Contains(tc.body, "18446744073709551615") {
						assert.Contains(t, string(body), "18446744073709551615")
					}
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer srv.Close()
			c := New(srv.URL, "bearer-secret").WithOperatorToken("operator-secret")
			got, err := tc.call(c)
			require.NoError(t, err)
			assert.EqualValues(t, 1, requests.Load())
			if tc.name == "terminal" {
				encoded, err := json.Marshal(got)
				require.NoError(t, err)
				assert.JSONEq(t, tc.response, string(encoded))
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestAutomationClientErrorsDoNotRetry(t *testing.T) {
	for _, tc := range automationContracts(t) {
		t.Run(tc.name, func(t *testing.T) {
			for _, status := range []int{400, 401, 403, 404, 409, 503} {
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					assert.Equal(t, tc.path, r.URL.RequestURI())
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":"daemon rejected request"}`)
				}))
				_, err := tc.call(New(srv.URL, ""))
				assert.ErrorContains(t, err, "daemon rejected request")
				assert.True(t, isHTTPStatus(err, status))
				assert.EqualValues(t, 1, requests.Load())
				srv.Close()
			}
		})
	}
}

func TestAutomationClientNullableFields(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "null", true: "known zero"}[populated], func(t *testing.T) {
			value := "null"
			cost := "null"
			if populated {
				value = "0"
				cost = `"0"`
			}
			response := `{"id":"aut-example","success_rate":` + value + `,"avg_wall":` + value + `,"spend_30d":` + cost + `,"current_run":null,"last_run":{"cost":` + cost + `,"exit_code":` + value + `,"observed_exit_code":` + value + `},"next_runs":[]}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, response)
			}))
			defer srv.Close()
			row, err := New(srv.URL, "").Automation("aut-example")
			require.NoError(t, err)
			require.NotNil(t, row.LastRun)
			encoded, err := json.Marshal(row)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &fields))
			assert.Equal(t, value, string(fields["success_rate"]))
			assert.Equal(t, value, string(fields["avg_wall"]))
			assert.Equal(t, cost, string(fields["spend_30d"]))
			assert.Equal(t, "null", string(fields["current_run"]))
			assert.Equal(t, "[]", string(fields["next_runs"]))
			var lastRun map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(fields["last_run"], &lastRun))
			assert.Equal(t, cost, string(lastRun["cost"]))
			assert.Equal(t, value, string(lastRun["exit_code"]))
			assert.Equal(t, value, string(lastRun["observed_exit_code"]))
		})
	}
}

func TestAutomationDownloadsUseUnaryDeadline(t *testing.T) {
	oldTimeout := unaryRequestTimeout
	unaryRequestTimeout = 20 * time.Millisecond
	t.Cleanup(func() { unaryRequestTimeout = oldTimeout })
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "raw"}[raw], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			c := New(srv.URL, "")
			if raw {
				_, err := c.RawAutomationLogs("deleted", 7)
				require.Error(t, err)
			} else {
				_, err := c.AutomationLogs("deleted", 7)
				require.Error(t, err)
			}
		})
	}
}
