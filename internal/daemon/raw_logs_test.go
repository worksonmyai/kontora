package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestDaemon_RawCaptureAcrossLegacyRetry(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("false", "true")
	const id, stage = "tst-raw-retry", "plan..review"
	cfg.Stages[stage] = cfg.Stages["step1"]
	cfg.Pipelines["retry-stage"].Legacy[0].Stage = stage
	started, written := make(chan string, 2), make(chan string, 2)
	write, finish := make(chan struct{}), make(chan struct{})
	var attempts int
	d := h.newDaemon(cfg, WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
		attempts++
		text := fmt.Sprintf("attempt %d output\n", attempts)
		started <- text
		select {
		case <-write:
		case <-ctx.Done():
			return process.Result{}, ctx.Err()
		}
		f, err := os.OpenFile(p.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return process.Result{}, err
		}
		_, err = f.WriteString(text)
		if err := errors.Join(err, f.Close()); err != nil {
			return process.Result{}, err
		}
		written <- text
		select {
		case <-finish:
		case <-ctx.Done():
			return process.Result{}, ctx.Err()
		}
		now := time.Now()
		return process.Result{ExitCode: 1, StartedAt: now, ExitedAt: now}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	defer func() { cancel(); assert.NoError(t, <-errCh) }()
	h.writeTicket(id+".md", h.taskMD(id, "todo", "retry-stage"))
	await := func(ch <-chan string) string {
		t.Helper()
		select {
		case s := <-ch:
			return s
		case <-ctx.Done():
			t.Fatal("runner did not reach the barrier")
			return ""
		}
	}
	exact := func(d *Daemon, run int, want string) {
		t.Helper()
		got, err := d.GetLogs(web.LogQuery{ID: id, Stage: stage, Run: &run})
		if want == "" {
			require.ErrorIs(t, err, web.ErrLogNotFound)
		} else {
			require.NoError(t, err)
		}
		assert.Equal(t, want, got)
	}
	for run := range 2 {
		want := await(started)
		exact(d, run, "")
		if run > 0 {
			exact(d, 0, "attempt 1 output\n")
		}
		write <- struct{}{}
		assert.Equal(t, want, await(written))
		exact(d, run, want)
		finish <- struct{}{}
	}
	result := h.waitForStatus(id+".md", ticket.StatusPaused, 5*time.Second)
	require.Len(t, result.History, 2)
	// Read from a fresh ticket index to exercise the persisted run binding.
	reloaded := h.newDaemon(cfg)
	reloaded.tickets[id] = newTicketState(result, filepath.Join(h.tasksDir, id+".md"))
	exact(reloaded, 0, "attempt 1 output\n")
	exact(reloaded, 1, "attempt 2 output\n")
	latest, err := reloaded.GetLogs(web.LogQuery{ID: id, Stage: stage, StageOnly: true})
	require.NoError(t, err)
	assert.Equal(t, "attempt 2 output\n", latest)
}

func TestDaemon_GetLogsNamedStageDoesNotFallBack(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	const id = "tst-raw-latest"
	path := h.writeTicket(id+".md", h.taskMD(id, "done", "one-stage"))
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets[id] = newTicketState(tk, path)
	q := web.LogQuery{ID: id, Stage: "missing", StageOnly: true}
	_, err = d.GetLogs(q)
	assert.ErrorIs(t, err, web.ErrLogNotFound)
	other := session.LogPath(h.logsDir, id, "step1")
	require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o755))
	require.NoError(t, os.WriteFile(other, []byte("other stage"), 0o600))
	_, err = d.GetLogs(q)
	assert.ErrorIs(t, err, web.ErrLogNotFound)
	q.StageOnly = false
	got, err := d.GetLogs(q)
	require.NoError(t, err)
	assert.Equal(t, "other stage", got)
}
