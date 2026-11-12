package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/web"
)

const automationTestID = "aut-0123456789abcdef0123456789abcdef"

func automationTestSpec() automation.Spec {
	return automation.Spec{
		Name: "Review", Prompt: "Review the code\nReport findings", Path: "/repo", Agent: "agent",
		Model: "model", Schedule: "0 9 * * 1-5", Timezone: "Europe/Berlin",
		Workspace: "repo", Overlap: "queue", Timeout: "1h", Enabled: true,
	}
}

func TestNewAutomationRequest(t *testing.T) {
	for _, tc := range []struct {
		name, key, requestID string
		change               func(*automation.Spec)
		wantError            bool
	}{
		{name: "generated identity", key: "review"},
		{name: "supplied identity", key: "review", requestID: "retry-original"},
		{name: "server derives key", requestID: "derive"},
		{name: "defaults", change: func(s *automation.Spec) {
			s.Timezone, s.Workspace, s.Overlap, s.Timeout, s.Enabled = "", "", "", "", false
		}},
		{name: "valid interval", change: func(s *automation.Spec) { s.Schedule = "30m" }},
		{name: "invalid schedule", change: func(s *automation.Spec) { s.Schedule = "bad" }, wantError: true},
		{name: "invalid prompt", change: func(s *automation.Spec) { s.Prompt = "" }, wantError: true},
		{name: "invalid key", key: "Bad_key", wantError: true},
		{name: "blank request identity", requestID: "  ", wantError: true},
		{name: "long request identity", requestID: strings.Repeat("x", 201), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := automationTestSpec()
			if tc.change != nil {
				tc.change(&spec)
			}
			var diagnostic bytes.Buffer
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/api/automations", r.URL.Path)
				var body struct {
					RequestID  string          `json:"request_id"`
					Key        string          `json:"key"`
					Definition automation.Spec `json:"definition"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if tc.requestID == "" {
					require.Len(t, body.RequestID, 32)
				} else {
					require.Equal(t, tc.requestID, body.RequestID)
				}
				require.Equal(t, "request_id: "+body.RequestID+"\n", diagnostic.String())
				require.Equal(t, tc.key, body.Key)
				require.Equal(t, spec.Normalize(), body.Definition)
				w.WriteHeader(http.StatusCreated)
				require.NoError(t, json.NewEncoder(w).Encode(automation.Definition{ID: automationTestID, Key: body.Key, Spec: body.Definition}))
			}))
			defer server.Close()
			row, err := NewAutomation(remote.New(server.URL, ""), tc.key, tc.requestID, spec, &diagnostic)
			if tc.wantError {
				require.ErrorIs(t, err, automation.ErrValidation)
				require.Zero(t, calls)
				require.Empty(t, diagnostic.String())
			} else {
				require.NoError(t, err)
				require.Equal(t, automationTestID, row.ID)
				require.Equal(t, spec.Normalize(), row.Spec)
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestNewAutomationRequestFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		diagnostic io.Writer
		wantCalls  int
	}{
		{name: "conflict does not retry", diagnostic: &bytes.Buffer{}, wantCalls: 1},
		{name: "diagnostic failure prevents mutation", diagnostic: automationFailWriter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"request conflict"}`)
			}))
			defer server.Close()
			_, err := NewAutomation(remote.New(server.URL, ""), "review", "retry", automationTestSpec(), tc.diagnostic)
			require.Error(t, err)
			require.Equal(t, tc.wantCalls, calls)
			if b, ok := tc.diagnostic.(*bytes.Buffer); ok {
				require.Equal(t, "request_id: retry\n", b.String())
				require.ErrorContains(t, err, "request conflict")
			}
		})
	}
}

func TestResolveAutomation(t *testing.T) {
	rows := []automation.Definition{
		{ID: automationTestID, Key: "review", Name: "Display name"},
		{ID: "aut-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Key: "aut-1234"},
	}
	for _, tc := range []struct {
		name, ref string
		available bool
		message   string
		status    int
		wantID    string
		wantError error
		wantCalls int
	}{
		{name: "deleted full ID bypasses unavailable list", ref: automationTestID, wantID: automationTestID},
		{name: "exact active key", ref: "review", available: true, wantID: automationTestID, wantCalls: 1},
		{name: "short aut prefix is a key", ref: "aut-1234", available: true, wantID: rows[1].ID, wantCalls: 1},
		{name: "not display name", ref: "Display name", available: true, wantError: automation.ErrNotFound, wantCalls: 1},
		{name: "not a prefix", ref: "rev", available: true, wantError: automation.ErrNotFound, wantCalls: 1},
		{name: "not case insensitive", ref: "Review", available: true, wantError: automation.ErrNotFound, wantCalls: 1},
		{name: "not ticket resolution", ref: "tst-1234", available: true, wantError: automation.ErrNotFound, wantCalls: 1},
		{name: "unavailable despite matching row", ref: "review", message: "lock is held", wantError: automation.ErrUnavailable, wantCalls: 1},
		{name: "unavailable without explanation", ref: "review", wantError: automation.ErrUnavailable, wantCalls: 1},
		{name: "HTTP failure", ref: "review", status: http.StatusUnauthorized, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/api/automations", r.URL.Path)
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
					return
				}
				require.NoError(t, json.NewEncoder(w).Encode(web.AutomationsResponse{Available: tc.available, Definitions: rows, Error: tc.message}))
			}))
			defer server.Close()
			id, err := ResolveAutomation(remote.New(server.URL, ""), tc.ref)
			if tc.wantError != nil {
				require.ErrorIs(t, err, tc.wantError)
				if tc.message != "" {
					require.ErrorContains(t, err, tc.message)
				}
			} else if tc.status != 0 {
				require.ErrorContains(t, err, "unauthorized")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantID, id)
			require.Equal(t, tc.wantCalls, calls)
		})
	}
}

func TestUpdateAutomation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		changes  AutomationUpdate
		apply    func(*automation.Spec)
		invalid  bool
		noChange bool
	}{
		{name: "explicit false and empty model", changes: AutomationUpdate{Enabled: new(false), Model: new("")}, apply: func(s *automation.Spec) { s.Enabled, s.Model = false, "" }},
		{name: "name", changes: AutomationUpdate{Name: new("Renamed")}, apply: func(s *automation.Spec) { s.Name = "Renamed" }},
		{name: "prompt", changes: AutomationUpdate{Prompt: new("New\nprompt")}, apply: func(s *automation.Spec) { s.Prompt = "New\nprompt" }},
		{name: "path", changes: AutomationUpdate{Path: new("/other")}, apply: func(s *automation.Spec) { s.Path = "/other" }},
		{name: "agent", changes: AutomationUpdate{Agent: new("other")}, apply: func(s *automation.Spec) { s.Agent = "other" }},
		{name: "schedule", changes: AutomationUpdate{Schedule: new("30m")}, apply: func(s *automation.Spec) { s.Schedule = "30m" }},
		{name: "timezone", changes: AutomationUpdate{Timezone: new("UTC")}, apply: func(s *automation.Spec) { s.Timezone = "UTC" }},
		{name: "workspace", changes: AutomationUpdate{Workspace: new("worktree")}, apply: func(s *automation.Spec) { s.Workspace = "worktree" }},
		{name: "overlap", changes: AutomationUpdate{Overlap: new("replace")}, apply: func(s *automation.Spec) { s.Overlap = "replace" }},
		{name: "timeout", changes: AutomationUpdate{Timeout: new("45m")}, apply: func(s *automation.Spec) { s.Timeout = "45m" }},
		{name: "explicit default fields", changes: AutomationUpdate{Timezone: new(""), Workspace: new(""), Overlap: new(""), Timeout: new("")}, apply: func(s *automation.Spec) { s.Timezone, s.Workspace, s.Overlap, s.Timeout = "", "", "", "" }},
		{name: "explicit unchanged field still submitted", changes: AutomationUpdate{Enabled: new(true)}},
		{name: "empty required name", changes: AutomationUpdate{Name: new("")}, invalid: true},
		{name: "empty required prompt", changes: AutomationUpdate{Prompt: new("")}, invalid: true},
		{name: "empty required path", changes: AutomationUpdate{Path: new("")}, invalid: true},
		{name: "empty required agent", changes: AutomationUpdate{Agent: new("")}, invalid: true},
		{name: "empty required schedule", changes: AutomationUpdate{Schedule: new("")}, invalid: true},
		{name: "invalid schedule", changes: AutomationUpdate{Schedule: new("30s")}, invalid: true},
		{name: "invalid timezone", changes: AutomationUpdate{Timezone: new("Local")}, invalid: true},
		{name: "invalid workspace", changes: AutomationUpdate{Workspace: new("bad")}, invalid: true},
		{name: "invalid overlap", changes: AutomationUpdate{Overlap: new("bad")}, invalid: true},
		{name: "invalid timeout", changes: AutomationUpdate{Timeout: new("0s")}, invalid: true},
		{name: "no supplied fields", noChange: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := automationTestSpec()
			want := spec
			if tc.apply != nil {
				tc.apply(&want)
			}
			want = want.Normalize()
			gets, puts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/automations/"+automationTestID, r.URL.Path)
				switch r.Method {
				case http.MethodGet:
					gets++
					require.NoError(t, json.NewEncoder(w).Encode(automation.Definition{ID: automationTestID, Key: "review", Revision: 7, Spec: spec}))
				case http.MethodPut:
					puts++
					var body struct {
						Revision   uint64          `json:"revision"`
						Definition automation.Spec `json:"definition"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, uint64(7), body.Revision)
					require.Equal(t, want, body.Definition)
					require.NoError(t, json.NewEncoder(w).Encode(automation.Definition{ID: automationTestID, Revision: 8, Spec: body.Definition}))
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
			}))
			defer server.Close()
			row, err := UpdateAutomation(remote.New(server.URL, ""), automationTestID, 0, tc.changes)
			switch {
			case tc.noChange:
				require.ErrorIs(t, err, automation.ErrValidation)
				require.Zero(t, gets)
				require.Zero(t, puts)
			case tc.invalid:
				require.ErrorIs(t, err, automation.ErrValidation)
				require.Equal(t, 1, gets)
				require.Zero(t, puts)
			default:
				require.NoError(t, err)
				require.Equal(t, want, row.Spec)
				require.Equal(t, uint64(8), row.Revision)
				require.Equal(t, 1, gets)
				require.Equal(t, 1, puts)
			}
		})
	}
}

func TestUpdateAutomationRevision(t *testing.T) {
	for _, revision := range []uint64{0, 3, 99} {
		t.Run(strconv.FormatUint(revision, 10), func(t *testing.T) {
			gets, puts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets++
					require.NoError(t, json.NewEncoder(w).Encode(automation.Definition{Revision: 7, Spec: automationTestSpec()}))
					return
				}
				puts++
				require.Equal(t, http.MethodPut, r.Method)
				var body struct {
					Revision uint64 `json:"revision"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				want := revision
				if want == 0 {
					want = 7
				}
				require.Equal(t, want, body.Revision)
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"stale revision"}`)
			}))
			defer server.Close()
			_, err := UpdateAutomation(remote.New(server.URL, ""), automationTestID, revision, AutomationUpdate{Name: new("Changed")})
			require.ErrorContains(t, err, "stale revision")
			require.Equal(t, 1, gets)
			require.Equal(t, 1, puts)
		})
	}
}

func TestAutomationMutationReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*remote.Client) error
	}{
		{"update", func(c *remote.Client) error {
			_, err := UpdateAutomation(c, automationTestID, 0, AutomationUpdate{Name: new("New")})
			return err
		}},
		{"delete", func(c *remote.Client) error { return DeleteAutomation(c, automationTestID, 0) }},
		{"pause", func(c *remote.Client) error { _, err := PauseAutomation(c, automationTestID, 0); return err }},
		{"resume", func(c *remote.Client) error { _, err := ResumeAutomation(c, automationTestID, 0); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, http.MethodGet, r.Method)
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":"deleted"}`)
			}))
			defer server.Close()
			require.ErrorContains(t, tc.call(remote.New(server.URL, "")), "deleted")
			require.Equal(t, 1, calls)
		})
	}
}

func TestAutomationRevisionMutations(t *testing.T) {
	for _, tc := range []struct {
		name, method, suffix string
		call                 func(*remote.Client, uint64) error
	}{
		{"delete", http.MethodDelete, "", func(c *remote.Client, rev uint64) error { return DeleteAutomation(c, automationTestID, rev) }},
		{"pause", http.MethodPost, "/pause", func(c *remote.Client, rev uint64) error {
			_, err := PauseAutomation(c, automationTestID, rev)
			return err
		}},
		{"resume", http.MethodPost, "/resume", func(c *remote.Client, rev uint64) error {
			_, err := ResumeAutomation(c, automationTestID, rev)
			return err
		}},
	} {
		for _, revision := range []uint64{0, 3} {
			for _, conflict := range []bool{false, true} {
				t.Run(tc.name+"/"+strconv.FormatUint(revision, 10)+"/"+strconv.FormatBool(conflict), func(t *testing.T) {
					gets, mutations := 0, 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet {
							gets++
							require.Equal(t, "/api/automations/"+automationTestID, r.URL.Path)
							require.NoError(t, json.NewEncoder(w).Encode(automation.Definition{Revision: 7}))
							return
						}
						mutations++
						require.Equal(t, tc.method, r.Method)
						require.Equal(t, "/api/automations/"+automationTestID+tc.suffix, r.URL.Path)
						var body struct {
							Revision uint64 `json:"revision"`
						}
						require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
						want := revision
						if want == 0 {
							want = 7
						}
						require.Equal(t, want, body.Revision)
						if conflict {
							w.WriteHeader(http.StatusConflict)
							_, _ = io.WriteString(w, `{"error":"stale revision"}`)
						} else if tc.name == "delete" {
							w.WriteHeader(http.StatusNoContent)
						} else {
							require.NoError(t, json.NewEncoder(w).Encode(automation.Definition{Revision: 8}))
						}
					}))
					defer server.Close()
					err := tc.call(remote.New(server.URL, ""), revision)
					if conflict {
						require.ErrorContains(t, err, "stale revision")
					} else {
						require.NoError(t, err)
					}
					if revision == 0 {
						require.Equal(t, 1, gets)
					} else {
						require.Zero(t, gets)
					}
					require.Equal(t, 1, mutations)
				})
			}
		}
	}
}

func TestRunAutomationRequest(t *testing.T) {
	for _, tc := range []struct {
		name, ref, requestID string
		conflict             bool
		invalid              bool
		wantList             int
	}{
		{name: "generated identity by key", ref: "review", wantList: 1},
		{name: "generated identity by ID", ref: automationTestID},
		{name: "replay deleted full ID", ref: automationTestID, requestID: "original"},
		{name: "no conflict retry", ref: automationTestID, requestID: "original", conflict: true},
		{name: "recreated key cannot replay", ref: "review", requestID: "original", invalid: true},
		{name: "short ID cannot replay", ref: "aut-0123", requestID: "original", invalid: true},
		{name: "blank request identity", ref: automationTestID, requestID: "  ", invalid: true},
		{name: "long request identity", ref: automationTestID, requestID: strings.Repeat("x", 201), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lists, runs := 0, 0
			var diagnostic bytes.Buffer
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/automations" {
					lists++
					require.Equal(t, http.MethodGet, r.Method)
					require.NoError(t, json.NewEncoder(w).Encode(web.AutomationsResponse{Available: true, Definitions: []automation.Definition{{ID: automationTestID, Key: "review"}}}))
					return
				}
				runs++
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/api/automations/"+automationTestID+"/run", r.URL.Path)
				var body struct {
					RequestID string `json:"request_id"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if tc.requestID != "" {
					require.Equal(t, tc.requestID, body.RequestID)
				} else {
					require.Len(t, body.RequestID, 32)
				}
				require.Equal(t, "automation_id: "+automationTestID+"\nrequest_id: "+body.RequestID+"\n", diagnostic.String())
				if tc.conflict {
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, `{"error":"request conflict"}`)
					return
				}
				w.WriteHeader(http.StatusAccepted)
				require.NoError(t, json.NewEncoder(w).Encode(automation.Run{AutomationID: automationTestID, N: 12, Phase: "queued"}))
			}))
			defer server.Close()
			run, err := RunAutomationRequest(remote.New(server.URL, ""), tc.ref, tc.requestID, &diagnostic)
			switch {
			case tc.invalid:
				require.ErrorIs(t, err, automation.ErrValidation)
				require.Zero(t, runs)
				require.Empty(t, diagnostic.String())
			case tc.conflict:
				require.ErrorContains(t, err, "request conflict")
				require.Equal(t, 1, runs)
			default:
				require.NoError(t, err)
				require.Equal(t, "queued", run.Phase)
				require.Equal(t, uint64(12), run.N)
				require.Equal(t, 1, runs)
			}
			require.Equal(t, tc.wantList, lists)
		})
	}
}

func TestFilterAutomations(t *testing.T) {
	rows := []automation.Definition{
		{ID: automationTestID, Key: "zulu", Project: "Backend", ScheduleHuman: "every hour", Name: "Review", Path: "/src/backend", Agent: "Claude", Schedule: "1h", Enabled: true, Prompt: "secret-prompt", Model: "hidden-model", Timezone: "hidden-zone"},
		{ID: "aut-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Key: "alpha", Project: "Frontend", Name: "Lint", Agent: "Codex"},
		{ID: "aut-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Key: "middle", Project: "Backend", Name: "Test", Agent: "Claude", Enabled: true},
	}
	for _, tc := range []struct {
		name   string
		filter AutomationFilter
		keys   []string
	}{
		{"all sorted", AutomationFilter{}, []string{"alpha", "middle", "zulu"}},
		{"explicit all", AutomationFilter{State: "all"}, []string{"alpha", "middle", "zulu"}},
		{"enabled", AutomationFilter{State: "enabled"}, []string{"middle", "zulu"}},
		{"paused", AutomationFilter{State: "paused"}, []string{"alpha"}},
		{"project exact", AutomationFilter{Project: "Backend"}, []string{"middle", "zulu"}},
		{"project case matters", AutomationFilter{Project: "backend"}, []string{}},
		{"agent exact", AutomationFilter{Agent: "Claude"}, []string{"middle", "zulu"}},
		{"agent not prefix", AutomationFilter{Agent: "Clau"}, []string{}},
		{"combined", AutomationFilter{State: "enabled", Project: "Backend", Agent: "Claude", Query: "REVIEW"}, []string{"zulu"}},
		{"query ID", AutomationFilter{Query: "0123456789ABCDEF"}, []string{"zulu"}},
		{"query key", AutomationFilter{Query: "ZUL"}, []string{"zulu"}},
		{"query path", AutomationFilter{Query: "/SRC/BACK"}, []string{"zulu"}},
		{"query agent", AutomationFilter{Query: "CLAUDE"}, []string{"middle", "zulu"}},
		{"query project", AutomationFilter{Query: "FRONTEND"}, []string{"alpha"}},
		{"query schedule", AutomationFilter{Query: "1H"}, []string{"zulu"}},
		{"query human schedule", AutomationFilter{Query: "EVERY HOUR"}, []string{"zulu"}},
		{"not prompt", AutomationFilter{Query: "secret-prompt"}, []string{}},
		{"not model", AutomationFilter{Query: "hidden-model"}, []string{}},
		{"not timezone", AutomationFilter{Query: "hidden-zone"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := FilterAutomations(rows, tc.filter)
			keys := make([]string, 0, len(out))
			for _, row := range out {
				keys = append(keys, row.Key)
			}
			require.Equal(t, tc.keys, keys)
			require.Equal(t, "zulu", rows[0].Key)
		})
	}
	t.Run("stable equal keys", func(t *testing.T) {
		rows := []automation.Definition{{ID: "second", Key: "same"}, {ID: "first", Key: "same"}}
		require.Equal(t, rows, FilterAutomations(rows, AutomationFilter{}))
	})
	require.NotNil(t, FilterAutomations(nil, AutomationFilter{}))
}

func TestWriteAutomation(t *testing.T) {
	at := time.Date(2026, time.November, 11, 14, 15, 16, 123, time.UTC)
	for _, known := range []bool{false, true} {
		t.Run(strconv.FormatBool(known), func(t *testing.T) {
			row := automation.Definition{
				ID: automationTestID, Key: "review", Revision: 8, Spec: automationTestSpec(), Project: "Backend",
				CreatedAt: at, Next: at.Add(time.Hour), NextRuns: []time.Time{at.Add(time.Hour), at.Add(2 * time.Hour)}, ScheduleHuman: "weekday mornings",
				Running: true, RunsTotal: 12, PricedRuns: 3, TrackedRuns: 9,
				CurrentRun: &automation.Run{N: 12, Revision: 2, Phase: "stopping", StopReason: "operator request"},
				LastRun:    &automation.Run{N: 11, Phase: "terminal", Outcome: "failed", Result: "Missing result"},
			}
			if known {
				row.SuccessRate, row.AvgWall, row.Spend30d = new(0.0), new(0.0), new("0")
			}
			var b bytes.Buffer
			require.NoError(t, WriteAutomation(&b, row))
			out := b.String()
			for _, want := range []string{
				"id: " + automationTestID, "key: review", "revision: 8", "name: Review", "prompt: Review the code\n  Report findings",
				"path: /repo", "agent: agent", "model: model", "schedule: 0 9 * * 1-5", "timezone: Europe/Berlin", "workspace: repo", "overlap: queue", "timeout: 1h", "enabled: true",
				"project: Backend", "created_at: 2026-11-11T14:15:16.000000123Z", "next: 2026-11-11T15:15:16.000000123Z", "2026-11-11T16:15:16.000000123Z", "schedule_human: weekday mornings",
				"running: true", "runs_total: 12", "priced_runs: 3", "tracked_runs: 9", "current_run:\n", "n: 12", "phase: stopping", "stop_reason: operator request", "last_run:\n", "n: 11", "outcome: failed", "result: Missing result",
			} {
				require.Contains(t, out, want)
			}
			if known {
				require.Contains(t, out, "success_rate: 0%\navg_wall: 0s\nspend_30d: 0\n")
			} else {
				require.Contains(t, out, "success_rate: unknown\navg_wall: unknown\nspend_30d: unknown\n")
			}
		})
	}
	var b bytes.Buffer
	require.NoError(t, WriteAutomation(&b, automation.Definition{}))
	require.Contains(t, b.String(), "current_run: none\nlast_run: none\n")
	require.Contains(t, b.String(), "created_at: unknown")
	require.Contains(t, b.String(), "next: unknown")
}

func TestWriteAutomations(t *testing.T) {
	var b bytes.Buffer
	rows := []automation.Definition{
		{ID: "aut-z", Key: "zulu", Name: "Z", Enabled: true, Agent: "agent", Schedule: "30m", Timezone: "UTC", CurrentRun: &automation.Run{Phase: "recovery_required"}},
		{ID: automationTestID, Key: "alpha", Name: "Alpha", Schedule: "1h", Timezone: "UTC", CurrentRun: &automation.Run{Phase: "running"}, LastRun: &automation.Run{Outcome: "skipped"}},
	}
	require.NoError(t, WriteAutomations(&b, rows))
	out := b.String()
	require.Less(t, strings.Index(out, "alpha"), strings.Index(out, "zulu"))
	for _, want := range []string{automationTestID, "Alpha", "paused", "enabled", "running", "recovery_required", "30m", "UTC", "unknown", "agent", "skipped"} {
		require.Contains(t, out, want)
	}
	require.NotContains(t, out, "0001-01-01")
	require.Equal(t, "zulu", rows[0].Key)
	b.Reset()
	require.NoError(t, WriteAutomations(&b, nil))
	require.Equal(t, "No automations.\n", b.String())
}

func TestWriteAutomationRun(t *testing.T) {
	at := time.Date(2026, time.November, 11, 14, 15, 16, 0, time.FixedZone("offset", 3600))
	for _, tc := range []struct {
		phase, outcome string
		known          bool
	}{
		{"queued", "", false}, {"stopping", "", false}, {"recovery_required", "", false},
		{"terminal", "skipped", false}, {"terminal", "complete", true}, {"terminal", "failed", true},
	} {
		t.Run(tc.phase+"/"+tc.outcome, func(t *testing.T) {
			run := automation.Run{
				AutomationID: automationTestID, Key: "review", N: 12, Revision: 3, InvocationID: "invocation",
				Phase: tc.phase, Outcome: tc.outcome, Trigger: "manual", ScheduledFor: at, AcceptedAt: at.Add(time.Second), StartedAt: at.Add(2 * time.Second), CompletedAt: at.Add(3 * time.Second),
				Result: "First line\nSecond line", Spec: automationTestSpec(), Session: "session", Window: "window", WindowID: "@2", SessionID: "$1", SessionDir: "/sessions/12",
				Worktree: "/worktree", Branch: "auto/review-12", BaseCommit: "abcdef", LogPath: "/logs/12.log", ResultPath: "/logs/result.json", StopReason: "requested", Termination: "confirmed",
			}
			if tc.known {
				run.ExitCode, run.ObservedExitCode, run.Cost = new(0), new(137), new("0")
			}
			var b bytes.Buffer
			require.NoError(t, WriteAutomationRun(&b, run))
			out := b.String()
			for _, want := range []string{
				"automation_id: " + automationTestID, "key: review", "n: 12", "revision: 3", "invocation_id: invocation", "phase: " + tc.phase, "trigger: manual",
				"scheduled_for: 2026-11-11T14:15:16+01:00", "accepted_at: 2026-11-11T14:15:17+01:00", "started_at: 2026-11-11T14:15:18+01:00", "completed_at: 2026-11-11T14:15:19+01:00",
				"result: First line\n  Second line", "session: session", "window: window", "window_id: @2", "session_id: $1", "session_dir: /sessions/12", "worktree: /worktree", "branch: auto/review-12", "base_commit: abcdef",
				"log_path: /logs/12.log", "result_path: /logs/result.json", "stop_reason: requested", "termination: confirmed", "spec:\n", "prompt: Review the code\n    Report findings",
			} {
				require.Contains(t, out, want)
			}
			if tc.outcome == "" {
				require.Contains(t, out, "outcome: unknown\n")
			} else {
				require.Contains(t, out, "outcome: "+tc.outcome+"\n")
			}
			if tc.known {
				require.Contains(t, out, "exit_code: 0\nobserved_exit_code: 137\n")
				require.Contains(t, out, "cost: 0\n")
			} else {
				require.Contains(t, out, "exit_code: unknown\nobserved_exit_code: unknown\n")
				require.Contains(t, out, "cost: unknown\n")
			}
		})
	}
}

func TestWriteAutomationRuns(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, WriteAutomationRuns(&b, []automation.Run{{N: 12, Phase: "queued"}, {N: 11, Phase: "terminal", Outcome: "skipped"}}))
	out := b.String()
	require.Less(t, strings.Index(out, "n: 12"), strings.Index(out, "n: 11"))
	require.Contains(t, out, "phase: queued\noutcome: unknown")
	require.Contains(t, out, "outcome: skipped")
	require.Contains(t, out, "accepted_at: unknown")
	b.Reset()
	require.NoError(t, WriteAutomationRuns(&b, nil))
	require.Equal(t, "No automation runs.\n", b.String())
}

func TestWriteAutomationLogs(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, WriteAutomationLogs(&b, []automation.LogLine{
		{At: time.Date(2026, time.November, 11, 14, 15, 16, 0, time.UTC), Source: "agent", Kind: "text", Text: "First\nSecond"},
		{Source: "kontora", Kind: "lifecycle", Text: "stopping"},
	}))
	require.Equal(t, "2026-11-11T14:15:16Z [agent] [text] First\nSecond\nunknown [kontora] [lifecycle] stopping\n", b.String())
	b.Reset()
	require.NoError(t, WriteAutomationLogs(&b, nil))
	require.Empty(t, b.String())
}

var errAutomationWrite = errors.New("write failed")

type automationFailWriter struct{}

func (automationFailWriter) Write([]byte) (int, error) { return 0, errAutomationWrite }

func TestAutomationOutputWriteErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(io.Writer) error
	}{
		{"definition", func(w io.Writer) error { return WriteAutomation(w, automation.Definition{}) }},
		{"list", func(w io.Writer) error { return WriteAutomations(w, []automation.Definition{{}}) }},
		{"empty list", func(w io.Writer) error { return WriteAutomations(w, nil) }},
		{"run", func(w io.Writer) error { return WriteAutomationRun(w, automation.Run{}) }},
		{"runs", func(w io.Writer) error { return WriteAutomationRuns(w, []automation.Run{{}, {}}) }},
		{"empty runs", func(w io.Writer) error { return WriteAutomationRuns(w, nil) }},
		{"logs", func(w io.Writer) error { return WriteAutomationLogs(w, []automation.LogLine{{}}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, tc.write(automationFailWriter{}), errAutomationWrite)
		})
	}
}
