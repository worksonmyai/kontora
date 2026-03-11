package web

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStaticIndexJS(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	script := filepath.Join(filepath.Dir(thisFile), "testdata", "index_html.test.mjs")
	cmd := exec.Command("node", "--test", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node static JS tests failed:\n%s", out)
	}
}
