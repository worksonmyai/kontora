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
	"slices"
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

func TestDialogsJS(t *testing.T) {
	cmd := nodeTest(t, "dialogs.test.mjs")
	cmd.Env = append(os.Environ(), "KONTORA_BUNDLE="+writeBundle(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node dialog tests failed:\n%s", out)
	}
}

func TestSettingsJS(t *testing.T) {
	bundle := writeBundle(t)
	for _, name := range []string{"settings.test.mjs", "settings_picker.test.mjs", "settings_routes.test.mjs", "automations.test.mjs", "graph.test.mjs", "workflow_layout.test.mjs", "wait_projection.test.mjs", "review_15_deployment_checkout_regression.test.mjs", "detail_view.test.mjs", "lifecycle.test.mjs", "run_confirm.test.mjs", "board_review.test.mjs", "dependency_refresh.test.mjs", "terminal.test.mjs", "evidence_data.test.mjs", "epic_management.test.mjs", "creation_navigation.test.mjs", "assistant_presentation.test.mjs", "prose_preferences.test.mjs", "assistant_ownership.test.mjs", "schedule_validation.test.mjs"} {
		t.Run(name, func(t *testing.T) {
			cmd := nodeTest(t, name)
			cmd.Env = append(os.Environ(), "KONTORA_BUNDLE="+bundle)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("node settings tests failed:\n%s", out)
			}
		})
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
	assets := serveBrowserAssets(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	for _, engine := range []string{"chromium", "webkit"} {
		t.Run(engine, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", "--test",
				filepath.Join(filepath.Dir(thisFile), "testdata", "mobile_browser.test.mjs"),
				filepath.Join(filepath.Dir(thisFile), "testdata", "tooltips_browser.test.mjs"))
			cmd.Env = append(cmd.Environ(), assets.env(engine)...)
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
}

type browserAssets struct {
	url      string
	manifest string
}

func (a browserAssets) env(engine string) []string {
	return []string{"KONTORA_BROWSER_URL=" + a.url, "KONTORA_BROWSER_ASSETS=" + a.manifest, "KONTORA_BROWSER_ENGINE=" + engine}
}

func serveBrowserAssets(t *testing.T) browserAssets {
	t.Helper()
	// Never use the developer overlay, a daemon, or a ticket store. This loopback
	// server serves embedded assets and one fixed authentication redirect.
	t.Setenv("KONTORA_WEB_DIR", "")
	assets, err := buildAssets()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{assets: assets, svc: &mockService{}}
	paths := []string{"/"}
	for name := range assets {
		paths = append(paths, "/"+name)
	}
	manifest, err := json.Marshal(paths)
	if err != nil {
		t.Fatal(err)
	}
	var escaped browserRequestLog
	server := httptest.NewServer(browserAssetHandler(s, &escaped))
	t.Cleanup(func() {
		server.Close()
		if requests := escaped.snapshot(); len(requests) > 0 {
			t.Errorf("requests escaped browser interception: %v", requests)
		}
	})
	return browserAssets{url: server.URL, manifest: string(manifest)}
}

type browserRequestLog struct {
	mu       sync.Mutex
	requests []string
}

func (l *browserRequestLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, r.Method+" "+r.URL.RequestURI())
}

func (l *browserRequestLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.requests...)
}

const browserEscapedRequestsPath = "/__visual/escaped-requests"

func browserAssetHandler(s *Server, escaped *browserRequestLog) http.Handler {
	secured := securityMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == browserEscapedRequestsPath && r.Header.Get("X-Kontora-Visual-Diagnostics") == "1" {
			writeJSON(w, http.StatusOK, escaped.snapshot())
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.RawQuery == "token=fixture-token" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.serveAsset(w, r)
	}))
	deny := securityMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "browser fixture interception missed a request", http.StatusForbidden)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		query := r.URL.Query()
		query.Del("v")
		if r.Method == http.MethodGet && r.Header.Get("Upgrade") == "" && ((s.assets[name] != nil && len(query) == 0) ||
			(r.URL.Path == "/" && r.URL.RawQuery == "token=fixture-token") ||
			(r.URL.Path == browserEscapedRequestsPath && r.Header.Get("X-Kontora-Visual-Diagnostics") == "1")) {
			secured.ServeHTTP(w, r)
			return
		}
		// Security middleware can reject an escaped request before it reaches the asset handler.
		escaped.add(r)
		deny.ServeHTTP(w, r)
	})
}

func TestBrowserAssetInterception(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, origin, mediaType string
		wantStatus                            int
	}{
		{"unknown asset", http.MethodGet, "/not-an-asset.js", "", "", http.StatusForbidden},
		{"unknown API rejected by origin check", http.MethodPost, "/api/unexpected", "https://example.test", "", http.StatusForbidden},
		{"unknown API rejected by media type check", http.MethodPost, "/api/unexpected", "", "text/plain", http.StatusUnsupportedMediaType},
		{"diagnostic path without header", http.MethodGet, browserEscapedRequestsPath, "", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var escaped browserRequestLog
			handler := browserAssetHandler(&Server{}, &escaped)
			req := httptest.NewRequest(tc.method, "http://127.0.0.1"+tc.path, strings.NewReader("synthetic request"))
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", tc.mediaType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d; want %d", response.Code, tc.wantStatus)
			}

			req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+browserEscapedRequestsPath, nil)
			req.Header.Set("X-Kontora-Visual-Diagnostics", "1")
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			var got []string
			if err := json.Unmarshal(response.Body.Bytes(), &got); response.Code != http.StatusOK || err != nil {
				t.Fatalf("reading diagnostics: status %d, body %q, error %v", response.Code, response.Body, err)
			}
			if want := []string{tc.method + " " + tc.path}; !slices.Equal(got, want) {
				t.Fatalf("escaped requests = %q; want %q", got, want)
			}
		})
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
