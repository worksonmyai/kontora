package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func exactOwner(path string) BranchOwnership {
	return BranchOwnership{BoardID: "board", ObjectiveID: "objective", Generation: 1, Worktree: path}
}

func reserveExact(t *testing.T, repo, branch, oid string) ExactOpts {
	t.Helper()
	opts := ExactOpts{RepoPath: repo, Branch: branch, Owner: exactOwner(filepath.Join(t.TempDir(), "workspace")), OID: oid}
	require.NoError(t, ReserveBranch(repo, branch, opts.Owner))
	return opts
}

func exactCommitFile(t *testing.T, path, name, content string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(path, name), []byte(content), 0o600))
	managedGit(t, path, "add", "--", name)
	managedGit(t, path, "commit", "-m", "fixture "+name)
	return revParse(t, path, "HEAD")
}

func TestCreateExactCheckedTipAndPersistentLease(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	checked := revParse(t, repo, "HEAD")
	newTip := exactCommitFile(t, repo, "later", "not part of checked tip")
	require.NotEqual(t, checked, newTip)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "dirty-root"), []byte("preserve"), 0o600))
	opts := reserveExact(t, repo, "child", checked)
	result, err := CreateExact(opts)
	require.NoError(t, err)
	require.True(t, result.Created)
	require.Equal(t, checked, result.OID)
	require.Equal(t, checked, revParse(t, result.Path, "HEAD"))
	require.NoFileExists(t, filepath.Join(result.Path, "later"))
	require.FileExists(t, filepath.Join(repo, "dirty-root"))

	result, err = CreateExact(opts)
	require.NoError(t, err)
	require.False(t, result.Created)
	require.ErrorIs(t, ReserveBranch(repo, "child", opts.Owner), ErrOwnedBranch)
	require.ErrorIs(t, New(t.TempDir()).Remove(repo, "child"), ErrOwnedBranch)
	require.ErrorIs(t, New(t.TempDir()).RemoveAt(repo, result.Path), ErrOwnedBranch)

	other := opts
	other.Owner.Generation++
	_, err = CreateExact(other)
	require.ErrorIs(t, err, ErrOwnedBranch)
	other = opts
	other.Owner.Worktree = filepath.Join(t.TempDir(), "other")
	_, err = CreateExact(other)
	require.ErrorIs(t, err, ErrOwnedBranch)
	require.Equal(t, checked, revParse(t, result.Path, "HEAD"))
}

func TestCreateExactRequiresFullCommitAndReservation(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	oid := revParse(t, repo, "HEAD")
	managedGit(t, repo, "tag", "-a", "annotated", "-m", "fixture tag")
	for i, invalid := range []string{"HEAD", oid[:12], oid + "^", revParse(t, repo, "annotated"), revParse(t, repo, "HEAD^{tree}")} {
		t.Run(invalid, func(t *testing.T) {
			opts := reserveExact(t, repo, fmt.Sprintf("invalid-%d", i), invalid)
			_, err := CreateExact(opts)
			require.Error(t, err)
			require.NoDirExists(t, opts.Owner.Worktree)
			current, err := directOID(repo, branchRef(opts.Branch))
			require.NoError(t, err)
			require.Empty(t, current)
		})
	}
	_, err := CreateExact(ExactOpts{RepoPath: repo, Branch: "unreserved", Owner: exactOwner(filepath.Join(t.TempDir(), "workspace")), OID: oid})
	require.ErrorIs(t, err, ErrOwnedBranch)
}

func TestCreateExactInterruptedProjection(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"branch only", "registered missing", "wrong tip", "foreign directory", "symbolic branch"} {
		t.Run(state, func(t *testing.T) {
			repo := initRepo(t)
			oid := revParse(t, repo, "HEAD")
			opts := reserveExact(t, repo, "child", oid)
			switch state {
			case "branch only":
				managedGit(t, repo, "update-ref", branchRef(opts.Branch), oid)
			case "registered missing":
				_, err := CreateExact(opts)
				require.NoError(t, err)
				require.NoError(t, os.RemoveAll(opts.Owner.Worktree))
			case "wrong tip":
				next := exactCommitFile(t, repo, "next", "new")
				managedGit(t, repo, "update-ref", branchRef(opts.Branch), next)
			case "foreign directory":
				require.NoError(t, os.Mkdir(opts.Owner.Worktree, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(opts.Owner.Worktree, "precious"), []byte("keep"), 0o600))
			case "symbolic branch":
				managedGit(t, repo, "symbolic-ref", branchRef(opts.Branch), "refs/heads/other")
			}
			result, err := CreateExact(opts)
			if state == "branch only" {
				require.NoError(t, err)
				require.True(t, result.Created)
				require.Equal(t, oid, revParse(t, result.Path, "HEAD"))
			} else {
				require.ErrorIs(t, err, ErrRecoveryRequired)
			}
			require.ErrorIs(t, ReserveBranch(repo, "child", opts.Owner), ErrOwnedBranch)
			if state == "foreign directory" {
				require.FileExists(t, filepath.Join(opts.Owner.Worktree, "precious"))
			}
		})
	}
}

func TestExactDirtyRefusalRetainsCommitBeforeCleanup(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"tracked", "staged", "untracked", "ignored", "assume-unchanged", "skip-worktree", "assume-unchanged clean", "skip-worktree clean"} {
		t.Run(state, func(t *testing.T) {
			repo := initRepo(t)
			oid := exactCommitFile(t, repo, "tracked", "original")
			managedGit(t, repo, "config", "core.excludesFile", filepath.Join(t.TempDir(), "unused"))
			opts := reserveExact(t, repo, "child", oid)
			result, err := CreateExact(opts)
			require.NoError(t, err)
			file := "tracked"
			if state == "untracked" || state == "ignored" {
				file = "untracked"
			}
			if state == "ignored" {
				common, err := CommonDir(repo)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(common, "info", "exclude"), []byte("untracked\n"), 0o600))
			}
			flag := strings.TrimSuffix(state, " clean")
			flagged := flag == "assume-unchanged" || flag == "skip-worktree"
			wantErr := ErrDirtyWorktree
			if flagged {
				managedGit(t, result.Path, "update-index", "--"+flag, "--", file)
				wantErr = ErrRecoveryRequired
			}
			wantContents := "precious"
			if strings.HasSuffix(state, " clean") {
				wantContents = "original"
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(result.Path, file), []byte(wantContents), 0o600))
			}
			if state == "staged" {
				managedGit(t, result.Path, "add", "tracked")
			}
			indexPath, err := gitStatePath(result.Path, "index")
			require.NoError(t, err)
			indexBefore, err := os.ReadFile(indexPath)
			require.NoError(t, err)
			require.ErrorIs(t, requireClean(result.Path), wantErr)
			_, err = CreateExact(opts)
			require.ErrorIs(t, err, wantErr)
			require.ErrorIs(t, RemoveExact(repo, "child", opts.Owner, oid, "refs/kontora/results/dirty"), wantErr)
			indexAfter, err := os.ReadFile(indexPath)
			require.NoError(t, err)
			require.Equal(t, indexBefore, indexAfter)
			require.Equal(t, oid, revParse(t, repo, "refs/kontora/results/dirty"))
			contents, err := os.ReadFile(filepath.Join(result.Path, file))
			require.NoError(t, err)
			require.Equal(t, wantContents, string(contents))
			require.Equal(t, oid, revParse(t, result.Path, "HEAD"))
		})
	}
}

func TestExactChildCommitRetentionAndNextChild(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	checked := revParse(t, repo, "HEAD")
	opts := reserveExact(t, repo, "child", checked)
	workspace, err := CreateExact(opts)
	require.NoError(t, err)
	childTip := exactCommitFile(t, workspace.Path, "child-result", "result")
	_, err = CreateExact(opts)
	require.ErrorIs(t, err, ErrRecoveryRequired)
	require.ErrorIs(t, RemoveExact(repo, "child", opts.Owner, checked, "refs/kontora/results/stale"), ErrRecoveryRequired)
	require.DirExists(t, workspace.Path)
	resultRef := "refs/kontora/results/child"
	require.NoError(t, RetainResult(repo, "child", opts.Owner, childTip, resultRef))
	require.NoError(t, RemoveExact(repo, "child", opts.Owner, childTip, resultRef))
	require.NoError(t, RemoveExact(repo, "child", opts.Owner, childTip, resultRef))
	require.NoDirExists(t, workspace.Path)
	require.Equal(t, childTip, revParse(t, repo, resultRef))
	require.Equal(t, childTip, revParse(t, repo, "refs/heads/child"))
	require.ErrorIs(t, ReserveBranch(repo, "child", opts.Owner), ErrOwnedBranch)

	exactCommitFile(t, repo, "unrelated-later-work", "not checked")
	next := reserveExact(t, repo, "next-child", childTip)
	nextWorkspace, err := CreateExact(next)
	require.NoError(t, err)
	require.Equal(t, childTip, revParse(t, nextWorkspace.Path, "HEAD"))
	require.FileExists(t, filepath.Join(nextWorkspace.Path, "child-result"))
	require.NoFileExists(t, filepath.Join(nextWorkspace.Path, "unrelated-later-work"))
	newTip := exactCommitFile(t, nextWorkspace.Path, "next-result", "next")
	require.ErrorIs(t, RetainResult(repo, "next-child", next.Owner, newTip, resultRef), ErrRecoveryRequired)
	require.Equal(t, childTip, revParse(t, repo, resultRef))
}

func TestExactPendingOperationAndChangedHEADPreserved(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"merge", "detached", "other branch"} {
		t.Run(state, func(t *testing.T) {
			repo := initRepo(t)
			oid := revParse(t, repo, "HEAD")
			opts := reserveExact(t, repo, "child", oid)
			workspace, err := CreateExact(opts)
			require.NoError(t, err)
			switch state {
			case "merge":
				p, err := gitStatePath(workspace.Path, "MERGE_HEAD")
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(p, []byte(oid+"\n"), 0o600))
			case "detached":
				managedGit(t, workspace.Path, "checkout", "--detach")
			case "other branch":
				managedGit(t, workspace.Path, "checkout", "-b", "other")
			}
			_, err = CreateExact(opts)
			require.ErrorIs(t, err, ErrRecoveryRequired)
			require.ErrorIs(t, RemoveExact(repo, "child", opts.Owner, oid, "refs/kontora/results/pending"), ErrRecoveryRequired)
			require.DirExists(t, workspace.Path)
		})
	}
}
