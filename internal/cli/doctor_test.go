package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

// noTicketsDirEnv keeps a developer's exported TICKETS_DIR out of Doctor, which
// folds the environment over the config the way every CLI verb does.
func noTicketsDirEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.TicketsDirEnvVar, "")
	t.Setenv(config.LegacyTicketsDirEnvVar, "")
}

func writeValidConfig(t *testing.T, dir string) string {
	t.Helper()
	configPath := filepath.Join(dir, "config.yaml")
	content := `agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause
`
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))
	return configPath
}

func TestDoctor_ValidConfig(t *testing.T) {
	noTicketsDirEnv(t)
	dir := t.TempDir()
	configPath := writeValidConfig(t, dir)

	var buf bytes.Buffer
	err := Doctor(configPath, &buf)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Config")
	assert.Contains(t, out, "git")
	assert.Contains(t, out, "tmux")
	assert.Contains(t, out, "All checks passed")
}

func TestDoctor_ConfigMissing(t *testing.T) {
	var buf bytes.Buffer
	err := Doctor("/nonexistent/path/config.yaml", &buf)
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "Config")
	assert.Contains(t, out, "Some checks failed")
}

func TestDoctor_ConfigInvalid(t *testing.T) {
	noTicketsDirEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("invalid: {{{"), 0o644))

	var buf bytes.Buffer
	err := Doctor(configPath, &buf)
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "Config")
	assert.Contains(t, out, "Some checks failed")
}

func TestDoctor_DirMissing(t *testing.T) {
	noTicketsDirEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := fmt.Sprintf(`tickets_dir: %s/nonexistent/tickets
logs_dir: %s/nonexistent/logs

agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause
`, dir, dir)
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

	var buf bytes.Buffer
	err := Doctor(configPath, &buf)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "will be auto-created")
}

func TestDoctor_AgentBinaryMissing(t *testing.T) {
	noTicketsDirEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := `agents:
  myagent:
    binary: nonexistent-binary-abc123

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: myagent
      on_success: done
      on_failure: pause
`
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

	var buf bytes.Buffer
	err := Doctor(configPath, &buf)
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "nonexistent-binary-abc123")
	assert.Contains(t, out, "Some checks failed")
}

func TestDoctor_WebPortBound(t *testing.T) {
	noTicketsDirEnv(t)
	// Bind a port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := fmt.Sprintf(`web:
  enabled: true
  host: 127.0.0.1
  port: %d

agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause
`, port)
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

	var buf bytes.Buffer
	err = Doctor(configPath, &buf)
	// Port bound is a warning, not a failure.
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Web port")
	assert.Contains(t, out, "not available")
}

func TestDoctor_ProjectPaths(t *testing.T) {
	noTicketsDirEnv(t)
	repo := initTestRepo(t)
	notARepo := t.TempDir()

	cases := []struct {
		name     string
		path     string
		wantFail bool
		want     string
	}{
		{name: "a git repository passes", path: repo, want: repo},
		{name: "a directory that is not a repository fails", path: notARepo, wantFail: true, want: "not a git repository"},
		{name: "a missing path fails", path: filepath.Join(notARepo, "gone"), wantFail: true, want: "does not exist"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			content := fmt.Sprintf(`agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause

projects:
  demo:
    path: %s
`, tc.path)
			require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

			var buf bytes.Buffer
			err := Doctor(configPath, &buf)
			if tc.wantFail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Contains(t, buf.String(), tc.want)
		})
	}
}

func TestDoctor_WarningsAreCounted(t *testing.T) {
	noTicketsDirEnv(t)
	// A missing directory is a warning, so the closing line must not claim a
	// clean bill of health.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := fmt.Sprintf(`tickets_dir: %s/nonexistent/tickets

agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause
`, dir)
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

	var buf bytes.Buffer
	require.NoError(t, Doctor(configPath, &buf))
	assert.Regexp(t, `All checks passed, with \d+ warning`, buf.String())
}

func TestDoctor_ReportsTicketsDirSource(t *testing.T) {
	envDir := t.TempDir()

	cases := []struct {
		name       string
		env        string
		wantSource bool
	}{
		{name: "config value stands", env: ""},
		{name: "environment overrides the config", env: envDir, wantSource: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.TicketsDirEnvVar, tc.env)
			t.Setenv(config.LegacyTicketsDirEnvVar, "")

			configPath := writeValidConfig(t, t.TempDir())

			var buf bytes.Buffer
			require.NoError(t, Doctor(configPath, &buf))
			out := buf.String()

			if !tc.wantSource {
				assert.NotContains(t, out, config.TicketsDirEnvVar)
				return
			}
			assert.Contains(t, out, envDir)
			assert.Contains(t, out, config.TicketsDirEnvVar)
			assert.Contains(t, out, "Tickets dir conflict")
			assert.Regexp(t, `All checks passed, with \d+ warning`, out)
		})
	}
}
