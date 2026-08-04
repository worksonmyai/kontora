package worktree

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Manager struct {
	worktreesDir string
}

func New(worktreesDir string) *Manager {
	return &Manager{
		worktreesDir: worktreesDir,
	}
}

func BranchName(prefix, taskID string) string {
	return prefix + "/" + taskID
}

func (m *Manager) Path(repoName, taskID string) string {
	return filepath.Join(m.worktreesDir, repoName, taskID)
}

// FindWorktreeForBranch returns the path of the worktree checked out on the
// given branch in repoPath, or "" if none exists. Parses `git worktree list --porcelain`.
func FindWorktreeForBranch(repoPath, branch string) (string, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return "", fmt.Errorf("git worktree list: %s: %w", msg, err)
		}
		return "", fmt.Errorf("git worktree list: %w", err)
	}

	wantRef := "refs/heads/" + branch

	var curPath string
	var detached bool
	var curBranch string

	reset := func() {
		curPath = ""
		detached = false
		curBranch = ""
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if curPath != "" && !detached && curBranch == wantRef {
				return curPath, nil
			}
			reset()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			curPath = strings.TrimPrefix(line, "worktree ")
		case line == "detached":
			detached = true
		case strings.HasPrefix(line, "branch "):
			curBranch = strings.TrimPrefix(line, "branch ")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scanning worktree list: %w", err)
	}
	// Handle the final stanza if the output didn't end with a blank line.
	if curPath != "" && !detached && curBranch == wantRef {
		return curPath, nil
	}
	return "", nil
}

// CreateOpts describes the worktree to create.
type CreateOpts struct {
	RepoPath string
	RepoName string
	TaskID   string
	Branch   string
	// Base names the branch the work branch starts from. Empty means the
	// repository's default branch. It is read only when the worktree is first
	// created; an existing worktree for Branch is reused as-is.
	Base string
}

func (m *Manager) Create(opts CreateOpts) (wtPath string, created bool, err error) {
	// Reject a base that names the work branch before anything else. Behind the
	// reuse short-circuit below, the collision would be silent: `git worktree
	// add -b develop <path> develop` fails with "a branch named 'develop'
	// already exists", the recovery path further down reads that as "reuse the
	// branch", and the agent commits straight onto the base.
	if opts.Base != "" && strings.TrimSpace(opts.Base) == opts.Branch {
		return "", false, fmt.Errorf("base branch %q is the ticket's own work branch", opts.Branch)
	}

	// Ask git, not the filesystem, whether a worktree for this branch already
	// exists. This handles old-layout worktrees, user-created worktrees in
	// arbitrary locations, and idempotent re-entry for this ticket.
	if existing, err := FindWorktreeForBranch(opts.RepoPath, opts.Branch); err != nil {
		return "", false, fmt.Errorf("finding existing worktree: %w", err)
	} else if existing != "" {
		return existing, false, nil
	}

	wtPath = m.Path(opts.RepoName, opts.TaskID)

	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return "", false, fmt.Errorf("creating worktree parent dir: %w", err)
	}

	// Resolve symlinks in the parent directory so the path we hand to git
	// matches what `git worktree list --porcelain` will later report for this
	// branch. Without this, macOS /tmp -> /private/tmp diverges and callers
	// like plannotator can't use FindWorktreeForBranch to round-trip the path.
	resolved, err := filepath.EvalSymlinks(filepath.Dir(wtPath))
	if err != nil {
		return "", false, fmt.Errorf("resolving worktree parent symlinks: %w", err)
	}
	wtPath = filepath.Join(resolved, filepath.Base(wtPath))

	base, err := ResolveBase(opts.RepoPath, opts.Base)
	if err != nil {
		return "", false, err
	}

	cmd := exec.Command("git", "worktree", "add", "-b", opts.Branch, wtPath, base)
	cmd.Dir = opts.RepoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if !strings.Contains(msg, "already exists") {
			return "", false, fmt.Errorf("git worktree add: %s: %w", msg, err)
		}
		// Branch exists but worktree directory is gone (e.g. after crash or manual removal).
		// Reuse the existing branch.
		cmd = exec.Command("git", "worktree", "add", wtPath, opts.Branch)
		cmd.Dir = opts.RepoPath
		if out, err := cmd.CombinedOutput(); err != nil {
			// Another goroutine may have created the worktree concurrently.
			if _, statErr := os.Stat(wtPath); statErr == nil {
				return wtPath, false, nil
			}
			return "", false, fmt.Errorf("git worktree add (existing branch): %s: %w", strings.TrimSpace(string(out)), err)
		}
	}

	return wtPath, true, nil
}

// ResolveBase returns the ref to branch from. An empty base falls back to the
// repository's default branch. A non-empty base must name a branch: it is
// looked up as a local branch first, then as a remote-tracking branch, so
// "develop" and "origin/develop" both work while tags, raw SHAs, and revisions
// like "HEAD~1" do not. Kontora never fetches, so a branch that exists only on
// the remote does not resolve.
func ResolveBase(repoPath, base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		branch, err := DetectDefaultBranch(repoPath)
		if err != nil {
			return "", fmt.Errorf("detecting default branch: %w", err)
		}
		return branch, nil
	}
	for _, ns := range []string{"refs/heads/", "refs/remotes/"} {
		cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ns+base)
		cmd.Dir = repoPath
		if err := cmd.Run(); err == nil {
			return ns + base, nil
		}
	}
	return "", fmt.Errorf("base branch %q not found in %s (looked for a local or remote-tracking branch)", base, repoPath)
}

// DetectDefaultBranch determines the default branch for a git repository.
// It first checks the symbolic ref of origin/HEAD, then falls back to
// whatever branch HEAD points to (covers local repos with no remote).
func DetectDefaultBranch(repoPath string) (string, error) {
	// Try origin/HEAD first (works when a remote is configured).
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = repoPath
	if out, err := cmd.Output(); err == nil {
		ref := strings.TrimSpace(string(out))
		// ref looks like "refs/remotes/origin/main"
		if _, branch, ok := strings.Cut(ref, "refs/remotes/origin/"); ok && branch != "" {
			return branch, nil
		}
	}

	// Fall back to whatever branch HEAD points to (covers local repos with no remote).
	cmd = exec.Command("git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err == nil {
		if branch := strings.TrimSpace(string(out)); branch != "" {
			return branch, nil
		}
	}

	return "", fmt.Errorf("could not detect default branch in %s (no origin/HEAD ref, HEAD fallback failed: %s: %w)", repoPath, strings.TrimSpace(string(out)), err)
}

// ErrDirtyWorktree is returned when attempting to remove a worktree that has
// uncommitted changes (modified, staged, or untracked files).
var ErrDirtyWorktree = fmt.Errorf("worktree has uncommitted changes")

// Remove discovers the worktree for the given branch via git and removes it.
// No-op when no worktree holds the branch. Returns ErrDirtyWorktree when the
// discovered worktree has uncommitted changes.
func (m *Manager) Remove(repoPath, branch string) error {
	wtPath, err := FindWorktreeForBranch(repoPath, branch)
	if err != nil {
		return fmt.Errorf("finding worktree for branch: %w", err)
	}
	if wtPath == "" {
		return nil
	}
	return m.RemoveAt(repoPath, wtPath)
}

// RemoveAt removes the worktree at wtPath directly, skipping branch discovery.
// Use this in hot cleanup paths where the caller already knows the worktree
// path. No-op when wtPath is empty. Returns ErrDirtyWorktree when the worktree
// has uncommitted changes.
func (m *Manager) RemoveAt(repoPath, wtPath string) error {
	if wtPath == "" {
		return nil
	}

	if dirty, err := isDirty(wtPath); err != nil {
		return fmt.Errorf("checking worktree status: %w", err)
	} else if dirty {
		return ErrDirtyWorktree
	}

	cmd := exec.Command("git", "worktree", "remove", wtPath)
	cmd.Dir = repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		if !isAlreadyGone(string(out)) {
			return fmt.Errorf("git worktree remove: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}

	return nil
}

// isDirty returns true if the worktree has any modified, staged, or untracked files.
func isDirty(wtPath string) (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

func isAlreadyGone(output string) bool {
	return strings.Contains(output, "not found") ||
		strings.Contains(output, "not a valid") ||
		strings.Contains(output, "is not a working tree")
}
