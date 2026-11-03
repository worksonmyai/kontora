package process

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTerminateGroup(t *testing.T) {
	type step struct {
		signal syscall.Signal
		err    error
	}
	for _, tt := range []struct {
		name    string
		steps   []step
		wantErr error
	}{
		{name: "term removes group", steps: []step{{syscall.SIGTERM, nil}, {0, syscall.ESRCH}}},
		{name: "leader exits but descendant needs kill", steps: []step{{syscall.SIGTERM, nil}, {0, nil}, {syscall.SIGKILL, nil}, {0, syscall.ESRCH}}},
		{name: "group already absent", steps: []step{{syscall.SIGTERM, syscall.ESRCH}, {0, syscall.ESRCH}}},
		{name: "term denied", steps: []step{{syscall.SIGTERM, syscall.EPERM}}, wantErr: syscall.EPERM},
		{name: "inspection denied", steps: []step{{syscall.SIGTERM, nil}, {0, syscall.EPERM}}, wantErr: syscall.EPERM},
		{name: "kill denied", steps: []step{{syscall.SIGTERM, nil}, {0, nil}, {syscall.SIGKILL, syscall.EPERM}}, wantErr: syscall.EPERM},
		{name: "group persists after kill", steps: []step{{syscall.SIGTERM, nil}, {0, nil}, {syscall.SIGKILL, nil}, {0, nil}}, wantErr: ErrTerminationUncertain},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			err := terminateGroup(123, 0, 0, func(pid int, signal syscall.Signal) error {
				require.Equal(t, -123, pid)
				require.Less(t, calls, len(tt.steps))
				step := tt.steps[calls]
				calls++
				require.Equal(t, step.signal, signal)
				return step.err
			})
			require.Equal(t, len(tt.steps), calls)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, ErrTerminationUncertain)
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRunNeverStartedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		params    RunParams
		cancelled bool
	}{
		{name: "missing executable", params: RunParams{Binary: "/no/such/worker"}},
		{name: "invalid directory", params: RunParams{Binary: "/bin/sh", Dir: "/no/such/directory"}},
		{name: "cancelled before spawn", params: RunParams{Binary: "/bin/sh"}, cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			result, err := Run(ctx, tc.params)
			require.Error(t, err)
			require.Equal(t, TerminationNotStarted, result.Termination)
			require.Equal(t, -1, result.ExitCode)
			require.True(t, result.StartedAt.IsZero())
			require.False(t, result.ExitedAt.IsZero())
		})
	}
}

func TestRunTimeoutWithSuccessfulTermHandler(t *testing.T) {
	result, err := Run(context.Background(), RunParams{
		Binary:             "/bin/sh",
		Args:               []string{"-c", `trap 'exit 0' TERM; while :; do :; done`},
		Timeout:            100 * time.Millisecond,
		RequireTermination: true,
	})
	require.ErrorIs(t, result.Interruption, context.DeadlineExceeded)
	if err != nil {
		require.ErrorIs(t, err, ErrTerminationUncertain)
		return
	}
	require.Zero(t, result.ExitCode)
	require.Equal(t, TerminationProcessGroupGone, result.Termination)
}

func TestRunStopsDescendants(t *testing.T) {
	if os.Getenv("NONO_CAP_FILE") != "" {
		t.Skip("nono denies process-group absence probes; do not retry the denied syscall")
	}
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			params := RunParams{
				Binary: "/bin/sh",
				Args: []string{"-c", `trap 'exit 0' TERM
sh -c 'trap "" TERM; echo $$ > "$1"; while :; do sleep 1; done' sh "$1" &
wait`, "sh", pidFile},
			}
			if mode == "timeout" {
				params.Timeout = time.Second
			}
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := Run(ctx, params)
				done <- outcome{result, err}
			}()
			var childPID int
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(pidFile)
				if err != nil {
					return false
				}
				childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
				return err == nil && childPID > 0
			}, 5*time.Second, 10*time.Millisecond)
			t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
			if mode == "cancel" {
				cancel()
			}
			select {
			case got := <-done:
				// Some systems retain orphaned zombies. They still count as an
				// existing group, so the runner must report uncertainty there.
				if got.err != nil {
					require.ErrorIs(t, got.err, ErrTerminationUncertain)
					assert.Equal(t, TerminationUnknown, got.result.Termination)
				} else {
					assert.Equal(t, TerminationProcessGroupGone, got.result.Termination)
					require.ErrorIs(t, syscall.Kill(childPID, 0), syscall.ESRCH)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("Run did not finish after stopping the process group")
			}
		})
	}
}
