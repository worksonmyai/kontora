package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStaticIndexJS(t *testing.T) {
	cmd := nodeTest(t, "index_html.test.mjs")
	cmd.Env = append(os.Environ(), "KONTORA_BUNDLE="+writeBundle(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node static JS tests failed:\n%s", out)
	}
}

func TestStatsOutcomesJS(t *testing.T) {
	if out, err := nodeTest(t, "stats_outcomes.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("node stats outcome tests failed:\n%s", out)
	}
}

// TestUIMixinKeys is its own node run because index_html.test.mjs drives the
// merged component, and this one has to see the mixins before they are merged.
func TestUIMixinKeys(t *testing.T) {
	if out, err := nodeTest(t, "ui_mixins.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("node mixin tests failed:\n%s", out)
	}
}

// TestMobileBrowserJS is deliberately opt-in: ordinary Go tests and builds
// need neither npm nor a downloaded browser. See make test-browser-install and
// make test-browser. Once opted in, missing dependencies are failures, not skips.
func TestMobileBrowserJS(t *testing.T) {
	if os.Getenv("KONTORA_TEST_BROWSER") != "1" {
		t.Skip("set KONTORA_TEST_BROWSER=1 to run the Playwright browser suite")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("Browser tests require Node.js 20 or later on PATH. Install Node.js, then run make test-browser-install.")
	}
	// Never use the developer overlay, a daemon, or a ticket store. Only the
	// actual embedded asset handler is reachable on this ephemeral loopback port.
	t.Setenv("KONTORA_WEB_DIR", "")
	assets, err := buildAssets()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{assets: assets}
	paths := []string{"/"}
	for name := range assets {
		paths = append(paths, "/"+name)
	}
	manifest, err := json.Marshal(paths)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var leaked []string
	server := httptest.NewServer(securityMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		query := r.URL.Query()
		query.Del("v")
		if r.Method != http.MethodGet || assets[name] == nil || len(query) > 0 {
			mu.Lock()
			leaked = append(leaked, r.Method+" "+r.URL.String())
			mu.Unlock()
			http.Error(w, "browser fixture interception missed a request", http.StatusForbidden)
			return
		}
		s.serveAsset(w, r)
	})))
	defer server.Close()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	for _, engine := range []string{"chromium", "webkit"} {
		t.Run(engine, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 7*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", "--test", filepath.Join(filepath.Dir(thisFile), "testdata", "mobile_browser.test.mjs"))
			cmd.Env = append(os.Environ(), "KONTORA_BROWSER_URL="+server.URL, "KONTORA_BROWSER_ASSETS="+string(manifest), "KONTORA_BROWSER_ENGINE="+engine)
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				if ctx.Err() != nil {
					t.Fatalf("browser tests stopped (%v):\n%s", ctx.Err(), out)
				}
				t.Fatalf("browser tests failed (%v):\n%s", runErr, out)
			}
			t.Logf("%s", out)
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) > 0 {
		t.Errorf("requests escaped browser interception: %v", leaked)
	}
}

func nodeTest(t *testing.T, name string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return exec.Command("node", "--test", filepath.Join(filepath.Dir(thisFile), "testdata", name))
}

// writeBundle compiles the UI the same way the daemon serves it and writes the
// result where the node suite can read it, so the bundle under test is the one
// the browser gets.
func writeBundle(t *testing.T) string {
	t.Helper()
	bundle, err := embeddedUIBundle()
	if err != nil {
		t.Fatalf("building the UI bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(path, bundle.js, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
