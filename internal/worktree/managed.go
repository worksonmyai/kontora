package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

var ErrOwnedBranch = errors.New("branch or worktree belongs to an orchestrator")

type BranchOwnership struct {
	BoardID     string `json:"board_id"`
	ObjectiveID string `json:"objective_id"`
	Generation  uint64 `json:"generation"`
	Worktree    string `json:"worktree,omitempty"`
}

func CommonDir(repo string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving git common directory: %w", err)
	}
	return filepath.EvalSymlinks(strings.TrimSpace(string(out)))
}

func branchRef(branch string) string {
	return "refs/heads/" + strings.TrimPrefix(branch, "refs/heads/")
}

func withBranchOwnership(repo string, fn func(string, map[string]BranchOwnership) error) error {
	return withBranchOwnershipSync(repo, syncDirectory, fn)
}

func withBranchOwnershipSync(repo string, syncParent func(string) error, fn func(string, map[string]BranchOwnership) error) error {
	common, err := CommonDir(repo)
	if err != nil {
		return err
	}
	dir := filepath.Join(common, "kontora")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Persist the ownership directory's entry before acknowledging any lease.
	if err := syncParent(common); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "branches.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	path := filepath.Join(dir, "branches.json")
	leases := map[string]BranchOwnership{}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &leases); err != nil {
			return fmt.Errorf("reading branch ownership: %w", err)
		}
		if leases == nil {
			return fmt.Errorf("invalid branch ownership")
		}
		for ref, owner := range leases {
			if !strings.HasPrefix(ref, "refs/heads/") || owner.BoardID == "" || owner.ObjectiveID == "" || owner.Generation == 0 || (owner.Worktree != "" && !filepath.IsAbs(owner.Worktree)) {
				return fmt.Errorf("invalid branch ownership for %q", ref)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fn(path, leases)
}

// ReserveBranch persists ownership before a managed worktree can be projected.
// Reservations remain held after projection errors and across daemon restarts.
func ReserveBranch(repo, branch string, owner BranchOwnership) error {
	if owner.BoardID == "" || owner.ObjectiveID == "" || owner.Generation == 0 {
		return fmt.Errorf("incomplete branch ownership")
	}
	ref := branchRef(branch)
	if err := exec.Command("git", "check-ref-format", ref).Run(); err != nil {
		return fmt.Errorf("invalid branch ref %q", ref)
	}
	if owner.Worktree != "" {
		var err error
		owner.Worktree, err = resolvedPath(owner.Worktree)
		if err != nil {
			return err
		}
	}
	return withBranchOwnership(repo, func(path string, leases map[string]BranchOwnership) error {
		if err := refuseOwned(repo, leases, branch, owner.Worktree); err != nil {
			return err
		}
		if owner.Worktree != "" {
			if err := checkReservationPath(repo, owner.Worktree); err != nil {
				return err
			}
		}
		existing, err := FindWorktreeForBranch(repo, strings.TrimPrefix(ref, "refs/heads/"))
		if err != nil {
			return err
		}
		if existing != "" {
			return fmt.Errorf("cannot adopt an existing worktree: %s", existing)
		}
		cmd := exec.Command("git", "show-ref", "--verify", "--quiet", ref)
		cmd.Dir = repo
		err = cmd.Run()
		if err == nil {
			return fmt.Errorf("cannot adopt an existing branch: %s", ref)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fmt.Errorf("checking existing branch: %w", err)
		}
		leases[ref] = owner
		data, err := json.Marshal(leases)
		if err != nil {
			return err
		}
		f, err := os.CreateTemp(filepath.Dir(path), "branches-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Rename(f.Name(), path); err != nil {
			return err
		}
		return syncDirectory(filepath.Dir(path))
	})
}

func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func checkReservationPath(repo, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("cannot adopt an existing worktree path: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cmd := exec.Command("git", "worktree", "list", "--porcelain", "-z")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("listing registered worktrees: %w", err)
	}
	for field := range strings.SplitSeq(string(out), "\x00") {
		if registered, ok := strings.CutPrefix(field, "worktree "); ok {
			registered, err = resolvedPath(registered)
			if err != nil {
				return err
			}
			if registered == path {
				return fmt.Errorf("cannot adopt a registered worktree path: %s", path)
			}
		}
	}
	return nil
}

func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent, err := resolvedPath(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func refuseOwned(repo string, leases map[string]BranchOwnership, branch, wt string) error {
	if branch != "" {
		ref := branchRef(branch)
		if _, ok := leases[ref]; ok {
			return ErrOwnedBranch
		}
		// Git follows symbolic branch aliases when checking out or updating refs.
		cmd := exec.Command("git", "symbolic-ref", "--quiet", ref)
		cmd.Dir = repo
		err := cmd.Run()
		if err == nil {
			return fmt.Errorf("%w: symbolic branch reference %s", ErrOwnedBranch, ref)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fmt.Errorf("checking symbolic branch reference: %w", err)
		}
	}
	if wt != "" {
		path, err := resolvedPath(wt)
		if err != nil {
			return err
		}
		for _, lease := range leases {
			if lease.Worktree != "" && lease.Worktree == path {
				return ErrOwnedBranch
			}
		}
	}
	return nil
}

func checkWorktreeOwnership(repo, wt string, leases map[string]BranchOwnership) error {
	if len(leases) != 0 {
		if _, err := os.Stat(wt); err == nil {
			cmd := exec.Command("git", "symbolic-ref", "--quiet", "HEAD")
			cmd.Dir = wt
			out, err := cmd.Output()
			if err != nil {
				return fmt.Errorf("%w: detached or unrecognized worktree", ErrOwnedBranch)
			}
			if err := refuseOwned(repo, leases, strings.TrimSpace(string(out)), ""); err != nil {
				return err
			}
		}
	}
	if err := refuseOwned(repo, leases, "", wt); err != nil {
		return err
	}
	// Consult Git's registration even if the directory was deleted or HEAD changed.
	for ref := range leases {
		path, err := FindWorktreeForBranch(repo, strings.TrimPrefix(ref, "refs/heads/"))
		if err != nil {
			return err
		}
		if path != "" {
			actual, err := resolvedPath(path)
			if err != nil {
				return err
			}
			target, err := resolvedPath(wt)
			if err != nil {
				return err
			}
			if actual == target {
				return ErrOwnedBranch
			}
		}
	}
	return nil
}
