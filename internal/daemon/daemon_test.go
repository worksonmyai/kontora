package daemon

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/testutil"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		t.Log(strings.TrimRight(string(p), "\n"))
		return len(p), nil
	}), nil))
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// testHarness sets up temp dirs, a real git repo, and an in-memory config.
type testHarness struct {
	t           *testing.T
	tasksDir    string
	wtDir       string
	logsDir     string
	lockPath    string
	repoDir     string
	repoName    string
	tmuxSession string
	cfg         *config.Config
}

// tmuxNameUnsafe matches what a tmux session name cannot hold. Subtest names
// arrive with "/" in them, and tmux reads "." and ":" as target separators.
var tmuxNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	tasksDir := t.TempDir()
	wtDir := t.TempDir()
	logsDir := t.TempDir()
	lockDir := t.TempDir()
	repoDir := initRepo(t)

	// Own session per harness, so a test that really spawns tmux can never
	// address a developer's running agents, and the cleanup below can never
	// tear down a sibling test's session. The name comes from the test name
	// because t.TempDir's last element is "001" for every test.
	session := fmt.Sprintf("kontora-test-%d-%s", os.Getpid(), tmuxNameUnsafe.ReplaceAllString(t.Name(), "-"))
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+session).Run() })

	h := &testHarness{
		t:           t,
		tasksDir:    tasksDir,
		wtDir:       wtDir,
		logsDir:     logsDir,
		lockPath:    filepath.Join(lockDir, "lock"),
		repoDir:     repoDir,
		repoName:    filepath.Base(repoDir),
		tmuxSession: session,
	}
	h.cfg = h.defaultConfig("true", "true")
	return h
}

func (h *testHarness) defaultConfig(agent1Binary, agent2Binary string) *config.Config {
	return &config.Config{
		TicketsDir:          h.tasksDir,
		BranchPrefix:        "kontora",
		WorktreesDir:        h.wtDir,
		LogsDir:             h.logsDir,
		DefaultAgent:        "agent1",
		MaxConcurrentAgents: 4,
		AutoPickUp:          new(true),
		InstanceName:        "test-instance",
		TmuxSession:         h.tmuxSession,
		Agents: map[string]config.Agent{
			"agent1": {Binary: agent1Binary},
			"agent2": {Binary: agent2Binary},
		},
		Stages: map[string]config.Stage{
			"step1": {Prompt: "do step1 for {{ .Ticket.ID }}"},
			"step2": {Prompt: "do step2 for {{ .Ticket.ID }}"},
		},
		Pipelines: map[string]config.Pipeline{
			"two-stage": {
				{Stage: "step1", Agent: "agent1", OnSuccess: "next", OnFailure: "pause"},
				{Stage: "step2", Agent: "agent2", OnSuccess: "done", OnFailure: "pause"},
			},
			"one-stage": {
				{Stage: "step1", Agent: "agent1", OnSuccess: "done", OnFailure: "pause"},
			},
			"retry-stage": {
				{Stage: "step1", Agent: "agent1", OnSuccess: "done", OnFailure: "retry", MaxRetries: 1},
			},
		},
	}
}

func (h *testHarness) newDaemon(cfg *config.Config, opts ...Option) *Daemon {
	base := make([]Option, 0, 6+len(opts))
	base = append(base,
		WithLogger(testLogger(h.t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(DirectRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)
	return New(cfg, append(base, opts...)...)
}

// newMetricsDaemon returns a daemon recording into a ManualReader, plus a
// collect function keyed by metric name.
//
// Collect only after the daemon has stopped (cancel(); <-errCh). Reading while
// it runs races the goroutines still recording, which -race reports.
func (h *testHarness) newMetricsDaemon(cfg *config.Config, opts ...Option) (*Daemon, func() map[string]metricdata.Metrics) {
	h.t.Helper()
	mp, collect := h.manualMetrics()
	return h.newDaemon(cfg, append(opts, WithMeterProvider(mp))...), collect
}

// manualMetrics returns a provider backed by a ManualReader and a collect
// function keyed by metric name, for a daemon a test builds itself. Same
// warning as newMetricsDaemon: collect only after the daemon has stopped.
func (h *testHarness) manualMetrics() (*sdkmetric.MeterProvider, func() map[string]metricdata.Metrics) {
	h.t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return mp, func() map[string]metricdata.Metrics { return collectMetrics(h.t, reader) }
}

// collectMetrics returns the reader's current export keyed by metric name.
func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// sumByAttr totals an int64 counter's data points, keyed by one attribute.
func sumByAttr(t *testing.T, m metricdata.Metrics, key string) map[string]int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s must be a counter, got %T", m.Name, m.Data)
	out := map[string]int64{}
	for _, dp := range sum.DataPoints {
		for _, kv := range dp.Attributes.ToSlice() {
			if string(kv.Key) == key {
				out[kv.Value.String()] += dp.Value
			}
		}
	}
	return out
}

// passthroughAgentLookup returns the binary unchanged. Tests use stand-in
// binary names (e.g. "opus-binary") that aren't on PATH — the real
// defaultAgentLookup would reject them before the runner is invoked.
func passthroughAgentLookup(binary string) (string, error) { return binary, nil }

func (h *testHarness) writeTicket(filename, content string) string {
	h.t.Helper()
	path := filepath.Join(h.tasksDir, filename)
	require.NoError(h.t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func (h *testHarness) readTask(filename string) *ticket.Ticket {
	h.t.Helper()
	path := filepath.Join(h.tasksDir, filename)
	t, err := ticket.ParseFile(path)
	require.NoError(h.t, err, "readTask %s", filename)
	return t
}

func (h *testHarness) waitForStatus(filename string, status ticket.Status, timeout time.Duration) *ticket.Ticket {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	path := filepath.Join(h.tasksDir, filename)
	for time.Now().Before(deadline) {
		t, err := ticket.ParseFile(path)
		if err == nil && t.Status == status {
			return t
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Final attempt for error message.
	t, err := ticket.ParseFile(path)
	require.NoError(h.t, err, "waitForStatus: cannot parse %s", filename)
	h.t.Fatalf("waitForStatus: %s has status=%s, want %s (timeout %v)", filename, t.Status, status, timeout)
	return nil
}

// waitForFinalSummary waits until the ticket's final_summary equals want, which
// must not be empty: the summary pass writes it after the terminal status, so
// waiting on the status alone would race it.
func (h *testHarness) waitForFinalSummary(filename, want string, timeout time.Duration) *ticket.Ticket {
	h.t.Helper()
	require.NotEmpty(h.t, want, "waitForFinalSummary needs the text to wait for")
	path := filepath.Join(h.tasksDir, filename)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		t, err := ticket.ParseFile(path)
		if err == nil && t.FinalSummary == want {
			return t
		}
		time.Sleep(50 * time.Millisecond)
	}
	t, err := ticket.ParseFile(path)
	require.NoError(h.t, err, "waitForFinalSummary: cannot parse %s", filename)
	h.t.Fatalf("waitForFinalSummary: %s has final_summary=%q, want %q (timeout %v)", filename, t.FinalSummary, want, timeout)
	return nil
}

func initRepo(t *testing.T) string {
	t.Helper()
	return testutil.InitRepo(t)
}

func (h *testHarness) taskMD(id, status, pipeline string) string {
	return fmt.Sprintf(`---
id: %s
kontora: true
status: %s
pipeline: %s
path: %s
created: 2026-01-01T00:00:00Z
---
# Test ticket %s
`, id, status, pipeline, h.repoDir, id)
}

func simpleTaskMD(id, status, repoPath string) string {
	return fmt.Sprintf(`---
id: %s
kontora: true
status: %s
path: %s
created: 2026-01-01T00:00:00Z
---
# Test ticket %s
`, id, status, repoPath, id)
}

func TestFullPipeline(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	// Wait for daemon to start.
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-001.md", h.taskMD("tst-001", "todo", "two-stage"))

	result := h.waitForStatus("tst-001.md", ticket.StatusDone, 10*time.Second)
	require.Len(t, result.History, 2)
	for i, entry := range result.History {
		assert.Equal(t, 0, entry.ExitCode, "history[%d]", i)
	}

	cancel()
	require.NoError(t, <-errCh)
}

// TestTicketBaseBranch covers the two ends of base_branch through the daemon:
// a ticket based on a diverged branch gets a work branch cut from it, and a
// base naming the ticket's own work branch pauses instead of silently checking
// the base out and letting the agent commit onto it.
func TestTicketBaseBranch(t *testing.T) {
	cases := []struct {
		name       string
		branch     string
		base       string
		wantStatus ticket.Status
		// wantCommits are commit subjects the work branch must contain.
		wantCommits []string
		wantErr     string
	}{
		{
			name:        "worktree is cut from the declared base",
			base:        "develop",
			wantStatus:  ticket.StatusDone,
			wantCommits: []string{"D1"},
		},
		{
			name:       "base equal to the work branch pauses the ticket",
			branch:     "develop",
			base:       "develop",
			wantStatus: ticket.StatusPaused,
			wantErr:    "own work branch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			// main -> A, develop -> A+D1.
			mustGit(t, h.repoDir, "checkout", "-b", "develop")
			require.NoError(t, os.WriteFile(filepath.Join(h.repoDir, "d.txt"), []byte("d1\n"), 0o644))
			mustGit(t, h.repoDir, "add", "d.txt")
			mustGit(t, h.repoDir, "commit", "-m", "D1")
			mustGit(t, h.repoDir, "checkout", "main")

			d := h.newDaemon(h.cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			branchLine := ""
			if tc.branch != "" {
				branchLine = "branch: " + tc.branch + "\n"
			}
			h.writeTicket("tst-base.md", fmt.Sprintf(`---
id: tst-base
kontora: true
status: todo
pipeline: one-stage
path: %s
%sbase_branch: %s
created: 2026-01-01T00:00:00Z
---
# Base branch ticket
`, h.repoDir, branchLine, tc.base))

			result := h.waitForStatus("tst-base.md", tc.wantStatus, 10*time.Second)

			if tc.wantErr != "" {
				assert.Contains(t, result.LastError, tc.wantErr)
				// The base must not have been checked out as the work branch.
				found, err := worktree.FindWorktreeForBranch(h.repoDir, tc.base)
				require.NoError(t, err)
				assert.Equal(t, "", found, "base branch must not be checked out in a worktree")
			}
			for _, subject := range tc.wantCommits {
				out, err := runGit(h.repoDir, "log", "--format=%s", result.Branch)
				require.NoError(t, err)
				assert.Contains(t, out, subject, "work branch should contain the base's commits")
			}

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

func TestFailureRetry(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("false", "true")
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-002.md", h.taskMD("tst-002", "todo", "retry-stage"))

	result := h.waitForStatus("tst-002.md", ticket.StatusPaused, 10*time.Second)
	// max_retries=1: initial attempt (0) + one retry (1) → pauses at attempt=1.
	assert.Equal(t, 1, result.Attempt)
	require.Len(t, result.History, 2)
	for i, entry := range result.History {
		assert.NotEqual(t, 0, entry.ExitCode, "history[%d] should be failure", i)
	}

	cancel()
	require.NoError(t, <-errCh)
}

func TestConcurrencyLimit(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.MaxConcurrentAgents = 1
	cfg.Agents = map[string]config.Agent{
		"agent1": {Binary: "sleep", Args: []string{"0.3"}},
		"agent2": {Binary: "sleep", Args: []string{"0.3"}},
	}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	cfg.Stages["step2"] = config.Stage{Prompt: ""}
	cfg.Pipelines["one-stage"] = config.Pipeline{
		{Stage: "step1", Agent: "agent1", OnSuccess: "done", OnFailure: "pause"},
	}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-003.md", h.taskMD("tst-003", "todo", "one-stage"))
	h.writeTicket("tst-004.md", h.taskMD("tst-004", "todo", "one-stage"))

	h.waitForStatus("tst-003.md", ticket.StatusDone, 10*time.Second)
	h.waitForStatus("tst-004.md", ticket.StatusDone, 10*time.Second)

	t1 := h.readTask("tst-003.md")
	t2 := h.readTask("tst-004.md")

	// With max_concurrent_agents=1, one must finish before the other starts.
	// Check that their execution didn't fully overlap.
	require.NotEmpty(t, t1.History, "missing history entries")
	require.NotEmpty(t, t2.History, "missing history entries")

	cancel()
	require.NoError(t, <-errCh)
}

func TestStartupScan(t *testing.T) {
	h := newHarness(t)

	// Write ticket files BEFORE starting daemon.
	h.writeTicket("tst-005.md", h.taskMD("tst-005", "todo", "one-stage"))

	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	result := h.waitForStatus("tst-005.md", ticket.StatusDone, 10*time.Second)
	require.Len(t, result.History, 1)

	cancel()
	require.NoError(t, <-errCh)
}

func TestKontoraGuard(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	// Ticket without kontora: true should be ignored.
	h.writeTicket("tst-guard.md", `---
id: tst-guard
status: todo
pipeline: one-stage
created: 2026-01-01T00:00:00Z
---
# Ignored ticket
`)

	// Write a normal ticket to prove the daemon is processing.
	h.writeTicket("tst-ok.md", h.taskMD("tst-ok", "todo", "one-stage"))
	h.waitForStatus("tst-ok.md", ticket.StatusDone, 10*time.Second)

	// The unguarded ticket should still be todo — daemon never touched it.
	result := h.readTask("tst-guard.md")
	assert.Equal(t, ticket.StatusTodo, result.Status, "ticket without kontora:true should not be processed")

	// But it should be visible via GetTicket (tracked for display).
	info, err := d.GetTicket("tst-guard")
	require.NoError(t, err, "non-kontora ticket should be visible via GetTicket")
	assert.Equal(t, "todo", info.Status)
	assert.False(t, info.Kontora, "non-kontora ticket should have kontora=false")

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemonEnqueueAndRunTicketRejectNonKontora(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	path := h.writeTicket("tst-guard.md", `---
id: tst-guard
status: todo
pipeline: one-stage
created: 2026-01-01T00:00:00Z
---
# Ignored ticket
`)
	tkt := h.readTask("tst-guard.md")

	d.mu.Lock()
	d.enqueue(tkt)
	queued := d.queued[tkt.ID]
	queueLen := d.queue.Len()
	d.tickets[tkt.ID] = &ticketState{ticket: tkt, filePath: path}
	d.mu.Unlock()

	assert.False(t, queued)
	assert.Equal(t, 0, queueLen)

	d.runTicket(context.Background(), tkt.ID)
	result := h.readTask("tst-guard.md")
	assert.Equal(t, ticket.StatusTodo, result.Status)
	assert.Equal(t, 0, d.RunningAgents())
}

func TestUserPause(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"10"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-006.md", h.taskMD("tst-006", "todo", "one-stage"))

	// Wait for it to start running.
	h.waitForStatus("tst-006.md", ticket.StatusInProgress, 5*time.Second)

	// Externally set status to paused.
	time.Sleep(100 * time.Millisecond)
	pausedContent := strings.Replace(
		h.taskMD("tst-006", "todo", "one-stage"),
		"status: todo",
		"status: paused",
		1,
	)
	path := h.writeTicket("tst-006.md", pausedContent)
	d.handleFileChanged(path)

	// Wait for agent to be killed — the ticket should stay paused.
	waitForAgentsDone(t, d, 5*time.Second)
	result := h.readTask("tst-006.md")
	assert.Equal(t, ticket.StatusPaused, result.Status)

	cancel()
	require.NoError(t, <-errCh)
}

func TestExternalSetOpen(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"10"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-open.md", h.taskMD("tst-open", "todo", "one-stage"))

	// Wait for it to start running.
	h.waitForStatus("tst-open.md", ticket.StatusInProgress, 5*time.Second)

	// Externally set status to open.
	time.Sleep(100 * time.Millisecond)
	openContent := strings.Replace(
		h.taskMD("tst-open", "todo", "one-stage"),
		"status: todo",
		"status: open",
		1,
	)
	path := h.writeTicket("tst-open.md", openContent)
	d.handleFileChanged(path)

	// Wait for agent to be killed — the ticket should stay open.
	waitForAgentsDone(t, d, 5*time.Second)
	result := h.readTask("tst-open.md")
	assert.Equal(t, ticket.StatusOpen, result.Status)

	cancel()
	require.NoError(t, <-errCh)
}

func TestMoveTicketToOpenKillsAgent(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"10"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-mv.md", h.taskMD("tst-mv", "todo", "one-stage"))
	h.waitForStatus("tst-mv.md", ticket.StatusInProgress, 5*time.Second)

	// Let the agent finish spawning so worktree setup has stopped writing the
	// ticket before we change its status.
	time.Sleep(100 * time.Millisecond)

	// Move to open via the web/API path (self-write through SetStatus), which
	// is what the kanban board uses. This must stop the running agent.
	require.NoError(t, d.MoveTicket("tst-mv", "open"))

	waitForAgentsDone(t, d, 5*time.Second)
	result := h.readTask("tst-mv.md")
	assert.Equal(t, ticket.StatusOpen, result.Status)

	cancel()
	require.NoError(t, <-errCh)
}

func TestExternalSetDone(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"10"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-done.md", h.taskMD("tst-done", "todo", "one-stage"))

	// Wait for it to start running.
	h.waitForStatus("tst-done.md", ticket.StatusInProgress, 5*time.Second)

	// Externally set status to done.
	time.Sleep(100 * time.Millisecond)
	doneContent := strings.Replace(
		h.taskMD("tst-done", "todo", "one-stage"),
		"status: todo",
		"status: done",
		1,
	)
	path := h.writeTicket("tst-done.md", doneContent)
	d.handleFileChanged(path)

	// Wait for agent to be killed — the ticket should stay done.
	waitForAgentsDone(t, d, 5*time.Second)
	result := h.readTask("tst-done.md")
	assert.Equal(t, ticket.StatusDone, result.Status)

	cancel()
	require.NoError(t, <-errCh)
}

func TestExternalSetArchived(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"10"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-arch.md", h.taskMD("tst-arch", "todo", "one-stage"))

	// Wait for it to start running.
	h.waitForStatus("tst-arch.md", ticket.StatusInProgress, 5*time.Second)

	// Externally set status to archived.
	time.Sleep(100 * time.Millisecond)
	archivedContent := strings.Replace(
		h.taskMD("tst-arch", "todo", "one-stage"),
		"status: todo",
		"status: archived",
		1,
	)
	path := h.writeTicket("tst-arch.md", archivedContent)
	d.handleFileChanged(path)

	// Agent should be killed and the ticket should stay archived (not re-enqueued).
	waitForAgentsDone(t, d, 5*time.Second)
	result := h.readTask("tst-arch.md")
	assert.Equal(t, ticket.StatusArchived, result.Status)

	cancel()
	require.NoError(t, <-errCh)
}

func TestFileLock(t *testing.T) {
	h := newHarness(t)
	d1 := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d1.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	// Second daemon with same lock path should fail.
	d2 := h.newDaemon(h.cfg)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()

	err := d2.Run(ctx2)
	require.ErrorContains(t, err, "lock")

	cancel()
	require.NoError(t, <-errCh)
}

func TestTmuxRunner(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}

	h := newHarness(t)

	// Use the default tmux runner (don't pass WithRunner).
	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-tmx.md", h.taskMD("tst-tmx", "todo", "one-stage"))

	result := h.waitForStatus("tst-tmx.md", ticket.StatusDone, 15*time.Second)
	require.Len(t, result.History, 1)
	assert.Equal(t, 0, result.History[0].ExitCode)

	// Verify no orphaned tmux windows remain.
	out, err := exec.Command("tmux", "list-windows", "-t", "="+h.tmuxSession, "-F", "#{window_name}").CombinedOutput()
	if err == nil {
		for line := range strings.SplitSeq(string(out), "\n") {
			assert.NotEqual(t, "tst-tmx", strings.TrimSpace(line), "orphaned window found")
		}
	}

	cancel()
	require.NoError(t, <-errCh)
}

func TestNoteInstructionAppended(t *testing.T) {
	h := newHarness(t)

	var captured []string
	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		captured = append(captured, p.Args...)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-note.md", h.taskMD("tst-note", "todo", "one-stage"))

	h.waitForStatus("tst-note.md", ticket.StatusDone, 10*time.Second)

	// The last arg is the rendered prompt with note instruction and context block.
	require.NotEmpty(t, captured, "expected at least one arg from agent runner")
	prompt := captured[len(captured)-1]
	assert.Contains(t, prompt, "kontora note tst-note", "note instruction not found in pipeline prompt")
	assert.Contains(t, prompt, "Task ID: tst-note", "task ID context not found in pipeline prompt")
	assert.Contains(t, prompt, "Ticket:", "ticket context not found in pipeline prompt")
	assert.Contains(t, prompt, "Workspace:", "workspace context not found in pipeline prompt")

	cancel()
	require.NoError(t, <-errCh)
}

func TestBuildOperationalAppendix(t *testing.T) {
	cases := []struct {
		name       string
		taskID     string
		filePath   string
		wtPath     string
		isPipeline bool
		wantAll    []string
		wantNone   []string
	}{
		{
			name:       "simple task includes context but not pipeline instruction",
			taskID:     "tst-1234",
			filePath:   "/home/user/.kontora/tasks/tst-1234.md",
			wtPath:     "/home/user/.kontora/worktrees/tst-1234",
			isPipeline: false,
			wantAll: []string{
				"## Operational Context",
				"Ticket ID: tst-1234",
				"Ticket file: /home/user/.kontora/tasks/tst-1234.md",
				"Worktree: /home/user/.kontora/worktrees/tst-1234",
				"kontora note tst-1234",
				"kontora view tst-1234",
				"Do not search $HOME",
			},
			wantNone: []string{
				"IMPORTANT: When you finish your work",
			},
		},
		{
			name:       "pipeline task includes context and pipeline instruction",
			taskID:     "pip-abcd",
			filePath:   "/tasks/pip-abcd.md",
			wtPath:     "/worktrees/pip-abcd",
			isPipeline: true,
			wantAll: []string{
				"## Operational Context",
				"Ticket ID: pip-abcd",
				"Ticket file: /tasks/pip-abcd.md",
				"Worktree: /worktrees/pip-abcd",
				"kontora note pip-abcd",
				"kontora view pip-abcd",
				"Do not search $HOME",
				"IMPORTANT: When you finish your work",
				"kontora note pip-abcd \"your results here\"",
			},
			wantNone: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOperationalAppendix(tc.taskID, tc.filePath, tc.wtPath, tc.isPipeline)
			for _, want := range tc.wantAll {
				assert.Contains(t, got, want)
			}
			for _, absent := range tc.wantNone {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

func TestSimpleTaskNoteInstructionAndContext(t *testing.T) {
	h := newHarness(t)

	var captured []string
	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		captured = append(captured, p.Args...)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-snote.md", simpleTaskMD("tst-snote", "todo", h.repoDir))

	h.waitForStatus("tst-snote.md", ticket.StatusDone, 10*time.Second)

	require.NotEmpty(t, captured, "expected at least one arg from agent runner")
	prompt := captured[len(captured)-1]
	assert.Contains(t, prompt, "Test ticket tst-snote", "ticket title not found in simple task prompt")
	assert.Contains(t, prompt, "kontora note tst-snote", "note instruction not found in simple task prompt")
	assert.Contains(t, prompt, "Ticket ID: tst-snote", "ticket ID context not found in simple task prompt")
	assert.Contains(t, prompt, "Ticket file:", "ticket file context not found in simple task prompt")
	assert.Contains(t, prompt, "Worktree:", "worktree context not found in simple task prompt")

	cancel()
	require.NoError(t, <-errCh)
}

func TestAgentInteractiveMode(t *testing.T) {
	cases := []struct {
		name            string
		binary          string
		wantInteractive bool
		wantSettings    bool
		wantSessionID   bool
		wantNoPrint     bool
		wantExtension   bool
	}{
		{
			name:            "claude agent is interactive with settings and session ID",
			binary:          "claude",
			wantInteractive: true,
			wantSettings:    true,
			wantSessionID:   true,
			wantNoPrint:     true,
		},
		{
			name:            "non-claude agent is not interactive",
			binary:          "true",
			wantInteractive: false,
		},
		{
			name:          "pi agent gets exit extension",
			binary:        "pi",
			wantExtension: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig(tc.binary, "true")

			var params RunnerParams
			var extensionContent string
			capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
				params = p
				// Read extension file inside runner before deferred cleanup fires.
				if idx := slices.Index(p.Args, "-e"); idx >= 0 && idx+1 < len(p.Args) {
					data, err := os.ReadFile(p.Args[idx+1])
					if err == nil {
						extensionContent = string(data)
					}
				}
				return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
			}

			d := New(cfg,
				WithLogger(testLogger(t)),
				WithDebounce(50*time.Millisecond),
				WithLockPath(h.lockPath),
				WithRunner(capturingRunner),
				WithAgentLookup(passthroughAgentLookup),
				WithSkipOrphanCleanup(),
			)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()

			time.Sleep(200 * time.Millisecond)

			ticketID := fmt.Sprintf("tst-%s", tc.binary)
			h.writeTicket(ticketID+".md", h.taskMD(ticketID, "todo", "one-stage"))
			h.waitForStatus(ticketID+".md", ticket.StatusDone, 10*time.Second)

			assert.Equal(t, tc.wantInteractive, params.Interactive)
			if tc.wantSettings {
				assert.True(t, slices.Contains(params.Args, "--settings"), "args missing --settings: %v", params.Args)
			}
			if tc.wantSessionID {
				assert.True(t, slices.Contains(params.Args, "--session-id"), "args missing --session-id: %v", params.Args)
				assert.NotEmpty(t, params.SessionID, "SessionID should be set for Claude agents")
			} else {
				assert.Empty(t, params.SessionID, "SessionID should be empty for non-Claude agents")
			}
			if tc.wantNoPrint {
				assert.False(t, slices.Contains(params.Args, "--print"), "args should not contain --print: %v", params.Args)
			}
			if tc.wantExtension {
				assert.True(t, slices.Contains(params.Args, "-e"), "args missing -e: %v", params.Args)
				assert.Contains(t, extensionContent, "agent_end")
				assert.Contains(t, extensionContent, "ctx.shutdown()")
			}

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

func TestWritePiExitExtension(t *testing.T) {
	path, err := writePiExitExtension()
	require.NoError(t, err)
	defer os.Remove(path)

	assert.True(t, strings.HasSuffix(path, ".ts"), "extension file should have .ts suffix: %s", path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "agent_end")
	assert.Contains(t, content, "ctx.shutdown()")
}

func TestPiSessionLogMaterialization(t *testing.T) {
	h := newHarness(t)

	var capturedParams RunnerParams

	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		capturedParams = p
		stage := strings.TrimSuffix(filepath.Base(p.LogFile), ".log")
		// Simulate pipe-pane writing raw PTY output to the log file.
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("raw PTY output from pipe-pane"), 0o644))
		// Write a fake pi session JSONL to the session dir.
		if p.SessionDir != "" {
			require.NoError(t, os.MkdirAll(p.SessionDir, 0o755))
			sessionFile := filepath.Join(p.SessionDir, "session-"+stage+".jsonl")
			jsonl := strings.Join([]string{
				`{"type":"model_change","modelId":"opus-4"}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"I will check the tests."}]}}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
				`{"type":"message","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"PASS\n"}]}}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"finished ` + stage + `."}]}}`,
			}, "\n")
			require.NoError(t, os.WriteFile(sessionFile, []byte(jsonl), 0o644))
		}
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("pi", "pi")
	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-pisl.md", h.taskMD("tst-pisl", "todo", "two-stage"))
	h.waitForStatus("tst-pisl.md", ticket.StatusDone, 10*time.Second)

	// Verify SessionDir was set and --session-dir was in args.
	assert.NotEmpty(t, capturedParams.SessionDir, "SessionDir should be set for pi agents")
	assert.True(t, slices.Contains(capturedParams.Args, "--session-dir"), "args missing --session-dir: %v", capturedParams.Args)
	idx := slices.Index(capturedParams.Args, "--session-dir")
	assert.Equal(t, capturedParams.SessionDir, capturedParams.Args[idx+1])

	// Verify each stage log holds formatted output from its own session, not
	// raw JSONL and not the other stage's session.
	for _, stage := range []string{"step1", "step2"} {
		logContent, err := os.ReadFile(filepath.Join(h.logsDir, "tst-pisl", stage+".log"))
		require.NoError(t, err, "log file for %s should exist", stage)

		content := string(logContent)
		assert.Contains(t, content, "[opus-4]")
		assert.Contains(t, content, "I will check the tests.")
		assert.Contains(t, content, "> bash go test ./...")
		assert.Contains(t, content, "PASS")
		assert.Contains(t, content, "finished "+stage+".")
		assert.NotContains(t, content, `"type":"message"`, "log should be formatted, not raw JSONL")
		assert.NotContains(t, content, "raw PTY output", "JSONL materialization should overwrite pipe-pane output")
	}

	cancel()
	require.NoError(t, <-errCh)
}

func TestPiSessionLogMaterializationMissing(t *testing.T) {
	h := newHarness(t)

	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		stage := strings.TrimSuffix(filepath.Base(p.LogFile), ".log")
		// Simulate pipe-pane writing raw PTY output to the log file.
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("raw PTY fallback output"), 0o644))
		// Only the first stage writes a session file; the second simulates pi
		// writing none.
		if p.SessionDir != "" && stage == "step1" {
			require.NoError(t, os.MkdirAll(p.SessionDir, 0o755))
			jsonl := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"step1 session text."}]}}`
			require.NoError(t, os.WriteFile(filepath.Join(p.SessionDir, "session-step1.jsonl"), []byte(jsonl), 0o644))
		}
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("pi", "pi")
	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-pmis.md", h.taskMD("tst-pmis", "todo", "two-stage"))

	// Ticket should still complete even if pi session JSONL is missing.
	h.waitForStatus("tst-pmis.md", ticket.StatusDone, 10*time.Second)

	// Verify the raw PTY output survives as fallback when JSONL is missing, and
	// that the stage without a session does not inherit the other stage's.
	logContent, err := os.ReadFile(filepath.Join(h.logsDir, "tst-pmis", "step2.log"))
	require.NoError(t, err, "log file should exist from pipe-pane")
	assert.Contains(t, string(logContent), "raw PTY fallback output", "pipe-pane output should survive when JSONL materialization fails")
	assert.NotContains(t, string(logContent), "step1 session text.", "a stage without a session must not show another stage's log")

	cancel()
	require.NoError(t, <-errCh)
}

func TestAgentEnvironmentOverride(t *testing.T) {
	h := newHarness(t)

	h.cfg.Environment = map[string]string{
		"GLOBAL_VAR": "global",
		"SHARED_VAR": "from-global",
		"UNSET_ME":   "should-disappear",
	}
	h.cfg.Agents["agent1"] = config.Agent{
		Binary: "true",
		Environment: map[string]string{
			"AGENT_VAR":  "agent-only",
			"SHARED_VAR": "from-agent",
			"UNSET_ME":   "",
		},
	}

	var captured RunnerParams
	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		captured = p
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-env.md", h.taskMD("tst-env", "todo", "one-stage"))
	h.waitForStatus("tst-env.md", ticket.StatusDone, 10*time.Second)

	assert.Equal(t, "global", captured.Env["GLOBAL_VAR"], "global env should be inherited")
	assert.Equal(t, "agent-only", captured.Env["AGENT_VAR"], "agent-specific env should be present")
	assert.Equal(t, "from-agent", captured.Env["SHARED_VAR"], "agent env should override global env")
	_, unsetPresent := captured.Env["UNSET_ME"]
	assert.False(t, unsetPresent, "empty string in agent env should unset global key")

	cancel()
	require.NoError(t, <-errCh)
}

// TestPickupWritesClaim asserts that the pipeline and simple pickup paths
// persist claimed_by=test-instance alongside status=in_progress, overwriting any
// stale foreign claim. A sleep agent keeps the ticket parked in_progress so we
// can read the claim off disk.
func TestPickupWritesClaim(t *testing.T) {
	cases := []struct {
		name       string
		pipeline   string // empty → simple (no-pipeline) ticket
		startClaim string // a stale claim already on the todo ticket
	}{
		{name: "pipeline", pipeline: "one-stage"},
		{name: "simple", pipeline: ""},
		{name: "pipeline overwrites stale foreign claim", pipeline: "one-stage", startClaim: "beta"},
		{name: "simple overwrites stale foreign claim", pipeline: "", startClaim: "beta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("sh", "sh")
			// The rendered prompt is appended as a trailing argument, so the
			// sleep is wrapped in sh -c to keep it from being read as an interval.
			cfg.Agents["agent1"] = config.Agent{Binary: "sh", Args: []string{"-c", "sleep 10"}}
			cfg.Stages["step1"] = config.Stage{Prompt: ""}
			d := h.newDaemon(cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			md := simpleTaskMD("tst-clm", "todo", h.repoDir)
			if tc.pipeline != "" {
				md = h.taskMD("tst-clm", "todo", tc.pipeline)
			}
			if tc.startClaim != "" {
				md = strings.Replace(md, "status: todo\n",
					"status: todo\nclaimed_by: "+tc.startClaim+"\n", 1)
			}
			h.writeTicket("tst-clm.md", md)

			result := h.waitForStatus("tst-clm.md", ticket.StatusInProgress, 5*time.Second)
			assert.Equal(t, "test-instance", result.ClaimedBy)

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

func TestCrashRecovery(t *testing.T) {
	h := newHarness(t)

	// Simulate crashed daemon: ticket file with status=in_progress.
	runningTask := strings.Replace(
		h.taskMD("tst-007", "todo", "one-stage"),
		"status: todo",
		"status: in_progress",
		1,
	)
	h.writeTicket("tst-007.md", runningTask)

	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	// Daemon should reset in_progress → todo, then process through to done.
	result := h.waitForStatus("tst-007.md", ticket.StatusDone, 10*time.Second)
	require.Len(t, result.History, 1)

	cancel()
	require.NoError(t, <-errCh)
}

// TestCrashRecoveryClaims checks that startup crash recovery respects claims:
// unclaimed and locally claimed in_progress tickets recover to todo and run,
// while a foreign-claimed ticket is left untouched and never started — even
// with auto_pick_up disabled.
func TestCrashRecoveryClaims(t *testing.T) {
	cases := []struct {
		name        string
		claimedBy   string
		autoPickUp  bool
		wantRecover bool
	}{
		{name: "unclaimed recovers", claimedBy: "", autoPickUp: true, wantRecover: true},
		{name: "local claim recovers", claimedBy: "test-instance", autoPickUp: true, wantRecover: true},
		{name: "foreign claim left alone", claimedBy: "other-host", autoPickUp: true, wantRecover: false},
		{name: "foreign claim, auto pickup off", claimedBy: "other-host", autoPickUp: false, wantRecover: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("true", "true")
			autoPickUp := tc.autoPickUp
			cfg.AutoPickUp = &autoPickUp

			md := strings.Replace(h.taskMD("tst-cr", "todo", "one-stage"),
				"status: todo", "status: in_progress", 1)
			if tc.claimedBy != "" {
				md = strings.Replace(md, "status: in_progress\n",
					"status: in_progress\nclaimed_by: "+tc.claimedBy+"\n", 1)
			}
			path := h.writeTicket("tst-cr.md", md)
			origBytes, err := os.ReadFile(path)
			require.NoError(t, err)

			d := h.newDaemon(cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()

			if tc.wantRecover {
				h.waitForStatus("tst-cr.md", ticket.StatusDone, 10*time.Second)
			} else {
				// Foreign claim: the file must be byte-for-byte untouched and no
				// agent may run.
				time.Sleep(500 * time.Millisecond)
				got := h.readTask("tst-cr.md")
				assert.Equal(t, ticket.StatusInProgress, got.Status)
				assert.Equal(t, tc.claimedBy, got.ClaimedBy)
				nowBytes, rErr := os.ReadFile(path)
				require.NoError(t, rErr)
				assert.Equal(t, origBytes, nowBytes, "foreign-claimed ticket must not be modified")
				assert.Equal(t, 0, d.RunningAgents())
			}

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

// TestPartitionWindows checks the ownership predicate startup cleanup runs on:
// a window is this daemon's only when it is named after a ticket in its own
// tickets_dir that no other instance has claimed.
func TestPartitionWindows(t *testing.T) {
	cases := []struct {
		name        string
		windows     []string
		wantMine    []string
		wantForeign []string
	}{
		{name: "unclaimed is mine", windows: []string{"tst-free"}, wantMine: []string{"tst-free"}},
		{name: "own claim is mine", windows: []string{"tst-mine"}, wantMine: []string{"tst-mine"}},
		{name: "foreign claim is not mine", windows: []string{"tst-them"}, wantForeign: []string{"tst-them"}},
		{
			name:        "unknown ticket is not mine",
			windows:     []string{"kon-4y7b"},
			wantForeign: []string{"kon-4y7b"},
		},
		{
			name:        "mixed set",
			windows:     []string{"tst-free", "kon-4y7b", "tst-mine", "tst-them"},
			wantMine:    []string{"tst-free", "tst-mine"},
			wantForeign: []string{"kon-4y7b", "tst-them"},
		},
		{name: "empty input"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			for id, claimedBy := range map[string]string{
				"tst-free": "",
				"tst-mine": "test-instance",
				"tst-them": "other-host",
			} {
				md := h.taskMD(id, "in_progress", "one-stage")
				if claimedBy != "" {
					md = strings.Replace(md, "status: in_progress\n",
						"status: in_progress\nclaimed_by: "+claimedBy+"\n", 1)
				}
				path := h.writeTicket(id+".md", md)
				tk, err := ticket.ParseFile(path)
				require.NoError(t, err)
				d.tickets[id] = newTicketState(tk, path)
			}

			mine, foreign := d.partitionWindows(tc.windows)
			assert.Equal(t, tc.wantMine, mine)
			assert.Equal(t, tc.wantForeign, foreign)
		})
	}
}

// A daemon started against its own tickets_dir must leave the windows of every
// agent another daemon is running alone. The ordering is pinned here too: with
// cleanup back before initialScan the ticket set is empty, nothing counts as
// owned, and tst-own survives.
func TestStartupCleanupKillsOnlyOwnWindows(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("tst-own.md", h.taskMD("tst-own", "paused", "one-stage"))

	var killed []string
	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(DirectRunner),
		WithAgentLookup(passthroughAgentLookup),
	)
	d.windows = windowOps{
		list: func(string) ([]string, error) {
			return []string{"tst-own", "tst-other", "kon-4y7b"}, nil
		},
		kill: func(_, window string) error {
			killed = append(killed, window)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(300 * time.Millisecond)
	cancel()
	require.NoError(t, <-errCh)

	assert.Equal(t, []string{"tst-own"}, killed)
}

// Sync tools (iCloud, Syncthing) leave stale conflict copies like "<id> 2.md"
// or "<id>.sync-conflict-*.md" that still carry the same frontmatter id. The
// daemon must not treat them as the live ticket: no crash recovery, no
// enqueue, no state updates.
func TestNonCanonicalFilesIgnored(t *testing.T) {
	h := newHarness(t)

	// A finished ticket plus a stale conflict copy claiming it still runs.
	h.writeTicket("tst-cc.md", h.taskMD("tst-cc", "done", "one-stage"))
	h.writeTicket("tst-cc 2.md", h.taskMD("tst-cc", "in_progress", "one-stage"))

	// Foreign tickets (no kontora frontmatter) get the same protection: a
	// stale conflict copy shares the id and must not shadow the canonical
	// file's content.
	h.writeTicket("for-1.md", "---\nid: for-1\nstatus: open\n---\n# Real foreign body\n")
	h.writeTicket("for-1.sync-conflict-20260610-070128-IDDACTZ.md", "---\nid: for-1\nstatus: open\n---\n# Stale stub\n")

	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(300 * time.Millisecond)

	// Initial scan: the copy is neither crash-recovered nor picked up, and
	// the canonical ticket stays done.
	assert.Equal(t, ticket.StatusDone, h.readTask("tst-cc.md").Status)
	assert.Equal(t, ticket.StatusInProgress, h.readTask("tst-cc 2.md").Status)

	info, err := d.GetTicket("for-1")
	require.NoError(t, err)
	assert.Equal(t, "Real foreign body", info.Title)

	// Watcher: a stale copy rewritten by sync churn must not re-enqueue the
	// ticket.
	path := h.writeTicket("tst-cc.sync-conflict-20260610-070128-IDDACTZ.md",
		h.taskMD("tst-cc", "todo", "one-stage"))
	d.handleFileChanged(path)
	time.Sleep(300 * time.Millisecond)

	assert.Equal(t, ticket.StatusDone, h.readTask("tst-cc.md").Status)
	assert.Equal(t, ticket.StatusTodo, h.readTask("tst-cc.sync-conflict-20260610-070128-IDDACTZ.md").Status)
	assert.Equal(t, 0, d.RunningAgents())

	// Watcher: same for the foreign ticket's conflict copy.
	d.handleFileChanged(filepath.Join(h.tasksDir, "for-1.sync-conflict-20260610-070128-IDDACTZ.md"))
	info, err = d.GetTicket("for-1")
	require.NoError(t, err)
	assert.Equal(t, "Real foreign body", info.Title)

	cancel()
	require.NoError(t, <-errCh)
}

// TestForeignClaimYield covers the watcher yield rule: while an agent runs, a
// foreign claim arriving through sync must cancel the local agent and leave the
// file untouched (no status, history, or last_error write).
func TestForeignClaimYield(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"10"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-yld.md", h.taskMD("tst-yld", "todo", "one-stage"))
	h.waitForStatus("tst-yld.md", ticket.StatusInProgress, 5*time.Second)
	time.Sleep(100 * time.Millisecond)

	// A foreign claim arrives: same in_progress status, different owner.
	foreign := strings.Replace(h.taskMD("tst-yld", "in_progress", "one-stage"),
		"status: in_progress\n", "status: in_progress\nclaimed_by: other-host\n", 1)
	path := h.writeTicket("tst-yld.md", foreign)
	d.handleFileChanged(path)

	// The local agent is cancelled and we write nothing.
	waitForAgentsDone(t, d, 5*time.Second)
	got := h.readTask("tst-yld.md")
	assert.Equal(t, ticket.StatusInProgress, got.Status)
	assert.Equal(t, "other-host", got.ClaimedBy)
	assert.Empty(t, got.LastError)
	assert.Empty(t, got.History)

	cancel()
	require.NoError(t, <-errCh)
}

// TestHandleAgentExitForeignClaimGuard covers the pipeline exit guard: when the
// re-read ticket is claimed by another instance, handleAgentExit must write
// nothing — for both success and failure exits.
func TestHandleAgentExitForeignClaimGuard(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
	}{
		{name: "success exit", exitCode: 0},
		{name: "failed exit", exitCode: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)

			md := strings.Replace(h.taskMD("tst-eg", "in_progress", "one-stage"),
				"status: in_progress\n", "status: in_progress\nclaimed_by: other-host\n", 1)
			path := h.writeTicket("tst-eg.md", md)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			d.handleAgentExit(context.Background(), context.Background(), handleExitParams{
				log:         testLogger(t),
				ticketID:    "tst-eg",
				filePath:    path,
				stageName:   "step1",
				result:      process.Result{ExitCode: tc.exitCode, ExitedAt: time.Now()},
				pipelineCfg: h.cfg.Pipelines["one-stage"],
				repoPath:    h.repoDir,
				wtPath:      filepath.Join(h.wtDir, h.repoName, "tst-eg"),
			})

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after), "foreign-claimed ticket must not be written on exit")
		})
	}
}

// TestSimpleTicketExitForeignClaimGuard covers the simple-ticket exit guard. A
// custom runner claims the ticket for another instance mid-run (suppressing the
// watcher event via recordSelfWrite), so the exit path — not the watcher —
// handles it. On a clean exit the guard must leave the ticket in_progress and
// foreign-claimed, with no done/last_error write.
func TestSimpleTicketExitForeignClaimGuard(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	ticketPath := filepath.Join(h.tasksDir, "tst-sg.md")

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
	d = New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-sg.md", simpleTaskMD("tst-sg", "todo", h.repoDir))

	// Wait until the runner has claimed the ticket for other-host (the agent
	// started and ran), then wait for the exit handler to finish.
	// Parse without failing the test: the runner writes the ticket in place
	// while this polls it, so a read can catch a truncated file. That is a torn
	// read, not a failed claim, and the next tick sees the whole file.
	require.Eventually(t, func() bool {
		tk, err := ticket.ParseFile(ticketPath)
		return err == nil && tk.ClaimedBy == "other-host"
	}, 5*time.Second, 20*time.Millisecond, "runner should claim for other-host")
	waitForAgentsDone(t, d, 5*time.Second)

	got := h.readTask("tst-sg.md")
	assert.Equal(t, ticket.StatusInProgress, got.Status, "guard must not write status=done")
	assert.Equal(t, "other-host", got.ClaimedBy)
	assert.Empty(t, got.LastError)

	cancel()
	require.NoError(t, <-errCh)
}

// TestStaleSelfClaimRecovery covers the both-sides-yielded recovery: an
// in_progress ticket claimed by this instance with no local agent must be reset
// to todo, enqueued, and run to completion.
func TestStaleSelfClaimRecovery(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	md := strings.Replace(h.taskMD("tst-ss", "in_progress", "one-stage"),
		"status: in_progress\n", "status: in_progress\nclaimed_by: test-instance\n", 1)
	path := h.writeTicket("tst-ss.md", md)
	d.handleFileChanged(path)

	h.waitForStatus("tst-ss.md", ticket.StatusDone, 10*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

func waitForAgentsDone(t *testing.T, d *Daemon, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.RunningAgents() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("agents still running after %v", timeout)
}

func (h *testHarness) waitForWorktreeGone(ticketID string, timeout time.Duration) {
	h.t.Helper()
	wtPath := filepath.Join(h.wtDir, h.repoName, ticketID)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(wtPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("worktree still exists at %s after %v", wtPath, timeout)
}

func TestTicketBranch(t *testing.T) {
	cfg := &config.Config{
		BranchPrefix: "kontora",
		Projects: map[string]config.Project{
			"kontora": {Path: "/repos/kontora", BranchPrefix: "feature"},
			"widget-api":   {Path: "/repos/widget-api"},
		},
	}

	cases := []struct {
		name string
		tkt  ticket.Ticket
		want string
	}{
		{
			name: "project prefix",
			tkt:  ticket.Ticket{ID: "tst-1", Path: "/repos/kontora"},
			want: "feature/tst-1",
		},
		{
			name: "project without a prefix falls back",
			tkt:  ticket.Ticket{ID: "tst-2", Path: "/repos/widget-api"},
			want: "kontora/tst-2",
		},
		{
			name: "unconfigured repository falls back",
			tkt:  ticket.Ticket{ID: "tst-3", Path: "/repos/other"},
			want: "kontora/tst-3",
		},
		{
			name: "branch already set wins",
			tkt:  ticket.Ticket{ID: "tst-4", Path: "/repos/kontora", Branch: "mine/tst-4"},
			want: "mine/tst-4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ticketBranch(cfg, &tc.tkt))
		})
	}
}

func TestGeneratedTicketBranch(t *testing.T) {
	cfg := &config.Config{BranchPrefix: "kontora"}
	tests := []struct {
		name string
		tkt  ticket.Ticket
		want string
		ok   bool
	}{
		{
			name: "title slug and ticket ID",
			tkt: ticket.Ticket{
				ID:   "kon-a3f2",
				Path: "/repos/kontora",
				Body: "# [kontora] Fix the retry double count\n",
			},
			want: "kontora/fix-retry-double-count-kon-a3f2",
			ok:   true,
		},
		{
			name: "empty slug",
			tkt: ticket.Ticket{
				ID:   "kon-a3f2",
				Path: "/repos/kontora",
				Body: "# !!!\n",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := generatedTicketBranch(cfg, &tt.tkt)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// The branch the web UI shows in an empty branch field has to be the one pickup
// would produce, for both naming modes and for a title that yields no slug.
func TestAutoTicketBranch(t *testing.T) {
	cfg := &config.Config{
		BranchPrefix: "kontora",
		BranchNaming: config.BranchNaming{Mode: config.BranchNamingModeSlug},
		Projects: map[string]config.Project{
			"api":    {Path: "/repos/api", BranchPrefix: "feature"},
			"legacy": {Path: "/repos/legacy", BranchNaming: config.BranchNaming{Mode: config.BranchNamingModeOff}},
		},
	}

	tests := []struct {
		name string
		tkt  ticket.Ticket
		want string
		// wantInfo is the auto branch the ticket's web projection carries, empty
		// when the ticket names a branch itself.
		wantInfo string
	}{
		{
			name:     "slug mode names the branch after the title",
			tkt:      ticket.Ticket{ID: "api-a3f2", Path: "/repos/api", Body: "# [kontora] Fix the retry double count\n"},
			want:     "feature/fix-retry-double-count-api-a3f2",
			wantInfo: "feature/fix-retry-double-count-api-a3f2",
		},
		{
			name:     "a project with naming off falls back to the ID",
			tkt:      ticket.Ticket{ID: "leg-1", Path: "/repos/legacy", Body: "# Fix the retry double count\n"},
			want:     "kontora/leg-1",
			wantInfo: "kontora/leg-1",
		},
		{
			name:     "a title with no slug falls back to the ID",
			tkt:      ticket.Ticket{ID: "api-2", Path: "/repos/api", Body: "# !!!\n"},
			want:     "feature/api-2",
			wantInfo: "feature/api-2",
		},
		{
			name: "a ticket that names its own branch reports no auto branch",
			tkt:  ticket.Ticket{ID: "api-3", Path: "/repos/api", Branch: "mine/api-3", Body: "# Fix the retry double count\n"},
			want: "feature/fix-retry-double-count-api-3",
		},
	}

	d := New(cfg)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, autoTicketBranch(cfg, &tt.tkt))

			info := d.buildTicketInfo(cfg, &ticketState{ticket: &tt.tkt}, false)
			assert.Equal(t, tt.wantInfo, info.AutoBranch)
		})
	}
}

func TestBranchNamingAtPickup(t *testing.T) {
	tests := []struct {
		name               string
		id                 string
		mode               string
		pipeline           string
		title              string
		presetBranch       string
		wantBranch         string
		wantNoGeneratedRef string
	}{
		{
			name:               "preset branch is preserved",
			id:                 "tst-custom",
			mode:               config.BranchNamingModeSlug,
			pipeline:           "one-stage",
			title:              "[kontora] Fix the retry double count",
			presetBranch:       "my/custom-branch",
			wantBranch:         "my/custom-branch",
			wantNoGeneratedRef: "kontora/fix-retry-double-count-tst-custom",
		},
		{
			name:       "off mode uses ticket ID",
			id:         "tst-off",
			mode:       config.BranchNamingModeOff,
			pipeline:   "one-stage",
			title:      "[kontora] Fix the retry double count",
			wantBranch: "kontora/tst-off",
		},
		{
			name:       "empty slug uses ticket ID",
			id:         "tst-empty",
			mode:       config.BranchNamingModeSlug,
			pipeline:   "one-stage",
			title:      "!!!",
			wantBranch: "kontora/tst-empty",
		},
		{
			name:       "simple ticket gets generated branch",
			id:         "tst-simple",
			mode:       config.BranchNamingModeSlug,
			title:      "[kontora] Fix the retry double count",
			wantBranch: "kontora/fix-retry-double-count-tst-simple",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.BranchNaming.Mode = tt.mode
			d := h.newDaemon(h.cfg)

			pipelineLine := ""
			if tt.pipeline != "" {
				pipelineLine = "pipeline: " + tt.pipeline + "\n"
			}
			branchLine := ""
			if tt.presetBranch != "" {
				branchLine = "branch: " + tt.presetBranch + "\n"
			}
			h.writeTicket(tt.id+".md", fmt.Sprintf(`---
id: %s
kontora: true
status: todo
%s%sbase_branch: main
path: %s
created: 2026-01-01T00:00:00Z
---
# %s
`, tt.id, pipelineLine, branchLine, h.repoDir, tt.title))

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()

			result := h.waitForStatus(tt.id+".md", ticket.StatusDone, 10*time.Second)
			assert.Equal(t, tt.wantBranch, result.Branch)
			assert.Equal(t, "main", result.BaseBranch)
			assert.True(t, gitBranchExists(h.repoDir, tt.wantBranch), "branch %q should exist", tt.wantBranch)
			if tt.wantNoGeneratedRef != "" {
				assert.False(t, gitBranchExists(h.repoDir, tt.wantNoGeneratedRef))
			}
			h.waitForWorktreeGone(tt.id, 5*time.Second)

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

func TestGeneratedBranchCleanupOnDone(t *testing.T) {
	h := newHarness(t)
	h.cfg.BranchNaming.Mode = config.BranchNamingModeSlug
	h.cfg.Agents["agent1"] = config.Agent{Binary: "sh", Args: []string{"-c", "sleep 30", "--"}}
	d := h.newDaemon(h.cfg)

	const id = "tst-clean"
	const wantBranch = "kontora/fix-retry-cleanup-tst-clean"
	path := h.writeTicket(id+".md", fmt.Sprintf(`---
id: %s
kontora: true
status: todo
pipeline: one-stage
base_branch: main
path: %s
created: 2026-01-01T00:00:00Z
---
# [kontora] Fix retry cleanup
`, id, h.repoDir))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	h.waitForStatus(id+".md", ticket.StatusInProgress, 5*time.Second)
	wtPath := filepath.Join(h.wtDir, h.repoName, id)
	require.Eventually(t, func() bool {
		cmd := exec.Command("git", "branch", "--show-current")
		cmd.Dir = wtPath
		out, err := cmd.Output()
		return err == nil && strings.TrimSpace(string(out)) == wantBranch
	}, 5*time.Second, 20*time.Millisecond)

	running := h.readTask(id + ".md")
	assert.Equal(t, wantBranch, running.Branch)
	assert.Equal(t, "main", running.BaseBranch)
	assert.True(t, gitBranchExists(h.repoDir, wantBranch))

	require.NoError(t, running.SetField("status", string(ticket.StatusDone)))
	data, err := running.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	d.handleFileChanged(path)

	h.waitForWorktreeGone(id, 5*time.Second)
	waitForAgentsDone(t, d, 5*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

func TestGeneratedBranchWriteFailureUsesFallback(t *testing.T) {
	h := newHarness(t)
	h.cfg.BranchNaming.Mode = config.BranchNamingModeSlug
	d := h.newDaemon(h.cfg)

	tkt, err := ticket.ParseBytes(fmt.Appendf(nil, `---
id: tst-write
kontora: true
status: todo
path: %s
---
# [kontora] Fix retry persistence
`, h.repoDir))
	require.NoError(t, err)

	blocker := filepath.Join(h.tasksDir, "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o644))
	badPath := filepath.Join(blocker, "tst-write.md")

	d.mu.Lock()
	d.persistGeneratedBranchLocked(h.cfg, testLogger(t), tkt, badPath)
	d.mu.Unlock()

	assert.Empty(t, tkt.Branch)
	assert.Equal(t, "kontora/tst-write", ticketBranch(h.cfg, tkt))
	assert.False(t, gitBranchExists(h.repoDir, "kontora/fix-retry-persistence-tst-write"))
}

func TestWorktreeCleanupOnComplete(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-wt1.md", h.taskMD("tst-wt1", "todo", "one-stage"))

	h.waitForStatus("tst-wt1.md", ticket.StatusDone, 10*time.Second)

	// removeWorktree runs after writeTicket in runTicket, so poll briefly.
	h.waitForWorktreeGone("tst-wt1", 5*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

// TestWorktreeCleanupOnClose tests that setting a ticket to done (via file edit,
// simulating `kontora done`) triggers worktree cleanup through handleFileChanged.
// Uses a ticket that already completed through runTicket (worktree exists) and was
// then set to done externally via a second write.
func TestWorktreeCleanupOnClose(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	// Manually create a worktree to simulate a ticket that had one.
	wtPath := filepath.Join(h.wtDir, h.repoName, "tst-wt2")
	cmd := exec.Command("git", "worktree", "add", "-b", "kontora/tst-wt2", wtPath, "main")
	cmd.Dir = h.repoDir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git worktree add: %s", out)

	// Create a ticket in "open" status — daemon won't pick it up (not todo).
	h.writeTicket("tst-wt2.md", h.taskMD("tst-wt2", "open", "one-stage"))
	time.Sleep(200 * time.Millisecond)

	// Set status to done (simulating `kontora done`). No self-writes to interfere.
	doneContent := strings.Replace(
		h.taskMD("tst-wt2", "open", "one-stage"),
		"status: open",
		"status: done",
		1,
	)
	h.writeTicket("tst-wt2.md", doneContent)

	h.waitForWorktreeGone("tst-wt2", 5*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

// TestWorktreeCleanupOnArchive tests that setting a ticket to archived via a
// file edit (simulating `kontora archive`) triggers the same terminal cleanup
// path as done/cancelled through handleFileChanged.
func TestWorktreeCleanupOnArchive(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	wtPath := filepath.Join(h.wtDir, h.repoName, "tst-arch")
	cmd := exec.Command("git", "worktree", "add", "-b", "kontora/tst-arch", wtPath, "main")
	cmd.Dir = h.repoDir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git worktree add: %s", out)

	// Create a done ticket — daemon won't pick it up (not todo).
	h.writeTicket("tst-arch.md", h.taskMD("tst-arch", "done", "one-stage"))
	time.Sleep(200 * time.Millisecond)

	archivedContent := strings.Replace(
		h.taskMD("tst-arch", "done", "one-stage"),
		"status: done",
		"status: archived",
		1,
	)
	h.writeTicket("tst-arch.md", archivedContent)

	h.waitForWorktreeGone("tst-arch", 5*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

func TestDaemon_ListTickets_HidesNonBoardStatuses(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	parse := func(id, status string) *ticketState {
		md := fmt.Sprintf("---\nid: %s\nkontora: true\nstatus: %s\n---\n# %s\n", id, status, id)
		tk, err := ticket.ParseBytes([]byte(md))
		require.NoError(t, err)
		return &ticketState{ticket: tk, filePath: filepath.Join(h.tasksDir, id+".md")}
	}

	d.tickets["t-todo"] = parse("t-todo", "todo")
	d.tickets["t-done"] = parse("t-done", "done")
	d.tickets["t-cancelled"] = parse("t-cancelled", "cancelled")
	d.tickets["t-arch"] = parse("t-arch", "archived")
	// Foreign statuses from the external ticket CLI map to no board column.
	d.tickets["t-closed"] = parse("t-closed", "closed")
	d.tickets["t-tomb"] = parse("t-tomb", "tombstone")

	ids := make([]string, 0, 3)
	for _, ti := range d.ListTickets() {
		ids = append(ids, ti.ID)
	}
	assert.ElementsMatch(t, []string{"t-todo", "t-done", "t-cancelled"}, ids)

	// Non-board statuses stay reachable by ID.
	for _, id := range []string{"t-arch", "t-closed", "t-tomb"} {
		got, err := d.GetTicket(id)
		require.NoError(t, err, "GetTicket(%s)", id)
		assert.Equal(t, id, got.ID)
	}
}

func TestDaemon_Relations(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	add := func(id, status, fm, title string) {
		md := fmt.Sprintf("---\nid: %s\nkontora: true\nstatus: %s\n%s---\n# %s\n", id, status, fm, title)
		tk, err := ticket.ParseBytes([]byte(md))
		require.NoError(t, err)
		d.tickets[id] = &ticketState{ticket: tk, filePath: filepath.Join(h.tasksDir, id+".md")}
	}

	// rel-main waits on one archived and one absent ticket, is related to one,
	// sits under an epic, and holds up two others.
	add("rel-main", "todo", "deps: [rel-old, rel-gone]\nlinks: [rel-side]\nparent: rel-epic\n", "Main ticket")
	add("rel-old", "archived", "", "Archived blocker")
	add("rel-side", "done", "", "Related work")
	add("rel-epic", "open", "", "The epic")
	add("rel-waiter", "open", "deps: [rel-main]\n", "Waiting on main")
	add("rel-early", "open", "deps: [rel-main, rel-side]\n", "Also waiting")

	detail, err := d.GetTicket("rel-main")
	require.NoError(t, err)

	// A dep the board hides still resolves: the store holds every file on disk.
	require.Len(t, detail.Deps, 2)
	assert.Equal(t, web.TicketRef{ID: "rel-old", Title: "Archived blocker", Status: "archived"}, detail.Deps[0])
	// An id with no ticket left keeps its bare form rather than disappearing.
	assert.Equal(t, web.TicketRef{ID: "rel-gone"}, detail.Deps[1])
	assert.Equal(t, []web.TicketRef{{ID: "rel-side", Title: "Related work", Status: "done"}}, detail.Links)
	require.NotNil(t, detail.Parent)
	assert.Equal(t, web.TicketRef{ID: "rel-epic", Title: "The epic", Status: "open"}, *detail.Parent)
	// blocks is the reverse of deps, which is stored nowhere: sorted by id, and
	// rel-side's own dependent is not confused for rel-main's.
	assert.Equal(t, []web.TicketRef{
		{ID: "rel-early", Title: "Also waiting", Status: "open"},
		{ID: "rel-waiter", Title: "Waiting on main", Status: "open"},
	}, detail.Blocks)

	// The board cards render ids, so the list payload carries no titles and no
	// derived reverse edges.
	var list web.TicketInfo
	for _, ti := range d.ListTickets() {
		if ti.ID == "rel-main" {
			list = ti
		}
	}
	require.Equal(t, "rel-main", list.ID)
	assert.Equal(t, []web.TicketRef{{ID: "rel-old"}, {ID: "rel-gone"}}, list.Deps)
	assert.Equal(t, []web.TicketRef{{ID: "rel-side"}}, list.Links)
	require.NotNil(t, list.Parent)
	assert.Equal(t, web.TicketRef{ID: "rel-epic"}, *list.Parent)
	assert.Empty(t, list.Blocks)

	// A ticket with no relations gets no rows at all.
	plain, err := d.GetTicket("rel-epic")
	require.NoError(t, err)
	assert.Empty(t, plain.Deps)
	assert.Empty(t, plain.Links)
	assert.Nil(t, plain.Parent)
	assert.Empty(t, plain.Blocks)
}

func TestDaemon_ListTickets_OmitsDetailFields(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	md := "---\n" +
		"id: t-detail\n" +
		"kontora: true\n" +
		"status: human_review\n" +
		"last_error: boom\n" +
		"last_log: /tmp/t-detail/stage.log\n" +
		"history:\n" +
		"  - stage: code\n" +
		"    agent: claude\n" +
		"    exit_code: 0\n" +
		"---\n# t-detail\n"
	tk, err := ticket.ParseBytes([]byte(md))
	require.NoError(t, err)
	d.tickets["t-detail"] = &ticketState{ticket: tk, filePath: filepath.Join(h.tasksDir, "t-detail.md")}

	list := d.ListTickets()
	require.Len(t, list, 1)
	li := list[0]
	assert.Empty(t, li.History, "list must omit history")
	assert.Empty(t, li.LastError, "list must omit last_error")
	assert.Empty(t, li.LastLog, "list must omit last_log")
	// The stage bars on in-progress/review cards still need Stages.
	assert.NotNil(t, li.Stages)

	detail, err := d.GetTicket("t-detail")
	require.NoError(t, err)
	assert.Len(t, detail.History, 1, "detail keeps history")
	assert.Equal(t, "boom", detail.LastError)
	assert.Equal(t, "/tmp/t-detail/stage.log", detail.LastLog)
}

func TestDaemon_UpdatedAt_ReflectsFileModTime(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	md := "---\nid: t-mtime\nkontora: true\nstatus: todo\n---\n# t-mtime\n"
	path := h.writeTicket("t-mtime.md", md)
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets["t-mtime"] = newTicketState(tk, path)

	st, err := os.Stat(path)
	require.NoError(t, err)
	got, err := d.GetTicket("t-mtime")
	require.NoError(t, err)
	require.NotNil(t, got.UpdatedAt)
	assert.True(t, got.UpdatedAt.Equal(st.ModTime()), "UpdatedAt should match file mtime")

	// A daemon write rewrites the file (new mtime); UpdatedAt must follow even
	// though the ticketState is reused in place rather than reconstructed.
	require.NoError(t, tk.SetField("status", "paused"))
	require.NoError(t, d.writeTicket(tk, path))

	st2, err := os.Stat(path)
	require.NoError(t, err)
	got2, err := d.GetTicket("t-mtime")
	require.NoError(t, err)
	require.NotNil(t, got2.UpdatedAt)
	assert.True(t, got2.UpdatedAt.Equal(st2.ModTime()), "UpdatedAt should reflect the new mtime after a write")
}

func TestSimpleTask(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-s01.md", simpleTaskMD("tst-s01", "todo", h.repoDir))

	result := h.waitForStatus("tst-s01.md", ticket.StatusDone, 10*time.Second)
	assert.NotNil(t, result.CompletedAt)
	assert.NotEmpty(t, result.Branch)

	cancel()
	require.NoError(t, <-errCh)
}

func TestSimpleTaskBranchOverride(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	md := fmt.Sprintf(`---
id: tst-br01
kontora: true
status: todo
branch: my-custom-branch
path: %s
created: 2026-01-01T00:00:00Z
---
# Test branch override
`, h.repoDir)
	h.writeTicket("tst-br01.md", md)

	result := h.waitForStatus("tst-br01.md", ticket.StatusDone, 10*time.Second)
	assert.Equal(t, "my-custom-branch", result.Branch)

	cancel()
	require.NoError(t, <-errCh)
}

func TestSimpleTaskFailure(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("false", "true")
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-s02.md", simpleTaskMD("tst-s02", "todo", h.repoDir))

	result := h.waitForStatus("tst-s02.md", ticket.StatusPaused, 10*time.Second)
	assert.Nil(t, result.CompletedAt)

	cancel()
	require.NoError(t, <-errCh)
}

func TestSimpleTaskOperationalAppendix(t *testing.T) {
	h := newHarness(t)

	var captured []string
	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		captured = append(captured, p.Args...)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-sapp.md", simpleTaskMD("tst-sapp", "todo", h.repoDir))

	h.waitForStatus("tst-sapp.md", ticket.StatusDone, 10*time.Second)

	found := false
	for _, arg := range captured {
		if strings.Contains(arg, "kontora note tst-sapp") {
			found = true
			break
		}
	}
	assert.True(t, found, "operational appendix not found in args: %v", captured)

	cancel()
	require.NoError(t, <-errCh)
}

func TestSessionLogMaterialization(t *testing.T) {
	h := newHarness(t)

	// Create a fake CLAUDE_CONFIG_DIR with a session JSONL file.
	claudeConfigDir := t.TempDir()
	projectsDir := filepath.Join(claudeConfigDir, "projects", "encoded-path")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755))

	var capturedSessionID string

	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		capturedSessionID = p.SessionID
		// Simulate pipe-pane writing raw PTY output to the log file.
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("raw PTY output from pipe-pane"), 0o644))
		// Write a fake session JSONL to the expected location.
		if capturedSessionID != "" {
			sessionFile := filepath.Join(projectsDir, capturedSessionID+".jsonl")
			jsonl := strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"text","text":"I will check the tests."}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"PASS\n"}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"All tests pass."}]}}`,
			}, "\n")
			require.NoError(t, os.WriteFile(sessionFile, []byte(jsonl), 0o644))
		}
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("claude", "true")
	cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeConfigDir}

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-slog.md", h.taskMD("tst-slog", "todo", "one-stage"))
	h.waitForStatus("tst-slog.md", ticket.StatusDone, 10*time.Second)

	assert.NotEmpty(t, capturedSessionID, "SessionID should be set for Claude agents")

	// Verify the log file contains formatted output, not raw JSONL.
	logFile := filepath.Join(h.logsDir, "tst-slog", "step1.log")
	logContent, err := os.ReadFile(logFile)
	require.NoError(t, err, "log file should exist")

	content := string(logContent)
	assert.Contains(t, content, "I will check the tests.")
	assert.Contains(t, content, "> Bash go test ./...")
	assert.Contains(t, content, "PASS")
	assert.Contains(t, content, "All tests pass.")
	assert.NotContains(t, content, `"type":"assistant"`, "log should be formatted, not raw JSONL")
	assert.NotContains(t, content, "raw PTY output", "JSONL materialization should overwrite pipe-pane output")

	cancel()
	require.NoError(t, <-errCh)
}

func TestSessionLogMaterializationMissing(t *testing.T) {
	h := newHarness(t)

	// Create a CLAUDE_CONFIG_DIR without any session files.
	claudeConfigDir := t.TempDir()
	projectsDir := filepath.Join(claudeConfigDir, "projects", "encoded-path")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755))

	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		// Simulate pipe-pane writing raw PTY output to the log file.
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("raw PTY fallback output"), 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("claude", "true")
	cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeConfigDir}

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-miss.md", h.taskMD("tst-miss", "todo", "one-stage"))

	// Ticket should still complete even if session JSONL is missing.
	h.waitForStatus("tst-miss.md", ticket.StatusDone, 10*time.Second)

	// Verify the raw PTY output survives as fallback when JSONL is missing.
	logFile := filepath.Join(h.logsDir, "tst-miss", "step1.log")
	logContent, err := os.ReadFile(logFile)
	require.NoError(t, err, "log file should exist from pipe-pane")
	assert.Contains(t, string(logContent), "raw PTY fallback output", "pipe-pane output should survive when JSONL materialization fails")

	cancel()
	require.NoError(t, <-errCh)
}

func TestPipelinePausesOnClaudeAPIError(t *testing.T) {
	h := newHarness(t)

	claudeConfigDir := t.TempDir()
	projectsDir := filepath.Join(claudeConfigDir, "projects", "encoded-path")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755))

	// Claude hits a quota limit: it ends the turn (exit 0) but writes a
	// synthetic isApiErrorMessage entry to the session JSONL.
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		require.NotEmpty(t, p.SessionID, "claude agent should get a session id")
		sessionFile := filepath.Join(projectsDir, p.SessionID+".jsonl")
		jsonl := strings.Join([]string{
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Starting work."}]}}`,
			`{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your limit · resets 5pm (Europe/Amsterdam)"}]}}`,
		}, "\n")
		require.NoError(t, os.WriteFile(sessionFile, []byte(jsonl), 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("claude", "claude")
	cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeConfigDir}

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-quota.md", h.taskMD("tst-quota", "todo", "two-stage"))

	result := h.waitForStatus("tst-quota.md", ticket.StatusPaused, 10*time.Second)
	// The clean exit must not be read as success: the ticket stays on the
	// first stage instead of advancing to step2.
	assert.Equal(t, "step1", result.Stage)
	assert.Empty(t, result.History, "a quota failure should not record a success history entry")
	assert.Contains(t, result.LastError, "hit your limit")

	cancel()
	require.NoError(t, <-errCh)
}

func TestPipelinePausesOnFailurePattern(t *testing.T) {
	h := newHarness(t)

	// A non-claude agent swallows an error into a zero exit code; the
	// configured failure_patterns catches it in the output log.
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("working...\nError: quota exceeded for today\n"), 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("some-agent", "some-agent")
	cfg.Agents["agent1"] = config.Agent{Binary: "some-agent", FailurePatterns: []string{"(?i)quota exceeded"}}

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-pat.md", h.taskMD("tst-pat", "todo", "one-stage"))

	result := h.waitForStatus("tst-pat.md", ticket.StatusPaused, 10*time.Second)
	assert.Contains(t, result.LastError, "failure pattern")

	cancel()
	require.NoError(t, <-errCh)
}

func TestFailurePatternNotPoisonedByPreviousAttempt(t *testing.T) {
	h := newHarness(t)

	// Attempt 1 fails (non-zero) and appends an error line matching the pattern;
	// attempt 2 appends clean output and exits 0. Because stage logs accumulate
	// across retries, detection must only scan the current run's output, or the
	// clean retry would keep matching attempt 1's stale error and pause forever.
	var attempt int
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		attempt++
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		f, err := os.OpenFile(p.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		require.NoError(t, err)
		defer f.Close()
		if attempt == 1 {
			_, _ = f.WriteString("attempt 1\nError: quota exceeded for today\n")
			return process.Result{ExitCode: 1, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
		}
		_, _ = f.WriteString("attempt 2\nall good\n")
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("some-agent", "some-agent")
	cfg.Agents["agent1"] = config.Agent{Binary: "some-agent", FailurePatterns: []string{"(?i)quota exceeded"}}

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// retry-stage: on_failure retry (max_retries 1), on_success done.
	h.writeTicket("tst-poison.md", h.taskMD("tst-poison", "todo", "retry-stage"))

	// Attempt 1 exits non-zero -> retry; attempt 2 exits 0 with a clean log.
	// The stale attempt-1 error must not trip detection on attempt 2.
	h.waitForStatus("tst-poison.md", ticket.StatusDone, 10*time.Second)
	assert.Equal(t, 2, attempt)

	cancel()
	require.NoError(t, <-errCh)
}

func TestNonClaudeAgentStillUsesPipePaneLogging(t *testing.T) {
	h := newHarness(t)

	var capturedParams RunnerParams
	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		capturedParams = p
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("some-agent", "true")
	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-ncla.md", h.taskMD("tst-ncla", "todo", "one-stage"))
	h.waitForStatus("tst-ncla.md", ticket.StatusDone, 10*time.Second)

	// Non-Claude agent should have empty SessionID.
	assert.Empty(t, capturedParams.SessionID, "non-Claude agent should not have SessionID")
	assert.False(t, capturedParams.Interactive, "non-Claude agent should not be interactive")

	cancel()
	require.NoError(t, <-errCh)
}

func TestExitTailUsesOutputAttribute(t *testing.T) {
	h := newHarness(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		// Write log content so tailFile finds something.
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("agent error output"), 0o644))
		return process.Result{ExitCode: 1, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("some-agent", "true")
	d := New(cfg,
		WithLogger(logger),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-otail.md", h.taskMD("tst-otail", "todo", "one-stage"))
	h.waitForStatus("tst-otail.md", ticket.StatusPaused, 10*time.Second)

	cancel()
	require.NoError(t, <-errCh)

	logged := logBuf.String()
	assert.Contains(t, logged, "output=", "exit tail should use 'output' attribute")
	assert.NotContains(t, logged, "stderr=", "exit tail should not use 'stderr' attribute")
}

func TestWebServerPortBound(t *testing.T) {
	h := newHarness(t)

	// Bind the port before starting the daemon.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	cfg := h.defaultConfig("true", "true")
	enabled := true
	cfg.Web.Enabled = &enabled
	cfg.Web.Host = "127.0.0.1"
	cfg.Web.Port = port
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	// Daemon should still start and process tickets even though web port is bound.
	h.writeTicket("tst-wp.md", h.taskMD("tst-wp", "todo", "one-stage"))
	h.waitForStatus("tst-wp.md", ticket.StatusDone, 10*time.Second)

	cancel()
	require.NoError(t, <-errCh)
}

func TestSimpleTaskCompletedAt(t *testing.T) {
	h := newHarness(t)

	// Use a runner that returns zero ExitedAt to test the fallback.
	zeroTimeRunner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		return process.Result{
			ExitCode:  0,
			StartedAt: time.Now(),
			// ExitedAt intentionally left zero.
		}, nil
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(zeroTimeRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-cat.md", simpleTaskMD("tst-cat", "todo", h.repoDir))
	result := h.waitForStatus("tst-cat.md", ticket.StatusDone, 10*time.Second)

	// completed_at should not be zero even though ExitedAt was zero.
	require.NotNil(t, result.CompletedAt, "completed_at should be set")
	assert.False(t, result.CompletedAt.IsZero(), "completed_at should not be zero")

	cancel()
	require.NoError(t, <-errCh)
}

func TestPauseTicketWritesReasonNote(t *testing.T) {
	h := newHarness(t)

	// Runner that always fails with ExitCode=-1 triggers the
	// "runner failed" pauseTicket path (pre-spawn failure).
	failRunner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		return process.Result{ExitCode: -1}, fmt.Errorf("connection refused")
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(failRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-pr1.md", h.taskMD("tst-pr1", "todo", "one-stage"))

	result := h.waitForStatus("tst-pr1.md", ticket.StatusPaused, 10*time.Second)
	assert.Contains(t, result.Body, "## Notes")
	assert.Contains(t, result.Body, "runner failed: connection refused")

	cancel()
	require.NoError(t, <-errCh)
}

// TestAgentBinaryMissing verifies that a missing agent binary is reported
// up-front with a clear message, instead of surfacing later as a cryptic
// "agent exited too quickly" once the tmux shell wrapper fails to exec it.
func TestAgentBinaryMissing(t *testing.T) {
	h := newHarness(t)

	var runnerCalled bool
	trackingRunner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		runnerCalled = true
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	failingLookup := func(binary string) (string, error) {
		return "", fmt.Errorf("%q not found in $PATH or common locations", binary)
	}

	h.cfg.Agents["agent1"] = config.Agent{Binary: "definitely-not-a-real-binary"}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(trackingRunner),
		WithAgentLookup(failingLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-bnf.md", h.taskMD("tst-bnf", "todo", "one-stage"))
	result := h.waitForStatus("tst-bnf.md", ticket.StatusPaused, 10*time.Second)

	assert.False(t, runnerCalled, "runner must not run when binary lookup fails")
	assert.Contains(t, result.LastError, "agent binary unavailable")
	assert.Contains(t, result.LastError, "definitely-not-a-real-binary")

	cancel()
	require.NoError(t, <-errCh)
}

func TestRunnerError_CapturesLogTailAndPath(t *testing.T) {
	h := newHarness(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	const marker = "boom-marker-output"
	failRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		if p.LogFile != "" {
			if err := os.MkdirAll(filepath.Dir(p.LogFile), 0o755); err != nil {
				return process.Result{ExitCode: -1}, fmt.Errorf("create log dir: %w", err)
			}
			if err := os.WriteFile(p.LogFile, []byte(marker+"\n"), 0o644); err != nil {
				return process.Result{ExitCode: -1}, fmt.Errorf("write log file: %w", err)
			}
		}
		return process.Result{ExitCode: -1}, fmt.Errorf("boom")
	}

	d := New(h.cfg,
		WithLogger(logger),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(failRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-le5.md", h.taskMD("tst-le5", "todo", "one-stage"))
	result := h.waitForStatus("tst-le5.md", ticket.StatusPaused, 10*time.Second)

	assert.Contains(t, result.LastError, "runner failed: boom")
	assert.NotContains(t, result.LastError, "see log:")

	assert.NotEmpty(t, result.LastLog)
	assert.Contains(t, result.LastLog, filepath.Join(h.logsDir, "tst-le5"))

	assert.Contains(t, logBuf.String(), "runner failed")
	assert.Contains(t, logBuf.String(), marker)

	cancel()
	require.NoError(t, <-errCh)
}

func TestAgentOverride_Pipeline(t *testing.T) {
	h := newHarness(t)
	// agent1=true (default for step1), agent2=true
	// But we'll capture what binary is actually spawned.
	var capturedBinary string
	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		capturedBinary = p.Binary
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("true", "true")
	cfg.Agents["opus"] = config.Agent{Binary: "opus-binary"}

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Ticket with agent override — should use "opus" instead of pipeline's "agent1".
	h.writeTicket("tst-ao1.md", fmt.Sprintf(`---
id: tst-ao1
kontora: true
status: todo
pipeline: one-stage
agent: opus
path: %s
created: 2026-01-01T00:00:00Z
---
# Agent override test
`, h.repoDir))

	h.waitForStatus("tst-ao1.md", ticket.StatusDone, 10*time.Second)
	assert.Equal(t, "opus-binary", capturedBinary)

	// Verify history records the overridden agent.
	result := h.readTask("tst-ao1.md")
	require.Len(t, result.History, 1)
	assert.Equal(t, "opus", result.History[0].Agent)

	cancel()
	require.NoError(t, <-errCh)
}

func TestAgentOverride_SimpleTask(t *testing.T) {
	h := newHarness(t)

	var capturedBinary string
	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		capturedBinary = p.Binary
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("true", "true")
	cfg.Agents["opus"] = config.Agent{Binary: "opus-binary"}

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(capturingRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Simple ticket (no pipeline) with agent override.
	h.writeTicket("tst-ao2.md", fmt.Sprintf(`---
id: tst-ao2
kontora: true
status: todo
agent: opus
path: %s
created: 2026-01-01T00:00:00Z
---
# Simple agent override
`, h.repoDir))

	h.waitForStatus("tst-ao2.md", ticket.StatusDone, 10*time.Second)
	assert.Equal(t, "opus-binary", capturedBinary)

	cancel()
	require.NoError(t, <-errCh)
}

func TestAgentOverride_UnknownAgent(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Ticket with unknown agent should be paused.
	h.writeTicket("tst-ao3.md", fmt.Sprintf(`---
id: tst-ao3
kontora: true
status: todo
pipeline: one-stage
agent: nonexistent
path: %s
created: 2026-01-01T00:00:00Z
---
# Unknown agent test
`, h.repoDir))

	result := h.waitForStatus("tst-ao3.md", ticket.StatusPaused, 10*time.Second)
	assert.Contains(t, result.Body, "unknown agent")

	cancel()
	require.NoError(t, <-errCh)
}

func TestAgentOverride_UnknownAgent_SimpleTask(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Simple ticket with unknown agent should be paused.
	h.writeTicket("tst-ao4.md", fmt.Sprintf(`---
id: tst-ao4
kontora: true
status: todo
agent: nonexistent
path: %s
created: 2026-01-01T00:00:00Z
---
# Unknown agent simple
`, h.repoDir))

	result := h.waitForStatus("tst-ao4.md", ticket.StatusPaused, 10*time.Second)
	assert.Contains(t, result.Body, "unknown agent")

	cancel()
	require.NoError(t, <-errCh)
}

// modelFlagCount counts the exact model flags in an argv. An override replaces
// the agent's own flag, so a second one would mean the winner depends on how
// the CLI resolves a repeated flag.
func modelFlagCount(args []string) int {
	n := 0
	for _, arg := range args {
		if arg == "--model" || strings.HasPrefix(arg, "--model=") {
			n++
		}
	}
	return n
}

// modelArg returns the value of the single model flag in an argv.
func modelArg(t *testing.T, args []string) string {
	t.Helper()
	require.Equal(t, 1, modelFlagCount(args), "want exactly one --model in %v", args)
	i := slices.Index(args, "--model")
	require.GreaterOrEqual(t, i, 0, "--model in the joined form: %v", args)
	require.Less(t, i+1, len(args), "--model has no value: %v", args)
	return args[i+1]
}

// TestStageModel runs one pipeline stage per case and reads the model out of
// the argv the agent was spawned with.
func TestStageModel(t *testing.T) {
	cases := []struct {
		name      string
		agentName string
		agent     config.Agent
		model     config.Model
		want      string
	}{
		{
			name:      "map keyed by agent kind",
			agentName: "pi-grafana-opus-5",
			agent:     config.Agent{Binary: "pi"},
			model:     config.Model{ByAgent: map[string]string{"pi": "anthropic/claude-haiku-4-5"}},
			want:      "anthropic/claude-haiku-4-5",
		},
		{
			name:      "map keyed by agent name",
			agentName: "pi-grafana-opus-5",
			agent:     config.Agent{Binary: "pi"},
			model:     config.Model{ByAgent: map[string]string{"pi-grafana-opus-5": "anthropic/claude-haiku-4-5"}},
			want:      "anthropic/claude-haiku-4-5",
		},
		{
			name:  "scalar",
			agent: config.Agent{Binary: "claude"},
			model: config.Model{Any: "haiku"},
			want:  "haiku",
		},
		{
			name:  "replaces the agent's own model",
			agent: config.Agent{Binary: "claude", Args: []string{"--dangerously-skip-permissions", "--model", "opus"}},
			model: config.Model{Any: "haiku"},
			want:  "haiku",
		},
		{
			name:  "replaces the model behind a wrapper",
			agent: config.Agent{Binary: "nono", Args: []string{"run", "--profile", "agent", "--", "pi", "--model", "anthropic/claude-opus-5"}},
			model: config.Model{ByAgent: map[string]string{"pi": "anthropic/claude-haiku-4-5"}},
			want:  "anthropic/claude-haiku-4-5",
		},
		{
			name:  "no model keeps the agent's own",
			agent: config.Agent{Binary: "claude", Args: []string{"--model", "opus"}},
			want:  "opus",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			agentName := cmp.Or(tc.agentName, "agent1")
			h.cfg.Agents[agentName] = tc.agent
			h.cfg.Stages["step1"] = config.Stage{Prompt: "do step1 for {{ .Ticket.ID }}", Model: tc.model}
			h.cfg.Pipelines["one-stage"] = config.Pipeline{
				{Stage: "step1", Agent: agentName, OnSuccess: "done", OnFailure: "pause"},
			}

			var captured RunnerParams
			d := New(h.cfg,
				WithLogger(testLogger(t)),
				WithDebounce(50*time.Millisecond),
				WithLockPath(h.lockPath),
				WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
					captured = p
					return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
				}),
				WithAgentLookup(passthroughAgentLookup),
				WithSkipOrphanCleanup(),
			)
			stop := runDaemon(t, d)

			h.writeTicket("tst-sm.md", h.taskMD("tst-sm", "todo", "one-stage"))
			h.waitForStatus("tst-sm.md", ticket.StatusDone, 10*time.Second)
			stop()

			assert.Equal(t, tc.want, modelArg(t, captured.Args))
			assert.Less(t, slices.Index(captured.Args, "--model"),
				slices.Index(captured.Args, renderedPrompt(captured)),
				"--model must come before the prompt: %v", captured.Args)
		})
	}
}

// TestStageModelUnsupportedAgentPauses: a ticket-level agent that takes no
// --model reaches a stage that names one. Config validation cannot see this
// pair, because the ticket's agent replaces the step's at spawn time.
func TestStageModelUnsupportedAgentPauses(t *testing.T) {
	h := newHarness(t)
	h.cfg.Agents["programmator"] = config.Agent{Binary: "programmator"}
	h.cfg.Stages["step1"] = config.Stage{Prompt: "do step1", Model: config.Model{Any: "haiku"}}

	spawned := 0
	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(func(_ context.Context, _ RunnerParams) (process.Result, error) {
			spawned++
			return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
		}),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)
	stop := runDaemon(t, d)

	h.writeTicket("tst-smu.md", fmt.Sprintf(`---
id: tst-smu
kontora: true
status: todo
pipeline: one-stage
agent: programmator
path: %s
created: 2026-01-01T00:00:00Z
---
# Stage model on an agent that takes no model flag
`, h.repoDir))

	result := h.waitForStatus("tst-smu.md", ticket.StatusPaused, 10*time.Second)
	stop()

	assert.Contains(t, result.Body, `model "haiku"`)
	assert.Contains(t, result.Body, "takes no --model")
	assert.Zero(t, spawned, "the stage must not run")
}

func TestBuildTicketInfo_AgentOverride(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Pipeline ticket with agent override.
	h.writeTicket("tst-bi1.md", fmt.Sprintf(`---
id: tst-bi1
kontora: true
status: open
pipeline: one-stage
agent: agent2
path: %s
stage: step1
created: 2026-01-01T00:00:00Z
---
# BuildTicketInfo test
`, h.repoDir))

	require.Eventually(t, func() bool {
		info, err := d.GetTicket("tst-bi1")
		return err == nil && info.Agent == "agent2"
	}, 5*time.Second, 50*time.Millisecond, "agent should be overridden in TicketInfo")

	cancel()
	require.NoError(t, <-errCh)
}

func TestPauseTicketNoPathWritesReasonNote(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	// Ticket without path triggers "resolve path failed" pauseTicket path.
	h.writeTicket("tst-pr2.md", `---
id: tst-pr2
kontora: true
status: todo
pipeline: one-stage
created: 2026-01-01T00:00:00Z
---
# Test ticket tst-pr2
`)

	result := h.waitForStatus("tst-pr2.md", ticket.StatusPaused, 10*time.Second)
	assert.Contains(t, result.Body, "## Notes")
	assert.Contains(t, result.Body, "resolve path failed:")

	cancel()
	require.NoError(t, <-errCh)
}

func TestPauseTicket_LastError(t *testing.T) {
	h := newHarness(t)

	failRunner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		return process.Result{ExitCode: -1}, fmt.Errorf("connection refused")
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(failRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-le1.md", h.taskMD("tst-le1", "todo", "one-stage"))
	h.waitForStatus("tst-le1.md", ticket.StatusPaused, 10*time.Second)

	info, err := d.GetTicket("tst-le1")
	require.NoError(t, err)
	assert.Equal(t, "paused", info.Status)
	assert.Contains(t, info.LastError, "runner failed: connection refused")
	assert.NotContains(t, info.LastError, "see log:")
	assert.NotEmpty(t, info.LastLog)

	// Verify last_error is persisted in frontmatter.
	tkt := h.readTask("tst-le1.md")
	assert.Contains(t, tkt.LastError, "runner failed: connection refused")
	assert.NotContains(t, tkt.LastError, "see log:")
	assert.NotEmpty(t, tkt.LastLog)

	cancel()
	require.NoError(t, <-errCh)
}

func TestRetryTicket_ClearsLastError(t *testing.T) {
	h := newHarness(t)

	var attempt int
	runner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		attempt++
		if attempt == 1 {
			return process.Result{ExitCode: -1}, fmt.Errorf("first attempt fails")
		}
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-le2.md", h.taskMD("tst-le2", "todo", "one-stage"))
	h.waitForStatus("tst-le2.md", ticket.StatusPaused, 10*time.Second)

	// Verify last_error is set in frontmatter.
	info, err := d.GetTicket("tst-le2")
	require.NoError(t, err)
	assert.NotEmpty(t, info.LastError)
	tkt := h.readTask("tst-le2.md")
	assert.NotEmpty(t, tkt.LastError)

	// Retry should clear last_error from frontmatter.
	require.NoError(t, d.RetryTicket("tst-le2"))

	info, err = d.GetTicket("tst-le2")
	require.NoError(t, err)
	assert.Empty(t, info.LastError)
	tkt = h.readTask("tst-le2.md")
	assert.Empty(t, tkt.LastError)

	cancel()
	require.NoError(t, <-errCh)
}

func TestPauseTicket_ManualPause_NoLastError(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-le3.md", h.taskMD("tst-le3", "todo", "one-stage"))
	h.waitForStatus("tst-le3.md", ticket.StatusInProgress, 5*time.Second)

	require.NoError(t, d.PauseTicket("tst-le3"))
	h.waitForStatus("tst-le3.md", ticket.StatusPaused, 5*time.Second)

	info, err := d.GetTicket("tst-le3")
	require.NoError(t, err)
	assert.Empty(t, info.LastError)

	cancel()
	require.NoError(t, <-errCh)
}

func TestHandleAgentExit_PipelinePause_SetsLastError(t *testing.T) {
	h := newHarness(t)

	// Use a runner that exits with code 1 — on_failure=pause will trigger ActionPause.
	failRunner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		return process.Result{ExitCode: 1, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("true", "true")
	// Set on_failure=pause for step1.
	p := cfg.Pipelines["one-stage"]
	p[0].OnFailure = "pause"
	cfg.Pipelines["one-stage"] = p

	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(failRunner),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-le4.md", h.taskMD("tst-le4", "todo", "one-stage"))
	h.waitForStatus("tst-le4.md", ticket.StatusPaused, 10*time.Second)

	require.Eventually(t, func() bool {
		info, err := d.GetTicket("tst-le4")
		return err == nil && info.LastError != ""
	}, 5*time.Second, 50*time.Millisecond)

	info, err := d.GetTicket("tst-le4")
	require.NoError(t, err)
	assert.Contains(t, info.LastError, "agent exited with code 1")
	assert.Contains(t, info.LastError, "stage: step1")
	assert.NotContains(t, info.LastError, "see log:")
	assert.NotEmpty(t, info.LastLog)

	// Verify last_error is persisted in frontmatter.
	tkt := h.readTask("tst-le4.md")
	assert.Contains(t, tkt.LastError, "agent exited with code 1")
	assert.Contains(t, tkt.LastError, "stage: step1")
	assert.NotContains(t, tkt.LastError, "see log:")
	assert.NotEmpty(t, tkt.LastLog)

	cancel()
	require.NoError(t, <-errCh)
}

func TestAutoPickUpDisabled(t *testing.T) {
	t.Run("startup scan skips todo tickets", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.AutoPickUp = new(false)

		// Write a todo ticket BEFORE starting daemon.
		h.writeTicket("tst-ap1.md", h.taskMD("tst-ap1", "todo", "one-stage"))

		d := h.newDaemon(h.cfg)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()

		time.Sleep(700 * time.Millisecond)

		result := h.readTask("tst-ap1.md")
		assert.Equal(t, ticket.StatusTodo, result.Status, "ticket should remain todo when auto_pick_up=false")

		cancel()
		require.NoError(t, <-errCh)
	})

	t.Run("new todo ticket not picked up", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.AutoPickUp = new(false)

		d := h.newDaemon(h.cfg)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()

		time.Sleep(200 * time.Millisecond)

		h.writeTicket("tst-ap2.md", h.taskMD("tst-ap2", "todo", "one-stage"))

		time.Sleep(500 * time.Millisecond)
		result := h.readTask("tst-ap2.md")
		assert.Equal(t, ticket.StatusTodo, result.Status, "new ticket should not be auto picked up")

		cancel()
		require.NoError(t, <-errCh)
	})

	t.Run("status transition to todo also blocked", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.AutoPickUp = new(false)

		d := h.newDaemon(h.cfg)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()

		time.Sleep(200 * time.Millisecond)

		// Write ticket as open first.
		h.writeTicket("tst-ap3.md", h.taskMD("tst-ap3", "open", "one-stage"))
		time.Sleep(200 * time.Millisecond)

		// Transition open→todo — should NOT be picked up.
		h.writeTicket("tst-ap3.md", h.taskMD("tst-ap3", "todo", "one-stage"))

		time.Sleep(500 * time.Millisecond)
		result := h.readTask("tst-ap3.md")
		assert.Equal(t, ticket.StatusTodo, result.Status, "open→todo transition should not enqueue when auto_pick_up=false")

		cancel()
		require.NoError(t, <-errCh)
	})

	t.Run("pipeline transitions still work", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.AutoPickUp = new(false)
		h.cfg.Web = config.Web{Enabled: new(false)}

		d := h.newDaemon(h.cfg)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()

		time.Sleep(200 * time.Millisecond)

		// Write a two-stage ticket and kick it off via RunTicket.
		h.writeTicket("tst-ap5.md", h.taskMD("tst-ap5", "todo", "two-stage"))
		time.Sleep(300 * time.Millisecond)
		require.NoError(t, d.RunTicket("tst-ap5"))

		// The pipeline should advance through both stages automatically.
		result := h.waitForStatus("tst-ap5.md", ticket.StatusDone, 10*time.Second)
		require.Len(t, result.History, 2)

		cancel()
		require.NoError(t, <-errCh)
	})

	t.Run("RunTicket enqueues todo ticket", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.AutoPickUp = new(false)
		h.cfg.Web = config.Web{Enabled: new(false)}

		d := h.newDaemon(h.cfg)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		errCh := make(chan error, 1)
		go func() { errCh <- d.Run(ctx) }()

		time.Sleep(200 * time.Millisecond)

		h.writeTicket("tst-ap4.md", h.taskMD("tst-ap4", "todo", "one-stage"))
		time.Sleep(300 * time.Millisecond)

		result := h.readTask("tst-ap4.md")
		require.Equal(t, ticket.StatusTodo, result.Status, "should still be todo before RunTicket")

		require.NoError(t, d.RunTicket("tst-ap4"))

		result = h.waitForStatus("tst-ap4.md", ticket.StatusDone, 10*time.Second)
		require.Len(t, result.History, 1)

		cancel()
		require.NoError(t, <-errCh)
	})
}

func TestSchedulerRejectsConcurrentSameBranch(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("sleep", "sleep")
	cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"5"}}
	cfg.Stages["step1"] = config.Stage{Prompt: ""}
	d := h.newDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)

	ticketMD := func(id string) string {
		return fmt.Sprintf(`---
id: %s
kontora: true
status: todo
pipeline: one-stage
branch: feat/stacked
path: %s
created: 2026-01-01T00:00:00Z
---
# Same-branch ticket %s
`, id, h.repoDir, id)
	}

	// Ticket A claims the branch and stays in_progress.
	h.writeTicket("tst-ca.md", ticketMD("tst-ca"))
	h.waitForStatus("tst-ca.md", ticket.StatusInProgress, 5*time.Second)

	// Ticket B on the same branch must be paused before any agent is spawned.
	h.writeTicket("tst-cb.md", ticketMD("tst-cb"))

	result := h.waitForStatus("tst-cb.md", ticket.StatusPaused, 5*time.Second)
	assert.Contains(t, result.LastError, "branch feat/stacked in use by ticket tst-ca")

	cancel()
	<-errCh
}

// TestSelfWriteSuppression covers how the daemon tells its own ticket writes
// from everyone else's. One run can write a ticket twice inside one debounce
// interval, so the record must survive a match, and the first differing
// content must still read as an external edit.
func TestSelfWriteSuppression(t *testing.T) {
	cases := []struct {
		name string
		// run performs the writes under test and reports what isSelfWrite
		// answered for each watcher event it stood in for.
		run  func(t *testing.T, d *Daemon, path string) []bool
		want []bool
	}{
		{
			name: "a path the daemon never wrote",
			run: func(t *testing.T, d *Daemon, path string) []bool {
				writeFile(t, path, "external")
				return []bool{d.isSelfWrite(path)}
			},
			want: []bool{false},
		},
		{
			name: "one write, one event",
			run: func(t *testing.T, d *Daemon, path string) []bool {
				d.recordSelfWrite(path, []byte("daemon"))
				writeFile(t, path, "daemon")
				return []bool{d.isSelfWrite(path)}
			},
			want: []bool{true},
		},
		{
			name: "two writes reported as one event and as two",
			run: func(t *testing.T, d *Daemon, path string) []bool {
				d.recordSelfWrite(path, []byte("first"))
				writeFile(t, path, "first")
				d.recordSelfWrite(path, []byte("second"))
				writeFile(t, path, "second")
				return []bool{d.isSelfWrite(path), d.isSelfWrite(path)}
			},
			want: []bool{true, true},
		},
		{
			name: "an external edit after two daemon writes",
			run: func(t *testing.T, d *Daemon, path string) []bool {
				d.recordSelfWrite(path, []byte("first"))
				writeFile(t, path, "first")
				d.recordSelfWrite(path, []byte("second"))
				writeFile(t, path, "second")
				first := d.isSelfWrite(path)
				writeFile(t, path, "external")
				return []bool{first, d.isSelfWrite(path), d.isSelfWrite(path)}
			},
			want: []bool{true, false, false},
		},
		{
			name: "a creation whose content the daemon does not know",
			run: func(t *testing.T, d *Daemon, path string) []bool {
				d.recordSelfWriteBlind(path)
				writeFile(t, path, "written by the cli")
				first := d.isSelfWrite(path)
				writeFile(t, path, "external")
				return []bool{first, d.isSelfWrite(path)}
			},
			want: []bool{true, false},
		},
		{
			name: "a removed file is forgotten",
			run: func(t *testing.T, d *Daemon, path string) []bool {
				d.recordSelfWrite(path, []byte("daemon"))
				writeFile(t, path, "daemon")
				d.forgetSelfWrite(path)
				return []bool{d.isSelfWrite(path)}
			},
			want: []bool{false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			assert.Equal(t, tc.want, tc.run(t, d, filepath.Join(h.tasksDir, "tst-sw.md")))
		})
	}
}

// TestRemovalIsNeverSuppressed: deleting a ticket the daemon has just written
// must still unregister it. The daemon's record of its own write answers for
// changes to the file, never for its disappearance.
func TestRemovalIsNeverSuppressed(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// status=open keeps the ticket out of the queue, so the only writes to the
	// file are the creation the daemon made itself.
	info, err := d.CreateTicket(web.CreateTicketRequest{Title: "Doomed", Path: h.repoDir, Status: "open"})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(h.tasksDir, info.ID+".md")))

	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, known := d.tickets[info.ID]
		return !known
	}, 5*time.Second, 20*time.Millisecond, "the deleted ticket must leave the registry")

	cancel()
	require.NoError(t, <-errCh)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// TestStageSummaryCapture covers summary capture at stage exit: a summary the
// agent writes into the ticket file during the run lands in both the top-level
// field and the stage's history entry.
func TestStageSummaryCapture(t *testing.T) {
	h := newHarness(t)
	ticketPath := filepath.Join(h.tasksDir, "tst-sum.md")

	runner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		tk, err := ticket.ParseFile(ticketPath)
		require.NoError(t, err)
		require.NoError(t, tk.SetField("summary", "agent-written summary"))
		data, err := tk.Marshal()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(ticketPath, data, 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-sum.md", h.taskMD("tst-sum", "todo", "one-stage"))

	result := h.waitForStatus("tst-sum.md", ticket.StatusDone, 10*time.Second)
	assert.Equal(t, "agent-written summary", result.Summary)
	require.Len(t, result.History, 1)
	assert.Equal(t, "agent-written summary", result.History[0].Summary)

	cancel()
	require.NoError(t, <-errCh)
}

// TestStageSummaryFallback covers the fallback: when the agent writes no
// summary, the last assistant message from the session JSONL fills the field.
func TestStageSummaryFallback(t *testing.T) {
	h := newHarness(t)

	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		require.NotEmpty(t, p.SessionDir, "SessionDir should be set for pi agents")
		require.NoError(t, os.MkdirAll(p.SessionDir, 0o755))
		jsonl := strings.Join([]string{
			`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Working on it."}]}}`,
			`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Implemented the fix and ran the tests."}]}}`,
		}, "\n")
		require.NoError(t, os.WriteFile(filepath.Join(p.SessionDir, "session.jsonl"), []byte(jsonl), 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("pi", "pi")
	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-sfb.md", h.taskMD("tst-sfb", "todo", "one-stage"))

	result := h.waitForStatus("tst-sfb.md", ticket.StatusDone, 10*time.Second)
	assert.Equal(t, "Implemented the fix and ran the tests.", result.Summary)
	require.Len(t, result.History, 1)
	assert.Equal(t, "Implemented the fix and ran the tests.", result.History[0].Summary)

	cancel()
	require.NoError(t, <-errCh)
}

// TestStageSummaryPerStageRetention: a later stage that records nothing does
// not erase the earlier stage's history summary, and the top-level field is
// left empty rather than holding the earlier stage's text.
func TestStageSummaryPerStageRetention(t *testing.T) {
	h := newHarness(t)
	ticketPath := filepath.Join(h.tasksDir, "tst-ret.md")

	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		stage := strings.TrimSuffix(filepath.Base(p.LogFile), ".log")
		if stage == "step1" {
			tk, err := ticket.ParseFile(ticketPath)
			require.NoError(t, err)
			require.NoError(t, tk.SetField("summary", "step1 summary"))
			data, err := tk.Marshal()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(ticketPath, data, 0o644))
		}
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-ret.md", h.taskMD("tst-ret", "todo", "two-stage"))

	result := h.waitForStatus("tst-ret.md", ticket.StatusDone, 10*time.Second)
	assert.Empty(t, result.Summary)
	require.Len(t, result.History, 2)
	assert.Equal(t, "step1 summary", result.History[0].Summary)
	assert.Empty(t, result.History[1].Summary)

	cancel()
	require.NoError(t, <-errCh)
}

// piSessionRunner writes a pi session JSONL into the run's session directory,
// standing in for an agent that produces a structured session record.
func piSessionRunner(exitCode int) RunnerFunc {
	return func(_ context.Context, p RunnerParams) (process.Result, error) {
		if p.SessionDir != "" {
			_ = os.MkdirAll(p.SessionDir, 0o755)
			line := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}` + "\n"
			_ = os.WriteFile(filepath.Join(p.SessionDir, "session.jsonl"), []byte(line), 0o644)
		}
		now := time.Now()
		return process.Result{ExitCode: exitCode, StartedAt: now, ExitedAt: now}, nil
	}
}

func TestStageRunKeysHistoryAndEventsSidecar(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("pi", "pi")
	d := New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(piSessionRunner(1)),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-run.md", h.taskMD("tst-run", "todo", "retry-stage"))

	// max_retries=1 runs step1 twice, so its two history entries carry run
	// keys 0 and 1 and each run writes its own sidecar.
	result := h.waitForStatus("tst-run.md", ticket.StatusPaused, 10*time.Second)
	require.Len(t, result.History, 2)
	assert.Equal(t, 0, result.History[0].Run)
	assert.Equal(t, 1, result.History[1].Run)

	logDir := filepath.Join(h.logsDir, "tst-run")
	assert.FileExists(t, filepath.Join(logDir, "step1.0.events.json"))
	assert.FileExists(t, filepath.Join(logDir, "step1.1.events.json"))
	assert.FileExists(t, filepath.Join(logDir, "step1.log"),
		"the plaintext log keeps its unsuffixed name")

	data, err := os.ReadFile(filepath.Join(logDir, "step1.1.events.json"))
	require.NoError(t, err)
	var tape logfmt.Tape
	require.NoError(t, json.Unmarshal(data, &tape))
	assert.Equal(t, "pi", tape.Agent)
	require.Len(t, tape.Events, 1)
	assert.Equal(t, "working", tape.Events[0].Text)

	cancel()
	require.NoError(t, <-errCh)
}

// TestActivityFollowsTheRunningStage drives the activity endpoint from inside
// the run, which is the only way to observe the registration: the entry exists
// for exactly as long as the runner call. It also puts a reader on one
// goroutine and the run on another, which is what -race checks here.
func TestActivityFollowsTheRunningStage(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("pi", "pi")

	type reading struct {
		info web.ActivityInfo
		err  error
	}

	var d *Daemon
	// The read happens on the run's goroutine, so its outcome travels back to
	// the test goroutine rather than being asserted here.
	midRun := make(chan reading, 1)
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		// Stand in for the agent appending its session JSONL as it works.
		_ = os.MkdirAll(p.SessionDir, 0o755)
		line := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}` + "\n"
		_ = os.WriteFile(filepath.Join(p.SessionDir, "session.jsonl"), []byte(line), 0o644)

		info, err := d.GetActivity(web.ActivityQuery{ID: p.TicketID, Stage: "step1", Run: 0})
		midRun <- reading{info: info, err: err}

		now := time.Now()
		return process.Result{ExitCode: 0, StartedAt: now, ExitedAt: now}, nil
	}
	d = New(cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-live.md", h.taskMD("tst-live", "todo", "one-stage"))
	h.waitForStatus("tst-live.md", ticket.StatusDone, 10*time.Second)

	got := <-midRun
	require.NoError(t, got.err)
	during := got.info
	assert.True(t, during.Live)
	assert.Equal(t, "events", during.Source)
	assert.Equal(t, "step1", during.Stage)
	assert.Equal(t, 0, during.Run)
	require.NotNil(t, during.Tape)
	require.Len(t, during.Tape.Events, 1)
	assert.Equal(t, "working", during.Tape.Events[0].Text)

	// The ticket file says done before the daemon's own view catches up, and
	// until it does the endpoint still answers for the stage it thinks is
	// running.
	var after web.ActivityInfo
	require.Eventually(t, func() bool {
		var err error
		after, err = d.GetActivity(web.ActivityQuery{ID: "tst-live", Stage: "step1", Run: 0})
		return err == nil && !after.Live
	}, 5*time.Second, 50*time.Millisecond, "the finished run must stop reporting itself live")
	assert.Equal(t, "events", after.Source)
	require.NotNil(t, after.Tape)
	require.Len(t, after.Tape.Events, 1)
	assert.Equal(t, "working", after.Tape.Events[0].Text)

	cancel()
	require.NoError(t, <-errCh)
}

func TestMaterializeSessionEvents(t *testing.T) {
	sessionLine := `{"type":"assistant","message":{"model":"m1","content":[{"type":"text","text":"hi"}]}}` + "\n"

	t.Run("writes a tape atomically", func(t *testing.T) {
		dir := t.TempDir()
		session := filepath.Join(dir, "session.jsonl")
		require.NoError(t, os.WriteFile(session, []byte(sessionLine), 0o644))

		out := filepath.Join(dir, "logs", "step1.0.events.json")
		returned, err := materializeSessionEvents(session, false, out)
		require.NoError(t, err)
		assert.Equal(t, "m1", returned.Model, "the parsed tape is returned for the token metrics")

		data, err := os.ReadFile(out)
		require.NoError(t, err)
		var tape logfmt.Tape
		require.NoError(t, json.Unmarshal(data, &tape))
		assert.Equal(t, "m1", tape.Model)

		entries, err := os.ReadDir(filepath.Dir(out))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "the temp file must not survive the rename")
	})

	t.Run("a failed sidecar leaves the plaintext log alone", func(t *testing.T) {
		dir := t.TempDir()
		session := filepath.Join(dir, "session.jsonl")
		require.NoError(t, os.WriteFile(session, []byte(sessionLine), 0o644))
		logFile := filepath.Join(dir, "logs", "step1.log")

		// A directory where the sidecar's parent should be makes MkdirAll fail.
		blocker := filepath.Join(dir, "blocked")
		require.NoError(t, os.WriteFile(blocker, nil, 0o644))

		require.NoError(t, materializeSessionLog(session, false, logFile))
		_, err := materializeSessionEvents(session, false, filepath.Join(blocker, "step1.0.events.json"))
		require.Error(t, err)
		assert.FileExists(t, logFile)
	})
}
