package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func reserveDelivery(t *testing.T, repo, oid string) BranchOwnership {
	t.Helper()
	owner := exactOwner("")
	require.NoError(t, ReserveBranch(repo, "delivery", owner))
	created, err := EnsureDeliveryBranch(repo, "delivery", owner, oid)
	require.NoError(t, err)
	require.True(t, created)
	return owner
}

func TestDeliveryUncheckedOutExactInitialization(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	oid := revParse(t, repo, "HEAD")
	owner := reserveDelivery(t, repo, oid)
	created, err := EnsureDeliveryBranch(repo, "delivery", owner, oid)
	require.NoError(t, err)
	require.False(t, created)
	path, err := registeredBranch(repo, "refs/heads/delivery")
	require.NoError(t, err)
	require.Empty(t, path)
	next := exactCommitFile(t, repo, "next", "next")
	_, err = EnsureDeliveryBranch(repo, "delivery", owner, next)
	require.ErrorIs(t, err, ErrDeliveryMoved)
	require.Equal(t, oid, revParse(t, repo, "delivery"))
	require.ErrorIs(t, ReserveBranch(repo, "delivery", owner), ErrOwnedBranch)

	checkout := filepath.Join(t.TempDir(), "delivery-checkout")
	managedGit(t, repo, "worktree", "add", checkout, "delivery")
	_, err = EnsureDeliveryBranch(repo, "delivery", owner, oid)
	require.ErrorIs(t, err, ErrRecoveryRequired)
	update := DeliveryUpdate{RepoPath: repo, Branch: "delivery", Owner: owner, ExpectedOID: oid, CandidateOID: next}
	_, err = UpdateDelivery(update)
	require.ErrorIs(t, err, ErrRecoveryRequired)
	require.Equal(t, oid, revParse(t, checkout, "HEAD"))
}

func TestFastForwardCandidateAndInterruptedDeliveryRecovery(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	target := revParse(t, repo, "HEAD")
	owner := reserveDelivery(t, repo, target)
	child := reserveExact(t, repo, "child", target)
	childWorkspace, err := CreateExact(child)
	require.NoError(t, err)
	resultOID := exactCommitFile(t, childWorkspace.Path, "child", "child")
	require.NoError(t, RemoveExact(repo, child.Branch, child.Owner, resultOID, "refs/kontora/results/child"))

	candidate := reserveExact(t, repo, "candidate", target)
	workspace, err := CreateExact(candidate)
	require.NoError(t, err)
	prepared, err := PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: resultOID})
	require.NoError(t, err)
	require.Equal(t, CandidateFastForward, prepared.Kind)
	require.Equal(t, resultOID, prepared.CandidateOID)
	require.Equal(t, target, revParse(t, workspace.Path, "HEAD"), "no merge or ref update during fast-forward preparation")
	require.Equal(t, target, revParse(t, repo, "delivery"))
	update := DeliveryUpdate{RepoPath: repo, Branch: "delivery", Owner: owner, ExpectedOID: target, CandidateOID: prepared.CandidateOID}
	state, err := InspectDelivery(update)
	require.NoError(t, err)
	require.Equal(t, DeliveryAtTarget, state.State)
	state, err = UpdateDelivery(update)
	require.NoError(t, err)
	require.Equal(t, DeliveryAtCandidate, state.State)
	require.True(t, state.Updated)

	state, err = InspectDelivery(update)
	require.NoError(t, err)
	require.Equal(t, DeliveryAtCandidate, state.State)
	state, err = UpdateDelivery(update)
	require.NoError(t, err)
	require.False(t, state.Updated)
	require.Equal(t, resultOID, state.CurrentOID)

	other := exactCommitFile(t, childWorkspaceOrNew(t, repo, resultOID), "other", "other")
	managedGit(t, repo, "update-ref", "refs/heads/delivery", other, resultOID)
	state, err = InspectDelivery(update)
	require.NoError(t, err)
	require.Equal(t, DeliveryAtOther, state.State)
	state, err = UpdateDelivery(update)
	require.ErrorIs(t, err, ErrDeliveryMoved)
	require.Equal(t, other, state.CurrentOID)
	require.Equal(t, other, revParse(t, repo, "delivery"))
	managedGit(t, repo, "update-ref", "-d", "refs/heads/delivery", other)
	state, err = InspectDelivery(update)
	require.NoError(t, err)
	require.Equal(t, DeliveryAtOther, state.State)
	require.Empty(t, state.CurrentOID)
	_, err = UpdateDelivery(update)
	require.ErrorIs(t, err, ErrDeliveryMoved)
	require.ErrorIs(t, ReserveBranch(repo, "delivery", owner), ErrOwnedBranch)
}

func childWorkspaceOrNew(t *testing.T, repo, oid string) string {
	t.Helper()
	opts := reserveExact(t, repo, "later-child", oid)
	workspace, err := CreateExact(opts)
	require.NoError(t, err)
	return workspace.Path
}

func TestDivergentCandidateRequiresApprovedCommitWithoutHooks(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	base := revParse(t, repo, "HEAD")
	child := reserveExact(t, repo, "child", base)
	childWorkspace, err := CreateExact(child)
	require.NoError(t, err)
	childOID := exactCommitFile(t, childWorkspace.Path, "child", "child")
	target := exactCommitFile(t, repo, "target", "target")
	owner := reserveDelivery(t, repo, target)
	candidate := reserveExact(t, repo, "candidate", target)
	workspace, err := CreateExact(candidate)
	require.NoError(t, err)

	common, err := CommonDir(repo)
	require.NoError(t, err)
	sentinel := filepath.Join(t.TempDir(), "hooks-ran")
	for _, name := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "pre-merge-commit", "post-merge"} {
		require.NoError(t, os.WriteFile(filepath.Join(common, "hooks", name), []byte("#!/bin/sh\nprintf '%s\\n' "+name+" >> '"+sentinel+"'\n"), 0o700))
	}
	prepared, err := PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: childOID})
	require.NoError(t, err)
	require.Equal(t, CandidateNeedsCommit, prepared.Kind)
	require.Empty(t, prepared.CandidateOID)
	require.False(t, prepared.Reused)
	require.Equal(t, target, revParse(t, workspace.Path, "HEAD"))
	require.Equal(t, target, revParse(t, repo, "delivery"))
	require.Equal(t, childOID, revParse(t, workspace.Path, "MERGE_HEAD"))
	require.FileExists(t, filepath.Join(workspace.Path, "child"))
	require.FileExists(t, filepath.Join(workspace.Path, "target"))
	require.NoFileExists(t, sentinel)
	prepared, err = PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: childOID})
	require.NoError(t, err)
	require.True(t, prepared.Reused)
	require.Equal(t, CandidateNeedsCommit, prepared.Kind)
	require.NoFileExists(t, sentinel)
	require.ErrorIs(t, RemoveExact(repo, candidate.Branch, candidate.Owner, target, "refs/kontora/results/pending"), ErrDirtyWorktree)

	managedGit(t, workspace.Path, "commit", "-m", "explicitly approved fixture merge")
	approvedOID := revParse(t, workspace.Path, "HEAD")
	require.FileExists(t, sentinel)
	require.Equal(t, target, revParse(t, workspace.Path, "HEAD^1"))
	require.Equal(t, childOID, revParse(t, workspace.Path, "HEAD^2"))
	require.NoError(t, RemoveExact(repo, candidate.Branch, candidate.Owner, approvedOID, "refs/kontora/results/candidate"))
	state, err := UpdateDelivery(DeliveryUpdate{RepoPath: repo, Branch: "delivery", Owner: owner, ExpectedOID: target, CandidateOID: approvedOID})
	require.NoError(t, err)
	require.True(t, state.Updated)
	require.Equal(t, approvedOID, revParse(t, repo, "delivery"))
}

func TestDivergentConflictsAreRecoverableAndNeverAutoCommitted(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	base := exactCommitFile(t, repo, "conflict", "base\n")
	child := reserveExact(t, repo, "child", base)
	childWorkspace, err := CreateExact(child)
	require.NoError(t, err)
	childOID := exactCommitFile(t, childWorkspace.Path, "conflict", "child\n")
	target := exactCommitFile(t, repo, "conflict", "target\n")
	candidate := reserveExact(t, repo, "candidate", target)
	workspace, err := CreateExact(candidate)
	require.NoError(t, err)
	prepared, err := PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: childOID})
	require.NoError(t, err)
	require.Equal(t, CandidateConflicts, prepared.Kind)
	require.Empty(t, prepared.CandidateOID)
	require.Equal(t, target, revParse(t, workspace.Path, "HEAD"))
	require.ErrorIs(t, RemoveExact(repo, candidate.Branch, candidate.Owner, target, "refs/kontora/results/conflicted"), ErrDirtyWorktree)

	repair := filepath.Join(workspace.Path, "conflict")
	require.NoError(t, os.WriteFile(repair, []byte("my careful repair\n"), 0o600))
	prepared, err = PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: childOID})
	require.NoError(t, err)
	require.True(t, prepared.Reused)
	require.Equal(t, CandidateConflicts, prepared.Kind)
	contents, err := os.ReadFile(repair)
	require.NoError(t, err)
	require.Equal(t, "my careful repair\n", string(contents))
	managedGit(t, workspace.Path, "add", "conflict")
	prepared, err = PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: childOID})
	require.NoError(t, err)
	require.Equal(t, CandidateNeedsCommit, prepared.Kind)
	require.Empty(t, prepared.CandidateOID)
	_, err = PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: base})
	require.ErrorIs(t, err, ErrRecoveryRequired)
	require.Equal(t, target, revParse(t, workspace.Path, "HEAD"))
}

func TestCandidateDirtyRefusalAndIncludedHistory(t *testing.T) {
	t.Parallel()
	for _, dirty := range []string{"clean", "tracked", "staged", "untracked"} {
		t.Run(dirty, func(t *testing.T) {
			repo := initRepo(t)
			base := exactCommitFile(t, repo, "tracked", "base")
			target := exactCommitFile(t, repo, "next", "next")
			candidate := reserveExact(t, repo, "candidate", target)
			workspace, err := CreateExact(candidate)
			require.NoError(t, err)
			managedGit(t, repo, "config", "merge.autoStash", "true")
			if dirty != "clean" {
				file := "tracked"
				if dirty == "untracked" {
					file = "precious"
				}
				require.NoError(t, os.WriteFile(filepath.Join(workspace.Path, file), []byte("preserve"), 0o600))
				if dirty == "staged" {
					managedGit(t, workspace.Path, "add", file)
				}
			}
			prepared, err := PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: base})
			if dirty == "clean" {
				require.NoError(t, err)
				require.Equal(t, CandidateAlreadyIncluded, prepared.Kind)
				require.Equal(t, target, prepared.CandidateOID)
			} else {
				require.ErrorIs(t, err, ErrDirtyWorktree)
			}
			require.Equal(t, target, revParse(t, workspace.Path, "HEAD"))
			p, err := gitStatePath(workspace.Path, "MERGE_HEAD")
			require.NoError(t, err)
			require.NoFileExists(t, p)
			stash, err := directOID(repo, "refs/stash")
			require.NoError(t, err)
			require.Empty(t, stash, "dirty work must never be autostashed")
		})
	}
}

func TestCandidateRefusesPreparationHooksInsteadOfDisabling(t *testing.T) {
	t.Parallel()
	for _, hook := range []string{"reference-transaction", "post-index-change", "fsmonitor", "configured-ref-hook", "configured-index-hook"} {
		t.Run(hook, func(t *testing.T) {
			repo := initRepo(t)
			base := revParse(t, repo, "HEAD")
			child := reserveExact(t, repo, "child", base)
			childWorkspace, err := CreateExact(child)
			require.NoError(t, err)
			childOID := exactCommitFile(t, childWorkspace.Path, "child", "child")
			target := exactCommitFile(t, repo, "target", "target")
			candidate := reserveExact(t, repo, "candidate", target)
			_, err = CreateExact(candidate)
			require.NoError(t, err)
			common, err := CommonDir(repo)
			require.NoError(t, err)
			hooks := filepath.Join(t.TempDir(), "required-hooks")
			require.NoError(t, os.Mkdir(hooks, 0o700))
			managedGit(t, repo, "config", "core.hooksPath", hooks)
			sentinel := filepath.Join(common, "hook-ran")
			hookPath := filepath.Join(hooks, hook)
			require.NoError(t, os.WriteFile(hookPath, []byte("#!/bin/sh\necho ran > '"+sentinel+"'\nexit 0\n"), 0o700))
			switch hook {
			case "fsmonitor":
				managedGit(t, repo, "config", "core.fsmonitor", hookPath)
			case "configured-ref-hook", "configured-index-hook":
				event := "reference-transaction"
				if hook == "configured-index-hook" {
					event = "post-index-change"
				}
				managedGit(t, repo, "config", "hook.required.event", event)
				managedGit(t, repo, "config", "hook.required.command", hookPath)
			}
			_, err = PrepareCandidate(CandidateOpts{Workspace: candidate, ResultOID: childOID})
			require.ErrorContains(t, err, "explicit approval required")
			require.NoFileExists(t, sentinel)
			require.FileExists(t, hookPath)
			require.Equal(t, target, revParse(t, repo, "refs/heads/candidate"))
		})
	}
}

func TestDeliveryUpdateRunsRequiredHooksAndEnforcesExpectedOldOID(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	target := revParse(t, repo, "HEAD")
	owner := reserveDelivery(t, repo, target)
	candidate := exactCommitFile(t, repo, "candidate", "candidate")
	other := exactCommitFile(t, repo, "other", "other")
	common, err := CommonDir(repo)
	require.NoError(t, err)
	hook := filepath.Join(common, "hooks", "reference-transaction")
	log := filepath.Join(t.TempDir(), "ref-input")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nif test \"$1\" = prepared; then\n  while IFS= read -r line; do printf '%s\\n' \"$line\" >> '"+log+"'; done\n  exit 1\nfi\n"), 0o700))
	update := DeliveryUpdate{RepoPath: repo, Branch: "delivery", Owner: owner, ExpectedOID: target, CandidateOID: candidate}
	state, err := UpdateDelivery(update)
	require.Error(t, err)
	require.Equal(t, DeliveryAtTarget, state.State)
	contents, err := os.ReadFile(log)
	require.NoError(t, err)
	require.Contains(t, string(contents), target+" "+candidate+" refs/heads/delivery")
	require.Equal(t, target, revParse(t, repo, "delivery"))
	require.NoError(t, os.Remove(hook))
	managedGit(t, repo, "update-ref", "refs/heads/delivery", other, target)
	state, err = UpdateDelivery(update)
	require.ErrorIs(t, err, ErrDeliveryMoved)
	require.Equal(t, DeliveryAtOther, state.State)
	require.Equal(t, other, revParse(t, repo, "delivery"))
	update.ExpectedOID, update.CandidateOID = other, target
	_, err = UpdateDelivery(update)
	require.ErrorContains(t, err, "not a descendant")
	require.Equal(t, other, revParse(t, repo, "delivery"))
}
