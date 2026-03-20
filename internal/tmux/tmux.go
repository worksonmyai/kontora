package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// DefaultSessionName is the default tmux session name used by the daemon.
	DefaultSessionName = "kontora"
	channelPrefix      = "kontora-"
)

// ChannelName returns the tmux wait-for channel name for a ticket.
func ChannelName(ticketID string) string {
	return channelPrefix + ticketID
}

// WindowTarget returns the tmux target for a ticket's window (=session:window).
// Uses = prefix for exact session name matching.
func WindowTarget(sessionName, ticketID string) string {
	return "=" + sessionName + ":" + ticketID
}

// SendKeys sends keystrokes to a ticket's tmux window.
func SendKeys(sessionName, ticketID, keys string) error {
	out, err := exec.Command("tmux", "send-keys", "-t", WindowTarget(sessionName, ticketID), keys, "Enter").CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux send-keys: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// KillWindow destroys a ticket's tmux window.
func KillWindow(sessionName, ticketID string) error {
	out, err := exec.Command("tmux", "kill-window", "-t", WindowTarget(sessionName, ticketID)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux kill-window: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// CapturePaneText captures the full scrollback text from a ticket's tmux pane.
func CapturePaneText(sessionName, ticketID string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-t", WindowTarget(sessionName, ticketID), "-p", "-S", "-").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// HasWindow returns true if a tmux window for the given ticket exists.
func HasWindow(sessionName, ticketID string) bool {
	return exec.Command("tmux", "list-panes", "-t", WindowTarget(sessionName, ticketID)).Run() == nil
}

// ListWindows returns names of all windows in the given tmux session.
func ListWindows(sessionName string) ([]string, error) {
	out, err := exec.Command("tmux", "list-windows", "-t", "="+sessionName, "-F", "#{window_name}").CombinedOutput()
	if err != nil {
		if isSessionMissing(string(out)) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-windows: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var windows []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			windows = append(windows, line)
		}
	}
	return windows, nil
}

// PipePaneTo configures a ticket's tmux window to tee PTY output to logPath.
func PipePaneTo(sessionName, ticketID, logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	pipeCmd := fmt.Sprintf("cat >> %s", shellQuote(logPath))
	out, err := exec.Command("tmux", "pipe-pane", "-t", WindowTarget(sessionName, ticketID), pipeCmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux pipe-pane: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// newWindow creates a tmux window for a ticket, lazily creating the session
// if it doesn't exist yet.
func newWindow(sessionName, ticketID, dir, script string) error {
	target := "=" + sessionName + ":"

	// Common path: add a window to the existing session.
	out, err := exec.Command("tmux", "new-window", "-t", target, "-n", ticketID, "-c", dir, "--", script).CombinedOutput()
	if err == nil {
		return nil
	}

	// Fall through only if the session doesn't exist.
	if !isSessionMissing(string(out)) {
		return fmt.Errorf("tmux new-window: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Session doesn't exist — create it with this as the first window.
	// Ensure LANG is set so the tmux server starts with UTF-8 support.
	createCmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "-n", ticketID, "-c", dir, "--", script)
	createCmd.Env = append(os.Environ(), "LANG=en_US.UTF-8")
	out, err = createCmd.CombinedOutput()
	if err == nil {
		return nil
	}

	// Race: another goroutine created the session between our attempts.
	if !strings.Contains(string(out), "duplicate session") {
		return fmt.Errorf("tmux new-session: %s: %w", strings.TrimSpace(string(out)), err)
	}

	out, err = exec.Command("tmux", "new-window", "-t", target, "-n", ticketID, "-c", dir, "--", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux new-window: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// isSessionMissing returns true if the tmux error output indicates the
// session doesn't exist or the server isn't running.
func isSessionMissing(output string) bool {
	return strings.Contains(output, "session not found") ||
		strings.Contains(output, "can't find session") ||
		strings.Contains(output, "no server running") ||
		strings.Contains(output, "no current target") ||
		strings.Contains(output, "error connecting")
}

// writeInteractiveWrapper creates a temporary executable script that gates
// on a file (when gateFile is non-empty) then exec's cmd, replacing the
// shell process. Used for interactive agents (e.g. Claude) where completion
// is signaled via hooks rather than exit code capture.
func writeInteractiveWrapper(cmd []string, gateFile string, env map[string]string) (string, error) {
	f, err := os.CreateTemp("", "kontora-wrapper-*.sh")
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("unset CLAUDECODE\n")
	writeEnvExports(&b, env)
	if gateFile != "" {
		fmt.Fprintf(&b, "while [ ! -f %s ]; do sleep 0.05; done\n", shellQuote(gateFile))
		fmt.Fprintf(&b, "rm -f %s\n", shellQuote(gateFile))
	}
	b.WriteString("exec ")
	for _, arg := range cmd {
		b.WriteString(shellQuote(arg))
		b.WriteByte(' ')
	}
	b.WriteByte('\n')

	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// writeStandardWrapper creates a temporary executable script that runs cmd,
// writes $? to exitFile, and self-deletes. Used for non-interactive agents
// where completion is detected by polling the exit file.
func writeStandardWrapper(cmd []string, exitFile, gateFile string, env map[string]string) (string, error) {
	f, err := os.CreateTemp("", "kontora-wrapper-*.sh")
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("unset CLAUDECODE\n")
	writeEnvExports(&b, env)
	if gateFile != "" {
		fmt.Fprintf(&b, "while [ ! -f %s ]; do sleep 0.05; done\n", shellQuote(gateFile))
		fmt.Fprintf(&b, "rm -f %s\n", shellQuote(gateFile))
	}
	for _, arg := range cmd {
		b.WriteString(shellQuote(arg))
		b.WriteByte(' ')
	}
	b.WriteByte('\n')
	b.WriteString("_EC=$?\n")
	fmt.Fprintf(&b, "echo $_EC > %s\n", shellQuote(exitFile))
	fmt.Fprintf(&b, "rm -f %s\n", shellQuote(f.Name()))
	b.WriteString("if [ $_EC -ne 0 ]; then exec \"${SHELL:-/bin/sh}\"; fi\n")

	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

var validEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// writeEnvExports writes "export KEY=VALUE" lines to the builder, sorted for
// deterministic script output. Keys that don't match [A-Za-z_][A-Za-z0-9_]*
// are skipped to prevent shell injection.
func writeEnvExports(b *strings.Builder, env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !validEnvKey.MatchString(k) {
			continue
		}
		fmt.Fprintf(b, "export %s=%s\n", k, shellQuote(env[k]))
	}
}

// shellQuote wraps s in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
