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
Record progress, verification evidence, and unresolved decisions in epic notes.

Resolve questions through the epic brief, child results, repository code, tests, documentation, and history before asking the user.
Choose safe, reversible defaults within the epic's scope and record important assumptions in epic notes.
Ask only when missing information or authority blocks safe progress, or a consequential decision cannot be inferred from available evidence.
Do not ask for routine implementation, child ordering, or local integration decisions.

You may commit and rebase local branches owned by this epic without asking for per-operation approval.
Before each operation, verify the current branch and worktree. Never commit or rebase on main, master, or the repository's default branch.
If HEAD is detached or branch ownership is unclear, resolve that before writing.
Preserve unrelated work. Do not perform destructive resets or rewrite published or shared history.
Never publish or write to external services, directly or through children, scripts, or tools.
This includes pushes, pull requests, remote issues or comments, messages, uploads, and releases.
Read-only external lookups are allowed. Keep all results and progress notes local.
Do not invoke interactive kontora attach. Pausing you does not pause children.
Return a final summary instead of changing your own lifecycle status.
A successful exit requires a nonempty child set, with every child done, closed, or human_review.
Cancelled and archived children are not completed requirements.
Kontora sends a successful result to human review. That is not human acceptance or proof of your verification claims.
`, id)
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
