package worktree

import (
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

func (m *Manager) Create(repoPath, repoName, taskID, branch string) (wtPath string, created bool, err error) {
	wtPath = m.Path(repoName, taskID)

	if _, err := os.Stat(wtPath); err == nil {
		return wtPath, false, nil
	}

	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return "", false, fmt.Errorf("creating worktree parent dir: %w", err)
	}

	base, err := DetectDefaultBranch(repoPath)
	if err != nil {
		return "", false, fmt.Errorf("detecting default branch: %w", err)
	}

	cmd := exec.Command("git", "worktree", "add", "-b", branch, wtPath, base)
	cmd.Dir = repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if !strings.Contains(msg, "already exists") {
			return "", false, fmt.Errorf("git worktree add: %s: %w", msg, err)
		}
		// Branch exists but worktree directory is gone (e.g. after crash or manual removal).
		// Reuse the existing branch.
		cmd = exec.Command("git", "worktree", "add", wtPath, branch)
		cmd.Dir = repoPath
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

func (m *Manager) Remove(repoPath, repoName, taskID, _ string) error {
	wtPath := m.Path(repoName, taskID)

	// Check if the worktree directory exists; if not, nothing to do.
	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		return nil
	}

	// Refuse to remove a dirty worktree.
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
