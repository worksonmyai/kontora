package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func automationOwner(run automation.Run) worktree.BranchOwnership {
	return worktree.BranchOwnership{Kind: "automation", AutomationID: run.AutomationID, RunNumber: run.N, Worktree: run.Worktree}
}

func automationGit(ctx context.Context, repo string, args ...string) (string, error) {
	var out bytes.Buffer
	result, err := process.Run(ctx, process.RunParams{Binary: "git", Args: args, Dir: repo, Stdout: &out, Stderr: &out, RequireTermination: true})
	if result.Interruption != nil {
		err = errors.Join(err, result.Interruption)
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(out.String()), err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git %s: %s (exit %d)", strings.Join(args, " "), strings.TrimSpace(out.String()), result.ExitCode)
	}
	return strings.TrimSpace(out.String()), nil
}

// The exact-worktree API cannot cancel Git commands. A deadline cannot prove that an operation stopped.
func automationWorkspaceOperation(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errors.Join(ctx.Err(), process.ErrTerminationUncertain)
	}
}

func (d *Daemon) prepareAutomationWorkspace(ctx context.Context, run automation.Run, launch automationLaunch) (automation.Run, error) {
	if err := ctx.Err(); err != nil {
		return run, err
	}
	// A queued symlink retarget must not move this run into a different checkout.
	repo, err := canonicalAutomationPath(launch.RepoPath)
	if err != nil {
		return run, err
	}
	if repo != launch.RepoPath {
		return run, errors.New("frozen repository path changed")
	}
	base, err := automationGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return run, err
	}
	run.BaseCommit, run.Worktree = base, launch.Worktree
	if run.Spec.Workspace == "worktree" {
		run.Branch = fmt.Sprintf("auto/%s-%d", run.Key, run.N)
	} else {
		branch, branchErr := automationGit(ctx, repo, "symbolic-ref", "--quiet", "--short", "HEAD")
		if branchErr == nil {
			run.Branch = branch
		} else if errors.Is(branchErr, context.Canceled) || errors.Is(branchErr, context.DeadlineExceeded) || errors.Is(branchErr, process.ErrTerminationUncertain) {
			return run, branchErr
		}
	}
	updated, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(current *automation.Run) error {
		if current.Phase != "preparing" || current.StopReason != "" || ctx.Err() != nil {
			return errors.Join(automation.ErrConflict, ctx.Err())
		}
		current.BaseCommit, current.Worktree, current.Branch = run.BaseCommit, run.Worktree, run.Branch
		return nil
	})
	if err != nil {
		return run, err
	}
	run = updated
	if run.Spec.Workspace == "repo" {
		return run, nil
	}
	owner := automationOwner(run)
	err = automationWorkspaceOperation(ctx, func() error {
		if err := worktree.ReserveBranch(repo, run.Branch, owner); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := worktree.CreateExact(worktree.ExactOpts{RepoPath: repo, Branch: run.Branch, Owner: owner, OID: base})
		return err
	})
	if err != nil {
		return run, err
	}
	if err := ctx.Err(); err != nil {
		return run, err
	}
	if err := d.automationEvent(run, "lifecycle", fmt.Sprintf("git worktree add %s · branch %s", run.Worktree, run.Branch)); err != nil {
		return run, err
	}
	return run, d.runAutomationHooks(ctx, launch, run, config.HookWorktreeCreated, nil, "")
}

func (d *Daemon) cleanupAutomationWorkspace(ctx context.Context, run automation.Run) error {
	if run.Spec.Workspace != "worktree" || run.Worktree == "" || run.Branch == "" || run.BaseCommit == "" {
		return nil
	}
	launch, err := readAutomationLaunch(run)
	if err != nil {
		return err
	}
	head, err := automationGit(ctx, run.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != run.BaseCommit {
		return d.automationEvent(run, "lifecycle", "worktree retained: run left commits")
	}
	status, err := automationGit(ctx, run.Worktree, "status", "--porcelain", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return err
	}
	if status != "" {
		return d.automationEvent(run, "lifecycle", "worktree retained: uncommitted files")
	}
	ref := fmt.Sprintf("refs/kontora/results/automations/%s/%d", run.AutomationID, run.N)
	if err := automationWorkspaceOperation(ctx, func() error {
		return worktree.RemoveExact(launch.RepoPath, run.Branch, automationOwner(run), run.BaseCommit, ref)
	}); err != nil {
		return err
	}
	return d.automationEvent(run, "lifecycle", "clean worktree removed")
}

const automationHookPending = "hook_termination_unconfirmed"

func (d *Daemon) automationHookState(run automation.Run, pending bool) error {
	_, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(current *automation.Run) error {
		if current.InvocationID != run.InvocationID || current.Phase == "terminal" {
			return automation.ErrConflict
		}
		current.Termination = ""
		if pending {
			current.Termination = automationHookPending
		}
		return nil
	})
	return err
}

func (d *Daemon) runAutomationHooks(ctx context.Context, launch automationLaunch, run automation.Run, event string, exit *int, outcome string) error {
	specs := launch.Hooks[event]
	if len(specs) == 0 {
		return ctx.Err()
	}
	out, err := openAutomationArtifact(launch.Paths.Hooks)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	env := automationExecutionEnv(launch)
	env["KONTORA_EVENT"], env["KONTORA_WORKTREE"], env["KONTORA_REPO_PATH"] = event, run.Worktree, launch.RepoPath
	env["KONTORA_BRANCH"], env["KONTORA_PROJECT"], env["KONTORA_RUN_OUTCOME"] = run.Branch, launch.Project, outcome
	env["KONTORA_TICKET_ID"], env["KONTORA_TICKET_FILE"], env["KONTORA_STAGE"], env["KONTORA_EXIT_CODE"] = "", "", "", ""
	if exit != nil {
		env["KONTORA_EXIT_CODE"] = fmt.Sprint(*exit)
	}
	for i, spec := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := spec.Name
		if name == "" {
			name = fmt.Sprintf("%s[%d]", event, i)
		}
		if _, err := fmt.Fprintf(out, "\n=== %s %s %s ===\n", time.Now().UTC().Format(time.RFC3339), event, name); err != nil {
			return err
		}
		hookCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
		if err := d.automationHookState(run, true); err != nil {
			cancel()
			return err
		}
		result, runErr := process.Run(hookCtx, process.RunParams{Binary: "/bin/sh", Args: []string{"-c", spec.Run}, Dir: run.Worktree, Env: envPairs(env), Stdout: out, Stderr: out, RequireTermination: true})
		interruption := errors.Join(hookCtx.Err(), result.Interruption)
		cancel()
		if result.Termination != process.TerminationNotStarted && result.Termination != process.TerminationProcessGroupGone {
			runErr = errors.Join(runErr, process.ErrTerminationUncertain)
		} else if err := d.automationHookState(run, false); err != nil {
			runErr = errors.Join(runErr, process.ErrTerminationUncertain, err)
		}
		runErr = errors.Join(runErr, interruption)
		if runErr == nil && result.ExitCode != 0 {
			runErr = fmt.Errorf("exited with code %d", result.ExitCode)
		}
		if runErr == nil {
			continue
		}
		runErr = fmt.Errorf("hook %s: %w", name, runErr)
		if spec.Fatal || ctx.Err() != nil || errors.Is(runErr, process.ErrTerminationUncertain) {
			return runErr
		}
		d.log.Warn("automation hook failed", "automation", run.AutomationID, "run", run.N, "err", runErr)
	}
	return out.Sync()
}
