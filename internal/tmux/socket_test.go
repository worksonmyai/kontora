package tmux

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSocketPathResponses(t *testing.T) {
	dir, err := os.MkdirTemp("", "kt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "socket with spaces")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "tmux"), []byte(`#!/bin/sh
[ "$#" = 6 ] && [ "$1" = -N ] && [ "$2" = display-message ] && [ "$3" = -p ] && [ "$4" = -t ] && [ "$5" = '=session:' ] && [ "$6" = '#{session_name}:#{socket_path}' ] || exit 99
printf '%s' "$TEST_TMUX_OUTPUT"
exit "$TEST_TMUX_CODE"
`), 0o755))
	t.Setenv("PATH", binDir)
	for _, tt := range []struct {
		name   string
		output string
		code   string
		want   string
	}{
		{name: "actual socket", output: "session:" + socket + "\n", code: "0", want: socket},
		{name: "empty response", code: "0"},
		{name: "empty path", output: "session:\n", code: "0"},
		{name: "relative", output: "session:relative/socket\n", code: "0"},
		{name: "unexpanded format", output: "session:#{socket_path}\n", code: "0"},
		{name: "multiple lines", output: "session:" + socket + "\n/other\n", code: "0"},
		{name: "missing file", output: "session:" + filepath.Join(dir, "missing") + "\n", code: "0"},
		{name: "regular file", output: "session:" + file + "\n", code: "0"},
		{name: "directory", output: "session:" + dir + "\n", code: "0"},
		{name: "wrong session", output: "session-long:" + socket + "\n", code: "0"},
		{name: "missing session context", output: ":" + socket + "\n", code: "0"},
		{name: "failed command with valid output", output: "session:" + socket + "\n", code: "1"},
		{name: "no server", output: "no server running\n", code: "1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_TMUX_OUTPUT", tt.output)
			t.Setenv("TEST_TMUX_CODE", tt.code)
			got, err := SocketPath(context.Background(), "session")
			assert.Equal(t, tt.want, got)
			if tt.want == "" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := SocketPath(ctx, "session")
		require.Error(t, err)
		assert.Empty(t, got)
	})
	t.Run("empty session", func(t *testing.T) {
		got, err := SocketPath(context.Background(), "")
		require.Error(t, err)
		assert.Empty(t, got)
	})
	t.Run("missing binary", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		got, err := SocketPath(context.Background(), "session")
		require.Error(t, err)
		assert.Empty(t, got)
	})
}

func TestSocketPathIsolatedServer(t *testing.T) {
	skipIfNoTmux(t)
	// TestMain isolates the whole package. This test gets another private
	// default socket so it can prove discovery never starts an absent server.
	dir, err := os.MkdirTemp("", "kt")
	require.NoError(t, err)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", dir)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-N", "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := SocketPath(ctx, "session")
	require.Error(t, err)
	assert.Empty(t, got)
	// Even a short-lived server normally leaves a socket behind.
	sockets, err := filepath.Glob(filepath.Join(dir, "tmux-*", "*"))
	require.NoError(t, err)
	assert.Empty(t, sockets, "discovery must not start a tmux server")

	out, err := exec.Command("tmux", "-f", "/dev/null", "new-session", "-d", "-s", "session-long", "--", "sleep", "30").CombinedOutput()
	require.NoError(t, err, "%s", out)
	got, err = SocketPath(ctx, "session")
	require.Error(t, err, "a prefix match must not advertise another session's socket")
	assert.Empty(t, got)

	got, err = SocketPath(ctx, "session-long")
	require.NoError(t, err)
	expected, err := filepath.Glob(filepath.Join(dir, "tmux-*", "default"))
	require.NoError(t, err)
	require.Len(t, expected, 1)
	resolved, err := filepath.EvalSymlinks(expected[0])
	require.NoError(t, err)
	assert.Equal(t, resolved, got)
}
