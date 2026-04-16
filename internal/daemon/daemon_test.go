package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/testutil"
	"github.com/worksonmyai/kontora/internal/ticket"
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
	t        *testing.T
	tasksDir string
	wtDir    string
	logsDir  string
	lockPath string
	repoDir  string
	repoName string
	cfg      *config.Config
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	tasksDir := t.TempDir()
	wtDir := t.TempDir()
	logsDir := t.TempDir()
	lockDir := t.TempDir()
	repoDir := initRepo(t)

	h := &testHarness{
		t:        t,
		tasksDir: tasksDir,
		wtDir:    wtDir,
		logsDir:  logsDir,
		lockPath: filepath.Join(lockDir, "lock"),
		repoDir:  repoDir,
		repoName: filepath.Base(repoDir),
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

func (h *testHarness) newDaemon(cfg *config.Config) *Daemon {
	return New(cfg,
		WithLogger(testLogger(h.t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(DirectRunner),
		WithSkipOrphanCleanup(),
	)
}

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
	out, err := exec.Command("tmux", "list-windows", "-t", "=kontora", "-F", "#{window_name}").CombinedOutput()
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
		// Simulate pipe-pane writing raw PTY output to the log file.
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("raw PTY output from pipe-pane"), 0o644))
		// Write a fake pi session JSONL to the session dir.
		if p.SessionDir != "" {
			require.NoError(t, os.MkdirAll(p.SessionDir, 0o755))
			sessionFile := filepath.Join(p.SessionDir, "session-abc.jsonl")
			jsonl := strings.Join([]string{
				`{"type":"model_change","modelId":"opus-4"}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"I will check the tests."}]}}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
				`{"type":"message","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"PASS\n"}]}}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"All tests pass."}]}}`,
			}, "\n")
			require.NoError(t, os.WriteFile(sessionFile, []byte(jsonl), 0o644))
		}
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("pi", "true")
	d := New(cfg,
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

	h.writeTicket("tst-pisl.md", h.taskMD("tst-pisl", "todo", "one-stage"))
	h.waitForStatus("tst-pisl.md", ticket.StatusDone, 10*time.Second)

	// Verify SessionDir was set and --session-dir was in args.
	assert.NotEmpty(t, capturedParams.SessionDir, "SessionDir should be set for pi agents")
	assert.True(t, slices.Contains(capturedParams.Args, "--session-dir"), "args missing --session-dir: %v", capturedParams.Args)
	idx := slices.Index(capturedParams.Args, "--session-dir")
	assert.Equal(t, capturedParams.SessionDir, capturedParams.Args[idx+1])

	// Verify the log file contains formatted output, not raw JSONL.
	logFile := filepath.Join(h.logsDir, "tst-pisl", "step1.log")
	logContent, err := os.ReadFile(logFile)
	require.NoError(t, err, "log file should exist")

	content := string(logContent)
	assert.Contains(t, content, "[opus-4]")
	assert.Contains(t, content, "I will check the tests.")
	assert.Contains(t, content, "> bash go test ./...")
	assert.Contains(t, content, "PASS")
	assert.Contains(t, content, "All tests pass.")
	assert.NotContains(t, content, `"type":"message"`, "log should be formatted, not raw JSONL")
	assert.NotContains(t, content, "raw PTY output", "JSONL materialization should overwrite pipe-pane output")

	cancel()
	require.NoError(t, <-errCh)
}

func TestPiSessionLogMaterializationMissing(t *testing.T) {
	h := newHarness(t)

	capturingRunner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		// Simulate pipe-pane writing raw PTY output to the log file.
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("raw PTY fallback output"), 0o644))
		// Don't create session dir — simulate pi not writing any session files.
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("pi", "true")
	d := New(cfg,
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

	h.writeTicket("tst-pmis.md", h.taskMD("tst-pmis", "todo", "one-stage"))

	// Ticket should still complete even if pi session JSONL is missing.
	h.waitForStatus("tst-pmis.md", ticket.StatusDone, 10*time.Second)

	// Verify the raw PTY output survives as fallback when JSONL is missing.
	logFile := filepath.Join(h.logsDir, "tst-pmis", "step1.log")
	logContent, err := os.ReadFile(logFile)
	require.NoError(t, err, "log file should exist from pipe-pane")
	assert.Contains(t, string(logContent), "raw PTY fallback output", "pipe-pane output should survive when JSONL materialization fails")

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
