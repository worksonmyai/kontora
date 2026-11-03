package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var ErrDeliveryMoved = errors.New("delivery ref differs from expected tip and candidate")

// EnsureDeliveryBranch initializes a previously reserved, unchecked-out branch
// at an exact commit. It never moves an existing branch. Owner.Worktree must be
// empty: delivery is a ref, not a workspace. Required ref-update hooks run normally.
func EnsureDeliveryBranch(repo, branch string, owner BranchOwnership, oid string) (created bool, err error) {
	err = withOwnedBranch(repo, branch, owner, func(ref string, lease BranchOwnership) error {
		if err := requireDelivery(repo, ref, lease); err != nil {
			return err
		}
		if err := fullCommit(repo, oid); err != nil {
			return err
		}
		current, err := directOID(repo, ref)
		if err != nil {
			return err
		}
		if current == oid {
			return nil
		}
		if current != "" {
			return fmt.Errorf("%w: delivery tip is %s", ErrDeliveryMoved, current)
		}
		if _, err := gitOutput(repo, "update-ref", "--no-deref", ref, oid, zeroOID(oid)); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

type CandidateKind string

const (
	CandidateFastForward     CandidateKind = "fast_forward"
	CandidateAlreadyIncluded CandidateKind = "already_included"
	CandidateNeedsCommit     CandidateKind = "needs_approved_commit"
	CandidateConflicts       CandidateKind = "conflicts"
)

type CandidateOpts struct {
	// Workspace is a separately reserved workspace created exactly at T (OID).
	Workspace ExactOpts
	// ResultOID is the exact child result to integrate, not a moving branch name.
	ResultOID string
}

type CandidateResult struct {
	Kind      CandidateKind
	TargetOID string
	ResultOID string
	// Empty for divergent histories; caller must obtain an explicitly approved commit separately.
	CandidateOID string
	Reused       bool // An existing, matching uncommitted merge was inspected only.
}

// Preparation never commits or moves delivery. Existing candidates leave the workspace
// unchanged; divergent merges leave staged changes or conflicts for an approved commit.
// Retries inspect matching merges without overwriting repairs.
//
// Preparation refuses hooks that could run before commit approval; it disables none.
// Callers must authorize merge drivers/filters and coordinate external Git writers.
func PrepareCandidate(opts CandidateOpts) (result CandidateResult, err error) {
	wt := opts.Workspace
	result.TargetOID, result.ResultOID = wt.OID, opts.ResultOID
	err = withOwnedBranch(wt.RepoPath, wt.Branch, wt.Owner, func(ref string, lease BranchOwnership) error {
		if lease.Worktree == "" {
			return fmt.Errorf("candidate requires a reserved workspace")
		}
		for _, oid := range []string{wt.OID, opts.ResultOID} {
			if err := fullCommit(wt.RepoPath, oid); err != nil {
				return err
			}
		}
		if err := refusePreparationHooks(lease.Worktree); err != nil {
			return err
		}
		registered, err := registeredBranch(wt.RepoPath, ref)
		if err != nil {
			return err
		}
		if registered != lease.Worktree {
			return fmt.Errorf("%w: candidate workspace not registered at reserved path", ErrRecoveryRequired)
		}
		if err := checkExactWorkspace(wt.RepoPath, ref, lease.Worktree, wt.OID, false); err != nil {
			return err
		}
		mergePath, err := gitStatePath(lease.Worktree, "MERGE_HEAD")
		if err != nil {
			return err
		}
		pending, err := os.ReadFile(mergePath)
		if err == nil {
			if strings.TrimSpace(string(pending)) != opts.ResultOID {
				return fmt.Errorf("%w: different merge already pending", ErrRecoveryRequired)
			}
			result.Reused = true
			return inspectPendingCandidate(lease.Worktree, &result)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := requireClean(lease.Worktree); err != nil {
			return err
		}
		if err := requireNoOperation(lease.Worktree); err != nil {
			return err
		}
		ff, err := isAncestor(wt.RepoPath, wt.OID, opts.ResultOID)
		if err != nil {
			return err
		}
		if ff {
			result.Kind, result.CandidateOID = CandidateFastForward, opts.ResultOID
			return nil
		}
		included, err := isAncestor(wt.RepoPath, opts.ResultOID, wt.OID)
		if err != nil {
			return err
		}
		if included {
			result.Kind, result.CandidateOID = CandidateAlreadyIncluded, wt.OID
			return nil
		}
		_, mergeErr := gitOutput(lease.Worktree, "merge", "--no-ff", "--no-commit", "--no-edit", "--no-autostash", "--no-rerere-autoupdate", "--", opts.ResultOID)
		pending, err = os.ReadFile(mergePath)
		if err != nil || strings.TrimSpace(string(pending)) != opts.ResultOID {
			if mergeErr != nil {
				return mergeErr
			}
			return fmt.Errorf("%w: merge did not leave the expected MERGE_HEAD", ErrRecoveryRequired)
		}
		if err := inspectPendingCandidate(lease.Worktree, &result); err != nil {
			return err
		}
		if mergeErr != nil && result.Kind != CandidateConflicts {
			return mergeErr
		}
		return nil
	})
	return result, err
}

func inspectPendingCandidate(path string, result *CandidateResult) error {
	out, err := gitOutput(path, "ls-files", "--unmerged", "-z")
	if err != nil {
		return err
	}
	result.Kind = CandidateNeedsCommit
	if out != "" {
		result.Kind = CandidateConflicts
	}
	return nil
}

func refusePreparationHooks(path string) error {
	cmd := exec.Command("git", "config", "--get", "core.fsmonitor")
	cmd.Dir = path
	out, err := cmd.Output()
	if err == nil {
		value := strings.TrimSpace(string(out))
		if value != "" && value != "true" && value != "false" && value != "0" && value != "1" {
			return fmt.Errorf("candidate preparation would execute fsmonitor hook; explicit approval required")
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fmt.Errorf("reading fsmonitor configuration: %w", err)
		}
	}
	// Config-defined hooks are independent of core.hooksPath; refuse their preparation events even when disabled.
	cmd = exec.Command("git", "config", "--null", "--get-regexp", `^hook\..*\.event$`)
	cmd.Dir = path
	out, err = cmd.Output()
	if err == nil {
		for record := range strings.SplitSeq(string(out), "\x00") {
			_, event, _ := strings.Cut(record, "\n")
			if event == "post-index-change" || event == "reference-transaction" {
				return fmt.Errorf("candidate preparation would execute configured %s hook; explicit approval required", event)
			}
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fmt.Errorf("reading configured hooks: %w", err)
		}
	}
	hooks, err := gitStatePath(path, "hooks")
	if err != nil {
		return err
	}
	for _, name := range []string{"post-index-change", "reference-transaction"} {
		info, err := os.Stat(filepath.Join(hooks, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return fmt.Errorf("candidate preparation would execute %s hook; explicit approval required", name)
		}
	}
	return nil
}

type DeliveryState string

const (
	DeliveryAtTarget    DeliveryState = "at_target"
	DeliveryAtCandidate DeliveryState = "at_candidate"
	DeliveryAtOther     DeliveryState = "at_other"
)

// Both tips must be persisted before UpdateDelivery is called.
type DeliveryUpdate struct {
	RepoPath     string
	Branch       string
	Owner        BranchOwnership
	ExpectedOID  string
	CandidateOID string
}

type DeliveryResult struct {
	State      DeliveryState
	CurrentOID string // Empty if the delivery ref was removed.
	Updated    bool   // This invocation performed the compare-and-swap.
}

// Inspection changes no Git state but takes the persistent lease lock.
// Equal target and candidate tips count as an already-applied update.
func InspectDelivery(update DeliveryUpdate) (result DeliveryResult, err error) {
	err = withOwnedBranch(update.RepoPath, update.Branch, update.Owner, func(ref string, lease BranchOwnership) error {
		if err := validateDeliveryUpdate(update, ref, lease); err != nil {
			return err
		}
		var inspectErr error
		result, inspectErr = inspectDelivery(update, ref)
		return inspectErr
	})
	return result, err
}

// Only descendants of the expected tip are accepted, preserving delivery commits.
// Candidate equality acknowledges a completed update; other tips return ErrDeliveryMoved.
// Required ref hooks can fail after an update. InspectDelivery resolves uncertain outcomes.
// The caller must verify and approve any newly created candidate commit separately.
func UpdateDelivery(update DeliveryUpdate) (result DeliveryResult, err error) {
	err = withOwnedBranch(update.RepoPath, update.Branch, update.Owner, func(ref string, lease BranchOwnership) error {
		if err := validateDeliveryUpdate(update, ref, lease); err != nil {
			return err
		}
		var err error
		result, err = inspectDelivery(update, ref)
		if err != nil {
			return err
		}
		switch result.State {
		case DeliveryAtCandidate:
			return nil
		case DeliveryAtOther:
			return ErrDeliveryMoved
		case DeliveryAtTarget:
		}
		_, updateErr := gitOutput(update.RepoPath, "update-ref", "--no-deref", ref, update.CandidateOID, update.ExpectedOID)
		result, err = inspectDelivery(update, ref)
		if err != nil {
			return err
		}
		if updateErr != nil {
			return updateErr
		}
		result.Updated = true
		if result.State != DeliveryAtCandidate {
			return ErrDeliveryMoved
		}
		return nil
	})
	return result, err
}

func validateDeliveryUpdate(update DeliveryUpdate, ref string, lease BranchOwnership) error {
	if err := requireDelivery(update.RepoPath, ref, lease); err != nil {
		return err
	}
	for _, oid := range []string{update.ExpectedOID, update.CandidateOID} {
		if err := fullCommit(update.RepoPath, oid); err != nil {
			return err
		}
	}
	ancestor, err := isAncestor(update.RepoPath, update.ExpectedOID, update.CandidateOID)
	if err != nil {
		return err
	}
	if !ancestor {
		return fmt.Errorf("candidate is not a descendant of expected delivery tip")
	}
	return nil
}

func requireDelivery(repo, ref string, owner BranchOwnership) error {
	if owner.Worktree != "" {
		return fmt.Errorf("delivery branch must not reserve a workspace")
	}
	path, err := registeredBranch(repo, ref)
	if err != nil {
		return err
	}
	if path != "" {
		return fmt.Errorf("%w: delivery branch checked out at %s", ErrRecoveryRequired, path)
	}
	return nil
}

func inspectDelivery(update DeliveryUpdate, ref string) (DeliveryResult, error) {
	current, err := directOID(update.RepoPath, ref)
	if err != nil {
		return DeliveryResult{}, err
	}
	result := DeliveryResult{CurrentOID: current, State: DeliveryAtOther}
	switch current {
	case update.CandidateOID:
		result.State = DeliveryAtCandidate
	case update.ExpectedOID:
		result.State = DeliveryAtTarget
	}
	return result, nil
}

func isAncestor(repo, ancestor, descendant string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = repo
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("checking commit ancestry: %w", err)
	}
	return true, nil
}
