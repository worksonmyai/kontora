package daemon

import (
	"context"
	"reflect"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/worktree"
)

type deploymentMatchKey struct {
	RepoPath, Repository, Required, Deployed string
}

// Cache only validated non-matches, for one reconciliation pass. Reusing a
// non-match cannot release a wait if objects disappear or shallow boundaries
// change. Positive matches and errors always revalidate local evidence.
// The cap bounds memory when many unrelated receipts are retained.
type deploymentMatchCache map[deploymentMatchKey]struct{}

func (c deploymentMatchCache) matches(ctx context.Context, t *ticket.Ticket, receipt *ticket.WorkflowReceipt) (bool, error) {
	key := deploymentMatchKey{expandTilde(t.Path), receipt.Repository, t.GraphState.Wait.RequiredCommit, receipt.Commit}
	if _, ok := c[key]; ok {
		return false, ctx.Err()
	}
	matches, err := worktree.DeploymentMatchesContext(ctx, key.RepoPath, key.Repository, key.Required, key.Deployed)
	if err == nil && !matches {
		if len(c) >= 256 {
			clear(c)
		}
		c[key] = struct{}{}
	}
	return matches, err
}

// History reads run without workflowEventsMu, the board lock, or d.mu. Timer
// and probe progress must not depend on how much Git history receipts retain.
func (d *Daemon) reconcileWorkflowDeployment(ctx context.Context, path string, cache deploymentMatchCache) {
	workflowEventsMu.Lock()
	event, err := readWorkflowEvent(path)
	if err != nil || event.Kind != "deployment" {
		workflowEventsMu.Unlock()
		return
	}
	var candidates []*app.StoredTicket
	err = d.withTicketMutation(func(repo app.MutationRepository) error {
		stored, err := repo.List()
		if err != nil {
			return err
		}
		for _, st := range stored {
			w := workflowWait(st.Ticket)
			if w == nil || w.Kind != "deployment" || w.Phase != "pending" || w.Disarmed ||
				w.Repository != event.Receipt.Repository || w.Environment != event.Receipt.Environment || workflowEventHasApplication(event, st.Ticket) {
				continue
			}
			if _, err := d.workflowActionTicket(repo, st.Ticket.ID, false); err != nil {
				continue
			}
			candidates = append(candidates, &app.StoredTicket{Ticket: st.Ticket.Clone(), FilePath: st.FilePath})
		}
		return nil
	})
	workflowEventsMu.Unlock()
	if err != nil {
		return
	}
	slices.SortFunc(candidates, func(a, b *app.StoredTicket) int { return strings.Compare(a.Ticket.ID, b.Ticket.ID) })
	for _, st := range candidates {
		if ctx.Err() != nil {
			return
		}
		if err := validateWaitWorkspace(st.Ticket, st.Ticket.GraphState.Wait); err != nil {
			continue
		}
		matches, err := cache.matches(ctx, st.Ticket, event.Receipt)
		if err != nil || !matches {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		d.acceptDeploymentApplication(path, event.Receipt, st)
	}
}

func (d *Daemon) acceptDeploymentApplication(path string, receipt *ticket.WorkflowReceipt, snapshot *app.StoredTicket) {
	workflowEventsMu.Lock()
	defer workflowEventsMu.Unlock()
	// Another request or replay can append applications while Git is running.
	// Never write the event snapshot back over those durable applications.
	event, err := readWorkflowEvent(path)
	if err != nil || event.Kind != "deployment" || !reflect.DeepEqual(event.Receipt, receipt) {
		return
	}
	var application *workflowEventApplication
	err = d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := d.workflowActionTicket(repo, snapshot.Ticket.ID, false)
		if err != nil {
			return err
		}
		t, before := st.Ticket, snapshot.Ticket
		if st.FilePath != snapshot.FilePath || t.Path != before.Path || t.Branch != before.Branch ||
			t.Pipeline != before.Pipeline || t.Stage != before.Stage || !reflect.DeepEqual(t.GraphState, before.GraphState) ||
			workflowEventHasApplication(event, t) {
			return errExecutionChanged
		}
		application, err = d.releaseApplication(t, st.FilePath, receipt)
		if err != nil {
			return err
		}
		event.Applications = append(event.Applications, application)
		return saveWorkflowEvent(path, event)
	})
	if err != nil {
		return
	}
	if err := d.applyWorkflowEvent(event, application); err != nil {
		return
	}
	application.Applied = true
	_ = saveWorkflowEvent(path, event)
}
