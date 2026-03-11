package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestResolveConfigPath(t *testing.T) {
	cases := []struct {
		name       string
		setupLocal bool
		setupDirs  []int // indices into configDirs to create config files in
		wantIdx    int   // -1 = local, 0+ = index into configDirs
	}{
		{
			name:       "local exists",
			setupLocal: true,
			wantIdx:    -1,
		},
		{
			name:      "first config dir",
			setupDirs: []int{0},
			wantIdx:   0,
		},
		{
			name:      "falls back to second config dir",
			setupDirs: []int{1},
			wantIdx:   1,
		},
		{
			name:      "first dir wins when both exist",
			setupDirs: []int{0, 1},
			wantIdx:   0,
		},
		{
			name:    "nothing found returns first dir path",
			wantIdx: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			configDirs := []string{t.TempDir(), t.TempDir()}

			localPath := filepath.Join(workDir, ".kontora", "config.yaml")

			if tc.setupLocal {
				require.NoError(t, os.MkdirAll(filepath.Dir(localPath), 0o755))
				require.NoError(t, os.WriteFile(localPath, []byte("# local"), 0o644))
			}
			for _, idx := range tc.setupDirs {
				p := filepath.Join(configDirs[idx], "kontora", "config.yaml")
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(t, os.WriteFile(p, []byte("# dir"), 0o644))
			}

			got := config.ResolveConfigPath(workDir, configDirs)

			var expected string
			if tc.wantIdx == -1 {
				expected = localPath
			} else {
				expected = filepath.Join(configDirs[tc.wantIdx], "kontora", "config.yaml")
			}

			assert.Equal(t, expected, got)
		})
	}
}
