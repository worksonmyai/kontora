package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectoryIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	require.NoError(t, os.Mkdir("board", 0o755))
	require.NoError(t, os.Symlink("board", "alias"))
	require.NoError(t, os.MkdirAll("branch/child", 0o755))
	require.NoError(t, os.Mkdir("branch/board", 0o755))
	require.NoError(t, os.Symlink("branch/child", "link"))
	require.NoError(t, os.WriteFile("file", nil, 0o600))
	want, err := filepath.EvalSymlinks(filepath.Join(dir, "board"))
	require.NoError(t, err)
	for _, tc := range []struct {
		path string
		fail bool
	}{
		{filepath.Join(dir, "board"), false},
		{"board", false},
		{"board/../board/", false},
		{"~/board", false},
		{"alias", false},
		{"", true},
		{"missing", true},
		{"file", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got, err := DirectoryIdentity(tc.path)
			if tc.fail {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.True(t, filepath.IsAbs(got))
			same, err := SameDirectory("board", tc.path)
			require.NoError(t, err)
			assert.True(t, same)
		})
	}
	same, err := SameDirectory("board", dir)
	require.NoError(t, err)
	assert.False(t, same)
	for _, path := range []string{"link/../board", dir + "/link/../board", "~/link/../board"} {
		resolved, err := DirectoryIdentity(path)
		require.NoError(t, err)
		want, err := filepath.EvalSymlinks(filepath.Join(dir, "branch/board"))
		require.NoError(t, err)
		assert.Equal(t, want, resolved)
		same, err := SameDirectory(path, "board")
		require.NoError(t, err)
		assert.False(t, same)
	}
	_, err = SameDirectory("missing", "board")
	require.Error(t, err)
	_, err = SameDirectory("board", "missing")
	require.Error(t, err)
}
