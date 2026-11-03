package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestInspectWindow(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
if [ "$1" != list-windows ] || [ "$2" != -a ] || [ "$3" != -F ]; then exit 99; fi
printf '%s' "$TEST_TMUX_OUTPUT"
exit "$TEST_TMUX_CODE"
`), 0o755))
	t.Setenv("PATH", dir)
	for _, tt := range []struct {
		name    string
		output  string
		code    string
		present bool
		wantErr bool
	}{
		{name: "exact", output: "session\tticket\n", code: "0", present: true},
		{name: "prefix is not present", output: "session\tticket-long\n", code: "0"},
		{name: "another session is not present", output: "session-long\tticket\n", code: "0"},
		{name: "numeric window name is exact", output: "session\t0\n", code: "0"},
		{name: "empty server", output: "no sessions\n", code: "1"},
		{name: "no running server", output: "no server running on /tmp/test\n", code: "1"},
		{name: "missing socket", output: "error connecting to /tmp/test (No such file or directory)\n", code: "1"},
		{name: "permission denied", output: "error connecting to /tmp/test (Permission denied)\n", code: "1", wantErr: true},
		{name: "connection refused is uncertain", output: "error connecting to /tmp/test (Connection refused)\n", code: "1", wantErr: true},
		{name: "unknown query failure", output: "server failed\n", code: "1", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_TMUX_OUTPUT", tt.output)
			t.Setenv("TEST_TMUX_CODE", tt.code)
			present, err := InspectWindow("session", "ticket")
			assert.Equal(t, tt.present, present)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAttemptWindowName(t *testing.T) {
	for _, tt := range []struct {
		name    string
		ticket  string
		attempt string
	}{
		{name: "ordinary ticket", ticket: "kon-123"},
		{name: "ordinary unusual name unchanged", ticket: "ticket.with spaces"},
		{name: "attempt", ticket: "kon-123", attempt: "run-1"},
		{name: "unsafe attempt", ticket: "ticket:with.dots", attempt: "run:1 / [x]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := RunParams{TicketID: tt.ticket, AttemptID: tt.attempt, SessionName: "session"}
			name := p.WindowName()
			assert.Equal(t, name, p.WindowName())
			if tt.attempt == "" {
				assert.Equal(t, tt.ticket, name)
			} else {
				assert.NotContains(t, name, ":")
				assert.NotContains(t, name, ".")
				assert.NotContains(t, name, " ")
				other := p
				other.AttemptID += "-next"
				assert.NotEqual(t, name, other.WindowName())
			}
			assert.Equal(t, CompactChannelName("session", name, ""), p.compactChannel())
			p.CompactChannel = "explicit-hook"
			assert.Equal(t, "explicit-hook", p.compactChannel())
		})
	}
	assert.NotEqual(t,
		(RunParams{TicketID: "ticket", AttemptID: "run:1"}).WindowName(),
		(RunParams{TicketID: "ticket", AttemptID: "run.1"}).WindowName())
}

func TestRunAttemptCancellation(t *testing.T) {
	skipIfNoTmux(t)
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "standard", true: "interactive"}[interactive], func(t *testing.T) {
			ticket := "attempt-test-" + randomSuffix()
			anchor := RunParams{SessionName: testSession, TicketID: ticket, AttemptID: "other"}
			startTestWindow(t, anchor.WindowName(), t.TempDir(), "sleep", "999")
			t.Cleanup(func() { _ = KillWindow(testSession, anchor.WindowName()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := RunParams{
				SessionName: testSession, TicketID: ticket, AttemptID: "cancel:me",
				Binary: "sleep", Args: []string{"999"}, Dir: t.TempDir(),
				Interactive: interactive, MinDuration: -1, OnReady: cancel,
			}
			t.Cleanup(func() { _ = KillWindow(testSession, p.WindowName()) })
			result, err := Run(ctx, p)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, -1, result.ExitCode)
			assert.Equal(t, process.TerminationWindowGone, result.Termination)
			present, err := InspectWindow(testSession, p.WindowName())
			require.NoError(t, err)
			assert.False(t, present)
			present, err = InspectWindow(testSession, anchor.WindowName())
			require.NoError(t, err)
			assert.True(t, present, "cancellation must not kill another attempt")
		})
	}
}

func TestRunCancellationKillFailure(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "standard", true: "interactive"}[interactive], func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
case "$1" in
new-window) exit 0 ;;
kill-window) echo 'kill failed' >&2; exit 1 ;;
list-windows) echo 'query failed' >&2; exit 1 ;;
wait-for) exit 0 ;;
*) exit 99 ;;
esac
`), 0o755))
			t.Setenv("PATH", dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := Run(ctx, RunParams{
				SessionName: "fake-session", TicketID: "fake-ticket", Binary: "/bin/true",
				Interactive: interactive, MinDuration: -1, OnReady: cancel,
			})
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorIs(t, err, process.ErrTerminationUncertain)
			assert.True(t, strings.Contains(err.Error(), "kill failed"))
			assert.Equal(t, process.TerminationUnknown, result.Termination)
			assert.Equal(t, -1, result.ExitCode)
			assert.WithinDuration(t, time.Now(), result.ExitedAt, time.Second)
		})
	}
}
