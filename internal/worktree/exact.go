package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrRecoveryRequired means existing Git state was preserved, not repaired.
var ErrRecoveryRequired = errors.New("worktree requires explicit recovery")

// ReserveBranch must establish Owner.Worktree before projection. Retries require
// the same branch, path, clean index/worktree, and exact full commit.
type ExactOpts struct {
	RepoPath string
	Branch   string
	Owner    BranchOwnership
	OID      string
}

type ExactResult struct {
	Path    string
	OID     string
	Created bool
}

// A branch-only interruption is recoverable; other partial checkouts require inspection.
// Checkout hooks remain enabled. Errors preserve branches, workspaces, and reservations.
func CreateExact(opts ExactOpts) (result ExactResult, err error) {
	err = withOwnedBranch(opts.RepoPath, opts.Branch, opts.Owner, func(ref string, owner BranchOwnership) error {
		if owner.Worktree == "" {
			return fmt.Errorf("exact workspace requires a reserved path")
		}
		if err := fullCommit(opts.RepoPath, opts.OID); err != nil {
			return err
		}
		result.Path, result.OID = owner.Worktree, opts.OID
		current, err := directOID(opts.RepoPath, ref)
		if err != nil {
			return err
		}
		if current != "" && current != opts.OID {
			return fmt.Errorf("%w: branch %s is at %s, not %s", ErrRecoveryRequired, ref, current, opts.OID)
		}
		registered, err := registeredBranch(opts.RepoPath, ref)
		if err != nil {
			return err
		}
		if registered != "" {
			if registered != owner.Worktree {
				return fmt.Errorf("%w: branch checked out at %s", ErrRecoveryRequired, registered)
			}
			return checkExactWorkspace(opts.RepoPath, ref, owner.Worktree, opts.OID, true)
		}
		if err := checkReservationPath(opts.RepoPath, owner.Worktree); err != nil {
			return fmt.Errorf("%w: %v", ErrRecoveryRequired, err)
		}
		if current == "" {
			if _, err := gitOutput(opts.RepoPath, "update-ref", "--no-deref", ref, opts.OID, zeroOID(opts.OID)); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(owner.Worktree), 0o755); err != nil {
			return err
		}
		if _, err := gitOutput(opts.RepoPath, "worktree", "add", "--", owner.Worktree, strings.TrimPrefix(ref, "refs/heads/")); err != nil {
			return err
		}
		result.Created = true
		return checkExactWorkspace(opts.RepoPath, ref, owner.Worktree, opts.OID, true)
	})
	return result, err
}

// Result references are immutable and must match the owned branch tip.
// Dirty work does not prevent retaining its committed result; reservations remain held.
func RetainResult(repo, branch string, owner BranchOwnership, oid, resultRef string) error {
	return withOwnedBranch(repo, branch, owner, func(ref string, _ BranchOwnership) error {
		return retainResult(repo, ref, oid, resultRef)
	})
}

// Cleanup retains the result first and preserves dirty or in-progress work.
// Branches and leases survive removal; retries verify the retained tip.
func RemoveExact(repo, branch string, owner BranchOwnership, oid, resultRef string) error {
	return withOwnedBranch(repo, branch, owner, func(ref string, lease BranchOwnership) error {
		if lease.Worktree == "" {
			return fmt.Errorf("exact workspace requires a reserved path")
		}
		if err := retainResult(repo, ref, oid, resultRef); err != nil {
			return err
		}
		registered, err := registeredBranch(repo, ref)
		if err != nil {
			return err
		}
		if registered == "" {
			if err := checkReservationPath(repo, lease.Worktree); err != nil {
				return fmt.Errorf("%w: %v", ErrRecoveryRequired, err)
			}
			return nil
		}
		if registered != lease.Worktree {
			return fmt.Errorf("%w: branch checked out at %s", ErrRecoveryRequired, registered)
		}
		if err := checkExactWorkspace(repo, ref, lease.Worktree, oid, true); err != nil {
			return err
		}
		_, err = gitOutput(repo, "worktree", "remove", "--", lease.Worktree)
		return err
	})
}

func InspectRetainedResult(repo, oid, resultRef string) error {
	if !strings.HasPrefix(resultRef, "refs/kontora/results/") {
		return fmt.Errorf("result ref must be under refs/kontora/results/")
	}
	if err := validRef(resultRef); err != nil {
		return err
	}
	if err := fullCommit(repo, oid); err != nil {
		return err
	}
	current, err := directOID(repo, resultRef)
	if err != nil {
		return err
	}
	if current != oid {
		return fmt.Errorf("%w: result reference no longer retains the expected commit", ErrRecoveryRequired)
	}
	return nil
}

func retainResult(repo, ref, oid, resultRef string) error {
	if !strings.HasPrefix(resultRef, "refs/kontora/results/") {
		return fmt.Errorf("result ref must be under refs/kontora/results/")
	}
	if err := validRef(resultRef); err != nil {
		return err
	}
	if err := fullCommit(repo, oid); err != nil {
		return err
	}
	current, err := directOID(repo, ref)
	if err != nil {
		return err
	}
	if current != oid {
		return fmt.Errorf("%w: result tip is %s, expected %s", ErrRecoveryRequired, current, oid)
	}
	existing, err := directOID(repo, resultRef)
	if err != nil {
		return err
	}
	if existing == oid {
		return nil
	}
	if existing != "" {
		return fmt.Errorf("%w: retained result %s already points to %s", ErrRecoveryRequired, resultRef, existing)
	}
	_, err = gitOutput(repo, "update-ref", "--no-deref", resultRef, oid, zeroOID(oid))
	return err
}

// Matching a lease does not acquire one. ReserveBranch refuses existing reservations,
// including reservations for the same owner.
func withOwnedBranch(repo, branch string, owner BranchOwnership, fn func(string, BranchOwnership) error) error {
	ref := branchRef(branch)
	if err := validRef(ref); err != nil {
		return err
	}
	if !owner.valid() {
		return fmt.Errorf("incomplete branch ownership")
	}
	if owner.Worktree != "" {
		var err error
		owner.Worktree, err = resolvedPath(owner.Worktree)
		if err != nil {
			return err
		}
	}
	return withBranchOwnership(repo, func(_ string, leases map[string]BranchOwnership) error {
		lease, ok := leases[ref]
		if !ok || lease != owner {
			return fmt.Errorf("%w: matching reservation required for %s", ErrOwnedBranch, ref)
		}
		return fn(ref, lease)
	})
}

func validRef(ref string) error {
	if err := exec.Command("git", "check-ref-format", ref).Run(); err != nil {
		return fmt.Errorf("invalid ref %q", ref)
	}
	return nil
}

func gitOutput(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)+stderr.String()), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func fullCommit(repo, oid string) error {
	if len(oid) != 40 && len(oid) != 64 {
		return fmt.Errorf("full commit OID required: %q", oid)
	}
	for _, c := range oid {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("full lowercase commit OID required: %q", oid)
		}
	}
	actual, err := gitOutput(repo, "rev-parse", "--verify", oid+"^{commit}")
	if err != nil {
		return err
	}
	if actual != oid {
		return fmt.Errorf("OID %s is not a commit", oid)
	}
	return nil
}

func zeroOID(oid string) string { return strings.Repeat("0", len(oid)) }

// directOID refuses symbolic refs, including dangling aliases, before reading.
func directOID(repo, ref string) (string, error) {
	cmd := exec.Command("git", "symbolic-ref", "--quiet", ref)
	cmd.Dir = repo
	err := cmd.Run()
	if err == nil {
		return "", fmt.Errorf("%w: symbolic ref %s", ErrRecoveryRequired, ref)
	}
	var symbolicExit *exec.ExitError
	if !errors.As(err, &symbolicExit) || symbolicExit.ExitCode() != 1 {
		return "", fmt.Errorf("checking symbolic ref %s: %w", ref, err)
	}
	cmd = exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	return "", fmt.Errorf("reading ref %s: %w", ref, err)
}

// Use the NUL format: paths may contain spaces, newlines or quotes.
func registeredBranch(repo, ref string) (string, error) {
	out, err := gitOutput(repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	path, found := "", ""
	for field := range strings.SplitSeq(out, "\x00") {
		if p, ok := strings.CutPrefix(field, "worktree "); ok {
			path, err = resolvedPath(p)
			if err != nil {
				return "", err
			}
		}
		if field == "branch "+ref {
			if found != "" {
				return "", fmt.Errorf("%w: branch checked out more than once", ErrRecoveryRequired)
			}
			found = path
		}
	}
	return found, nil
}

func checkExactWorkspace(repo, ref, path, oid string, clean bool) error {
	common, err := CommonDir(repo)
	if err != nil {
		return err
	}
	actual, err := CommonDir(path)
	if err != nil || actual != common {
		return fmt.Errorf("%w: workspace is missing or belongs to another repository", ErrRecoveryRequired)
	}
	headRef, err := gitOutput(path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || headRef != ref {
		return fmt.Errorf("%w: workspace HEAD is not %s", ErrRecoveryRequired, ref)
	}
	head, err := gitOutput(path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if head != oid {
		return fmt.Errorf("%w: workspace tip is %s, expected %s", ErrRecoveryRequired, head, oid)
	}
	if clean {
		if err := requireClean(path); err != nil {
			return err
		}
		return requireNoOperation(path)
	}
	return nil
}

func requireClean(path string) error {
	// Git status can hide edits under these index flags. Preserve the flags and workspace for explicit recovery.
	out, err := gitOutput(path, "--no-optional-locks", "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for entry := range strings.SplitSeq(out, "\x00") {
		if len(entry) > 0 && (entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z') {
			return fmt.Errorf("%w: index has assume-unchanged or skip-worktree entries", ErrRecoveryRequired)
		}
	}
	out, err = gitOutput(path, "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if out != "" {
		return ErrDirtyWorktree
	}
	return nil
}

func gitStatePath(path, name string) (string, error) {
	return gitOutput(path, "rev-parse", "--path-format=absolute", "--git-path", name)
}

func requireNoOperation(path string) error {
	// Operation markers belong to the per-worktree Git directory, not the common directory.
	dir, err := gitOutput(path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return err
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer", "BISECT_START"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%w: %s in progress", ErrRecoveryRequired, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
