package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/process"
)

// EnsureWindowAbsent refuses duplicate names and kills a single match by ID.
// A kill error remains an error even when inspection confirms absence.
// Inspection failures and surviving windows return process.ErrTerminationUncertain.
// The caller must prevent concurrent creation of a replacement window.
func EnsureWindowAbsent(ctx context.Context, sessionName, windowName string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	windows, err := inspectWindowIDs(ctx)
	if err != nil {
		return errors.Join(process.ErrTerminationUncertain, err)
	}
	var matches []string
	for _, window := range windows {
		if window.session == sessionName && window.name == windowName {
			matches = append(matches, window.id)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	if len(matches) > 1 {
		return fmt.Errorf("%w: multiple tmux windows named %q in session %q: %v", process.ErrTerminationUncertain, windowName, sessionName, matches)
	}

	id := matches[0]
	out, killErr := exec.CommandContext(ctx, "tmux", "kill-window", "-t", id).CombinedOutput()
	if killErr != nil {
		killErr = fmt.Errorf("tmux kill-window %s: %s: %w", id, strings.TrimSpace(string(out)), killErr)
	}
	windows, err = inspectWindowIDs(ctx)
	if err != nil {
		return errors.Join(killErr, process.ErrTerminationUncertain, err)
	}
	for _, window := range windows {
		// A rename must not hide a surviving window, nor may a new ID reuse its name.
		if window.id == id || window.session == sessionName && window.name == windowName {
			return errors.Join(killErr, fmt.Errorf("%w: tmux window %s (%q in session %q) remains after killing %s", process.ErrTerminationUncertain, window.id, window.name, window.session, id))
		}
	}
	return killErr
}

type inspectedWindow struct {
	session string
	name    string
	id      string
}

func inspectWindowIDs(ctx context.Context) ([]inspectedWindow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []byte
	var err error
	for attempt := range 3 {
		out, err = exec.CommandContext(ctx, "tmux", "-u", "list-windows", "-a", "-F", "#{session_name}\t#{window_name}\t#{window_id}").CombinedOutput()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil || strings.TrimSpace(string(out)) != "server exited unexpectedly" || attempt == 2 {
			break
		}
		// The last-window server may exit while a query is connected.
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err != nil {
		message := strings.TrimSpace(string(out))
		if strings.HasPrefix(message, "no server running on ") || message == "no sessions" ||
			(strings.HasPrefix(message, "error connecting to ") && strings.HasSuffix(message, " (No such file or directory)")) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspecting tmux window IDs: %s: %w", message, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	var windows []inspectedWindow
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" || !validWindowID(fields[2]) {
			return nil, fmt.Errorf("inspecting tmux window IDs: malformed record %q", line)
		}
		windows = append(windows, inspectedWindow{session: fields[0], name: fields[1], id: fields[2]})
	}
	return windows, nil
}

func validWindowID(id string) bool {
	if len(id) < 2 || id[0] != '@' {
		return false
	}
	for _, digit := range id[1:] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
