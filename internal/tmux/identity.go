package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/worksonmyai/kontora/internal/process"
)

// WindowIdentity names a physical window, not a session's mutable link to it.
// Persist the entire value; window IDs can be reused after a server restart.
// SessionID records creation provenance and does not limit inspection or killing.
type WindowIdentity struct {
	SocketPath      string `json:"socket_path"`
	ServerPID       int    `json:"server_pid"`
	ServerStartedAt int64  `json:"server_started_at"`
	SessionID       string `json:"session_id"`
	WindowID        string `json:"window_id"`
}

const identityFormat = "#{socket_path}\t#{pid}\t#{start_time}\t#{session_id}\t#{window_id}"

func numericID(s, prefix string) bool {
	n, err := strconv.ParseUint(strings.TrimPrefix(s, prefix), 10, 64)
	return strings.HasPrefix(s, prefix) && err == nil && prefix+strconv.FormatUint(n, 10) == s
}

func (id WindowIdentity) validate() error {
	if !filepath.IsAbs(id.SocketPath) || strings.ContainsAny(id.SocketPath, "\x00\r\n\t") || id.ServerPID <= 0 || id.ServerStartedAt <= 0 || !numericID(id.SessionID, "$") || !numericID(id.WindowID, "@") {
		return fmt.Errorf("invalid tmux window identity")
	}
	return nil
}

func parseWindowIdentity(out string) (WindowIdentity, error) {
	fields := strings.Split(strings.TrimSuffix(out, "\n"), "\t")
	if len(fields) != 5 {
		return WindowIdentity{}, fmt.Errorf("invalid tmux creation identity %q", out)
	}
	pid, _ := strconv.Atoi(fields[1])
	started, _ := strconv.ParseInt(fields[2], 10, 64)
	id := WindowIdentity{SocketPath: fields[0], ServerPID: pid, ServerStartedAt: started, SessionID: fields[3], WindowID: fields[4]}
	return id, id.validate()
}

// newWindowIdentity captures identity in the creation command itself. Looking up
// a name afterwards could capture a replacement window instead of our launch.
func newWindowIdentity(session, window, dir, script string) (WindowIdentity, error) {
	create := func(newSession bool) (WindowIdentity, string, error) {
		args := make([]string, 0, 13)
		if newSession {
			args = append(args, "new-session", "-d", "-s", session)
		} else {
			args = append(args, "new-window", "-t", "="+session+":")
		}
		args = append(args, "-P", "-F", identityFormat, "-n", window, "-c", dir, "--", script)
		cmd := exec.Command("tmux", args...)
		if newSession {
			cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8")
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			// The server may have created the gated window before disconnecting.
			return WindowIdentity{}, string(out), errors.Join(process.ErrTerminationUncertain, fmt.Errorf("creating tmux window: %s: %w", strings.TrimSpace(string(out)), err))
		}
		id, err := parseWindowIdentity(string(out))
		if err != nil {
			// Creation may have succeeded. Never kill a name to repair this.
			err = errors.Join(process.ErrTerminationUncertain, err)
		}
		return id, string(out), err
	}
	id, output, err := create(false)
	if err == nil || !isSessionMissing(output) {
		return id, err
	}
	id, output, err = create(true)
	if err != nil && strings.Contains(output, "duplicate session") {
		id, _, err = create(false)
	}
	return id, err
}

func (id WindowIdentity) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "tmux", append([]string{"-S", id.SocketPath, "-N", "-u"}, args...)...)
}

// InspectWindowIdentity finds all links by global window ID. A removed original
// session or renamed window is not absence while a viewer still links the window.
func InspectWindowIdentity(id WindowIdentity) (bool, error) {
	if err := id.validate(); err != nil {
		return false, errors.Join(process.ErrTerminationUncertain, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := id.command(ctx, "display-message", "-p", "#{pid}\t#{start_time}", ";", "list-windows", "-a", "-F", "#{window_id}").CombinedOutput()
	if err != nil {
		// An unreachable socket alone says nothing about the processes behind it.
		// Only a dead server PID supplies absence evidence here.
		if errors.Is(syscall.Kill(id.ServerPID, 0), syscall.ESRCH) {
			return false, nil
		}
		return false, errors.Join(process.ErrTerminationUncertain, fmt.Errorf("inspecting tmux identity: %s: %w", strings.TrimSpace(string(out)), err))
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if lines[0] != fmt.Sprintf("%d\t%d", id.ServerPID, id.ServerStartedAt) {
		return false, errors.Join(process.ErrTerminationUncertain, fmt.Errorf("tmux server identity changed"))
	}
	present := false
	for _, line := range lines[1:] {
		if !numericID(line, "@") {
			return false, errors.Join(process.ErrTerminationUncertain, fmt.Errorf("invalid tmux window listing"))
		}
		if line == id.WindowID {
			present = true
		}
	}
	return present, nil
}

// KillWindowIdentity destroys the window across all linked viewer sessions.
// The server-side guard prevents a reconnect race from killing a reused ID on
// a replacement server. The caller should inspect afterwards for evidence.
func KillWindowIdentity(id WindowIdentity) error {
	present, err := InspectWindowIdentity(id)
	if err != nil || !present {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	guard := fmt.Sprintf("#{&&:#{==:#{pid},%d},#{==:#{start_time},%d}}", id.ServerPID, id.ServerStartedAt)
	out, err := id.command(ctx, "if-shell", "-F", guard, "kill-window -t "+id.WindowID, "display-message -p kontora-server-identity-changed").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		return errors.Join(process.ErrTerminationUncertain, fmt.Errorf("killing tmux identity: %s: %v", strings.TrimSpace(string(out)), err))
	}
	return nil
}

// identityTmux keeps every operation on the captured server and window.
type identityTmux struct{ id WindowIdentity }

func (w identityTmux) InspectWindow() (bool, error) { return InspectWindowIdentity(w.id) }
func (w identityTmux) KillWindow() error            { return KillWindowIdentity(w.id) }
func (w identityTmux) WaitWindowExit(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		present, err := w.InspectWindow()
		if err != nil || !present {
			return err
		}
		if !time.Now().Before(deadline) {
			return errWindowExitTimeout
		}
		time.Sleep(pollInterval)
	}
}
func (w identityTmux) run(args ...string) error {
	present, err := w.InspectWindow()
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("tmux window %s is gone", w.id.WindowID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	guard := fmt.Sprintf("#{&&:#{==:#{pid},%d},#{==:#{start_time},%d}}", w.id.ServerPID, w.id.ServerStartedAt)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	out, err := w.id.command(ctx, "if-shell", "-F", guard, strings.Join(quoted, " "), "display-message -p kontora-server-identity-changed").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		return errors.Join(process.ErrTerminationUncertain, fmt.Errorf("tmux identity command: %s: %v", strings.TrimSpace(string(out)), err))
	}
	return nil
}
func (w identityTmux) SendKeys(keys string) error {
	return w.run("send-keys", "-c", "/dev/null", "-t", w.id.WindowID, keys, "Enter")
}
func (w identityTmux) SendLiteral(text string) error {
	if err := w.run("send-keys", "-c", "/dev/null", "-t", w.id.WindowID, "-l", "--", text); err != nil {
		return err
	}
	time.Sleep(sendKeysEnterDelay)
	return w.run("send-keys", "-c", "/dev/null", "-t", w.id.WindowID, "Enter")
}
func (w identityTmux) WaitFor(ctx context.Context, channel string) error {
	return w.id.command(ctx, "wait-for", channel).Run()
}
func (w identityTmux) Signal(channel string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = w.id.command(ctx, "wait-for", "-S", channel).Run()
}
func (w identityTmux) pipePane(logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	return w.run("pipe-pane", "-t", w.id.WindowID, "cat >> "+shellQuote(logPath))
}
