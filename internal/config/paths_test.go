package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigDirs_XDGConfigHome(t *testing.T) {
	cases := []struct {
		name string
		xdg  string
		want string
	}{
		{
			name: "XDG_CONFIG_HOME is respected",
			xdg:  "/custom/config",
			want: "/custom/config",
		},
		{
			name: "falls back to ~/.config when unset",
			xdg:  "",
			want: filepath.Join(mustHomeDir(t), ".config"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.xdg != "" {
				t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			} else {
				t.Setenv("XDG_CONFIG_HOME", "")
			}
			dirs := configDirs()
			require.Len(t, dirs, 1)
			assert.Equal(t, tc.want, dirs[0])
		})
	}
}

func TestDefaultConfigPath_UsesXDGConfigHome(t *testing.T) {
	xdgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgDir)

	configDir := filepath.Join(xdgDir, "kontora")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("# test"), 0o644))

	// Use a workdir with no local .kontora so DefaultConfigPath falls through to configDirs.
	t.Chdir(t.TempDir())

	got := DefaultConfigPath()
	assert.Equal(t, filepath.Join(xdgDir, "kontora", "config.yaml"), got)
}

func mustHomeDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	return home
}

func TestDefaultConfigPath_HonoursEnvVar(t *testing.T) {
	// The daemon exports this to its agents, so a `kontora note` run inside a
	// worktree must prefer it over anything under the working directory.
	dir := t.TempDir()
	local := filepath.Join(dir, ".kontora", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o755))
	require.NoError(t, os.WriteFile(local, []byte("# local"), 0o644))

	named := filepath.Join(t.TempDir(), "elsewhere.yaml")
	t.Setenv(PathEnvVar, named)
	t.Chdir(dir)

	assert.Equal(t, named, DefaultConfigPath())
}
