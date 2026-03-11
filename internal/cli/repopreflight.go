package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// CheckRepo validates that the given path is a git repository with at least
// one commit and that a default branch can be detected.
func CheckRepo(path string) error {
	path = config.ExpandTilde(path)

	if err := git(path, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("path %q: not a git repository", path)
	}
	if err := git(path, "rev-parse", "HEAD"); err != nil {
		return fmt.Errorf("path %q: repository has no commits", path)
	}
	if _, err := worktree.DetectDefaultBranch(path); err != nil {
		return fmt.Errorf("path %q: %w", path, err)
	}
	return nil
}

// GitRoot returns the root directory of the current git repository.
func GitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repository; use --path")
	}
	return strings.TrimSpace(string(out)), nil
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.Run()
}
