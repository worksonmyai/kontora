package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestEnsureWindowAbsentRealTmux(t *testing.T) {
	skipIfNoTmux(t)
	for _, tt := range []struct {
		name       string
		session    string
		window     string
		duplicates bool
		lastWindow bool
		wantErr    bool
	}{
		{name: "missing session", session: "other-session", window: "ticket"},
		{name: "missing window", session: "session", window: "other-ticket"},
		{name: "window prefix", session: "session", window: "tick"},
		{name: "session prefix", session: "sess", window: "ticket"},
		{name: "exact match", session: "session", window: "ticket"},
		{name: "numeric name is not an index", session: "session", window: "0"},
		{name: "literal pattern and spaces", session: "session", window: "ticket * [x]"},
		{name: "duplicate names", session: "session", window: "ticket", duplicates: true, wantErr: true},
		{name: "last window", session: "session", window: "ticket", lastWindow: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolatePreflightTmux(t)
			var keep []string
			if !tt.lastWindow {
				keep = append(keep, preflightTmuxOutput(t, "new-session", "-d", "-P", "-F", "#{window_id}", "-s", "session", "-n", "anchor", "--", "sleep", "999"))
			}
			window := "ticket"
			if tt.window == "0" || tt.window == "ticket * [x]" {
				window = tt.window
			}
			var target string
			if tt.lastWindow {
				target = preflightTmuxOutput(t, "new-session", "-d", "-P", "-F", "#{window_id}", "-s", "session", "-n", window, "--", "sleep", "999")
			} else {
				target = preflightTmuxOutput(t, "new-window", "-P", "-F", "#{window_id}", "-t", "=session:", "-n", window, "--", "sleep", "999")
				keep = append(keep, preflightTmuxOutput(t, "new-session", "-d", "-P", "-F", "#{window_id}", "-s", "session-long", "-n", window, "--", "sleep", "999"))
				keep = append(keep, preflightTmuxOutput(t, "new-window", "-P", "-F", "#{window_id}", "-t", "=session:", "-n", window+"-long", "--", "sleep", "999"))
			}
			if tt.duplicates {
				keep = append(keep, preflightTmuxOutput(t, "new-window", "-P", "-F", "#{window_id}", "-t", "=session:", "-n", window, "--", "sleep", "999"))
			}
			err := EnsureWindowAbsent(context.Background(), tt.session, tt.window)
			if tt.wantErr {
				require.ErrorIs(t, err, process.ErrTerminationUncertain)
				assert.Contains(t, err.Error(), "multiple tmux windows")
			} else {
				require.NoError(t, err)
			}
			if tt.session != "session" || tt.window != window || tt.wantErr {
				keep = append(keep, target)
			}
			if tt.lastWindow {
				require.NoError(t, EnsureWindowAbsent(context.Background(), tt.session, tt.window))
			} else {
				remaining := strings.Fields(preflightTmuxOutput(t, "list-windows", "-a", "-F", "#{window_id}"))
				assert.ElementsMatch(t, keep, remaining)
			}
		})
	}
}

func isolatePreflightTmux(t *testing.T) {
	t.Helper()
	// macOS limits socket paths, so t.TempDir's test name can make the path too long.
	dir, err := os.MkdirTemp("", "kt")
	require.NoError(t, err)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", dir)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
}

func preflightTmuxOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", append([]string{"-f", "/dev/null"}, args...)...).CombinedOutput()
	require.NoError(t, err, "tmux %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func TestEnsureWindowAbsentFailures(t *testing.T) {
	const match = "session\tticket\t@7\n"
	for _, tt := range []struct {
		name          string
		before        string
		after         string
		inspectCode   int
		verifyCode    int
		killCode      int
		exitBefore    int
		exitAfter     int
		wantErr       string
		wantUncertain bool
		wantKill      bool
	}{
		{name: "absent", before: "other\tticket\t@1\nsession\tticket-long\t@2\n"},
		{name: "no sessions", before: "no sessions\n", inspectCode: 1},
		{name: "no server", before: "no server running on /tmp/test\n", inspectCode: 1},
		{name: "missing socket", before: "error connecting to /tmp/test (No such file or directory)\n", inspectCode: 1},
		{name: "permission denied", before: "error connecting to /tmp/test (Permission denied)\n", inspectCode: 1, wantErr: "Permission denied", wantUncertain: true},
		{name: "connection refused", before: "error connecting to /tmp/test (Connection refused)\n", inspectCode: 1, wantErr: "Connection refused", wantUncertain: true},
		{name: "query failure", before: "query failed\n", inspectCode: 1, wantErr: "query failed", wantUncertain: true},
		{name: "duplicate names", before: match + "session\tticket\t@8\n", wantErr: "multiple tmux windows", wantUncertain: true},
		{name: "missing ID", before: "session\tticket\n", wantErr: "malformed record", wantUncertain: true},
		{name: "invalid ID", before: "session\tticket\tticket\n", wantErr: "malformed record", wantUncertain: true},
		{name: "ID with target syntax", before: "session\tticket\t@7:0\n", wantErr: "malformed record", wantUncertain: true},
		{name: "unrelated malformed record", before: match + "unexpected output\n", wantErr: "malformed record", wantUncertain: true},
		{name: "kill succeeds", before: match, wantKill: true},
		{name: "kill error survives confirmed absence", before: match, killCode: 1, wantKill: true, wantErr: "kill failed"},
		{name: "kill and query fail", before: match, killCode: 1, after: "verification failed\n", verifyCode: 1, wantKill: true, wantErr: "kill failed", wantUncertain: true},
		{name: "query after kill fails", before: match, after: "verification failed\n", verifyCode: 1, wantKill: true, wantErr: "verification failed", wantUncertain: true},
		{name: "window remains", before: match, after: match, wantKill: true, wantErr: "remains", wantUncertain: true},
		{name: "window renamed", before: match, after: "other\trenamed\t@7\n", wantKill: true, wantErr: "remains", wantUncertain: true},
		{name: "new ID reuses name", before: match, after: "session\tticket\t@8\n", wantKill: true, wantErr: "remains", wantUncertain: true},
		{name: "malformed verification", before: match, after: "session\tticket\n", wantKill: true, wantErr: "malformed record", wantUncertain: true},
		{name: "server exits after kill", before: match, after: "no server running on /tmp/test\n", verifyCode: 1, wantKill: true},
		{name: "server exiting during initial inspection", exitBefore: 1},
		{name: "persistent initial server exit", exitBefore: 3, wantErr: "server exited unexpectedly", wantUncertain: true},
		{name: "server exiting then absent", before: match, exitAfter: 1, after: "no server running on /tmp/test\n", verifyCode: 1, wantKill: true},
		{name: "server exiting twice then absent", before: match, exitAfter: 2, after: "error connecting to /tmp/test (No such file or directory)\n", verifyCode: 1, wantKill: true},
		{name: "server exit then renamed survivor", before: match, exitAfter: 1, after: "other\trenamed\t@7\n", wantKill: true, wantErr: "remains", wantUncertain: true},
		{name: "server exit then replacement", before: match, exitAfter: 1, after: "session\tticket\t@8\n", wantKill: true, wantErr: "remains", wantUncertain: true},
		{name: "server exit then malformed output", before: match, exitAfter: 1, after: "malformed\n", wantKill: true, wantErr: "malformed record", wantUncertain: true},
		{name: "persistent server exit after kill", before: match, exitAfter: 3, wantKill: true, wantErr: "server exited unexpectedly", wantUncertain: true},
		{name: "server exit then permission failure", before: match, exitAfter: 1, after: "error connecting to /tmp/test (Permission denied)\n", verifyCode: 1, wantKill: true, wantErr: "Permission denied", wantUncertain: true},
		{name: "server exit with other diagnostics", before: "server exited unexpectedly\nother failure\n", inspectCode: 1, wantErr: "other failure", wantUncertain: true},
		{name: "kill failure survives server exit retry", before: match, killCode: 1, exitAfter: 1, wantKill: true, wantErr: "kill failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$TEST_TMUX_LOG"
if [ "$1" = -u ] && [ "$2" = list-windows ] && [ "$3" = -a ] && [ "$4" = -F ] && [ "$5" = '#{session_name}	#{window_name}	#{window_id}' ]; then
    attempt=0
    if [ -f "$TEST_TMUX_ATTEMPTS" ]; then read -r attempt < "$TEST_TMUX_ATTEMPTS"; fi
    printf '%s\n' "$((attempt + 1))" > "$TEST_TMUX_ATTEMPTS"
    exits="$TEST_TMUX_EXIT_BEFORE"
    if [ -f "$TEST_TMUX_KILLED" ]; then exits="$TEST_TMUX_EXIT_AFTER"; fi
    if [ "$attempt" -lt "$exits" ]; then
        printf 'server exited unexpectedly\n' >&2
        exit 1
    fi
    if [ -f "$TEST_TMUX_KILLED" ]; then
        printf '%s' "$TEST_TMUX_AFTER"
        exit "$TEST_TMUX_VERIFY_CODE"
    fi
    printf '%s' "$TEST_TMUX_BEFORE"
    exit "$TEST_TMUX_INSPECT_CODE"
fi
if [ "$1" = kill-window ] && [ "$2" = -t ] && [ "$3" = @7 ] && [ "$#" = 3 ]; then
    printf done > "$TEST_TMUX_KILLED"
    printf '0\n' > "$TEST_TMUX_ATTEMPTS"
    if [ "$TEST_TMUX_KILL_CODE" != 0 ]; then printf 'kill failed\n' >&2; fi
    exit "$TEST_TMUX_KILL_CODE"
fi
printf 'unexpected invocation\n' >&2
exit 99
`), 0o755))
			t.Setenv("PATH", dir)
			t.Setenv("TEST_TMUX_LOG", filepath.Join(dir, "calls"))
			t.Setenv("TEST_TMUX_KILLED", filepath.Join(dir, "killed"))
			t.Setenv("TEST_TMUX_ATTEMPTS", filepath.Join(dir, "attempts"))
			t.Setenv("TEST_TMUX_EXIT_BEFORE", strconv.Itoa(tt.exitBefore))
			t.Setenv("TEST_TMUX_EXIT_AFTER", strconv.Itoa(tt.exitAfter))
			t.Setenv("TEST_TMUX_BEFORE", tt.before)
			t.Setenv("TEST_TMUX_AFTER", tt.after)
			t.Setenv("TEST_TMUX_INSPECT_CODE", strconv.Itoa(tt.inspectCode))
			t.Setenv("TEST_TMUX_VERIFY_CODE", strconv.Itoa(tt.verifyCode))
			t.Setenv("TEST_TMUX_KILL_CODE", strconv.Itoa(tt.killCode))

			err := EnsureWindowAbsent(context.Background(), "session", "ticket")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantUncertain, errors.Is(err, process.ErrTerminationUncertain))
			calls, readErr := os.ReadFile(filepath.Join(dir, "calls"))
			require.NoError(t, readErr)
			inspections := min(tt.exitBefore+1, 3)
			if tt.wantKill {
				assert.Equal(t, 1, strings.Count(string(calls), "kill-window -t @7\n"))
				inspections += min(tt.exitAfter+1, 3)
			} else {
				assert.NotContains(t, string(calls), "kill-window")
			}
			assert.Equal(t, inspections, strings.Count(string(calls), "list-windows"))
			assert.NotContains(t, string(calls), "unexpected invocation")
		})
	}
}

func TestEnsureWindowAbsentContext(t *testing.T) {
	for _, phase := range []string{"before inspection", "during inspection", "during kill"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
if [ "$TEST_TMUX_PHASE" = 'during inspection' ] || [ "$1" = kill-window ]; then
    exec /bin/sleep 30
fi
printf 'session\tticket\t@7\n'
`), 0o755))
			t.Setenv("PATH", dir)
			t.Setenv("TEST_TMUX_PHASE", phase)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			wantErr := context.DeadlineExceeded
			if phase == "before inspection" {
				cancel()
				wantErr = context.Canceled
			}
			err := EnsureWindowAbsent(ctx, "session", "ticket")
			require.ErrorIs(t, err, wantErr)
			require.ErrorIs(t, err, process.ErrTerminationUncertain)
		})
	}
}
