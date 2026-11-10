package daemon

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/worksonmyai/kontora/internal/ticket"
)

// slotPool accounts for all admitted and recovered executions, even when a
// resize or crash recovery leaves used above limit. All access requires d.mu.
// Pools are resized in place so outstanding reservations keep their identity.
type slotPool struct {
	limit int
	used  int
}

// Requires d.mu, together with publishing the new config.
func (d *Daemon) resizePoolsLocked(agents, supervisors int) {
	d.sem.limit = agents
	d.epicSem.limit = supervisors
	d.queueCond.Broadcast()
}

// managedDispatch stays disabled until the authority and integration paths are installed.
type managedDispatch struct {
	enabled       bool
	queue         []*dispatchJob
	lastObjective string
	preferManaged bool
	active        map[string]*dispatchJob
}

type dispatchJob struct {
	objectiveID string
	attemptID   string
	planner     bool
	slots       int // match the durable reservation; zero uses one slot
	ready       func() bool
	reserve     func() error
	run         func(context.Context) bool
}

// Uncertain ordinary execution retains ownership and capacity through a shared context flag.
type ordinaryReservationKey struct{}

func retainOrdinaryReservation(ctx context.Context) {
	if retained, ok := ctx.Value(ordinaryReservationKey{}).(*atomic.Bool); ok {
		retained.Store(true)
	}
}

func ordinaryReservationRetained(ctx context.Context) bool {
	retained, ok := ctx.Value(ordinaryReservationKey{}).(*atomic.Bool)
	return ok && retained.Load()
}

func (job *dispatchJob) slotCount() int {
	if job.slots == 0 {
		return 1
	}
	return job.slots
}

func (d *Daemon) queueManagedJob(job *dispatchJob) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.managedDispatch == nil || !d.managedDispatch.enabled {
		return errors.New("managed execution is disabled pending authority and integration support")
	}
	if job == nil || job.objectiveID == "" || job.attemptID == "" || job.ready == nil || job.reserve == nil || job.run == nil {
		return errors.New("incomplete managed dispatch request")
	}
	if job.slotCount() < 1 || job.slotCount() > d.sem.limit {
		return errors.New("managed slot request exceeds dispatcher capacity")
	}
	for _, queued := range d.managedDispatch.queue {
		if queued.attemptID == job.attemptID {
			return errors.New("attempt is already queued")
		}
	}
	if _, exists := d.managedDispatch.active[job.attemptID]; exists {
		return errors.New("attempt is already admitted")
	}
	d.managedDispatch.queue = append(d.managedDispatch.queue, job)
	d.queueCond.Broadcast()
	return nil
}

func (d *Daemon) readyManagedJobLocked() int {
	state := d.managedDispatch
	if state == nil || !state.enabled {
		return -1
	}
	plannerActive := false
	for _, job := range state.active {
		plannerActive = plannerActive || job.planner
	}
	first := -1
	for i, job := range state.queue {
		if job.planner && plannerActive || job.slotCount() > d.availablePoolSlotsLocked(d.sem) || !job.ready() {
			continue
		}
		if first < 0 {
			first = i
		}
		if job.objectiveID != state.lastObjective {
			return i
		}
	}
	return first
}

type dispatchPoolKey struct{}
type dispatchExplicitKey struct{}

func pickupExplicit(ctx context.Context) bool {
	explicit, dispatched := ctx.Value(dispatchExplicitKey{}).(bool)
	return !dispatched || explicit
}

// dispatchPoolMatches checks the reservation again after dispatch releases the
// mutex. A ticket can change execution mode before runTicket claims it.
func dispatchPoolMatches(ctx context.Context, t *ticket.Ticket) bool {
	epic, reserved := ctx.Value(dispatchPoolKey{}).(bool)
	return !reserved || epic == t.IsAgentEpic()
}

func (d *Daemon) ticketPool(t *ticket.Ticket) *slotPool {
	if t.IsAgentEpic() {
		return d.epicSem
	}
	return d.sem
}

// Heap storage is not sorted, so compare every eligible item with Less.
func (d *Daemon) readyOrdinaryJobLocked() int {
	eligible := d.queue[:0]
	for _, item := range d.queue {
		if _, ok := d.claimablePickupLocked(item.ticketID, d.ticketLog(item.ticketID), item.explicit); !ok {
			delete(d.queued, item.ticketID)
			continue
		}
		eligible = append(eligible, item)
	}
	if len(eligible) != d.queue.Len() {
		clear(d.queue[len(eligible):])
		d.queue = eligible
		heap.Init(&d.queue)
		d.syncQueueDepthLocked()
	}
	first := -1
	for i, item := range d.queue {
		pool := d.ticketPool(d.tickets[item.ticketID].ticket)
		if d.availablePoolSlotsLocked(pool) > 0 && (first < 0 || d.queue.Less(i, first)) {
			first = i
		}
	}
	return first
}

func (d *Daemon) dispatch(ctx context.Context, wg *sync.WaitGroup) {
	go func() {
		<-ctx.Done()
		d.mu.Lock()
		d.queueCond.Broadcast()
		d.mu.Unlock()
	}()
	for {
		d.mu.Lock()
		for {
			if ctx.Err() != nil {
				d.mu.Unlock()
				return
			}
			managedIndex := d.readyManagedJobLocked()
			ordinaryIndex := d.readyOrdinaryJobLocked()
			hasOrdinary := ordinaryIndex >= 0
			if len(d.automationJobs) > 0 && d.availablePoolSlotsLocked(d.sem) > 0 && (d.preferAutomation || (!hasOrdinary && managedIndex < 0)) {
				job := d.automationJobs[0]
				d.automationJobs = d.automationJobs[1:]
				d.sem.used++
				d.preferAutomation = false
				d.mu.Unlock()
				wg.Go(func() {
					stopped := job.run(ctx)
					d.mu.Lock()
					defer d.mu.Unlock()
					if stopped {
						d.sem.used--
					} else {
						d.log.Error("automation termination unconfirmed; capacity retained", "invocation", job.key)
					}
					d.queueCond.Broadcast()
				})
				break
			}
			if !hasOrdinary && managedIndex < 0 {
				d.queueCond.Wait()
				continue
			}
			d.preferAutomation = true
			managed := managedIndex >= 0 && (!hasOrdinary || d.managedDispatch.preferManaged)
			slots := 1
			pool := d.sem
			if managed {
				slots = d.managedDispatch.queue[managedIndex].slotCount()
			} else {
				pool = d.ticketPool(d.tickets[d.queue[ordinaryIndex].ticketID].ticket)
			}
			if slots > d.availablePoolSlotsLocked(pool) {
				d.queueCond.Wait()
				continue
			}
			pool.used += slots
			if managed {
				state := d.managedDispatch
				job := state.queue[managedIndex]
				state.queue = append(state.queue[:managedIndex], state.queue[managedIndex+1:]...)
				if err := job.reserve(); err != nil {
					pool.used -= slots
					d.log.Error("managed admission refused", "objective", job.objectiveID, "attempt", job.attemptID, "err", err)
					continue
				}
				if state.active == nil {
					state.active = map[string]*dispatchJob{}
				}
				state.active[job.attemptID] = job
				state.lastObjective, state.preferManaged = job.objectiveID, false
				d.mu.Unlock()
				wg.Go(func() {
					stopped := job.run(ctx)
					d.mu.Lock()
					defer d.mu.Unlock()
					if stopped {
						delete(state.active, job.attemptID)
						pool.used -= slots
					} else {
						d.log.Error("managed attempt termination unconfirmed; reservation retained", "objective", job.objectiveID, "attempt", job.attemptID)
					}
					d.queueCond.Broadcast()
				})
			} else {
				item := heap.Remove(&d.queue, ordinaryIndex).(*queueItem)
				epic := d.tickets[item.ticketID].ticket.IsAgentEpic()
				delete(d.queued, item.ticketID)
				d.syncQueueDepthLocked()
				if d.managedDispatch != nil {
					d.managedDispatch.preferManaged = true
				}
				d.mu.Unlock()
				d.metrics.QueueWait(ctx, time.Since(item.enqueuedAt))
				wg.Go(func() {
					runCtx := context.WithValue(ctx, ordinaryReservationKey{}, &atomic.Bool{})
					runCtx = context.WithValue(runCtx, dispatchPoolKey{}, epic)
					runCtx = context.WithValue(runCtx, dispatchExplicitKey{}, item.explicit)
					defer func() {
						d.mu.Lock()
						defer d.mu.Unlock()
						if !ordinaryReservationRetained(runCtx) {
							pool.used--
						} else {
							d.log.Error("ordinary attempt termination unconfirmed; reservation retained", "ticket", item.ticketID)
						}
						d.queueCond.Broadcast()
					}()
					d.runTicket(runCtx, item.ticketID)
				})
			}
			break
		}
	}
}
