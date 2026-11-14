package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestNotifyDurableWaitUsesPausedWithoutChangingStoredStatus(t *testing.T) {
	h := newHarness(t)
	recorder := &notifyRecorder{}
	d := h.newDaemon(notifyConfig(h), WithNotifier(recorder))
	tk := &ticket.Ticket{ID: "durable-wait", Kontora: true, Status: ticket.StatusInProgress,
		Notify:     ticket.NotifyList{Statuses: []string{"paused", "done"}},
		GraphState: &ticket.GraphState{ExecutionID: "exec", Revision: 7},
	}
	d.observe(tk, notify.OriginObserved)
	tk.Status = ticket.StatusPaused
	tk.GraphState.Wait = &ticket.WorkflowWait{ID: "wait", Kind: "human", Phase: "pending", Next: "Continue after sign-off"}
	d.observe(tk, notify.OriginDaemon)
	d.observe(tk, notify.OriginDaemon)
	observation := recorder.lastFor(t, tk.ID)
	require.Equal(t, "paused", observation.Status)
	require.Equal(t, ticket.StatusPaused, tk.Status)
	require.Contains(t, observation.Fields.Question, "Continue after sign-off")
	require.Contains(t, observation.Fields.Question, "--execution-id exec --wait-id wait --revision 7")
	events := recorder.sends(t)
	require.Len(t, events, 1)
	require.Equal(t, "paused", events[0].To)
	tk.GraphState.Wait = nil
	tk.Status = ticket.StatusDone
	d.observe(tk, notify.OriginDaemon)
	require.Len(t, recorder.sends(t), 2)
}

func TestNotifyOrdinaryPauseIsNotDurableWaiting(t *testing.T) {
	h := newHarness(t)
	recorder := &notifyRecorder{}
	d := h.newDaemon(notifyConfig(h), WithNotifier(recorder))
	d.observe(&ticket.Ticket{ID: "paused", Status: ticket.StatusPaused}, notify.OriginDaemon)
	require.Equal(t, "paused", recorder.lastFor(t, "paused").Status)
}
