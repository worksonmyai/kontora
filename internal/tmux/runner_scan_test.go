package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestRunScanPreparationFailure(t *testing.T) {
	for _, invalidPattern := range []bool{true, false} {
		t.Run(fmt.Sprintf("invalidPattern=%t", invalidPattern), func(t *testing.T) {
			patterns := []string{"["}
			if !invalidPattern {
				patterns = []string{"provider error"}
				path := filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(t, os.WriteFile(path, nil, 0o600))
				t.Setenv("TMPDIR", path)
			}
			launched := false
			result, err := Run(t.Context(), RunParams{
				Binary: "must-not-run", FailurePatterns: patterns,
				OnReady: func() { launched = true },
			})
			require.ErrorContains(t, err, "preparing PTY failure scan")
			assert.Equal(t, process.TerminationNotStarted, result.Termination)
			assert.False(t, result.ExitedAt.IsZero())
			assert.False(t, launched)
		})
	}
}

func TestRunFailurePatterns(t *testing.T) {
	skipIfNoTmux(t)
	for _, tc := range []struct {
		name           string
		command        string
		existing       int64
		patterns       []string
		want           *string
		noCapture      bool
		captureFailure bool
		exitCode       int
	}{
		{name: "late provider error", command: fmt.Sprintf("head -c %d /dev/zero; printf 'provider error\\n'", maxPaneLogBytes+4096), patterns: []string{"provider error"}, want: new("provider error")},
		{name: "already capped", command: "printf 'provider error\\n'", existing: maxPaneLogBytes, patterns: []string{"provider error"}, want: new("provider error")},
		{name: "already oversized", command: "printf 'provider error\\n'", existing: 45_778_216_576, patterns: []string{"provider error"}, want: new("provider error")},
		{name: "empty pattern", command: "printf 'output\\n'", existing: maxPaneLogBytes, patterns: []string{""}, want: new("")},
		{name: "multiline", command: "printf 'provider\\n'; sleep 0.05; printf 'failed\\n'", patterns: []string{`(?m)^provider\r?\nfailed\r?$`}, want: new(`(?m)^provider\r?\nfailed\r?$`)},
		{name: "configured precedence", command: "printf 'second error\\nfirst error\\n'", patterns: []string{"first error", "second error"}, want: new("first error")},
		{name: "without raw log", command: "printf 'provider error\\n'", patterns: []string{"provider error"}, want: new("provider error"), noCapture: true},
		{name: "capture fails", command: "printf 'provider error\\n'", patterns: []string{"provider error"}, want: new("provider error"), captureFailure: true},
		{name: "debug shell", command: "printf 'provider error\\n'; exit 1", patterns: []string{"provider error"}, exitCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "raw '雪 $(printf bad) #{pid}.log")
			if tc.existing > 0 {
				seedPaneLog(t, path, tc.existing)
			}
			if tc.noCapture {
				path = ""
			} else if tc.captureFailure {
				require.NoError(t, os.Mkdir(path, 0o755))
			}
			p := RunParams{
				Binary: "sh", Args: []string{"-c", tc.command}, Dir: dir,
				TicketID: "scan-" + randomSuffix(), SessionName: testSession, LogFile: path,
				FailurePatterns: tc.patterns, Timeout: 15 * time.Second,
				OnWindowCreated: func(id WindowIdentity) error {
					t.Cleanup(func() { _ = KillWindowIdentity(id) })
					return nil
				},
			}
			started := time.Now()
			result, err := Run(t.Context(), p)
			require.NoError(t, err)
			assert.Equal(t, tc.exitCode, result.ExitCode)
			assert.Equal(t, tc.want, result.FailurePattern)
			if tc.exitCode != 0 {
				assert.Less(t, time.Since(started), 5*time.Second, "debug shell must not hold scan completion open")
			}
			if path != "" && !tc.captureFailure {
				info, err := os.Stat(path)
				require.NoError(t, err)
				assert.LessOrEqual(t, info.Size(), int64(maxPaneLogBytes))
			}
		})
	}
}

func TestRunFailurePatternRetryIsolation(t *testing.T) {
	skipIfNoTmux(t)
	path := filepath.Join(t.TempDir(), "raw.log")
	for _, existing := range []int64{0, maxPaneLogBytes, 45_778_216_576} {
		seedPaneLog(t, path, existing)
		for _, output := range []string{"provider error", "clean retry"} {
			result, err := Run(t.Context(), RunParams{
				Binary: "printf", Args: []string{output + "\n"}, Dir: t.TempDir(),
				TicketID: "scan-retry-" + randomSuffix(), SessionName: testSession,
				LogFile: path, FailurePatterns: []string{"provider error"}, Timeout: 10 * time.Second,
			})
			require.NoError(t, err)
			require.Zero(t, result.ExitCode)
			if output == "provider error" {
				assert.Equal(t, new("provider error"), result.FailurePattern)
			} else {
				assert.Nil(t, result.FailurePattern)
			}
		}
	}
}

func TestRunInteractiveFailurePatternCompletion(t *testing.T) {
	skipIfNoTmux(t)
	for _, ignoresExit := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignoresExit=%v", ignoresExit), func(t *testing.T) {
			name := "scan-interactive-" + randomSuffix()
			command := "tmux wait-for -S " + shellQuote(ChannelName(testSession, name)) + "; "
			if ignoresExit {
				command += "sleep 999"
			} else {
				command += "IFS= read -r line; printf 'provider error\\n'"
			}
			result, err := Run(t.Context(), RunParams{
				Binary: "sh", Args: []string{"-c", command}, Dir: t.TempDir(),
				TicketID: name, SessionName: testSession, Interactive: true, MinDuration: -1,
				FailurePatterns: []string{`(?m)^provider error\r?$`}, Timeout: 7 * time.Second,
				OnWindowCreated: func(id WindowIdentity) error {
					t.Cleanup(func() { _ = KillWindowIdentity(id) })
					return nil
				},
			})
			if ignoresExit {
				require.ErrorContains(t, err, "waiting for PTY failure scan completion")
				assert.Nil(t, result.FailurePattern)
			} else {
				require.NoError(t, err)
				assert.Equal(t, new(`(?m)^provider error\r?$`), result.FailurePattern)
			}
			assert.Zero(t, result.ExitCode)
		})
	}
}

func TestRunMissingScanCompletionFailsClosed(t *testing.T) {
	skipIfNoTmux(t)
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	result, err := Run(t.Context(), RunParams{
		Binary: "printf", Args: []string{"clean output\n"}, Dir: dir,
		TicketID: "scan-missing-" + randomSuffix(), SessionName: testSession,
		FailurePatterns: []string{"provider error"}, Timeout: 2 * time.Second,
		OnReady: func() {
			paths, err := filepath.Glob(filepath.Join(dir, "kontora-scan-*"))
			require.NoError(t, err)
			require.Len(t, paths, 1)
			require.NoError(t, os.RemoveAll(paths[0]))
		},
	})
	require.Zero(t, result.ExitCode)
	require.ErrorContains(t, err, "waiting for PTY failure scan completion")
	assert.Nil(t, result.FailurePattern)
}

func TestRunScanCancellation(t *testing.T) {
	skipIfNoTmux(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := Run(ctx, RunParams{
		Binary: "sleep", Args: []string{"999"}, Dir: t.TempDir(),
		TicketID: "scan-cancel-" + randomSuffix(), SessionName: testSession,
		FailurePatterns: []string{"provider error"}, OnReady: cancel,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, -1, result.ExitCode)
}

func TestPipePaneScanPathEscaping(t *testing.T) {
	skipIfNoTmux(t)
	scan := testPaneLogScan(t, "provider error")
	scanPath := filepath.Join(filepath.Dir(scan.path), "scan '雪 $(printf bad) #{pid}.json")
	require.NoError(t, os.Rename(scan.path, scanPath))
	scan.path = scanPath
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	script, err := writeInteractiveWrapper([]string{"printf", "provider error\n"}, gate, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(script) })
	p := RunParams{SessionName: testSession, TicketID: "scan-path-" + randomSuffix(), Dir: dir, logScan: scan}
	tm, err := createRunWindow(p, script)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tm.KillWindow() })
	require.NoError(t, pipeRunPane(tm, p))
	require.NoError(t, os.WriteFile(gate, []byte("go"), 0o600))
	pattern, err := scan.wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, new("provider error"), pattern)
}

func TestRunScanCleanupUsesCapturedWindow(t *testing.T) {
	skipIfNoTmux(t)
	dir := t.TempDir()
	name := "scan-identity-" + randomSuffix()
	var replacement WindowIdentity
	result, err := Run(t.Context(), RunParams{
		Binary: "sh", Args: []string{"-c", "echo provider error; exit 1"}, Dir: dir,
		TicketID: name, SessionName: testSession, FailurePatterns: []string{"provider error"},
		OnWindowCreated: func(id WindowIdentity) error {
			t.Cleanup(func() { _ = KillWindowIdentity(id) })
			out, err := exec.Command("tmux", "rename-window", "-t", id.WindowID, name+"-original").CombinedOutput()
			if err != nil {
				return fmt.Errorf("renaming original window: %s: %w", out, err)
			}
			replacement, err = newWindowIdentity(testSession, name, dir, "sleep 999")
			if err != nil {
				return err
			}
			t.Cleanup(func() { _ = KillWindowIdentity(replacement) })
			return (identityTmux{id: replacement}).pipePane(filepath.Join(dir, "replacement.log"))
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExitCode)
	out, err := exec.Command("tmux", "display-message", "-p", "-t", replacement.WindowID, "#{pane_pipe}").CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Equal(t, "1", strings.TrimSpace(string(out)), "cleanup must not disable a replacement window's pipe")
}
