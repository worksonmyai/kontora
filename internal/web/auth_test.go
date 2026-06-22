package web

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startAuthTestServer(t *testing.T, svc TicketService, token string) *Server {
	t.Helper()
	srv := New(svc, NewSSEBroker(), "127.0.0.1", 0, token, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

type authResult struct {
	statusCode int
	body       string
	cookies    []*http.Cookie
}

func doAuthRequest(t *testing.T, srv *Server, path string, mutate func(*http.Request)) authResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+path, nil)
	require.NoError(t, err)
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return authResult{statusCode: resp.StatusCode, body: string(body), cookies: resp.Cookies()}
}

func TestAuth_RejectsWithoutToken(t *testing.T) {
	srv := startAuthTestServer(t, &mockService{}, "secret")

	res := doAuthRequest(t, srv, "/api/tickets", nil)
	assert.Equal(t, http.StatusUnauthorized, res.statusCode)
	assert.NotContains(t, res.body, "tickets")
}

func TestAuth_BearerHeaderAuthorizes(t *testing.T) {
	srv := startAuthTestServer(t, &mockService{}, "secret")

	res := doAuthRequest(t, srv, "/api/tickets", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer secret")
	})
	assert.Equal(t, http.StatusOK, res.statusCode)
	assert.Contains(t, res.body, "tickets")
}

func TestAuth_WrongBearerRejected(t *testing.T) {
	srv := startAuthTestServer(t, &mockService{}, "secret")

	res := doAuthRequest(t, srv, "/api/tickets", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer wrong")
	})
	assert.Equal(t, http.StatusUnauthorized, res.statusCode)
}

func TestAuth_CookieAuthorizes(t *testing.T) {
	srv := startAuthTestServer(t, &mockService{}, "secret")

	res := doAuthRequest(t, srv, "/api/tickets", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: tokenCookieName, Value: "secret"})
	})
	assert.Equal(t, http.StatusOK, res.statusCode)
	assert.Contains(t, res.body, "tickets")
}

func TestAuth_QueryTokenAuthorizes(t *testing.T) {
	srv := startAuthTestServer(t, &mockService{}, "secret")

	res := doAuthRequest(t, srv, "/api/tickets?token=secret", nil)
	assert.Equal(t, http.StatusOK, res.statusCode)
}

func TestAuth_NoTokenConfiguredAllowsOpenAccess(t *testing.T) {
	srv := startAuthTestServer(t, &mockService{}, "")

	res := doAuthRequest(t, srv, "/api/tickets", nil)
	assert.Equal(t, http.StatusOK, res.statusCode)
	assert.Contains(t, res.body, "tickets")
}

func TestAuth_HealthAndStaticPublic(t *testing.T) {
	srv := startAuthTestServer(t, &mockService{}, "secret")

	health := doAuthRequest(t, srv, "/health", nil)
	assert.Equal(t, http.StatusOK, health.statusCode)

	root := doAuthRequest(t, srv, "/", nil)
	assert.Equal(t, http.StatusOK, root.statusCode)

	// Protected /ws/* still requires the token (a plain GET without it is
	// rejected before reaching the websocket handler).
	ws := doAuthRequest(t, srv, "/ws/terminal/tst-001", nil)
	assert.Equal(t, http.StatusUnauthorized, ws.statusCode)
}

func TestAuth_QueryTokenOnStaticSetsCookieAndRedirects(t *testing.T) {
	srv := startAuthTestServer(t, &mockService{}, "secret")

	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/?token=secret", nil)
	require.NoError(t, err)
	// Don't follow the redirect so we can inspect the cookie and Location.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == tokenCookieName && c.Value == "secret" {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "expected kontora_token cookie to be set")
	assert.Equal(t, int(tokenCookieMaxAge.Seconds()), cookie.MaxAge, "cookie should persist for a fixed lifetime, not be a session cookie")
	assert.True(t, cookie.HttpOnly)
	assert.False(t, cookie.Secure, "plain HTTP must not mark the cookie Secure, or the browser drops it")

	loc := resp.Header.Get("Location")
	assert.NotContains(t, loc, "token", "token must be stripped from the redirect target")
}

func TestIsHTTPS(t *testing.T) {
	tests := []struct {
		name           string
		directTLS      bool
		forwardedProto string
		want           bool
	}{
		{name: "plain http", want: false},
		{name: "direct tls", directTLS: true, want: true},
		{name: "proxy https", forwardedProto: "https", want: true},
		{name: "proxy https uppercase", forwardedProto: "HTTPS", want: true},
		{name: "proxy http", forwardedProto: "http", want: false},
		{name: "proxy chain takes first", forwardedProto: "https, http", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.directTLS {
				r.TLS = &tls.ConnectionState{}
			}
			if tt.forwardedProto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.forwardedProto)
			}
			assert.Equal(t, tt.want, isHTTPS(r))
		})
	}
}
