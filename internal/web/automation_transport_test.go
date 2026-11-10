package web

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/worksonmyai/kontora/internal/automation"
)

var automationProtectedRoutes = []struct{ method, path, body string }{
	{"GET", "/api/automations", ""},
	{"POST", "/api/automations", `{"request_id":"r1","definition":{}}`},
	{"POST", "/api/automations/preview", `{"frequency":"daily","time":"09:00","timezone":"UTC"}`},
	{"GET", "/api/automations/a1", ""},
	{"PUT", "/api/automations/a1", `{"revision":7}`},
	{"GET", "/api/automations/a1/occurrences", ""},
	{"POST", "/api/automations/a1/occurrences/o1/skip", `{"revision":1}`},
}

func TestAutomationSharedTokenProtection(t *testing.T) {
	svc := &automationMockService{definitions: []automation.Definition{automationFixture()}}
	srv := automationTestServer(t, svc, "secret")
	for _, route := range automationProtectedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			for _, tc := range []struct {
				name, query string
				headers     map[string]string
				allowed     bool
			}{
				{name: "missing"},
				{name: "wrong bearer", headers: map[string]string{"Authorization": "Bearer wrong"}},
				{name: "wrong cookie", headers: map[string]string{"Cookie": "kontora_token=wrong"}},
				{name: "bearer", headers: map[string]string{"Authorization": "Bearer secret"}, allowed: true},
				{name: "cookie", headers: map[string]string{"Cookie": "kontora_token=secret"}, allowed: true},
				{name: "query", query: "?token=secret", allowed: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					svc.calls = nil
					w := automationRequest(srv, route.method, route.path+tc.query, route.body, tc.headers)
					if tc.allowed {
						assert.Less(t, w.Code, 300, w.Body.String())
					} else {
						assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
						assert.Empty(t, svc.calls)
					}
				})
			}
		})
	}
}

func TestAutomationHostAndOriginProtection(t *testing.T) {
	svc := &automationMockService{definitions: []automation.Definition{automationFixture()}}
	srv := automationTestServer(t, svc, "secret")
	for _, route := range automationProtectedRoutes {
		for _, tc := range []struct {
			name    string
			headers map[string]string
			want    int
		}{
			{"foreign host", map[string]string{"Host": "evil.example"}, 403},
			{"foreign origin", map[string]string{"Origin": "https://evil.example"}, 403},
			{"different port", map[string]string{"Origin": "http://127.0.0.1:8081"}, 403},
			{"null origin", map[string]string{"Origin": "null"}, 403},
			{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
			{"same-site fetch", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
			{"same origin", map[string]string{"Origin": "http://127.0.0.1:8080"}, 0},
			{"no origin", nil, 0},
		} {
			t.Run(route.method+" "+route.path+"/"+tc.name, func(t *testing.T) {
				headers := map[string]string{"Authorization": "Bearer secret"}
				for key, value := range tc.headers {
					headers[key] = value
				}
				svc.calls = nil
				w := automationRequest(srv, route.method, route.path, route.body, headers)
				if tc.want == 0 {
					assert.Less(t, w.Code, 300, w.Body.String())
				} else {
					assert.Equal(t, tc.want, w.Code, w.Body.String())
					assert.Empty(t, svc.calls)
				}
			})
		}
	}
}

func TestAutomationMediaTypeAndUnsupportedActions(t *testing.T) {
	svc := &automationMockService{definitions: []automation.Definition{automationFixture()}}
	srv := automationTestServer(t, svc, "")
	for _, route := range automationProtectedRoutes {
		if route.method == http.MethodGet {
			continue
		}
		for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=test"} {
			svc.calls = nil
			w := automationRequest(srv, route.method, route.path, route.body, map[string]string{"Content-Type": contentType})
			assert.Equal(t, http.StatusUnsupportedMediaType, w.Code, w.Body.String())
			assert.Empty(t, svc.calls)
		}
	}
	for _, route := range []struct{ method, path string }{
		{"DELETE", "/api/automations/a1"},
		{"POST", "/api/automations/a1/run"},
		{"POST", "/api/automations/a1/run-now"},
	} {
		svc.calls = nil
		w := automationRequest(srv, route.method, route.path, `{}`, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, w.Body.String())
		assert.Empty(t, svc.calls)
	}
}

func TestAutomationAuthorityProtection(t *testing.T) {
	svc := &automationMockService{definitions: []automation.Definition{automationFixture()}}
	srv := automationTestServer(t, svc, "shared-secret", WithAuthority(*testAuthorityConfig()))
	for _, route := range automationProtectedRoutes {
		for _, tc := range []struct {
			name    string
			headers map[string]string
			want    int
		}{
			{"missing", nil, 401},
			{"shared bearer", map[string]string{"Authorization": "Bearer shared-secret"}, 401},
			{"shared cookie", map[string]string{"Cookie": "kontora_token=shared-secret"}, 401},
			{"operator bearer", map[string]string{"Authorization": "Bearer operator-secret"}, 401},
			{"reader", map[string]string{"Authorization": "Bearer reader-secret"}, 403},
			{"worker", map[string]string{"Authorization": "Bearer worker-secret"}, 403},
			{"operator", map[string]string{OperatorTokenHeader: "operator-secret"}, 0},
		} {
			t.Run(route.method+" "+route.path+"/"+tc.name, func(t *testing.T) {
				svc.calls = nil
				w := automationRequest(srv, route.method, route.path, route.body, tc.headers)
				if tc.want == 0 {
					assert.Less(t, w.Code, 300, w.Body.String())
				} else {
					assert.Equal(t, tc.want, w.Code, w.Body.String())
					assert.Empty(t, svc.calls)
				}
			})
		}
	}
}
