package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skipIfNoTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
}

func waitForWindow(t *testing.T, ticketID string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if HasWindow(DefaultSessionName, ticketID) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for window %s exists=%v", ticketID, want)
}

// startTestWindow creates a tmux window in the kontora session running cmd in dir.
// Lazily creates the session if it doesn't exist.
func startTestWindow(t *testing.T, ticketID, dir string, cmd ...string) {
	t.Helper()
	if exec.Command("tmux", "has-session", "-t", "="+DefaultSessionName).Run() != nil {
		args := make([]string, 0, 8+len(cmd))
		args = append(args, "new-session", "-d", "-s", DefaultSessionName, "-n", ticketID, "-c", dir, "--")
		args = append(args, cmd...)
		out, err := exec.Command("tmux", args...).CombinedOutput()
		require.NoError(t, err, "tmux new-session: %s", strings.TrimSpace(string(out)))
	} else {
		args := make([]string, 0, 7+len(cmd))
		args = append(args, "new-window", "-t", "="+DefaultSessionName+":", "-n", ticketID, "-c", dir, "--")
		args = append(args, cmd...)
		out, err := exec.Command("tmux", args...).CombinedOutput()
		require.NoError(t, err, "tmux new-window: %s", strings.TrimSpace(string(out)))
	}
}

func TestChannelName(t *testing.T) {
	assert.Equal(t, "kontora-tst-001", ChannelName("tst-001"))
}

func TestWindowTarget(t *testing.T) {
	assert.Equal(t, "=kontora:tst-001", WindowTarget(DefaultSessionName, "tst-001"))
}

func TestSendKeys(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-sk-" + randomSuffix()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.txt")

	// Start a window running a shell that waits for commands.
	// SendKeys sends a command that creates a file, proving keystrokes arrive.
	startTestWindow(t, ticketID, dir, "sh")
	t.Cleanup(func() { _ = KillWindow(DefaultSessionName, ticketID) })
	waitForWindow(t, ticketID, true, 5*time.Second)
	// Give the shell inside tmux a moment to initialize.
	time.Sleep(300 * time.Millisecond)

	require.NoError(t, SendKeys(DefaultSessionName, ticketID, "echo SENDKEYS_OK > "+outFile))

	// Wait for the command to execute and write the file.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(outFile); err == nil && strings.Contains(string(data), "SENDKEYS_OK") {
			return // success
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timed out waiting for SendKeys output file")
}

func TestKillWindow(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-kill-" + randomSuffix()
	startTestWindow(t, ticketID, t.TempDir(), "sleep", "999")

	waitForWindow(t, ticketID, true, 5*time.Second)

	require.NoError(t, KillWindow(DefaultSessionName, ticketID))

	waitForWindow(t, ticketID, false, 5*time.Second)
}

func TestListWindows(t *testing.T) {
	skipIfNoTmux(t)

	suffix := randomSuffix()
	taskID1 := "test-list1-" + suffix
	taskID2 := "test-list2-" + suffix

	dir := t.TempDir()

	startTestWindow(t, taskID1, dir, "sleep", "999")
	startTestWindow(t, taskID2, dir, "sleep", "999")
	t.Cleanup(func() {
		_ = KillWindow(DefaultSessionName, taskID1)
		_ = KillWindow(DefaultSessionName, taskID2)
	})

	waitForWindow(t, taskID1, true, 5*time.Second)
	waitForWindow(t, taskID2, true, 5*time.Second)

	windows, err := ListWindows(DefaultSessionName)
	require.NoError(t, err)

	assert.Contains(t, windows, taskID1)
	assert.Contains(t, windows, taskID2)
}

func TestPipePaneTo(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-pipe-" + randomSuffix()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "output.log")

	startTestWindow(t, ticketID, dir, "sh", "-c", "echo KONTORA_TEST_OUTPUT; sleep 1")
	t.Cleanup(func() { _ = KillWindow(DefaultSessionName, ticketID) })

	waitForWindow(t, ticketID, true, 5*time.Second)

	require.NoError(t, PipePaneTo(DefaultSessionName, ticketID, logPath))

	// Wait for the window to exit.
	waitForWindow(t, ticketID, false, 30*time.Second)

	if data, err := os.ReadFile(logPath); err == nil {
		t.Logf("captured log: %q", string(data))
	}
}

func TestWriteInteractiveWrapper(t *testing.T) {
	cases := []struct {
		name     string
		cmd      []string
		gateFile string
		env      map[string]string
		wantExec bool
		wantGate bool
	}{
		{
			name:     "with gate",
			cmd:      []string{"claude", "--dangerously-skip-permissions", "do stuff"},
			gateFile: "/tmp/gate",
			wantExec: true,
			wantGate: true,
		},
		{
			name:     "without gate",
			cmd:      []string{"claude", "prompt"},
			gateFile: "",
			wantExec: true,
			wantGate: false,
		},
		{
			name:     "with env",
			cmd:      []string{"claude", "prompt"},
			env:      map[string]string{"FOO": "bar", "BAZ": "qux"},
			wantExec: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, err := writeInteractiveWrapper(tc.cmd, tc.gateFile, tc.env)
			require.NoError(t, err)
			defer os.Remove(path)

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			script := string(data)

			assert.Contains(t, script, "exec ")
			assert.NotContains(t, script, "echo $?")
			if tc.wantGate {
				assert.Contains(t, script, "/tmp/gate")
			} else {
				assert.NotContains(t, script, "while [")
			}
			for k, v := range tc.env {
				assert.Contains(t, script, fmt.Sprintf("export %s='%s'", k, v))
			}
		})
	}
}

func TestWriteStandardWrapper(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "no env"},
		{name: "with env", env: map[string]string{"KEY": "value"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, err := writeStandardWrapper([]string{"echo", "hello"}, "/tmp/exit", "", tc.env)
			require.NoError(t, err)
			defer os.Remove(path)

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			script := string(data)

			assert.Contains(t, script, "'echo' 'hello'")
			assert.Contains(t, script, "_EC=$?")
			assert.Contains(t, script, "echo $_EC > '/tmp/exit'")
			assert.Contains(t, script, `exec "${SHELL:-/bin/sh}"`)
			for k, v := range tc.env {
				assert.Contains(t, script, fmt.Sprintf("export %s='%s'", k, v))
			}
		})
	}
}

func TestWriteEnvExports_InvalidKeys(t *testing.T) {
	var b strings.Builder
	writeEnvExports(&b, map[string]string{
		"VALID_KEY":      "good",
		"ALSO_VALID1":    "good",
		"bad;rm -rf /":   "injected",
		"$(evil)":        "injected",
		"has space":      "injected",
		"":               "empty",
		"123_STARTS_NUM": "injected",
		"_UNDERSCORE_OK": "good",
	})
	script := b.String()

	assert.Contains(t, script, "export VALID_KEY='good'")
	assert.Contains(t, script, "export ALSO_VALID1='good'")
	assert.Contains(t, script, "export _UNDERSCORE_OK='good'")
	assert.NotContains(t, script, "rm -rf")
	assert.NotContains(t, script, "evil")
	assert.NotContains(t, script, "has space")
	assert.NotContains(t, script, "123_STARTS_NUM")
}

// --- Runner tests ---

func TestRunExitCode(t *testing.T) {
	skipIfNoTmux(t)

	cases := []struct {
		name         string
		binary       string
		wantExitCode int
	}{
		{"success", "true", 0},
		{"failure", "false", 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticketID := "test-r-" + tc.name + "-" + randomSuffix()
			t.Cleanup(func() { _ = KillWindow(DefaultSessionName, ticketID) })
			result, err := Run(context.Background(), RunParams{
				Binary:   tc.binary,
				Dir:      t.TempDir(),
				TicketID: ticketID,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantExitCode, result.ExitCode)
			assert.False(t, result.StartedAt.IsZero(), "StartedAt is zero")
			assert.False(t, result.ExitedAt.IsZero(), "ExitedAt is zero")
		})
	}
}

func TestRunTimeout(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-rtmo-" + randomSuffix()
	result, err := Run(context.Background(), RunParams{
		Binary:   "sleep",
		Args:     []string{"999"},
		Dir:      t.TempDir(),
		Timeout:  500 * time.Millisecond,
		TicketID: ticketID,
	})
	require.Error(t, err)
	assert.Equal(t, -1, result.ExitCode)

	waitForWindow(t, ticketID, false, 5*time.Second)
}

func TestRunContextCancel(t *testing.T) {
	skipIfNoTmux(t)

	ctx, cancel := context.WithCancel(context.Background())
	ticketID := "test-rctx-" + randomSuffix()

	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	result, err := Run(ctx, RunParams{
		Binary:   "sleep",
		Args:     []string{"999"},
		Dir:      t.TempDir(),
		TicketID: ticketID,
	})
	require.Error(t, err)
	assert.Equal(t, -1, result.ExitCode)

	waitForWindow(t, ticketID, false, 5*time.Second)
}

func TestRunLogCapture(t *testing.T) {
	skipIfNoTmux(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	ticketID := "test-rlog-" + randomSuffix()
	t.Cleanup(func() { _ = KillWindow(DefaultSessionName, ticketID) })

	result, err := Run(context.Background(), RunParams{
		Binary:   "sh",
		Args:     []string{"-c", "echo RUNNER_LOG_TEST; sleep 1"},
		Dir:      dir,
		TicketID: ticketID,
		LogFile:  logPath,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "RUNNER_LOG_TEST")
}

func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func TestRunWindowVanished(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-rvan-" + randomSuffix()

	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = KillWindow(DefaultSessionName, ticketID)
	}()

	result, err := Run(context.Background(), RunParams{
		Binary:   "sleep",
		Args:     []string{"999"},
		Dir:      t.TempDir(),
		TicketID: ticketID,
	})
	require.ErrorContains(t, err, "vanished")
	assert.Equal(t, -1, result.ExitCode)
}

func TestRunInteractiveWindowVanished(t *testing.T) {
	skipIfNoTmux(t)

	// Keep a dummy window alive so the session survives the kill,
	// forcing the windowGone poll path instead of hookFired.
	anchor := "test-rivan-anchor-" + randomSuffix()
	startTestWindow(t, anchor, t.TempDir(), "sleep", "999")
	t.Cleanup(func() { _ = KillWindow(DefaultSessionName, anchor) })

	ticketID := "test-rivan-" + randomSuffix()

	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = KillWindow(DefaultSessionName, ticketID)
	}()

	result, err := Run(context.Background(), RunParams{
		Binary:      "sleep",
		Args:        []string{"999"},
		Dir:         t.TempDir(),
		TicketID:    ticketID,
		Interactive: true,
		MinDuration: -1,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExitCode)
}

func TestRunInteractiveTimeout(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-rito-" + randomSuffix()
	result, err := Run(context.Background(), RunParams{
		Binary:      "sleep",
		Args:        []string{"999"},
		Dir:         t.TempDir(),
		Timeout:     500 * time.Millisecond,
		TicketID:    ticketID,
		Interactive: true,
	})
	require.Error(t, err)
	assert.Equal(t, -1, result.ExitCode)

	waitForWindow(t, ticketID, false, 5*time.Second)
}

func TestRunInteractiveMinDuration(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-rimd-" + randomSuffix()
	channel := ChannelName(ticketID)

	// Signal quickly — simulates agent crash on startup.
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = exec.Command("tmux", "wait-for", "-S", channel).Run()
	}()

	t.Cleanup(func() { _ = KillWindow(DefaultSessionName, ticketID) })

	result, err := Run(context.Background(), RunParams{
		Binary:      "sleep",
		Args:        []string{"999"},
		Dir:         t.TempDir(),
		Timeout:     10 * time.Second,
		TicketID:    ticketID,
		Interactive: true,
	})
	require.ErrorContains(t, err, "exited too quickly")
	assert.Equal(t, 1, result.ExitCode)
}

func TestRunInteractiveWaitFor(t *testing.T) {
	skipIfNoTmux(t)

	ticketID := "test-riwf-" + randomSuffix()
	channel := ChannelName(ticketID)

	// Signal the wait-for channel after a short delay (simulating the hook).
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = exec.Command("tmux", "wait-for", "-S", channel).Run()
	}()

	t.Cleanup(func() { _ = KillWindow(DefaultSessionName, ticketID) })

	result, err := Run(context.Background(), RunParams{
		Binary:      "sleep",
		Args:        []string{"999"},
		Dir:         t.TempDir(),
		Timeout:     10 * time.Second,
		TicketID:    ticketID,
		Interactive: true,
		MinDuration: -1,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
}
