package daemon

import (
	"fmt"
	"strings"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// dependencyPromptContext names human_review dependencies whose branch tip is
// missing from the worktree. human_review releases dependents before a person
// merges the dependency, so its commits are often absent from the new branch.
func (d *Daemon) dependencyPromptContext(t *ticket.Ticket, wtPath string) string {
	if wtPath == "" || len(t.Deps) == 0 {
		return ""
	}
	type dep struct{ id, branch string }
	var deps []dep
	d.mu.Lock()
	for _, id := range t.Deps {
		dt, _ := d.dependencyLocked(id)
		if dt == nil || dt.Status != ticket.StatusHumanReview || dt.Branch == "" {
			continue
		}
		deps = append(deps, dep{id: id, branch: dt.Branch})
	}
	d.mu.Unlock()

	var lines []string
	for _, dp := range deps {
		tip, err := worktree.UnmergedBranchTip(wtPath, dp.branch)
		if err != nil {
			d.log.Warn("checking dependency branch", "ticket", t.ID, "dep", dp.id, "branch", dp.branch, "err", err)
			continue
		}
		if tip == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: branch `%s` at %s", dp.id, dp.branch, shortCommit(tip)))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n## Unmerged dependencies\n" +
		"These dependencies are in human_review, and this worktree does not contain their commits. " +
		"Kontora orders dependencies but does not merge them.\n" +
		strings.Join(lines, "\n")
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
