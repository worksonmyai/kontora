package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestSlug(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "strips leading tag and stopwords",
			input: "[kontora] Move the LLM round-trips out of the agent-lock holds",
			want:  "move-llm-round-trips-out-agent-lock-holds",
		},
		{
			name:  "preserves hyphenated compounds",
			input: "Add a built-in opt-out end-to-end toggle",
			want:  "add-built-in-opt-out-end-to-end-toggle",
		},
		{
			name:  "deletes apostrophes",
			input: "Trace astra's work end to end",
			want:  "trace-astras-work-end-end",
		},
		{
			name:  "deletes curly apostrophes",
			input: "Fix owner’s retries",
			want:  "fix-owners-retries",
		},
		{
			name:  "uses first non-empty line",
			input: "\n  \nFix retry, NOW!\nIgnored",
			want:  "fix-retry-now",
		},
		{
			name:  "replaces unicode letters",
			input: "Café déjà vu",
			want:  "caf-d-j-vu",
		},
		{
			name:  "caps at word boundary",
			input: "one two three four five six seven eight nine ten eleven",
			want:  "one-two-three-four-five-six-seven-eight-nine-ten",
		},
		{
			name:  "hard cuts long word",
			input: strings.Repeat("a", 60),
			want:  strings.Repeat("a", 48),
		},
		{
			name:  "keeps all-stopword title",
			input: "The The",
			want:  "the-the",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
		{
			name:  "punctuation only",
			input: "!!!",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Slug(tt.input)
			assert.Equal(t, tt.want, got)
			assert.LessOrEqual(t, len(got), 48)
		})
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

	path, created, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "kontora/tkt-1"})
	require.NoError(t, err)
	assert.True(t, created)
	_, err = os.Stat(path)
	require.NoError(t, err, "worktree dir does not exist")
	assertBranch(t, path, "kontora/tkt-1")

	path2, created2, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "kontora/tkt-1"})
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

	path, created, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "kontora/tkt-1"})
	require.NoError(t, err)
	assert.True(t, created)

	require.NoError(t, m.Remove(repoDir, "kontora/tkt-1"))
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "worktree dir should be gone after remove")

	// Re-create: branch still exists, should reuse it.
	path2, created2, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "kontora/tkt-1"})
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

	p1, _, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "kontora/tkt-1"})
	require.NoError(t, err)
	p2, _, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-2", Branch: "kontora/tkt-2"})
	require.NoError(t, err)

	assert.NotEqual(t, p1, p2)

	assertBranch(t, p1, "kontora/tkt-1")
	assertBranch(t, p2, "kontora/tkt-2")
}

func TestCreateWithCustomBranch(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, _, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "custom/tkt-1"})
	require.NoError(t, err)
	assertBranch(t, path, "custom/tkt-1")

	require.NoError(t, m.Remove(repoDir, "custom/tkt-1"))
	assertBranchExists(t, repoDir, "custom/tkt-1")
}

func TestCreateWithBase(t *testing.T) {
	// main -> A, develop -> A+D1. A worktree based on develop must start at D1;
	// one with no base must start at A.
	setup := func(t *testing.T) string {
		t.Helper()
		repoDir := initRepoWithBranch(t, "main")
		mustGit(t, repoDir, "checkout", "-b", "develop")
		require.NoError(t, os.WriteFile(filepath.Join(repoDir, "d.txt"), []byte("d1"), 0o644))
		mustGit(t, repoDir, "add", "d.txt")
		mustGit(t, repoDir, "commit", "-m", "D1")
		mustGit(t, repoDir, "checkout", "main")
		return repoDir
	}

	t.Run("branches from the declared base", func(t *testing.T) {
		repoDir := setup(t)
		m := New(t.TempDir())

		path, created, err := m.Create(CreateOpts{
			RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1",
			Branch: "feature/x", Base: "develop",
		})
		require.NoError(t, err)
		assert.True(t, created)
		assertBranch(t, path, "feature/x")
		assert.Equal(t, revParse(t, repoDir, "develop"), revParse(t, path, "HEAD"))
		assert.FileExists(t, filepath.Join(path, "d.txt"))
	})

	t.Run("empty base branches from the default branch", func(t *testing.T) {
		repoDir := setup(t)
		m := New(t.TempDir())

		path, _, err := m.Create(CreateOpts{
			RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "feature/x",
		})
		require.NoError(t, err)
		assert.Equal(t, revParse(t, repoDir, "main"), revParse(t, path, "HEAD"))
		assert.NoFileExists(t, filepath.Join(path, "d.txt"))
	})

	t.Run("missing base fails", func(t *testing.T) {
		repoDir := setup(t)
		m := New(t.TempDir())

		_, _, err := m.Create(CreateOpts{
			RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1",
			Branch: "feature/x", Base: "devlop",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "devlop")
		assert.Contains(t, err.Error(), repoDir)
	})

	t.Run("base equal to the work branch fails", func(t *testing.T) {
		repoDir := setup(t)
		m := New(t.TempDir())

		_, _, err := m.Create(CreateOpts{
			RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1",
			Branch: "develop", Base: "develop",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "develop")
		assert.Contains(t, err.Error(), "own work branch")

		// The recovery path must not have checked develop out anywhere.
		found, err := FindWorktreeForBranch(repoDir, "develop")
		require.NoError(t, err)
		assert.Equal(t, "", found, "develop must not be checked out in a worktree")
	})

	t.Run("base is ignored when a worktree already exists", func(t *testing.T) {
		repoDir := setup(t)
		m := New(t.TempDir())

		first, _, err := m.Create(CreateOpts{
			RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "feature/x",
		})
		require.NoError(t, err)
		head := revParse(t, first, "HEAD")

		second, created, err := m.Create(CreateOpts{
			RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1",
			Branch: "feature/x", Base: "develop",
		})
		require.NoError(t, err)
		assert.False(t, created)
		assert.Equal(t, first, second)
		assert.Equal(t, head, revParse(t, second, "HEAD"), "HEAD must not move when the base changes")
	})
}

func TestResolveBase(t *testing.T) {
	// A repo on main with a develop branch, a branch-shaped v1.0 tag, and a
	// refs/remotes/origin/develop ref standing in for a fetched remote branch.
	repoDir := initRepoWithBranch(t, "main")
	mustGit(t, repoDir, "branch", "develop")
	mustGit(t, repoDir, "tag", "v1.0")
	mustGit(t, repoDir, "update-ref", "refs/remotes/origin/develop", "refs/heads/develop")
	sha := revParse(t, repoDir, "HEAD")

	// A second commit so HEAD~1 names a real commit.
	mustGit(t, repoDir, "commit", "--allow-empty", "-m", "second")

	cases := []struct {
		name    string
		base    string
		want    string
		wantErr bool
	}{
		{name: "empty falls back to the default branch", base: "", want: "main"},
		{name: "whitespace falls back to the default branch", base: "  ", want: "main"},
		{name: "local branch", base: "develop", want: "refs/heads/develop"},
		{name: "remote-tracking branch", base: "origin/develop", want: "refs/remotes/origin/develop"},
		{name: "missing branch", base: "devlop", wantErr: true},
		{name: "tag with a branch-shaped name", base: "v1.0", wantErr: true},
		{name: "raw commit sha", base: sha, wantErr: true},
		{name: "revision expression", base: "HEAD~1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveBase(repoDir, tc.base)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.base)
				assert.Contains(t, err.Error(), repoDir)
				assert.Contains(t, err.Error(), "local or remote-tracking branch")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRemoveDirtyWorktree(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, _, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-dirty", Branch: "kontora/tkt-dirty"})
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

	path, _, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-staged", Branch: "kontora/tkt-staged"})
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

	path, _, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "feat/stacked"})
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
	pathA, createdA, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-a", Branch: "feat/stacked"})
	require.NoError(t, err)
	require.True(t, createdA)

	pathBefore := m.Path("myrepo", "tkt-b")
	_, statErr := os.Stat(pathBefore)
	require.True(t, os.IsNotExist(statErr), "tkt-b dir should not yet exist")

	// Second create with a different ticketID but the same branch must reuse pathA.
	pathB, createdB, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-b", Branch: "feat/stacked"})
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

	pathA, _, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-a", Branch: "feat/stacked"})
	require.NoError(t, err)

	// Make worktree dirty with an untracked file.
	dirtyFile := filepath.Join(pathA, "dirty.txt")
	require.NoError(t, os.WriteFile(dirtyFile, []byte("wip"), 0o644))

	pathB, createdB, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-b", Branch: "feat/stacked"})
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

	path, _, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "feat/stacked"})
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

	path, created, err := m.Create(CreateOpts{RepoPath: dir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "kontora/tkt-1"})
	require.NoError(t, err)
	assert.True(t, created)
	assertBranch(t, path, "kontora/tkt-1")
}

func TestCreateCustomBranch(t *testing.T) {
	repoDir := initRepo(t)
	wtDir := t.TempDir()
	m := New(wtDir)

	path, created, err := m.Create(CreateOpts{RepoPath: repoDir, RepoName: "myrepo", TaskID: "tkt-1", Branch: "my-feature-branch"})
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

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err, "git rev-parse %s", ref)
	return strings.TrimSpace(string(out))
}

func assertBranchExists(t *testing.T, repoDir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", branch)
	cmd.Dir = repoDir
	assert.NoError(t, cmd.Run(), "branch %q does not exist", branch)
}
