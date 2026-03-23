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
)

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
