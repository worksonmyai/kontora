package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// IdleAction is what the daemon wants done when an interactive agent goes idle.
type IdleAction int

const (
	// IdleFinish ends the run: the runner sends /exit and waits for the window
	// to close. It is what every run does without an OnIdle callback.
	IdleFinish IdleAction = iota
	// IdlePrompt types the decision's Continuation and waits for the agent to
	// go idle again.
	IdlePrompt
	// IdleCompact types /compact with the decision's CompactInstructions, waits
	// for the compaction to land, then types the Continuation.
	IdleCompact
)

// IdleEvent is what the runner knows when it asks for a decision.
type IdleEvent struct {
	// CompactErr is set when the compaction the previous decision asked for did
	// not land. The run is unharmed: the agent still holds its full context, so
	// the decision maker records the failure and carries on.
	CompactErr error
}

// IdleDecision is the answer OnIdle gives.
type IdleDecision struct {
	Action IdleAction
	// CompactInstructions is appended to /compact, telling the agent what the
	// summary must preserve. Only read for IdleCompact.
	CompactInstructions string
	// Continuation is the prompt typed after a compaction, or on its own for
	// IdlePrompt. Read for both.
	Continuation string
}

// DefaultCompactTimeout bounds how long the runner waits for a compaction it
// asked for. Compaction summarises the whole conversation, so it is slow; past
// this the run continues uncompacted rather than stalling.
const DefaultCompactTimeout = 5 * time.Minute

// compactDrainTimeout is how long a drain of the compact channel waits before
// deciding the channel held no latched signal. tmux answers a latched channel
// at once, so this only has to cover the round trip to the server.
const compactDrainTimeout = 250 * time.Millisecond

// idleTmux is the tmux surface the idle loop drives. Tests substitute a stub
// for it so the loop's decisions can be exercised without a tmux server.
type idleTmux interface {
	SendKeys(keys string) error
	SendLiteral(text string) error
	HasWindow() bool
	KillWindow() error
	WaitFor(ctx context.Context, channel string) error
	// Signal unblocks anyone waiting on channel, including this loop's own
	// wait-for goroutine on a path that is already returning.
	Signal(channel string)
	WaitWindowExit(timeout time.Duration)
}

// windowTmux drives a real ticket window.
type windowTmux struct {
	session  string
	ticketID string
}

func (w windowTmux) SendKeys(keys string) error { return SendKeys(w.session, w.ticketID, keys) }
func (w windowTmux) SendLiteral(text string) error {
	return SendKeysLiteral(w.session, w.ticketID, text)
}
func (w windowTmux) HasWindow() bool   { return HasWindow(w.session, w.ticketID) }
func (w windowTmux) KillWindow() error { return KillWindow(w.session, w.ticketID) }

func (w windowTmux) WaitFor(ctx context.Context, channel string) error {
	return exec.CommandContext(ctx, "tmux", "wait-for", channel).Run()
}

func (w windowTmux) WaitWindowExit(timeout time.Duration) {
	waitForWindowExit(w.session, w.ticketID, timeout)
}

func (w windowTmux) Signal(channel string) {
	_ = exec.Command("tmux", "wait-for", "-S", channel).Run()
}

// idleLoop is the wake/decide/act cycle of an interactive run. It blocks until
// the agent signals it is idle, asks p.OnIdle what to do, and either types what
// the decision asks for and waits again, or sends /exit and returns.
func idleLoop(ctx context.Context, tm idleTmux, p RunParams, startedAt time.Time) (int, error) {
	sess := p.session()
	channel := ChannelName(sess, p.TicketID)
	compactChannel := p.compactChannel()

	windowGone := make(chan struct{}, 1)
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !tm.HasWindow() {
					windowGone <- struct{}{}
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	hookFired := make(chan error, 1)
	arm := func() {
		go func() { hookFired <- tm.WaitFor(ctx, channel) }()
	}
	arm()

	// Carried into the next decision so the daemon learns that the compaction it
	// asked for never landed.
	var compactErr error
	first := true

	for {
		select {
		case err := <-hookFired:
			if err != nil {
				if ctx.Err() != nil {
					_ = tm.KillWindow()
					return -1, ctx.Err()
				}
				return -1, fmt.Errorf("tmux wait-for: %w", err)
			}

			// A first signal that arrives almost immediately means the agent
			// crashed on startup before doing any real work. Later signals are
			// turns ending, however fast they came.
			if first {
				first = false
				if err := checkMinDuration(p, startedAt); err != nil {
					_ = tm.KillWindow()
					return 1, err
				}
			}

			// The window is gone while the run is still ours to end: the agent
			// was killed or crashed after signalling. Reporting that as a
			// success would advance the pipeline over work that stopped
			// halfway, and a signal left latched by an earlier turn makes it
			// race the windowGone poller below for who reports it.
			if !tm.HasWindow() {
				return 1, nil
			}

			decision := IdleDecision{Action: IdleFinish}
			if p.OnIdle != nil {
				decision = p.OnIdle(ctx, IdleEvent{CompactErr: compactErr})
			}
			compactErr = nil

			if decision.Action == IdleFinish {
				if err := tm.SendKeys("/exit"); err != nil {
					return -1, fmt.Errorf("sending /exit after hook: %w", err)
				}
				tm.WaitWindowExit(5 * time.Second)
				return 0, nil
			}

			if decision.Action == IdleCompact {
				compactErr = compact(ctx, tm, compactChannel, decision.CompactInstructions, p.compactTimeout())
			}
			if err := tm.SendLiteral(decision.Continuation); err != nil {
				return -1, fmt.Errorf("sending continuation prompt: %w", err)
			}
			arm()

		case <-windowGone:
			// The agent was exited by hand: the window is gone but wait-for was
			// never signaled. Signal it ourselves to unblock the goroutine, then
			// report the run as failed.
			tm.Signal(channel)
			return 1, nil

		case <-ctx.Done():
			_ = tm.KillWindow()
			tm.Signal(channel)
			return -1, ctx.Err()
		}
	}
}

// compact types /compact and waits for the agent's PostCompact hook to signal.
// A compaction that errors or never lands is reported, not fatal: the caller
// continues the run on the uncompacted conversation.
func compact(ctx context.Context, tm idleTmux, channel, instructions string, timeout time.Duration) error {
	drainCompact(ctx, tm, channel)

	text := "/compact"
	if instructions != "" {
		text += " " + instructions
	}
	if err := tm.SendLiteral(text); err != nil {
		return fmt.Errorf("sending /compact: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := tm.WaitFor(waitCtx, channel); err != nil {
		if ctx.Err() == nil && waitCtx.Err() != nil {
			return fmt.Errorf("compaction did not finish within %s", timeout)
		}
		return fmt.Errorf("waiting for compaction: %w", err)
	}
	return nil
}

// checkMinDuration reports an agent that signaled before it could have done any
// work. A MinDuration of -1 disables the check.
func checkMinDuration(p RunParams, startedAt time.Time) error {
	minDur := p.MinDuration
	if minDur == 0 {
		minDur = MinInteractiveDuration
	}
	if minDur <= 0 {
		return nil
	}
	if dur := time.Since(startedAt); dur < minDur {
		return fmt.Errorf("interactive agent exited too quickly (%s < %s)", dur.Truncate(time.Millisecond), minDur)
	}
	return nil
}

// drainCompact clears a signal left latched on the compact channel. tmux holds
// a wait-for -S sent while nobody waits and hands it to the next waiter, so a
// compaction that landed after its wait timed out, an auto-compaction, or a
// hand-typed /compact would otherwise make this compaction's wait return at
// once. Anything latched now is by definition older than the /compact about to
// be typed, so consuming it loses nothing.
func drainCompact(ctx context.Context, tm idleTmux, channel string) {
	drainCtx, cancel := context.WithTimeout(ctx, compactDrainTimeout)
	defer cancel()
	_ = tm.WaitFor(drainCtx, channel)
}
