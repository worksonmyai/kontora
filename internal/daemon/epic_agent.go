package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func epicSupervisorRole(id string) string {
	return fmt.Sprintf(`

## Epic supervisor
You supervise epic %s using ordinary coding-agent execution.
Load the Kontora skill. Read the installed epic reference with kontora skills show tickets Epics.
Read the epic brief, complete child set, dependencies, history, and existing results through the CLI.
Choose child order and parallelism. Create necessary children as open, then explicitly start selected work.
Monitor children through CLI reads and shell waits. Do not exit merely because children are running.
On resume or retry, inspect children, branches, worktrees, dirty files, and interrupted merges or rebases before issuing commands.
Do not restart children blindly.

Use your branch as the integration branch. Children retain separate branches, worktrees, and commits.
Dependency readiness does not mean prerequisite code exists in a child's Git base.
Before starting dependent work, verify prerequisites exist in its base. Set base_branch through kontora update before first execution.
Changing base_branch does not rebase an existing branch or worktree. Inspect those before retrying.
Run independent children in parallel when appropriate. Serialize integration-branch writes.
Never rebase or modify a child worktree while its agent is running.
Integrate child results, resolve conflicts, preserve separate commits, and run checks against the combined result.
Record progress, verification evidence, and unresolved decisions in epic notes.

Existing commit approvals and off-machine write restrictions still apply.
Starting an epic does not authorize pushes, publication, destructive resets, or changes to shared history.
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

func (d *Daemon) recordEpicRun(p spawnAgentParams, run agentRun) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := ticket.ParseFile(p.filePath)
	if err != nil || !t.IsAgentEpic() || t.ClaimedBy != d.instanceName {
		return
	}
	appendEpicHistory(t, run, p.agentName, p.run)
	_ = t.SetField("summary", runSummary(t.Summary, run.FinalMessage))
	if err := d.writeTicketLocked(t, p.filePath, notify.OriginDaemon); err != nil {
		p.log.Error("record supervisor run", "err", err)
		return
	}
	d.setTicketState(t.ID, t, p.filePath)
	d.broadcastTicketUpdate(t.ID)
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
