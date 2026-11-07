package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestIdleLoopTermination(t *testing.T) {
	killFailure := errors.New("kill rejected")
	queryFailure := errors.New("query rejected")
	for _, tt := range []struct {
		name               string
		killErr            error
		waitErr            error
		inspectErr         error
		cancel             bool
		hook               bool
		cancelInDecision   bool
		requireTermination bool
		wantErr            error
		wantUncertain      bool
		wantCode           int
	}{
		{name: "cancel confirms absence", cancel: true, wantErr: context.Canceled, wantCode: -1},
		{name: "cancel preserves kill failure even if window gone", cancel: true, killErr: killFailure, wantErr: killFailure, wantCode: -1},
		{name: "cancel cannot verify absence", cancel: true, waitErr: queryFailure, wantErr: queryFailure, wantUncertain: true, wantCode: -1},
		{name: "cancel window remains", cancel: true, waitErr: errWindowExitTimeout, wantErr: errWindowExitTimeout, wantUncertain: true, wantCode: -1},
		{name: "poll query failure is not window gone", inspectErr: queryFailure, wantErr: queryFailure, wantUncertain: true, wantCode: -1},
		{name: "hook query failure is not window gone", hook: true, inspectErr: queryFailure, wantErr: queryFailure, wantUncertain: true, wantCode: -1},
		{name: "cancel during decision", hook: true, cancelInDecision: true, wantErr: context.Canceled, wantCode: -1},
		{name: "finish query failure", hook: true, waitErr: queryFailure, wantErr: queryFailure, wantUncertain: true, wantCode: -1},
		{name: "finish keeps existing grace timeout behavior", hook: true, waitErr: errWindowExitTimeout, wantCode: 0},
		{name: "required finish confirms absence", hook: true, requireTermination: true, wantCode: 0},
		{name: "required finish rejects live window", hook: true, requireTermination: true, waitErr: errWindowExitTimeout, wantErr: errWindowExitTimeout, wantUncertain: true, wantCode: -1},
		{name: "required finish rejects query failure", hook: true, requireTermination: true, waitErr: queryFailure, wantErr: queryFailure, wantUncertain: true, wantCode: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := RunParams{TicketID: "idle-termination", MinDuration: -1, RequireTermination: tt.requireTermination}
			tm := newStubTmux()
			tm.killErr, tm.waitErr, tm.inspectErr = tt.killErr, tt.waitErr, tt.inspectErr
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			if tt.hook {
				tm.latch(ChannelName(p.session(), p.WindowName()))
			}
			if tt.cancelInDecision {
				p.OnIdle = func(context.Context, IdleEvent) IdleDecision {
					cancel()
					return IdleDecision{Action: IdlePrompt, Continuation: "must not be sent"}
				}
			}
			code, err := idleLoop(ctx, tm, p, time.Now())
			require.Equal(t, tt.wantCode, code)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantUncertain, errors.Is(err, process.ErrTerminationUncertain))
			if tt.cancel || tt.cancelInDecision {
				assert.ErrorIs(t, err, context.Canceled)
				assert.True(t, tm.killed)
				assert.True(t, tm.exited, "absence must be checked even if kill failed")
				assert.Empty(t, tm.sent())
			}
		})
	}
}

// The fake tmux runs the wrapper synchronously, so its exit file is already
// visible, but independently controls window inspection. This models the gap
// between command exit and window disappearance (or an uninspectable server).
func TestRunExitFileTermination(t *testing.T) {
	for _, tt := range []struct {
		name               string
		mode               string
		requireTermination bool
		wantUncertain      bool
		wantGone           bool
		cancelDuringWait   bool
	}{
		{name: "unrelated live window retains existing behavior", mode: "live"},
		{name: "required waits for window disappearance", mode: "delayed", requireTermination: true, wantGone: true},
		{name: "required rejects live window after exit file", mode: "live", requireTermination: true, wantUncertain: true},
		{name: "required rejects inspection failure after exit file", mode: "error", requireTermination: true, wantUncertain: true},
		{name: "cancellation during required confirmation is not success", mode: "delayed", requireTermination: true, wantGone: true, cancelDuringWait: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "queries")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(`#!/bin/sh
if [ "$1" = -u ]; then shift; fi
case "$1" in
new-window)
  for script; do :; done
  /bin/sh "$script"
  ;;
kill-window) exit 0 ;;
list-windows)
  count=0
  if [ -f "$TEST_TMUX_STATE" ]; then read -r count < "$TEST_TMUX_STATE"; fi
  count=$((count + 1))
  echo "$count" > "$TEST_TMUX_STATE"
  case "$TEST_TMUX_MODE" in
    error) echo 'query rejected' >&2; exit 1 ;;
    delayed) if [ "$count" -ge 3 ]; then exit 0; fi ;;
  esac
  printf 'fake-session\tfake-ticket\n'
  ;;
*) exit 99 ;;
esac
`), 0o755))
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_TMUX_STATE", state)
			t.Setenv("TEST_TMUX_MODE", tt.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if tt.cancelDuringWait {
				go func() {
					for {
						if _, err := os.Stat(state); err == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}()
			}
			result, err := Run(ctx, RunParams{
				SessionName: "fake-session", TicketID: "fake-ticket", Binary: "/bin/sh", Args: []string{"-c", "exit 0"},
				RequireTermination: tt.requireTermination,
			})
			if tt.cancelDuringWait {
				require.ErrorIs(t, err, context.Canceled)
			} else if tt.wantUncertain {
				require.ErrorIs(t, err, process.ErrTerminationUncertain)
			} else {
				require.NoError(t, err)
			}
			if tt.cancelDuringWait {
				assert.Equal(t, -1, result.ExitCode)
			} else {
				assert.Equal(t, 0, result.ExitCode, "preserve the observed command status")
			}
			if tt.wantGone {
				assert.Equal(t, process.TerminationWindowGone, result.Termination)
			} else {
				assert.Equal(t, process.TerminationUnknown, result.Termination)
			}
		})
	}
}

func TestWindowResultEvidence(t *testing.T) {
	queryFailure := errors.New("inspection failed")
	for _, tt := range []struct {
		name               string
		absent             bool
		inspectErr         error
		want               process.TerminationEvidence
		requireTermination bool
	}{
		{name: "window remains", want: process.TerminationUnknown},
		{name: "window gone", absent: true, want: process.TerminationWindowGone},
		{name: "query failed", inspectErr: queryFailure, want: process.TerminationUnknown},
		{name: "required window remains", requireTermination: true, want: process.TerminationUnknown},
		{name: "required window gone", requireTermination: true, absent: true, want: process.TerminationWindowGone},
		{name: "required query failed", requireTermination: true, inspectErr: queryFailure, want: process.TerminationUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tm := newStubTmux()
			tm.noWindow, tm.inspectErr = tt.absent, tt.inspectErr
			result, err := windowResult(tm, 1, time.Now(), nil, tt.requireTermination)
			assert.Equal(t, tt.want, result.Termination)
			assert.Equal(t, 1, result.ExitCode)
			if tt.inspectErr != nil || (tt.requireTermination && !tt.absent) {
				require.ErrorIs(t, err, process.ErrTerminationUncertain)
				if tt.inspectErr != nil {
					require.ErrorIs(t, err, queryFailure)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}
