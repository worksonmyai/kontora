package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

func TestEpicAgentInterruptedAccountingSurvivesRetry(t *testing.T) {
	cases := []struct {
		name                     string
		pauseHook, runnerFailure bool
	}{
		{name: "short runner failure", runnerFailure: true},
		{name: "pause during successful end hook", pauseHook: true},
		{name: "pause during failed end hook", pauseHook: true, runnerFailure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEpicAgentHarness(t)
			h.cfg.Agents["agent1"] = config.Agent{Binary: "pi", FailurePatterns: []string{}}
			entered, released := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(released) }) }
			defer release()
			var hookCount, attempts atomic.Int32
			logger := testLogger(t)
			if tc.pauseHook {
				h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "end", Run: "true"}}}
				logger = slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
					if strings.Contains(string(b), "running hooks") && strings.Contains(string(b), "event=stage_end") && hookCount.Add(1) == 1 {
						close(entered)
						<-released
					}
					return len(b), nil
				}), nil))
			}
			d := h.newDaemon(h.cfg, WithLogger(logger), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				started := time.Now()
				attempt := attempts.Add(1)
				input := 100
				if attempt > 1 {
					input = 9
				}
				stamp := time.Now().UTC().Format(time.RFC3339Nano)
				line := fmt.Sprintf(`{"type":"model_change","timestamp":%q,"provider":"anthropic","modelId":"claude-haiku-4-5"}`+"\n", stamp)
				line += strings.ReplaceAll(piAssistantLine(input, 20, 0, 0), "2026-08-11T02:07:44.744Z", stamp)
				require.NoError(t, os.MkdirAll(p.SessionDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(p.SessionDir, fmt.Sprintf("session-%d.jsonl", attempt)), []byte(line), 0o644))
				result := process.Result{StartedAt: started, ExitedAt: time.Now(), Termination: process.TerminationProcessGroupGone}
				if tc.runnerFailure && attempt == 1 {
					result.ExitCode = -1
					return result, assert.AnError
				}
				completeEpicInvocation(t, p)
				return result, nil
			}))
			h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", "todo", "epc-c1"))
			h.writeTicket("epc-c1.md", h.epicChildMD("epc-c1", "done", "epc-a", "2026-01-02T00:00:00Z"))
			stop := startDaemon(t, d)
			defer func() {
				release()
				stop()
			}()
			if tc.pauseHook {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("stage_end hook not reached")
				}
				require.NoError(t, d.PauseTicket("epc-a"))
				release()
			}
			h.waitForStatus("epc-a.md", ticket.StatusPaused, 5*time.Second)
			waitForAgentsDone(t, d, 5*time.Second)
			first := h.readTask("epc-a.md")
			require.Len(t, first.History, 1)
			require.NotEmpty(t, first.History[0].SessionRef)
			cost, err := d.GetTicketCost("epc-a")
			require.NoError(t, err)
			assert.Equal(t, 1, cost.TrackedRuns)
			assert.Equal(t, 1, cost.PricedRuns)
			firstPath := stageEventsPath(h.cfg, "epc-a", simpleStageName, 0)
			before, err := os.ReadFile(firstPath)
			require.NoError(t, err)
			require.Contains(t, string(before), `"input":100`)
			require.NoError(t, d.RetryTicket("epc-a"))
			h.waitForStatus("epc-a.md", ticket.StatusHumanReview, 5*time.Second)
			waitForAgentsDone(t, d, 5*time.Second)
			got := h.readTask("epc-a.md")
			require.Len(t, got.History, 2)
			assert.Equal(t, 0, got.History[0].Run)
			assert.Equal(t, 1, got.History[1].Run)
			after, err := os.ReadFile(firstPath)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			second, err := os.ReadFile(stageEventsPath(h.cfg, "epc-a", simpleStageName, 1))
			require.NoError(t, err)
			assert.Contains(t, string(second), `"input":9`)
			cost, err = d.GetTicketCost("epc-a")
			require.NoError(t, err)
			assert.Equal(t, 2, cost.TrackedRuns)
			assert.Equal(t, 2, cost.PricedRuns)
		})
	}
}

func TestEpicAgentPauseDuringChildRefresh(t *testing.T) {
	h := newEpicAgentHarness(t)
	childPath := filepath.Join(h.tasksDir, "epc-c1.md")
	h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "delayed-child", Run: fmt.Sprintf("mkfifo %q", childPath)}}}
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		completeEpicInvocation(t, p)
		now := time.Now()
		return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, nil
	}))
	path := h.writeTicket("epc-a.md", h.agentEpicMD("epc-a", "todo", "epc-c1"))
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets[tk.ID] = newTicketState(tk, path)
	entered, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	defer release()
	writerErr := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if st, err := os.Stat(childPath); err == nil && st.Mode()&os.ModeNamedPipe != 0 {
				break
			}
			if time.Now().After(deadline) {
				writerErr <- fmt.Errorf("child FIFO not created")
				return
			}
			time.Sleep(time.Millisecond)
		}
		f, err := os.OpenFile(childPath, os.O_WRONLY, 0o600)
		if err != nil {
			writerErr <- err
			return
		}
		close(entered)
		<-released
		_, err = f.Write([]byte(h.epicChildMD("epc-c1", "done", "epc-a", "2026-01-02T00:00:00Z")))
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		writerErr <- err
	}()
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	taskCtx, cancelTask, claimed, claimedPath, branchKey, err := d.claimExecution(rootCtx, h.cfg, tk.ID, testLogger(t))
	require.NoError(t, err)
	defer d.finishOrdinaryRun(taskCtx, cancelTask, tk.ID, branchKey, claimedPath, testLogger(t))
	done := make(chan struct{})
	go func() {
		d.runSimpleTicket(rootCtx, taskCtx, h.cfg, testLogger(t), tk.ID, claimed, claimedPath)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child refresh did not open FIFO")
	}
	// The scanner keeps its delayed stream; pause admission must read a regular file.
	require.NoError(t, os.Rename(childPath, childPath+".delayed"))
	require.NoError(t, os.WriteFile(childPath, []byte(h.epicChildMD("epc-c1", "done", "epc-a", "2026-01-02T00:00:00Z")), 0o644))
	require.NoError(t, d.PauseTicket(tk.ID))
	require.Equal(t, ticket.StatusPaused, h.readTask("epc-a.md").Status)
	release()
	require.NoError(t, <-writerErr)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor exit did not finish")
	}
	got := h.readTask("epc-a.md")
	assert.Equal(t, ticket.StatusPaused, got.Status)
	assert.Len(t, got.History, 1)
}
