package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/tmux"
)

func TestAssistantRoutes(t *testing.T) {
	created := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		svc        *mockService
		method     string
		path       string
		body       string
		wantStatus int
		// wantBody, when set, is compared as JSON.
		wantBody  string
		check     func(t *testing.T, res httpResult)
		wantCalls []string
	}{
		{
			name:       "the configure state when no agent is set",
			svc:        &mockService{assistantConfig: AssistantConfigInfo{Enabled: false, Hint: "Set assistant.agent"}},
			method:     http.MethodGet,
			path:       "/api/assistant",
			wantStatus: http.StatusOK,
			wantBody:   `{"enabled":false,"hint":"Set assistant.agent"}`,
		},
		{
			name:       "the enabled state names the agent",
			svc:        &mockService{assistantConfig: AssistantConfigInfo{Enabled: true, Agent: "cl", Kind: "claude", Autonomy: "ask"}},
			method:     http.MethodGet,
			path:       "/api/assistant",
			wantStatus: http.StatusOK,
			wantBody:   `{"enabled":true,"agent":"cl","kind":"claude","autonomy":"ask"}`,
		},
		{
			name:       "an empty history is a list, not null",
			svc:        &mockService{},
			method:     http.MethodGet,
			path:       "/api/assistant/threads",
			wantStatus: http.StatusOK,
			wantBody:   `{"threads":[]}`,
		},
		{
			name: "the history lists threads",
			svc: &mockService{assistantThreads: []AssistantThreadInfo{
				{ID: "t1", Title: "what is running", CreatedAt: created, UpdatedAt: created, Autonomy: "ask", Turns: 2},
			}},
			method:     http.MethodGet,
			path:       "/api/assistant/threads",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, res httpResult) {
				var out struct{ Threads []AssistantThreadInfo }
				require.NoError(t, json.Unmarshal([]byte(res.body), &out))
				require.Len(t, out.Threads, 1)
				assert.Equal(t, "what is running", out.Threads[0].Title)
			},
		},
		{
			name:       "creating a thread returns it",
			svc:        &mockService{},
			method:     http.MethodPost,
			path:       "/api/assistant/threads",
			body:       `{"autonomy":"read"}`,
			wantStatus: http.StatusCreated,
			check: func(t *testing.T, res httpResult) {
				var thread AssistantThreadInfo
				require.NoError(t, json.Unmarshal([]byte(res.body), &thread))
				assert.Equal(t, "read", thread.Autonomy)
			},
		},
		{
			name:       "creating a thread with no body takes the defaults",
			svc:        &mockService{},
			method:     http.MethodPost,
			path:       "/api/assistant/threads",
			wantStatus: http.StatusCreated,
		},
		{
			name: "creating a thread with no assistant configured is not implemented",
			svc: &mockService{assistantCreateFn: func(CreateAssistantThreadRequest) (AssistantThreadInfo, error) {
				return AssistantThreadInfo{}, ErrAssistantDisabled
			}},
			method:     http.MethodPost,
			path:       "/api/assistant/threads",
			wantStatus: http.StatusNotImplemented,
		},
		{
			name:       "an unknown thread is a 404",
			svc:        &mockService{},
			method:     http.MethodGet,
			path:       "/api/assistant/threads/nope",
			wantStatus: http.StatusNotFound,
		},
		{
			name: "a known thread comes back with its messages",
			svc: &mockService{assistantThreads: []AssistantThreadInfo{
				{ID: "t1", Title: "hi", Messages: []AssistantMessage{{N: 1, Text: "what is running"}}},
			}},
			method:     http.MethodGet,
			path:       "/api/assistant/threads/t1",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, res httpResult) {
				var thread AssistantThreadInfo
				require.NoError(t, json.Unmarshal([]byte(res.body), &thread))
				require.Len(t, thread.Messages, 1)
				assert.Equal(t, "what is running", thread.Messages[0].Text)
			},
		},
		{
			name:       "deleting a thread",
			svc:        &mockService{},
			method:     http.MethodDelete,
			path:       "/api/assistant/threads/t1",
			wantStatus: http.StatusNoContent,
			wantCalls:  []string{"delete t1"},
		},
		{
			name:       "deleting an unknown thread is a 404",
			svc:        &mockService{assistantDeleteFn: func(string) error { return ErrAssistantNotFound }},
			method:     http.MethodDelete,
			path:       "/api/assistant/threads/nope",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "posting a message starts a turn",
			svc:        &mockService{},
			method:     http.MethodPost,
			path:       "/api/assistant/threads/t1/messages",
			body:       `{"text":"what is running"}`,
			wantStatus: http.StatusAccepted,
			wantCalls:  []string{"message t1 what is running"},
		},
		{
			name:       "an empty message is rejected",
			svc:        &mockService{},
			method:     http.MethodPost,
			path:       "/api/assistant/threads/t1/messages",
			body:       `{"text":""}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a second turn on a busy thread is a conflict",
			svc:        &mockService{assistantMsgFn: func(string, AssistantMessageRequest) error { return ErrAssistantBusy }},
			method:     http.MethodPost,
			path:       "/api/assistant/threads/t1/messages",
			body:       `{"text":"and now"}`,
			wantStatus: http.StatusConflict,
		},
		{
			// Not the same refusal as a busy thread: this chat has never run,
			// and the daemon is at its global cap.
			name:       "a turn refused by the global cap is a 503",
			svc:        &mockService{assistantMsgFn: func(string, AssistantMessageRequest) error { return ErrAssistantAtCapacity }},
			method:     http.MethodPost,
			path:       "/api/assistant/threads/t1/messages",
			body:       `{"text":"and now"}`,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "a thread whose agent has been repointed is a conflict",
			svc:        &mockService{assistantMsgFn: func(string, AssistantMessageRequest) error { return ErrAssistantStale }},
			method:     http.MethodPost,
			path:       "/api/assistant/threads/t1/messages",
			body:       `{"text":"and now"}`,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "stopping a turn",
			svc:        &mockService{},
			method:     http.MethodPost,
			path:       "/api/assistant/threads/t1/stop",
			wantStatus: http.StatusNoContent,
			wantCalls:  []string{"stop t1"},
		},
		{
			name:       "approving a parked write",
			svc:        &mockService{},
			method:     http.MethodPost,
			path:       "/api/assistant/gate/g1",
			body:       `{"decision":"approve"}`,
			wantStatus: http.StatusNoContent,
			wantCalls:  []string{"gate g1 true"},
		},
		{
			name:       "skipping a parked write",
			svc:        &mockService{},
			method:     http.MethodPost,
			path:       "/api/assistant/gate/g1",
			body:       `{"decision":"skip"}`,
			wantStatus: http.StatusNoContent,
			wantCalls:  []string{"gate g1 false"},
		},
		{
			name:       "a decision that is neither is rejected",
			svc:        &mockService{},
			method:     http.MethodPost,
			path:       "/api/assistant/gate/g1",
			body:       `{"decision":"maybe"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "answering a call that is already gone is a 404",
			svc:        &mockService{assistantGateFn: func(string, bool) error { return ErrAssistantGateNotFound }},
			method:     http.MethodPost,
			path:       "/api/assistant/gate/g1",
			body:       `{"decision":"approve"}`,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "the agent side of the gate answers allow",
			svc: &mockService{assistantAskFn: func(req AssistantGateAskRequest) (AssistantGateAskResponse, error) {
				assert.Equal(t, "Bash", req.Tool)
				return AssistantGateAskResponse{Allow: true}, nil
			}},
			method:     http.MethodPost,
			path:       "/api/assistant/gate/ask",
			body:       `{"thread":"t1","nonce":"n","tool":"Bash","input":{"command":"kontora ls"}}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"allow":true}`,
		},
		{
			name: "a gate call with the wrong nonce is refused",
			svc: &mockService{assistantAskFn: func(AssistantGateAskRequest) (AssistantGateAskResponse, error) {
				return AssistantGateAskResponse{}, ErrAssistantGateDenied
			}},
			method:     http.MethodPost,
			path:       "/api/assistant/gate/ask",
			body:       `{"thread":"t1","nonce":"wrong"}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a negative cursor is rejected",
			svc:        &mockService{},
			method:     http.MethodGet,
			path:       "/api/assistant/threads/t1/activity?after=-1",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startHandlerTestServer(t, tt.svc)
			res := request(t, srv, tt.method, tt.path, tt.body)
			assert.Equal(t, tt.wantStatus, res.statusCode, "body: %s", res.body)
			if tt.wantBody != "" {
				assert.JSONEq(t, tt.wantBody, res.body)
			}
			if tt.check != nil {
				tt.check(t, res)
			}
			if tt.wantCalls != nil {
				assert.Equal(t, tt.wantCalls, tt.svc.assistantCalls)
			}
		})
	}
}

func TestAssistantActivityCursorAndETag(t *testing.T) {
	tape := logfmt.Tape{Version: 1, Agent: "claude", Events: []logfmt.Event{{Kind: "text", Text: "hello"}}}
	pending := assistant.Pending{ID: "g1", Call: assistant.Call{ThreadID: "t1", Tool: "Bash", Kind: assistant.DecisionWrite}}

	svc := &mockService{assistantActFn: func(q AssistantActivityQuery) (AssistantActivityInfo, error) {
		if q.IfNoneMatch == `"same"` {
			return AssistantActivityInfo{NotModified: true, ETag: `"same"`}, nil
		}
		return AssistantActivityInfo{
			Running: true, Autonomy: "ask", Offset: q.After,
			Tape: &tape, Gate: &pending, ETag: `"same"`,
		}, nil
	}}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/assistant/threads/t1/activity?after=3")
	require.Equal(t, http.StatusOK, res.statusCode)
	assert.Equal(t, `"same"`, res.etag)

	var info AssistantActivityInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &info))
	assert.True(t, info.Running)
	assert.Equal(t, 3, info.Offset)
	require.NotNil(t, info.Gate)
	assert.Equal(t, "g1", info.Gate.ID)
	require.NotNil(t, info.Tape)
	require.Len(t, info.Tape.Events, 1)

	// The same validator comes back as a 304 with no body.
	res = getWithHeaders(t, srv, "/api/assistant/threads/t1/activity", map[string]string{"If-None-Match": `"same"`})
	assert.Equal(t, http.StatusNotModified, res.statusCode)
	assert.Empty(t, res.body)
}

func TestAssistantRoutesRequireToken(t *testing.T) {
	srv := startTestServer(t, &mockService{}, NewSSEBroker(), "secret", tmux.DefaultSessionName)
	for _, path := range []string{
		"/api/assistant",
		"/api/assistant/threads",
		"/api/assistant/threads/t1/activity",
	} {
		res := get(t, srv, path)
		assert.Equal(t, http.StatusUnauthorized, res.statusCode, "path %s", path)
	}
	res := request(t, srv, http.MethodPost, "/api/assistant/gate/ask", `{"thread":"t1"}`)
	assert.Equal(t, http.StatusUnauthorized, res.statusCode)
}

// request drives any method, which the table above needs and the get/post
// helpers do not cover between them.
func request(t *testing.T, srv *Server, method, path, body string) httpResult {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://%s%s", srv.Addr(), path), reader)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return httpResult{
		statusCode:  resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		etag:        resp.Header.Get("ETag"),
		body:        string(raw),
	}
}
