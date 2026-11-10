package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
)

type automationMockService struct {
	mockService
	definitions    []automation.Definition
	runs           []automation.Run
	err            error
	calls          []string
	requestID, key string
	input          automation.Spec
	revision, n    uint64
}

var _ AutomationService = (*automationMockService)(nil)

func (m *automationMockService) ListAutomations() ([]automation.Definition, error) {
	m.calls = append(m.calls, "list")
	return m.definitions, m.err
}
func (m *automationMockService) GetAutomation(id string) (automation.Definition, error) {
	m.calls = append(m.calls, "get:"+id)
	if len(m.definitions) == 0 {
		return automation.Definition{}, m.err
	}
	return m.definitions[0], m.err
}
func (m *automationMockService) CreateAutomation(requestID, key string, input automation.Spec) (automation.Definition, error) {
	m.calls = append(m.calls, "create")
	m.requestID, m.key, m.input = requestID, key, input
	return automation.Definition{ID: "a1", Key: key, Revision: 1, Spec: input}, m.err
}
func (m *automationMockService) UpdateAutomation(id string, revision uint64, input automation.Spec) (automation.Definition, error) {
	m.calls = append(m.calls, "update:"+id)
	m.revision, m.input = revision, input
	return automation.Definition{ID: id, Revision: revision + 1, Spec: input}, m.err
}
func (m *automationMockService) DeleteAutomation(id string, revision uint64) error {
	m.calls = append(m.calls, "delete:"+id)
	m.revision = revision
	return m.err
}
func (m *automationMockService) RunAutomation(id, requestID string) (automation.Run, error) {
	m.calls = append(m.calls, "run:"+id)
	m.requestID = requestID
	return automation.Run{AutomationID: id, N: 1, Phase: "queued", Trigger: "manual"}, m.err
}
func (m *automationMockService) StopAutomation(id string, n uint64) (automation.Run, error) {
	m.calls = append(m.calls, "stop:"+id)
	m.n = n
	return automation.Run{AutomationID: id, N: n, Phase: "stopping"}, m.err
}
func (m *automationMockService) AutomationRuns(id string) ([]automation.Run, error) {
	m.calls = append(m.calls, "runs:"+id)
	return m.runs, m.err
}
func (m *automationMockService) AutomationLog(id string, n uint64) ([]automation.LogLine, string, error) {
	m.calls = append(m.calls, "log:"+id)
	m.n = n
	return []automation.LogLine{{At: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Source: "agent", Kind: "text", Text: "hello"}}, "hello\n", m.err
}

func automationTestServer(t *testing.T, svc TicketService, token string, options ...ServerOption) *Server {
	t.Helper()
	srv, err := New(svc, NewSSEBroker(), "127.0.0.1", 0, token, nil, "test", slog.New(slog.NewTextHandler(io.Discard, nil)), options...)
	require.NoError(t, err)
	return srv
}
func automationRequest(srv *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		if name == "Host" {
			r.Host = value
		} else {
			r.Header.Set(name, value)
		}
	}
	w := httptest.NewRecorder()
	srv.httpSrv.Handler.ServeHTTP(w, r)
	return w
}
func automationJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}
func automationFixture() automation.Definition {
	return automation.Definition{ID: "a1", Key: "daily-report", Revision: 7,
		Name: "Daily report", Path: "/repo", Agent: "agent", Prompt: "Inspect the repository and report changes.", Schedule: "0 9 * * *", Timezone: "Europe/Berlin", Workspace: "worktree", Overlap: "skip", Timeout: "30m", Enabled: true,
		Next: time.Date(2030, 1, 2, 8, 0, 0, 0, time.UTC),
	}
}

func TestAutomationHandlersContract(t *testing.T) {
	def := automationFixture()
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		calls                    []string
	}{
		{"list", "GET", "/api/automations", "", 200, []string{"list"}},
		{"get", "GET", "/api/automations/a1", "", 200, []string{"get:a1"}},
		{"create", "POST", "/api/automations", automationJSON(t, map[string]any{"request_id": "create-1", "key": def.Key, "definition": def.Spec}), 201, []string{"create"}},
		{"update", "PUT", "/api/automations/a1", automationJSON(t, map[string]any{"revision": 7, "definition": def.Spec}), 200, []string{"update:a1"}},
		{"delete", "DELETE", "/api/automations/a1", `{"revision":7}`, 204, []string{"delete:a1"}},
		{"run", "POST", "/api/automations/a1/run", `{"request_id":"run-1"}`, 202, []string{"run:a1"}},
		{"stop", "POST", "/api/automations/a1/stop", `{"n":9}`, 202, []string{"stop:a1"}},
		{"pause", "POST", "/api/automations/a1/pause", `{"revision":7}`, 200, []string{"get:a1", "update:a1"}},
		{"resume", "POST", "/api/automations/a1/resume", `{"revision":7}`, 200, []string{"get:a1", "update:a1"}},
		{"runs", "GET", "/api/automations/a1/runs", "", 200, []string{"runs:a1"}},
		{"log", "GET", "/api/automations/a1/runs/9/log", "", 200, []string{"log:a1"}},
		{"raw log", "GET", "/api/automations/a1/runs/9/log?raw=1", "", 200, []string{"log:a1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &automationMockService{definitions: []automation.Definition{def}}
			w := automationRequest(automationTestServer(t, svc, ""), tc.method, tc.path, tc.body, nil)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Equal(t, tc.calls, svc.calls)
			switch tc.name {
			case "list":
				var got AutomationsResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
				assert.True(t, got.Available)
				assert.Equal(t, []automation.Definition{def}, got.Definitions)
			case "get":
				assert.JSONEq(t, automationJSON(t, def), w.Body.String())
			case "create":
				assert.Equal(t, "create-1", svc.requestID)
				assert.Equal(t, def.Key, svc.key)
				assert.Equal(t, def.Spec, svc.input)
			case "update":
				assert.Equal(t, uint64(7), svc.revision)
				assert.Equal(t, def.Spec, svc.input)
			case "delete":
				assert.Equal(t, uint64(7), svc.revision)
				assert.Empty(t, w.Body.String())
			case "run":
				assert.Equal(t, "run-1", svc.requestID)
				var got map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
				assert.Nil(t, got["cost"])
				assert.Nil(t, got["exit_code"])
			case "stop":
				assert.Equal(t, uint64(9), svc.n)
			case "pause", "resume":
				expected := def.Spec
				expected.Enabled = tc.name == "resume"
				assert.Equal(t, expected, svc.input)
				assert.Equal(t, uint64(7), svc.revision)
			case "runs":
				assert.JSONEq(t, `{"runs":[],"total":0}`, w.Body.String())
			case "log":
				var got struct {
					Lines []automation.LogLine
					Raw   string
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
				require.Len(t, got.Lines, 1)
				assert.Equal(t, "agent", got.Lines[0].Source)
				assert.Equal(t, "hello\n", got.Raw)
				assert.Equal(t, uint64(9), svc.n)
			case "raw log":
				assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))
				assert.Equal(t, "hello\n", w.Body.String())
			}
		})
	}
}

func TestAutomationStrictRequests(t *testing.T) {
	for _, route := range automationProtectedRoutes {
		if route.method == "GET" {
			continue
		}
		t.Run(route.method+route.path, func(t *testing.T) {
			for _, body := range []string{"", "null", "[]", `"text"`, "{", route.body + ` {}`, strings.TrimSuffix(route.body, "}") + `,"unknown":true}`, route.body + strings.Repeat(" ", automationRequestMax)} {
				svc := &automationMockService{definitions: []automation.Definition{automationFixture()}}
				w := automationRequest(automationTestServer(t, svc, ""), route.method, route.path, body, nil)
				assert.Equal(t, 400, w.Code, w.Body.String())
				assert.Empty(t, svc.calls)
			}
		})
	}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/automations", `{"request_id":"r"}`},
		{"POST", "/api/automations", `{"request_id":"r","definition":null}`},
		{"POST", "/api/automations", `{"request_id":" ","definition":{}}`},
		{"POST", "/api/automations", `{"request_id":"` + strings.Repeat("x", 201) + `","definition":{}}`},
		{"POST", "/api/automations", `{"request_id":"r","definition":{"id":"spoofed"}}`},
		{"POST", "/api/automations", `{"request_id":"r","definition":{"instructions":"legacy"}}`},
		{"PUT", "/api/automations/a1", `{"revision":0,"definition":{}}`},
		{"PUT", "/api/automations/a1", `{"revision":7}`},
		{"PUT", "/api/automations/a1", `{"revision":-1,"definition":{}}`},
		{"PUT", "/api/automations/a1", `{"revision":1.5,"definition":{}}`},
		{"DELETE", "/api/automations/a1", `{}`},
		{"POST", "/api/automations/a1/pause", `{}`},
		{"POST", "/api/automations/a1/resume", `{"revision":"7"}`},
		{"POST", "/api/automations/a1/run", `{"request_id":" "}`},
		{"POST", "/api/automations/a1/run", `{"request_id":"` + strings.Repeat("x", 201) + `"}`},
		{"POST", "/api/automations/a1/run", `{"request_id":"r","session":"arbitrary"}`},
		{"POST", "/api/automations/a1/stop", `{"n":0}`},
		{"POST", "/api/automations/a1/stop", `{"n":-1}`},
		{"GET", "/api/automations/a1/runs/0/log", ""},
		{"GET", "/api/automations/a1/runs/not-a-number/log", ""},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			svc := &automationMockService{}
			w := automationRequest(automationTestServer(t, svc, ""), tc.method, tc.path, tc.body, nil)
			assert.Equal(t, 400, w.Code, w.Body.String())
			assert.Empty(t, svc.calls)
		})
	}
}

func TestAutomationReadOnlyFields(t *testing.T) {
	for _, field := range []string{"id", "key", "revision", "created_at", "next", "next_runs", "schedule_revision", "skip_overlap", "running", "current_run", "last_run", "runs_total", "success_rate", "session", "window", "window_id", "worktree", "result_path"} {
		for _, method := range []string{"POST", "PUT"} {
			t.Run(method+"/"+field, func(t *testing.T) {
				spec := map[string]any{}
				require.NoError(t, json.Unmarshal([]byte(automationJSON(t, automationFixture().Spec)), &spec))
				spec[field] = "client-selected"
				path := "/api/automations"
				body := map[string]any{"request_id": "r", "key": "daily-report", "definition": spec}
				if method == "PUT" {
					path += "/a1"
					body = map[string]any{"revision": 7, "definition": spec}
				}
				svc := &automationMockService{}
				w := automationRequest(automationTestServer(t, svc, ""), method, path, automationJSON(t, body), nil)
				assert.Equal(t, 400, w.Code, w.Body.String())
				assert.Empty(t, svc.calls)
			})
		}
	}
}

func TestAutomationBodyLimitWithoutContentLength(t *testing.T) {
	valid := `{"request_id":"r","definition":{}}`
	spec := automationFixture().Spec
	spec.Prompt = "x" + strings.Repeat("\n", 65535)
	escaped := automationJSON(t, map[string]any{"request_id": "r", "definition": spec})
	require.Greater(t, len(escaped), 128<<10)
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"at limit", valid + strings.Repeat(" ", automationRequestMax-len(valid)), http.StatusCreated},
		{"over limit", valid + strings.Repeat(" ", automationRequestMax+1-len(valid)), http.StatusBadRequest},
		{"maximum escaped prompt", escaped, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &automationMockService{}
			srv := automationTestServer(t, svc, "")
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/automations", strings.NewReader(tc.body))
			r.ContentLength = -1
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.httpSrv.Handler.ServeHTTP(w, r)
			assert.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == http.StatusCreated {
				assert.Equal(t, []string{"create"}, svc.calls)
				if tc.name == "maximum escaped prompt" {
					assert.Equal(t, spec, svc.input)
				}
			} else {
				assert.Empty(t, svc.calls)
			}
		})
	}
}

func TestAutomationPreview(t *testing.T) {
	srv := automationTestServer(t, &mockService{}, "")
	for _, tc := range []struct {
		schedule, zone string
		valid          bool
	}{
		{"1m", "", true}, {"90m", "UTC", true}, {"0 9 * * 1-5", "Europe/Berlin", true},
		{"30s", "UTC", false}, {"90s", "UTC", false}, {"0m", "UTC", false}, {"@daily", "UTC", false}, {"0 0 9 * * *", "UTC", false}, {"0 9 * * MON", "UTC", false}, {"1m", "Not/AZone", false},
	} {
		t.Run(tc.schedule+tc.zone, func(t *testing.T) {
			before := time.Now()
			w := automationRequest(srv, "POST", "/api/automations/preview", automationJSON(t, map[string]string{"schedule": tc.schedule, "timezone": tc.zone}), nil)
			if !tc.valid {
				assert.Equal(t, 400, w.Code, w.Body.String())
				return
			}
			require.Equal(t, 200, w.Code, w.Body.String())
			var got struct {
				Occurrences []time.Time
				Human       string
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			require.Len(t, got.Occurrences, 4)
			assert.NotEmpty(t, got.Human)
			for _, at := range got.Occurrences {
				assert.True(t, at.After(before))
				before = at
			}
		})
	}
}

func TestAutomationRunsPagination(t *testing.T) {
	svc := &automationMockService{}
	for i := 25; i > 0; i-- {
		svc.runs = append(svc.runs, automation.Run{N: uint64(i)})
	}
	srv := automationTestServer(t, svc, "")
	for _, tc := range []struct {
		query string
		count int
		first uint64
	}{
		{"", 20, 25}, {"?offset=20&limit=20", 5, 5}, {"?offset=25", 0, 0}, {"?offset=999999&limit=200", 0, 0}, {"?offset=1&limit=1", 1, 24},
	} {
		w := automationRequest(srv, "GET", "/api/automations/a1/runs"+tc.query, "", nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		var got AutomationRunsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.Equal(t, 25, got.Total)
		require.Len(t, got.Runs, tc.count)
		if tc.count > 0 {
			assert.Equal(t, tc.first, got.Runs[0].N)
		} else {
			assert.NotNil(t, got.Runs)
		}
	}
	for _, query := range []string{"?offset=-1", "?offset=", "?offset=1&offset=2", "?offset=99999999999999999999999", "?limit=0", "?limit=201", "?limit=x", "?limit=1&limit=2"} {
		svc.calls = nil
		w := automationRequest(srv, "GET", "/api/automations/a1/runs"+query, "", nil)
		assert.Equal(t, 400, w.Code, w.Body.String())
		assert.Empty(t, svc.calls)
	}
}

func TestAutomationServiceErrorsAndAvailability(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{automation.ErrValidation, 400}, {automation.ErrNotFound, 404}, {automation.ErrConflict, 409}, {automation.ErrUnavailable, 503}, {io.ErrUnexpectedEOF, 500}, {ErrInvalidState, 409},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			svc := &automationMockService{err: fmt.Errorf("service: %w", tc.err), definitions: []automation.Definition{automationFixture()}}
			srv := automationTestServer(t, svc, "")
			for _, route := range automationProtectedRoutes {
				if route.path == "/api/automations/preview" {
					continue
				}
				w := automationRequest(srv, route.method, route.path, route.body, nil)
				if route.method == "GET" && route.path == "/api/automations" && errors.Is(tc.err, automation.ErrUnavailable) {
					assert.Equal(t, 200, w.Code)
					assert.JSONEq(t, `{"available":false,"error":"service: automation storage unavailable","definitions":[]}`, w.Body.String())
				} else {
					assert.Equal(t, tc.status, w.Code, w.Body.String())
				}
			}
		})
	}
	for _, tc := range []struct {
		svc       TicketService
		available bool
	}{{&mockService{}, false}, {&automationMockService{}, true}} {
		srv := automationTestServer(t, tc.svc, "")
		w := automationRequest(srv, "GET", "/api/automations", "", nil)
		require.Equal(t, 200, w.Code)
		var got AutomationsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.Equal(t, tc.available, got.Available)
		assert.NotNil(t, got.Definitions)
		if !tc.available {
			for _, route := range automationProtectedRoutes {
				if route.path == "/api/automations/preview" || route.path == "/api/automations" && route.method == "GET" {
					continue
				}
				w = automationRequest(srv, route.method, route.path, route.body, nil)
				assert.Equal(t, 503, w.Code, w.Body.String())
			}
		}
	}
}

func TestAutomationInvalidationSSE(t *testing.T) {
	broker := NewSSEBroker()
	srv := startTestServer(t, &mockService{}, broker, "", "test")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr()+"/api/events", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Eventually(t, func() bool {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		return len(broker.clients) > 0
	}, time.Second, time.Millisecond)
	broker.Broadcast(TicketEvent{Type: "automations_updated", Ticket: TicketInfo{ID: "must-not-leak"}})
	scanner := bufio.NewScanner(response.Body)
	var event bytes.Buffer
	for scanner.Scan() {
		if scanner.Text() == "" {
			break
		}
		event.WriteString(scanner.Text() + "\n")
	}
	require.NoError(t, scanner.Err())
	assert.Equal(t, "event: automations_updated\ndata: {}\n", event.String())
}
