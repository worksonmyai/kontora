package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/process"
)

const (
	pollInterval      = 100 * time.Millisecond
	minInteractiveDur = 2 * time.Second
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
	OnReady     func()            // called after the tmux window is created
	MinDuration time.Duration     // interactive only: treat exits faster than this as crashes; 0 = use default (2s), -1 = disable
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

// runInteractive executes an interactive agent (e.g. Claude) that signals
// completion via a tmux wait-for channel. After the signal, /exit is sent
// to close the window.
func runInteractive(ctx context.Context, p RunParams) (process.Result, error) {
	cmd := append([]string{p.Binary}, p.Args...)
	channel := ChannelName(p.TicketID)
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

	scriptPath, err := writeInteractiveWrapper(cmd, gateFile, p.Env)
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

	// Block until either (a) the Notification hook fires tmux wait-for -S,
	// or (b) the tmux window disappears (user manually exited the agent).
	hookFired := make(chan error, 1)
	go func() {
		waitCmd := exec.CommandContext(ctx, "tmux", "wait-for", channel)
		hookFired <- waitCmd.Run()
	}()

	windowGone := make(chan struct{}, 1)
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !HasWindow(sess, p.TicketID) {
					windowGone <- struct{}{}
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	select {
	case err := <-hookFired:
		if err != nil {
			if ctx.Err() != nil {
				_ = KillWindow(sess, p.TicketID)
				return process.Result{
					ExitCode:  -1,
					StartedAt: startedAt,
					ExitedAt:  time.Now(),
				}, ctx.Err()
			}
			return process.Result{
				ExitCode:  -1,
				StartedAt: startedAt,
				ExitedAt:  time.Now(),
			}, fmt.Errorf("tmux wait-for: %w", err)
		}

		// If the hook fired almost immediately, the agent likely crashed on
		// startup before doing any real work.
		minDur := p.MinDuration
		if minDur == 0 {
			minDur = minInteractiveDur
		}
		if minDur > 0 {
			if dur := time.Since(startedAt); dur < minDur {
				_ = KillWindow(sess, p.TicketID)
				return process.Result{
					ExitCode:  1,
					StartedAt: startedAt,
					ExitedAt:  time.Now(),
				}, fmt.Errorf("interactive agent exited too quickly (%s < %s)", dur.Truncate(time.Millisecond), minDur)
			}
		}

		// Hook fired — tell Claude to exit and wait for the window to close.
		// If the window is already gone (agent exited on its own), skip /exit.
		if HasWindow(sess, p.TicketID) {
			if err := SendKeys(sess, p.TicketID, "/exit"); err != nil {
				return process.Result{
					ExitCode:  -1,
					StartedAt: startedAt,
					ExitedAt:  time.Now(),
				}, fmt.Errorf("sending /exit after hook: %w", err)
			}
			waitForWindowExit(sess, p.TicketID, 5*time.Second)
		}

		return process.Result{
			ExitCode:  0,
			StartedAt: startedAt,
			ExitedAt:  time.Now(),
		}, nil

	case <-windowGone:
		// User manually exited the agent — the tmux window is gone but
		// wait-for was never signaled. Signal the channel ourselves to
		// unblock the wait-for goroutine, then treat as exit code 1.
		_ = exec.Command("tmux", "wait-for", "-S", channel).Run()
		return process.Result{
			ExitCode:  1,
			StartedAt: startedAt,
			ExitedAt:  time.Now(),
		}, nil

	case <-ctx.Done():
		_ = KillWindow(sess, p.TicketID)
		// Unblock the wait-for goroutine.
		_ = exec.Command("tmux", "wait-for", "-S", channel).Run()
		return process.Result{
			ExitCode:  -1,
			StartedAt: startedAt,
			ExitedAt:  time.Now(),
		}, ctx.Err()
	}
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

	scriptPath, err := writeStandardWrapper(cmd, exitPath, gateFile, p.Env)
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
