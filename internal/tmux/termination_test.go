package tmux

import (
	"context"
	"errors"
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
		name             string
		killErr          error
		waitErr          error
		inspectErr       error
		cancel           bool
		hook             bool
		cancelInDecision bool
		wantErr          error
		wantUncertain    bool
		wantCode         int
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
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := RunParams{TicketID: "idle-termination", MinDuration: -1}
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

func TestWindowResultEvidence(t *testing.T) {
	queryFailure := errors.New("inspection failed")
	for _, tt := range []struct {
		name       string
		absent     bool
		inspectErr error
		want       process.TerminationEvidence
	}{
		{name: "window remains", want: process.TerminationUnknown},
		{name: "window gone", absent: true, want: process.TerminationWindowGone},
		{name: "query failed", inspectErr: queryFailure, want: process.TerminationUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tm := newStubTmux()
			tm.noWindow, tm.inspectErr = tt.absent, tt.inspectErr
			result, err := windowResult(tm, 1, time.Now(), nil)
			assert.Equal(t, tt.want, result.Termination)
			assert.Equal(t, 1, result.ExitCode)
			if tt.inspectErr != nil {
				require.ErrorIs(t, err, process.ErrTerminationUncertain)
				require.ErrorIs(t, err, queryFailure)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
