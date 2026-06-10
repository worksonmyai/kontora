package web

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServer_VendoredAssets verifies the self-hosted front-end assets are
// embedded and served with the right content types. The .mjs check matters:
// app.js loads xterm via dynamic import(), and browsers reject ES modules that
// aren't served with a JavaScript MIME type.
func TestServer_VendoredAssets(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		path     string
		wantType string
	}{
		{"/app.css", "text/css"},
		{"/vendor/alpinejs@3.14.8/cdn.min.js", "javascript"},
		{"/vendor/sortablejs@1.15.6/Sortable.min.js", "javascript"},
		{"/vendor/marked@15.0.7/marked.min.js", "javascript"},
		{"/vendor/dompurify@3.3.2/purify.min.js", "javascript"},
		{"/vendor/xterm@5.5.0/xterm.css", "text/css"},
		{"/vendor/xterm@5.5.0/xterm.mjs", "javascript"},
		{"/vendor/addon-fit@0.10.0/addon-fit.mjs", "javascript"},
		{"/vendor/addon-unicode11@0.8.0/addon-unicode11.mjs", "javascript"},
		{"/vendor/addon-webgl@0.18.0/addon-webgl.mjs", "javascript"},
		{"/vendor/fonts/fonts.css", "text/css"},
		{"/vendor/fonts/dm-sans-latin.woff2", "font/woff2"},
		{"/vendor/fonts/jetbrains-mono-latin.woff2", "font/woff2"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get("http://" + srv.Addr() + tc.path)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, resp.Header.Get("Content-Type"), tc.wantType)
		})
	}
}

// TestServer_NoExternalAssets guards against reintroducing runtime CDN
// dependencies: the page must load entirely from the embedded file server.
func TestServer_NoExternalAssets(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	for _, host := range []string{"cdn.tailwindcss.com", "cdn.jsdelivr.net", "fonts.googleapis.com", "fonts.gstatic.com"} {
		assert.False(t, strings.Contains(html, host), "index.html still references external host %q", host)
	}
}
