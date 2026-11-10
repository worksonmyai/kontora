package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
)

func TestAutomationArtifactRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.jsonl")
	for _, text := range []string{"first\n", "second\n"} {
		f, err := openAutomationArtifact(path)
		require.NoError(t, err)
		_, err = f.WriteString(text)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	data, err := readAutomationArtifact(path)
	require.NoError(t, err)
	assert.Equal(t, "first\nsecond\n", string(data))
}

func TestAutomationArtifactSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("unchanged"), 0o600))
	path := filepath.Join(dir, "artifact")
	require.NoError(t, os.Symlink(target, path))
	f, err := openAutomationArtifact(path)
	if f != nil {
		require.NoError(t, f.Close())
	}
	require.Error(t, err)
	_, err = readAutomationArtifact(path)
	require.Error(t, err)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", string(data))
	info, err := os.Lstat(path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
}

func TestAutomationArtifactNonregular(t *testing.T) {
	for _, path := range []string{t.TempDir(), os.DevNull} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := openAutomationArtifact(path)
			if f != nil {
				require.NoError(t, f.Close())
			}
			require.Error(t, err)
			_, err = readAutomationArtifact(path)
			require.ErrorContains(t, err, "not a regular file")
		})
	}
}

func TestReadAutomationArtifactSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.log")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate((16<<20)+1))
	require.NoError(t, f.Close())
	_, err = readAutomationArtifact(path)
	require.ErrorContains(t, err, "exceeds 16 MiB")
	_, err = readAutomationArtifact(path + ".missing")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAutomationArtifactFIFO(t *testing.T) {
	for _, mode := range []string{"read", "write", "write_with_reader"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "artifact")
			require.NoError(t, syscall.Mkfifo(path, 0o600))
			if mode == "write_with_reader" {
				peer, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
				require.NoError(t, err)
				defer peer.Close()
			}
			// A subprocess bounds the test even if opening the FIFO blocks before stat.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAutomationArtifactFIFOHelper$", "-test.timeout=3s")
			cmd.Env = append(os.Environ(), "KONTORA_TEST_ARTIFACT_FIFO="+path, "KONTORA_TEST_ARTIFACT_MODE="+mode)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "FIFO %s: %s", mode, output)
		})
	}
}

func TestAutomationArtifactFIFOHelper(t *testing.T) {
	path := os.Getenv("KONTORA_TEST_ARTIFACT_FIFO")
	if path == "" {
		t.Skip("subprocess only")
	}
	if os.Getenv("KONTORA_TEST_ARTIFACT_MODE") == "read" {
		_, err := readAutomationArtifact(path)
		require.ErrorContains(t, err, "not a regular file")
		return
	}
	f, err := openAutomationArtifact(path)
	if f != nil {
		require.NoError(t, f.Close())
	}
	require.Error(t, err)
	if os.Getenv("KONTORA_TEST_ARTIFACT_MODE") == "write_with_reader" {
		require.ErrorContains(t, err, "not a regular file")
	}
}

type automationEventTestFile struct {
	bytes.Buffer
	writeErr, syncErr, closeErr error
	calls                       []string
}

func (f *automationEventTestFile) Write(p []byte) (int, error) {
	f.calls = append(f.calls, "write")
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.Buffer.Write(p)
}

func (f *automationEventTestFile) Sync() error {
	f.calls = append(f.calls, "sync")
	return f.syncErr
}

func (f *automationEventTestFile) Close() error {
	f.calls = append(f.calls, "close")
	return f.closeErr
}

func TestWriteAutomationEvent(t *testing.T) {
	writeErr := errors.New("write failed")
	syncErr := errors.New("sync failed")
	closeErr := errors.New("close failed")
	for _, tc := range []struct {
		name                        string
		writeErr, syncErr, closeErr error
	}{
		{name: "success"},
		{name: "write", writeErr: writeErr},
		{name: "sync", syncErr: syncErr},
		{name: "close", closeErr: closeErr},
		{name: "write and close", writeErr: writeErr, closeErr: closeErr},
		{name: "sync and close", syncErr: syncErr, closeErr: closeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &automationEventTestFile{writeErr: tc.writeErr, syncErr: tc.syncErr, closeErr: tc.closeErr}
			line := automation.LogLine{At: time.Now().UTC(), Source: "kontora", Kind: "lifecycle", Text: "started"}
			err := writeAutomationEvent(f, line)
			for _, want := range []error{tc.writeErr, tc.syncErr, tc.closeErr} {
				if want != nil {
					assert.ErrorIs(t, err, want)
				}
			}
			if tc.writeErr == nil && tc.syncErr == nil && tc.closeErr == nil {
				require.NoError(t, err)
			}
			if tc.writeErr != nil {
				assert.Equal(t, []string{"write", "close"}, f.calls)
			} else {
				assert.Equal(t, []string{"write", "sync", "close"}, f.calls)
				var got automation.LogLine
				require.NoError(t, json.Unmarshal(f.Bytes(), &got))
				assert.Equal(t, line, got)
				assert.True(t, bytes.HasSuffix(f.Bytes(), []byte("\n")))
			}
		})
	}
}
