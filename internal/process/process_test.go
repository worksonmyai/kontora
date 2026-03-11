package process

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		params   RunParams
		wantCode int
		wantErr  bool
	}{
		{
			name:     "true exits 0",
			params:   RunParams{Binary: "true"},
			wantCode: 0,
		},
		{
			name:     "false exits 1",
			params:   RunParams{Binary: "false"},
			wantCode: 1,
		},
		{
			name:     "custom exit code",
			params:   RunParams{Binary: "sh", Args: []string{"-c", "exit 42"}},
			wantCode: 42,
		},
		{
			name:    "nonexistent binary",
			params:  RunParams{Binary: "/no/such/binary"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Run(context.Background(), tt.params)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCode, result.ExitCode)
			assert.False(t, result.StartedAt.IsZero() || result.ExitedAt.IsZero(), "timestamps not set")
		})
	}
}

func TestRunTimeout(t *testing.T) {
	result, err := Run(context.Background(), RunParams{
		Binary:  "sleep",
		Args:    []string{"60"},
		Timeout: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.NotEqual(t, 0, result.ExitCode)
}

func TestRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := Run(ctx, RunParams{
			Binary: "sleep",
			Args:   []string{"60"},
		})
		// Use assert (not require) — t.FailNow() panics from goroutines.
		assert.NoError(t, err)
		assert.NotEqual(t, 0, result.ExitCode)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestRunEnv(t *testing.T) {
	var buf bytes.Buffer
	result, err := Run(context.Background(), RunParams{
		Binary: "sh",
		Args:   []string{"-c", "echo $KONTORA_TEST_VAR"},
		Stdout: &buf,
		Env:    []string{"KONTORA_TEST_VAR=hello_from_kontora"},
	})
	require.NoError(t, err)
	require.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "hello_from_kontora\n", buf.String())
}

func TestRunDir(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	result, err := Run(context.Background(), RunParams{
		Binary: "sh",
		Args:   []string{"-c", "pwd"},
		Dir:    dir,
		Stdout: &buf,
	})
	require.NoError(t, err)
	require.Equal(t, 0, result.ExitCode)
	// Resolve symlinks since macOS /tmp → /private/tmp
	wantDir, _ := filepath.EvalSymlinks(dir)
	gotDir, _ := filepath.EvalSymlinks(buf.String()[:len(buf.String())-1])
	assert.Equal(t, wantDir, gotDir)
}
