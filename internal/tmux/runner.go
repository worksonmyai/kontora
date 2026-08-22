package tmux

import (
	"context"
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
	Binary      string
	Args        []string
	Dir         string
	Timeout     time.Duration
	TicketID    string
	SessionName string // tmux session name; defaults to DefaultSessionName if empty
	LogFile     string
	Interactive bool              // when true, use tmux wait-for + /exit flow for hook-based completion
	SessionID   string            // Claude session ID; daemon uses this for session JSONL materialization
	Env         map[string]string // environment variables to export in the wrapper script
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
	// CompactTimeout bounds the wait for a compaction OnIdle asked for.
	// 0 = DefaultCompactTimeout.
	CompactTimeout time.Duration
	// CompactChannel is the tmux wait-for channel the agent's PostCompact hook
	// signals. The caller sets the same name it wrote into the hook, and scopes
	// it to this run so a signal latched by an earlier one is not read as this
	// compaction landing. Empty = the ticket's default channel.
	CompactChannel string
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
	return CompactChannelName(p.session(), p.TicketID, "")
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
	sess := p.session()

	usePipePaneLogging := p.LogFile != ""

	var gateFile string
	if usePipePaneLogging {
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

	if err := newWindow(sess, p.TicketID, p.Dir, scriptPath); err != nil {
		os.Remove(scriptPath)
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

	if usePipePaneLogging {
		if err := PipePaneTo(sess, p.TicketID, p.LogFile); err != nil {
			_ = KillWindow(sess, p.TicketID)
			return process.Result{ExitCode: -1, StartedAt: startedAt, ExitedAt: time.Now()}, err
		}
		if err := os.WriteFile(gateFile, []byte("go"), 0o644); err != nil {
			_ = KillWindow(sess, p.TicketID)
			return process.Result{ExitCode: -1, StartedAt: startedAt, ExitedAt: time.Now()}, fmt.Errorf("writing gate file: %w", err)
		}
	}

	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	code, err := idleLoop(ctx, windowTmux{session: sess, ticketID: p.TicketID}, p, startedAt)
	return process.Result{
		ExitCode:  code,
		StartedAt: startedAt,
		ExitedAt:  time.Now(),
	}, err
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
	sess := p.session()

	var gateFile string
	if p.LogFile != "" {
		g, err := os.CreateTemp("", "kontora-gate-*")
		if err != nil {
			return process.Result{}, fmt.Errorf("creating gate file: %w", err)
		}
		gateFile = g.Name()
		g.Close()
		os.Remove(gateFile) // Script waits for this file to appear.
	}

	scriptPath, err := writeStandardWrapper(cmd, exitPath, gateFile, p.Env, p.PathDirs)
	if err != nil {
		return process.Result{}, fmt.Errorf("writing wrapper: %w", err)
	}

	startedAt := time.Now()

	if err := newWindow(sess, p.TicketID, p.Dir, scriptPath); err != nil {
		os.Remove(scriptPath)
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

	if p.LogFile != "" {
		if err := PipePaneTo(sess, p.TicketID, p.LogFile); err != nil {
			return process.Result{}, err
		}
		if err := os.WriteFile(gateFile, []byte("go"), 0o644); err != nil {
			return process.Result{}, fmt.Errorf("writing gate file: %w", err)
		}
	}

	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			code, done := readExitFile(exitPath)
			if done {
				return process.Result{
					ExitCode:  code,
					StartedAt: startedAt,
					ExitedAt:  time.Now(),
				}, nil
			}
			if !HasWindow(sess, p.TicketID) {
				for range 5 {
					time.Sleep(pollInterval)
					if code, ok := readExitFile(exitPath); ok {
						return process.Result{
							ExitCode:  code,
							StartedAt: startedAt,
							ExitedAt:  time.Now(),
						}, nil
					}
				}
				return process.Result{
					ExitCode:  -1,
					StartedAt: startedAt,
					ExitedAt:  time.Now(),
				}, fmt.Errorf("tmux window for ticket %q vanished without writing exit code", p.TicketID)
			}
		case <-ctx.Done():
			_ = KillWindow(sess, p.TicketID)
			return process.Result{
				ExitCode:  -1,
				StartedAt: startedAt,
				ExitedAt:  time.Now(),
			}, ctx.Err()
		}
	}
}

// waitForWindowExit polls HasWindow until the window is gone or timeout.
func waitForWindowExit(sessionName, taskID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !HasWindow(sessionName, taskID) {
			return
		}
		time.Sleep(pollInterval)
	}
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
