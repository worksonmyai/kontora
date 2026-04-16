package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/testutil"
)

func initRepo(t *testing.T) string {
	t.Helper()
	return testutil.InitRepo(t)
}

func initRepoWithBranch(t *testing.T, branch string) string {
	t.Helper()
	return testutil.InitRepoWithBranch(t, branch)
}

func TestBranchName(t *testing.T) {
	tests := []struct {
		prefix string
		taskID string
		want   string
	}{
		{"kontora", "abc-123", "kontora/abc-123"},
		{"custom", "abc-123", "custom/abc-123"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, BranchName(tt.prefix, tt.taskID))
	}
}

func TestPath(t *testing.T) {
	m := New("/tmp/wt")
	got := m.Path("myrepo", "tkt-1")
	assert.Equal(t, filepath.Join("/tmp/wt", "myrepo", "tkt-1"), got)
}

func TestCreateAndRemove(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, created, err := m.Create(repoDir, "myrepo", "tkt-1", "kontora/tkt-1")
	require.NoError(t, err)
	assert.True(t, created)
	_, err = os.Stat(path)
	require.NoError(t, err, "worktree dir does not exist")
	assertBranch(t, path, "kontora/tkt-1")

	path2, created2, err := m.Create(repoDir, "myrepo", "tkt-1", "kontora/tkt-1")
	require.NoError(t, err, "idempotent Create")
	assert.False(t, created2)
	assert.Equal(t, path, path2)

	require.NoError(t, m.Remove(repoDir, "kontora/tkt-1"))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "worktree dir still exists after remove")
	assertBranchExists(t, repoDir, "kontora/tkt-1")
}

func TestCreateAfterRemoveReusesBranch(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, created, err := m.Create(repoDir, "myrepo", "tkt-1", "kontora/tkt-1")
	require.NoError(t, err)
	assert.True(t, created)

	require.NoError(t, m.Remove(repoDir, "kontora/tkt-1"))
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "worktree dir should be gone after remove")

	// Re-create: branch still exists, should reuse it.
	path2, created2, err := m.Create(repoDir, "myrepo", "tkt-1", "kontora/tkt-1")
	require.NoError(t, err)
	assert.True(t, created2)
	assert.Equal(t, path, path2)
	assertBranch(t, path2, "kontora/tkt-1")
}

func TestRemoveNonexistent(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	require.NoError(t, m.Remove(repoDir, "kontora/no-such-tkt"))
}

func TestTwoWorktreesSameRepo(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	p1, _, err := m.Create(repoDir, "myrepo", "tkt-1", "kontora/tkt-1")
	require.NoError(t, err)
	p2, _, err := m.Create(repoDir, "myrepo", "tkt-2", "kontora/tkt-2")
	require.NoError(t, err)

	assert.NotEqual(t, p1, p2)

	assertBranch(t, p1, "kontora/tkt-1")
	assertBranch(t, p2, "kontora/tkt-2")
}

func TestCreateWithCustomBranch(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, _, err := m.Create(repoDir, "myrepo", "tkt-1", "custom/tkt-1")
	require.NoError(t, err)
	assertBranch(t, path, "custom/tkt-1")

	require.NoError(t, m.Remove(repoDir, "custom/tkt-1"))
	assertBranchExists(t, repoDir, "custom/tkt-1")
}

func TestRemoveDirtyWorktree(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, _, err := m.Create(repoDir, "myrepo", "tkt-dirty", "kontora/tkt-dirty")
	require.NoError(t, err)

	// Create an untracked file to make the worktree dirty.
	require.NoError(t, os.WriteFile(filepath.Join(path, "dirty.txt"), []byte("wip"), 0o644))

	err = m.Remove(repoDir, "kontora/tkt-dirty")
	assert.ErrorIs(t, err, ErrDirtyWorktree)

	// Worktree and branch should still exist.
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "worktree dir should still exist")
	assertBranchExists(t, repoDir, "kontora/tkt-dirty")
}

func TestRemoveDirtyWorktreeStaged(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, _, err := m.Create(repoDir, "myrepo", "tkt-staged", "kontora/tkt-staged")
	require.NoError(t, err)

	// Create and stage a file without committing.
	require.NoError(t, os.WriteFile(filepath.Join(path, "staged.txt"), []byte("wip"), 0o644))
	cmd := exec.Command("git", "add", "staged.txt")
	cmd.Dir = path
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git add: %s", out)

	err = m.Remove(repoDir, "kontora/tkt-staged")
	assert.ErrorIs(t, err, ErrDirtyWorktree)
}

func TestFindWorktreeForBranch(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, _, err := m.Create(repoDir, "myrepo", "tkt-1", "feat/stacked")
	require.NoError(t, err)

	found, err := FindWorktreeForBranch(repoDir, "feat/stacked")
	require.NoError(t, err)
	assert.Equal(t, path, found)
}

func TestFindWorktreeForBranchNotFound(t *testing.T) {
	repoDir := initRepo(t)

	found, err := FindWorktreeForBranch(repoDir, "no/such/branch")
	require.NoError(t, err)
	assert.Equal(t, "", found)
}

func TestCreateReusesExistingWorktree(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	// First create establishes the worktree at Path(repoName, "tkt-a").
	pathA, createdA, err := m.Create(repoDir, "myrepo", "tkt-a", "feat/stacked")
	require.NoError(t, err)
	require.True(t, createdA)

	pathBefore := m.Path("myrepo", "tkt-b")
	_, statErr := os.Stat(pathBefore)
	require.True(t, os.IsNotExist(statErr), "tkt-b dir should not yet exist")

	// Second create with a different ticketID but the same branch must reuse pathA.
	pathB, createdB, err := m.Create(repoDir, "myrepo", "tkt-b", "feat/stacked")
	require.NoError(t, err)
	assert.False(t, createdB)
	assert.Equal(t, pathA, pathB)

	// And the tkt-b default location was never created on disk.
	_, statErr = os.Stat(pathBefore)
	assert.True(t, os.IsNotExist(statErr), "no fresh dir should have been created for tkt-b")
}

func TestCreateReusesDirtyWorktree(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	pathA, _, err := m.Create(repoDir, "myrepo", "tkt-a", "feat/stacked")
	require.NoError(t, err)

	// Make worktree dirty with an untracked file.
	dirtyFile := filepath.Join(pathA, "dirty.txt")
	require.NoError(t, os.WriteFile(dirtyFile, []byte("wip"), 0o644))

	pathB, createdB, err := m.Create(repoDir, "myrepo", "tkt-b", "feat/stacked")
	require.NoError(t, err)
	assert.False(t, createdB)
	assert.Equal(t, pathA, pathB)

	// Dirty file must still be present and untouched.
	data, err := os.ReadFile(dirtyFile)
	require.NoError(t, err)
	assert.Equal(t, "wip", string(data))
}

func TestRemoveByBranchDiscoversPath(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, _, err := m.Create(repoDir, "myrepo", "tkt-1", "feat/stacked")
	require.NoError(t, err)

	require.NoError(t, m.Remove(repoDir, "feat/stacked"))

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "worktree dir should be gone")

	found, err := FindWorktreeForBranch(repoDir, "feat/stacked")
	require.NoError(t, err)
	assert.Equal(t, "", found, "branch should no longer have a worktree")
}

func TestRemoveWhenBranchHasNoWorktree(t *testing.T) {
	repoDir := initRepo(t)
	m := New(t.TempDir())

	require.NoError(t, m.Remove(repoDir, "feat/absent"))
}

func TestDetectDefaultBranch(t *testing.T) {
	cases := []struct {
		name       string
		initBranch string
		want       string
	}{
		{name: "main branch", initBranch: "main", want: "main"},
		{name: "master branch", initBranch: "master", want: "master"},
		{name: "develop branch", initBranch: "develop", want: "develop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := initRepoWithBranch(t, tc.initBranch)
			got, err := DetectDefaultBranch(dir)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDetectDefaultBranchNoBranch(t *testing.T) {
	// An empty repo with no commits still has HEAD pointing to an unborn branch.
	// DetectDefaultBranch should return that branch name.
	dir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)

	got, err := DetectDefaultBranch(dir)
	require.NoError(t, err)
	assert.NotEmpty(t, got)
}

func TestDetectDefaultBranchOriginHEADPrecedence(t *testing.T) {
	// Create an "upstream" repo with default branch "upstream-default".
	upstream := initRepoWithBranch(t, "upstream-default")

	// Clone it — git sets origin/HEAD automatically.
	cloneDir := t.TempDir()
	cmd := exec.Command("git", "clone", upstream, cloneDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git clone: %s", out)

	// Switch the local repo to a different branch so HEAD != origin/HEAD.
	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "-b", "feature-branch"},
		{"commit", "--allow-empty", "-m", "feature"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = cloneDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	got, err := DetectDefaultBranch(cloneDir)
	require.NoError(t, err)
	assert.Equal(t, "upstream-default", got, "origin/HEAD should take precedence over local HEAD")
}

func TestCreateAutoDetectBranch(t *testing.T) {
	dir := initRepoWithBranch(t, "master")
	wtDir := t.TempDir()
	m := New(wtDir)

	path, created, err := m.Create(dir, "myrepo", "tkt-1", "kontora/tkt-1")
	require.NoError(t, err)
	assert.True(t, created)
	assertBranch(t, path, "kontora/tkt-1")
}

func TestCreateCustomBranch(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, created, err := m.Create(repoDir, "myrepo", "tkt-1", "my-feature-branch")
	require.NoError(t, err)
	assert.True(t, created)
	assertBranch(t, path, "my-feature-branch")
}

func assertBranch(t *testing.T, dir, wantBranch string) {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err, "git rev-parse")
	got := string(out)
	got = got[:len(got)-1] // trim newline
	assert.Equal(t, wantBranch, got)
}

func assertBranchExists(t *testing.T, repoDir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", branch)
	cmd.Dir = repoDir
	assert.NoError(t, cmd.Run(), "branch %q does not exist", branch)
}
