package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStaticIndexJS(t *testing.T) {
	cmd := nodeTest(t, "index_html.test.mjs")
	cmd.Env = append(os.Environ(), "KONTORA_BUNDLE="+writeBundle(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node static JS tests failed:\n%s", out)
	}
}

// TestUIMixinKeys is its own node run because index_html.test.mjs drives the
// merged component, and this one has to see the mixins before they are merged.
func TestUIMixinKeys(t *testing.T) {
	if out, err := nodeTest(t, "ui_mixins.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("node mixin tests failed:\n%s", out)
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
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
