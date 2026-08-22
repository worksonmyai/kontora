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
		out := renderPiExtension(0, false, "")
		assert.Contains(t, out, "const THRESHOLD = 0;")
		assert.Contains(t, out, "const ENABLED = false;")
		assert.NotContains(t, out, "__CHECKPOINT_THRESHOLD__")
		assert.NotContains(t, out, "__CHECKPOINT_ENABLED__")
		// Must still register agent_settled for shutdown.
		assert.Contains(t, out, "agent_settled")
	})

	t.Run("enabled", func(t *testing.T) {
		out := renderPiExtension(150000, true, "")
		assert.Contains(t, out, "const THRESHOLD = 150000;")
		assert.Contains(t, out, "const ENABLED = true;")
		assert.Contains(t, out, "kontora_phase_complete")
		assert.Contains(t, out, "agent_settled")
		assert.Contains(t, out, "turn_end")
	})

	t.Run("wait marker", func(t *testing.T) {
		cases := []struct {
			name     string
			waitFile string
			want     string
		}{
			{name: "absent", waitFile: "", want: `const WAIT_MARKER = "";`},
			{
				name:     "plain path",
				waitFile: "/logs/kon-1/implement.waiting.json",
				want:     `const WAIT_MARKER = "/logs/kon-1/implement.waiting.json";`,
			},
			{
				// A path is arbitrary bytes: concatenating one would end the
				// literal and leave a source file pi cannot parse.
				name:     "quotes and backslashes",
				waitFile: `/tmp/a"b\c/implement.waiting.json`,
				want:     `const WAIT_MARKER = "/tmp/a\"b\\c/implement.waiting.json";`,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out := renderPiExtension(0, false, tc.waitFile)
				assert.NotContains(t, out, "__WAIT_MARKER_PATH__")
				assert.Contains(t, out, tc.want)
				// The handlers are in the source whatever the path; the
				// if (WAIT_MARKER) gate decides whether they register.
				assert.Contains(t, out, `pi.on("tool_execution_start"`)
				assert.Contains(t, out, `pi.on("tool_execution_end"`)
				assert.Contains(t, out, `pi.on("session_shutdown"`)
				assert.Contains(t, out, "ask_user_question")
			})
		}
	})
}

// ── Temp file write ─────────────────────────────────────────────────────────

func TestWritePiExtension(t *testing.T) {
	path, err := writePiExtension(150000, true, "/logs/kon-1/implement.waiting.json")
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
	assert.Contains(t, content, `const WAIT_MARKER = "/logs/kon-1/implement.waiting.json";`)
}

func TestWritePiExtensionDisabled(t *testing.T) {
	path, err := writePiExtension(0, false, "")
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
	path, err := writePiExtension(100000, true, "")
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
	// The waiting-marker tests render their own source with a temp path, so the
	// two variants the Go side pins carry no marker.
	cmd := exec.Command("node", "--test", script)
	cmd.Env = append(os.Environ(),
		"KONTORA_PI_EXT_ENABLED="+renderPiExtension(150000, true, ""),
		"KONTORA_PI_EXT_DISABLED="+renderPiExtension(0, false, ""),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node state-machine tests failed:\n%s", out)
	}
}
