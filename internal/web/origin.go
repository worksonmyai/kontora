package web

import (
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

// uploadPath is the one route that takes a multipart body.
const uploadPath = "/api/tickets/upload"

// baselineCSP is what every response carries unless it overrides it. Nothing
// under /api/ is a document, so it needs to load nothing at all.
const baselineCSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'"

// documentCSP applies to index.html only.
//
// 'unsafe-eval' is there because Alpine's evaluator builds functions from
// expression strings; dropping it means moving to @alpinejs/csp and rewriting
// every x-* binding into a component method. style-src keeps 'unsafe-inline'
// because the document carries inline style attributes, which no nonce or hash
// covers. Neither concession weakens script-src: an injected <script> and an
// external script both stay blocked.
const documentCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// resolveAllowedHosts folds the configured allowlist together with the hosts
// this daemon answers to by definition: the bind address when it names one
// interface, and the machine's own hostname. Loopback is not listed because
// LoopbackHost covers every spelling of it.
func resolveAllowedHosts(bindHost string, configured []string) []string {
	var hosts []string
	add := func(h string) {
		h, _ = splitAuthority(strings.TrimSpace(h))
		if h = strings.ToLower(h); h != "" && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	for _, h := range configured {
		add(h)
	}
	if !UnspecifiedHost(bindHost) {
		add(bindHost)
	}
	if name, err := os.Hostname(); err == nil {
		add(name)
	}
	return hosts
}

// securityMiddleware is the outermost layer: it stamps the baseline headers,
// refuses requests that a browser on another site could have forged, and holds
// writes to a media type that cannot be sent cross-site without a preflight.
// It runs ahead of authMiddleware so a rejected request never reaches a
// handler, and it stands on its own — none of it needs a token or a cookie.
func securityMiddleware(allowedHosts []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set before delegating: gzipResponseWriter defers WriteHeader, so a
		// header written after next.ServeHTTP returns never reaches the client.
		securityHeaders(w.Header())

		if err := checkRequestOrigin(r, allowedHosts); err != nil {
			writeJSONError(w, http.StatusForbidden, err.Error())
			return
		}
		if writeWithUnsupportedMediaType(r) {
			writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported media type: writes with a body must be application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(header http.Header) {
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Content-Security-Policy", baselineCSP)
}

func checkRequestOrigin(r *http.Request, allowedHosts []string) error {
	host, port := splitAuthority(r.Host)
	hostAllowed := slices.Contains(allowedHosts, strings.ToLower(host))
	if !LoopbackHost(host) && !hostAllowed {
		// Host keeps the page's hostname after DNS rebinding points it at
		// loopback, so this check must run before any handler reads a ticket.
		return fmt.Errorf("refused: Host %q is not this machine; add it to web.allowed_hosts to allow it", r.Host)
	}
	if port == "" {
		port = defaultPortForRequest(r, hostAllowed)
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameLocalOrigin(origin, port, allowedHosts) {
		return fmt.Errorf("refused: cross-origin request from %q", origin)
	}
	return checkFetchMetadata(r)
}

// splitAuthority is net.SplitHostPort with the portless case handled, which is
// where it fails on a bracketed IPv6 literal.
func splitAuthority(authority string) (string, string) {
	host, port, err := net.SplitHostPort(authority)
	if err == nil {
		return host, port
	}
	return strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]"), ""
}

// UnspecifiedHost reports whether a bind address names every interface. Such
// an address is not a name any client can send in Host, so it cannot be an
// allowed host; every other name the daemon is then reached by has to be
// listed in web.allowed_hosts.
func UnspecifiedHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// LoopbackHost reports whether a hostname or address names this machine over
// the loopback interface.
func LoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

// sameLocalOrigin reports whether origin names a host this daemon answers to,
// on the port the request arrived on. An empty port means that port is unknown
// (see defaultPortForRequest), and then only an origin on its own scheme's
// default port matches.
func sameLocalOrigin(origin, port string, allowedHosts []string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	defaultPort := defaultPortForScheme(u.Scheme)
	if defaultPort == "" {
		return false
	}
	host, originPort := splitAuthority(u.Host)
	if !LoopbackHost(host) && !slices.Contains(allowedHosts, strings.ToLower(host)) {
		return false
	}
	if originPort == "" {
		originPort = defaultPort
	}
	if port == "" {
		return originPort == defaultPort
	}
	return originPort == port
}

// defaultPortForRequest reports the port a request arrived on when its Host
// header leaves the port out. A direct connection tells us its scheme, but
// behind a reverse proxy that rewrote Host, only X-Forwarded-Proto does, and
// nginx does not send it unless configured to. Guessing "80" there would refuse
// every write from an https page, so an absent header reports "" — unknown.
func defaultPortForRequest(r *http.Request, proxied bool) string {
	if r.TLS != nil {
		return "443"
	}
	if !proxied {
		return "80"
	}
	// An allowlisted host is how a local reverse proxy opts in. The proxy must
	// replace this client-controlled header, not append.
	if proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); proto != "" {
		return defaultPortForScheme(strings.TrimSpace(proto))
	}
	return ""
}

func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func checkFetchMetadata(r *http.Request) error {
	site := r.Header.Get("Sec-Fetch-Site")
	switch site {
	case "", "same-origin", "none":
		return nil
	}

	// A linking site cannot read a top-level tab, so following a link to the UI
	// stays allowed. Frames and subresources keep the response inside the
	// linking site's page and remain blocked.
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.Header.Get("Sec-Fetch-Dest") == "document" {
		return nil
	}
	return fmt.Errorf("refused: cross-site request (Sec-Fetch-Site: %s)", site)
}

// writeWithUnsupportedMediaType reports whether a write carries a body a
// browser could have posted from another site without a preflight.
//
// A bodyless write is exempt: the CLI sets Content-Type only when it sends a
// body, so run/pause/retry/skip and the plannotator routes arrive without one.
// Those stay covered by the Origin and Sec-Fetch-Site checks above.
func writeWithUnsupportedMediaType(r *http.Request) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		return false
	}
	header := r.Header.Get("Content-Type")
	if header == "" && r.ContentLength == 0 {
		return false
	}
	return !mediaTypeAccepted(r.URL.Path, header)
}

func mediaTypeAccepted(path, header string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	switch mediaType {
	case "application/json":
		return true
	case "multipart/form-data":
		return path == uploadPath
	default:
		return false
	}
}

// wsOriginPatterns turns the allowlist into coder/websocket patterns so the
// library's own origin check agrees with checkRequestOrigin instead of relying
// on its default, which compares the Origin host to r.Host exactly and so
// fails behind a proxy on another hostname. Patterns go through path.Match,
// where a bracketed IPv6 literal reads as a character class, so those are left
// out; the library's exact r.Host comparison still covers them.
func wsOriginPatterns(allowedHosts []string) []string {
	patterns := []string{"localhost", "localhost:*", "127.0.0.1", "127.0.0.1:*"}
	for _, h := range allowedHosts {
		if strings.ContainsAny(h, "[]*?\\") {
			continue
		}
		patterns = append(patterns, h, h+":*")
	}
	return patterns
}
