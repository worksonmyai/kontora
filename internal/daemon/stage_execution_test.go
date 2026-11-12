package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestStageChangeWaitsForTeardown(t *testing.T) {
	h := newHarness(t)
	started, stopping, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var firstRuns, secondRuns atomic.Int32
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	d := h.newDaemon(h.cfg, WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
		now := time.Now()
		if p.Env[config.StageEnvVar] == "step1" {
			firstRuns.Add(1)
			close(started)
			<-ctx.Done()
			close(stopping)
			<-released
			return process.Result{StartedAt: now, ExitedAt: time.Now(), ExitCode: -1, Termination: process.TerminationProcessGroupGone}, ctx.Err()
		}
		secondRuns.Add(1)
		publishCompleteResult(t, p)
		return process.Result{StartedAt: now, ExitedAt: time.Now(), Termination: process.TerminationProcessGroupGone}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	defer func() { release(); cancel(); require.NoError(t, <-errCh) }()
	h.writeTicket("tst-stop.md", h.taskMD("tst-stop", "todo", "two-stage"))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first stage did not start")
	}
	require.ErrorContains(t, d.SkipStage("tst-stop"), "pause first")
	require.NoError(t, d.PauseTicket("tst-stop"))
	select {
	case <-stopping:
	case <-ctx.Done():
		t.Fatal("first stage was not cancelled")
	}
	require.Equal(t, ticket.StatusPaused, h.readTask("tst-stop.md").Status)
	require.ErrorContains(t, d.SkipStage("tst-stop"), "pause first")
	require.ErrorContains(t, d.SetStage("tst-stop", "step2"), "pause first")
	release()
	waitForAgentsDone(t, d, 5*time.Second)
	require.NoError(t, d.SkipStage("tst-stop"))
	done := h.waitForStatus("tst-stop.md", ticket.StatusDone, 5*time.Second)
	waitForAgentsDone(t, d, 5*time.Second)
	assert.EqualValues(t, 1, firstRuns.Load())
	assert.EqualValues(t, 1, secondRuns.Load())
	require.Len(t, done.History, 2)
	assert.Equal(t, "step1", done.History[0].Stage)
	assert.Equal(t, ticket.RunOutcomeFailed, done.History[0].RunOutcome)
	assert.Contains(t, done.History[0].OutcomeReason, "cancelled by operator")
	assert.Equal(t, "step2", done.History[1].Stage)
	assert.Equal(t, ticket.RunOutcomeComplete, done.History[1].RunOutcome)
}
