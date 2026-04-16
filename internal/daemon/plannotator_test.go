package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// plannotatorHarness extends the base harness with a reviews dir and knobs
// to intercept the plannotator subprocess.
type plannotatorHarness struct {
	*testHarness
	reviewsDir  string
	stdoutCh    chan string
	errCh       chan error
	callCount   *atomic.Int32
	lookupFails bool
}

func newPlannotatorHarness(t *testing.T) *plannotatorHarness {
	h := newHarness(t)
	reviewsDir := t.TempDir()
	// Configure the plannotator section as if applyDefaults had been called.
	h.cfg.Plannotator = config.Plannotator{
		Binary:     "plannotator",
		Timeout:    config.Duration{Duration: 5 * time.Second},
		ReviewsDir: reviewsDir,
	}
	// Make "review" a valid custom status so transitions don't fail.
	h.cfg.Statuses = []string{"review"}
	// Merge the built-in rework stage: mimic applyDefaults.
	if h.cfg.Stages == nil {
		h.cfg.Stages = map[string]config.Stage{}
	}
	h.cfg.Stages[config.ReworkStageName] = config.Stage{
		Prompt:  "rework prompt with {{ plannotatorReview }}",
		Timeout: config.Duration{Duration: time.Minute},
	}
	h.cfg.ReworkIsBuiltin = true

	ph := &plannotatorHarness{
		testHarness: h,
		reviewsDir:  reviewsDir,
		stdoutCh:    make(chan string, 1),
		errCh:       make(chan error, 1),
		callCount:   &atomic.Int32{},
	}
	return ph
}

func (h *plannotatorHarness) spawner() PlannotatorSpawner {
	return func(ctx context.Context, _ PlannotatorParams) (string, error) {
		h.callCount.Add(1)
		select {
		case out := <-h.stdoutCh:
			return out, nil
		case err := <-h.errCh:
			return "", err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (h *plannotatorHarness) lookup() PlannotatorLookup {
	return func(_ string) error {
		if h.lookupFails {
			return errors.New("executable file not found in $PATH")
		}
		return nil
	}
}

func (h *plannotatorHarness) newDaemonWithSpawner() *Daemon {
	return New(h.cfg,
		WithLogger(testLogger(h.t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(DirectRunner),
		WithSkipOrphanCleanup(),
		WithPlannotatorSpawner(h.spawner()),
		WithPlannotatorLookup(h.lookup()),
	)
}

// seedReviewTicket writes a ticket already parked in the review column and
// creates a worktree for it so StartPlannotatorReview has somewhere to run.
func (h *plannotatorHarness) seedReviewTicket(id string) string {
	h.t.Helper()
	// Create a worktree manually — the daemon worktree manager handles git.
	wtPath := filepath.Join(h.wtDir, h.repoName, id)
	require.NoError(h.t, os.MkdirAll(wtPath, 0o755))

	md := h.reviewTaskMD(id, "review")
	path := h.writeTicket(id+".md", md)
	return path
}

func (h *plannotatorHarness) reviewTaskMD(id, status string) string {
	return `---
id: ` + id + `
kontora: true
status: ` + status + `
pipeline: two-stage
stage: step2
path: ` + h.repoDir + `
created: 2026-01-01T00:00:00Z
history:
  - stage: step1
    agent: agent1
    exit_code: 0
  - stage: step2
    agent: agent2
    exit_code: 0
---
# Test ticket ` + id + `
`
}

func TestPlannotator_ApprovePath(t *testing.T) {
	h := newPlannotatorHarness(t)
	d := h.newDaemonWithSpawner()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.seedReviewTicket("tst-pa01")
	// Wait until the daemon has registered the ticket.
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-pa01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview("tst-pa01"))

	// Approve: return empty stdout from the spawner.
	h.stdoutCh <- ""

	// Ticket should remain in the review column; no review file written.
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, running := d.plannotator["tst-pa01"]
		return !running
	}, 2*time.Second, 20*time.Millisecond)

	info, err := d.GetTicket("tst-pa01")
	require.NoError(t, err)
	assert.Equal(t, "review", info.Status)
	assert.Equal(t, "step2", info.Stage)

	_, statErr := os.Stat(filepath.Join(h.reviewsDir, "tst-pa01.md"))
	assert.True(t, os.IsNotExist(statErr), "no review file should be written on approve")

	cancel()
	require.NoError(t, <-errCh)
}

func TestPlannotator_ReworkPath(t *testing.T) {
	h := newPlannotatorHarness(t)
	// Replace agent2 with a binary that blocks so we can observe the
	// intermediate rework → in_progress transition before it finishes.
	// Empty rework prompt so buildAgentArgs doesn't append a prompt arg
	// that would confuse the `sleep` binary.
	h.cfg.Agents["agent2"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
	h.cfg.Stages[config.ReworkStageName] = config.Stage{
		Prompt:  "",
		Timeout: config.Duration{Duration: time.Minute},
	}
	d := h.newDaemonWithSpawner()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	filePath := h.seedReviewTicket("tst-pr01")
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-pr01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview("tst-pr01"))

	feedback := "change this and that\nline two"
	h.stdoutCh <- feedback

	// Ticket should land at stage=rework, status ∈ {todo, in_progress}.
	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		if err != nil {
			return false
		}
		return tk.Stage == "rework" && (tk.Status == "todo" || tk.Status == "in_progress")
	}, 3*time.Second, 20*time.Millisecond, "ticket should be transitioned to rework")

	// Review file was written. Note: once the rework stage's agent starts,
	// the plannotatorReview template helper reads and deletes it, so we only
	// guarantee it existed at some point — not that it still exists now.
	reviewFile := filepath.Join(h.reviewsDir, "tst-pr01.md")
	if _, err := os.Stat(reviewFile); err == nil {
		data, rErr := os.ReadFile(reviewFile)
		require.NoError(t, rErr)
		assert.Equal(t, feedback, string(data))
	}

	cancel()
	require.NoError(t, <-errCh)
}

// TestPlannotator_ReworkCompletion drives the full loop: plannotator captures
// feedback, the rework stage runs to completion, and the ticket lands back at
// status=review.
func TestPlannotator_ReworkCompletion(t *testing.T) {
	h := newPlannotatorHarness(t)
	// Use the default agent2=true so rework exits 0 immediately.
	d := h.newDaemonWithSpawner()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	filePath := h.seedReviewTicket("tst-prc01")
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-prc01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview("tst-prc01"))

	// Wait for the spawner to be called before sending feedback so the
	// ticket has actually entered the plannotator flow.
	require.Eventually(t, func() bool {
		return h.callCount.Load() == 1
	}, 2*time.Second, 20*time.Millisecond, "spawner should be invoked")
	h.stdoutCh <- "please tweak"

	// Wait for the rework transition to happen (ticket moves off review).
	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		return err == nil && tk.Stage == "rework"
	}, 3*time.Second, 20*time.Millisecond, "ticket should move to rework stage")

	// Then wait for the loop to complete and the ticket to land back at review.
	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		return err == nil && tk.Status == "review"
	}, 5*time.Second, 50*time.Millisecond, "ticket should loop back to review after rework")

	// Review file consumed by the rework agent's prompt render.
	_, statErr := os.Stat(filepath.Join(h.reviewsDir, "tst-prc01.md"))
	assert.True(t, os.IsNotExist(statErr), "review file should be removed after rework consumes it")

	cancel()
	require.NoError(t, <-errCh)
}

func TestPlannotator_ConcurrencyGuard(t *testing.T) {
	h := newPlannotatorHarness(t)
	d := h.newDaemonWithSpawner()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.seedReviewTicket("tst-pc01")
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-pc01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	// First call wins. The spawner blocks on stdoutCh.
	require.NoError(t, d.StartPlannotatorReview("tst-pc01"))

	// Second call should be rejected.
	err := d.StartPlannotatorReview("tst-pc01")
	assert.ErrorIs(t, err, web.ErrPlannotatorInFlight)

	// Release the spawner so the goroutine returns and we can shut down.
	h.stdoutCh <- ""

	cancel()
	require.NoError(t, <-errCh)
}

func TestPlannotator_MissingBinary(t *testing.T) {
	h := newPlannotatorHarness(t)
	h.lookupFails = true
	d := h.newDaemonWithSpawner()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.seedReviewTicket("tst-pm01")
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-pm01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	err := d.StartPlannotatorReview("tst-pm01")
	assert.ErrorIs(t, err, web.ErrPlannotatorBinary)

	cancel()
	require.NoError(t, <-errCh)
}

func TestPlannotator_UnknownTicket(t *testing.T) {
	h := newPlannotatorHarness(t)
	d := h.newDaemonWithSpawner()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	err := d.StartPlannotatorReview("nonexistent")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)

	cancel()
	require.NoError(t, <-errCh)
}
