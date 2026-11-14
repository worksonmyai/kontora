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

// DeploymentEvidence freezes the canonical Git common directory and the current
// worktree HEAD commit. The worktree must belong to repoPath. It does not inspect
// or modify the index or working files.
func DeploymentEvidence(repoPath, worktreePath string) (repository, commit string, err error) {
	repository, err = deploymentCommonDir(repoPath)
	if err != nil {
		return "", "", err
	}
	common, err := deploymentCommonDir(worktreePath)
	if err != nil {
		return "", "", err
	}
	if common != repository {
		return "", "", fmt.Errorf("deployment worktree belongs to a different repository")
	}
	inside, err := deploymentGit(worktreePath, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return "", "", err
	}
	if inside != "true" {
		return "", "", fmt.Errorf("deployment evidence requires a worktree")
	}
	commit, err = deploymentGit(worktreePath, "rev-parse", "--verify", "HEAD")
	if err == nil {
		err = deploymentCommit(worktreePath, commit)
	}
	if err != nil {
		return "", "", err
	}
	return repository, commit, nil
}

// DeploymentMatches checks immutable local evidence, not refs or content
// equivalence. A different repository identity is a non-match. Invalid commits,
// unavailable history and unsupported Git options are errors, never proof.
// Non-equal commits require complete, non-shallow history; equal commits need no
// ancestry traversal. No objects are fetched and replacement refs are ignored.
func DeploymentMatches(repoPath, repository, required, deployed string) (bool, error) {
	common, err := deploymentCommonDir(repoPath)
	if err != nil {
		return false, err
	}
	if repository != common {
		return false, nil
	}
	for _, oid := range []string{required, deployed} {
		if err := deploymentCommit(repoPath, oid); err != nil {
			return false, err
		}
	}
	if required == deployed {
		return true, nil
	}
	shallow, err := deploymentGit(repoPath, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, err
	}
	if shallow != "false" {
		return false, fmt.Errorf("deployment ancestry requires complete, non-shallow history")
	}
	// Do not let merge-base's early exit or generation/date pruning conceal
	// missing parents. Ignore commit-graph acceleration in all these reads.
	if _, err := deploymentGit(repoPath, "rev-list", "--quiet", required, deployed, "--"); err != nil {
		return false, err
	}
	_, err = deploymentGit(repoPath, "merge-base", "--is-ancestor", required, deployed)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func deploymentCommonDir(repo string) (string, error) {
	path, err := deploymentGit(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolving deployment repository identity: %w", err)
	}
	return filepath.Abs(path)
}

func deploymentCommit(repo, oid string) error {
	if len(oid) != 40 && len(oid) != 64 {
		return fmt.Errorf("deployment requires a full lowercase commit ID: %q", oid)
	}
	for _, c := range oid {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("deployment requires a full lowercase commit ID: %q", oid)
		}
	}
	actual, err := deploymentGit(repo, "rev-parse", "--verify", "--end-of-options", oid)
	if err != nil {
		return err
	}
	if actual != oid {
		return fmt.Errorf("deployment requires a full commit ID: %q", oid)
	}
	kind, err := deploymentGit(repo, "cat-file", "-t", oid)
	if err != nil {
		return err
	}
	if kind != "commit" {
		return fmt.Errorf("deployment object %s is not a commit", oid)
	}
	return nil
}

// Keep deployment reads separate from the package's general Git helpers. Never
// retry without these options: old Git must fail closed. Inherited Git variables
// must not redirect repository/object discovery, inject config, or hide shallow
// boundaries. Grafts, like replacement refs, must not rewrite ancestry.
func deploymentGit(repo string, args ...string) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("deployment repository path is required")
	}
	argv := make([]string, 0, 4+len(args))
	argv = append(argv, "--no-lazy-fetch", "--no-replace-objects", "-c", "core.commitGraph=false")
	cmd := exec.Command("git", append(argv, args...)...)
	cmd.Dir = repo
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_GRAFT_FILE="+os.DevNull, "GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("deployment git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}
