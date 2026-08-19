package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTmux records what the loop typed and lets the test release each wait, so
// the loop's decisions can be driven without a tmux server.
type stubTmux struct {
	mu    sync.Mutex
	typed []string
	waits map[string]chan struct{}
	// latched counts signals sent while nobody was waiting, which tmux holds
	// and hands to the next waiter.
	latched  map[string]int
	killed   bool
	exited   bool
	noWindow bool
}

func newStubTmux(channels ...string) *stubTmux {
	s := &stubTmux{
		waits:   make(map[string]chan struct{}, len(channels)),
		latched: make(map[string]int),
	}
	for _, c := range channels {
		s.waits[c] = make(chan struct{})
	}
	return s
}

// latch leaves a signal on channel the way tmux does when it is sent while
// nobody is waiting.
func (s *stubTmux) latch(channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latched[channel]++
}

func (s *stubTmux) SendKeys(keys string) error    { return s.record("keys:" + keys) }
func (s *stubTmux) SendLiteral(text string) error { return s.record("literal:" + text) }
func (s *stubTmux) KillWindow() error             { s.set(&s.killed); return nil }
func (s *stubTmux) Signal(string)                 {}
func (s *stubTmux) WaitWindowExit(time.Duration)  { s.set(&s.exited) }

func (s *stubTmux) HasWindow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.noWindow
}

func (s *stubTmux) WaitFor(ctx context.Context, channel string) error {
	s.mu.Lock()
	if s.latched[channel] > 0 {
		s.latched[channel]--
		s.mu.Unlock()
		return nil
	}
	ch, ok := s.waits[channel]
	s.mu.Unlock()
	if !ok {
		// A channel nobody signals: block until the run ends.
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitTyped blocks until the loop has typed something starting with prefix, so
// a test can answer a keystroke rather than race it.
func (s *stubTmux) awaitTyped(t *testing.T, prefix string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, got := range s.sent() {
			if strings.HasPrefix(got, prefix) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q to be typed", prefix)
}

// signal releases one waiter on channel.
func (s *stubTmux) signal(t *testing.T, channel string) {
	t.Helper()
	s.mu.Lock()
	ch := s.waits[channel]
	s.mu.Unlock()
	require.NotNil(t, ch, "no stub channel %q", channel)
	select {
	case ch <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out signalling %q", channel)
	}
}

func (s *stubTmux) record(what string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.typed = append(s.typed, what)
	return nil
}

func (s *stubTmux) set(field *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*field = true
}

func (s *stubTmux) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.typed...)
}

func TestIdleLoop(t *testing.T) {
	const (
		ticketID = "tst-idle"
		session  = "kontora-idletest"
	)
	channel := ChannelName(session, ticketID)
	compactChannel := CompactChannelName(session, ticketID, "")

	tests := []struct {
		name string
		// decisions are handed out in order, one per idle signal. The last one
		// repeats if the loop asks again. Nil means no OnIdle callback at all.
		decisions []IdleDecision
		// run releases the channels that drive one pass of the loop.
		run        func(t *testing.T, tm *stubTmux)
		wantTyped  []string
		wantExited bool
	}{
		{
			name:      "no callback finishes on the first idle",
			decisions: nil,
			run: func(t *testing.T, tm *stubTmux) {
				tm.signal(t, channel)
			},
			wantTyped:  []string{"keys:/exit"},
			wantExited: true,
		},
		{
			name:      "finish on the first idle",
			decisions: []IdleDecision{{Action: IdleFinish}},
			run: func(t *testing.T, tm *stubTmux) {
				tm.signal(t, channel)
			},
			wantTyped:  []string{"keys:/exit"},
			wantExited: true,
		},
		{
			name: "prompt then finish",
			decisions: []IdleDecision{
				{Action: IdlePrompt, Continuation: "Continue with Phase 3."},
				{Action: IdleFinish},
			},
			run: func(t *testing.T, tm *stubTmux) {
				tm.signal(t, channel)
				tm.signal(t, channel)
			},
			wantTyped:  []string{"literal:Continue with Phase 3.", "keys:/exit"},
			wantExited: true,
		},
		{
			name: "compact then prompt then finish",
			decisions: []IdleDecision{
				{Action: IdleCompact, CompactInstructions: "Preserve the goal.", Continuation: "Continue with Phase 3."},
				{Action: IdleFinish},
			},
			run: func(t *testing.T, tm *stubTmux) {
				tm.signal(t, channel)
				// Answering only once /compact is on screen keeps the hook out
				// of the drain that precedes it.
				tm.awaitTyped(t, "literal:/compact")
				tm.signal(t, compactChannel)
				tm.signal(t, channel)
			},
			wantTyped: []string{
				"literal:/compact Preserve the goal.",
				"literal:Continue with Phase 3.",
				"keys:/exit",
			},
			wantExited: true,
		},
		{
			name: "compact wait timeout is reported to the next decision",
			decisions: []IdleDecision{
				{Action: IdleCompact, Continuation: "Continue with Phase 3."},
				{Action: IdleFinish},
			},
			run: func(t *testing.T, tm *stubTmux) {
				tm.signal(t, channel)
				// The compact channel is never signalled, so the wait times out
				// and the loop delivers the continuation anyway.
				tm.signal(t, channel)
			},
			wantTyped: []string{
				"literal:/compact",
				"literal:Continue with Phase 3.",
				"keys:/exit",
			},
			wantExited: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := newStubTmux(channel, compactChannel)

			var mu sync.Mutex
			var onIdle func(context.Context, IdleEvent) IdleDecision
			if tt.decisions != nil {
				decisions := tt.decisions
				onIdle = func(context.Context, IdleEvent) IdleDecision {
					mu.Lock()
					defer mu.Unlock()
					d := decisions[0]
					if len(decisions) > 1 {
						decisions = decisions[1:]
					}
					return d
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			type outcome struct {
				code int
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				code, err := idleLoop(ctx, tm, RunParams{
					TicketID:       ticketID,
					SessionName:    session,
					MinDuration:    -1,
					OnIdle:         onIdle,
					CompactTimeout: 200 * time.Millisecond,
				}, time.Now())
				done <- outcome{code, err}
			}()

			tt.run(t, tm)

			select {
			case got := <-done:
				require.NoError(t, got.err)
				assert.Equal(t, 0, got.code)
			case <-time.After(10 * time.Second):
				t.Fatal("idleLoop did not return")
			}

			assert.Equal(t, tt.wantTyped, tm.sent())
			assert.Equal(t, tt.wantExited, tm.exited)
		})
	}
}

func TestIdleLoopCompactErrorReachesNextDecision(t *testing.T) {
	const (
		ticketID = "tst-idle-err"
		session  = "kontora-idletest"
	)
	channel := ChannelName(session, ticketID)
	tm := newStubTmux(channel, CompactChannelName(session, ticketID, ""))

	var mu sync.Mutex
	var events []IdleEvent
	calls := 0
	onIdle := func(_ context.Context, e IdleEvent) IdleDecision {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
		calls++
		if calls == 1 {
			return IdleDecision{Action: IdleCompact, Continuation: "Continue."}
		}
		return IdleDecision{Action: IdleFinish}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := idleLoop(ctx, tm, RunParams{
			TicketID:       ticketID,
			SessionName:    session,
			MinDuration:    -1,
			OnIdle:         onIdle,
			CompactTimeout: 200 * time.Millisecond,
		}, time.Now())
		done <- err
	}()

	tm.signal(t, channel)
	tm.signal(t, channel)
	require.NoError(t, <-done)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 2)
	assert.NoError(t, events[0].CompactErr)
	require.Error(t, events[1].CompactErr)
	assert.ErrorContains(t, events[1].CompactErr, "did not finish within")
}

func TestIdleLoopWindowGone(t *testing.T) {
	const (
		ticketID = "tst-idle-gone"
		session  = "kontora-idletest"
	)
	tm := newStubTmux()
	tm.noWindow = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code, err := idleLoop(ctx, tm, RunParams{
		TicketID:    ticketID,
		SessionName: session,
		MinDuration: -1,
		OnIdle:      func(context.Context, IdleEvent) IdleDecision { return IdleDecision{Action: IdlePrompt} },
	}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, code)
	assert.Empty(t, tm.sent())
}

func TestIdleLoopMinDuration(t *testing.T) {
	const (
		ticketID = "tst-idle-fast"
		session  = "kontora-idletest"
	)
	channel := ChannelName(session, ticketID)
	tm := newStubTmux(channel)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := idleLoop(ctx, tm, RunParams{
			TicketID:    ticketID,
			SessionName: session,
			OnIdle:      func(context.Context, IdleEvent) IdleDecision { return IdleDecision{Action: IdlePrompt} },
		}, time.Now())
		done <- err
	}()

	tm.signal(t, channel)
	err := <-done
	require.ErrorContains(t, err, "exited too quickly")
	assert.True(t, tm.killed)
	assert.Empty(t, tm.sent())
}

// TestRunInteractiveIdleLoopAgainstTmux drives the loop through a real tmux
// window. The stand-in agent is `cat`, so everything the loop types lands in a
// file and the keystrokes can be checked as the agent would receive them.
func TestRunInteractiveIdleLoopAgainstTmux(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-loop-" + randomSuffix()
	dir := t.TempDir()
	typed := filepath.Join(dir, "typed.txt")
	channel := ChannelName(testSession, ticketID)
	compact := CompactChannelName(testSession, ticketID, "step1-0")
	t.Cleanup(func() { _ = KillWindow(testSession, ticketID) })

	signalAfter := func(d time.Duration, ch string) {
		go func() {
			time.Sleep(d)
			_ = exec.Command("tmux", "wait-for", "-S", ch).Run()
		}()
	}

	// The agent's first turn ends once the window is up.
	signalAfter(1500*time.Millisecond, channel)

	var calls int
	result, err := Run(context.Background(), RunParams{
		SessionName:    testSession,
		Binary:         "sh",
		Args:           []string{"-c", "cat > " + typed},
		Dir:            dir,
		TicketID:       ticketID,
		Timeout:        60 * time.Second,
		Interactive:    true,
		MinDuration:    -1,
		CompactTimeout: 10 * time.Second,
		CompactChannel: compact,
		OnIdle: func(_ context.Context, e IdleEvent) IdleDecision {
			calls++
			if calls > 1 {
				assert.NoError(t, e.CompactErr)
				return IdleDecision{Action: IdleFinish}
			}
			// The PostCompact hook fires, then the agent's next turn ends.
			signalAfter(500*time.Millisecond, compact)
			signalAfter(2*time.Second, channel)
			return IdleDecision{
				Action:              IdleCompact,
				CompactInstructions: "Preserve the goal.",
				Continuation:        "Continue with Phase 2: b.",
			}
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, 2, calls)

	data, err := os.ReadFile(typed)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	assert.Equal(t, []string{"/compact Preserve the goal.", "Continue with Phase 2: b.", "/exit"}, lines)
}

// TestIdleLoopDrainsALatchedCompactSignal covers the signal tmux holds when a
// compaction lands after its wait gave up. Handed to the next compaction's
// wait it would read as that one landing at once, and the continuation would be
// typed into an agent still summarising.
func TestIdleLoopDrainsALatchedCompactSignal(t *testing.T) {
	const (
		ticketID = "tst-idle-latch"
		session  = "kontora-idletest"
	)
	channel := ChannelName(session, ticketID)
	compactChannel := CompactChannelName(session, ticketID, "")
	tm := newStubTmux(channel, compactChannel)
	tm.latch(compactChannel)

	var mu sync.Mutex
	var events []IdleEvent
	onIdle := func(_ context.Context, e IdleEvent) IdleDecision {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
		if len(events) == 1 {
			return IdleDecision{Action: IdleCompact, Continuation: "Continue."}
		}
		return IdleDecision{Action: IdleFinish}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := idleLoop(ctx, tm, RunParams{
			TicketID:       ticketID,
			SessionName:    session,
			MinDuration:    -1,
			OnIdle:         onIdle,
			CompactTimeout: 200 * time.Millisecond,
		}, time.Now())
		done <- err
	}()

	tm.signal(t, channel)
	tm.signal(t, channel)
	require.NoError(t, <-done)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 2)
	require.Error(t, events[1].CompactErr, "the latched signal must not pass for this compaction landing")
	assert.ErrorContains(t, events[1].CompactErr, "did not finish within")
}

// TestIdleLoopWindowGoneAfterHook covers the window disappearing while a hook
// signal is in hand: the agent was killed or crashed, and kontora did not end
// the run, so it is not a success.
func TestIdleLoopWindowGoneAfterHook(t *testing.T) {
	const (
		ticketID = "tst-idle-hookgone"
		session  = "kontora-idletest"
	)
	channel := ChannelName(session, ticketID)
	tm := newStubTmux(channel)
	tm.latch(channel)
	tm.noWindow = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code, err := idleLoop(ctx, tm, RunParams{
		TicketID:    ticketID,
		SessionName: session,
		MinDuration: -1,
		OnIdle: func(context.Context, IdleEvent) IdleDecision {
			t.Error("no decision is asked for once the window is gone")
			return IdleDecision{Action: IdleFinish}
		},
	}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, code)
	assert.Empty(t, tm.sent())
}
