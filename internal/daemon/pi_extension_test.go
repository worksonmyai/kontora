package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Rendering ───────────────────────────────────────────────────────────────

func TestRenderPiExtension(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		out := renderPiExtension(0, false)
		assert.Contains(t, out, "const THRESHOLD = 0;")
		assert.Contains(t, out, "const ENABLED = false;")
		assert.NotContains(t, out, "__CHECKPOINT_THRESHOLD__")
		assert.NotContains(t, out, "__CHECKPOINT_ENABLED__")
		// Must still register agent_settled for shutdown.
		assert.Contains(t, out, "agent_settled")
	})

	t.Run("enabled", func(t *testing.T) {
		out := renderPiExtension(150000, true)
		assert.Contains(t, out, "const THRESHOLD = 150000;")
		assert.Contains(t, out, "const ENABLED = true;")
		assert.Contains(t, out, "kontora_phase_complete")
		assert.Contains(t, out, "agent_settled")
		assert.Contains(t, out, "turn_end")
	})
}

// ── Temp file write ─────────────────────────────────────────────────────────

func TestWritePiExtension(t *testing.T) {
	path, err := writePiExtension(150000, true)
	require.NoError(t, err)
	defer os.Remove(path)

	assert.True(t, strings.HasSuffix(path, ".js"), "extension file should have .js suffix: %s", path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "agent_settled")
	assert.Contains(t, content, "ctx.shutdown()")
	assert.Contains(t, content, "const THRESHOLD = 150000;")
	assert.Contains(t, content, "const ENABLED = true;")
}

func TestWritePiExtensionDisabled(t *testing.T) {
	path, err := writePiExtension(0, false)
	require.NoError(t, err)
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "agent_settled")
	assert.Contains(t, content, "ctx.shutdown()")
	assert.Contains(t, content, "const THRESHOLD = 0;")
	assert.Contains(t, content, "const ENABLED = false;")
	// Disabled extension source must not contain the tool registration block
	// being active — the JS guards on ENABLED && THRESHOLD > 0.
}

func TestWritePiExtensionCleanup(t *testing.T) {
	// Verify that the file is a valid temp file that can be removed.
	path, err := writePiExtension(100000, true)
	require.NoError(t, err)
	require.FileExists(t, path)
	require.NoError(t, os.Remove(path))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should be removed")
}

// ── Node state-machine tests ────────────────────────────────────────────────

func TestPiExtensionJS(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	script := filepath.Join(filepath.Dir(thisFile), "testdata", "pi_extension.test.mjs")
	require.FileExists(t, script)

	// The script requires both variants or neither and validates that each
	// source was rendered with the expected threshold and enabled state.
	cmd := exec.Command("node", "--test", script)
	cmd.Env = append(os.Environ(),
		"KONTORA_PI_EXT_ENABLED="+renderPiExtension(150000, true),
		"KONTORA_PI_EXT_DISABLED="+renderPiExtension(0, false),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node state-machine tests failed:\n%s", out)
	}
}
