package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveThroughSecurity runs one request through the security layer and reports
// the response plus whether the wrapped handler ran.
func serveThroughSecurity(allowedHosts []string, r *http.Request) (*httptest.ResponseRecorder, bool) {
	reached := false
	h := securityMiddleware(allowedHosts, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, reached
}

func TestSecurityMiddleware_HostAndOrigin(t *testing.T) {
	tests := []struct {
		name         string
		host         string
		allowedHosts []string
		headers      map[string]string
		method       string
		path         string
		wantStatus   int
		wantBody     string
	}{
		{
			name:       "loopback host allowed",
			host:       "127.0.0.1:8080",
			wantStatus: http.StatusOK,
		},
		{
			name:       "localhost allowed",
			host:       "localhost:8080",
			wantStatus: http.StatusOK,
		},
		{
			name:       "ipv6 loopback without port allowed",
			host:       "[::1]",
			wantStatus: http.StatusOK,
		},
		{
			name:       "rebound hostname refused",
			host:       "evil.com",
			wantStatus: http.StatusForbidden,
			wantBody:   "web.allowed_hosts",
		},
		{
			name:         "allowlisted tailnet host allowed",
			host:         "kontora.tailnet.ts.net",
			allowedHosts: []string{"kontora.tailnet.ts.net"},
			wantStatus:   http.StatusOK,
		},
		{
			name:         "allowlist match is case insensitive",
			host:         "Kontora.Tailnet.TS.net:9090",
			allowedHosts: []string{"kontora.tailnet.ts.net"},
			wantStatus:   http.StatusOK,
		},
		{
			name:       "matching origin allowed",
			host:       "127.0.0.1:8080",
			headers:    map[string]string{"Origin": "http://127.0.0.1:8080"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "origin port mismatch refused",
			host:       "127.0.0.1:8080",
			headers:    map[string]string{"Origin": "http://127.0.0.1:9999"},
			wantStatus: http.StatusForbidden,
			wantBody:   "cross-origin",
		},
		{
			name:       "foreign origin refused",
			host:       "127.0.0.1:8080",
			headers:    map[string]string{"Origin": "http://evil.com:8080"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "null origin refused",
			host:       "127.0.0.1:8080",
			headers:    map[string]string{"Origin": "null"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "origin with unknown scheme refused",
			host:       "127.0.0.1:8080",
			headers:    map[string]string{"Origin": "chrome-extension://abcd"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "default http port filled in on both sides",
			host:       "localhost",
			headers:    map[string]string{"Origin": "http://localhost"},
			wantStatus: http.StatusOK,
		},
		{
			name:         "forwarded proto trusted for an allowlisted host",
			host:         "kontora.tailnet.ts.net",
			allowedHosts: []string{"kontora.tailnet.ts.net"},
			headers: map[string]string{
				"X-Forwarded-Proto": "https",
				"Origin":            "https://kontora.tailnet.ts.net",
			},
			wantStatus: http.StatusOK,
		},
		{
			name:         "https proxy that forwards no proto still accepts its own origin",
			host:         "kontora.example",
			allowedHosts: []string{"kontora.example"},
			headers:      map[string]string{"Origin": "https://kontora.example"},
			wantStatus:   http.StatusOK,
		},
		{
			name:         "http proxy that forwards no proto still accepts its own origin",
			host:         "kontora.example",
			allowedHosts: []string{"kontora.example"},
			headers:      map[string]string{"Origin": "http://kontora.example"},
			wantStatus:   http.StatusOK,
		},
		{
			name:         "non-default origin port refused when the request port is unknown",
			host:         "kontora.example",
			allowedHosts: []string{"kontora.example"},
			headers:      map[string]string{"Origin": "https://kontora.example:9999"},
			wantStatus:   http.StatusForbidden,
			wantBody:     "cross-origin",
		},
		{
			name: "forwarded proto ignored for a loopback host",
			host: "127.0.0.1",
			headers: map[string]string{
				"X-Forwarded-Proto": "https",
				"Origin":            "https://127.0.0.1",
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "same-origin fetch allowed",
			host:       "127.0.0.1:8080",
			headers:    map[string]string{"Sec-Fetch-Site": "same-origin"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "sec-fetch-site none allowed",
			host:       "127.0.0.1:8080",
			headers:    map[string]string{"Sec-Fetch-Site": "none"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "cross-site write refused",
			host:       "127.0.0.1:8080",
			method:     http.MethodPost,
			path:       "/api/tickets/abc/run",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusForbidden,
			wantBody:   "cross-site",
		},
		{
			name:       "same-site subresource refused",
			host:       "127.0.0.1:8080",
			headers:    map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Dest": "image"},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "cross-site top-level navigation allowed",
			host: "127.0.0.1:8080",
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site",
				"Sec-Fetch-Mode": "navigate",
				"Sec-Fetch-Dest": "document",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "cross-site framed navigation refused",
			host: "127.0.0.1:8080",
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site",
				"Sec-Fetch-Mode": "navigate",
				"Sec-Fetch-Dest": "iframe",
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "client sending neither header allowed",
			host:       "127.0.0.1:8080",
			method:     http.MethodPost,
			path:       "/api/tickets/abc/run",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			path := tt.path
			if path == "" {
				path = "/api/tickets"
			}
			req := httptest.NewRequest(method, path, nil)
			req.Host = tt.host
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			rec, reached := serveThroughSecurity(tt.allowedHosts, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantStatus == http.StatusOK, reached)
			if tt.wantBody != "" {
				assert.Contains(t, rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestSecurityMiddleware_MediaType(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		wantStatus  int
	}{
		{
			name:        "json write accepted",
			method:      http.MethodPost,
			path:        "/api/tickets",
			contentType: "application/json",
			body:        `{"title":"x"}`,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "json with charset accepted",
			method:      http.MethodPut,
			path:        "/api/config/raw",
			contentType: "application/json; charset=utf-8",
			body:        `{}`,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "form encoded write refused",
			method:      http.MethodPost,
			path:        "/api/tickets",
			contentType: "application/x-www-form-urlencoded",
			body:        "title=x",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "text write refused",
			method:      http.MethodPost,
			path:        "/api/tickets",
			contentType: "text/plain",
			body:        "x",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:       "bodyless write accepted",
			method:     http.MethodPost,
			path:       "/api/tickets/abc/run",
			wantStatus: http.StatusOK,
		},
		{
			name:        "multipart accepted on the upload route",
			method:      http.MethodPost,
			path:        uploadPath,
			contentType: `multipart/form-data; boundary=x`,
			body:        "--x--",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "multipart refused elsewhere",
			method:      http.MethodPost,
			path:        "/api/tickets",
			contentType: `multipart/form-data; boundary=x`,
			body:        "--x--",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "get with a body is not a write",
			method:      http.MethodGet,
			path:        "/api/tickets",
			contentType: "text/plain",
			body:        "x",
			wantStatus:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Host = "127.0.0.1:8080"
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			rec, _ := serveThroughSecurity(nil, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
		})
	}
}

// Every refusal has to carry the {"error": ...} shape: the CLI's decodeError
// and the UI both read that field, and a text/plain body leaves them showing a
// bare status code instead of what to fix.
func TestSecurityMiddleware_RefusalsAreJSON(t *testing.T) {
	tests := []struct {
		name       string
		req        func() *http.Request
		wantStatus int
		wantError  string
	}{
		{
			name: "refused host",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
				r.Host = "evil.com"
				return r
			},
			wantStatus: http.StatusForbidden,
			wantError:  "web.allowed_hosts",
		},
		{
			name: "refused media type",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader("title=x"))
				r.Host = "127.0.0.1:8080"
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return r
			},
			wantStatus: http.StatusUnsupportedMediaType,
			wantError:  "application/json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, _ := serveThroughSecurity(nil, tt.req())

			require.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

			var body struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Contains(t, body.Error, tt.wantError)
		})
	}
}

func TestResolveAllowedHosts(t *testing.T) {
	tests := []struct {
		name       string
		bindHost   string
		configured []string
		wantHas    []string
		wantLacks  []string
	}{
		{
			name:      "wildcard bind is not an allowed host",
			bindHost:  "0.0.0.0",
			wantLacks: []string{"0.0.0.0"},
		},
		{
			name:      "ipv6 wildcard bind is not an allowed host",
			bindHost:  "::",
			wantLacks: []string{"::"},
		},
		{
			name:     "concrete bind address is allowed",
			bindHost: "100.64.0.7",
			wantHas:  []string{"100.64.0.7"},
		},
		{
			name:       "configured hosts are normalized",
			bindHost:   "127.0.0.1",
			configured: []string{" Kontora.Tailnet.TS.net:8080 ", "kontora.tailnet.ts.net"},
			wantHas:    []string{"kontora.tailnet.ts.net"},
			wantLacks:  []string{"kontora.tailnet.ts.net:8080"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveAllowedHosts(tt.bindHost, tt.configured)
			for _, h := range tt.wantHas {
				assert.Contains(t, got, h)
			}
			for _, h := range tt.wantLacks {
				assert.NotContains(t, got, h)
			}
			// The hostname is always there, so a request under the machine's
			// own name works without any config.
			assert.NotEmpty(t, got)
		})
	}
}

func TestUnspecifiedHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{host: "0.0.0.0", want: true},
		{host: "::", want: true},
		{host: "127.0.0.1"},
		{host: "localhost"},
		{host: "100.64.0.7"},
		{host: ""},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.want, UnspecifiedHost(tt.host))
		})
	}
}

func TestSecurityHeaders_OnEveryResponse(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name    string
		path    string
		wantCSP string
	}{
		{name: "api response", path: "/api/tickets", wantCSP: baselineCSP},
		{name: "error response", path: "/api/tickets/missing/logs", wantCSP: baselineCSP},
		{name: "document", path: "/", wantCSP: documentCSP},
		{name: "script asset", path: "/app.js", wantCSP: baselineCSP},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get("http://" + srv.Addr() + tt.path)
			require.NoError(t, err)
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)

			assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
			assert.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
			assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
			assert.Equal(t, "same-origin", resp.Header.Get("Cross-Origin-Resource-Policy"))
			assert.Equal(t, tt.wantCSP, resp.Header.Get("Content-Security-Policy"))
		})
	}
}

func TestDocumentCSP_ScriptSrcHasNoUnsafeInline(t *testing.T) {
	var scriptSrc string
	for directive := range strings.SplitSeq(documentCSP, ";") {
		if strings.HasPrefix(strings.TrimSpace(directive), "script-src ") {
			scriptSrc = strings.TrimSpace(directive)
		}
	}
	require.NotEmpty(t, scriptSrc, "the document policy must name script-src")
	assert.NotContains(t, scriptSrc, "'unsafe-inline'")
	assert.Equal(t, "script-src 'self' 'unsafe-eval'", scriptSrc)
	assert.Contains(t, documentCSP, "default-src 'self'")
}
