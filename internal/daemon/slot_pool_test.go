package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func poolUsed(d *Daemon, pool *slotPool) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return pool.used
}

func TestReload_SlotPoolAdmission(t *testing.T) {
	for _, supervisor := range []bool{false, true} {
		for _, grow := range []bool{false, true} {
			t.Run(fmt.Sprintf("supervisor=%t/grow=%t", supervisor, grow), func(t *testing.T) {
				h := newReloadHarness(t)
				initial, next := 2, 1
				if grow {
					initial, next = 1, 2
				}
				content := func(limit int) string {
					return strings.Replace(h.yaml(configOpts{extra: fmt.Sprintf("max_concurrent_epic_supervisors: %d\n", limit)}), "max_concurrent_agents: 4", fmt.Sprintf("max_concurrent_agents: %d", limit), 1)
				}
				started := make(chan string, 4)
				release := map[string]chan struct{}{}
				for _, id := range []string{"pool-first", "pool-second", "pool-third"} {
					release[id] = make(chan struct{})
				}
				d := h.newDaemonWithConfig(t, content(initial), WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
					started <- p.TicketID
					select {
					case <-release[p.TicketID]:
					case <-ctx.Done():
					}
					return process.Result{StartedAt: time.Now(), ExitedAt: time.Now(), Termination: process.TerminationProcessGroupGone}, nil
				}))
				write := func(id string) string {
					md := h.taskMD(id, "todo", "one-stage")
					if supervisor {
						md = strings.Replace(h.epicMD(id, "todo"), "kind: epic\n", "kind: epic\nepic_execution: agent\n", 1)
					}
					return h.writeTicket(id+".md", md)
				}
				await := func(want string) {
					t.Helper()
					select {
					case id := <-started:
						require.Equal(t, want, id)
					case <-time.After(5 * time.Second):
						t.Fatalf("%s did not start", want)
					}
				}
				assertWaiting := func() {
					t.Helper()
					select {
					case id := <-started:
						t.Fatalf("unexpected admission: %s", id)
					case <-time.After(100 * time.Millisecond):
					}
				}
				write("pool-first")
				stop := startDaemon(t, d)
				defer stop()
				await("pool-first")
				d.handleFileChanged(write("pool-second"))
				pool := d.sem
				if supervisor {
					pool = d.epicSem
				}
				if grow {
					assertWaiting()
				} else {
					await("pool-second")
					d.handleFileChanged(write("pool-third"))
				}
				h.writeConfigAtomic(t, content(next))
				require.NoError(t, d.reloadConfig())
				if grow {
					await("pool-second")
					require.Equal(t, 2, poolUsed(d, pool))
					return
				}
				require.Equal(t, 2, poolUsed(d, pool), "shrinking must not stop admitted work")
				assertWaiting()
				close(release["pool-first"])
				require.Eventually(t, func() bool { return poolUsed(d, pool) == 1 }, 5*time.Second, time.Millisecond)
				assertWaiting()
				close(release["pool-second"])
				await("pool-third")
			})
		}
	}
}

func TestRecoveredPoolResizeRelease(t *testing.T) {
	for _, supervisor := range []bool{false, true} {
		for _, limit := range []int{0, 1, 4} {
			t.Run(fmt.Sprintf("supervisor=%t/limit=%d", supervisor, limit), func(t *testing.T) {
				h := newHarness(t)
				d := h.newDaemon(h.cfg)
				pool := d.sem
				d.mu.Lock()
				defer d.mu.Unlock()
				for _, id := range []string{"recovered-first", "recovered-second"} {
					task := &ticket.Ticket{ID: id}
					if supervisor {
						task.Kind, task.EpicExecution = ticket.KindEpic, "agent"
						pool = d.epicSem
					}
					d.reserveRecoveredExecutionLocked(task)
				}
				d.resizePoolsLocked(limit, limit)
				require.Equal(t, 2, pool.used)
				require.Equal(t, limit-2, d.availablePoolSlotsLocked(pool))
				require.Same(t, pool, d.recoveredReservations["recovered-first"].pool)
				d.releaseRecoveredExecutionLocked("recovered-first")
				d.releaseRecoveredExecutionLocked("recovered-first")
				require.Equal(t, 1, pool.used)
				d.releaseRecoveredExecutionLocked("recovered-second")
				require.Zero(t, pool.used)
				require.Equal(t, limit, d.availablePoolSlotsLocked(pool))
			})
		}
	}
}

func TestReload_DisablesQueuedAutomaticPickup(t *testing.T) {
	h := newReloadHarness(t)
	content := strings.Replace(h.yaml(configOpts{}), "max_concurrent_agents: 4", "max_concurrent_agents: 1", 1)
	started := make(chan string, 3)
	releaseFirst := make(chan struct{})
	d := h.newDaemonWithConfig(t, content, WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
		started <- p.TicketID
		if p.TicketID == "disable-first" {
			select {
			case <-releaseFirst:
			case <-ctx.Done():
			}
		}
		return process.Result{StartedAt: time.Now(), ExitedAt: time.Now(), Termination: process.TerminationProcessGroupGone}, nil
	}))
	h.writeTicket("disable-first.md", h.taskMD("disable-first", "todo", "one-stage"))
	stop := startDaemon(t, d)
	defer stop()
	select {
	case id := <-started:
		require.Equal(t, "disable-first", id)
	case <-time.After(5 * time.Second):
		t.Fatal("first ticket did not start")
	}
	d.handleFileChanged(h.writeTicket("disable-second.md", h.taskMD("disable-second", "todo", "two-stage")))
	d.mu.Lock()
	queued := d.queued["disable-second"]
	d.mu.Unlock()
	require.True(t, queued)
	h.writeConfigAtomic(t, content+"auto_pick_up: false\n")
	require.NoError(t, d.reloadConfig())
	close(releaseFirst)
	require.Eventually(t, func() bool { return poolUsed(d, d.sem) == 0 }, 5*time.Second, time.Millisecond)
	select {
	case id := <-started:
		t.Fatalf("automatic ticket started after disabling pickup: %s", id)
	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, ticket.StatusTodo, h.readTask("disable-second.md").Status)
	require.NoError(t, d.RunTicket("disable-second"))
	for range 2 { // Both the explicit run and its pipeline continuation start.
		select {
		case id := <-started:
			require.Equal(t, "disable-second", id)
		case <-time.After(5 * time.Second):
			t.Fatal("explicit run or pipeline continuation did not start")
		}
	}
	h.waitForStatus("disable-second.md", ticket.StatusDone, 5*time.Second)
}

func TestDequeueAutoPickup(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			task := &ticket.Ticket{ID: "pickup-toggle", Kontora: true, Status: ticket.StatusTodo}
			d.mu.Lock()
			defer d.mu.Unlock()
			d.tickets[task.ID] = &ticketState{ticket: task}
			d.enqueueAutomatic(task)
			if explicit {
				d.enqueue(task)
			}
			next := *d.config()
			next.AutoPickUp = new(false)
			d.cfg.Store(&next)
			index := d.readyOrdinaryJobLocked()
			if explicit {
				require.GreaterOrEqual(t, index, 0)
				require.True(t, d.queued[task.ID])
			} else {
				require.Equal(t, -1, index)
				require.False(t, d.queued[task.ID])
				require.Empty(t, d.queue)
				require.Zero(t, d.queueDepth.Load())
				d.enqueue(task)
				require.GreaterOrEqual(t, d.readyOrdinaryJobLocked(), 0, "explicit run works after automatic pickup was dropped")
			}
			require.Equal(t, ticket.StatusTodo, task.Status)
		})
	}
}
