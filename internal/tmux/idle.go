package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/worksonmyai/kontora/internal/process"
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
	InspectWindow() (bool, error)
	KillWindow() error
	WaitFor(ctx context.Context, channel string) error
	// Signal unblocks anyone waiting on channel, including this loop's own
	// wait-for goroutine on a path that is already returning.
	Signal(channel string)
	WaitWindowExit(timeout time.Duration) error
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
func (w windowTmux) InspectWindow() (bool, error) { return InspectWindow(w.session, w.ticketID) }
func (w windowTmux) KillWindow() error            { return KillWindow(w.session, w.ticketID) }

func (w windowTmux) WaitFor(ctx context.Context, channel string) error {
	return exec.CommandContext(ctx, "tmux", "wait-for", channel).Run()
}

func (w windowTmux) WaitWindowExit(timeout time.Duration) error {
	return waitForWindowExit(w.session, w.ticketID, timeout)
}

func (w windowTmux) Signal(channel string) {
	_ = exec.Command("tmux", "wait-for", "-S", channel).Run()
}

// idleLoop is the wake/decide/act cycle of an interactive run. It blocks until
// the agent signals it is idle, asks p.OnIdle what to do, and either types what
// the decision asks for and waits again, or sends /exit and returns.
func idleLoop(ctx context.Context, tm idleTmux, p RunParams, startedAt time.Time) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sess := p.session()
	channel := ChannelName(sess, p.WindowName())
	compactChannel := p.compactChannel()

	windowGone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				present, err := tm.InspectWindow()
				if err != nil || !present {
					windowGone <- err
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
			if ctx.Err() != nil {
				return -1, errors.Join(ctx.Err(), stopWindow(tm))
			}
			if err != nil {
				return -1, fmt.Errorf("tmux wait-for: %w", err)
			}

			// A first signal that arrives almost immediately means the agent
			// crashed on startup before doing any real work. Later signals are
			// turns ending, however fast they came.
			if first {
				first = false
				if err := checkMinDuration(p, startedAt); err != nil {
					return 1, errors.Join(err, stopWindow(tm))
				}
			}

			// The window is gone while the run is still ours to end: the agent
			// was killed or crashed after signalling. Reporting that as a
			// success would advance the pipeline over work that stopped
			// halfway, and a signal left latched by an earlier turn makes it
			// race the windowGone poller below for who reports it.
			present, err := tm.InspectWindow()
			if err != nil {
				return -1, errors.Join(process.ErrTerminationUncertain, err)
			}
			if !present {
				return 1, nil
			}

			decision := IdleDecision{Action: IdleFinish}
			if !p.terminalReported() && p.OnIdle != nil {
				decision = p.OnIdle(ctx, IdleEvent{CompactErr: compactErr})
			}
			compactErr = nil
			if ctx.Err() != nil {
				return -1, errors.Join(ctx.Err(), stopWindow(tm))
			}
			if p.terminalReported() {
				decision.Action = IdleFinish
			}
			if decision.Action == IdleCompact {
				compactErr = compact(ctx, tm, compactChannel, decision.CompactInstructions, p.compactTimeout())
			}
			if ctx.Err() != nil {
				return -1, errors.Join(ctx.Err(), stopWindow(tm))
			}
			// A terminal result may have arrived while the decision or compaction
			// was in flight. Recheck at the continuation boundary, then use the
			// normal /exit flow rather than killing the reporting agent.
			if p.terminalReported() {
				decision.Action = IdleFinish
			}
			if decision.Action == IdleFinish {
				return finishIdle(ctx, tm, p.RequireTermination)
			}

			if err := tm.SendLiteral(decision.Continuation); err != nil {
				return -1, fmt.Errorf("sending continuation prompt: %w", err)
			}
			arm()

		case inspectErr := <-windowGone:
			if ctx.Err() != nil {
				return -1, errors.Join(ctx.Err(), stopWindow(tm))
			}
			if inspectErr != nil {
				return -1, errors.Join(process.ErrTerminationUncertain, inspectErr)
			}
			// The agent was exited by hand: the window is gone but wait-for was
			// never signaled. Signal it ourselves to unblock the goroutine, then
			// report the run as failed.
			tm.Signal(channel)
			return 1, nil

		case <-ctx.Done():
			err := stopWindow(tm)
			tm.Signal(channel)
			return -1, errors.Join(ctx.Err(), err)
		}
	}
}

func finishIdle(ctx context.Context, tm idleTmux, requireTermination bool) (int, error) {
	if err := tm.SendKeys("/exit"); err != nil {
		if ctx.Err() != nil {
			return -1, errors.Join(ctx.Err(), err, stopWindow(tm))
		}
		return -1, fmt.Errorf("sending /exit after hook: %w", err)
	}
	// Allow five seconds after /exit. Only unrelated runs may count a
	// timeout as success without termination evidence.
	waitErr := tm.WaitWindowExit(5 * time.Second)
	if ctx.Err() != nil {
		return -1, errors.Join(ctx.Err(), stopWindow(tm))
	}
	if waitErr != nil && (requireTermination || !errors.Is(waitErr, errWindowExitTimeout)) {
		return -1, errors.Join(process.ErrTerminationUncertain, waitErr)
	}
	return 0, nil
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
