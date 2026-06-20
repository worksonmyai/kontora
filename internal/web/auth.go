package web

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// tokenCookieName is the cookie the browser UI uses to authenticate. The
// browser cannot set an Authorization header on EventSource/WebSocket, so it
// bootstraps a same-origin cookie via /?token=... instead.
const tokenCookieName = "kontora_token"

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

// tokenMatches compares the configured and presented tokens in constant time.
func tokenMatches(want, got string) bool {
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
