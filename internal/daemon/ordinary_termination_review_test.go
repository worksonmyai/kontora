package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestOrdinaryUncertainTerminationRetainsAdmission(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.MaxConcurrentAgents = 1
	calls := make(chan RunnerParams, 2)
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls <- p
		now := time.Now()
		return process.Result{StartedAt: now, ExitedAt: now, ExitCode: -1}, process.ErrTerminationUncertain
	}
	d := h.newDaemon(cfg, WithRunner(runner))
	h.writeTicket("tst-uncertain.md", h.taskMD("tst-uncertain", "todo", "one-stage"))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
	var first RunnerParams
	select {
	case first = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("first runner did not start")
	}
	path := h.writeTicket("tst-next.md", h.taskMD("tst-next", "todo", "one-stage"))
	d.handleFileChanged(path)
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.queued["tst-next"] && len(d.sem) == 1 && len(d.runningBranches) == 1 && len(d.liveRuns) == 1
	}, 5*time.Second, 10*time.Millisecond)
	select {
	case next := <-calls:
		t.Fatalf("replacement runner started after uncertain termination: %s", next.TicketID)
	case <-time.After(100 * time.Millisecond):
	}
	d.mu.Lock()
	_, retained := d.running["tst-uncertain"]
	d.mu.Unlock()
	require.True(t, retained)
	d.cleanupWorktree(testLogger(t), h.readTask("tst-uncertain.md"))
	_, err := os.Stat(first.Dir)
	require.NoError(t, err)
}
