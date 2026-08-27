package daemon

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// schedTaskMD is taskMD with a scheduled_at line and an optional dependency,
// which is what the schedule tests vary. An empty scheduledAt leaves the field
// out entirely.
func (h *testHarness) schedTaskMD(id, status, scheduledAt string, deps ...string) string {
	extra := ""
	if scheduledAt != "" {
		extra += fmt.Sprintf("scheduled_at: %q\n", scheduledAt)
	}
	if len(deps) > 0 {
		extra += "deps: ["
		for i, dep := range deps {
			if i > 0 {
				extra += ", "
			}
			extra += dep
		}
		extra += "]\n"
	}
	return fmt.Sprintf(`---
id: %s
kontora: true
status: %s
pipeline: one-stage
path: %s
%screated: 2026-01-01T00:00:00Z
---
# Test ticket %s
`, id, status, h.repoDir, extra, id)
}

func TestPromoteDueSchedules(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	past := ticket.FormatSchedule(now.Add(-time.Hour))
	future := ticket.FormatSchedule(now.Add(time.Hour))

	cases := []struct {
		name       string
		autoPickUp bool
		files      func(h *testHarness) map[string]string
		// setup runs after the scan and before the promotion pass.
		setup          func(t *testing.T, h *testHarness, d *Daemon)
		wantStatus     ticket.Status
		wantScheduleAt string
		wantQueued     []string
	}{
		{
			name:       "a future schedule leaves the ticket open",
			autoPickUp: true,
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", future)}
			},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: future,
		},
		{
			name:       "a schedule due exactly now promotes",
			autoPickUp: true,
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", ticket.FormatSchedule(now))}
			},
			wantStatus: ticket.StatusTodo,
			wantQueued: []string{"sch-a"},
		},
		{
			name:       "an overdue schedule promotes and queues",
			autoPickUp: true,
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", past)}
			},
			wantStatus: ticket.StatusTodo,
			wantQueued: []string{"sch-a"},
		},
		{
			name:       "auto_pick_up off promotes but does not queue",
			autoPickUp: false,
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", past)}
			},
			wantStatus: ticket.StatusTodo,
		},
		{
			name:       "an unresolved dependency promotes but holds the queue",
			autoPickUp: true,
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"sch-a.md": h.schedTaskMD("sch-a", "open", past, "sch-b"),
					"sch-b.md": h.schedTaskMD("sch-b", "todo", ""),
				}
			},
			wantStatus: ticket.StatusTodo,
			wantQueued: []string{"sch-b"},
		},
		{
			name:       "a malformed schedule is left alone",
			autoPickUp: true,
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", "next tuesday")}
			},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: "next tuesday",
		},
		{
			name:       "a pending annotation defers the promotion",
			autoPickUp: true,
			files: func(h *testHarness) map[string]string {
				md := h.schedTaskMD("sch-a", "open", past)
				return map[string]string{"sch-a.md": md + "\n"}
			},
			setup: func(t *testing.T, h *testHarness, d *Daemon) {
				tk := h.readTask("sch-a.md")
				require.NoError(t, tk.SetField("annotation_return_status", "open"))
				data, err := tk.Marshal()
				require.NoError(t, err)
				h.writeTicket("sch-a.md", string(data))
				d.mu.Lock()
				d.tickets["sch-a"] = newTicketState(tk, h.taskPath("sch-a.md"))
				d.mu.Unlock()
			},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: past,
		},
		{
			name:       "an open plannotator session defers the promotion",
			autoPickUp: true,
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", past)}
			},
			setup: func(_ *testing.T, _ *testHarness, d *Daemon) {
				d.mu.Lock()
				d.plannotator["sch-a"] = func() {}
				d.mu.Unlock()
			},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: past,
		},
		{
			name:       "a schedule on a ticket that is not managed is ignored",
			autoPickUp: true,
			files: func(_ *testHarness) map[string]string {
				return map[string]string{"sch-a.md": fmt.Sprintf("---\nid: sch-a\nstatus: open\nscheduled_at: %q\n---\n# Foreign\n", past)}
			},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: past,
		},
		{
			name:       "the file wins over a stale cache",
			autoPickUp: true,
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", past)}
			},
			setup: func(_ *testing.T, h *testHarness, _ *Daemon) {
				// The cache still says due; the file no longer does.
				h.writeTicket("sch-a.md", h.schedTaskMD("sch-a", "open", future))
			},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: future,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			for name, content := range tc.files(h) {
				h.writeTicket(name, content)
			}
			h.cfg.AutoPickUp = new(tc.autoPickUp)
			d := h.newDaemon(h.cfg)
			require.NoError(t, d.initialScan(h.tasksDir))
			if tc.setup != nil {
				tc.setup(t, h, d)
			}

			d.promoteDueSchedules(now)

			got := h.readTask("sch-a.md")
			assert.Equal(t, tc.wantStatus, got.Status)
			assert.Equal(t, tc.wantScheduleAt, got.ScheduledAt)
			assert.ElementsMatch(t, tc.wantQueued, d.queuedIDs())
		})
	}
}

func TestNextScheduleWait(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		files func(h *testHarness) map[string]string
		want  time.Duration
	}{
		{
			name: "no schedule sleeps until something signals",
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", "")}
			},
			want: scheduleIdle,
		},
		{
			name: "a distant deadline is capped by the wall-clock recheck",
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", ticket.FormatSchedule(now.Add(4*time.Hour)))}
			},
			want: scheduleRecheck,
		},
		{
			name: "a near deadline is waited for exactly",
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", ticket.FormatSchedule(now.Add(20*time.Second)))}
			},
			want: 20 * time.Second,
		},
		{
			name: "the nearest of several wins",
			files: func(h *testHarness) map[string]string {
				return map[string]string{
					"sch-a.md": h.schedTaskMD("sch-a", "open", ticket.FormatSchedule(now.Add(40*time.Second))),
					"sch-b.md": h.schedTaskMD("sch-b", "open", ticket.FormatSchedule(now.Add(10*time.Second))),
				}
			},
			want: 10 * time.Second,
		},
		{
			name: "a deadline already past still sleeps, so a refused promotion cannot spin",
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "open", ticket.FormatSchedule(now.Add(-time.Hour)))}
			},
			want: scheduleFloor,
		},
		{
			name: "a todo ticket carrying a stale timestamp is not a deadline",
			files: func(h *testHarness) map[string]string {
				return map[string]string{"sch-a.md": h.schedTaskMD("sch-a", "todo", ticket.FormatSchedule(now.Add(10*time.Second)))}
			},
			want: scheduleIdle,
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

			assert.Equal(t, tc.want, d.nextScheduleWait(now))
		})
	}
}

func TestScheduleTicketAction(t *testing.T) {
	// Far enough out that the past-instant refusal never catches the table.
	at := "2099-09-01T09:00:00Z"

	cases := []struct {
		name  string
		md    func(h *testHarness) string
		setup func(t *testing.T, h *testHarness, d *Daemon)
		req   web.ScheduleTicketRequest
		// wantErr is a substring of the refusal; empty means the call succeeds.
		wantErr        string
		wantStatus     ticket.Status
		wantScheduleAt string
		wantQueued     []string
	}{
		{
			name:           "schedules an open ticket",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "open", "") },
			req:            web.ScheduleTicketRequest{ScheduledAt: at},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: at,
		},
		{
			name:           "an offset is normalized to UTC",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "open", "") },
			req:            web.ScheduleTicketRequest{ScheduledAt: "2099-09-01T11:00:00+02:00"},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: at,
		},
		{
			name:           "scheduling a queued todo ticket returns it to open and drops it from the queue",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "todo", "") },
			req:            web.ScheduleTicketRequest{ScheduledAt: at},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: at,
		},
		{
			name:           "rescheduling replaces the instant",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "open", "2026-08-01T09:00:00Z") },
			req:            web.ScheduleTicketRequest{ScheduledAt: at},
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: at,
		},
		{
			name:       "clearing removes the field and leaves the ticket open",
			md:         func(h *testHarness) string { return h.schedTaskMD("sch-a", "open", at) },
			req:        web.ScheduleTicketRequest{Clear: true},
			wantStatus: ticket.StatusOpen,
		},
		{
			name:           "a malformed instant is refused",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "open", "") },
			req:            web.ScheduleTicketRequest{ScheduledAt: "next tuesday"},
			wantErr:        "RFC 3339",
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: "",
		},
		{
			name: "a ticket running on another instance is refused",
			// A claim this daemon does not own survives the crash-recovery scan.
			md: func(h *testHarness) string {
				return fmt.Sprintf("---\nid: sch-a\nkontora: true\nstatus: in_progress\npipeline: one-stage\npath: %s\nclaimed_by: other-instance\n---\n# Test ticket sch-a\n", h.repoDir)
			},
			req:            web.ScheduleTicketRequest{ScheduledAt: at},
			wantErr:        "must be open or todo",
			wantStatus:     ticket.StatusInProgress,
			wantScheduleAt: "",
		},
		{
			name: "a ticket the scheduler just claimed is refused",
			md:   func(h *testHarness) string { return h.schedTaskMD("sch-a", "todo", "") },
			setup: func(_ *testing.T, _ *testHarness, d *Daemon) {
				d.mu.Lock()
				d.running["sch-a"] = func() {}
				d.mu.Unlock()
			},
			req:        web.ScheduleTicketRequest{ScheduledAt: at},
			wantErr:    "while it is running",
			wantStatus: ticket.StatusTodo,
			wantQueued: []string{"sch-a"},
		},
		{
			name:           "a done ticket is refused",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "done", "") },
			req:            web.ScheduleTicketRequest{ScheduledAt: at},
			wantErr:        "must be open or todo",
			wantStatus:     ticket.StatusDone,
			wantScheduleAt: "",
		},
		{
			name:           "an archived ticket is refused",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "archived", "") },
			req:            web.ScheduleTicketRequest{ScheduledAt: at},
			wantErr:        "must be open or todo",
			wantStatus:     ticket.StatusArchived,
			wantScheduleAt: "",
		},
		{
			name: "an uninitialized ticket is refused",
			md: func(_ *testHarness) string {
				return "---\nid: sch-a\nstatus: open\n---\n# Foreign\n"
			},
			req:        web.ScheduleTicketRequest{ScheduledAt: at},
			wantErr:    "not initialized",
			wantStatus: ticket.StatusOpen,
		},
		{
			name: "an open plannotator session is refused",
			md:   func(h *testHarness) string { return h.schedTaskMD("sch-a", "open", "") },
			setup: func(_ *testing.T, _ *testHarness, d *Daemon) {
				d.mu.Lock()
				d.plannotator["sch-a"] = func() {}
				d.mu.Unlock()
			},
			req:        web.ScheduleTicketRequest{ScheduledAt: at},
			wantErr:    "plannotator session",
			wantStatus: ticket.StatusOpen,
		},
		{
			name:           "an instant in the past is refused",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "open", "") },
			req:            web.ScheduleTicketRequest{ScheduledAt: "2020-01-01T00:00:00Z"},
			wantErr:        "is in the past",
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: "",
		},
		{
			name:           "a stale schedule can be cleared from a done ticket",
			md:             func(h *testHarness) string { return h.schedTaskMD("sch-a", "done", at) },
			req:            web.ScheduleTicketRequest{Clear: true},
			wantStatus:     ticket.StatusDone,
			wantScheduleAt: "",
		},
		{
			name: "clearing is refused while the ticket runs",
			md:   func(h *testHarness) string { return h.schedTaskMD("sch-a", "open", at) },
			setup: func(_ *testing.T, _ *testHarness, d *Daemon) {
				d.mu.Lock()
				d.running["sch-a"] = func() {}
				d.mu.Unlock()
			},
			req:            web.ScheduleTicketRequest{Clear: true},
			wantErr:        "while it is running",
			wantStatus:     ticket.StatusOpen,
			wantScheduleAt: at,
		},
		{
			name: "a pending annotation is refused",
			md: func(h *testHarness) string {
				return h.schedTaskMD("sch-a", "open", "") + "\n"
			},
			setup: func(t *testing.T, h *testHarness, d *Daemon) {
				tk := h.readTask("sch-a.md")
				require.NoError(t, tk.SetField("annotation_return_status", "open"))
				data, err := tk.Marshal()
				require.NoError(t, err)
				h.writeTicket("sch-a.md", string(data))
				d.mu.Lock()
				d.tickets["sch-a"] = newTicketState(tk, h.taskPath("sch-a.md"))
				d.mu.Unlock()
			},
			req:        web.ScheduleTicketRequest{ScheduledAt: at},
			wantErr:    "annotation run is pending",
			wantStatus: ticket.StatusOpen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeTicket("sch-a.md", tc.md(h))
			d := h.newDaemon(h.cfg)
			require.NoError(t, d.initialScan(h.tasksDir))
			if tc.setup != nil {
				tc.setup(t, h, d)
			}

			err := d.ScheduleTicket("sch-a", tc.req)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}

			got := h.readTask("sch-a.md")
			assert.Equal(t, tc.wantStatus, got.Status)
			assert.Equal(t, tc.wantScheduleAt, got.ScheduledAt)
			assert.ElementsMatch(t, tc.wantQueued, d.queuedIDs())
		})
	}
}

func TestScheduleTicketUnknownID(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	require.NoError(t, d.initialScan(h.tasksDir))

	err := d.ScheduleTicket("ghost", web.ScheduleTicketRequest{ScheduledAt: "2099-09-01T09:00:00Z"})
	require.ErrorIs(t, err, web.ErrTicketNotFound)
}

// A run asked for by hand answers the schedule, so the timestamp goes with the
// move — and it queues the ticket whether or not automatic pickup is on.
func TestRunNowClearsTheSchedule(t *testing.T) {
	for _, autoPickUp := range []bool{true, false} {
		t.Run(fmt.Sprintf("auto_pick_up=%v", autoPickUp), func(t *testing.T) {
			h := newHarness(t)
			h.writeTicket("sch-a.md", h.schedTaskMD("sch-a", "open", "2099-12-01T09:00:00Z"))
			h.cfg.AutoPickUp = new(autoPickUp)
			d := h.newDaemon(h.cfg)
			require.NoError(t, d.initialScan(h.tasksDir))

			require.NoError(t, d.RunTicket("sch-a"))

			got := h.readTask("sch-a.md")
			assert.Equal(t, ticket.StatusTodo, got.Status)
			assert.Empty(t, got.ScheduledAt)
			assert.Equal(t, []string{"sch-a"}, d.queuedIDs())
		})
	}
}

// A write the daemon cannot make must leave the schedule on disk, so a later
// pass tries again rather than losing the ticket in open with no timestamp. It
// must also back the ticket off: a due ticket that keeps failing would otherwise
// hold the loop at its floor and rescan every second for as long as it exists.
func TestPromoteScheduleWriteFailureKeepsTheSchedule(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	past := ticket.FormatSchedule(now.Add(-time.Hour))

	h := newHarness(t)
	path := h.writeTicket("sch-a.md", h.schedTaskMD("sch-a", "open", past))
	d := h.newDaemon(h.cfg)
	require.NoError(t, d.initialScan(h.tasksDir))

	require.NoError(t, os.Chmod(path, 0o444))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	d.promoteDueSchedules(now)

	got := h.readTask("sch-a.md")
	assert.Equal(t, ticket.StatusOpen, got.Status)
	assert.Equal(t, past, got.ScheduledAt)
	assert.Empty(t, d.queuedIDs())
	assert.Equal(t, scheduleRetryMin, d.nextScheduleWait(now), "a failed promotion must not stay due")

	d.promoteDueSchedules(now.Add(scheduleRetryMin))
	assert.Equal(t, 2*scheduleRetryMin, d.nextScheduleWait(now.Add(scheduleRetryMin)), "each failure doubles the wait")

	// A write that succeeds drops the backoff with the schedule.
	retried := now.Add(3 * scheduleRetryMin)
	require.NoError(t, os.Chmod(path, 0o644))
	d.promoteDueSchedules(retried)
	got = h.readTask("sch-a.md")
	assert.Equal(t, ticket.StatusTodo, got.Status)
	assert.Empty(t, got.ScheduledAt)
	assert.Equal(t, scheduleIdle, d.nextScheduleWait(retried))
}

// The dequeue that a schedule makes must not outlive a write that failed: the
// file and the cache both still say todo, and nothing else would re-queue it.
func TestScheduleWriteFailureKeepsTheTicketQueued(t *testing.T) {
	h := newHarness(t)
	path := h.writeTicket("sch-a.md", h.schedTaskMD("sch-a", "todo", ""))
	d := h.newDaemon(h.cfg)
	require.NoError(t, d.initialScan(h.tasksDir))
	require.Equal(t, []string{"sch-a"}, d.queuedIDs())

	require.NoError(t, os.Chmod(path, 0o444))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	require.Error(t, d.ScheduleTicket("sch-a", web.ScheduleTicketRequest{ScheduledAt: "2099-09-01T09:00:00Z"}))

	got := h.readTask("sch-a.md")
	assert.Equal(t, ticket.StatusTodo, got.Status)
	assert.Empty(t, got.ScheduledAt)
	assert.Equal(t, []string{"sch-a"}, d.queuedIDs())
}

// A run asked for while the scheduler is already starting the ticket must be
// refused. Enqueuing it again is a pop that finds the ticket running and puts it
// straight back, which spins for as long as the real run lasts.
func TestRunTicketRefusesARunningTicket(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("sch-a.md", h.schedTaskMD("sch-a", "open", ""))
	d := h.newDaemon(h.cfg)
	require.NoError(t, d.initialScan(h.tasksDir))

	d.mu.Lock()
	d.running["sch-a"] = func() {}
	d.mu.Unlock()

	require.ErrorContains(t, d.RunTicket("sch-a"), "already running")
	assert.Empty(t, d.queuedIDs())
	assert.Equal(t, ticket.StatusOpen, h.readTask("sch-a.md").Status)
}

// Clearing, rescheduling or deleting the nearest ticket must move the loop's
// next wake-up, and must not act on the ticket the stale deadline named.
func TestScheduleRecalculatesAfterChanges(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	near := ticket.FormatSchedule(now.Add(10 * time.Second))
	far := ticket.FormatSchedule(now.Add(45 * time.Second))

	cases := []struct {
		name   string
		change func(t *testing.T, h *testHarness, d *Daemon)
		want   time.Duration
	}{
		{
			name: "clearing the nearest",
			change: func(t *testing.T, _ *testHarness, d *Daemon) {
				require.NoError(t, d.ScheduleTicket("sch-near", web.ScheduleTicketRequest{Clear: true}))
			},
			want: 45 * time.Second,
		},
		{
			name: "moving the nearest later",
			change: func(t *testing.T, _ *testHarness, d *Daemon) {
				require.NoError(t, d.ScheduleTicket("sch-near", web.ScheduleTicketRequest{
					ScheduledAt: ticket.FormatSchedule(now.Add(50 * time.Second)),
				}))
			},
			want: 45 * time.Second,
		},
		{
			name: "deleting the nearest",
			change: func(t *testing.T, h *testHarness, d *Daemon) {
				require.NoError(t, os.Remove(h.taskPath("sch-near.md")))
				d.handleFileRemoved(h.taskPath("sch-near.md"))
			},
			want: 45 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeTicket("sch-near.md", h.schedTaskMD("sch-near", "open", near))
			h.writeTicket("sch-far.md", h.schedTaskMD("sch-far", "open", far))
			d := h.newDaemon(h.cfg)
			require.NoError(t, d.initialScan(h.tasksDir))
			require.Equal(t, 10*time.Second, d.nextScheduleWait(now))

			tc.change(t, h, d)

			assert.Equal(t, tc.want, d.nextScheduleWait(now))
			// The stale deadline must not promote what it named.
			d.promoteDueSchedules(now.Add(20 * time.Second))
			assert.Empty(t, d.queuedIDs())
		})
	}
}

// A daemon started over a store with schedules on both sides of now: the future
// one waits, the past one is promoted by the loop's first pass.
func TestScheduleSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("sch-past.md", h.schedTaskMD("sch-past", "open", ticket.FormatSchedule(time.Now().Add(-time.Hour))))
	h.writeTicket("sch-future.md", h.schedTaskMD("sch-future", "open", ticket.FormatSchedule(time.Now().Add(24*time.Hour))))

	// No agent may run: the promoted ticket must be observable as todo, not as
	// a run already in flight.
	h.cfg.AutoPickUp = new(false)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	promoted := h.waitForStatus("sch-past.md", ticket.StatusTodo, 5*time.Second)
	assert.Empty(t, promoted.ScheduledAt)

	future := h.readTask("sch-future.md")
	assert.Equal(t, ticket.StatusOpen, future.Status)
	assert.NotEmpty(t, future.ScheduledAt)

	cancel()
	require.NoError(t, <-errCh)
}

// The loop must stop with the daemon rather than hold shutdown open.
func TestScheduleLoopStopsWithTheDaemon(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("sch-a.md", h.schedTaskMD("sch-a", "open", ticket.FormatSchedule(time.Now().Add(24*time.Hour))))
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	h.waitForStatus("sch-a.md", ticket.StatusOpen, 2*time.Second)
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

// Scheduling and running the same ticket at the same moment must leave one
// coherent file, not a todo ticket that still carries a timestamp.
func TestScheduleRacesRunNow(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("sch-a.md", h.schedTaskMD("sch-a", "open", ""))
	d := h.newDaemon(h.cfg)
	require.NoError(t, d.initialScan(h.tasksDir))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = d.ScheduleTicket("sch-a", web.ScheduleTicketRequest{ScheduledAt: "2099-12-01T09:00:00Z"})
	}()
	go func() {
		defer wg.Done()
		_ = d.RunTicket("sch-a")
	}()
	wg.Wait()

	got := h.readTask("sch-a.md")
	switch got.Status { //nolint:exhaustive // only these two are reachable here
	case ticket.StatusOpen:
		assert.Equal(t, "2099-12-01T09:00:00Z", got.ScheduledAt, "an open ticket keeps the schedule that put it there")
	case ticket.StatusTodo:
		assert.Empty(t, got.ScheduledAt, "a run clears the schedule in the same save")
	default:
		t.Fatalf("unexpected status %s", got.Status)
	}
}

// An uploaded file arrives as a draft. The status clamp says so, and a
// scheduled_at it carries would undo that: a past one starts the agent within
// the second, on a ticket nobody has read.
func TestUploadDropsTheSchedule(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	require.NoError(t, d.initialScan(h.tasksDir))

	content := "---\nid: up-a\nkontora: true\nstatus: todo\nscheduled_at: \"2020-01-01T00:00:00Z\"\npath: " +
		h.repoDir + "\n---\n# Uploaded\n"

	info, err := d.UploadTicket([]byte(content))
	require.NoError(t, err)
	assert.Equal(t, "open", info.Status)
	assert.Empty(t, info.ScheduledAt)

	got := h.readTask(info.ID + ".md")
	assert.Equal(t, ticket.StatusOpen, got.Status)
	assert.Empty(t, got.ScheduledAt)

	d.promoteDueSchedules(time.Now())
	assert.Empty(t, d.queuedIDs(), "an upload must not queue itself")
}

// Init re-runs on an open ticket, which is exactly where a scheduled one waits.
// An init that queues the ticket answers the schedule; one that leaves it open
// must not, or the remote CLI has no way to re-init without losing the pickup.
func TestInitKeepsTheScheduleOnlyWhenItStaysOpen(t *testing.T) {
	at := "2099-09-01T09:00:00Z"

	cases := []struct {
		name       string
		status     string
		wantStatus ticket.Status
		wantSched  string
	}{
		{name: "the default queues the ticket", wantStatus: ticket.StatusTodo},
		{name: "todo queues the ticket", status: "todo", wantStatus: ticket.StatusTodo},
		{name: "open keeps the schedule", status: "open", wantStatus: ticket.StatusOpen, wantSched: at},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeTicket("sch-a.md", h.schedTaskMD("sch-a", "open", at))
			d := h.newDaemon(h.cfg)
			require.NoError(t, d.initialScan(h.tasksDir))

			require.NoError(t, d.InitTicket("sch-a", web.InitTicketRequest{
				Pipeline: "two-stage",
				Path:     h.repoDir,
				Status:   tc.status,
			}))

			got := h.readTask("sch-a.md")
			assert.Equal(t, tc.wantStatus, got.Status)
			assert.Equal(t, tc.wantSched, got.ScheduledAt)
		})
	}
}
