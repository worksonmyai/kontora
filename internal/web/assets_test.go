package web

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
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
		{"/app.js", "javascript"},
		{"/app.css", "text/css"},
		{"/theme.js", "javascript"},
		{"/tips.js", "javascript"},
		{"/vendor/yaml@2.8.1/yaml.mjs", "javascript"},
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

// TestServer_NoInlineScript keeps the document servable under script-src 'self'.
// Every script the page needs has to be its own file: the CSP carries no nonce,
// because the assets are compressed once at startup and rewriting the document
// per request would give up the ETag.
func TestServer_NoInlineScript(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// "</script>" does not contain the separator, so every piece but the first
	// starts at the attributes of an opening tag.
	pieces := strings.Split(string(body), "<script")
	require.Greater(t, len(pieces), 1, "index.html loads no scripts at all")
	for _, piece := range pieces[1:] {
		attrs, _, _ := strings.Cut(piece, ">")
		assert.Contains(t, attrs, "src=", "index.html carries an inline script element")
	}
}

// TestServer_DevStaticOverlay covers the rest of KONTORA_WEB_DIR: the JS is
// only half of the UI, and a markup or CSS edit has to reach the same reload.
// A name with no file in the working copy keeps serving the embedded one.
func TestServer_DevStaticOverlay(t *testing.T) {
	dir := t.TempDir()
	static := filepath.Join(dir, "internal", "web", "static")
	require.NoError(t, os.MkdirAll(static, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(static, "index.html"),
		[]byte("<html>devStaticMarker_4e21</html>\n"), 0o600))

	t.Setenv(webDirEnv, dir)
	srv := newTestServer(t)

	get := func(path string) string {
		resp, err := http.Get("http://" + srv.Addr() + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}

	assert.Contains(t, get("/"), "devStaticMarker_4e21")
	assert.Contains(t, get("/app.css"), "--", "app.css is not in the working copy, so the embedded one serves")
}

// TestServer_DevBundleRebuilds covers the KONTORA_WEB_DIR path. The asset
// table is built once at startup, so /app.js has to be compiled in the request
// path or a browser reload would keep serving the copy from then.
func TestServer_DevBundleRebuilds(t *testing.T) {
	dir := t.TempDir()
	ui := filepath.Join(dir, "internal", "web", "ui")
	require.NoError(t, os.MkdirAll(ui, 0o755))
	write := func(body string) {
		require.NoError(t, os.WriteFile(filepath.Join(ui, "index.js"), []byte(body), 0o600))
	}

	// Markers no real bundle can contain: an assertion on a word like "first"
	// would pass against the embedded copy and hide the rebuild never
	// happening, which is the one thing this test exists to catch.
	const first, second = "devBundleMarker_c1f0", "devBundleMarker_9ab7"

	write("globalThis.kontora = () => ({ mark: '" + first + "' });\n")
	t.Setenv(webDirEnv, dir)
	srv := newTestServer(t)

	get := func() (int, string) {
		resp, err := http.Get("http://" + srv.Addr() + "/app.js")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}

	status, body := get()
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, first)

	write("globalThis.kontora = () => ({ mark: '" + second + "' });\n")
	status, body = get()
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, second)
	assert.NotContains(t, body, first, "the reply still carries the previous build")

	// A broken edit must not fall back to the last good bundle. The esbuild
	// message goes to the log, not to an unauthenticated response body.
	write("function (\n")
	status, body = get()
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.NotContains(t, body, dir, "the reply leaks the source path it read")
	assert.NotContains(t, body, second)
}
