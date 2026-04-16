package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	return func(binary string) (string, error) {
		if h.lookupFails {
			return "", errors.New("executable file not found in $PATH")
		}
		return binary, nil
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

// seedReviewTicket writes a ticket already parked in the human_review column
// and creates a real git worktree with one commit ahead of main — matching
// what kontora produces in production when an agent finishes its work.
// setupPlannotatorWorktree needs a real worktree to diff/apply against.
func (h *plannotatorHarness) seedReviewTicket(id string) string {
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

	md := h.reviewTaskMD(id, "human_review", branch)
	path := h.writeTicket(id+".md", md)
	return path
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
		// expectedFiles maps paths (relative to the review worktree) to their
		// expected content on disk after setup.
		expectedFiles map[string]string
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, wt := setupRealGitWorktree(t)
			tc.setup(t, repo, wt)

			reviewPath, cleanup, err := setupPlannotatorWorktree(testLogger(t), repo, wt)
			require.NoError(t, err)
			t.Cleanup(cleanup)

			for rel, want := range tc.expectedFiles {
				got, rErr := os.ReadFile(filepath.Join(reviewPath, rel))
				require.NoError(t, rErr, "read %s", rel)
				assert.Equal(t, want, string(got), "file %s", rel)
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

	reviewPath, cleanup, err := setupPlannotatorWorktree(testLogger(t), repo, wt)
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
