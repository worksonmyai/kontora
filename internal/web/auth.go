package web

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// tokenCookieName is the cookie the browser UI uses to authenticate. The
// browser cannot set an Authorization header on EventSource/WebSocket, so it
// bootstraps a same-origin cookie via /?token=... instead.
const tokenCookieName = "kontora_token"

// tokenCookieMaxAge is how long the auth cookie persists. A fixed lifetime
// makes it a persistent cookie rather than a session cookie, so the user does
// not have to re-enter the token every time the browser restarts.
const tokenCookieMaxAge = 30 * 24 * time.Hour

// authMiddleware gates /api/ and /ws/ routes with a shared bearer token when
// one is configured. With an empty token it is a pass-through, preserving the
// daemon's original open-access behavior. GET /health and the static UI under
// GET / stay reachable without a token.
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requiresAuth(r.URL.Path) && !tokenMatches(token, extractToken(r)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requiresAuth reports whether a request path is behind the token gate.
func requiresAuth(path string) bool {
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/ws/")
}

// extractToken pulls the presented token from, in order: the Authorization
// Bearer header (CLI), the kontora_token cookie (browser), or a token query
// parameter (SSE/WebSocket bootstrap where headers cannot be set).
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return after
		}
	}
	if c, err := r.Cookie(tokenCookieName); err == nil {
		return c.Value
	}
	return r.URL.Query().Get("token")
}

// isHTTPS reports whether the browser's connection to us is HTTPS. It is true
// for a direct TLS connection and also when a TLS-terminating proxy (Tailscale
// Serve, nginx, Caddy) forwards plain HTTP with X-Forwarded-Proto: https. We
// only use this to decide the cookie's Secure flag, so trusting the header is
// safe: a spoofed value can at worst mark a client's own cookie Secure.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// tokenMatches compares the configured and presented tokens in constant time.
func tokenMatches(want, got string) bool {
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
