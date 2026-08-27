package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// scheduleRecheck caps how long the schedule timer sleeps while a deadline is
// pending. Go timers run on the monotonic clock, which does not advance while
// the machine is suspended, so a single long sleep across a laptop lid would
// fire well after the ticket was due. Rechecking against the wall clock bounds
// that lateness.
const scheduleRecheck = time.Minute

// scheduleIdle is the sleep when no ticket is waiting on a deadline. Every path
// that can create one signals the wake channel, so this is only a backstop
// against a signal that was never sent.
const scheduleIdle = time.Hour

// scheduleFloor is the shortest sleep the loop takes, so a deadline that is
// already past when the wait is computed cannot spin it.
const scheduleFloor = time.Second

// scheduleRetryMin and scheduleRetryMax bound the wait after a promotion fails.
// A file that cannot be written (a read-only mount, a bad mode) fails the same
// way every pass, and without a backoff the deadline stays due and the loop
// retries every scheduleFloor for as long as the ticket exists.
const (
	scheduleRetryMin = 5 * time.Second
	scheduleRetryMax = 5 * time.Minute
)

// scheduleBackoff is when the loop may try a failed promotion again, and the
// wait that produced it, which the next failure doubles.
type scheduleBackoff struct {
	at   time.Time
	wait time.Duration
}

// ScheduleTicket sets or clears a ticket's pickup time.
//
// The whole read-modify-write holds d.mu, for the reason SetStage does: a
// scheduler pickup takes the same lock to claim a ticket, and a write built on
// a ticket parsed before the claim would put back the status, started_at and
// claimed_by the claim wrote.
func (d *Daemon) ScheduleTicket(id string, req web.ScheduleTicketRequest) error {
	var at time.Time
	if !req.Clear {
		parsed, err := ticket.ParseSchedule(req.ScheduledAt)
		if err != nil {
			return fmt.Errorf("%w: %s", web.ErrInvalidSchedule, err)
		}
		if ticket.SchedulePast(parsed, time.Now()) {
			return fmt.Errorf("%w: %s is in the past (%s)", web.ErrInvalidSchedule, ticket.FieldScheduledAt, ticket.FormatSchedule(parsed))
		}
		at = parsed
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	ts, ok := d.tickets[id]
	if !ok {
		return web.ErrTicketNotFound
	}
	// Only setting a schedule is refused by status. Clearing one has to work
	// wherever a stale timestamp can end up, or the only way out of it is the
	// hand edit the docs tell people not to make.
	refuse := d.scheduleSetRefusalLocked
	if req.Clear {
		refuse = d.scheduleWriteRefusalLocked
	}
	if err := refuse(id, ts.ticket); err != nil {
		return err
	}
	filePath := ts.filePath

	// Read from disk rather than from the cache: the file is the state a pickup
	// would act on, and the watcher is debounced, so the cache can lag an edit.
	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return err
	}
	if err := refuse(id, t2); err != nil {
		return err
	}

	if req.Clear {
		if err := t2.ClearSchedule(); err != nil {
			return fmt.Errorf("clearing %s: %w", ticket.FieldScheduledAt, err)
		}
	} else {
		if err := t2.SetSchedule(at); err != nil {
			return fmt.Errorf("setting %s: %w", ticket.FieldScheduledAt, err)
		}
		// The schedule is what moves the ticket to todo, so it waits in open
		// until then. A todo ticket already in the queue has to leave it.
		if t2.Status != ticket.StatusOpen {
			if err := t2.SetField("status", string(ticket.StatusOpen)); err != nil {
				return fmt.Errorf("setting status: %w", err)
			}
		}
	}

	if err := d.writeTicketLocked(t2, filePath, notify.OriginRequest); err != nil {
		return err
	}
	// After the write, not before: a failed write would otherwise leave a todo
	// ticket out of the queue with nothing left to put it back.
	if !req.Clear {
		d.removeQueuedLocked(id)
	}
	delete(d.scheduleRetry, id)
	d.setTicketState(id, t2, filePath)
	d.broadcastTicketUpdate(id)
	d.signalSchedule()
	return nil
}

// scheduleWriteRefusalLocked reports why the daemon cannot write a ticket's
// schedule field at all. A running agent owns the file, so a write under it is
// lost when the run writes the file back. Must be called with d.mu held.
func (d *Daemon) scheduleWriteRefusalLocked(id string, _ *ticket.Ticket) error {
	// d.running is registered when the scheduler claims the ticket, before the
	// file says in_progress, so it catches a pickup the cached status has not
	// seen yet.
	if _, running := d.running[id]; running {
		return fmt.Errorf("%w: cannot schedule ticket %s while it is running", web.ErrInvalidState, id)
	}
	return nil
}

// scheduleSetRefusalLocked reports why a ticket cannot be given a schedule, or
// nil when it can. It guards both ends: the action a person asks for, and the
// promotion the timer would make. Must be called with d.mu held.
func (d *Daemon) scheduleSetRefusalLocked(id string, t *ticket.Ticket) error {
	if !t.Kontora {
		return fmt.Errorf("%w: ticket %s is not initialized", web.ErrInvalidState, id)
	}
	if err := d.scheduleWriteRefusalLocked(id, t); err != nil {
		return err
	}
	switch t.Status { //nolint:exhaustive // only open and todo can be scheduled
	case ticket.StatusOpen, ticket.StatusTodo:
	default:
		return fmt.Errorf("%w: cannot schedule ticket %s in status %s (must be open or todo)", web.ErrInvalidState, id, t.Status)
	}
	// A Plannotator session owns the ticket text, and a pending annotation owns
	// the next pickup. Either way the schedule would compete for the same move.
	if _, annotating := d.plannotator[id]; annotating {
		return fmt.Errorf("%w: a plannotator session is open for ticket %s", web.ErrInvalidState, id)
	}
	if t.AnnotationReturnStatus != "" {
		return fmt.Errorf("%w: an annotation run is pending on ticket %s", web.ErrInvalidState, id)
	}
	return nil
}

// signalSchedule asks the schedule loop to recompute its next deadline. The
// channel is buffered and the send never blocks, so it is safe to call with or
// without d.mu held.
func (d *Daemon) signalSchedule() {
	select {
	case d.scheduleWake <- struct{}{}:
	default:
	}
}

// scheduleLoop promotes due tickets and sleeps until the nearest remaining
// deadline. One timer and an O(n) scan of the ticket map replace a delayed
// heap: ticket counts are small, and a heap would need stale-entry handling for
// every reschedule, clear and deletion.
func (d *Daemon) scheduleLoop(ctx context.Context) {
	timer := time.NewTimer(scheduleIdle)
	defer timer.Stop()

	for {
		d.promoteDueSchedules(time.Now())
		timer.Reset(d.nextScheduleWait(time.Now()))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-d.scheduleWake:
		}
	}
}

// nextScheduleWait is how long the loop may sleep before scanning again.
func (d *Daemon) nextScheduleWait(now time.Time) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()

	var (
		nearest time.Duration
		found   bool
	)
	for id, ts := range d.tickets {
		at, ok := d.pendingScheduleLocked(id, ts.ticket)
		if !ok {
			continue
		}
		// An overdue deadline is negative, so "no deadline yet" is a separate
		// flag rather than a sentinel duration.
		if until := at.Sub(now); !found || until < nearest {
			nearest, found = until, true
		}
	}
	switch {
	case !found:
		return scheduleIdle
	case nearest < scheduleFloor:
		return scheduleFloor
	case nearest < scheduleRecheck:
		return nearest
	default:
		return scheduleRecheck
	}
}

// pendingScheduleLocked returns the deadline of a ticket the loop is waiting
// on: one in open whose scheduled_at parses and whose promotion nothing
// currently refuses. A malformed value is no schedule, so a hand-edited typo
// leaves the ticket where it is rather than running it at an instant nobody
// asked for. A refused ticket is left out rather than rescanned every pass;
// every path that ends a refusal signals the loop.
//
// A ticket whose promotion has failed carries a retry time, and the later of
// the two is the deadline, so a file the daemon cannot write does not hold the
// loop at its floor.
//
// Must be called with d.mu held.
func (d *Daemon) pendingScheduleLocked(id string, t *ticket.Ticket) (time.Time, bool) {
	if t.Status != ticket.StatusOpen {
		return time.Time{}, false
	}
	if d.scheduleSetRefusalLocked(id, t) != nil {
		return time.Time{}, false
	}
	at, ok := t.Schedule()
	if !ok {
		return time.Time{}, false
	}
	if r, backing := d.scheduleRetry[id]; backing && r.at.After(at) {
		return r.at, true
	}
	return at, true
}

// promoteDueSchedules moves every ticket whose deadline has passed to todo.
func (d *Daemon) promoteDueSchedules(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var due []string
	for id, ts := range d.tickets {
		if at, ok := d.pendingScheduleLocked(id, ts.ticket); ok && !at.After(now) {
			due = append(due, id)
		}
	}
	// A backoff outlives its ticket otherwise: the entry is only ever deleted by
	// a promotion or a schedule write, and a deleted ticket makes neither.
	for id := range d.scheduleRetry {
		if ts, ok := d.tickets[id]; !ok || ts.ticket.ScheduledAt == "" {
			delete(d.scheduleRetry, id)
		}
	}
	// Map order is random; promoting in id order gives two tickets due in the
	// same pass a stable queue order.
	slices.Sort(due)
	for _, id := range due {
		d.promoteScheduledLocked(id, now)
	}
}

// promoteScheduledLocked writes status todo and removes the schedule in one
// save, then hands the ticket to the ordinary reconciliation. It does not
// enqueue: auto_pick_up and the dependency graph decide that, exactly as they
// do for a ticket a person moved to todo. Must be called with d.mu held.
func (d *Daemon) promoteScheduledLocked(id string, now time.Time) {
	ts, ok := d.tickets[id]
	if !ok {
		return
	}
	log := d.ticketLog(id)

	// The cached ticket said it was due; the file is what a pickup would act on,
	// and the watcher is debounced, so every condition is asked again of a fresh
	// read.
	t2, err := ticket.ParseFile(ts.filePath)
	if err != nil {
		log.Error("scheduled pickup: re-reading ticket failed", "err", err)
		d.backOffScheduleLocked(id, now)
		return
	}
	at, ok := d.pendingScheduleLocked(id, t2)
	if !ok || at.After(now) {
		// The cache said due and the file disagrees. Adopt the file, so the next
		// pass does not read the same stale answer.
		d.setTicketState(id, t2, ts.filePath)
		return
	}

	if err := t2.SetField("status", string(ticket.StatusTodo)); err != nil {
		log.Error("scheduled pickup: setting status failed", "err", err)
		d.backOffScheduleLocked(id, now)
		return
	}
	if err := t2.ClearSchedule(); err != nil {
		log.Error("scheduled pickup: clearing the schedule failed", "err", err)
		d.backOffScheduleLocked(id, now)
		return
	}
	if err := d.writeTicketLocked(t2, ts.filePath, notify.OriginDaemon); err != nil {
		// The schedule is still on disk, so a later pass tries again.
		log.Error("scheduled pickup: write failed", "err", err)
		d.backOffScheduleLocked(id, now)
		return
	}
	delete(d.scheduleRetry, id)

	log.Info("scheduled pickup", "due", ticket.FormatSchedule(at), "pipeline", t2.Pipeline, "stage", t2.Stage)
	// setTicketState reconciles the dependency graph, which is what queues the
	// ticket when auto_pick_up is on and nothing blocks it.
	d.setTicketState(id, t2, ts.filePath)
	d.broadcastTicketUpdate(id)
}

// backOffScheduleLocked pushes a ticket whose promotion failed out to a later
// deadline, doubling the wait up to scheduleRetryMax. Without it a ticket the
// daemon cannot write stays due, and the loop takes both locks, scans the map
// twice and re-reads the file every scheduleFloor for as long as it exists.
// Must be called with d.mu held.
func (d *Daemon) backOffScheduleLocked(id string, now time.Time) {
	wait := scheduleRetryMin
	if prev, ok := d.scheduleRetry[id]; ok {
		wait = min(prev.wait*2, scheduleRetryMax)
	}
	d.scheduleRetry[id] = scheduleBackoff{at: now.Add(wait), wait: wait}
}
