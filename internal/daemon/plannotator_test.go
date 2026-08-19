package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
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
	spawnParams chan PlannotatorParams
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
		spawnParams: make(chan PlannotatorParams, 1),
		callCount:   &atomic.Int32{},
	}
	return ph
}

func (h *plannotatorHarness) spawner() PlannotatorSpawner {
	return func(ctx context.Context, p PlannotatorParams) (string, error) {
		select {
		case h.spawnParams <- p:
		default:
		}
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
	return func(binary string) (string, error) {
		if h.lookupFails {
			return "", errors.New("executable file not found in $PATH")
		}
		return binary, nil
	}
}

func (h *plannotatorHarness) newDaemonWithSpawner(opts ...Option) *Daemon {
	base := make([]Option, 0, 7+len(opts))
	base = append(base,
		WithLogger(testLogger(h.t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(DirectRunner),
		WithSkipOrphanCleanup(),
		WithPlannotatorSpawner(h.spawner()),
		WithPlannotatorLookup(h.lookup()),
	)
	return New(h.cfg, append(base, opts...)...)
}

// seedReviewTicket writes a ticket already parked in the human_review column
// and creates a real git worktree with one commit ahead of main — matching
// what kontora produces in production when an agent finishes its work.
// The worktree is what the rework stage would pick up; the review worktree
// is derived independently by setupPlannotatorWorktree.
func (h *plannotatorHarness) seedReviewTicket(id string) string {
	h.t.Helper()
	h.seedReviewWorktree(id)
	return h.writeTicket(id+".md", h.reviewTaskMD(id, "human_review", "kontora/"+id))
}

// seedReviewWorktree creates the branch, worktree, and agent commit half of
// seedReviewTicket, for tests that write their own ticket file.
func (h *plannotatorHarness) seedReviewWorktree(id string) {
	h.t.Helper()
	wtPath := filepath.Join(h.wtDir, h.repoName, id)
	require.NoError(h.t, os.MkdirAll(filepath.Dir(wtPath), 0o755))

	branch := "kontora/" + id
	for _, args := range [][]string{
		{"worktree", "add", "-b", branch, wtPath, "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = h.repoDir
		out, err := cmd.CombinedOutput()
		require.NoError(h.t, err, "git %v: %s", args, out)
	}
	// Simulate an agent commit so plannotator has a real diff to review.
	require.NoError(h.t, os.WriteFile(filepath.Join(wtPath, id+".txt"), []byte("agent work\n"), 0o644))
	for _, args := range [][]string{
		{"add", id + ".txt"},
		{"commit", "-m", "agent: work on " + id},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		out, err := cmd.CombinedOutput()
		require.NoError(h.t, err, "git %v: %s", args, out)
	}
}

func (h *plannotatorHarness) reviewTaskMD(id, status, branch string) string {
	return `---
id: ` + id + `
kontora: true
status: ` + status + `
pipeline: two-stage
stage: step2
path: ` + h.repoDir + `
branch: ` + branch + `
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

// TestPlannotator_NoStateChangePaths covers the stdout values that must leave
// the ticket parked in human_review: empty output, the explicit approval
// marker, and the cancel-without-feedback marker. Each case asserts both that
// the ticket did not move and that the broadcast outcome matches.
func TestPlannotator_NoStateChangePaths(t *testing.T) {
	cases := []struct {
		name        string
		stdout      string
		wantOutcome string
	}{
		{name: "empty stdout", stdout: "", wantOutcome: web.PlannotatorOutcomeApproved},
		{name: "approved marker", stdout: plannotatorApprovedMarker + "\n", wantOutcome: web.PlannotatorOutcomeApproved},
		{name: "cancelled marker", stdout: plannotatorCancelledMarker + "\n", wantOutcome: web.PlannotatorOutcomeCancelled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlannotatorHarness(t)
			d := h.newDaemonWithSpawner()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			events, unsub := d.Subscribe()
			defer unsub()

			h.seedReviewTicket("tst-pn01")
			require.Eventually(t, func() bool {
				_, err := d.GetTicket("tst-pn01")
				return err == nil
			}, 2*time.Second, 20*time.Millisecond)

			require.NoError(t, d.StartPlannotatorReview("tst-pn01"))
			h.stdoutCh <- tc.stdout

			// Wait for the spawn goroutine to finish before inspecting state.
			require.Eventually(t, func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				_, running := d.plannotator["tst-pn01"]
				return !running
			}, 2*time.Second, 20*time.Millisecond)

			info, err := d.GetTicket("tst-pn01")
			require.NoError(t, err)
			assert.Equal(t, "human_review", info.Status)
			assert.Equal(t, "step2", info.Stage)

			_, statErr := os.Stat(filepath.Join(h.reviewsDir, "tst-pn01.md"))
			assert.True(t, os.IsNotExist(statErr), "no review file should be written")

			// Drain events until we see the finished broadcast for this ticket.
			deadline := time.After(2 * time.Second)
			var got string
		loop:
			for {
				select {
				case ev := <-events:
					if ev.Type == "plannotator_finished" && ev.Ticket.ID == "tst-pn01" {
						got = ev.Outcome
						break loop
					}
				case <-deadline:
					t.Fatal("timed out waiting for plannotator_finished event")
				}
			}
			assert.Equal(t, tc.wantOutcome, got)

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

// TestPlannotator_ReworkClaim asserts the rework pickup writes claimed_by in
// the same in_progress transition, matching the pipeline and simple paths.
func TestPlannotator_ReworkClaim(t *testing.T) {
	h := newPlannotatorHarness(t)
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

	filePath := h.seedReviewTicket("tst-rc01")
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-rc01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview("tst-rc01"))
	h.stdoutCh <- "please fix this"

	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		return err == nil && tk.Stage == "rework" &&
			tk.Status == ticket.StatusInProgress && tk.ClaimedBy == "test-instance"
	}, 3*time.Second, 20*time.Millisecond, "rework pickup should claim for test-instance")

	cancel()
	require.NoError(t, <-errCh)
}

// TestPlannotator_ReworkExitForeignClaimGuard covers the rework exit guard: a
// custom runner claims the ticket for another instance mid-run (suppressing the
// watcher event), so the exit path handles it. The guard must leave the ticket
// in_progress and foreign-claimed — not routed to human_review, no new history.
func TestPlannotator_ReworkExitForeignClaimGuard(t *testing.T) {
	h := newPlannotatorHarness(t)
	h.cfg.Stages[config.ReworkStageName] = config.Stage{
		Prompt:  "",
		Timeout: config.Duration{Duration: time.Minute},
	}

	ticketPath := filepath.Join(h.tasksDir, "tst-rg01.md")
	var d *Daemon
	runner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		if tk, err := ticket.ParseFile(ticketPath); err == nil {
			_ = tk.SetField("claimed_by", "other-host")
			if data, mErr := tk.Marshal(); mErr == nil {
				d.recordSelfWrite(ticketPath, data)
				_ = os.WriteFile(ticketPath, data, 0o644)
			}
		}
		return process.Result{ExitCode: 0, ExitedAt: time.Now()}, nil
	}
	d = New(h.cfg,
		WithLogger(testLogger(h.t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithSkipOrphanCleanup(),
		WithPlannotatorSpawner(h.spawner()),
		WithPlannotatorLookup(h.lookup()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.seedReviewTicket("tst-rg01")
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-rg01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview("tst-rg01"))
	h.stdoutCh <- "please fix this"

	// Parse directly rather than through h.readTask: the runner writes this
	// file with os.WriteFile, which truncates in place, so a poll can catch it
	// half-written. That is a retry, not a failure.
	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(ticketPath)
		return err == nil && tk.ClaimedBy == "other-host"
	}, 5*time.Second, 20*time.Millisecond, "rework runner should claim for other-host")
	waitForAgentsDone(t, d, 5*time.Second)

	got := h.readTask("tst-rg01.md")
	assert.Equal(t, "other-host", got.ClaimedBy)
	assert.Equal(t, ticket.StatusInProgress, got.Status, "rework guard must not route to human_review")
	assert.Len(t, got.History, 2, "rework guard must not append history")

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
	reader := sdkmetric.NewManualReader()
	d := h.newDaemonWithSpawner(WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))

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

	// Then wait for the loop to complete and the ticket to land back at human_review.
	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		return err == nil && tk.Status == "human_review"
	}, 5*time.Second, 50*time.Millisecond, "ticket should loop back to human_review after rework")

	// Review file consumed by the rework agent's prompt render.
	_, statErr := os.Stat(filepath.Join(h.reviewsDir, "tst-prc01.md"))
	assert.True(t, os.IsNotExist(statErr), "review file should be removed after rework consumes it")

	cancel()
	require.NoError(t, <-errCh)

	// The rework stage calls the runner directly, bypassing spawnAgentRun, so
	// it needs its own record. Collected after the daemon stopped.
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	byName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}

	runs, ok := byName["kontora.stage.runs"]
	require.True(t, ok, "the rework run must be counted")
	assert.Equal(t, map[string]int64{config.ReworkStageName: 1}, sumByAttr(t, runs, "stage"))
	assert.Equal(t, map[string]int64{"success": 1}, sumByAttr(t, runs, "outcome"))

	dur, ok := byName["kontora.stage.duration"]
	require.True(t, ok, "the rework run must record a duration")
	hist := dur.Data.(metricdata.Histogram[float64])
	require.Len(t, hist.DataPoints, 1)
	assert.Equal(t, uint64(1), hist.DataPoints[0].Count, "exactly one sample for the same run")
}

// TestPlannotator_ReworkStageModel: the rework stage spawns its agent itself
// instead of going through runAgentOnce, so the stage model and effort have to
// be resolved on that path too.
func TestPlannotator_ReworkStageModel(t *testing.T) {
	const id = "tst-prm01"
	h := newPlannotatorHarness(t)
	h.cfg.Agents["agent2"] = config.Agent{Binary: "claude", Args: []string{"--model", "opus"}}
	h.cfg.Stages[config.ReworkStageName] = config.Stage{
		Prompt:  "rework prompt with {{ plannotatorReview }}",
		Timeout: config.Duration{Duration: time.Minute},
		Model:   config.PerAgent{ByAgent: map[string]string{"claude": "haiku"}},
		Effort:  config.PerAgent{ByAgent: map[string]string{"claude": "xhigh"}},
	}

	var runs annotationRun
	d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	filePath := h.seedReviewTicket(id)
	require.Eventually(t, func() bool {
		_, err := d.GetTicket(id)
		return err == nil
	}, 3*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview(id))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)
	h.stdoutCh <- "please tweak"

	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		return err == nil && tk.Status == "human_review" && len(runs.all()) == 1
	}, 10*time.Second, 50*time.Millisecond, "the rework run should finish")

	args := runs.all()[0].Args
	assert.Equal(t, "haiku", modelArg(t, args))
	assert.Equal(t, "xhigh", flagArg(t, args, "--effort"))

	// Rework resolves the pair on its own path, so it records its own.
	tk, err := ticket.ParseFile(filePath)
	require.NoError(t, err)
	require.NotEmpty(t, tk.History)
	rework := tk.History[len(tk.History)-1]
	assert.Equal(t, config.ReworkStageName, rework.Stage)
	assert.Equal(t, "haiku", rework.Model)
	assert.Equal(t, "xhigh", rework.Effort)

	cancel()
	require.NoError(t, <-errCh)
}

func TestPlannotatorReworkDisablesCheckpointCompaction(t *testing.T) {
	const id = "tst-prc02"
	h := newPlannotatorHarness(t)
	h.cfg.Agents["agent2"] = config.Agent{
		Binary:                     "pi",
		CheckpointCompactionTokens: 150000,
	}

	var runs annotationRun
	var extensionContent string
	d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		i := slices.Index(p.Args, "-e")
		require.GreaterOrEqual(t, i, 0)
		data, err := os.ReadFile(p.Args[i+1])
		require.NoError(t, err)
		extensionContent = string(data)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	filePath := h.seedReviewTicket(id)
	require.Eventually(t, func() bool {
		_, err := d.GetTicket(id)
		return err == nil
	}, 3*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview(id))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 }, 3*time.Second, 20*time.Millisecond)
	h.stdoutCh <- "please tweak"
	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		return err == nil && tk.Status == "human_review" && len(runs.all()) == 1
	}, 10*time.Second, 50*time.Millisecond)

	assert.Contains(t, extensionContent, "const THRESHOLD = 150000;")
	assert.Contains(t, extensionContent, "const ENABLED = false;")
	assert.Contains(t, extensionContent, "agent_settled")
	assert.NotContains(t, renderedPrompt(runs.all()[0]), "## Phase checkpoints")

	cancel()
	require.NoError(t, <-errCh)
}

// TestPlannotator_ReworkFinalSummary covers the ticket-level summary across a
// review round: the stale text is dropped before the rework agent starts, the
// rework run records its own summary, and the ticket-level one is regenerated
// from the history the round produced.
func TestPlannotator_ReworkFinalSummary(t *testing.T) {
	h := newPlannotatorHarness(t)
	h.cfg.Agents["agent2"] = config.Agent{Binary: "claude"}
	filePath := filepath.Join(h.tasksDir, "tst-prf01.md")

	duringRework := make(chan string, 2)
	runner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		tk, err := ticket.ParseFile(filePath)
		require.NoError(t, err)
		duringRework <- tk.FinalSummary
		require.NoError(t, tk.SetField("summary", "reworked the review comments"))
		data, mErr := tk.Marshal()
		require.NoError(t, mErr)
		require.NoError(t, os.WriteFile(filePath, data, 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	var prompt string
	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
		WithPlannotatorSpawner(h.spawner()),
		WithPlannotatorLookup(h.lookup()),
		WithFinalSummarySpawner(func(_ context.Context, p FinalSummaryParams) (string, error) {
			prompt = p.Args[len(p.Args)-1]
			return "the whole ticket after rework", nil
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// A reviewed ticket whose earlier stages already recorded summaries, and a
	// ticket-level summary written for the state before this review round.
	md := strings.Replace(h.reviewTaskMD("tst-prf01", "human_review", "kontora/tst-prf01"),
		"    exit_code: 0\n  - stage: step2", "    exit_code: 0\n    summary: planned it\n  - stage: step2", 1)
	md = strings.Replace(md, "---\n# Test ticket", "    summary: coded it\nfinal_summary: stale ticket-level text\n---\n# Test ticket", 1)
	// A body, not just a title: the pass drops the ticket block for a ticket
	// that has nothing to state a goal from.
	md += "\nRework the review comments.\n"
	h.seedReviewWorktree("tst-prf01")
	h.writeTicket("tst-prf01.md", md)

	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-prf01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview("tst-prf01"))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		2*time.Second, 20*time.Millisecond, "spawner should be invoked")
	h.stdoutCh <- "please tweak"

	assert.Empty(t, <-duringRework, "the stale ticket-level summary must be gone before rework starts")

	result := h.waitForFinalSummary("tst-prf01.md", "the whole ticket after rework", 10*time.Second)
	assert.Equal(t, ticket.StatusHumanReview, result.Status)
	assert.Equal(t, "reworked the review comments", result.Summary)
	require.Len(t, result.History, 3)
	assert.Equal(t, "planned it", result.History[0].Summary)
	assert.Equal(t, "coded it", result.History[1].Summary)
	assert.Equal(t, config.ReworkStageName, result.History[2].Stage)
	assert.Equal(t, "reworked the review comments", result.History[2].Summary)
	assert.Contains(t, prompt, "reworked the review comments", "the rework run must be in the input")
	assert.Contains(t, prompt, "Test ticket tst-prf01", "the ticket text must reach the pass on the rework path too")
	assert.Contains(t, prompt, "Rework the review comments.")

	cancel()
	require.NoError(t, <-errCh)
}

// TestPlannotator_ReloadKeepsStartingSettings pins the plannotator settings to
// the values the review started with: a reload while the subprocess runs must
// not move the review file to the new reviews_dir.
func TestPlannotator_ReloadKeepsStartingSettings(t *testing.T) {
	h := newPlannotatorHarness(t)
	// Empty rework prompt so the plannotatorReview helper does not consume the
	// review file before the assertion, and a rework agent that blocks.
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

	filePath := h.seedReviewTicket("tst-prl01")
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-prl01")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	require.NoError(t, d.StartPlannotatorReview("tst-prl01"))

	var params PlannotatorParams
	select {
	case params = <-h.spawnParams:
	case <-time.After(2 * time.Second):
		t.Fatal("spawner should be invoked")
	}
	assert.Equal(t, 5*time.Second, params.Timeout)

	// Reload to a config pointing elsewhere while the subprocess still runs.
	movedDir := t.TempDir()
	next := *h.cfg
	next.Plannotator.ReviewsDir = movedDir
	next.Plannotator.Timeout = config.Duration{Duration: time.Hour}
	d.cfg.Store(&next)

	feedback := "please tweak"
	h.stdoutCh <- feedback

	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(filePath)
		return err == nil && tk.Stage == "rework"
	}, 3*time.Second, 20*time.Millisecond, "ticket should move to rework stage")

	data, err := os.ReadFile(filepath.Join(h.reviewsDir, "tst-prl01.md"))
	require.NoError(t, err, "review should land in the dir the run started with")
	assert.Equal(t, feedback, string(data))
	assert.NoFileExists(t, filepath.Join(movedDir, "tst-prl01.md"))

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

	// Wait until setup has completed and the goroutine is blocked in the spawner.
	require.Eventually(t, func() bool {
		return h.callCount.Load() == 1
	}, 2*time.Second, 20*time.Millisecond, "spawner should be invoked")

	// Release the spawner and wait for cleanup before the test temp dirs go away.
	h.stdoutCh <- ""
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, running := d.plannotator["tst-pc01"]
		return !running
	}, 2*time.Second, 20*time.Millisecond, "plannotator should finish")

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

func TestDefaultPlannotatorLookup(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "plannotator-fake")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	cases := []struct {
		name    string
		binary  string
		wantErr bool
		want    string
	}{
		{name: "empty", binary: "", wantErr: true},
		{name: "absolute existing", binary: bin, want: bin},
		{name: "absolute missing", binary: filepath.Join(dir, "nope"), wantErr: true},
		{name: "relative not found", binary: "definitely-not-a-real-binary-xyz", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := defaultPlannotatorLookup(tc.binary)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSetupPlannotatorWorktree covers the disposable-worktree path: the
// function creates a detached checkout at the merge-base and applies the
// branch's diff on top. The review worktree should contain the agent's
// committed work as pending changes (exact staged/unstaged split is a git
// detail we don't pin), and `git diff HEAD` should match what plannotator
// would show against the base.
func TestSetupPlannotatorWorktree(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, repo, wt string)
		// base is the ticket's base_branch. Empty means the repo default.
		base string
		// expectedFiles maps paths (relative to the review worktree) to their
		// expected content on disk after setup.
		expectedFiles map[string]string
		// expectChangedFiles is the exact set of paths the review worktree
		// presents as pending changes. Files that are committed at the
		// merge-base exist on disk but must not appear here.
		expectChangedFiles []string
		// expectDiffEmpty asserts `git diff HEAD` is empty — used for the
		// no-commits-ahead case where the review worktree should match base.
		expectDiffEmpty bool
	}{
		{
			name: "single committed file shows up in review worktree",
			setup: func(t *testing.T, _, wt string) {
				commitFile(t, wt, "hello.txt", "world\n", "add hello")
			},
			expectedFiles: map[string]string{"hello.txt": "world\n"},
		},
		{
			name: "multiple commits flatten into one diff",
			setup: func(t *testing.T, _, wt string) {
				commitFile(t, wt, "a.txt", "one\n", "c1")
				commitFile(t, wt, "b.txt", "two\n", "c2")
				commitFile(t, wt, "a.txt", "one-updated\n", "c3")
			},
			expectedFiles: map[string]string{
				"a.txt": "one-updated\n",
				"b.txt": "two\n",
			},
		},
		{
			name: "modifying a file that exists at base",
			setup: func(t *testing.T, repo, wt string) {
				// Seed main with a file, then fast-forward feature so the
				// merge-base contains it. Modifying it on feature should come
				// through as a change — not a new file.
				commitFile(t, repo, "base.txt", "original\n", "base")
				mustGit(t, wt, "reset", "--hard", "main")
				commitFile(t, wt, "base.txt", "changed\n", "modify base")
				mustGit(t, repo, "update-ref", "refs/remotes/origin/main", "main")
			},
			expectedFiles: map[string]string{"base.txt": "changed\n"},
		},
		{
			name:            "no commits ahead of base yields clean review worktree",
			setup:           func(*testing.T, string, string) {},
			expectDiffEmpty: true,
		},
		{
			name: "branch exists without a registered worktree",
			setup: func(t *testing.T, repo, wt string) {
				commitFile(t, wt, "orphan.txt", "kept\n", "commit then drop worktree")
				// Drop the branch's worktree. The branch itself still points at
				// the commit, which is the scenario we want to cover.
				mustGit(t, repo, "worktree", "remove", "--force", wt)
			},
			expectedFiles: map[string]string{"orphan.txt": "kept\n"},
		},
		{
			// main -> A, develop -> A+D1, feature -> A+D1+T1. Measured against
			// develop the review shows only T1. Measured against main it would
			// also show D1, presenting develop's commits as the agent's work.
			name: "base branch diverged from the default branch",
			setup: func(t *testing.T, repo, wt string) {
				commitFile(t, wt, "d.txt", "d1\n", "D1")
				mustGit(t, repo, "branch", "develop", "feature")
				commitFile(t, wt, "t.txt", "t1\n", "T1")
			},
			base:               "develop",
			expectedFiles:      map[string]string{"t.txt": "t1\n"},
			expectChangedFiles: []string{"t.txt"},
		},
		{
			// The same history with no base_branch falls back to main, so both
			// commits show up. This pins the fallback the case above overrides.
			name: "no base branch falls back to the default branch",
			setup: func(t *testing.T, repo, wt string) {
				commitFile(t, wt, "d.txt", "d1\n", "D1")
				mustGit(t, repo, "branch", "develop", "feature")
				commitFile(t, wt, "t.txt", "t1\n", "T1")
			},
			expectedFiles:      map[string]string{"d.txt": "d1\n", "t.txt": "t1\n"},
			expectChangedFiles: []string{"d.txt", "t.txt"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, wt := setupRealGitWorktree(t)
			tc.setup(t, repo, wt)

			reviewPath := filepath.Join(t.TempDir(), "review.plannotator")
			reviewPath, cleanup, err := setupPlannotatorWorktree(testLogger(t), repo, "feature", tc.base, reviewPath)
			require.NoError(t, err)
			t.Cleanup(cleanup)

			for rel, want := range tc.expectedFiles {
				got, rErr := os.ReadFile(filepath.Join(reviewPath, rel))
				require.NoError(t, rErr, "read %s", rel)
				assert.Equal(t, want, string(got), "file %s", rel)
			}
			if tc.expectChangedFiles != nil {
				assert.ElementsMatch(t, tc.expectChangedFiles, changedFiles(t, reviewPath),
					"pending changes in the review worktree")
			}

			// The review worktree's HEAD is the merge-base; any applied diff
			// should surface via `git diff HEAD` (which covers staged +
			// unstaged + untracked via the `--` spec). Plannotator's default
			// view reads this same data, so this check mirrors what the UI
			// would show the user.
			out, gErr := runGit(reviewPath, "diff", "HEAD", "--", ".")
			require.NoError(t, gErr)
			untracked, gErr := runGit(reviewPath, "ls-files", "--others", "--exclude-standard")
			require.NoError(t, gErr)
			totalDiff := strings.TrimSpace(out) + strings.TrimSpace(untracked)
			if tc.expectDiffEmpty {
				assert.Empty(t, totalDiff, "expected clean review worktree")
			} else {
				assert.NotEmpty(t, totalDiff, "expected changes to be visible in review worktree")
			}
		})
	}
}

// TestSetupPlannotatorWorktree_CleanupIsIdempotent verifies we can call the
// cleanup twice (once explicitly, once by deferred path) without the second
// call failing — important because the caller uses `defer cleanup()` after
// already invoking it on error.
func TestSetupPlannotatorWorktree_CleanupIsIdempotent(t *testing.T) {
	repo, wt := setupRealGitWorktree(t)
	commitFile(t, wt, "x.txt", "y\n", "c")

	reviewPath := filepath.Join(t.TempDir(), "review.plannotator")
	reviewPath, cleanup, err := setupPlannotatorWorktree(testLogger(t), repo, "feature", "", reviewPath)
	require.NoError(t, err)
	cleanup()
	// Directory gone after first cleanup.
	_, err = os.Stat(reviewPath)
	assert.True(t, os.IsNotExist(err))
	// Second call must not panic or error fatally.
	cleanup()
}

// setupRealGitWorktree creates a real git repo on `main` and a separate
// working worktree on branch `feature` both rooted at the returned paths.
// The worktree starts at the same commit as main.
func setupRealGitWorktree(t *testing.T) (repo, wt string) {
	t.Helper()
	repo = initRepo(t)
	// origin/main is what DetectDefaultBranch prefers. Set it up with a fake
	// remote so that resolution path is exercised.
	mustGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	mustGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	wt = filepath.Join(t.TempDir(), "wt")
	mustGit(t, repo, "worktree", "add", "-b", "feature", wt, "main")
	return repo, wt
}

// changedFiles returns the paths the review worktree presents as pending
// changes: tracked modifications against HEAD plus untracked files. This is the
// same data plannotator's default view reads.
func changedFiles(t *testing.T, reviewPath string) []string {
	t.Helper()
	sources := [][]string{
		{"diff", "HEAD", "--name-only", "--", "."},
		{"ls-files", "--others", "--exclude-standard"},
	}
	files := make([]string, 0, len(sources))
	for _, args := range sources {
		out, err := runGit(reviewPath, args...)
		require.NoError(t, err)
		files = append(files, strings.Fields(out)...)
	}
	return files
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	mustGit(t, dir, "add", name)
	mustGit(t, dir, "commit", "-m", msg)
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}
