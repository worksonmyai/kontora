package web

import (
	"os"
	"testing"
)

func TestStartupJS(t *testing.T) {
	cmd := nodeTest(t, "startup.test.mjs")
	cmd.Env = append(os.Environ(), "KONTORA_BUNDLE="+writeBundle(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node startup tests failed:\n%s", out)
	}
}
