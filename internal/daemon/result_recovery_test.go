package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	"github.com/worksonmyai/kontora/internal/worktree"
)

func TestResultRecoveryReservationRelease(t *testing.T) {
	for _, supervisor := range []bool{false, true} {
		for _, trigger := range []string{"reconcile", "watcher", "pause and retry", "retry while unconfirmed"} {
			t.Run(fmt.Sprintf("supervisor=%t/%s", supervisor, trigger), func(t *testing.T) {
				f := newResultFixture(t)
				f.h.cfg.MaxConcurrentEpicSupervisors = 1
				f.d.epicSem = make(chan struct{}, 1)
				if supervisor {
					f.rec.EpicID = f.tkt.ID
					require.NoError(t, f.tkt.SetField("kind", "epic"))
					require.NoError(t, f.tkt.SetField("epic_execution", "agent"))
					f.write(t)
					require.NoError(t, saveExecution(f.h.cfg, f.rec))
				}
				f.d.windows.list = func(string) ([]string, error) { return nil, errors.New("temporary inspection failure") }
				require.Equal(t, ticket.StatusInProgress, f.reconcile(t).Status)
				require.Contains(t, f.d.running, f.tkt.ID)
				require.Len(t, f.d.ticketPool(f.tkt), 1)
				if trigger != "retry while unconfirmed" {
					f.d.windows.list = func(string) ([]string, error) { return nil, nil }
				}
				switch trigger {
				case "reconcile":
					f.reconcile(t)
				case "watcher":
					f.d.handleFileChanged(f.path)
				case "pause and retry", "retry while unconfirmed":
					require.NoError(t, f.d.PauseTicket(f.tkt.ID))
					f.d.background.Wait()
					require.NoError(t, f.d.RetryTicket(f.tkt.ID))
					if trigger == "retry while unconfirmed" {
						f.d.handleFileChanged(f.path)
						require.Contains(t, f.d.running, f.tkt.ID)
						require.Len(t, f.d.ticketPool(f.tkt), 1)
						f.d.windows.list = func(string) ([]string, error) { return nil, nil }
						f.d.handleFileChanged(f.path)
					}
				}
				f.d.background.Wait()
				assert.Equal(t, ticket.StatusTodo, f.h.readTask(resultTicketID+".md").Status)
				assert.Empty(t, f.d.running)
				assert.Empty(t, f.d.sem)
				assert.Empty(t, f.d.epicSem)
				assert.Empty(t, f.d.runningBranches)
				assert.Empty(t, f.d.recoveredReservations)
			})
		}
	}
}

func TestResultRecoveryOriginalResourcesAndStartupOverrides(t *testing.T) {
	for _, status := range []ticket.Status{ticket.StatusInProgress, ticket.StatusPaused, ticket.StatusTodo} {
		for _, phase := range []string{"running", "transition"} {
			t.Run(string(status)+"/"+phase, func(t *testing.T) {
				f := newResultFixture(t)
				common, err := worktree.CommonDir(f.h.repoDir)
				require.NoError(t, err)
				f.rec.RepoCommonDir = common
				f.rec.Phase = phase
				f.rec.Transition = &executionTransition{Status: ticket.StatusDone}
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
				require.NoError(t, f.tkt.SetField("status", string(status)))
				require.NoError(t, f.tkt.SetField("branch", "operator-new-branch"))
				require.NoError(t, f.tkt.SetField("path", t.TempDir()))
				if status == ticket.StatusTodo {
					require.NoError(t, f.tkt.SetField("attempt", 0))
				}
				f.write(t)
				before, err := os.ReadFile(f.path)
				require.NoError(t, err)
				f.d.windows = windowOps{
					list: func(string) ([]string, error) { return []string{f.tkt.ID}, nil },
					kill: func(string, string) error { return errors.New("termination unconfirmed") },
				}
				require.NoError(t, f.d.initialScan(f.h.tasksDir))
				f.d.cleanOrphanedWindows()
				after, err := os.ReadFile(f.path)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "unknown termination must preserve operator state")
				assert.Equal(t, f.tkt.ID, f.d.runningBranches[common+"\x00"+f.rec.Branch])
				assert.Contains(t, f.d.running, f.tkt.ID)
				assert.Len(t, f.d.sem, 1)
				other := f.h.readTask(resultTicketID + ".md").Clone()
				require.NoError(t, other.SetField("id", "tst-contender"))
				require.NoError(t, other.SetField("status", "todo"))
				require.NoError(t, other.SetField("path", f.rec.RepoPath))
				require.NoError(t, other.SetField("branch", f.rec.Branch))
				data, err := other.Marshal()
				require.NoError(t, err)
				otherPath := f.h.writeTicket(other.ID+".md", string(data))
				f.d.tickets[other.ID] = newTicketState(other, otherPath)
				f.d.runTicket(context.Background(), other.ID)
				assert.Contains(t, f.h.readTask(other.ID+".md").LastError, "in use by ticket")
				f.d.windows.list = func(string) ([]string, error) { return nil, nil }
				f.d.handleFileChanged(f.path)
				got := f.h.readTask(resultTicketID + ".md")
				assert.Equal(t, status, got.Status)
				assert.Equal(t, "operator-new-branch", got.Branch)
				assert.Empty(t, got.InvocationID)
				assert.Empty(t, got.History, "an overridden transition must not replay")
				assert.Empty(t, f.d.running)
				assert.Empty(t, f.d.sem)
				assert.Empty(t, f.d.runningBranches)
				assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
			})
		}
	}
}

func TestResultRecoveryOverfullPool(t *testing.T) {
	for _, supervisor := range []bool{false, true} {
		t.Run(fmt.Sprintf("supervisor=%t", supervisor), func(t *testing.T) {
			f := newResultFixture(t)
			f.h.cfg.MaxConcurrentAgents, f.h.cfg.MaxConcurrentEpicSupervisors = 1, 1
			f.d = f.h.newDaemon(f.h.cfg)
			f.d.windows.list = func(string) ([]string, error) { return nil, errors.New("inspection unavailable") }
			pool := f.d.sem
			if supervisor {
				pool = f.d.epicSem
				f.rec.EpicID = f.tkt.ID
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
			}
			pool <- struct{}{} // A live dispatcher owns this slot before recovery discovers two more executions.
			f.reconcile(t)
			other := f.tkt.Clone()
			require.NoError(t, other.SetField("id", "tst-second-recovery"))
			require.NoError(t, other.SetField("branch", "kontora/second"))
			second := *f.rec
			second.Invocation.TicketID = other.ID
			second.Invocation.ResultPath = filepath.Join(f.h.logsDir, other.ID, "results", second.Invocation.ID+".json")
			second.Branch = other.Branch
			if supervisor {
				second.EpicID = other.ID
			}
			require.NoError(t, saveExecution(f.h.cfg, &second))
			data, err := other.Marshal()
			require.NoError(t, err)
			otherPath := f.h.writeTicket(other.ID+".md", string(data))
			f.d.tickets[other.ID] = newTicketState(other, otherPath)
			f.d.mu.Lock()
			f.d.reconcileExecutionLocked(other, otherPath)
			<-pool // The ordinary worker exits; both recovered executions still consume capacity.
			assert.Equal(t, -1, f.d.availablePoolSlotsLocked(pool))
			f.d.mu.Unlock()
			candidate := f.h.readTask(resultTicketID + ".md").Clone()
			require.NoError(t, candidate.SetField("id", "tst-next"))
			require.NoError(t, candidate.SetField("status", "todo"))
			require.NoError(t, candidate.SetField("branch", "kontora/next"))
			if supervisor {
				require.NoError(t, candidate.SetField("kind", "epic"))
				require.NoError(t, candidate.SetField("epic_execution", "agent"))
			}
			f.d.tickets[candidate.ID] = newTicketState(candidate, "unused")
			f.d.mu.Lock()
			f.d.enqueue(candidate)
			assert.Equal(t, -1, f.d.readyOrdinaryJobLocked(), "released channel space is not available recovery capacity")
			if !supervisor {
				f.d.managedDispatch = &managedDispatch{enabled: true, queue: []*dispatchJob{{slots: 1, ready: func() bool { return true }}}}
				assert.Equal(t, -1, f.d.readyManagedJobLocked())
			}
			f.d.mu.Unlock()
		})
	}
}

func TestResultBindingRelativeLogsDir(t *testing.T) {
	f := newResultFixture(t)
	root := t.TempDir()
	t.Chdir(root)
	f.h.cfg.LogsDir = "relative/logs"
	params := RunnerParams{Env: map[string]string{}}
	rec, err := f.d.bindExecution(f.tkt, spawnAgentParams{cfg: f.h.cfg, ticketID: f.tkt.ID, filePath: f.path,
		stageName: f.tkt.Stage, wtPath: f.h.wtDir, agentName: "agent2"}, &params, f.rec.EpicID)
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(rec.Invocation.ResultPath))
	_, err = session.InvocationFromEnv(func(key string) string { return params.Env[key] })
	require.NoError(t, err)
	t.Chdir(f.h.repoDir)
	publishDaemonResult(t, rec.Invocation.ResultPath, "blocked")
	t.Chdir(root)
	loaded, err := loadExecution(f.h.cfg, f.tkt.ID)
	require.NoError(t, err)
	assert.Equal(t, rec.Invocation, loaded.Invocation)
}

func TestResultSameDaemonTransitionReplay(t *testing.T) {
	for _, pipelineName := range []string{"one-stage", ""} {
		t.Run("pipeline="+pipelineName, func(t *testing.T) {
			h := newHarness(t)
			h.writeTicket("epc-result.md", "---\nid: epc-result\nkind: epic\nepic_execution: agent\nstatus: paused\n---\n# Supervisor\n")
			md := strings.Replace(h.taskMD("tst-replay", "todo", pipelineName), "kontora: true\n", "kontora: true\nparent: epc-result\n", 1)
			path := h.writeTicket("tst-replay.md", md)
			h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "fail-ticket-write", Run: `chmod 444 "$KONTORA_TICKET_FILE"`}}}
			d, collect := h.newMetricsDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				publishDaemonResult(t, p.Env["KONTORA_RESULT_FILE"], "complete")
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationWindowGone}, nil
			}))
			d.windows.list = func(string) ([]string, error) { return nil, nil }
			d.tickets["tst-replay"] = newTicketState(h.readTask("tst-replay.md"), path)
			d.runTicket(context.Background(), "tst-replay")
			require.Equal(t, ticket.StatusInProgress, h.readTask("tst-replay.md").Status)
			rec, err := loadExecution(h.cfg, "tst-replay")
			require.NoError(t, err)
			require.Equal(t, "transition", rec.Phase)
			require.DirExists(t, rec.Worktree)
			d.handleFileChanged(path)
			require.Equal(t, ticket.StatusInProgress, h.readTask("tst-replay.md").Status)
			require.Contains(t, d.running, "tst-replay", "failed replay keeps a releasable reservation")
			require.NoError(t, os.Chmod(path, 0o644))
			d.handleFileChanged(path)
			d.background.Wait()
			d.handleFileChanged(path)
			d.background.Wait()
			got := h.readTask("tst-replay.md")
			assert.Equal(t, ticket.StatusDone, got.Status)
			assert.Len(t, got.History, 1)
			assert.Empty(t, got.InvocationID)
			assert.NoFileExists(t, executionPath(h.cfg, got.ID))
			assert.NoDirExists(t, rec.Worktree)
			assert.Empty(t, d.running)
			assert.Empty(t, d.sem)
			assert.Empty(t, d.pendingFinalizations)
			metric, ok := collect()["kontora.stage.runs"]
			require.True(t, ok)
			assert.Equal(t, map[string]int64{"success": 1}, sumByAttr(t, metric, "outcome"))
		})
	}
}

func TestResultFailedFinalWriteRetainsDispatcherToken(t *testing.T) {
	f := newResultFixture(t)
	f.h.cfg.MaxConcurrentAgents = 1
	f.h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "fail-ticket-write", Run: `chmod 444 "$KONTORA_TICKET_FILE"`}}}
	require.NoError(t, f.tkt.SetField("status", "todo"))
	f.write(t)
	d := f.h.newDaemon(f.h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		publishDaemonResult(t, p.Env["KONTORA_RESULT_FILE"], "complete")
		now := time.Now()
		return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationWindowGone}, nil
	}))
	d.windows.list = func(string) ([]string, error) { return nil, nil }
	d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	d.enqueue(f.tkt)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.dispatch(ctx, &wg) })
	t.Cleanup(func() { cancel(); wg.Wait(); d.background.Wait() })
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		reservation, exists := d.recoveredReservations[f.tkt.ID]
		return exists && reservation.token && len(d.sem) == 1
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Chmod(f.path, 0o644))
	d.handleFileChanged(f.path)
	d.background.Wait()
	d.mu.Lock()
	assert.Empty(t, d.running)
	assert.Empty(t, d.sem)
	assert.Empty(t, d.runningBranches)
	d.mu.Unlock()
	assert.Equal(t, ticket.StatusDone, f.h.readTask(resultTicketID+".md").Status)
}

func TestResultReplayRetainsResourcesThroughCleanup(t *testing.T) {
	f := newResultFixture(t)
	f.rec.Phase = "transition"
	f.rec.Transition = &executionTransition{Status: ticket.StatusDone,
		History: ticket.HistoryEntry{InvocationID: f.rec.Invocation.ID}}
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	started, release := make(chan struct{}), make(chan struct{})
	f.d.rememberFinalization(f.rec, func(*ticket.Ticket) { close(started); <-release })
	f.reconcile(t)
	<-started
	f.d.handleFileChanged(f.path)
	f.d.mu.Lock()
	assert.Contains(t, f.d.running, f.tkt.ID, "cleanup still owns the original resources")
	assert.Len(t, f.d.sem, 1)
	assert.Len(t, f.d.runningBranches, 1)
	assert.FileExists(t, executionPath(f.h.cfg, f.tkt.ID))
	f.d.mu.Unlock()
	close(release)
	f.d.background.Wait()
	assert.NoFileExists(t, executionPath(f.h.cfg, f.tkt.ID))
	assert.Empty(t, f.d.running)
	assert.Empty(t, f.d.sem)
	assert.Empty(t, f.d.runningBranches)
}

func TestResultCancellationRetiresIdentityBeforeRetry(t *testing.T) {
	f := newResultFixture(t)
	require.NoError(t, f.tkt.SetField("status", "paused"))
	f.write(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := agentRun{Execution: f.rec, Result: process.Result{ExitCode: 7, Termination: process.TerminationWindowGone}}
	f.d.recordStoppedInvocation(context.Background(), ctx, f.path, "agent2", "step2", 3, run)
	got := f.h.readTask(resultTicketID + ".md")
	require.Len(t, got.History, 1)
	assert.Empty(t, got.InvocationID)
	assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
	require.NoError(t, f.d.RetryTicket(got.ID))
	got = f.h.readTask(resultTicketID + ".md")
	require.Zero(t, got.Attempt)
	// A crash after pickup but before bindExecution must not revive the cancelled identity.
	require.NoError(t, got.SetField("status", "in_progress"))
	f.tkt = got
	f.write(t)
	for range 2 {
		d := f.h.newDaemon(f.h.cfg)
		d.windows.list = func(string) ([]string, error) { return nil, nil }
		require.NoError(t, d.initialScan(f.h.tasksDir))
		assert.Empty(t, d.running)
		assert.Empty(t, d.sem)
		assert.Equal(t, ticket.StatusTodo, f.h.readTask(resultTicketID+".md").Status)
	}
}

// Keep the live-dispatch reproduction independent of the synchronous capacity assertions.
func TestResultRecoveryOverfullPoolBlocksLiveDispatch(t *testing.T) {
	f := newResultFixture(t)
	f.h.cfg.MaxConcurrentAgents = 1
	started := make(chan string, 2)
	release := make(chan struct{})
	d := f.h.newDaemon(f.h.cfg, WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
		started <- p.TicketID
		select {
		case <-release:
		case <-ctx.Done():
		}
		now := time.Now()
		return process.Result{ExitCode: 1, StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, nil
	}))
	d.windows.list = func(string) ([]string, error) { return nil, errors.New("inspection unavailable") }
	livePath := f.h.writeTicket("tst-live.md", f.h.taskMD("tst-live", "todo", "one-stage"))
	live := f.h.readTask("tst-live.md")
	d.tickets[live.ID] = newTicketState(live, livePath)
	d.enqueue(live)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.dispatch(ctx, &wg) })
	t.Cleanup(func() { cancel(); wg.Wait(); d.background.Wait() })
	select {
	case id := <-started:
		require.Equal(t, live.ID, id)
	case <-time.After(5 * time.Second):
		t.Fatal("live runner did not start")
	}
	d.handleFileChanged(f.path)
	nextPath := f.h.writeTicket("tst-next.md", f.h.taskMD("tst-next", "todo", "one-stage"))
	d.handleFileChanged(nextPath)
	close(release)
	select {
	case id := <-started:
		t.Fatalf("started %s while recovered execution remains unconfirmed", id)
	case <-time.After(300 * time.Millisecond):
	}
}
