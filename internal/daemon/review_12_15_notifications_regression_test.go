package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestReview12DurableWaitNotifiesPausedSubscribers(t *testing.T) {
	for _, kind := range []string{"human", "deployment", "timer", "check"} {
		for _, subscription := range []string{"paused", notify.StatusWaiting} {
			t.Run(kind+"/"+subscription, func(t *testing.T) {
				h := newHarness(t)
				recorder := &notifyRecorder{}
				d := h.newDaemon(notifyConfig(h), WithNotifier(recorder))
				tk := &ticket.Ticket{ID: "review-wait", Status: ticket.StatusInProgress,
					Notify:     ticket.NotifyList{Statuses: []string{subscription}},
					GraphState: &ticket.GraphState{ExecutionID: "exec", Revision: 7},
				}
				d.observe(tk, notify.OriginObserved)
				tk.Status = ticket.StatusPaused
				tk.GraphState.Wait = &ticket.WorkflowWait{ID: "wait", Kind: kind, Phase: "pending", Next: "Review evidence"}
				d.observe(tk, notify.OriginDaemon)
				d.observe(tk, notify.OriginDaemon)
				require.Equal(t, "paused", recorder.lastFor(t, tk.ID).Status)
				if subscription == "paused" {
					events := recorder.sends(t)
					require.Len(t, events, 1)
					require.Equal(t, "paused", events[0].To)
					require.Contains(t, events[0].Fields.Question, "Waiting: "+kind)
					require.Contains(t, events[0].Fields.Question, "Review evidence")
				} else {
					require.Empty(t, recorder.sends(t))
				}
				require.Empty(t, recorder.waitingFields())
			})
		}
	}
}

func TestReview15DeploymentNotificationUsesCheckout(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(notifyConfig(h))
	w := &ticket.WorkflowWait{ID: "deployment", Kind: "deployment", Phase: "pending",
		Repository: "/repos/project/.git", Worktree: "/checkouts/release candidate",
		Environment: "production", RequiredCommit: "0123456789012345678901234567890123456789"}
	tk := &ticket.Ticket{ID: "review-deployment", Status: ticket.StatusPaused, GraphState: &ticket.GraphState{Wait: w}}
	question := d.notifyTarget(tk).fields.Question
	require.Contains(t, question, fmt.Sprintf("--repo %q --environment %q", w.Worktree, w.Environment))
	require.Contains(t, question, "--rev <full-commit> --event-id <unique-id>")
	require.Contains(t, question, "required commit "+w.RequiredCommit)
	require.NotContains(t, question, w.Repository)
}
