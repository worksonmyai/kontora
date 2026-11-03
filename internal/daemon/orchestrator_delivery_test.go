package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func deliveryGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	data, err := command.CombinedOutput()
	require.NoError(t, err, "%v: %s", args, data)
	return strings.TrimSpace(string(data))
}

func checkedDeliveryFixture(t *testing.T, store *controlstore.Store, repo, base, candidate string) orchestrator.ResultRef {
	t.Helper()
	common, err := worktree.CommonDir(repo)
	require.NoError(t, err)
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.CreateObjective, Objective: orchestrator.Objective{ID: "objective", Assignment: "deliver checked branch", Criteria: []string{"retain child commits"}, Policy: orchestrator.Policy{Repositories: []string{common}, RequireCommitApproval: true, RequireAcceptance: true}, Budget: orchestrator.Budget{MaxAttempts: 10, MaxRuntime: time.Hour}}})
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.SetObjectiveStatus, ObjectiveID: "objective", Status: orchestrator.ObjectiveActive})
	for _, w := range []orchestrator.WorkItem{{ID: "worker", ObjectiveID: "objective", Kind: orchestrator.WorkJob, Specification: "implement"}, {ID: "verify", ObjectiveID: "objective", Kind: orchestrator.WorkCheck, Specification: "verify"}} {
		controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutWork, Work: w})
	}
	definition := orchestrator.CheckDefinition{ID: "check", Revision: "v1", ConfigurationRevision: "config", Command: []string{"test-profile"}, Directory: repo, Timeout: time.Minute}
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordVerificationPolicy, OperatorID: "operator", At: time.Now(), VerificationPolicy: orchestrator.VerificationPolicy{ID: "policy", ObjectiveID: "objective", Revision: "v1", Definitions: []orchestrator.CheckDefinition{definition}}})
	outputs := []orchestrator.QualifiedOutput{{Repository: common, Commit: candidate}}
	log, err := store.PutArtifact([]byte("fake-runner check passed"))
	require.NoError(t, err)
	for _, id := range []string{"worker", "verify"} {
		recipe := orchestrator.Recipe{Profile: id, ConfigurationRevision: "config", Timeout: time.Minute, Repository: common, BaseCommit: candidate}
		if id == "verify" {
			recipe.Verification = &orchestrator.VerificationExecution{VerificationPolicyID: "policy", DefinitionID: "check", DefinitionRevision: "v1", Command: definition.Command, Directory: repo, Outputs: outputs}
		}
		controlApply(t, store, orchestrator.Command{Kind: orchestrator.AdmitAttempt, Capacity: 1, At: time.Now(), Attempt: orchestrator.Attempt{ID: id + "-attempt", WorkID: id, ObjectiveID: "objective", SpecificationRevision: 1, Recipe: recipe, Reservation: orchestrator.Reservation{Slots: 1}}})
		controlApply(t, store, orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: id, AttemptID: id + "-attempt", AttemptStatus: orchestrator.AttemptSucceeded, At: time.Now(), Receipt: "fake runner stopped", CostKnown: true, Logs: []orchestrator.ArtifactRef{log}})
	}
	result := orchestrator.Result{ID: "result", WorkID: "worker", AttemptID: "worker-attempt", Generation: 1, SpecificationRevision: 1, Commit: candidate, Valid: true, Checked: &orchestrator.CheckedOutput{Disposition: orchestrator.OutputCode, VerificationPolicyID: "policy", Outputs: outputs, Ranges: []orchestrator.ReviewRange{{Repository: common, BaseCommit: base, HeadCommit: candidate}}}, Checks: []orchestrator.Check{{ID: "check", AttemptID: "verify-attempt", DefinitionRevision: "v1", ConfigurationRevision: "config", Command: definition.Command, Directory: repo, Timeout: time.Minute, InputsUnchanged: true, Log: log, Outputs: outputs}}}
	if base == candidate {
		result.Checked.Disposition = orchestrator.OutputNoChange
		result.Checked.Reason = "checked delivery already satisfies the work"
		result.Checked.Evidence = log
	}
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordCheckedResult, Result: result})
	return orchestrator.ResultRef{ResultID: result.ID, WorkID: result.WorkID, Generation: 1}
}

func TestManagedDeliveryRecovery(t *testing.T) {
	for _, disposition := range []orchestrator.OutputDisposition{orchestrator.OutputCode, orchestrator.OutputNoChange} {
		for _, mode := range []string{"normal", "interrupted update", "interrupted update with stale input", "unexpected reference", "missing reference", "stale checked result", "candidate differs", "missing retained reference", "already at candidate", "unexpected before record", "missing before record"} {
			t.Run(string(disposition)+"/"+mode, func(t *testing.T) {
				repo := initRepo(t)
				base := deliveryGit(t, repo, "rev-parse", "HEAD")
				if disposition == orchestrator.OutputCode {
					require.NoError(t, os.WriteFile(filepath.Join(repo, "child.txt"), []byte("child result"), 0o600))
					deliveryGit(t, repo, "add", "child.txt")
					deliveryGit(t, repo, "commit", "-m", "child result")
				}
				candidate := deliveryGit(t, repo, "rev-parse", "HEAD")
				stateDir := t.TempDir()
				store, err := controlstore.Open(stateDir, "board")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				result := checkedDeliveryFixture(t, store, repo, base, candidate)
				owner := worktree.BranchOwnership{BoardID: "board", ObjectiveID: "objective", Generation: 1}
				require.NoError(t, worktree.ReserveBranch(repo, "delivery", owner))
				_, err = worktree.EnsureDeliveryBranch(repo, "delivery", owner, base)
				require.NoError(t, err)
				retainedRef := "refs/kontora/results/result"
				deliveryGit(t, repo, "update-ref", retainedRef, candidate)
				intent := deliveryIntent{Result: result, RetainedRef: retainedRef, Update: worktree.DeliveryUpdate{RepoPath: repo, Branch: "delivery", Owner: owner, ExpectedOID: base, CandidateOID: candidate}}
				delivery := managedDelivery{store: store}
				actionID := "integrate-" + string(disposition)
				if mode == "candidate differs" {
					deliveryGit(t, repo, "commit", "--allow-empty", "-m", "unrelated unchecked result")
					intent.Update.CandidateOID = deliveryGit(t, repo, "rev-parse", "HEAD")
					deliveryGit(t, repo, "update-ref", retainedRef, intent.Update.CandidateOID)
					require.ErrorContains(t, delivery.record(actionID, intent), "not the checked repository output")
					return
				}
				if mode == "missing retained reference" {
					deliveryGit(t, repo, "update-ref", "-d", retainedRef)
					require.ErrorIs(t, delivery.record(actionID, intent), worktree.ErrRecoveryRequired)
					return
				}
				switch mode {
				case "already at candidate":
					deliveryGit(t, repo, "update-ref", "refs/heads/delivery", candidate)
				case "unexpected before record":
					deliveryGit(t, repo, "commit", "--allow-empty", "-m", "external change")
					deliveryGit(t, repo, "update-ref", "refs/heads/delivery", "HEAD")
				case "missing before record":
					deliveryGit(t, repo, "update-ref", "-d", "refs/heads/delivery")
				}
				if mode == "unexpected before record" || mode == "missing before record" || mode == "already at candidate" && base != candidate {
					require.ErrorIs(t, delivery.record(actionID, intent), orchestrator.ErrStale)
					state, err := store.State()
					require.NoError(t, err)
					require.Empty(t, state.Actions)
					return
				}
				reflogBefore := deliveryGit(t, repo, "reflog", "show", "--format=%H", "refs/heads/delivery")
				require.NoError(t, delivery.record(actionID, intent))
				reviseInput := func() {
					state, err := store.State()
					require.NoError(t, err)
					w := state.Work["worker"]
					controlApply(t, store, orchestrator.Command{Kind: orchestrator.ReviseWork, WorkID: w.ID, Preconditions: orchestrator.Preconditions{DocumentRevision: w.DocumentRevision, SpecificationRevision: w.SpecificationRevision}, Work: orchestrator.WorkItem{Specification: "changed"}})
				}
				if mode == "stale checked result" {
					reviseInput()
					require.Error(t, delivery.advance(actionID))
					require.Equal(t, base, deliveryGit(t, repo, "rev-parse", "delivery"))
					return
				}
				if mode == "unexpected reference" || mode == "missing reference" {
					if mode == "unexpected reference" {
						deliveryGit(t, repo, "commit", "--allow-empty", "-m", "external change")
						deliveryGit(t, repo, "update-ref", "refs/heads/delivery", "HEAD")
					} else {
						deliveryGit(t, repo, "update-ref", "-d", "refs/heads/delivery")
					}
					require.ErrorIs(t, delivery.reconcile(actionID), worktree.ErrDeliveryMoved)
					state, err := store.State()
					require.NoError(t, err)
					require.Equal(t, orchestrator.ActionParked, state.Actions[actionID].Status)
					return
				}
				if mode == "interrupted update" || mode == "interrupted update with stale input" {
					delivery.afterUpdate = func() error { return errors.New("interrupted before receipt") }
					require.ErrorContains(t, delivery.advance(actionID), "interrupted before receipt")
					state, err := store.State()
					require.NoError(t, err)
					require.Equal(t, orchestrator.ActionPending, state.Actions[actionID].Status)
					require.NoError(t, store.Close())
					store, err = controlstore.Open(stateDir, "board")
					require.NoError(t, err)
					delivery = managedDelivery{store: store, afterUpdate: func() error { t.Fatal("recovery repeated reference effect"); return nil }}
					if mode == "interrupted update with stale input" {
						reviseInput()
					}
					require.NoError(t, delivery.reconcile(actionID))
				} else {
					require.NoError(t, delivery.advance(actionID))
				}
				state, err := store.State()
				require.NoError(t, err)
				require.Equal(t, orchestrator.ActionCompleted, state.Actions[actionID].Status)
				require.Equal(t, candidate, deliveryGit(t, repo, "rev-parse", "delivery"))
				if disposition == orchestrator.OutputNoChange {
					require.Equal(t, "1", deliveryGit(t, repo, "rev-list", "--count", "delivery"))
					require.Equal(t, reflogBefore, deliveryGit(t, repo, "reflog", "show", "--format=%H", "refs/heads/delivery"))
				} else {
					require.Equal(t, "2", deliveryGit(t, repo, "rev-list", "--count", "delivery"))
				}
				require.NotEmpty(t, state.Actions[actionID].Receipt)
				require.NoError(t, delivery.reconcile(actionID))
			})
		}
	}
}
