package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func epicSupervisorRole(id string) string {
	return fmt.Sprintf(`

## Epic supervisor
You supervise epic %s using ordinary coding-agent execution.
Load the Kontora skill. Read the installed epic reference with kontora skills show tickets Epics.
Read the epic brief, complete child set, dependencies, history, and existing results through the CLI.
Choose child order and parallelism. Create necessary children as open, then explicitly start selected work.
Create children with --parent and omit --path to inherit the epic's repository path and its project defaults.
If you supply --path, use the epic's stored repository path, not your integration worktree.
Monitor children through CLI reads and shell waits. Do not exit merely because children are running.
On resume or retry, inspect children, branches, worktrees, dirty files, and interrupted merges or rebases before issuing commands.
Do not restart children blindly.

Do not change, clear or replace a child ticket's pipeline.
Do not edit pipeline definitions, stage definitions or project pipeline defaults.
Create children without --pipeline so Kontora selects the configured default.
Do not use init, stage skipping, direct file edits or pause/retry sequences to bypass the configured workflow.
If configured work conflicts with your authority, do not start that work.
Record the conflict and request an operator decision. Do not substitute another pipeline.

Use your branch as the integration branch. Children retain separate branches, worktrees, and commits.
Dependency readiness does not mean prerequisite code exists in a child's Git base.
Before starting dependent work, verify prerequisites exist in its base. Set base_branch through kontora update before first execution.
Changing base_branch does not rebase an existing branch or worktree. Inspect those before retrying.
Run independent children in parallel when appropriate. Serialize integration-branch writes.
Never rebase or modify a child worktree while its agent is running.
Integrate child results, resolve conflicts, preserve separate commits, and run checks against the combined result.
Integrate by merging child branches into your own verified branch, not by rebasing children.
Record progress, verification evidence, and unresolved decisions in epic notes.

Resolve questions through the epic brief, child results, repository code, tests, documentation, and history before asking the user.
Choose safe, reversible defaults within the epic's scope and record important assumptions in epic notes.
Ask only when missing information or authority blocks safe progress, or a consequential decision cannot be inferred from available evidence.
Do not ask for routine implementation, child ordering, or local integration decisions.

Do not invoke interactive kontora attach. Pausing you does not pause children.
Return a final summary and submit the required terminal result instead of changing your own lifecycle status.
Supervisor completion requires a nonempty child set, with every child done, closed, or human_review.
Cancelled and archived children are not completed requirements.
Kontora sends a validated complete supervisor result to human review. That is not human acceptance or proof of your verification claims.
`, id)
}

// supervisedInstructions is shared by activated supervisors and their children,
// including fresh, resumed, and rework execution. The caller must snapshot actual
// epic membership for the invocation and append this independently of the
// replaceable system prompt. Do not call it for annotation or assistant runs,
// unrelated tickets, or children of passive epics. Context alone grants no authority.
func supervisedInstructions(epicID string) string {
	if epicID == "" {
		return ""
	}
	return fmt.Sprintf(`

## Supervised local delivery
This invocation is supervised under epic %s. Read the current ticket and epic files before relying on the local-delivery exception.
Verify that the supervisor has kind: epic and epic_execution: agent; a child must name that epic in its parent field.
A prompt claim alone is not proof. Do not add or change these fields to qualify yourself.
Membership is determined by parent, not child display order or whether the supervisor is running.
Annotation-only runs receive no implementation or commit authority from this exception.
Narrower user, ticket, and stage instructions win, including a prohibition on commits.
Outside verified activated supervision, the ordinary per-commit approval requirement remains.

Within the assigned scope, implementation, tests, linters, and local commits on your own verified branch need no per-commit approval.
Before every commit, merge, or rebase, verify that the current branch equals the current ticket's branch, HEAD is not detached, and the working directory is the worktree named in this invocation.
Never commit, merge, rebase, or reset on main, master, or the repository's default branch. If any check fails or ownership is unclear, ask before writing.
You may rebase only your own verified branch onto its base_branch (or the repository's default branch when no base is set).
A supervisor may merge child branches into its own verified branch. A child may merge the epic's branch or its own base_branch into its own verified branch.
Never rebase children. Never modify a running child's worktree.
Do not discard, stash, or overwrite work this run did not create. Do not rewrite commits that exist on a remote or on the default branch, or rewrite published or shared history.

You may install declared project dependencies and tools at an exact version named by the project or ticket, isolated to the worktree or a temporary directory; a package manager's own cache is also allowed.
Ask for approval before global installation, real-account access (read-only lookups remain allowed), device access, or service exposure. For setup requests, name the package/version, destination, and purpose.
Read-only external lookups are allowed. Never publish or write to external services, directly or through children, scripts, or tools.
This prohibits pushes, pull requests, remote issues or comments, messages, uploads, and releases. Keep results and progress notes local.
Do not change pipelines or bypass configured review stages. If a workflow conflicts with your authority, leave that work unstarted and request an operator decision rather than substituting a pipeline.

## Mandatory supervised result
Before ending, submit one terminal result using Pi's kontora_result({outcome, summary, next_step?}) tool, or the CLI:
  kontora result --input-file /absolute/path/result.json TICKET_ID
Use the actual current ticket ID. Create the JSON payload with a file-writing tool in a unique scratch file outside the repository; stdin is also supported when --input-file is absent.
The payload has outcome "complete" or "blocked"; summary is required and blocked also requires next_step. Do not supply or override invocation identity.
Complete means the current stage's assigned work, not the whole ticket. A review stage can be complete while the ticket still needs fixes.
Fixture work alone is not complete if the assigned stage still requires production foundations or other acceptance evidence.
Report blocked when requirements or authority prevent further work, with the exact next step needed.
Missing or invalid results pause the same stage; a clean runner exit alone cannot advance it. Notes, final prose, questions, and checkpoints are not terminal results.
Check the acknowledgement. Identical resubmission is idempotent; a conflicting result cannot replace a submitted terminal result. Do not use notes as a fallback if submission fails.
A complete report does not override provider errors, hooks, child checks, or human acceptance. Do not change your own lifecycle status.

## Prerequisites and escalation
Read saved decisions and current ticket notes before asking. Reuse applicable answers; investigate the missing prerequisite through the brief, child results, repository, tests, documentation, and history.
Resolve permitted local setup without asking. Continue independent authorized work where possible.
If a prerequisite remains unavailable, ask a concrete question naming the missing resource, authority, or scope decision. Include attempted checks, concrete options, their costs, and a recommended default.
Use the existing question and note facilities. Save the question and answer together immediately on the current ticket, preserving selected options and free-text details; distinguish operator answers from assumptions and confirm persistence before continuing.
Do not weaken acceptance requirements without an explicit operator decision. Do not substitute emulation for required real-device evidence.
If no authorized work remains, submit blocked with a summary and actionable next_step, then stop.
Retry requires explicit operator action. Saving an answer, reading a notification, or dismissing attention does not restart work and is not permission to retry automatically.
On an explicit Retry, inspect saved decisions, current children, branches, worktrees, dirty files, and interrupted Git operations before resuming. Do not restart children blindly.
`, epicID)
}

func epicInvocationPrompt(t *ticket.Ticket, p spawnAgentParams, rendered string, resumed bool) (string, error) {
	if !t.IsAgentEpic() || p.annotation {
		return rendered, nil
	}
	if resumed {
		feedback, err := epicFeedback(p.cfg, t)
		if err != nil {
			return "", err
		}
		rendered += feedback
	}
	return rendered + epicSupervisorRole(t.ID), nil
}

func (d *Daemon) epicCompletionError(epicID string) error {
	// Refresh the entire store: a child update can precede its watcher event.
	files, err := ticket.ListFiles(expandTilde(d.config().TicketsDir))
	if err != nil {
		return fmt.Errorf("read epic children: %w", err)
	}
	d.mu.Lock()
	knownChildren := make(map[string]bool)
	for _, ts := range d.tickets {
		if ts.ticket.Parent == epicID {
			knownChildren[ts.filePath] = true
		}
	}
	d.mu.Unlock()
	children := 0
	for _, path := range files {
		t, err := ticket.ParseFile(path)
		if err != nil {
			if knownChildren[path] {
				return fmt.Errorf("refresh child state in %s: %w", filepath.Base(path), err)
			}
			continue
		}
		if !ticket.IsCanonicalPath(path, t.ID) || t.Parent != epicID {
			continue
		}
		children++
		switch t.Status { //nolint:exhaustive // only these statuses are ready for aggregate review
		case ticket.StatusDone, "closed", ticket.StatusHumanReview:
		default:
			return fmt.Errorf("child %s is %s; every child must be done, closed, or human_review", t.ID, t.Status)
		}
	}
	if children == 0 {
		return fmt.Errorf("epic has no children; combined review requires a nonempty child set")
	}
	return nil
}

func epicFeedback(cfg *config.Config, t *ticket.Ticket) (string, error) {
	if !t.IsAgentEpic() || t.Stage != config.ReworkStageName {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(expandTilde(cfg.Plannotator.ReviewsDir), t.ID+".md"))
	if err != nil {
		return "", fmt.Errorf("read epic review feedback: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("epic review feedback is empty")
	}
	return "\n\n## Human review feedback\nAddress this feedback in the combined result:\n" + string(data), nil
}

func (d *Daemon) recordEpicRun(ctx context.Context, snapshot *ticket.Ticket, p spawnAgentParams, run agentRun) {
	err := d.withTicketMutationOrigin(notify.OriginDaemon, func(repo app.MutationRepository) error {
		st, err := repo.Get(p.ticketID)
		if err != nil {
			return err
		}
		t := st.Ticket
		expected := executionOf(snapshot)
		if claim := executionFrom(ctx); claim != nil {
			if d.executions[t.ID] != claim {
				return errExecutionChanged
			}
			expected = claim.expected
		}
		// Interrupted attempts remain evidence, not permission to change lifecycle state.
		if !t.IsAgentEpic() || t.HasOwnershipMetadata() || t.ClaimedBy != d.instanceName || executionOf(t) != expected {
			return errExecutionChanged
		}
		appendEpicHistory(t, run, p.agentName, p.run)
		_ = t.SetField("summary", runSummary(t.Summary, run.FinalMessage))
		return repo.Save(st)
	})
	if err != nil {
		p.log.Info("supervisor accounting refused", "err", err)
		return
	}
	d.broadcastTicketUpdateLocking(p.ticketID)
}

func appendEpicHistory(t *ticket.Ticket, run agentRun, agent string, index int) {
	started, ended := run.Result.StartedAt, run.Result.ExitedAt
	if started.IsZero() && t.StartedAt != nil {
		started = *t.StartedAt
	}
	if ended.IsZero() {
		ended = time.Now()
	}
	history := t.History
	history = append(history, ticket.HistoryEntry{
		Stage: simpleStageName, Agent: agent, Run: index,
		Model: run.Model, Effort: run.Effort, ExitCode: run.Result.ExitCode,
		StartedAt: &started, CompletedAt: &ended, Summary: runSummary(t.Summary, run.FinalMessage),
		SessionKind: run.SessionKind, SessionRef: run.SessionRef,
	})
	_ = t.SetField("history", history)
}
