package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/web"
)

func setupExecutionTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir, err := os.MkdirTemp("", "kyr")
	require.NoError(t, err)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", dir)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run(); _ = os.RemoveAll(dir) })
	out, err := exec.Command("tmux", "-f", "/dev/null", "new-session", "-d", "-s", "unrelated", "-n", "anchor", "--", "sleep", "60").CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func TestExecutionRealTmuxAPIRetryReleasesCapacity(t *testing.T) {
	setupExecutionTmux(t)
	h := newHarness(t)
	h.cfg.MaxConcurrentAgents = 1
	marker := filepath.Join(t.TempDir(), "first-attempt")
	agent := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(agent, []byte(fmt.Sprintf("#!/bin/sh\necho KONTORA_AGENT_OUTPUT\nif [ \"$%s\" = tst-retry ] && [ ! -f %q ]; then touch %q; exit 1; fi\nexit 0\n", config.TicketEnvVar, marker, marker)), 0o755))
	agentCfg := h.cfg.Agents["agent1"]
	agentCfg.Binary = agent
	h.cfg.Agents["agent1"] = agentCfg
	d := h.newDaemon(h.cfg, WithRunner(tmuxRunner), WithExecutionPreparer(tmux.EnsureWindowAbsent))
	h.writeTicket("tst-retry.md", h.taskMD("tst-retry", "todo", "one-stage"))
	h.writeTicket("tst-queued.md", h.taskMD("tst-queued", "open", "one-stage"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
	h.waitForStatus("tst-retry.md", ticket.StatusPaused, 10*time.Second)
	waitForAgentsDone(t, d, 5*time.Second)
	windows, err := tmux.ListWindows(h.tmuxSession)
	require.NoError(t, err)
	assert.Contains(t, windows, "tst-retry")
	srv, err := web.New(d, d.broker, "127.0.0.1", 0, "", nil, h.tmuxSession, testLogger(t))
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	for _, target := range []struct{ id, action string }{{"tst-retry", "retry"}, {"tst-queued", "run"}} {
		resp, err := http.Post("http://"+srv.Addr()+"/api/tickets/"+target.id+"/"+target.action, "application/json", nil)
		require.NoError(t, err)
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	}
	h.waitForStatus("tst-retry.md", ticket.StatusDone, 10*time.Second)
	h.waitForStatus("tst-queued.md", ticket.StatusDone, 10*time.Second)
	waitForAgentsDone(t, d, 5*time.Second)
	got := h.readTask("tst-retry.md")
	require.Len(t, got.History, 2)
	assert.Equal(t, 1, got.History[0].ExitCode)
	assert.Zero(t, got.History[1].ExitCode)
	for _, id := range []string{"tst-retry", "tst-queued"} {
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(stageLogPath(h.cfg, id, "step1"))
			return err == nil && strings.Contains(string(data), "KONTORA_AGENT_OUTPUT")
		}, 5*time.Second, 10*time.Millisecond, "sink must capture output instead of launching tests")
	}
	windows, err = tmux.ListWindows(h.tmuxSession)
	require.NoError(t, err)
	for _, window := range windows {
		assert.False(t, strings.HasPrefix(window, "tst-"), "remaining window %q", window)
	}
	d.mu.Lock()
	assert.Empty(t, d.running)
	assert.Empty(t, d.runningBranches)
	assert.Empty(t, d.executions)
	assert.Zero(t, d.sem.used)
	d.mu.Unlock()
}

func TestExecutionRealTmuxFailurePatternsBeyondLogCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry bool
		fails bool
		empty bool
	}{
		{name: "late error", fails: true},
		{name: "clean capped output"},
		{name: "error on capped retry", retry: true, fails: true},
		{name: "clean capped retry", retry: true},
		{name: "empty pattern on capped retry", retry: true, fails: true, empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupExecutionTmux(t)
			h := newHarness(t)
			marker := filepath.Join(t.TempDir(), "first-attempt")
			agent := filepath.Join(t.TempDir(), "agent")
			flood := "dd if=/dev/zero bs=1048576 count=17 2>/dev/null\n"
			script := "#!/bin/sh\n"
			pipeline := "one-stage"
			if tc.retry {
				pipeline = "retry-stage"
				script += fmt.Sprintf("if [ ! -f %q ]; then touch %q; printf '\\nprovider rejected\\n'; exit 1; fi\n", marker, marker)
			} else {
				script += flood
			}
			if tc.fails {
				script += "printf '\\nprovider rejected\\n'\n"
			} else {
				script += "printf '\\nall good\\n'\n"
			}
			script += "exit 0\n"
			require.NoError(t, os.WriteFile(agent, []byte(script), 0o755))
			agentCfg := h.cfg.Agents["agent1"]
			agentCfg.Binary = agent
			pattern := "provider rejected"
			if tc.empty {
				pattern = ""
			}
			agentCfg.FailurePatterns = []string{pattern}
			h.cfg.Agents["agent1"] = agentCfg
			exits := make(chan int, 2)
			var seed sync.Once
			runner := func(ctx context.Context, p RunnerParams) (process.Result, error) {
				var seedErr error
				if tc.retry {
					seed.Do(func() {
						if seedErr = os.WriteFile(p.LogFile, []byte("provider rejected\n"), 0o600); seedErr == nil {
							seedErr = os.Truncate(p.LogFile, 16<<20)
						}
					})
				}
				if seedErr != nil {
					return process.Result{}, seedErr
				}
				result, err := tmuxRunner(ctx, p)
				exits <- result.ExitCode
				return result, err
			}
			d := h.newDaemon(h.cfg, WithRunner(runner), WithExecutionPreparer(tmux.EnsureWindowAbsent))
			h.writeTicket("tst-stream.md", h.taskMD("tst-stream", "todo", pipeline))
			stop := startDaemon(t, d)
			defer stop()
			want := ticket.StatusDone
			if tc.fails {
				want = ticket.StatusPaused
			}
			got := h.waitForStatus("tst-stream.md", want, 20*time.Second)
			waitForAgentsDone(t, d, 5*time.Second)
			if tc.fails {
				assert.Contains(t, got.LastError, fmt.Sprintf("output matched failure pattern %q", pattern))
			}
			attempts := 1
			if tc.retry {
				attempts++
			}
			for i := range attempts {
				select {
				case code := <-exits:
					if i == attempts-1 {
						assert.Zero(t, code)
					} else {
						assert.Equal(t, 1, code)
					}
				default:
					t.Fatal("runner did not report every attempt")
				}
			}
			if !tc.fails {
				require.Len(t, got.History, attempts)
			}
			info, err := os.Stat(stageLogPath(h.cfg, "tst-stream", "step1"))
			require.NoError(t, err)
			if tc.retry {
				require.NotEmpty(t, got.History)
				previous := got.History[0]
				require.NotEmpty(t, previous.RawLogID)
				retained, err := os.Stat(session.RawLogPath(h.logsDir, "tst-stream", previous.RawLogID, "step1"))
				require.NoError(t, err)
				assert.EqualValues(t, 16<<20, retained.Size())
				assert.Less(t, info.Size(), int64(16<<20), "the retry has its own capture")
			} else {
				assert.EqualValues(t, 16<<20, info.Size())
			}
		})
	}
}
