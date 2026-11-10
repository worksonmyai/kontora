package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutomationOwnership(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		owner BranchOwnership
		valid bool
	}{
		{"automation", BranchOwnership{Kind: "automation", AutomationID: "auto-1", RunNumber: 1}, true},
		{"ordinary managed", BranchOwnership{BoardID: "board", ObjectiveID: "objective", Generation: 1}, true},
		{"unknown kind", BranchOwnership{Kind: "other", AutomationID: "auto-1", RunNumber: 1}, false},
		{"no run", BranchOwnership{Kind: "automation", AutomationID: "auto-1"}, false},
		{"no automation", BranchOwnership{Kind: "automation", RunNumber: 1}, false},
		{"missing tag", BranchOwnership{AutomationID: "auto-1", RunNumber: 1}, false},
		{"mixed owners", BranchOwnership{Kind: "automation", AutomationID: "auto-1", RunNumber: 1, BoardID: "fake", ObjectiveID: "fake", Generation: 1}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := initRepo(t)
			err := ReserveBranch(repo, "automation", tt.owner)
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.ErrorIs(t, ReserveBranch(repo, "automation", tt.owner), ErrOwnedBranch)
		})
	}
}

func TestAutomationExactCleanup(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"clean", "dirty", "commit", "commit and dirty", "wrong run"} {
		t.Run(state, func(t *testing.T) {
			repo := initRepo(t)
			base := revParse(t, repo, "HEAD")
			owner := BranchOwnership{Kind: "automation", AutomationID: "auto-1", RunNumber: 7, Worktree: filepath.Join(t.TempDir(), "run-7")}
			require.NoError(t, ReserveBranch(repo, "automation/run-7", owner))
			workspace, err := CreateExact(ExactOpts{RepoPath: repo, Branch: "automation/run-7", Owner: owner, OID: base})
			require.NoError(t, err)
			tip := base
			if state == "commit" || state == "commit and dirty" {
				tip = exactCommitFile(t, workspace.Path, "result", "keep committed result")
			}
			if state == "dirty" || state == "commit and dirty" {
				require.NoError(t, os.WriteFile(filepath.Join(workspace.Path, "precious"), []byte("keep edits"), 0o600))
			}
			if state == "wrong run" {
				other := owner
				other.RunNumber++
				require.ErrorIs(t, RemoveExact(repo, "automation/run-7", other, tip, "refs/kontora/results/auto-1/7"), ErrOwnedBranch)
				require.DirExists(t, workspace.Path)
				return
			}
			err = RemoveExact(repo, "automation/run-7", owner, tip, "refs/kontora/results/auto-1/7")
			if state == "dirty" || state == "commit and dirty" {
				require.ErrorIs(t, err, ErrDirtyWorktree)
				require.FileExists(t, filepath.Join(workspace.Path, "precious"))
			} else {
				require.NoError(t, err)
				require.NoDirExists(t, workspace.Path)
			}
			require.NoError(t, InspectRetainedResult(repo, tip, "refs/kontora/results/auto-1/7"))
			require.Equal(t, tip, revParse(t, repo, "refs/heads/automation/run-7"))
			require.ErrorIs(t, ReserveBranch(repo, "automation/run-7", owner), ErrOwnedBranch)
		})
	}
}
