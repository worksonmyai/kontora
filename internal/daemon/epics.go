package daemon

import (
	"time"

	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// ticketIndexLocked snapshots the tracked tickets as a dependency index. Must
// be called with d.mu held.
func (d *Daemon) ticketIndexLocked() map[string]*ticket.Ticket {
	index := make(map[string]*ticket.Ticket, len(d.tickets))
	for id, ts := range d.tickets {
		index[id] = ts.ticket
	}
	return index
}

// rederiveParentEpicLocked recomputes the epic a ticket belongs to, if it
// belongs to one. It walks exactly one level: epics do not nest, so a parent
// that is itself parented is a hand-edited file and is left alone. Must be
// called with d.mu held.
func (d *Daemon) rederiveParentEpicLocked(t *ticket.Ticket) {
	if t == nil || t.Parent == "" || t.Kind == ticket.KindEpic {
		return
	}
	parent, ok := d.tickets[t.Parent]
	if !ok || parent.ticket.Kind != ticket.KindEpic {
		return
	}
	d.rederiveEpicLocked(t.Parent, t.ID)
}

// rederiveEpicMoveLocked derives both ends of a ticket that may have changed
// parent: the epic it belongs to now, and the one it left. A save carries only
// the new parent, so the old one is read from the copy the cache still holds.
// Must be called with d.mu held, before the cache is replaced.
func (d *Daemon) rederiveEpicMoveLocked(prev, t *ticket.Ticket) {
	if t == nil || t.Kind == ticket.KindEpic {
		return
	}
	d.rederiveParentEpicLocked(t)
	if prev == nil || prev.Parent == "" || prev.Parent == t.Parent {
		return
	}
	d.rederiveEpicLocked(prev.Parent, "")
}

// rederiveEpicLocked recomputes an epic's status from its children and writes
// it back. lastChild names the child whose change triggered the pass, for the
// note an epic that closes itself carries; it may be empty. Must be called with
// d.mu held.
func (d *Daemon) rederiveEpicLocked(epicID, lastChild string) {
	ts, ok := d.tickets[epicID]
	if !ok {
		return
	}
	log := d.ticketLog(epicID)

	// The file is what a pickup and every other reader act on, and the watcher
	// is debounced, so the derivation is run against a fresh read rather than
	// the cached copy.
	t2, err := ticket.ParseFile(ts.filePath)
	if err != nil {
		log.Error("epic derivation: re-reading the epic failed", "err", err)
		return
	}
	if t2.Kind != ticket.KindEpic {
		d.setTicketState(epicID, t2, ts.filePath)
		return
	}
	// Archived is terminal for an epic as it is for anything else. The archive
	// stamps say which status it was filed away from, and a derivation on top
	// of them would put it back on the board carrying stamps that contradict
	// the status it now has.
	if t2.Status == ticket.StatusArchived {
		d.setTicketState(epicID, t2, ts.filePath)
		return
	}

	index := d.ticketIndexLocked()
	index[epicID] = t2
	derived := ticket.DeriveEpicStatus(index, epicID)
	if derived == t2.Status {
		// Adopt the fresh read anyway, so the next pass does not ask the same
		// question of a stale copy.
		d.setTicketState(epicID, t2, ts.filePath)
		return
	}

	if err := t2.SetField("status", string(derived)); err != nil {
		log.Error("epic derivation: setting status failed", "err", err)
		return
	}
	if derived == ticket.StatusDone && lastChild != "" {
		if _, err := t2.AddNote(ticket.AddNoteOptions{
			Text:   "closed itself when " + lastChild + " landed",
			Author: ticket.SystemAuthor,
			At:     time.Now(),
		}); err != nil {
			log.Error("epic derivation: writing the lifecycle note failed", "err", err)
		}
	}
	// OriginDerived seeds the remembered status without sending: the change a
	// person asked to hear about is the child's, and that one sent on its own.
	if err := d.writeTicketLocked(t2, ts.filePath, notify.OriginDerived); err != nil {
		log.Error("epic derivation: write failed", "err", err)
		return
	}
	log.Info("epic status derived", "from", string(ts.ticket.Status), "to", string(derived))
	d.setTicketState(epicID, t2, ts.filePath)
	d.broadcastTicketUpdate(epicID)
}

// rederiveAllEpicsLocked runs the derivation over every tracked epic. The
// startup scan needs it because an epic is read before the children it derives
// from. Must be called with d.mu held.
func (d *Daemon) rederiveAllEpicsLocked() {
	var ids []string
	for id, ts := range d.tickets {
		if ts.ticket.Kind == ticket.KindEpic {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		d.rederiveEpicLocked(id, "")
	}
}
