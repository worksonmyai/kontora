package daemon

import (
	"github.com/worksonmyai/kontora/internal/ticket"
)

// daemonRuntime implements app.RuntimeHooks for daemon-mode operation.
type daemonRuntime struct {
	d *Daemon
}

func (r *daemonRuntime) Enqueue(t *ticket.Ticket) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	r.d.enqueue(t)
}

func (r *daemonRuntime) Cancel(ticketID string) {
	r.d.mu.Lock()
	cancel, ok := r.d.running[ticketID]
	r.d.mu.Unlock()
	if ok {
		cancel()
	}
}

func (r *daemonRuntime) BroadcastUpdated(ticketID string) {
	r.d.broadcastTicketUpdateLocking(ticketID)
}

func (r *daemonRuntime) BroadcastDeleted(ticketID string) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	ts, ok := r.d.tickets[ticketID]
	if ok {
		r.d.broadcastTicketDeleted(ts)
	}
}
