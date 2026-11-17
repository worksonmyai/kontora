package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainPaneLog(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing int64
		input    string
	}{
		{name: "ordinary output", input: "hello\r\n\x1b[31mworld\x1b[0m\r\n"},
		{name: "append", existing: 32, input: "next turn\r\n"},
		{name: "new file reaches cap", input: strings.Repeat("x", maxPaneLogBytes+4096)},
		{name: "append reaches cap", existing: maxPaneLogBytes - 4, input: "123456789"},
		{name: "already capped", existing: maxPaneLogBytes, input: "discard"},
		{name: "already too large", existing: maxPaneLogBytes + 4096, input: "discard"},
		{name: "incident size", existing: 45_778_216_576, input: "discard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "raw '雪.log")
			if tc.existing > 0 {
				seedPaneLog(t, path, tc.existing)
			}
			input := strings.NewReader(tc.input)
			require.NoError(t, DrainPaneLog(input, path))
			assert.Zero(t, input.Len(), "must consume output beyond the cap")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Len(t, data, int(min(tc.existing+int64(len(tc.input)), maxPaneLogBytes)))
			if tc.existing > 0 {
				assert.True(t, bytes.HasPrefix(data, []byte("existing output\n")))
			}
			if tc.existing < maxPaneLogBytes {
				assert.Equal(t, tc.input[:min(int64(len(tc.input)), maxPaneLogBytes-tc.existing)], string(data[tc.existing:]))
			}
		})
	}
}

func TestLivePaneLogCap(t *testing.T) {
	skipIfNoTmux(t)
	for _, stable := range []bool{false, true} {
		for _, existing := range []int64{maxPaneLogBytes - 4, 45_778_216_576} {
			t.Run(fmt.Sprintf("stable=%v/existing=%d", stable, existing), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "raw '雪 $(printf bad) #{pid}.log")
				seedPaneLog(t, path, existing)
				start, resume := filepath.Join(dir, "start"), filepath.Join(dir, "resume")
				first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
				flood := "i=0; while [ \"$i\" -lt 512 ]; do printf '%s' " + shellQuote(strings.Repeat("0123456789abcdef", 256)) + "; i=$((i+1)); done"
				command := fmt.Sprintf("%s; printf done > %s; while [ ! -f %s ]; do sleep 0.05; done; %s; printf done > %s; sleep 999", flood, shellQuote(first), shellQuote(resume), flood, shellQuote(second))
				script, err := writeInteractiveWrapper([]string{"sh", "-c", command}, start, nil, nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = os.Remove(script) })
				p := RunParams{SessionName: testSession, TicketID: "logcap-" + randomSuffix(), Dir: dir, LogFile: path, StableWindowIdentity: stable}
				tm, err := createRunWindow(p, script)
				require.NoError(t, err)
				t.Cleanup(func() { _ = tm.KillWindow() })
				require.NoError(t, pipeRunPane(tm, p))
				require.NoError(t, os.WriteFile(start, []byte("go"), 0o644))

				for _, marker := range []string{first, second} {
					requireFileContains(t, marker, "done", 15*time.Second)
					info, err := os.Stat(path)
					require.NoError(t, err)
					assert.EqualValues(t, maxPaneLogBytes, info.Size())
					present, err := tm.InspectWindow()
					require.NoError(t, err)
					assert.True(t, present, "cap must apply before the runner exits")
					out, err := exec.Command("tmux", "display-message", "-p", "-t", WindowTarget(testSession, p.WindowName()), "#{pane_pipe}").CombinedOutput()
					require.NoError(t, err, "%s", out)
					assert.Equal(t, "1", strings.TrimSpace(string(out)), "sink must stay alive after the cap")
					require.NoError(t, os.WriteFile(resume, []byte("go"), 0o644))
				}
			})
		}
	}
}

func seedPaneLog(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = f.WriteString("existing output\n")
	require.NoError(t, err)
	// Sparse files cover incident-sized logs without using that much disk space.
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
}

func TestDrainPaneLogContinuesAfterCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.log")
	seedPaneLog(t, path, maxPaneLogBytes-4)
	input, output := io.Pipe()
	t.Cleanup(func() { _ = input.Close(); _ = output.Close() })
	done := make(chan error, 1)
	go func() { done <- DrainPaneLog(input, path) }()
	_, err := io.WriteString(output, "1234")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		info, err := os.Stat(path)
		return err == nil && info.Size() == maxPaneLogBytes
	}, 5*time.Second, 10*time.Millisecond)

	later := make(chan error, 1)
	go func() {
		_, err := io.WriteString(output, strings.Repeat("discard", 1<<17))
		later <- err
	}()
	select {
	case err := <-later:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("output blocked after reaching the cap")
	}
	select {
	case err := <-done:
		t.Fatalf("sink exited before EOF: %v", err)
	default:
	}
	require.NoError(t, output.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("sink did not exit after EOF")
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.EqualValues(t, maxPaneLogBytes, info.Size())
}

func TestDrainPaneLogOpenFailureStillDrains(t *testing.T) {
	for _, kind := range []string{"missing directory", "directory", "device", "symlink", "fifo", "busy"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "raw.log")
			switch kind {
			case "missing directory":
				path = filepath.Join(path, "missing.log")
			case "directory":
				require.NoError(t, os.Mkdir(path, 0o755))
			case "device":
				path = os.DevNull
			case "symlink":
				target := filepath.Join(t.TempDir(), "session.jsonl")
				require.NoError(t, os.WriteFile(target, []byte("structured evidence"), 0o644))
				require.NoError(t, os.Symlink(target, path))
				t.Cleanup(func() {
					data, err := os.ReadFile(target)
					require.NoError(t, err)
					assert.Equal(t, "structured evidence", string(data))
				})
			case "fifo":
				require.NoError(t, syscall.Mkfifo(path, 0o600))
			case "busy":
				seedPaneLog(t, path, maxPaneLogBytes-4)
				f, _, err := openPaneLog(path)
				require.NoError(t, err)
				t.Cleanup(func() { _ = f.Close() })
			}
			input := strings.NewReader(strings.Repeat("output", 1<<16))
			require.Error(t, DrainPaneLog(input, path))
			assert.Zero(t, input.Len())
		})
	}
}

type failingPaneLog struct {
	writeErr, closeErr error
	short              bool
	writes             int
	closed             bool
}

func (f *failingPaneLog) Write(p []byte) (int, error) {
	f.writes++
	n := len(p)
	if f.short {
		n /= 2
	}
	return n, f.writeErr
}

func (f *failingPaneLog) Close() error {
	f.closed = true
	return f.closeErr
}

func TestDrainPaneLogIOFailureStillDrains(t *testing.T) {
	closeErr := errors.New("close failed")
	for _, tc := range []struct {
		name     string
		short    bool
		writeErr error
		closeErr error
		wantErr  error
	}{
		{name: "ENOSPC", short: true, writeErr: syscall.ENOSPC, wantErr: syscall.ENOSPC},
		{name: "short write", short: true, wantErr: io.ErrShortWrite},
		{name: "close fails", closeErr: closeErr, wantErr: closeErr},
		{name: "write and close fail", short: true, writeErr: syscall.EIO, closeErr: closeErr, wantErr: syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.NewReader(strings.Repeat("output", 1<<16))
			output := &failingPaneLog{writeErr: tc.writeErr, closeErr: tc.closeErr, short: tc.short}
			err := drainPaneLog(input, output, 64)
			require.ErrorIs(t, err, tc.wantErr)
			if tc.closeErr != nil {
				require.ErrorIs(t, err, tc.closeErr)
			}
			assert.Zero(t, input.Len())
			assert.Equal(t, 1, output.writes, "must stop writing after an I/O failure")
			assert.True(t, output.closed)
		})
	}
}
