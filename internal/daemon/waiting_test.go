package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// writeWaitMarker writes the marker the pi extension would write, the same way
// the extension does: into a temp file that is renamed in.
func writeWaitMarker(t *testing.T, path, tool, callID, question string, since time.Time) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	body := fmt.Sprintf(`{"tool":%q,"tool_call_id":%q,"started_at":%q,"question":%q}`,
		tool, callID, since.Format(time.RFC3339), question)
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(body), 0o644))
	require.NoError(t, os.Rename(tmp, path))
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, timeout, 10*time.Millisecond, msg)
}

// fastWaitPoll keeps the marker poller well under a test's patience.
const fastWaitPoll = 20 * time.Millisecond

// waitingEvents collects the waiting state of every ticket_updated event for one
// ticket, so a test can assert on the transitions rather than a final state.
type waitingEvents struct {
	ch     <-chan web.TicketEvent
	unsub  func()
	id     string
	states []web.TicketInfo
}

func subscribeWaiting(d *Daemon, id string) *waitingEvents {
	ch, unsub := d.Subscribe()
	return &waitingEvents{ch: ch, unsub: unsub, id: id}
}

// awaitWaiting drains events until one reports the wanted waiting flag.
func (w *waitingEvents) awaitWaiting(t *testing.T, want bool, timeout time.Duration) web.TicketInfo {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-w.ch:
			if ev.Type != "ticket_updated" || ev.Ticket.ID != w.id {
				continue
			}
			w.states = append(w.states, ev.Ticket)
			if ev.Ticket.WaitingForInput == want {
				return ev.Ticket
			}
		case <-deadline:
			t.Fatalf("no ticket_updated with waiting_for_input=%v for %s (saw %d events)", want, w.id, len(w.states))
			return web.TicketInfo{}
		}
	}
}

// runningTicket returns a ticket's current board projection.
func runningTicket(t *testing.T, d *Daemon, id string) web.TicketInfo {
	t.Helper()
	for _, info := range d.ListTickets(web.ListTicketsOptions{}) {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("ticket %s not in the list payload", id)
	return web.TicketInfo{}
}

// TestWaiting_MarkerDrivesTicketState runs a pi stage whose agent publishes a
// marker, holds it, then clears it and exits cleanly. It covers the whole
// requirement: the state appears, both transitions broadcast, the run stays
// alive through the wait, and nothing is left behind afterwards.
func TestWaiting_MarkerDrivesTicketState(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("pi", "pi")
	cfg.Pipelines["one-stage"] = config.Pipeline{
		{Stage: "step1", Agent: "agent1", OnSuccess: "done", OnFailure: "pause"},
	}

	since := time.Now().Add(-90 * time.Second).Truncate(time.Second)
	release := make(chan struct{})
	agentRunning := make(chan struct{}, 1)
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		marker := stageWaitPath(cfg, p.TicketID, "step1")
		writeWaitMarker(t, marker, "ask_user_question", "call-7", "Which database?", since)
		agentRunning <- struct{}{}
		<-release
		os.Remove(marker)
		// Stay alive past the removal so the poller sees the cleared state
		// while the run is still going.
		time.Sleep(4 * fastWaitPoll)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := h.newDaemon(cfg, WithRunner(runner), WithWaitPollInterval(fastWaitPoll))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	events := subscribeWaiting(d, "tst-wait")
	defer events.unsub()

	h.writeTicket("tst-wait.md", h.taskMD("tst-wait", "todo", "one-stage"))
	<-agentRunning

	got := events.awaitWaiting(t, true, 10*time.Second)
	assert.Equal(t, "ask_user_question", got.WaitingTool)
	assert.Equal(t, "Which database?", got.WaitingQuestion)
	require.NotNil(t, got.WaitingSince)
	assert.True(t, got.WaitingSince.Equal(since), "waiting_since %s should be the marker's %s", got.WaitingSince, since)

	// A page reload reads the same state off the list endpoint.
	listed := runningTicket(t, d, "tst-wait")
	assert.True(t, listed.WaitingForInput)
	assert.Equal(t, got.WaitingTool, listed.WaitingTool)
	assert.Equal(t, got.WaitingQuestion, listed.WaitingQuestion)
	require.NotNil(t, listed.WaitingSince)
	assert.True(t, listed.WaitingSince.Equal(since))

	// The run is still going: waiting is not an exit.
	assert.Equal(t, string(ticket.StatusInProgress), string(h.readTask("tst-wait.md").Status))

	close(release)

	cleared := events.awaitWaiting(t, false, 10*time.Second)
	assert.Empty(t, cleared.WaitingTool)
	assert.Empty(t, cleared.WaitingQuestion)
	assert.Nil(t, cleared.WaitingSince)

	h.waitForStatus("tst-wait.md", ticket.StatusDone, 10*time.Second)
	assert.NoFileExists(t, stageWaitPath(cfg, "tst-wait", "step1"))

	cancel()
	require.NoError(t, <-errCh)
}

// TestWaiting_StaleMarkerIsDeletedBeforeTheRun covers a previous run that died
// with its marker in place: the daemon must not read it as this run's question.
func TestWaiting_StaleMarkerIsDeletedBeforeTheRun(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("pi", "pi")
	marker := stageWaitPath(cfg, "tst-stale", "step1")
	writeWaitMarker(t, marker, "ask_user_question", "old-call", "A question from last time", time.Now())

	sawStale := make(chan bool, 1)
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		_, err := os.Stat(stageWaitPath(cfg, p.TicketID, "step1"))
		sawStale <- err == nil
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := h.newDaemon(cfg, WithRunner(runner), WithWaitPollInterval(fastWaitPoll))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-stale.md", h.taskMD("tst-stale", "todo", "one-stage"))
	h.waitForStatus("tst-stale.md", ticket.StatusDone, 10*time.Second)

	assert.False(t, <-sawStale, "the stale marker must be gone before the agent starts")
	assert.False(t, runningTicket(t, d, "tst-stale").WaitingForInput,
		"a stale marker must never become the new run's waiting state")

	cancel()
	require.NoError(t, <-errCh)
}

// TestWaiting_CancelledRunClearsState covers the run ending while the marker is
// still there: no ticket may keep reporting a wait its process cannot answer.
func TestWaiting_CancelledRunClearsState(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("pi", "pi")

	runner := func(runCtx context.Context, p RunnerParams) (process.Result, error) {
		writeWaitMarker(t, stageWaitPath(cfg, p.TicketID, "step1"),
			"ask_user_question", "call-1", "Answer me", time.Now())
		<-runCtx.Done()
		return process.Result{ExitCode: -1, StartedAt: time.Now(), ExitedAt: time.Now()}, runCtx.Err()
	}

	d := h.newDaemon(cfg, WithRunner(runner), WithWaitPollInterval(fastWaitPoll))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-cxl.md", h.taskMD("tst-cxl", "todo", "one-stage"))

	waitFor(t, 10*time.Second, "ticket should report the wait", func() bool {
		_, ok := d.pendingWait("tst-cxl")
		return ok
	})

	require.NoError(t, d.PauseTicket("tst-cxl"))

	waitFor(t, 10*time.Second, "cancelling must clear the waiting state", func() bool {
		_, ok := d.pendingWait("tst-cxl")
		return !ok
	})
	waitFor(t, 10*time.Second, "cancelling must remove the marker", func() bool {
		_, err := os.Stat(stageWaitPath(cfg, "tst-cxl", "step1"))
		return os.IsNotExist(err)
	})

	cancel()
	require.NoError(t, <-errCh)
}

// TestWaiting_TimeoutNamesTheQuestion covers the stage deadline killing a run
// that sat on a question. The bare timeout says nothing about why the agent
// stopped, so the pause reason names the tool and the question — and keeps the
// original runner error, which existing notes and searches match on.
func TestWaiting_TimeoutNamesTheQuestion(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("pi", "pi")

	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		writeWaitMarker(t, stageWaitPath(cfg, p.TicketID, "step1"),
			"ask_user_question", "call-1", "Should I drop the column?", time.Now())
		// Give the poller time to read it, then fail the way the tmux runner
		// fails when the stage deadline fires.
		time.Sleep(6 * fastWaitPoll)
		return process.Result{ExitCode: -1, StartedAt: time.Now(), ExitedAt: time.Now()}, context.DeadlineExceeded
	}

	d := h.newDaemon(cfg, WithRunner(runner), WithWaitPollInterval(fastWaitPoll))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-tmo.md", h.taskMD("tst-tmo", "todo", "one-stage"))
	tkt := h.waitForStatus("tst-tmo.md", ticket.StatusPaused, 15*time.Second)

	assert.Contains(t, tkt.LastError, "runner failed: context deadline exceeded")
	assert.Contains(t, tkt.LastError, "ask_user_question")
	assert.Contains(t, tkt.LastError, "Should I drop the column?")
	assert.Contains(t, tkt.Body, "runner failed: context deadline exceeded", "the pause reason is appended as a note")
	assert.Contains(t, tkt.Body, "Should I drop the column?")
	assert.NoFileExists(t, stageWaitPath(cfg, "tst-tmo", "step1"))

	cancel()
	require.NoError(t, <-errCh)
}

// TestWaiting_NoMarkerForOtherAgents covers the runs that must be unchanged: a
// non-pi agent is passed no marker path at all, and a pi run that never calls a
// question tool never reports a wait.
func TestWaiting_NoMarkerForOtherAgents(t *testing.T) {
	t.Run("claude gets no marker path and unchanged settings", func(t *testing.T) {
		claude := config.Agent{Binary: "claude"}
		args, settingsFile, _, err := buildAgentArgs(claude, "prompt", "", "chan", "", "", "", "/logs/kon-1/step1.waiting.json", nil, true)
		require.NoError(t, err)
		t.Cleanup(func() { os.Remove(settingsFile) })

		require.Contains(t, args, "--settings")
		body, err := os.ReadFile(settingsFile)
		require.NoError(t, err)
		assert.NotContains(t, string(body), "waiting")
		assert.JSONEq(t,
			`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"tmux wait-for -S chan"}]}],`+
				`"Notification":[{"matcher":"idle_prompt","hooks":[{"type":"command","command":"tmux wait-for -S chan"}]}]}}`,
			string(body))
	})

	t.Run("pi still gets -e with the extension", func(t *testing.T) {
		pi := config.Agent{Binary: "pi"}
		args, extFile, _, err := buildAgentArgs(pi, "prompt", "", "chan", "", "", "", "/logs/kon-1/step1.waiting.json", nil, true)
		require.NoError(t, err)
		t.Cleanup(func() { os.Remove(extFile) })

		require.Contains(t, args, "-e")
		assert.Equal(t, extFile, args[slices.Index(args, "-e")+1])
		body, err := os.ReadFile(extFile)
		require.NoError(t, err)
		assert.Contains(t, string(body), `const WAIT_MARKER = "/logs/kon-1/step1.waiting.json";`)
	})

	t.Run("a pi run that asks nothing reports nothing", func(t *testing.T) {
		h := newHarness(t)
		cfg := h.defaultConfig("pi", "pi")
		runner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
			time.Sleep(4 * fastWaitPoll)
			return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
		}
		d := h.newDaemon(cfg, WithRunner(runner), WithWaitPollInterval(fastWaitPoll))

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()
		time.Sleep(200 * time.Millisecond)

		h.writeTicket("tst-quiet.md", h.taskMD("tst-quiet", "todo", "one-stage"))
		h.waitForStatus("tst-quiet.md", ticket.StatusDone, 10*time.Second)

		_, waiting := d.pendingWait("tst-quiet")
		assert.False(t, waiting)
		assert.NoFileExists(t, stageWaitPath(cfg, "tst-quiet", "step1"))

		cancel()
		require.NoError(t, <-errCh)
	})
}

func TestReadWaitMarker(t *testing.T) {
	seen := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	since := time.Date(2026, 8, 22, 11, 30, 0, 0, time.UTC)

	cases := []struct {
		name string
		body string
		want *waitingState
	}{
		{
			name: "complete marker",
			body: `{"tool":"ask_user_question","tool_call_id":"c1","started_at":"2026-08-22T11:30:00Z","question":"Which one?"}`,
			want: &waitingState{Tool: "ask_user_question", ToolCallID: "c1", Since: since, Question: "Which one?"},
		},
		{
			name: "no question text falls back to the tool alone",
			body: `{"tool":"question","tool_call_id":"c2","started_at":"2026-08-22T11:30:00Z","question":""}`,
			want: &waitingState{Tool: "question", ToolCallID: "c2", Since: since},
		},
		{
			// A clock the daemon cannot parse still has to produce a badge.
			name: "unparseable start time falls back to the read time",
			body: `{"tool":"question","tool_call_id":"c3","started_at":"yesterday"}`,
			want: &waitingState{Tool: "question", ToolCallID: "c3", Since: seen},
		},
		{
			// What the extension actually writes: Date.toISOString(), which
			// carries milliseconds. RFC3339 parsing takes them.
			name: "the extension's own millisecond timestamp",
			body: `{"tool":"ask_user_question","tool_call_id":"c0","started_at":"2026-08-22T11:30:00.227Z","question":"Q"}`,
			want: &waitingState{Tool: "ask_user_question", ToolCallID: "c0", Since: since.Add(227 * time.Millisecond), Question: "Q"},
		},
		{
			// A zone offset must not survive into the state: two reads of one
			// marker have to compare equal, or the poller rebroadcasts the same
			// wait on every tick.
			name: "offset start time is normalized to UTC",
			body: `{"tool":"question","tool_call_id":"c5","started_at":"2026-08-22T13:30:00+02:00"}`,
			want: &waitingState{Tool: "question", ToolCallID: "c5", Since: since},
		},
		{name: "not json", body: `{"tool":`, want: nil},
		{name: "no tool", body: `{"tool_call_id":"c4"}`, want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "step1.waiting.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o644))
			got := readWaitMarker(path, seen)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tc.want, *got)
			// applyWaitMarker suppresses a rebroadcast with ==, so two reads of
			// one unchanged marker have to be equal by that operator.
			assert.True(t, *got == *readWaitMarker(path, seen), "repeat reads must compare equal")
		})
	}

	t.Run("absent file", func(t *testing.T) {
		assert.Nil(t, readWaitMarker(filepath.Join(t.TempDir(), "gone.json"), seen))
	})
}

// An unparseable started_at falls back to the poll's own clock, which differs
// on every tick. Without the carry-forward the stored state changes twice a
// second, so the badge's clock resets and every poll rebroadcasts the same wait.
func TestApplyWaitMarkerCarriesForwardAnUnparseableStart(t *testing.T) {
	// Built by hand rather than through New, so the seams New installs have to
	// be named: applyWaitMarker reaches the notifier for a new question.
	d := &Daemon{waiting: map[string]waitingState{}, notifier: noopNotifier{}}
	path := filepath.Join(t.TempDir(), "step1.waiting.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"tool":"question","tool_call_id":"c1","started_at":"yesterday","question":"Q1"}`), 0o644))

	first := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	d.applyWaitMarker("tst-1", readWaitMarker(path, first))
	d.applyWaitMarker("tst-1", readWaitMarker(path, first.Add(2*time.Second)))
	d.applyWaitMarker("tst-1", readWaitMarker(path, first.Add(4*time.Second)))

	st, ok := d.pendingWait("tst-1")
	require.True(t, ok)
	assert.Equal(t, first, st.Since, "the first read's clock must survive later polls")

	// A different question is a different wait, so it takes the new clock.
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"tool":"question","tool_call_id":"c2","started_at":"yesterday","question":"Q2"}`), 0o644))
	d.applyWaitMarker("tst-1", readWaitMarker(path, first.Add(6*time.Second)))

	st, ok = d.pendingWait("tst-1")
	require.True(t, ok)
	assert.Equal(t, "Q2", st.Question)
	assert.Equal(t, first.Add(6*time.Second), st.Since)
}
