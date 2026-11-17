package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestPTYLogSink(t *testing.T) {
	assert.NotContains(t, renderUsage(), "pty-log-sink")
	for _, tc := range []struct {
		name     string
		existing int64
		input    string
		wantSize int64
	}{
		{name: "ordinary capture", input: "output\r\n", wantSize: 8},
		{name: "append to cap", existing: (16 << 20) - 4, input: "123456789", wantSize: 16 << 20},
		{name: "already oversized", existing: 45_778_216_576, input: "discard", wantSize: 16 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "raw '雪 $(printf bad).log")
			if tc.existing > 0 {
				f, err := os.Create(path)
				require.NoError(t, err)
				require.NoError(t, f.Truncate(tc.existing))
				require.NoError(t, f.Close())
			}
			out, diagnostics, err := runCLISeparated(t, []string{
				"KONTORA_URL=http://127.0.0.1:1",
				config.PathEnvVar + "=" + filepath.Join(t.TempDir(), "missing-config.yaml"),
			}, strings.NewReader(tc.input), "pty-log-sink", path)
			require.NoError(t, err, diagnostics)
			assert.Empty(t, out)
			assert.Empty(t, diagnostics)
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSize, info.Size())
			if tc.existing == 0 {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, tc.input, string(data))
			}
		})
	}
}

func TestPTYLogSinkFailureDrains(t *testing.T) {
	input := strings.NewReader(strings.Repeat("output", 1<<18))
	path := filepath.Join(t.TempDir(), "missing", "raw.log")
	out, diagnostics, err := runCLISeparated(t, nil, input, "pty-log-sink", path)
	require.Error(t, err)
	assert.Empty(t, out)
	assert.Contains(t, diagnostics, "raw PTY log:")
	assert.Zero(t, input.Len(), "file errors must not close the pipe before draining")
}

func TestPTYLogSinkRequiresPath(t *testing.T) {
	out, diagnostics, err := runCLISeparated(t, nil, nil, "pty-log-sink")
	require.Error(t, err)
	assert.Empty(t, out)
	assert.Contains(t, diagnostics, "usage: kontora pty-log-sink LOG_PATH")
}
