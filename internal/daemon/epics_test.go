package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// epicMD is a pipeline-less epic file, the shape `kontora new -kind epic`
// writes. status is whatever a hand edit left behind; the daemon derives it.
func (h *testHarness) epicMD(id, status string, children ...string) string {
	list := ""
	if len(children) > 0 {
		list = "children: ["
		for i, c := range children {
			if i > 0 {
				list += ", "
			}
			list += c
		}
		list += "]\n"
	}
	return fmt.Sprintf(`---
id: %s
kontora: true
kind: epic
status: %s
path: %s
%screated: 2026-01-01T00:00:00Z
---
# Epic %s
`, id, status, h.repoDir, list, id)
}

// epicChildMD is a runnable child of an epic. A child with no pipeline is what
// a plain ticket looks like; this one carries one so the daemon can finish it.
func (h *testHarness) epicChildMD(id, status, parent, created string) string {
	return fmt.Sprintf(`---
id: %s
kontora: true
status: %s
pipeline: one-stage
path: %s
parent: %s
created: %s
---
# Child %s
`, id, status, h.repoDir, parent, created, id)
}

// An epic is not work. Whatever status its file carries, it is never queued and
// never claimed, so no agent is ever spawned for it.
func TestEpicIsNeverEnqueuedOrClaimed(t *testing.T) {
	for _, status := range []string{"todo", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			h.writeTicket("epc-a.md", h.epicMD("epc-a", status))

			// Long enough for the watcher debounce and a scheduler pass.
			time.Sleep(600 * time.Millisecond)
			assert.Empty(t, d.queuedIDs())
			assert.Equal(t, 0, d.RunningAgents())
			// With no children it derives to open, so it never sits runnable.
			assert.Equal(t, ticket.StatusOpen, h.readTask("epc-a.md").Status)

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

// Crash recovery resets a running kontora ticket to todo. An epic must be left
// out of that: it is never running, and todo is the one status that would spend
// money on it. Its status is derived from the children instead.
func TestCrashRecoveryLeavesAnEpicAlone(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("epc-a.md", h.epicMD("epc-a", "in_progress", "epc-c1"))
	h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "open", "epc-a", "2026-01-02T00:00:00Z"))

	d := h.newDaemon(h.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	// The one open child derives the epic to open, not to todo.
	h.waitForStatus("epc-a.md", ticket.StatusOpen, 5*time.Second)
	assert.Empty(t, d.queuedIDs())
	assert.Equal(t, 0, d.RunningAgents())

	cancel()
	require.NoError(t, <-errCh)
}

// The last child finishing closes the epic on its own, writes the lifecycle
// note and broadcasts, without anyone moving the epic by hand.
func TestChildFinishingDerivesTheEpic(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("epc-a.md", h.epicMD("epc-a", "open", "epc-c1"))
	h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "todo", "epc-a", "2026-01-02T00:00:00Z"))

	d := h.newDaemon(h.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	h.waitForStatus("epc-c1.md", ticket.StatusDone, 15*time.Second)
	epic := h.waitForStatus("epc-a.md", ticket.StatusDone, 5*time.Second)

	notes := ticket.ParseNotes(epic.Body)
	require.Len(t, notes, 1)
	assert.Equal(t, ticket.SystemAuthor, notes[0].Author)
	assert.Contains(t, notes[0].Text, "closed itself when epc-c1 landed")

	cancel()
	require.NoError(t, <-errCh)
}

// An epic has no pipeline to run and no status of its own to write, so every
// verb that would put one there refuses it rather than succeeding and being
// silently undone by the next derivation.
func TestEpicRefusesTheRunVerbs(t *testing.T) {
	h := newHarness(t)
	// A child in human_review holds the epic at in_progress, which is the
	// status pause needs and the one nothing else here changes.
	h.writeTicket("epc-a.md", h.epicMD("epc-a", "open", "epc-c1"))
	h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "human_review", "epc-a", "2026-01-02T00:00:00Z"))
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	require.Eventually(t, func() bool {
		return h.readTask("epc-a.md").Status == ticket.StatusInProgress
	}, 5*time.Second, 20*time.Millisecond)

	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		call func() error
	}{
		{name: "run", call: func() error { return d.RunTicket("epc-a") }},
		{name: "move", call: func() error { return d.MoveTicket("epc-a", "done") }},
		{name: "pause", call: func() error { return d.PauseTicket("epc-a") }},
		{name: "schedule", call: func() error {
			return d.ScheduleTicket("epc-a", web.ScheduleTicketRequest{ScheduledAt: at})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.ErrorIs(t, err, web.ErrInvalidState)
			epic := h.readTask("epc-a.md")
			assert.Equal(t, ticket.StatusInProgress, epic.Status)
			assert.Empty(t, epic.ScheduledAt)
		})
	}

	cancel()
	require.NoError(t, <-errCh)
}

// Archived is terminal for an epic too. The derivation cannot produce it, so
// without an early return the archive write is followed straight back to the
// derived status, leaving a board card carrying archive stamps.
func TestArchivingAnEpicSticks(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("epc-a.md", h.epicMD("epc-a", "open", "epc-c1"))
	h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "done", "epc-a", "2026-01-02T00:00:00Z"))

	d := h.newDaemon(h.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	require.Eventually(t, func() bool {
		return h.readTask("epc-a.md").Status == ticket.StatusDone
	}, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, d.ArchiveTicket("epc-a", "filed away"))
	assert.Equal(t, ticket.StatusArchived, h.readTask("epc-a.md").Status)

	// Restoring hands the epic back to the derivation, which is what should own
	// its status again.
	require.NoError(t, d.RestoreTicket("epc-a"))
	assert.Equal(t, ticket.StatusDone, h.readTask("epc-a.md").Status)

	cancel()
	require.NoError(t, <-errCh)
}

// An epic's status is a summary of the children it has now, so losing one is a
// reason to derive it again: the last unfinished child leaving finishes it.
func TestDeletingAChildDerivesTheEpic(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("epc-a.md", h.epicMD("epc-a", "open", "epc-c1", "epc-c2"))
	h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "done", "epc-a", "2026-01-02T00:00:00Z"))
	h.writeTicket("epc-c2.md", h.epicChildMD("epc-c2", "human_review", "epc-a", "2026-01-03T00:00:00Z"))

	d := h.newDaemon(h.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	require.Eventually(t, func() bool {
		return h.readTask("epc-a.md").Status == ticket.StatusInProgress
	}, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, d.DeleteTicket("epc-c2"))
	assert.Equal(t, ticket.StatusDone, h.readTask("epc-a.md").Status)

	cancel()
	require.NoError(t, <-errCh)
}

// Moving a child between epics changes two of them. The save carries only the
// epic it joined, so the one it left has to be derived from the copy the cache
// still holds, or it keeps counting a child it no longer has.
func TestMovingAChildDerivesBothEpics(t *testing.T) {
	cases := []struct {
		name string
		move func(d *Daemon) error
	}{
		{name: "re-parent", move: func(d *Daemon) error { return d.SetParent("epc-c1", "epc-b") }},
		{name: "unparent", move: func(d *Daemon) error { return d.ClearParent("epc-c1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeTicket("epc-a.md", h.epicMD("epc-a", "open", "epc-c1"))
			h.writeTicket("epc-b.md", h.epicMD("epc-b", "open"))
			h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "human_review", "epc-a", "2026-01-02T00:00:00Z"))

			d := h.newDaemon(h.cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			require.Eventually(t, func() bool {
				return h.readTask("epc-a.md").Status == ticket.StatusInProgress
			}, 5*time.Second, 20*time.Millisecond)

			require.NoError(t, tc.move(d))

			// An epic with no children is open, whichever end it is.
			assert.Equal(t, ticket.StatusOpen, h.readTask("epc-a.md").Status)
			if tc.name == "re-parent" {
				assert.Equal(t, ticket.StatusInProgress, h.readTask("epc-b.md").Status)
			}

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

// trackedKind reports the kind the daemon has cached for a ticket, or "" when
// it does not track one.
func (d *Daemon) trackedKind(id string) ticket.Kind {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ts, ok := d.tickets[id]; ok {
		return ts.ticket.Kind
	}
	return ""
}

// Deleting an epic keeps its work: the children are ordinary tickets filed
// under it, so they lose the parent and nothing else.
func TestDeletingAnEpicKeepsItsChildren(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("epc-a.md", h.epicMD("epc-a", "open", "epc-c1", "epc-c2", "epc-c3"))
	for i, id := range []string{"epc-c1", "epc-c2", "epc-c3"} {
		h.writeTicket(id+".md", h.epicChildMD(id, "open", "epc-a", fmt.Sprintf("2026-01-0%dT00:00:00Z", i+2)))
	}

	d := h.newDaemon(h.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	require.Eventually(t, func() bool { return d.trackedKind("epc-a") == ticket.KindEpic },
		5*time.Second, 20*time.Millisecond)

	require.NoError(t, d.DeleteTicket("epc-a"))

	for _, id := range []string{"epc-c1", "epc-c2", "epc-c3"} {
		child := h.readTask(id + ".md")
		assert.Equal(t, ticket.StatusOpen, child.Status)
		assert.Empty(t, child.Parent, "%s must survive the delete without a dangling parent", id)
	}

	cancel()
	require.NoError(t, <-errCh)
}

// The brief is the whole reason an epic exists, and an epic spends most of its
// life derived to in_progress or done. Both are statuses an ordinary ticket may
// not be edited in, so the guard has to let an epic through.
func TestEpicBriefIsEditableInEveryDerivedStatus(t *testing.T) {
	h := newHarness(t)
	// A child waiting on a person is neither recovered nor queued, so the epic
	// settles on in_progress and stays there for the length of the test.
	h.writeTicket("epc-a.md", h.epicMD("epc-a", "open", "epc-c1"))
	h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "human_review", "epc-a", "2026-01-02T00:00:00Z"))
	h.writeTicket("tst-p1.md", h.taskMD("tst-p1", "done", "one-stage"))

	d := h.newDaemon(h.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	require.Eventually(t, func() bool {
		return h.readTask("epc-a.md").Status == ticket.StatusInProgress
	}, 5*time.Second, 20*time.Millisecond)

	body := "# Epic epc-a\n\n## Description\n\nEdited while the epic was running.\n"
	require.NoError(t, d.UpdateTicket("epc-a", web.UpdateTicketRequest{Body: &body}))
	assert.Equal(t, body, h.readTask("epc-a.md").Body)

	// An ordinary ticket in a status the guard refuses is still refused: the
	// exemption is the kind, not a hole in the check.
	other := "# changed\n"
	assert.ErrorIs(t, d.UpdateTicket("tst-p1", web.UpdateTicketRequest{Body: &other}), web.ErrInvalidState)

	cancel()
	require.NoError(t, <-errCh)
}
