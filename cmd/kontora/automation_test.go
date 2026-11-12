package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/web"
)

const testAutomationID = "aut-0123456789abcdef0123456789abcdef"

func automationSpec() automation.Spec {
	return automation.Spec{Name: "Review", Prompt: "Review code", Path: "/repo", Agent: "agent", Model: "old-model", Schedule: "30m", Enabled: true}.Normalize()
}

func TestAutomationCommands(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		args                   []string
		method, path, response string
		body                   map[string]any
	}{
		{"list", []string{"ls"}, "GET", "/api/automations", `{"available":true,"definitions":[]}`, nil},
		{"view", []string{"view", testAutomationID}, "GET", "/api/automations/" + testAutomationID, `{"id":"` + testAutomationID + `","model":"x","success_rate":null}`, nil},
		{"preview", []string{"preview", "--schedule", "30m"}, "POST", "/api/automations/preview", `{"human":"Every 30 minutes","occurrences":[]}`, map[string]any{"schedule": "30m", "timezone": "UTC"}},
		{"history after deletion", []string{"runs", testAutomationID, "--offset", "5", "--limit", "3"}, "GET", "/api/automations/" + testAutomationID + "/runs?limit=3&offset=5", `{"runs":[],"total":9}`, nil},
		{"structured logs after deletion", []string{"logs", testAutomationID, "--run", "7"}, "GET", "/api/automations/" + testAutomationID + "/runs/7/log", `{"lines":[],"raw":""}`, nil},
		{"terminal after deletion", []string{"terminal", testAutomationID, "--run", "7"}, "GET", "/api/automations/" + testAutomationID + "/runs/7/terminal", `{"session":"kontora","window":"review","identity":{}}`, nil},
		{"run replay after deletion", []string{"run", testAutomationID, "--request-id", "original"}, "POST", "/api/automations/" + testAutomationID + "/run", `{"n":7,"phase":"queued","cost":null}`, map[string]any{"request_id": "original"}},
		{"exact stop acceptance", []string{"stop", testAutomationID, "--run", "7"}, "POST", "/api/automations/" + testAutomationID + "/stop", `{"n":7,"phase":"stopping","cost":null}`, map[string]any{"n": float64(7)}},
		{"delete confirmed", []string{"delete", testAutomationID, "--yes", "--revision", "5"}, "DELETE", "/api/automations/" + testAutomationID, "", map[string]any{"revision": float64(5)}},
		{"delete short confirmation", []string{"delete", testAutomationID, "-f", "--revision", "5"}, "DELETE", "/api/automations/" + testAutomationID, "", map[string]any{"revision": float64(5)}},
		{"pause", []string{"pause", testAutomationID, "--revision", "5"}, "POST", "/api/automations/" + testAutomationID + "/pause", `{"enabled":false}`, map[string]any{"revision": float64(5)}},
		{"resume", []string{"resume", testAutomationID, "--revision", "5"}, "POST", "/api/automations/" + testAutomationID + "/resume", `{"enabled":true}`, map[string]any{"revision": float64(5)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				assert.Equal(t, tc.method, r.Method)
				assert.Equal(t, tc.path, r.URL.RequestURI())
				assert.Equal(t, "Bearer bearer", r.Header.Get("Authorization"))
				assert.Equal(t, "operator", r.Header.Get(web.OperatorTokenHeader))
				if tc.body != nil {
					var body map[string]any
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					assert.Equal(t, tc.body, body)
				}
				if tc.method == "DELETE" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()
			args := append([]string{"automation"}, tc.args...)
			args = append(args, "--json", "--url", srv.URL, "--token", "bearer", "--operator-token", "operator")
			out, diagnostics, err := runCLISeparated(t, nil, nil, args...)
			require.NoError(t, err, diagnostics)
			var response map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &response), out)
			if tc.method == "DELETE" {
				assert.Equal(t, map[string]any{"id": testAutomationID, "deleted": true}, response)
			}
			if tc.name == "run replay after deletion" {
				assert.Contains(t, diagnostics, testAutomationID)
				assert.Contains(t, diagnostics, "original")
				assert.Nil(t, response["cost"])
			}
			mu.Lock()
			assert.Equal(t, 1, calls)
			mu.Unlock()
		})
	}
}

func TestAutomationArgumentValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"unknown"}, "unknown automation command"},
		{"missing reference", []string{"view"}, "reference is required"},
		{"excess reference", []string{"view", testAutomationID, "extra"}, "unexpected argument"},
		{"new positional", []string{"new", "extra"}, "unexpected argument"},
		{"new missing prompt", []string{"new"}, "--prompt-file is required"},
		{"no update", []string{"update", testAutomationID}, "at least one"},
		{"zero revision", []string{"pause", testAutomationID, "--revision", "0"}, "--revision must be positive"},
		{"unconfirmed delete", []string{"delete", testAutomationID}, "requires --yes or -f"},
		{"missing run", []string{"stop", testAutomationID}, "--run must be"},
		{"zero run", []string{"logs", testAutomationID, "--run", "0"}, "--run must be"},
		{"negative run", []string{"terminal", testAutomationID, "--run", "-1"}, "invalid value"},
		{"overflow run", []string{"logs", testAutomationID, "--run", "18446744073709551616"}, "invalid value"},
		{"zero attach run", []string{"attach", testAutomationID, "--run", "0"}, "--run must be"},
		{"raw json", []string{"logs", testAutomationID, "--run", "1", "--raw", "--json"}, "mutually exclusive"},
		{"negative offset", []string{"runs", testAutomationID, "--offset", "-1"}, "--offset must"},
		{"zero limit", []string{"runs", testAutomationID, "--limit", "0"}, "--limit must"},
		{"excess limit", []string{"runs", testAutomationID, "--limit", "201"}, "--limit must"},
		{"bad state", []string{"ls", "--state", "running"}, "--state must"},
		{"blank request", []string{"run", testAutomationID, "--request-id", " "}, "--request-id must"},
		{"long request", []string{"run", testAutomationID, "--request-id", strings.Repeat("a", 201)}, "--request-id must"},
		{"key retry", []string{"run", "review", "--request-id", "original"}, "original full automation ID"},
		{"missing preview schedule", []string{"preview"}, "--schedule is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("invalid command contacted daemon")
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			args := append([]string{"automation"}, tc.args...)
			args = append(args, "--url", srv.URL)
			out, diagnostics, err := runCLISeparated(t, nil, nil, args...)
			require.Error(t, err)
			assert.Empty(t, out)
			assert.Contains(t, diagnostics, tc.want)
		})
	}
}

func TestAutomationOutput(t *testing.T) {
	for _, tc := range []struct {
		name           string
		args           []string
		response, want string
		fail           bool
		status         int
	}{
		{"empty json", []string{"ls", "--json"}, `{"available":true,"definitions":[]}`, `"definitions":[]`, false, http.StatusOK},
		{"unavailable json", []string{"ls", "--json"}, `{"available":false,"definitions":[],"error":"disk unavailable"}`, `"available":false`, true, http.StatusOK},
		{"unavailable human", []string{"ls"}, `{"available":false,"definitions":[],"error":"disk unavailable"}`, "", true, http.StatusOK},
		{"history unavailable json", []string{"runs", testAutomationID, "--json"}, `{"error":"disk unavailable"}`, "", true, http.StatusServiceUnavailable},
		{"null costs", []string{"runs", testAutomationID, "--json"}, `{"runs":[{"n":1,"cost":null,"exit_code":null}],"total":1}`, `"cost":null`, false, http.StatusOK},
		{"raw text", []string{"logs", testAutomationID, "--run", "2", "--raw"}, "first\r\n\x1b[31mtext\x1b[0m\nno final newline", "first\r\n\x1b[31mtext\x1b[0m\nno final newline", false, http.StatusOK},
		{"stopping human", []string{"stop", testAutomationID, "--run", "2"}, `{"n":2,"phase":"stopping","outcome":"","result":"waiting"}`, "stopping", false, http.StatusOK},
		{"skipped human", []string{"run", testAutomationID}, `{"n":2,"phase":"finished","outcome":"skipped","result":"overlap"}`, "skipped", false, http.StatusOK},
		{"recovery human", []string{"stop", testAutomationID, "--run", "2"}, `{"n":2,"phase":"recovery_required","result":"identity unknown"}`, "recovery_required", false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()
			args := append([]string{"automation"}, tc.args...)
			args = append(args, "--url", srv.URL)
			out, diagnostics, err := runCLISeparated(t, nil, nil, args...)
			if tc.fail {
				require.Error(t, err)
				assert.Contains(t, diagnostics, "disk unavailable")
			} else {
				require.NoError(t, err, diagnostics)
			}
			assert.Contains(t, out, tc.want)
			if tc.status != http.StatusOK {
				assert.Empty(t, out)
			}
			if tc.name == "raw text" {
				assert.Equal(t, tc.response, out)
				assert.Empty(t, diagnostics)
			}
			if tc.name == "unavailable json" || tc.name == "empty json" {
				assert.True(t, json.Valid([]byte(out)), out)
			}
		})
	}
}

func TestAutomationDefinitionMutations(t *testing.T) {
	for _, tc := range []struct {
		name, sub string
		flags     []string
		conflict  bool
	}{
		{"partial update", "update", []string{"--enabled=false", "--model="}, false},
		{"rename preserves values", "update", []string{"--name", "Renamed"}, false},
		{"stale update", "update", []string{"--enabled=false", "--model=", "--revision", "2"}, true},
		{"duplicate defaults", "duplicate", []string{"--request-id", "copy"}, false},
		{"duplicate overrides", "duplicate", []string{"--request-id", "copy", "--name", "Other", "--key", "other", "--enabled"}, false},
		{"delete fetched revision", "delete", []string{"--yes"}, false},
		{"pause fetched revision", "pause", nil, false},
		{"resume fetched revision", "resume", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := automationSpec()
			spec.Timeout = "1h"
			spec.Timezone = "Europe/Berlin"
			spec.Workspace = "repo"
			spec.Overlap = "queue"
			current := automation.Definition{ID: testAutomationID, Key: "review", Revision: 5, Spec: spec}
			var mu sync.Mutex
			gets, writes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodGet {
					gets++
					_ = json.NewEncoder(w).Encode(current)
					return
				}
				writes++
				var body struct {
					Revision   uint64          `json:"revision"`
					Definition automation.Spec `json:"definition"`
					Key        string          `json:"key"`
					RequestID  string          `json:"request_id"`
				}
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if tc.sub == "duplicate" {
					assert.Equal(t, "copy", body.RequestID)
					want := current.Spec
					want.Enabled = tc.name == "duplicate overrides"
					want.Name += " (copy)"
					if tc.name == "duplicate overrides" {
						want.Name = "Other"
						assert.Equal(t, "other", body.Key)
					} else {
						assert.Empty(t, body.Key)
					}
					assert.Equal(t, want, body.Definition)
				} else {
					wantRevision := uint64(5)
					if tc.conflict {
						wantRevision = 2
					}
					assert.Equal(t, wantRevision, body.Revision)
					if tc.sub == "update" {
						want := current.Spec
						if tc.name == "rename preserves values" {
							want.Name = "Renamed"
						} else {
							want.Enabled = false
							want.Model = ""
						}
						assert.Equal(t, want, body.Definition)
					}
				}
				if tc.conflict {
					w.WriteHeader(http.StatusConflict)
					_, _ = fmt.Fprint(w, `{"error":"stale revision"}`)
					return
				}
				if tc.sub == "delete" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				_ = json.NewEncoder(w).Encode(current)
			}))
			defer srv.Close()
			args := []string{"automation", tc.sub, testAutomationID, "--url", srv.URL, "--json"}
			args = append(args, tc.flags...)
			out, diagnostics, err := runCLISeparated(t, nil, nil, args...)
			if tc.conflict {
				require.Error(t, err)
				assert.Empty(t, out)
				assert.Contains(t, diagnostics, "stale revision")
			} else {
				require.NoError(t, err, diagnostics)
				assert.True(t, json.Valid([]byte(out)), out)
			}
			mu.Lock()
			assert.Equal(t, 1, gets)
			assert.Equal(t, 1, writes)
			mu.Unlock()
		})
	}
}

func TestAutomationNewStdin(t *testing.T) {
	for _, requestID := range []string{"", "stable-request"} {
		t.Run("request "+requestID, func(t *testing.T) {
			requestIDs := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/automations", r.URL.Path)
				var body struct {
					RequestID  string          `json:"request_id"`
					Definition automation.Spec `json:"definition"`
				}
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				requestIDs <- body.RequestID
				assert.Equal(t, "prompt from stdin\n", body.Definition.Prompt)
				assert.False(t, body.Definition.Enabled)
				_ = json.NewEncoder(w).Encode(automation.Definition{ID: testAutomationID, Spec: body.Definition})
			}))
			defer srv.Close()
			args := []string{"automation", "new", "--url", srv.URL, "--name", "Review", "--prompt-file", "-", "--path", "/repo", "--agent", "agent", "--schedule", "30m", "--json"}
			if requestID != "" {
				args = append(args, "--request-id", requestID)
			}
			out, diagnostics, err := runCLISeparated(t, nil, strings.NewReader("prompt from stdin\n"), args...)
			require.NoError(t, err, diagnostics)
			assert.True(t, json.Valid([]byte(out)), out)
			gotID := <-requestIDs
			assert.NotEmpty(t, gotID)
			assert.Contains(t, diagnostics, gotID)
			if requestID != "" {
				assert.Equal(t, requestID, gotID)
			}
		})
	}
}

func TestAutomationAttachmentRouting(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		local, exact bool
		ticketID     string
	}{
		{"top-level remote", []string{"attach", testAutomationID}, false, false, ""},
		{"top-level local", []string{"attach", testAutomationID}, true, false, ""},
		{"automation current", []string{"automation", "attach", testAutomationID}, false, false, ""},
		{"automation exact deleted", []string{"automation", "attach", testAutomationID, "--run", "7"}, false, true, ""},
		{"ticket remote", []string{"attach", "kon-1234"}, false, false, "kon-1234"},
		{"ticket automation-like ID", []string{"attach", "aut-1234"}, false, false, "aut-1234"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			gets, handshakes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "operator", r.Header.Get(web.OperatorTokenHeader))
				assert.Equal(t, "Bearer bearer", r.Header.Get("Authorization"))
				if strings.HasPrefix(r.URL.Path, "/api/") {
					mu.Lock()
					gets++
					mu.Unlock()
					assert.False(t, tc.exact, "exact run must skip live definition")
					if tc.ticketID != "" {
						row := web.TicketInfo{ID: tc.ticketID, Status: "in_progress"}
						if r.URL.Path == "/api/tickets" {
							assert.Equal(t, "true", r.URL.Query().Get("all"))
							_ = json.NewEncoder(w).Encode(map[string]any{"tickets": []web.TicketInfo{row}})
						} else {
							assert.Equal(t, "/api/tickets/"+tc.ticketID, r.URL.Path)
							_ = json.NewEncoder(w).Encode(row)
						}
						return
					}
					_ = json.NewEncoder(w).Encode(automation.Definition{ID: testAutomationID, Running: true, CurrentRun: &automation.Run{N: 7}})
					return
				}
				mu.Lock()
				handshakes++
				mu.Unlock()
				if tc.ticketID != "" {
					assert.Equal(t, "/ws/terminal/"+tc.ticketID, r.URL.Path)
				} else {
					assert.Equal(t, "/ws/automations/"+testAutomationID+"/7", r.URL.Path)
				}
				assert.NotEqual(t, "1", r.URL.Query().Get("rw"))
				conn, err := websocket.Accept(w, r, nil)
				if !assert.NoError(t, err) {
					return
				}
				_ = conn.Close(websocket.StatusNormalClosure, "")
			}))
			defer srv.Close()
			args := append([]string{}, tc.args...)
			if tc.local {
				cfg := filepath.Join(t.TempDir(), "config.yaml")
				address := strings.TrimPrefix(srv.URL, "http://")
				host, port, _ := strings.Cut(address, ":")
				require.NoError(t, os.WriteFile(cfg, fmt.Appendf(nil, "agents:\n  claude:\n    binary: true\nweb:\n  host: %s\n  port: %s\n  token: bearer\n", host, port), 0600))
				args = append(args, "--config", cfg)
			} else {
				args = append(args, "--url", srv.URL, "--token", "bearer")
			}
			for _, source := range []string{"environment", "flag"} {
				t.Run(source, func(t *testing.T) {
					callArgs := append([]string{}, args...)
					env := []string{"KONTORA_OPERATOR_TOKEN=operator"}
					if source == "flag" {
						callArgs = append(callArgs, "--operator-token", "operator")
						env = []string{"KONTORA_OPERATOR_TOKEN=ignored"}
					}
					_, diagnostics, err := runCLISeparated(t, env, nil, callArgs...)
					require.NoError(t, err, diagnostics)
					mu.Lock()
					defer mu.Unlock()
					if tc.exact {
						assert.Zero(t, gets)
					} else if tc.ticketID != "" {
						assert.Equal(t, 2, gets)
					} else {
						assert.Equal(t, 1, gets)
					}
					assert.Equal(t, 1, handshakes)
					gets, handshakes = 0, 0
				})
			}
		})
	}
}

func TestAutomationUnreachableLocalDaemon(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("agents:\n  claude:\n    binary: true\nweb:\n  host: 127.0.0.1\n  port: 1\n")
	require.NoError(t, os.WriteFile(cfg, original, 0600))
	out, diagnostics, err := runCLISeparated(t, nil, nil, "automation", "ls", "--config", cfg, "--json")
	require.Error(t, err)
	assert.Empty(t, out)
	assert.Contains(t, diagnostics, "connect")
	data, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.Equal(t, original, data)
}
