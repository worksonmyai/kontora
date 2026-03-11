package cli

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/testutil"
)

func TestCheckRepo(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr string
	}{
		{
			name:  "valid repo with main",
			setup: initTestRepo,
		},
		{
			name: "valid repo with master",
			setup: func(t *testing.T) string {
				return testutil.InitRepoWithBranch(t, "master")
			},
		},
		{
			name: "not a git repo",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantErr: "not a git repository",
		},
		{
			name: "empty repo (no commits)",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				cmd := exec.Command("git", "init", "-b", "main")
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "git init: %s", out)
				return dir
			},
			wantErr: "repository has no commits",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			err := CheckRepo(dir)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
