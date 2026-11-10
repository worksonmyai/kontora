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
	definitions []automation.Definition
	occurrences []automation.Occurrence
	err         error
	updateErr   error
	calls       []string
	requestID   string
	input       automation.Definition
	revision    uint64
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

func (m *automationMockService) CreateAutomation(requestID string, input automation.Definition) (automation.Definition, error) {
	m.calls = append(m.calls, "create")
	m.requestID, m.input = requestID, input
	input.ID, input.Revision, input.ScheduleRevision = "a1", 1, 1
	return input, m.err
}

func (m *automationMockService) UpdateAutomation(input automation.Definition) (automation.Definition, error) {
	m.calls = append(m.calls, "update")
	m.input = input
	input.Revision++
	if m.updateErr != nil {
		return input, m.updateErr
	}
	return input, m.err
}

func (m *automationMockService) AutomationOccurrences(id string) ([]automation.Occurrence, error) {
	m.calls = append(m.calls, "history:"+id)
	return m.occurrences, m.err
}

func (m *automationMockService) SkipAutomationOccurrence(id, occurrenceID string, revision uint64) (automation.Occurrence, error) {
	m.calls = append(m.calls, "skip:"+id+":"+occurrenceID)
	m.revision = revision
	return automation.Occurrence{ID: occurrenceID, AutomationID: id, Revision: revision + 1, Phase: "skipped"}, m.err
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
	return automation.Definition{
		ID: "a1", Revision: 7, ScheduleRevision: 2, Name: "Daily report", Path: "/repo", Agent: "agent",
		Instructions: "Inspect the repository and report changes.", SkipOverlap: true,
		Schedule: automation.Schedule{Frequency: "daily", Time: "09:00", Timezone: "Europe/Berlin"},
		Next:     time.Date(2030, 1, 2, 8, 0, 0, 0, time.UTC),
	}
}

func TestAutomationHandlersContract(t *testing.T) {
	definition := automationFixture()
	svc := &automationMockService{definitions: []automation.Definition{definition}}
	srv := automationTestServer(t, svc, "")

	w := automationRequest(srv, "GET", "/api/automations", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var list AutomationsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.True(t, list.Available)
	assert.Equal(t, []automation.Definition{definition}, list.Definitions)

	w = automationRequest(srv, "GET", "/api/automations/a1", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, automationJSON(t, definition), w.Body.String())

	input := definition
	input.ID, input.Revision, input.ScheduleRevision, input.Next = "", 0, 0, time.Time{}
	body := automationJSON(t, map[string]any{"request_id": "receipt-1", "definition": input})
	for range 2 {
		w = automationRequest(srv, "POST", "/api/automations", body, nil)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		assert.Equal(t, "receipt-1", svc.requestID)
		assert.Equal(t, input, svc.input)
	}

	definition.Name = "Updated name"
	w = automationRequest(srv, "PUT", "/api/automations/a1", automationJSON(t, definition), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, definition, svc.input)
	var updated automation.Definition
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.Equal(t, uint64(8), updated.Revision)

	w = automationRequest(srv, "POST", "/api/automations/a1/occurrences/o1/skip", `{"revision":9}`, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, uint64(9), svc.revision)
	assert.Equal(t, "skip:a1:o1", svc.calls[len(svc.calls)-1])
	var skipped automation.Occurrence
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &skipped))
	assert.Equal(t, "skipped", skipped.Phase)
	assert.Equal(t, uint64(10), skipped.Revision)
}

func TestAutomationCreateDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         bool
	}{
		{"omitted overlap", "", true},
		{"skip overlap", `,"skip_overlap":true`, true},
		{"allow overlap", `,"skip_overlap":false`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &automationMockService{}
			srv := automationTestServer(t, svc, "")
			body := `{"request_id":"r1","definition":{"name":"Daily report","path":"/repo","agent":"agent","instructions":"Report changes","schedule":{"frequency":"daily","time":"09:00","timezone":"UTC"}` + tc.fields + `}}`
			w := automationRequest(srv, "POST", "/api/automations", body, nil)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			assert.Equal(t, tc.want, svc.input.SkipOverlap)
			assert.False(t, svc.input.Enabled)
		})
	}
}

func TestAutomationPreview(t *testing.T) {
	// Calendar preview is stateless, even when the optional store is unavailable.
	srv := automationTestServer(t, &mockService{}, "")
	before := time.Now()
	w := automationRequest(srv, "POST", "/api/automations/preview", `{"frequency":"daily","time":"09:00","timezone":"Europe/Berlin"}`, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var preview struct {
		Occurrences []time.Time `json:"occurrences"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &preview))
	require.Len(t, preview.Occurrences, 3)
	loc, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	for i, instant := range preview.Occurrences {
		assert.True(t, instant.After(before))
		assert.Equal(t, 9, instant.In(loc).Hour())
		assert.Zero(t, instant.In(loc).Minute())
		if i > 0 {
			assert.True(t, instant.After(preview.Occurrences[i-1]))
		}
	}
	for _, body := range []string{
		`{}`, `{"frequency":"daily","time":"25:00","timezone":"UTC"}`,
		`{"frequency":"daily","time":"09:00","timezone":"Local"}`,
		`{"frequency":"daily","time":"09:00","timezone":"Invalid/Timezone"}`,
		`{"frequency":"weekly","days":[7],"time":"09:00","timezone":"UTC"}`,
	} {
		w = automationRequest(srv, "POST", "/api/automations/preview", body, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, body)
	}
}

func TestAutomationStrictDecode(t *testing.T) {
	svc := &automationMockService{definitions: []automation.Definition{automationFixture()}}
	srv := automationTestServer(t, svc, "")
	for _, route := range []struct{ method, path, valid string }{
		{"POST", "/api/automations", `{"request_id":"r1","definition":{}}`},
		{"PUT", "/api/automations/a1", `{"revision":7}`},
		{"POST", "/api/automations/preview", `{"frequency":"daily","time":"09:00","timezone":"UTC"}`},
		{"POST", "/api/automations/a1/occurrences/o1/skip", `{"revision":1}`},
	} {
		for _, body := range []string{
			"", "null", "[]", "42", `"text"`, "{", `{"unknown":true}`,
			route.valid + `{}`, route.valid + " null", route.valid + " garbage",
			route.valid + strings.Repeat(" ", automationRequestMax),
			`{"unknown":"` + strings.Repeat("x", automationRequestMax) + `"}`,
		} {
			t.Run(route.method+route.path+"/"+fmt.Sprint(len(body))+"/"+body[:min(len(body), 50)], func(t *testing.T) {
				svc.calls = nil
				w := automationRequest(srv, route.method, route.path, body, nil)
				assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
				assert.Empty(t, svc.calls)
			})
		}
	}
	for _, body := range []string{
		`{"request_id":"r","definition":{"schedule":{"unknown":true}}}`,
		`{"request_id":"r","definition":{"automation_id":"spoofed"}}`,
		`{"request_id":"r","definition":null}`, `{"request_id":"r"}`,
		`{"request_id":" ","definition":{}}`, `{"definition":{}}`,
		`{"request_id":"` + strings.Repeat("x", 201) + `","definition":{}}`,
	} {
		svc.calls = nil
		w := automationRequest(srv, "POST", "/api/automations", body, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Empty(t, svc.calls)
	}
}

func TestAutomationBodyLimitWithoutContentLength(t *testing.T) {
	svc := &automationMockService{}
	srv := automationTestServer(t, svc, "")
	valid := `{"request_id":"r1","definition":{}}`
	for _, size := range []int{automationRequestMax, automationRequestMax + 1} {
		svc.calls = nil
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/automations", strings.NewReader(valid+strings.Repeat(" ", size-len(valid))))
		r.ContentLength = -1
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.httpSrv.Handler.ServeHTTP(w, r)
		if size == automationRequestMax {
			assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			assert.Equal(t, []string{"create"}, svc.calls)
		} else {
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Empty(t, svc.calls)
		}
	}
}

func TestAutomationReadonlyFieldsAndRevision(t *testing.T) {
	svc := &automationMockService{definitions: []automation.Definition{automationFixture()}}
	srv := automationTestServer(t, svc, "")
	for _, fields := range []string{
		`"id":"spoofed"`, `"revision":1`, `"schedule_revision":1`,
		`"next":"2030-01-01T00:00:00Z"`, `"enabled":true`,
	} {
		svc.calls = nil
		w := automationRequest(srv, "POST", "/api/automations", `{"request_id":"r1","definition":{`+fields+`}}`, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Empty(t, svc.calls)
	}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"revision":7,"id":"different"}`, 400},
		{`{"revision":0}`, 400},
		{`{"revision":-1}`, 400},
		{`{"revision":1.1}`, 400},
		{`{"revision":18446744073709551616}`, 400},
		{`{"revision":null}`, 400},
		{`{"revision":6}`, 409},
		{`{"revision":7,"schedule_revision":3}`, 400},
	} {
		svc.calls = nil
		w := automationRequest(srv, "PUT", "/api/automations/a1", tc.body, nil)
		assert.Equal(t, tc.want, w.Code, w.Body.String())
		assert.NotContains(t, svc.calls, "update")
	}
	for _, body := range []string{`{"revision":7}`, `{"revision":7,"next":"2031-01-01T00:00:00Z"}`} {
		w := automationRequest(srv, "PUT", "/api/automations/a1", body, nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "a1", svc.input.ID)
		assert.Equal(t, uint64(2), svc.input.ScheduleRevision)
		assert.Equal(t, svc.definitions[0].Next, svc.input.Next)
	}

	for _, body := range []string{`{}`, `{"revision":0}`, `{"revision":null}`, `{"revision":-1}`, `{"revision":1.5}`, `{"revision":"1"}`} {
		svc.calls = nil
		w := automationRequest(srv, "POST", "/api/automations/a1/occurrences/o1/skip", body, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Empty(t, svc.calls)
	}
}

func TestAutomationPagination(t *testing.T) {
	svc := &automationMockService{}
	for i := range 205 {
		svc.occurrences = append(svc.occurrences, automation.Occurrence{ID: fmt.Sprint(i)})
	}
	srv := automationTestServer(t, svc, "")
	for _, tc := range []struct {
		query        string
		start, count int
	}{
		{"", 0, 50}, {"?offset=2&limit=3", 2, 3},
		{"?offset=0&limit=200", 0, 200}, {"?offset=204&limit=200", 204, 1},
		{"?offset=205", 205, 0}, {"?offset=999999999", 205, 0},
	} {
		w := automationRequest(srv, "GET", "/api/automations/a1/occurrences"+tc.query, "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var page AutomationOccurrencesResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
		assert.Equal(t, 205, page.Total)
		require.Len(t, page.Occurrences, tc.count)
		assert.NotContains(t, w.Body.String(), `"occurrences":null`)
		if tc.count > 0 {
			assert.Equal(t, fmt.Sprint(tc.start), page.Occurrences[0].ID)
		}
	}
	for _, query := range []string{
		"?offset=-1", "?offset=", "?offset=1.5", "?offset=no", "?offset=1&offset=2",
		"?offset=999999999999999999999999", "?limit=0", "?limit=-1", "?limit=201", "?limit=", "?limit=1&limit=2",
	} {
		svc.calls = nil
		w := automationRequest(srv, "GET", "/api/automations/a1/occurrences"+query, "", nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, query)
		assert.Empty(t, svc.calls)
	}
	svc.occurrences = nil
	w := automationRequest(srv, "GET", "/api/automations/a1/occurrences", "", nil)
	assert.JSONEq(t, `{"occurrences":[],"total":0}`, w.Body.String())
}

func TestAutomationServiceErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{automation.ErrValidation, 400}, {automation.ErrNotFound, 404},
		{automation.ErrConflict, 409}, {automation.ErrUnavailable, 503}, {io.ErrUnexpectedEOF, 500}, {ErrInvalidState, 409},
	} {
		svc := &automationMockService{err: fmt.Errorf("service: %w", tc.err)}
		srv := automationTestServer(t, svc, "")
		for _, route := range []struct{ method, path, body string }{
			{"GET", "/api/automations/a1", ""},
			{"POST", "/api/automations", `{"request_id":"r1","definition":{}}`},
			{"PUT", "/api/automations/a1", `{"revision":1}`},
			{"GET", "/api/automations/a1/occurrences", ""},
			{"POST", "/api/automations/a1/occurrences/o1/skip", `{"revision":1}`},
		} {
			w := automationRequest(srv, route.method, route.path, route.body, nil)
			assert.Equal(t, tc.want, w.Code, "%s %s: %s", route.method, route.path, w.Body.String())
			assert.Contains(t, w.Body.String(), `"error":`)
		}
		// Update can fail after the read, including a concurrent revision change.
		svc.err, svc.updateErr = nil, fmt.Errorf("update: %w", tc.err)
		svc.definitions = []automation.Definition{automationFixture()}
		w := automationRequest(srv, "PUT", "/api/automations/a1", `{"revision":7}`, nil)
		assert.Equal(t, tc.want, w.Code, w.Body.String())
		assert.Contains(t, svc.calls, "update")
		svc.err = fmt.Errorf("service: %w", tc.err)
		w = automationRequest(srv, "GET", "/api/automations", "", nil)
		if errors.Is(tc.err, automation.ErrUnavailable) {
			assert.Equal(t, http.StatusOK, w.Code)
			assert.JSONEq(t, `{"available":false,"error":"service: automation storage unavailable","definitions":[]}`, w.Body.String())
		} else {
			assert.Equal(t, tc.want, w.Code)
		}
	}
}

func TestAutomationOptionalService(t *testing.T) {
	srv := automationTestServer(t, &mockService{}, "")
	w := automationRequest(srv, "GET", "/api/automations", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"available":false,"error":"automation storage unavailable","definitions":[]}`, w.Body.String())
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/automations/a1"}, {"POST", "/api/automations"}, {"PUT", "/api/automations/a1"},
		{"GET", "/api/automations/a1/occurrences"}, {"POST", "/api/automations/a1/occurrences/o1/skip"},
	} {
		w = automationRequest(srv, route.method, route.path, `{}`, nil)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	}
	srv = automationTestServer(t, &automationMockService{}, "")
	w = automationRequest(srv, "GET", "/api/automations", "", nil)
	assert.JSONEq(t, `{"available":true,"definitions":[]}`, w.Body.String())
}

func TestAutomationInvalidationSSE(t *testing.T) {
	broker := NewSSEBroker()
	srv := startTestServer(t, &mockService{}, broker, "", "test")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+srv.Addr()+"/api/events", nil)
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
