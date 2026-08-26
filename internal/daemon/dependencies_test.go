package daemon

import (
	"container/heap"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// depTaskMD is taskMD with a deps list and a creation time, which is what the
// scheduling tests vary.
func (h *testHarness) depTaskMD(id, status, created string, deps ...string) string {
	list := "[]"
	if len(deps) > 0 {
		list = "[" + deps[0]
		for _, d := range deps[1:] {
			list += ", " + d
		}
		list += "]"
	}
	return fmt.Sprintf(`---
id: %s
kontora: true
status: %s
pipeline: one-stage
path: %s
deps: %s
created: %s
---
# Test ticket %s
`, id, status, h.repoDir, list, created, id)
}

// taskPath is the full path of a ticket file in the harness's tickets dir.
func (h *testHarness) taskPath(filename string) string {
	return filepath.Join(h.tasksDir, filename)
}

// queuedIDs returns the ticket IDs currently on the scheduler heap.
func (d *Daemon) queuedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, d.queue.Len())
	for _, item := range d.queue {
		ids = append(ids, item.ticketID)
	}
	return ids
}

// TestDependencyGating covers what the startup scan queues. A blocked ticket
// must not reach the heap however its dependency is unresolved.
func TestDependencyGating(t *testing.T) {
	cases := []struct {
		name string
		// files maps a ticket filename to its markdown.
		files      func(h *testHarness) map[string]string
		wantQueued []string
	}{
		{
			name: "an open dependency keeps the dependent out of the queue",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"),
					"dep-b.md": h.depTaskMD("dep-b", "open", "2026-01-01T00:00:00Z"),
				}
			},
			wantQueued: nil,
		},
		{
			name: "a missing dependency blocks",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "ghost"),
				}
			},
			wantQueued: nil,
		},
		{
			name: "a done dependency releases the dependent",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"),
					"dep-b.md": h.depTaskMD("dep-b", "done", "2026-01-01T00:00:00Z"),
				}
			},
			wantQueued: []string{"dep-a"},
		},
		{
			name: "a human_review dependency releases the dependent",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"),
					"dep-b.md": h.depTaskMD("dep-b", "human_review", "2026-01-01T00:00:00Z"),
				}
			},
			wantQueued: []string{"dep-a"},
		},
		{
			name: "a legacy closed dependency releases the dependent",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"),
					"dep-b.md": h.depTaskMD("dep-b", "closed", "2026-01-01T00:00:00Z"),
				}
			},
			wantQueued: []string{"dep-a"},
		},
		{
			name: "a cycle never queues either ticket",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-01T00:00:00Z", "dep-b"),
					"dep-b.md": h.depTaskMD("dep-b", "todo", "2026-01-02T00:00:00Z", "dep-a"),
				}
			},
			wantQueued: nil,
		},
		{
			name: "a newer ready ticket is queued while an older blocked one waits",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-old.md": h.depTaskMD("dep-old", "todo", "2026-01-01T00:00:00Z", "dep-block"),
					"dep-new.md": h.depTaskMD("dep-new", "todo", "2026-01-03T00:00:00Z"),
					// The blocker is open, so it is not a scheduling candidate
					// either.
					"dep-block.md": h.depTaskMD("dep-block", "open", "2026-01-02T00:00:00Z"),
				}
			},
			wantQueued: []string{"dep-new"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			for name, content := range tc.files(h) {
				h.writeTicket(name, content)
			}
			d := h.newDaemon(h.cfg)

			require.NoError(t, d.initialScan(h.tasksDir))
			assert.ElementsMatch(t, tc.wantQueued, d.queuedIDs())
		})
	}
}

// A ticket whose dependency is resolved after the scan must be queued without
// anyone editing the dependent's own file.
func TestDependencyReconciliation(t *testing.T) {
	cases := []struct {
		name string
		// change applies the event under test after the initial scan.
		change     func(t *testing.T, h *testHarness, d *Daemon)
		files      func(h *testHarness) map[string]string
		wantQueued []string
	}{
		{
			name: "completing a dependency wakes every ready dependent",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-c"),
					"dep-b.md": h.depTaskMD("dep-b", "todo", "2026-01-03T00:00:00Z", "dep-c"),
					"dep-c.md": h.depTaskMD("dep-c", "open", "2026-01-01T00:00:00Z"),
				}
			},
			change: func(_ *testing.T, h *testHarness, d *Daemon) {
				h.writeTicket("dep-c.md", h.depTaskMD("dep-c", "done", "2026-01-01T00:00:00Z"))
				d.handleFileChanged(h.taskPath("dep-c.md"))
			},
			wantQueued: []string{"dep-a", "dep-b"},
		},
		{
			name: "parking a dependency in human_review wakes its dependents",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-c"),
					"dep-c.md": h.depTaskMD("dep-c", "open", "2026-01-01T00:00:00Z"),
				}
			},
			change: func(_ *testing.T, h *testHarness, d *Daemon) {
				h.writeTicket("dep-c.md", h.depTaskMD("dep-c", "human_review", "2026-01-01T00:00:00Z"))
				d.handleFileChanged(h.taskPath("dep-c.md"))
			},
			wantQueued: []string{"dep-a"},
		},
		{
			name: "adding a dependency drops a queued ticket",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z"),
					"dep-b.md": h.depTaskMD("dep-b", "open", "2026-01-01T00:00:00Z"),
				}
			},
			change: func(t *testing.T, _ *testHarness, d *Daemon) {
				_, err := d.svc.AddDependency("dep-a", "dep-b")
				require.NoError(t, err)
			},
			wantQueued: nil,
		},
		{
			name: "removing a dependency wakes the ticket",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"),
					"dep-b.md": h.depTaskMD("dep-b", "open", "2026-01-01T00:00:00Z"),
				}
			},
			change: func(t *testing.T, _ *testHarness, d *Daemon) {
				_, err := d.svc.RemoveDependency("dep-a", "dep-b")
				require.NoError(t, err)
			},
			wantQueued: []string{"dep-a"},
		},
		{
			name: "creating the missing dependency as done wakes the ticket",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"),
				}
			},
			change: func(_ *testing.T, h *testHarness, d *Daemon) {
				h.writeTicket("dep-b.md", h.depTaskMD("dep-b", "done", "2026-01-01T00:00:00Z"))
				d.handleFileChanged(h.taskPath("dep-b.md"))
			},
			wantQueued: []string{"dep-a"},
		},
		{
			name: "deleting a resolved dependency blocks the dependent again",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"),
					"dep-b.md": h.depTaskMD("dep-b", "done", "2026-01-01T00:00:00Z"),
				}
			},
			change: func(_ *testing.T, h *testHarness, d *Daemon) {
				d.handleFileRemoved(h.taskPath("dep-b.md"))
			},
			wantQueued: nil,
		},
		{
			name: "a remote relation write reaches the same queue state as a file write",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"dep-a.md": h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z"),
					"dep-b.md": h.depTaskMD("dep-b", "open", "2026-01-01T00:00:00Z"),
				}
			},
			change: func(t *testing.T, _ *testHarness, d *Daemon) {
				require.NoError(t, d.AddDependency("dep-a", "dep-b"))
			},
			wantQueued: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			for name, content := range tc.files(h) {
				h.writeTicket(name, content)
			}
			d := h.newDaemon(h.cfg)
			require.NoError(t, d.initialScan(h.tasksDir))

			tc.change(t, h, d)
			assert.ElementsMatch(t, tc.wantQueued, d.queuedIDs())
		})
	}
}

// runTicket re-checks readiness, because a dependency can be added between the
// enqueue and the claim.
func TestDependencyRecheckedAtPickup(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("dep-a.md", h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z"))
	h.writeTicket("dep-b.md", h.depTaskMD("dep-b", "open", "2026-01-01T00:00:00Z"))
	d := h.newDaemon(h.cfg)
	require.NoError(t, d.initialScan(h.tasksDir))
	require.Equal(t, []string{"dep-a"}, d.queuedIDs())

	// Write the edge behind the scheduler's back, the way a second daemon or a
	// hand edit would, so nothing removes the ticket from the heap.
	h.writeTicket("dep-a.md", h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"))
	d.mu.Lock()
	d.tickets["dep-a"] = newTicketState(h.readTask("dep-a.md"), h.taskPath("dep-a.md"))
	d.mu.Unlock()

	d.runTicket(context.Background(), "dep-a")

	assert.Equal(t, ticket.StatusTodo, h.readTask("dep-a.md").Status)
	assert.Equal(t, 0, d.RunningAgents())
}

// End to end: a dependent runs only after its dependency's own run completes.
func TestDependencyReleasedByCompletedRun(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("dep-a.md", h.depTaskMD("dep-a", "todo", "2026-01-01T00:00:00Z", "dep-b"))
	h.writeTicket("dep-b.md", h.depTaskMD("dep-b", "todo", "2026-01-02T00:00:00Z"))
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	h.waitForStatus("dep-b.md", ticket.StatusDone, 10*time.Second)
	// dep-a is the older ticket, so FIFO order would have run it first had the
	// dependency not held it back.
	h.waitForStatus("dep-a.md", ticket.StatusDone, 10*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

// Equal creation times run in ID order, so the heap is deterministic.
func TestQueueOrdersEqualCreationTimesByID(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"dep-c", "dep-a", "dep-b"} {
		h.writeTicket(id+".md", h.depTaskMD(id, "todo", "2026-01-01T00:00:00Z"))
	}
	d := h.newDaemon(h.cfg)
	require.NoError(t, d.initialScan(h.tasksDir))

	d.mu.Lock()
	defer d.mu.Unlock()
	var order []string
	for d.queue.Len() > 0 {
		order = append(order, heap.Pop(&d.queue).(*queueItem).ticketID)
	}
	assert.Equal(t, []string{"dep-a", "dep-b", "dep-c"}, order)
}

// A ticket created with status open is never claimed, however fast the daemon
// sees the file. cli.New writes the whole ticket in one write, so there is no
// intermediate todo state to pick up.
func TestOpenTicketIsNeverEnqueued(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("dep-open.md", h.depTaskMD("dep-open", "open", "2026-01-01T00:00:00Z"))

	// Long enough for the watcher debounce and a scheduler pass.
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, ticket.StatusOpen, h.readTask("dep-open.md").Status)
	assert.Empty(t, d.queuedIDs())
	assert.Equal(t, 0, d.RunningAgents())

	cancel()
	require.NoError(t, <-errCh)
}

// A standalone ticket has no pipeline, so it closes outside the pipeline exit
// handler. It must still release whatever waited on it.
func TestStandaloneDependencyReleasesDependent(t *testing.T) {
	h := newHarness(t)
	standalone := func(id, status, created, deps string) string {
		return fmt.Sprintf("---\nid: %s\nkontora: true\nstatus: %s\npath: %s\ndeps: %s\ncreated: %s\n---\n# Test ticket %s\n",
			id, status, h.repoDir, deps, created, id)
	}
	h.writeTicket("dep-a.md", standalone("dep-a", "todo", "2026-01-01T00:00:00Z", "[dep-b]"))
	h.writeTicket("dep-b.md", standalone("dep-b", "todo", "2026-01-02T00:00:00Z", "[]"))
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	h.waitForStatus("dep-b.md", ticket.StatusDone, 10*time.Second)
	h.waitForStatus("dep-a.md", ticket.StatusDone, 10*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

// A relation the graph will not accept is a conflict with the store, not a
// server fault, so the API layer must see it as ErrInvalidState.
func TestRefusedRelationIsAnInvalidState(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Daemon) error
		wantErr string
	}{
		{
			name:    "a dependency that would close a cycle",
			mutate:  func(d *Daemon) error { return d.AddDependency("dep-b", "dep-a") },
			wantErr: "dep-b -> dep-a -> dep-b",
		},
		{
			name:    "a ticket depending on itself",
			mutate:  func(d *Daemon) error { return d.AddDependency("dep-a", "dep-a") },
			wantErr: "cannot be related to itself",
		},
		{
			name:    "a ticket linked to itself",
			mutate:  func(d *Daemon) error { return d.LinkTickets("dep-a", []string{"dep-a"}) },
			wantErr: "cannot be related to itself",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeTicket("dep-a.md", h.depTaskMD("dep-a", "todo", "2026-01-02T00:00:00Z", "dep-b"))
			h.writeTicket("dep-b.md", h.depTaskMD("dep-b", "open", "2026-01-01T00:00:00Z"))
			d := h.newDaemon(h.cfg)
			require.NoError(t, d.initialScan(h.tasksDir))

			err := tc.mutate(d)
			require.Error(t, err)
			assert.ErrorIs(t, err, web.ErrInvalidState)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
