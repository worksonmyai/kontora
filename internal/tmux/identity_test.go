package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestWindowIdentityAcrossViewerLinks(t *testing.T) {
	skipIfNoTmux(t)
	session, viewer := "origin-"+randomSuffix(), "viewer-"+randomSuffix()
	id, err := newWindowIdentity(session, "run", t.TempDir(), "sleep 999")
	require.NoError(t, err)
	t.Cleanup(func() { _ = KillWindowIdentity(id) })
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("tmux", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	run("new-session", "-d", "-s", viewer, "--", "sleep 999")
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+viewer).Run() })
	run("link-window", "-s", id.WindowID, "-t", "="+viewer+":")
	run("rename-window", "-t", id.WindowID, "renamed")
	run("kill-session", "-t", "="+session)
	// Reuse the mutable names. They must not become our inspection/kill target.
	replacement, err := newWindowIdentity(session, "run", t.TempDir(), "sleep 999")
	require.NoError(t, err)
	t.Cleanup(func() { _ = KillWindowIdentity(replacement) })
	present, err := InspectWindowIdentity(id)
	require.NoError(t, err)
	require.True(t, present)
	require.NoError(t, KillWindowIdentity(id))
	present, err = InspectWindowIdentity(id)
	require.NoError(t, err)
	require.False(t, present)
	present, err = InspectWindowIdentity(replacement)
	require.NoError(t, err)
	require.True(t, present)
}

func TestRunStableWindowIdentity(t *testing.T) {
	skipIfNoTmux(t)
	for _, interactive := range []bool{false, true} {
		for _, callbackFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("interactive=%v/failure=%v", interactive, callbackFailure), func(t *testing.T) {
				dir := t.TempDir()
				marker := filepath.Join(dir, "started")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var id WindowIdentity
				callbackErr := errors.New("cannot persist identity")
				result, err := Run(ctx, RunParams{
					SessionName: testSession, TicketID: "stable-" + randomSuffix(),
					Binary: "/bin/sh", Args: []string{"-c", "touch " + shellQuote(marker) + "; sleep 999"},
					Dir: dir, Interactive: interactive, RequireTermination: true,
					OnWindowCreated: func(created WindowIdentity) error {
						id = created
						assert.NoFileExists(t, marker, "agent must wait for durable identity")
						if callbackFailure {
							return callbackErr
						}
						// Renaming cannot break cancellation/termination checks.
						out, err := exec.Command("tmux", "rename-window", "-t", id.WindowID, "renamed-"+randomSuffix()).CombinedOutput()
						require.NoError(t, err, "%s", out)
						cancel()
						return nil
					},
				})
				if callbackFailure {
					require.ErrorIs(t, err, callbackErr)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
				assert.Equal(t, process.TerminationWindowGone, result.Termination)
				present, err := InspectWindowIdentity(id)
				require.NoError(t, err)
				assert.False(t, present)
				if callbackFailure {
					assert.NoFileExists(t, marker)
				}
			})
		}
	}
}

func TestInspectWindowIdentityFailsClosed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
if [ "$1" != -S ] || [ "$2" != /tmp/expected-socket ] || [ "$3" != -N ]; then exit 99; fi
printf '%s' "$TEST_TMUX_OUTPUT"
exit "$TEST_TMUX_CODE"
`), 0o755))
	t.Setenv("PATH", dir)
	id := WindowIdentity{SocketPath: "/tmp/expected-socket", ServerPID: os.Getpid(), ServerStartedAt: 123, SessionID: "$1", WindowID: "@3"}
	header := fmt.Sprintf("%d\t123\n", id.ServerPID)
	for _, tt := range []struct {
		name, output, code string
		present, uncertain bool
	}{
		{"present anywhere", header + "@2\n@3\n", "0", true, false},
		{"gone", header + "@2\n", "0", false, false},
		{"not a prefix", header + "@30\n", "0", false, false},
		{"restarted server", "999\t123\n@3\n", "0", false, true},
		{"same pid new server", fmt.Sprintf("%d\t124\n@3\n", id.ServerPID), "0", false, true},
		{"missing socket but live server", "no server running on /tmp/expected-socket\n", "1", false, true},
		{"query denied", "permission denied\n", "1", false, true},
		{"malformed", "", "0", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_TMUX_OUTPUT", tt.output)
			t.Setenv("TEST_TMUX_CODE", tt.code)
			present, err := InspectWindowIdentity(id)
			assert.Equal(t, tt.present, present)
			if tt.uncertain {
				require.ErrorIs(t, err, process.ErrTerminationUncertain)
				require.ErrorIs(t, KillWindowIdentity(id), process.ErrTerminationUncertain)
			} else {
				require.NoError(t, err)
			}
		})
	}
	for _, bad := range []WindowIdentity{{}, {SocketPath: "/tmp/socket", ServerPID: 1, ServerStartedAt: 1, SessionID: "$1", WindowID: "@1;kill-server"}} {
		_, err := InspectWindowIdentity(bad)
		require.ErrorIs(t, err, process.ErrTerminationUncertain)
		require.ErrorIs(t, KillWindowIdentity(bad), process.ErrTerminationUncertain)
	}
}

func TestRunIdentityCreationUncertain(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
if [ "$1" != new-window ]; then exit 99; fi
printf '%s' "$TEST_TMUX_OUTPUT"
exit "$TEST_TMUX_CODE"
`), 0o755))
	t.Setenv("PATH", dir)
	for _, tt := range []struct{ name, output, code string }{
		{"disconnected after creation", "server disconnected", "1"},
		{"missing identity", "", "0"},
		{"malformed identity", "/tmp/socket\t1\t1\t$1\t@1\nextra", "0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_TMUX_OUTPUT", tt.output)
			t.Setenv("TEST_TMUX_CODE", tt.code)
			_, err := Run(context.Background(), RunParams{Binary: "/bin/true", TicketID: "run", StableWindowIdentity: true, RequireTermination: true})
			require.ErrorIs(t, err, process.ErrTerminationUncertain)
		})
	}
}

func TestRunExpectedIdentityMismatch(t *testing.T) {
	skipIfNoTmux(t)
	expected, err := newWindowIdentity(testSession, "expected-"+randomSuffix(), t.TempDir(), "sleep 999")
	require.NoError(t, err)
	t.Cleanup(func() { _ = KillWindowIdentity(expected) })
	marker := filepath.Join(t.TempDir(), "must-not-start")
	result, err := Run(context.Background(), RunParams{
		SessionName: testSession, TicketID: "mismatch-" + randomSuffix(),
		Binary: "/usr/bin/touch", Args: []string{marker}, Dir: t.TempDir(),
		ExpectedIdentity: &expected, RequireTermination: true,
	})
	require.ErrorContains(t, err, "does not match expected identity")
	assert.Equal(t, process.TerminationWindowGone, result.Termination)
	assert.NoFileExists(t, marker)
	present, err := InspectWindowIdentity(expected)
	require.NoError(t, err)
	assert.True(t, present, "the expectation is not the newly created window")
}

func TestIdentitySendLiteral(t *testing.T) {
	skipIfNoTmux(t)
	dir := t.TempDir()
	id, err := newWindowIdentity(testSession, "literal-"+randomSuffix(), dir, "sh")
	require.NoError(t, err)
	t.Cleanup(func() { _ = KillWindowIdentity(id) })
	// Let the shell initialize before sending a command through the guarded target.
	time.Sleep(200 * time.Millisecond)
	path := filepath.Join(dir, "output '雪.txt")
	require.NoError(t, (identityTmux{id: id}).SendLiteral("printf '%s' "+shellQuote("literal ' text 雪")+" > "+shellQuote(path)))
	require.Eventually(t, func() bool { data, _ := os.ReadFile(path); return string(data) == "literal ' text 雪" }, 5*time.Second, 20*time.Millisecond)
}

func TestRunStableWindowCompletion(t *testing.T) {
	skipIfNoTmux(t)
	for _, code := range []int{0, 3} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "stable log '雪.txt")
			result, err := Run(context.Background(), RunParams{
				SessionName: testSession, TicketID: "complete-" + randomSuffix(),
				Binary: "/bin/sh", Args: []string{"-c", fmt.Sprintf("echo stable-output; exit %d", code)},
				Dir: t.TempDir(), LogFile: logPath, Timeout: 10 * time.Second,
				StableWindowIdentity: true, RequireTermination: true,
			})
			require.NoError(t, err)
			assert.Equal(t, code, result.ExitCode)
			assert.Equal(t, process.TerminationWindowGone, result.Termination)
			log, err := os.ReadFile(logPath)
			require.NoError(t, err)
			assert.Contains(t, string(log), "stable-output")
		})
	}
}
