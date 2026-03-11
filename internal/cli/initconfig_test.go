package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestInitConfig_WritesValidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "kontora", "config.yaml")

	// Inject a test double that bypasses the TUI and writes canned answers
	runSetupFn = func(path string, w io.Writer) error {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		return writeSetupConfig(path, &SetupAnswers{
			Agents:              map[string]agentArgs{"claude": {Binary: "claude"}},
			TicketsDir:          filepath.Join(tmpDir, "tickets"),
			LogsDir:             filepath.Join(tmpDir, "logs"),
			WorktreesDir:        filepath.Join(tmpDir, "worktrees"),
			MaxConcurrentAgents: 3,
			WebEnabled:          true,
			WebPort:             8080,
		}, w)
	}
	t.Cleanup(func() { runSetupFn = RunSetup })

	var buf bytes.Buffer
	require.NoError(t, InitConfig(configPath, &buf))

	// Verify the config was written and parses.
	cfg, err := config.Load(configPath)
	require.NoError(t, err)
	assert.Contains(t, cfg.Agents, cfg.DefaultAgent)
	assert.Contains(t, cfg.Pipelines, "default")
	assert.Contains(t, cfg.Pipelines, "implement-review-commit")

	out := buf.String()
	assert.Contains(t, out, "Config written to")
}

func TestInitConfig_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "kontora", "config.yaml")

	// Pre-create the config file.
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte("# existing"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, InitConfig(configPath, &buf))

	// Should report existing and not overwrite.
	assert.Contains(t, buf.String(), "already exists")

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, "# existing", string(data))
}
