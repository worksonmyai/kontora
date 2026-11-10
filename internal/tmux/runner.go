package tmux

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/process"
)

const (
	pollInterval = 100 * time.Millisecond
	// MinInteractiveDuration is how long an interactive agent must stay alive
	// before its exit counts as a real one. A faster exit is reported as an
	// error, because the agent crashed on startup instead of doing any work.
	MinInteractiveDuration = 2 * time.Second
)

// RunParams contains parameters for running a command inside a tmux window.
type RunParams struct {
	Binary   string
	Args     []string
	Dir      string
	Timeout  time.Duration
	TicketID string
	// AttemptID scopes the window and default hook channels to one execution.
	// Empty preserves the ticket's existing window and channel names.
	AttemptID   string
	SessionName string // tmux session name; defaults to DefaultSessionName if empty
	LogFile     string
	// RequireTermination rejects completion unless the tmux window is confirmed
	// gone. It also disables the noninteractive failure debug shell. Window
	// absence does not prove that detached descendants stopped.
	RequireTermination bool
	// StableWindowIdentity gates launch until identity has been captured. Either
	// callback/expectation also enables it. Ordinary tickets keep name targets.
	StableWindowIdentity bool
	OnWindowCreated      func(WindowIdentity) error
	ExpectedIdentity     *WindowIdentity
	Interactive          bool              // when true, use tmux wait-for + /exit flow for hook-based completion
	SessionID            string            // Claude session ID; daemon uses this for session JSONL materialization
	Env                  map[string]string // environment variables to export in the wrapper script
	// PathDirs are appended to $PATH in the wrapper script. tmux starts the
	// wrapper through default-shell, so the script runs after that shell built
	// its own PATH: these are added to it, never substituted for it.
	PathDirs    []string
	OnReady     func()        // called after the tmux window is created
	MinDuration time.Duration // interactive only: treat exits faster than this as crashes; 0 = use default (2s), -1 = disable
	// OnIdle is asked what to do each time an interactive agent signals it is
	// idle. Nil finishes the run on the first signal. It is called with the
	// run's context, and must return once that context is cancelled: the loop
	// is inside the call and cannot act on a shutdown until it does.
	OnIdle func(context.Context, IdleEvent) IdleDecision
	// TerminalReported suppresses checkpoint decisions and continuations once
	// a terminal result exists. It is checked only at idle/continuation
	// boundaries, not polled to kill a running agent. Nil preserves behavior.
	TerminalReported func() bool
	// CompactTimeout bounds the wait for a compaction OnIdle asked for.
	// 0 = DefaultCompactTimeout.
	CompactTimeout time.Duration
	// CompactChannel is the tmux wait-for channel the agent's PostCompact hook
	// signals. The caller sets the same name it wrote into the hook, and scopes
	// it to this run so a signal latched by an earlier one is not read as this
	// compaction landing. Empty = the ticket's default channel.
	CompactChannel string
}

// WindowName returns the execution's window name. Callers must use the same
// name in hook channels and when attaching to or inspecting an attempt.
func (p RunParams) WindowName() string {
	if p.AttemptID == "" {
		return p.TicketID
	}
	normalize := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, s)
	}
	// The digest separates identities whose normalized names are the same.
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%s", len(p.TicketID), p.TicketID, p.AttemptID)))
	return normalize(p.TicketID) + "--" + normalize(p.AttemptID) + fmt.Sprintf("-%x", digest[:6])
}

func (p RunParams) stableIdentity() bool {
	return p.StableWindowIdentity || p.OnWindowCreated != nil || p.ExpectedIdentity != nil
}

func createRunWindow(p RunParams, script string) (idleTmux, error) {
	if !p.stableIdentity() {
		tm := windowTmux{session: p.session(), ticketID: p.WindowName()}
		return tm, newWindow(p.session(), p.WindowName(), p.Dir, script)
	}
	id, err := newWindowIdentity(p.session(), p.WindowName(), p.Dir, script)
	if err != nil {
		return nil, err
	}
	tm := identityTmux{id: id}
	if p.ExpectedIdentity != nil && *p.ExpectedIdentity != id {
		return tm, fmt.Errorf("created tmux window does not match expected identity")
	}
	if p.OnWindowCreated != nil {
		if err := p.OnWindowCreated(id); err != nil {
			return tm, fmt.Errorf("persisting tmux window identity: %w", err)
		}
	}
	return tm, nil
}

func pipeRunPane(tm idleTmux, p RunParams) error {
	if identity, ok := tm.(identityTmux); ok {
		return identity.pipePane(p.LogFile)
	}
	return PipePaneTo(p.session(), p.WindowName(), p.LogFile)
}

func (p RunParams) terminalReported() bool {
	return p.TerminalReported != nil && p.TerminalReported()
}

func (p RunParams) compactTimeout() time.Duration {
	if p.CompactTimeout > 0 {
		return p.CompactTimeout
	}
	return DefaultCompactTimeout
}

func (p RunParams) compactChannel() string {
	if p.CompactChannel != "" {
		return p.CompactChannel
	}
	return CompactChannelName(p.session(), p.WindowName(), "")
}

func (p RunParams) session() string {
	if p.SessionName != "" {
		return p.SessionName
	}
	return DefaultSessionName
}

// Run executes a command inside a tmux window and waits for it to complete.
// Interactive mode blocks on tmux wait-for (signaled by a Notification hook)
// then sends /exit. Standard mode polls an exit file. On timeout or context
// cancellation the window is killed.
func Run(ctx context.Context, p RunParams) (process.Result, error) {
	if err := ctx.Err(); err != nil {
		return process.Result{ExitCode: -1}, err
	}
	if p.Interactive {
		return runInteractive(ctx, p)
	}
	return runStandard(ctx, p)
}

// runInteractive executes an interactive agent (e.g. Claude) that signals it is
// idle over a tmux wait-for channel. Once the window is up, idleLoop decides
// what each signal means: without an OnIdle callback the first one ends the run
// with /exit.
func runInteractive(ctx context.Context, p RunParams) (process.Result, error) {
	cmd := append([]string{p.Binary}, p.Args...)
	usePipePaneLogging := p.LogFile != ""

	var gateFile string
	if usePipePaneLogging || p.stableIdentity() {
		g, err := os.CreateTemp("", "kontora-gate-*")
		if err != nil {
			return process.Result{}, fmt.Errorf("creating gate file: %w", err)
		}
		gateFile = g.Name()
		g.Close()
		os.Remove(gateFile) // Script waits for this file to appear.
	}

	scriptPath, err := writeInteractiveWrapper(cmd, gateFile, p.Env, p.PathDirs)
	if err != nil {
		return process.Result{}, fmt.Errorf("writing wrapper: %w", err)
	}

	startedAt := time.Now()

	tm, err := createRunWindow(p, scriptPath)
	if err != nil {
		os.Remove(scriptPath)
		if tm != nil && p.stableIdentity() {
			return windowResult(tm, -1, startedAt, errors.Join(err, stopWindow(tm)), p.RequireTermination)
		}
		return process.Result{ExitCode: -1}, err
	}
	if p.OnReady != nil {
		p.OnReady()
	}

	defer func() {
		os.Remove(scriptPath)
		if gateFile != "" {
			os.Remove(gateFile)
		}
	}()

	if p.stableIdentity() && ctx.Err() != nil {
		return windowResult(tm, -1, startedAt, errors.Join(ctx.Err(), stopWindow(tm)), p.RequireTermination)
	}
	if usePipePaneLogging {
		if err := pipeRunPane(tm, p); err != nil {
			return windowResult(tm, -1, startedAt, errors.Join(err, stopWindow(tm)), p.RequireTermination)
		}
	}
	if gateFile != "" {
		if err := os.WriteFile(gateFile, []byte("go"), 0o644); err != nil {
			return windowResult(tm, -1, startedAt, errors.Join(fmt.Errorf("writing gate file: %w", err), stopWindow(tm)), p.RequireTermination)
		}
	}

	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	code, err := idleLoop(ctx, tm, p, startedAt)
	return windowResult(tm, code, startedAt, err, p.RequireTermination)
}

// runStandard executes a non-interactive command in tmux, polling an exit
// file for the process exit code.
func runStandard(ctx context.Context, p RunParams) (process.Result, error) {
	exitFile, err := os.CreateTemp("", "kontora-exit-*")
	if err != nil {
		return process.Result{}, fmt.Errorf("creating exit file: %w", err)
	}
	exitPath := exitFile.Name()
	exitFile.Close()
	os.Remove(exitPath) // Remove so we can detect when the script writes it.

	cmd := append([]string{p.Binary}, p.Args...)
	var gateFile string
	if p.LogFile != "" || p.stableIdentity() {
		g, err := os.CreateTemp("", "kontora-gate-*")
		if err != nil {
			return process.Result{}, fmt.Errorf("creating gate file: %w", err)
		}
		gateFile = g.Name()
		g.Close()
		os.Remove(gateFile) // Script waits for this file to appear.
	}

	scriptPath, err := writeStandardWrapper(cmd, exitPath, gateFile, p.Env, p.PathDirs, p.RequireTermination)
	if err != nil {
		return process.Result{}, fmt.Errorf("writing wrapper: %w", err)
	}

	startedAt := time.Now()

	tm, err := createRunWindow(p, scriptPath)
	if err != nil {
		os.Remove(scriptPath)
		if tm != nil && p.stableIdentity() {
			return windowResult(tm, -1, startedAt, errors.Join(err, stopWindow(tm)), p.RequireTermination)
		}
		return process.Result{ExitCode: -1}, err
	}
	if p.OnReady != nil {
		p.OnReady()
	}

	defer func() {
		os.Remove(exitPath)
		os.Remove(scriptPath)
		if gateFile != "" {
			os.Remove(gateFile)
		}
	}()

	if p.stableIdentity() && ctx.Err() != nil {
		return windowResult(tm, -1, startedAt, errors.Join(ctx.Err(), stopWindow(tm)), p.RequireTermination)
	}
	if p.LogFile != "" {
		if err := pipeRunPane(tm, p); err != nil {
			return windowResult(tm, -1, startedAt, errors.Join(err, stopWindow(tm)), p.RequireTermination)
		}
	}
	if gateFile != "" {
		if err := os.WriteFile(gateFile, []byte("go"), 0o644); err != nil {
			return windowResult(tm, -1, startedAt, errors.Join(fmt.Errorf("writing gate file: %w", err), stopWindow(tm)), p.RequireTermination)
		}
	}

	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	finish := func(code int) (process.Result, error) {
		var err error
		if p.RequireTermination {
			// The wrapper writes its exit file before it exits. Seeing that file
			// alone is not termination evidence, even for a successful command.
			if waitErr := tm.WaitWindowExit(5 * time.Second); waitErr != nil {
				err = errors.Join(process.ErrTerminationUncertain, waitErr)
			}
			if ctx.Err() != nil {
				return windowResult(tm, -1, startedAt, errors.Join(ctx.Err(), err, stopWindow(tm)), p.RequireTermination)
			}
		}
		return windowResult(tm, code, startedAt, err, p.RequireTermination)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			code, done := readExitFile(exitPath)
			if done {
				return finish(code)
			}
			present, err := tm.InspectWindow()
			if err != nil {
				return windowResult(tm, -1, startedAt, err, p.RequireTermination)
			}
			if !present {
				for range 5 {
					time.Sleep(pollInterval)
					if code, ok := readExitFile(exitPath); ok {
						return finish(code)
					}
				}
				return windowResult(tm, -1, startedAt, fmt.Errorf("tmux window for ticket %q vanished without writing exit code", p.TicketID), p.RequireTermination)
			}
		case <-ctx.Done():
			return windowResult(tm, -1, startedAt, errors.Join(ctx.Err(), stopWindow(tm)), p.RequireTermination)
		}
	}
}

var errWindowExitTimeout = errors.New("tmux window exit wait timed out")

// waitForWindowExit only succeeds after an exact query confirms absence.
func waitForWindowExit(sessionName, windowName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		present, err := InspectWindow(sessionName, windowName)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: window %q still exists after %s", errWindowExitTimeout, windowName, timeout)
		}
		time.Sleep(pollInterval)
	}
}

func stopWindow(tm idleTmux) error {
	killErr := tm.KillWindow()
	waitErr := tm.WaitWindowExit(5 * time.Second)
	if waitErr != nil {
		waitErr = errors.Join(process.ErrTerminationUncertain, waitErr)
	}
	return errors.Join(killErr, waitErr)
}

func windowResult(tm idleTmux, code int, startedAt time.Time, err error, requireTermination bool) (process.Result, error) {
	result := process.Result{ExitCode: code, StartedAt: startedAt, ExitedAt: time.Now()}
	present, inspectErr := tm.InspectWindow()
	if inspectErr != nil {
		err = errors.Join(err, process.ErrTerminationUncertain, inspectErr)
	} else if !present {
		result.Termination = process.TerminationWindowGone
	} else if requireTermination {
		err = errors.Join(err, process.ErrTerminationUncertain)
	}
	return result, err
}

func readExitFile(path string) (code int, done bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, false
	}
	code, err = strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return code, true
}
