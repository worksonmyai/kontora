package testutil

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// InitRepo creates a temporary git repository with an initial empty commit.
func InitRepo(t *testing.T) string {
	return InitRepoWithBranch(t, "main")
}

// InitRepoWithBranch creates a temporary git repository on the given branch.
func InitRepoWithBranch(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", branch},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	return dir
}
