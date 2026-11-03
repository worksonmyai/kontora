package worktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func managedGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func TestManagedBranchAliasesAndCleanup(t *testing.T) {
	repo := initRepo(t)
	root := t.TempDir()
	linked := filepath.Join(root, "linked")
	managedGit(t, repo, "worktree", "add", "-b", "ordinary", linked)
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(repo, alias))
	owned := filepath.Join(root, "owned")
	require.NoError(t, ReserveBranch(repo, "managed", BranchOwnership{BoardID: "board", ObjectiveID: "objective", Generation: 1, Worktree: owned}))
	managedGit(t, repo, "branch", "managed")
	managedGit(t, repo, "symbolic-ref", "refs/heads/alias", "refs/heads/managed")
	managedGit(t, repo, "symbolic-ref", "refs/heads/chain", "refs/heads/alias")
	managedGit(t, repo, "symbolic-ref", "refs/heads/ordinary-alias", "refs/heads/ordinary")
	require.NoError(t, ReserveBranch(repo, "unprojected", BranchOwnership{BoardID: "board", ObjectiveID: "objective", Generation: 1}))
	managedGit(t, repo, "symbolic-ref", "refs/heads/dangling", "refs/heads/unprojected")
	manager := New(filepath.Join(root, "ordinary-worktrees"))
	for _, checkout := range []string{repo, alias, linked} {
		t.Run(checkout, func(t *testing.T) {
			reused, created, err := manager.Create(CreateOpts{RepoPath: checkout, RepoName: "repo", TaskID: "task", Branch: "ordinary"})
			require.NoError(t, err)
			resolved, err := filepath.EvalSymlinks(linked)
			require.NoError(t, err)
			require.Equal(t, resolved, reused)
			require.False(t, created)
			for _, branch := range []string{"managed", "refs/heads/managed", "alias", "refs/heads/alias", "chain", "dangling", "ordinary-alias"} {
				t.Run(branch, func(t *testing.T) {
					_, _, err := manager.Create(CreateOpts{RepoPath: checkout, RepoName: "repo", TaskID: "task", Branch: branch})
					require.ErrorIs(t, err, ErrOwnedBranch)
					require.ErrorIs(t, manager.Remove(checkout, branch), ErrOwnedBranch)
					require.ErrorIs(t, ReserveBranch(checkout, branch, BranchOwnership{BoardID: "other", ObjectiveID: "other", Generation: 1}), ErrOwnedBranch)
				})
			}
		})
	}
	managedGit(t, repo, "worktree", "add", owned, "managed")
	ownedAlias := filepath.Join(root, "owned-alias")
	require.NoError(t, os.Symlink(owned, ownedAlias))
	require.ErrorIs(t, manager.RemoveAt(linked, ownedAlias), ErrOwnedBranch)
	managedGit(t, owned, "checkout", "--detach")
	require.ErrorIs(t, manager.RemoveAt(alias, owned), ErrOwnedBranch)
	_, err := os.Stat(owned)
	require.NoError(t, err)
}

func TestManagedBranchCleanupWithoutProjectedWorktreeLease(t *testing.T) {
	repo := initRepo(t)
	root := t.TempDir()
	require.NoError(t, ReserveBranch(repo, "managed", BranchOwnership{BoardID: "board", ObjectiveID: "objective", Generation: 1}))
	owned := filepath.Join(root, "owned")
	managedGit(t, repo, "worktree", "add", "-b", "managed", owned)
	require.ErrorIs(t, New(root).RemoveAt(repo, owned), ErrOwnedBranch)
}

func TestManagedWorktreeReuseAfterBranchChange(t *testing.T) {
	for _, change := range []string{"checkout", "symbolic-ref"} {
		t.Run(change, func(t *testing.T) {
			repo := initRepo(t)
			root := t.TempDir()
			owned := filepath.Join(root, "owned")
			require.NoError(t, ReserveBranch(repo, "managed", BranchOwnership{BoardID: "board", ObjectiveID: "objective", Generation: 1, Worktree: owned}))
			managedGit(t, repo, "worktree", "add", "-b", "managed", owned)
			managedGit(t, repo, "branch", "ordinary")
			if change == "checkout" {
				managedGit(t, owned, "checkout", "ordinary")
			} else {
				managedGit(t, owned, "symbolic-ref", "HEAD", "refs/heads/ordinary")
			}
			manager := New(filepath.Join(root, "new"))
			path, created, err := manager.Create(CreateOpts{RepoPath: repo, RepoName: "repo", TaskID: "task", Branch: "ordinary"})
			require.ErrorIs(t, err, ErrOwnedBranch)
			require.Empty(t, path)
			require.False(t, created)
			require.ErrorIs(t, manager.Remove(repo, "ordinary"), ErrOwnedBranch)
			_, err = os.Stat(owned)
			require.NoError(t, err)
		})
	}
}

func TestManagedBranchReservationPaths(t *testing.T) {
	for _, tt := range []struct {
		name    string
		kind    string
		alias   bool
		wantErr bool
	}{
		{name: "duplicate", kind: "reserved", wantErr: true},
		{name: "duplicate through symlink", kind: "reserved", alias: true, wantErr: true},
		{name: "ordinary checkout", kind: "checkout", wantErr: true},
		{name: "ordinary checkout through symlink", kind: "checkout", alias: true, wantErr: true},
		{name: "deleted registered checkout", kind: "deleted", wantErr: true},
		{name: "main checkout", kind: "main", wantErr: true},
		{name: "existing directory", kind: "directory", wantErr: true},
		{name: "existing file", kind: "file", wantErr: true},
		{name: "new path", kind: "new"},
		{name: "new path through symlink", kind: "new", alias: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := initRepo(t)
			root := t.TempDir()
			path := filepath.Join(root, "worktree")
			owner := BranchOwnership{BoardID: "board", ObjectiveID: "first", Generation: 1, Worktree: path}
			switch tt.kind {
			case "reserved":
				require.NoError(t, ReserveBranch(repo, "first", owner))
			case "checkout", "deleted":
				managedGit(t, repo, "worktree", "add", "-b", "ordinary", path)
				if tt.kind == "deleted" {
					require.NoError(t, os.RemoveAll(path))
				}
			case "main":
				path = repo
			case "directory":
				require.NoError(t, os.Mkdir(path, 0o700))
			case "file":
				require.NoError(t, os.WriteFile(path, []byte("ordinary data"), 0o600))
			}
			if tt.alias {
				alias := filepath.Join(t.TempDir(), "alias")
				require.NoError(t, os.Symlink(root, alias))
				path = filepath.Join(alias, "worktree")
			}
			owner.ObjectiveID = "second"
			owner.Worktree = path
			err := ReserveBranch(repo, "second", owner)
			if tt.wantErr {
				require.Error(t, err)
				if tt.kind == "reserved" {
					require.ErrorIs(t, err, ErrOwnedBranch)
				}
				_, _, err = New(filepath.Join(root, "ordinary")).Create(CreateOpts{RepoPath: repo, RepoName: "repo", TaskID: "task", Branch: "second"})
				require.NoError(t, err)
				if tt.kind == "checkout" {
					require.NoError(t, New(root).Remove(repo, "ordinary"))
				}
			} else {
				require.NoError(t, err)
				_, _, err = New(root).Create(CreateOpts{RepoPath: repo, RepoName: "repo", TaskID: "task", Branch: "second"})
				require.ErrorIs(t, err, ErrOwnedBranch)
			}
		})
	}
}

func TestManagedBranchEnrollmentAcquisitionRace(t *testing.T) {
	for _, tt := range []struct {
		name        string
		branch      string
		reservePath bool
	}{
		{name: "same branch", branch: "contended"},
		{name: "different branches at same path", branch: "ordinary", reservePath: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := initRepo(t)
			manager := New(t.TempDir())
			owner := BranchOwnership{BoardID: "board", ObjectiveID: "objective", Generation: 1}
			if tt.reservePath {
				owner.Worktree = manager.Path("repo", "task")
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			wg.Go(func() {
				<-start
				results <- ReserveBranch(repo, "contended", owner)
			})
			wg.Go(func() {
				<-start
				_, _, err := manager.Create(CreateOpts{RepoPath: repo, RepoName: "repo", TaskID: "task", Branch: tt.branch})
				results <- err
			})
			close(start)
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				}
			}
			require.Equal(t, 1, successes)
		})
	}
}

func TestBranchOwnershipParentSync(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			name := "new"
			if preexisting {
				name = "existing"
			}
			if fail {
				name += "/sync failure"
			} else {
				name += "/synced"
			}
			t.Run(name, func(t *testing.T) {
				repo := initRepo(t)
				common, err := CommonDir(repo)
				require.NoError(t, err)
				dir := filepath.Join(common, "kontora")
				if preexisting {
					require.NoError(t, os.Mkdir(dir, 0o700))
				}
				syncErr := errors.New("parent sync failed")
				synced, called := false, false
				err = withBranchOwnershipSync(repo, func(path string) error {
					require.Equal(t, common, path)
					_, err := os.Stat(dir)
					require.NoError(t, err)
					if fail {
						return syncErr
					}
					require.NoError(t, syncDirectory(path))
					synced = true
					return nil
				}, func(_ string, _ map[string]BranchOwnership) error {
					require.True(t, synced)
					called = true
					return nil
				})
				if fail {
					require.ErrorIs(t, err, syncErr)
					require.False(t, called)
					_, err = os.Stat(filepath.Join(dir, "branches.json"))
					require.ErrorIs(t, err, os.ErrNotExist)
				} else {
					require.NoError(t, err)
					require.True(t, called)
				}
			})
		}
	}
}
