package web

import (
	"os"
	"testing"
)

func TestArchiveCoordinationJS(t *testing.T) {
	cmd := nodeTest(t, "archive_coordination.test.mjs")
	cmd.Env = append(os.Environ(), "KONTORA_BUNDLE="+writeBundle(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node archive coordination tests failed:\n%s", out)
	}
}
