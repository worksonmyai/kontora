package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAttemptPath = "/api/boards/board-1/attempts/attempt-1"

var testAttemptScope = AttemptScope{BoardID: "board-1", WorkID: "work-1", AttemptID: "attempt-1"}

func testAuthorityConfig() *AuthorityConfig {
	return &AuthorityConfig{
		Operators: []OperatorCredential{{ID: "alice", Token: "operator-secret"}},
		Attempts: []AttemptCapability{
			{Token: "reader-secret", Scope: testAttemptScope, Access: AttemptRead, ExpiresAt: time.Now().Add(time.Hour)},
			{Token: "worker-secret", Scope: testAttemptScope, Access: AttemptPropose, ExpiresAt: time.Now().Add(time.Hour)},
		},
		AttemptActive: func(_ context.Context, scope AttemptScope) bool { return scope == testAttemptScope },
	}
}

func TestAuthorityAttemptRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, token string
		workID                    string
		want                      int
	}{
		{name: "read own attempt", method: "GET", path: testAttemptPath, token: "reader-secret", want: 204},
		{name: "read feedback attempt", method: "GET", path: testAttemptPath, token: "worker-secret", workID: "feedback/request", want: 204},
		{name: "propose feedback", method: "POST", path: testAttemptPath + "/proposals", token: "worker-secret", workID: "feedback/request", want: 204},
		{name: "feedback cannot address another attempt", method: "GET", path: strings.Replace(testAttemptPath, "attempt-1", "attempt-2", 1), token: "worker-secret", workID: "feedback/request", want: 403},
		{name: "read own logs", method: "GET", path: testAttemptPath + "/logs", token: "reader-secret", want: 204},
		{name: "read own activity", method: "GET", path: testAttemptPath + "/activity", token: "reader-secret", want: 204},
		{name: "proposer can read", method: "GET", path: testAttemptPath, token: "worker-secret", want: 204},
		{name: "propose", method: "POST", path: testAttemptPath + "/proposals", token: "worker-secret", want: 204},
		{name: "reader cannot propose", method: "POST", path: testAttemptPath + "/proposals", token: "reader-secret", want: 403},
		{name: "wrong attempt", method: "GET", path: strings.Replace(testAttemptPath, "attempt-1", "attempt-2", 1), token: "worker-secret", want: 403},
		{name: "wrong board", method: "GET", path: strings.Replace(testAttemptPath, "board-1", "board-2", 1), token: "worker-secret", want: 403},
		{name: "board listing", method: "GET", path: "/api/boards/board-1", token: "worker-secret", want: 403},
		{name: "head is not granted", method: "HEAD", path: testAttemptPath, token: "worker-secret", want: 403},
		{name: "get proposals is not granted", method: "GET", path: testAttemptPath + "/proposals", token: "worker-secret", want: 403},
		{name: "put proposals is not granted", method: "PUT", path: testAttemptPath + "/proposals", token: "worker-secret", want: 403},
		{name: "delete attempt", method: "DELETE", path: testAttemptPath, token: "worker-secret", want: 403},
		{name: "approve", method: "POST", path: testAttemptPath + "/approve", token: "worker-secret", want: 403},
		{name: "accept", method: "POST", path: testAttemptPath + "/accept", token: "worker-secret", want: 403},
		{name: "unknown read", method: "GET", path: testAttemptPath + "/secrets", token: "worker-secret", want: 403},
		{name: "extra path", method: "POST", path: testAttemptPath + "/proposals/accept", token: "worker-secret", want: 403},
		{name: "trailing slash", method: "GET", path: testAttemptPath + "/", token: "worker-secret", want: 403},
		{name: "encoded slash", method: "GET", path: testAttemptPath + "%2flogs%2faccept", token: "worker-secret", want: 403},
		{name: "encoded board separator", method: "GET", path: "/api/boards/board-1%2Fattempts%2Fattempt-1", token: "worker-secret", want: 403},
		{name: "encoded api separator", method: "GET", path: "/api%2Fboards/board-1/attempts/attempt-1", token: "worker-secret", want: 403},
		{name: "dot path", method: "GET", path: testAttemptPath + "/../attempt-2", token: "worker-secret", want: 403},
		{name: "missing credential", method: "GET", path: testAttemptPath, want: 401},
		{name: "wrong credential length", method: "GET", path: testAttemptPath, token: "worker", want: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			cfg := testAuthorityConfig()
			wantScope := testAttemptScope
			if tc.workID != "" {
				wantScope.WorkID = tc.workID
				cfg.Attempts[1].Scope = wantScope
				cfg.AttemptActive = func(_ context.Context, scope AttemptScope) bool { return scope == wantScope }
			}
			handler, err := AuthorityMiddleware("shared-secret", cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				_, operator := OperatorIdentity(r.Context())
				assert.False(t, operator)
				scope, ok := AttemptIdentity(r.Context())
				assert.True(t, ok)
				assert.Equal(t, wantScope, scope)
				w.WriteHeader(http.StatusNoContent)
			}))
			require.NoError(t, err)
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"operator_id":"alice","work_id":"other"}`))
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			r.Header.Set("X-Kontora-Operator-ID", "alice")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, tc.want, w.Code, w.Body.String())
			assert.Equal(t, tc.want == 204, called)
		})
	}
}

func TestAuthorityOperatorTransport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{name: "operator header", mutate: func(r *http.Request) { r.Header.Set(OperatorTokenHeader, "operator-secret") }, want: 204},
		{name: "operator with shared bearer", mutate: func(r *http.Request) {
			r.Header.Set(OperatorTokenHeader, "operator-secret")
			r.Header.Set("Authorization", "Bearer shared-secret")
		}, want: 204},
		{name: "shared bearer is not operator", mutate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer shared-secret") }, want: 401},
		{name: "shared cookie is not operator", mutate: func(r *http.Request) { r.AddCookie(&http.Cookie{Name: tokenCookieName, Value: "shared-secret"}) }, want: 401},
		{name: "operator in shared bearer rejected", mutate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer operator-secret") }, want: 401},
		{name: "operator cookie rejected", mutate: func(r *http.Request) { r.AddCookie(&http.Cookie{Name: tokenCookieName, Value: "operator-secret"}) }, want: 401},
		{name: "operator query rejected", mutate: func(r *http.Request) { r.URL.RawQuery = "token=operator-secret&operator_id=alice" }, want: 401},
		{name: "forwarded identity rejected", mutate: func(r *http.Request) { r.Header.Set("X-Forwarded-User", "alice") }, want: 401},
		{name: "body identity rejected", want: 401},
		{name: "worker rejected", mutate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer worker-secret") }, want: 403},
		{name: "worker bearer prefix required", mutate: func(r *http.Request) { r.Header.Set("Authorization", "worker-secret") }, want: 401},
		{name: "worker in operator header rejected", mutate: func(r *http.Request) { r.Header.Set(OperatorTokenHeader, "worker-secret") }, want: 401},
		{name: "bad operator does not fall back", mutate: func(r *http.Request) {
			r.Header.Set(OperatorTokenHeader, "wrong")
			r.Header.Set("Authorization", "Bearer worker-secret")
		}, want: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler, err := AuthorityMiddleware("shared-secret", testAuthorityConfig(), RequireOperator(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				id, ok := OperatorIdentity(r.Context())
				assert.True(t, ok)
				assert.Equal(t, "alice", id)
				_, worker := AttemptIdentity(r.Context())
				assert.False(t, worker)
				w.WriteHeader(http.StatusNoContent)
			})))
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/api/boards/board-1/accept", strings.NewReader(`{"operator_id":"mallory"}`))
			if tc.mutate != nil {
				tc.mutate(r)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, tc.want, w.Code, w.Body.String())
			assert.Equal(t, tc.want == 204, called)
		})
	}
}

func TestAuthorityOperatorRoutesRejectWorkers(t *testing.T) {
	// Workers must not bypass authority by moving or editing the backing ticket.
	routes := []string{
		"GET /api/tickets", "POST /api/tickets", "POST /api/tickets/upload",
		"GET /api/stats", "GET /api/config", "GET /api/config/raw", "PUT /api/config/raw",
		"GET /api/tickets/t", "GET /api/tickets/archived", "DELETE /api/tickets/t", "PUT /api/tickets/t",
		"GET /api/tickets/t/cost", "GET /api/tickets/t/logs", "GET /api/tickets/t/activity",
		"GET /api/tickets/t/changes", "GET /api/tickets/t/chain", "GET /api/events",
		"PATCH /api/tickets/t/notes/n", "DELETE /api/tickets/t/notes/n",
		"POST /api/tickets/t/notes/n/reactions", "DELETE /api/tickets/t/notes/n/reactions/ok",
		"PUT /api/tickets/t/children", "GET /ws/terminal/t", "GET /ws/terminal/t?rw=1",
		"GET /api/assistant", "GET /api/assistant/threads", "POST /api/assistant/threads",
		"GET /api/assistant/threads/t", "DELETE /api/assistant/threads/t",
		"GET /api/assistant/threads/t/activity", "GET /api/assistant/threads/t/stream",
		"POST /api/assistant/threads/t/messages", "POST /api/assistant/threads/t/stop",
		"POST /api/assistant/gate/ask", "POST /api/assistant/gate/g",
		"POST /api/boards/b/actions/a/approve", "POST /api/boards/b/objectives/o/accept",
		"GET /api/objectives", "GET /api/objectives/o", "POST /api/objectives", "POST /api/objectives/o/controls",
		"POST /api/future-operator-endpoint",
	}
	for _, action := range []string{"pause", "retry", "run", "skip", "set-stage", "schedule", "move", "archive", "restore", "note", "summary", "init", "dep", "undep", "link", "unlink", "parent", "unparent", "plannotator-review", "plannotator-annotate"} {
		routes = append(routes, "POST /api/tickets/t/"+action)
	}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			handler, err := AuthorityMiddleware("shared-secret", testAuthorityConfig(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			require.NoError(t, err)
			method, path, _ := strings.Cut(route, " ")
			for _, token := range []string{"reader-secret", "worker-secret"} {
				r := httptest.NewRequest(method, path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				assert.Equal(t, http.StatusForbidden, w.Code, "%s: %s", token, w.Body.String())
			}
			r := httptest.NewRequest(method, path, nil)
			r.Header.Set(OperatorTokenHeader, "operator-secret")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		})
	}
}

func TestRequireOperatorRejectsAuthenticatedAttempt(t *testing.T) {
	called := false
	handler, err := AuthorityMiddleware("", testAuthorityConfig(), RequireOperator(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})))
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, testAttemptPath, nil)
	r.Header.Set("Authorization", "Bearer worker-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, called)
}

func TestAuthorityRevocationAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expired bool
		revoked bool
	}{
		{name: "expired", expired: true},
		{name: "revoked", revoked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testAuthorityConfig()
			active := true
			cfg.AttemptActive = func(_ context.Context, scope AttemptScope) bool {
				assert.Equal(t, testAttemptScope, scope)
				return active
			}
			if tc.expired {
				cfg.Attempts[1].ExpiresAt = time.Now().Add(-time.Second)
			}
			handler, err := AuthorityMiddleware("", cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			require.NoError(t, err)
			request := func() int {
				r := httptest.NewRequest(http.MethodPost, testAttemptPath+"/proposals", nil)
				r.Header.Set("Authorization", "Bearer worker-secret")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w.Code
			}
			if tc.revoked {
				require.Equal(t, 204, request())
				active = false
			}
			assert.Equal(t, http.StatusUnauthorized, request())
		})
	}
}

func TestAuthorityConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*AuthorityConfig)
	}{
		{name: "empty operator token", mutate: func(c *AuthorityConfig) { c.Operators[0].Token = "" }},
		{name: "empty operator identity", mutate: func(c *AuthorityConfig) { c.Operators[0].ID = " " }},
		{name: "duplicate operator identity", mutate: func(c *AuthorityConfig) {
			c.Operators = append(c.Operators, OperatorCredential{ID: "alice", Token: "different"})
		}},
		{name: "shared token is not an operator credential", mutate: func(c *AuthorityConfig) { c.Operators[0].Token = "shared-secret" }},
		{name: "shared token is not a capability", mutate: func(c *AuthorityConfig) { c.Attempts[0].Token = "shared-secret" }},
		{name: "operator token is not a capability", mutate: func(c *AuthorityConfig) { c.Attempts[0].Token = "operator-secret" }},
		{name: "duplicate capability token", mutate: func(c *AuthorityConfig) { c.Attempts[0].Token = c.Attempts[1].Token }},
		{name: "empty capability token", mutate: func(c *AuthorityConfig) { c.Attempts[0].Token = "" }},
		{name: "no current-state validator", mutate: func(c *AuthorityConfig) { c.AttemptActive = nil }},
		{name: "no expiry", mutate: func(c *AuthorityConfig) { c.Attempts[0].ExpiresAt = time.Time{} }},
		{name: "no board", mutate: func(c *AuthorityConfig) { c.Attempts[0].Scope.BoardID = "" }},
		{name: "no work", mutate: func(c *AuthorityConfig) { c.Attempts[0].Scope.WorkID = "" }},
		{name: "blank work", mutate: func(c *AuthorityConfig) { c.Attempts[0].Scope.WorkID = " \t" }},
		{name: "path in board", mutate: func(c *AuthorityConfig) { c.Attempts[0].Scope.BoardID = "a/b" }},
		{name: "no attempt", mutate: func(c *AuthorityConfig) { c.Attempts[0].Scope.AttemptID = "" }},
		{name: "path in identity", mutate: func(c *AuthorityConfig) { c.Attempts[0].Scope.AttemptID = "a/b" }},
		{name: "dot identity", mutate: func(c *AuthorityConfig) { c.Attempts[0].Scope.BoardID = ".." }},
		{name: "no permission", mutate: func(c *AuthorityConfig) { c.Attempts[0].Access = "" }},
		{name: "approval permission rejected", mutate: func(c *AuthorityConfig) { c.Attempts[0].Access = "approve" }},
		{name: "acceptance permission rejected", mutate: func(c *AuthorityConfig) { c.Attempts[0].Access = "accept" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testAuthorityConfig()
			tc.mutate(cfg)
			handler, err := AuthorityMiddleware("shared-secret", cfg, http.NotFoundHandler())
			require.Error(t, err)
			assert.Nil(t, handler)
			assert.NotContains(t, err.Error(), "shared-secret")
			assert.NotContains(t, err.Error(), "operator-secret")
		})
	}
}

func TestAuthoritySnapshotsCredentials(t *testing.T) {
	cfg := testAuthorityConfig()
	handler, err := AuthorityMiddleware("", cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	require.NoError(t, err)
	cfg.Operators[0].Token = "worker-secret"
	cfg.Attempts[1].Scope.AttemptID = "other"
	cfg.Attempts[1].Access = AttemptRead
	cfg.AttemptActive = nil
	r := httptest.NewRequest(http.MethodPost, testAttemptPath+"/proposals", nil)
	r.Header.Set("Authorization", "Bearer worker-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, 204, w.Code)
}

func TestAuthorityOrdinaryCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, shared, credential, path string
		cfg                            *AuthorityConfig
		want                           int
	}{
		{name: "ordinary open raw config", path: "/api/config/raw", want: 204},
		{name: "ordinary open writable terminal", path: "/ws/terminal/t?rw=1", want: 204},
		{name: "ordinary shared token", shared: "shared", credential: "shared", path: "/api/config/raw", want: 204},
		{name: "ordinary shared writable terminal", shared: "shared", credential: "shared", path: "/ws/terminal/t?rw=1", want: 204},
		{name: "ordinary token required", shared: "shared", path: "/api/config/raw", want: 401},
		{name: "enabled empty config fails closed", cfg: &AuthorityConfig{}, path: "/api/config/raw", want: 401},
		{name: "health stays public", cfg: testAuthorityConfig(), path: "/health", want: 204},
		{name: "static stays public", cfg: testAuthorityConfig(), path: "/", want: 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := AuthorityMiddleware(tc.shared, tc.cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, operator := OperatorIdentity(r.Context())
				assert.False(t, operator)
				w.WriteHeader(http.StatusNoContent)
			}))
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.credential != "" {
				r.AddCookie(&http.Cookie{Name: tokenCookieName, Value: tc.credential})
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, tc.want, w.Code)
		})
	}
}

func TestAuthorityRawConfigAndTerminalHandlers(t *testing.T) {
	s := &Server{svc: &mockService{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config/raw", s.handleGetRawConfig)
	mux.HandleFunc("PUT /api/config/raw", s.handlePutRawConfig)
	mux.HandleFunc("GET /ws/terminal/{id}", s.handleTerminalWS)
	handler, err := AuthorityMiddleware("shared-secret", testAuthorityConfig(), mux)
	require.NoError(t, err)
	for _, tc := range []struct {
		method, path, token string
		operator            bool
		want                int
	}{
		{method: "GET", path: "/api/config/raw", token: "worker-secret", want: 403},
		{method: "PUT", path: "/api/config/raw", token: "worker-secret", want: 403},
		{method: "GET", path: "/ws/terminal/t", token: "worker-secret", want: 403},
		{method: "GET", path: "/ws/terminal/t?rw=1", token: "worker-secret", want: 403},
		{method: "GET", path: "/api/config/raw", token: "shared-secret", want: 401},
		{method: "GET", path: "/api/config/raw", token: "operator-secret", operator: true, want: 200},
		{method: "PUT", path: "/api/config/raw", token: "operator-secret", operator: true, want: 204},
		// A missing terminal proves the request reached the real handler, without
		// starting tmux or upgrading a WebSocket in this transport test.
		{method: "GET", path: "/ws/terminal/t?rw=1", token: "operator-secret", operator: true, want: 404},
	} {
		t.Run(tc.method+tc.path+tc.token, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"content":"example"}`))
			if tc.operator {
				r.Header.Set(OperatorTokenHeader, tc.token)
			} else {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, tc.want, w.Code, w.Body.String())
		})
	}
}
