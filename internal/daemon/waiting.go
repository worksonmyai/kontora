package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// defaultWaitPollInterval is how often a run's poller stats its marker. The
// extension and the agent process have no channel to the daemon, so the marker
// file is the only signal, and a stat every couple of seconds costs less than
// an fsnotify watcher per concurrent run.
const defaultWaitPollInterval = 2 * time.Second

// waitingState is what the daemon holds for a run whose agent is blocked on a
// question tool. It exists only while that process is alive, so it lives in
// memory rather than in the ticket file: persisting it would leave stale state
// behind after a crash and rewrite the markdown for every question.
type waitingState struct {
	Tool       string
	ToolCallID string
	Since      time.Time
	Question   string
}

// waitMarker is the on-disk form the pi extension writes.
type waitMarker struct {
	Tool       string `json:"tool"`
	ToolCallID string `json:"tool_call_id"`
	StartedAt  string `json:"started_at"`
	Question   string `json:"question"`
}

// readWaitMarker parses the marker at path, or returns nil when it is absent or
// unreadable. seen stands in for a start time the extension wrote in a form
// this daemon cannot parse, so a badge still gets a clock.
func readWaitMarker(path string, seen time.Time) *waitingState {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m waitMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	if m.Tool == "" {
		return nil
	}
	since, err := time.Parse(time.RFC3339, m.StartedAt)
	if err != nil {
		since = seen
	}
	return &waitingState{
		Tool:       m.Tool,
		ToolCallID: m.ToolCallID,
		// Normalized so applyWaitMarker's == holds across polls: parsing an
		// offset like +02:00 builds a fresh location each time, and the ticker's
		// own time carries a monotonic reading. Either would compare unequal to
		// itself and rebroadcast the same wait every tick.
		Since:    since.UTC(),
		Question: m.Question,
	}
}

// startWaitWatcher polls one run's marker until the returned stop function is
// called. stop does not return until the poller has, so a caller that clears
// the ticket's waiting state right after cannot have a tick already in flight
// put it straight back.
func (d *Daemon) startWaitWatcher(ticketID, path string) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.watchWaitMarker(ctx, ticketID, path)
	}()
	return func() {
		cancel()
		<-done
	}
}

// watchWaitMarker follows one run's marker until ctx ends.
func (d *Daemon) watchWaitMarker(ctx context.Context, ticketID, path string) {
	ticker := time.NewTicker(d.waitPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.applyWaitMarker(ticketID, readWaitMarker(path, now))
		}
	}
}

// applyWaitMarker stores what the poller read and broadcasts when anything a
// reader can see has changed, including a second question replacing the first.
func (d *Daemon) applyWaitMarker(ticketID string, st *waitingState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, had := d.waiting[ticketID]
	switch {
	case st == nil:
		if !had {
			return
		}
		delete(d.waiting, ticketID)
	default:
		// An unparseable started_at falls back to the poll's own time, which
		// differs every tick. Carrying the first one forward keeps the badge's
		// clock growing and the same wait from rebroadcasting every 2s.
		if had && prev.Tool == st.Tool && prev.ToolCallID == st.ToolCallID && prev.Question == st.Question {
			st.Since = prev.Since
		}
		if had && prev == *st {
			return
		}
		d.waiting[ticketID] = *st
	}
	d.broadcastTicketUpdate(ticketID)
}

// pendingWait is the waiting state of a ticket right now, for a caller that has
// to read it before the run's cleanup drops it.
func (d *Daemon) pendingWait(ticketID string) (waitingState, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.waiting[ticketID]
	return st, ok
}

// clearWaiting drops a ticket's waiting state when its run ends, whatever ended
// it. A ticket that was not waiting is left alone, and broadcasts nothing.
func (d *Daemon) clearWaiting(ticketID string) {
	d.applyWaitMarker(ticketID, nil)
}

// runnerFailureReason is what the ticket pauses with when the runner returns an
// error. A stage deadline that fired while the agent was blocked on a question
// names it: the bare timeout reads as a stuck agent and says nothing about the
// question nobody answered. The original error is kept whole, because existing
// notes and searches match on its text.
//
// The pending wait is passed in rather than read here: the agent's own shutdown
// removes the marker, so it has to be captured as soon as the runner returns,
// before the log and tmux cleanup that stands between there and this call.
func runnerFailureReason(runnerErr error, st waitingState, ok bool) string {
	reason := "runner failed: " + runnerErr.Error()
	if !ok || !errors.Is(runnerErr, context.DeadlineExceeded) {
		return reason
	}
	reason += " (agent was waiting on " + st.Tool
	if st.Question != "" {
		reason += ": " + st.Question
	}
	return reason + ")"
}
