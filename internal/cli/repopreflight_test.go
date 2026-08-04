package cli

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/testutil"
)

func TestCheckRepo(t *testing.T) {
	// repoWithDevelopAndTag builds a repo on main that also holds a develop
	// branch and a branch-shaped v1.0 tag.
	repoWithDevelopAndTag := func(t *testing.T) string {
		t.Helper()
		dir := testutil.InitRepoWithBranch(t, "main")
		for _, args := range [][]string{{"branch", "develop"}, {"tag", "v1.0"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "git %v: %s", args, out)
		}
		return dir
	}

	cases := []struct {
		name       string
		setup      func(t *testing.T) string
		baseBranch string
		wantErr    string
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
		{
			name:       "existing base branch",
			setup:      repoWithDevelopAndTag,
			baseBranch: "develop",
		},
		{
			name:       "empty base still checks the default branch",
			setup:      repoWithDevelopAndTag,
			baseBranch: "",
		},
		{
			name:       "missing base branch",
			setup:      repoWithDevelopAndTag,
			baseBranch: "devlop",
			wantErr:    `base branch "devlop" not found`,
		},
		{
			name:       "tag is not a branch",
			setup:      repoWithDevelopAndTag,
			baseBranch: "v1.0",
			wantErr:    `base branch "v1.0" not found`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			err := CheckRepo(dir, tc.baseBranch)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
