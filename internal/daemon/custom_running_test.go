package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/watcher"
	"github.com/worksonmyai/kontora/internal/web"
)

const awaitingData ticket.Status = "awaiting_data"

func customRunningPipeline(h *testHarness, name string) {
	h.cfg.Statuses = []string{string(awaitingData)}
	steps := h.cfg.Pipelines[name]
	for i := range steps {
		steps[i].RunningStatus = string(awaitingData)
	}
}

func saveCustomTestTicket(t *testing.T, tk *ticket.Ticket, path string) {
	t.Helper()
	data, err := tk.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func receiveCustomRun(t *testing.T, launches <-chan RunnerParams) RunnerParams {
	t.Helper()
	select {
	case p := <-launches:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not launch")
		return RunnerParams{}
	}
}

func TestCustomRunningCollectWaitAnalyze(t *testing.T) {
	h := newHarness(t)
	h.cfg = notifyConfig(h)
	h.cfg.MaxConcurrentAgents = 1
	h.cfg.Statuses = []string{string(awaitingData)}
	h.cfg.Stages = map[string]config.Stage{
		"collect": {Prompt: "collect the data"},
		"wait":    {Prompt: "wait until the data is there"},
		"analyze": {Prompt: "analyze the collected data"},
	}
	h.cfg.Pipelines["research"] = config.Pipeline{
		{Stage: "collect", Agent: "agent1", OnSuccess: "next", OnFailure: "pause"},
		{Stage: "wait", Agent: "agent1", RunningStatus: string(awaitingData), OnSuccess: "next", OnFailure: "pause"},
		{Stage: "analyze", Agent: "agent1", OnSuccess: "done", OnFailure: "pause"},
	}
	launches := make(chan RunnerParams, 8)
	ready := make(chan struct{})
	recorder := &notifyRecorder{}
	d := h.newDaemon(h.cfg, WithNotifier(recorder), WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
		start := time.Now()
		launches <- p
		if filepath.Base(p.LogFile) == "wait.log" {
			select {
			case <-ready:
			case <-ctx.Done():
				return process.Result{StartedAt: start, ExitedAt: time.Now()}, ctx.Err()
			}
		}
		return process.Result{StartedAt: start, ExitedAt: time.Now()}, nil
	}))
	stop := runDaemon(t, d)
	h.writeTicket("tst-research.md", notifyTaskMD(h, "tst-research", "todo", "research", []string{string(awaitingData)}, nil))
	collectPrompt, _, _ := strings.Cut(receiveCustomRun(t, launches).Args[0], "\n\n---\n")
	assert.Equal(t, "collect the data", collectPrompt)
	waiting := receiveCustomRun(t, launches)
	waitPrompt, _, _ := strings.Cut(waiting.Args[0], "\n\n---\n")
	assert.Equal(t, "wait until the data is there", waitPrompt)
	assert.Zero(t, waiting.Timeout)
	active := h.readTask("tst-research.md")
	assert.Equal(t, awaitingData, active.Status)
	assert.Equal(t, awaitingData, active.RunningStatus)
	info, err := d.GetTicket(active.ID)
	require.NoError(t, err)
	assert.True(t, info.ExecutionActive)
	assert.Equal(t, string(awaitingData), info.Status)
	activity, err := d.GetActivity(web.ActivityQuery{ID: active.ID, Stage: "wait", Run: 0})
	require.NoError(t, err)
	assert.True(t, activity.Live)

	h.writeTicket("tst-parked.md", h.taskMD("tst-parked", string(awaitingData), "research"))
	require.Eventually(t, func() bool {
		parked, err := d.GetTicket("tst-parked")
		return err == nil && parked.Status == string(awaitingData) && !parked.ExecutionActive
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, d.RunningAgents(), "same-column parked work occupies no process slot")

	h.writeTicket("tst-capacity.md", h.taskMD("tst-capacity", "open", ""))
	h.writeTicket("tst-dependent.md", strings.Replace(h.taskMD("tst-dependent", "todo", ""), "kontora: true\n", "kontora: true\ndeps: [tst-research]\n", 1))
	require.Eventually(t, func() bool {
		_, err := d.GetTicket("tst-capacity")
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, d.RunTicket("tst-capacity"))
	require.Eventually(t, func() bool {
		info, err := d.GetTicket("tst-dependent")
		return err == nil && len(info.Blockers) == 1
	}, 5*time.Second, 10*time.Millisecond)
	select {
	case p := <-launches:
		t.Fatalf("waiting agent released capacity too soon: %s", p.TicketID)
	case <-time.After(150 * time.Millisecond):
	}
	d.mu.Lock()
	assert.Equal(t, 1, d.sem.used)
	assert.Len(t, d.runningBranches, 1)
	d.mu.Unlock()
	close(ready)
	var analyzed bool
	for range 3 { // analysis, independent contender and dependency released by done
		p := receiveCustomRun(t, launches)
		if filepath.Base(p.LogFile) == "analyze.log" {
			analyzed = true
			analyzePrompt, _, _ := strings.Cut(p.Args[0], "\n\n---\n")
			assert.Equal(t, "analyze the collected data", analyzePrompt)
			got := h.readTask("tst-research.md")
			// Runner completion can race this read; only the cleared snapshot is stable.
			assert.NotEqual(t, awaitingData, got.Status)
			assert.Empty(t, got.RunningStatus)
		}
	}
	assert.True(t, analyzed)
	got := h.waitForStatus("tst-research.md", ticket.StatusDone, 5*time.Second)
	require.Len(t, got.History, 3)
	assert.Equal(t, []string{"collect", "wait", "analyze"}, []string{got.History[0].Stage, got.History[1].Stage, got.History[2].Stage})
	stop()
	events := recorder.sends(t)
	require.Len(t, events, 1)
	assert.Equal(t, string(awaitingData), events[0].To)
	assert.Equal(t, notify.OriginDaemon, recorder.lastFor(t, active.ID).Origin)
}

func TestCustomRunningWatcherIdentity(t *testing.T) {
	for _, change := range []string{"note", "marker", "undeclared marker", "foreign", "status", "status only", "pair", "snapshot", "disabled"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t)
			customRunningPipeline(h, "one-stage")
			path := h.writeTicket("tst-watch.md", h.taskMD("tst-watch", "todo", "one-stage"))
			d := h.newDaemon(h.cfg)
			d.tickets["tst-watch"] = newTicketState(h.readTask("tst-watch.md"), path)
			ctx, cancel, snapshot, filePath, branch, err := d.claimExecution(context.Background(), h.cfg, "tst-watch", testLogger(t))
			require.NoError(t, err)
			defer d.finishOrdinaryRun(ctx, cancel, snapshot.ID, branch, filePath, testLogger(t))
			current := h.readTask("tst-watch.md")
			switch change {
			case "note":
				current.SetBody(current.Body + "\nAn unrelated observation.\n")
			case "marker", "undeclared marker":
				require.NoError(t, current.SetField("running_status", ""))
				if change == "undeclared marker" {
					next := *h.cfg
					next.Statuses = nil
					d.cfg.Store(&next)
				}
			case "foreign":
				require.NoError(t, current.SetField("claimed_by", "foreign-instance"))
			case "status":
				require.NoError(t, current.SetField("status", string(awaitingData)))
			case "status only":
				require.NoError(t, current.SetField("status", "other_status"))
				require.NoError(t, current.SetField("running_status", string(awaitingData)))
			case "pair":
				require.NoError(t, current.SetRunningStatus("other_status"))
			case "snapshot":
				require.NoError(t, current.SetField("running_status", "other_status"))
			case "disabled":
				require.NoError(t, current.SetField("kontora", false))
			}
			saveCustomTestTicket(t, current, path)
			d.handleFileChanged(path)
			assert.Equal(t, change != "note", ctx.Err() != nil)
			assert.Contains(t, d.running, snapshot.ID, "a cancelled context is not confirmed teardown")
			if change == "note" {
				assert.True(t, d.executionCurrent(ctx, snapshot, path))
				assert.Contains(t, h.readTask("tst-watch.md").Body, "An unrelated observation.")
				assert.Contains(t, d.tickets[snapshot.ID].ticket.Body, "An unrelated observation.")
			}
			if change == "foreign" {
				assert.Equal(t, "foreign-instance", h.readTask("tst-watch.md").ClaimedBy)
			}
		})
	}
}

func TestCustomRunningExitPolicies(t *testing.T) {
	for _, outcome := range []string{"next", "same-name park", "failure", "timeout", "provider", "hook", "marker removed", "status changed", "pair changed", "uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			h := newHarness(t)
			customRunningPipeline(h, "two-stage")
			if outcome == "same-name park" {
				h.cfg.Pipelines["two-stage"][0].OnSuccess = string(awaitingData)
			}
			if outcome == "hook" {
				h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Run: "exit 1", OnFailure: config.HookOnFailurePause}}}
			}
			agent := h.cfg.Agents["agent1"]
			agent.FailurePatterns = []string{"provider rejected"}
			h.cfg.Agents["agent1"] = agent
			path := h.writeTicket("tst-exit.md", h.taskMD("tst-exit", "todo", "two-stage"))
			d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				now := time.Now()
				result := process.Result{StartedAt: now, ExitedAt: now}
				current := h.readTask("tst-exit.md")
				assert.True(t, current.IsRunning())
				assert.Equal(t, awaitingData, current.Status)
				switch outcome {
				case "failure":
					result.ExitCode = 1
				case "timeout":
					return result, context.DeadlineExceeded
				case "uncertain":
					return result, process.ErrTerminationUncertain
				case "provider":
					require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
					require.NoError(t, os.WriteFile(p.LogFile, []byte("provider rejected\n"), 0o644))
				case "marker removed":
					require.NoError(t, current.SetField("running_status", ""))
					saveCustomTestTicket(t, current, path)
				case "pair changed":
					require.NoError(t, current.SetRunningStatus("other_status"))
					saveCustomTestTicket(t, current, path)
				case "status changed":
					require.NoError(t, current.SetField("status", "paused"))
					saveCustomTestTicket(t, current, path)
				}
				return result, nil
			}))
			d.tickets["tst-exit"] = newTicketState(h.readTask("tst-exit.md"), path)
			d.runTicket(context.Background(), "tst-exit")
			got := h.readTask("tst-exit.md")
			switch outcome {
			case "next":
				assert.Equal(t, ticket.StatusTodo, got.Status)
				assert.Equal(t, "step2", got.Stage)
				require.Len(t, got.History, 1)
			case "same-name park", "marker removed":
				assert.Equal(t, awaitingData, got.Status)
				assert.False(t, got.IsRunning())
			case "pair changed":
				assert.Equal(t, ticket.Status("other_status"), got.Status)
				assert.Equal(t, ticket.Status("other_status"), got.RunningStatus)
			case "uncertain":
				assert.Contains(t, d.running, got.ID)
				assert.Len(t, d.runningBranches, 1)
			default:
				assert.Equal(t, ticket.StatusPaused, got.Status)
			}
			if outcome != "uncertain" {
				if outcome != "pair changed" {
					assert.Empty(t, got.RunningStatus)
				}
				assert.Empty(t, d.running)
				assert.Empty(t, d.runningBranches)
			}
			if outcome == "marker removed" || outcome == "status changed" || outcome == "pair changed" {
				assert.Empty(t, got.History, "stale exits cannot finalize")
			}
		})
	}
}

func makeCustomResultFixture(t *testing.T) *resultFixture {
	t.Helper()
	f := newResultFixture(t)
	require.NoError(t, f.tkt.SetRunningStatus(awaitingData))
	f.rec.Status, f.rec.RunningStatus = f.tkt.Status, f.tkt.RunningStatus
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	f.write(t)
	// Deliberately do not declare the status in current config.
	return f
}

func TestCustomRunningRecoveryPhases(t *testing.T) {
	for _, phase := range []string{"running", "complete", "blocked", "malformed", "finalizing", "transition", "preparing bound", "preparing unbound", "legacy supervised", "recovered"} {
		t.Run(phase, func(t *testing.T) {
			f := makeCustomResultFixture(t)
			want := ticket.StatusTodo
			switch phase {
			case "complete", "blocked":
				publishDaemonResult(t, f.rec.Invocation.ResultPath, phase)
				if phase == "blocked" {
					want = ticket.StatusPaused
				}
			case "malformed":
				require.NoError(t, os.MkdirAll(filepath.Dir(f.rec.Invocation.ResultPath), 0o755))
				require.NoError(t, os.WriteFile(f.rec.Invocation.ResultPath, []byte("bad result"), 0o600))
				want = ticket.StatusPaused
			case "finalizing":
				f.rec.Phase, want = phase, ticket.StatusPaused
			case "transition":
				f.rec.Phase, want = phase, awaitingData
				f.rec.Transition = &executionTransition{Status: awaitingData, Stage: f.tkt.Stage, Attempt: f.tkt.Attempt,
					History: ticket.HistoryEntry{InvocationID: f.rec.Invocation.ID, RunOutcome: ticket.RunOutcomeComplete}}
			case "preparing bound", "preparing unbound":
				prepareLaunchRecovery(t, f, "inv-result", phase == "preparing bound")
			case "recovered":
				f.rec.Phase = phase
				require.NoError(t, f.tkt.SetField("status", "todo"))
				f.write(t)
			case "legacy supervised":
				require.NoError(t, f.tkt.SetField("invocation_id", ""))
				f.write(t)
			}
			if phase == "legacy supervised" {
				require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
			} else {
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
			}
			got := f.reconcile(t)
			assert.Equal(t, want, got.Status)
			assert.Empty(t, got.RunningStatus)
			assert.False(t, got.IsRunning())
			assert.Empty(t, f.d.running)
			assert.Zero(t, f.d.sem.used)
			if phase == "transition" {
				require.Len(t, got.History, 1)
				assert.True(t, committedExecutionMatches(got, f.rec))
				require.NoError(t, got.SetRunningStatus(awaitingData))
				assert.False(t, committedExecutionMatches(got, f.rec), "same-status active pair is not a committed parked transition")
			}
		})
	}
}

func TestCustomRunningRecoveryOverrides(t *testing.T) {
	for _, phase := range []string{"ordinary", "running", "preparing bound", "preparing unbound", "transition"} {
		for _, change := range []string{"marker", "status", "foreign"} {
			for _, timing := range []string{"before confirmation", "during confirmation"} {
				t.Run(phase+"/"+change+"/"+timing, func(t *testing.T) {
					f := makeCustomResultFixture(t)
					if strings.HasPrefix(phase, "preparing") {
						prepareLaunchRecovery(t, f, "inv-result", phase == "preparing bound")
					} else if phase == "transition" {
						f.rec.Phase = phase
						f.rec.Transition = &executionTransition{Status: ticket.StatusDone}
					}
					require.NoError(t, saveExecution(f.h.cfg, f.rec))
					if phase == "ordinary" {
						require.NoError(t, f.tkt.SetField("parent", ""))
						require.NoError(t, f.tkt.SetField("invocation_id", ""))
						require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
						f.write(t)
					}
					override := func() {
						current := f.h.readTask(resultTicketID + ".md")
						switch change {
						case "marker":
							require.NoError(t, current.SetField("running_status", ""))
						case "status":
							require.NoError(t, current.SetField("status", "paused"))
						case "foreign":
							require.NoError(t, current.SetField("claimed_by", "foreign"))
						}
						saveCustomTestTicket(t, current, f.path)
					}
					if timing == "during confirmation" {
						require.NoError(t, os.Chmod(f.path, 0o444))
						t.Cleanup(func() { _ = os.Chmod(f.path, 0o644) })
						require.NoError(t, f.d.initialScan(f.h.tasksDir))
						require.Contains(t, f.d.recoveredReservations, f.tkt.ID)
						require.NoError(t, os.Chmod(f.path, 0o644))
						confirmations := 0
						f.d.windows.list = func(string) ([]string, error) {
							confirmations++
							// Model a disk edit that completes while the tmux call blocks.
							override()
							return nil, nil
						}
						f.d.handleFileChanged(f.path)
						assert.Equal(t, 1, confirmations)
					} else {
						override()
						if change == "foreign" {
							f.d.windows.list = func(string) ([]string, error) { t.Error("foreign claim cannot authorize termination"); return nil, nil }
						}
						f.reconcile(t)
					}
					got := f.h.readTask(resultTicketID + ".md")
					wantStatus, wantSnapshot := awaitingData, awaitingData
					if change == "status" {
						wantStatus = ticket.StatusPaused
					}
					if change != "foreign" {
						wantSnapshot = ""
					}
					assert.Equal(t, wantStatus, got.Status)
					assert.Equal(t, wantSnapshot, got.RunningStatus)
					assert.Empty(t, got.History)
					if timing == "during confirmation" {
						cachedData, err := f.d.tickets[got.ID].ticket.Marshal()
						require.NoError(t, err)
						storedData, err := got.Marshal()
						require.NoError(t, err)
						assert.Equal(t, string(storedData), string(cachedData))
					}
					if change == "foreign" {
						assert.Equal(t, "foreign", got.ClaimedBy)
						if phase != "ordinary" {
							assert.FileExists(t, executionPath(f.h.cfg, got.ID))
						}
						if timing == "during confirmation" {
							assert.Contains(t, f.d.recoveredReservations, got.ID)
							assert.Equal(t, 1, f.d.sem.used)
							assert.Len(t, f.d.runningBranches, 1)
						}
					} else {
						assert.Empty(t, got.InvocationID)
						assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
						assert.Empty(t, f.d.recoveredReservations)
						assert.Empty(t, f.d.running)
						assert.Empty(t, f.d.runningBranches)
						assert.Zero(t, f.d.sem.used)
					}
					assert.Empty(t, f.d.queue, "operator overrides must not enqueue replacement work")
				})
			}
		}
	}
}

func TestCustomRunningRecoveryWriteRollback(t *testing.T) {
	for _, phase := range []string{"running", "blocked", "transition", "invalid journal", "ordinary startup", "ordinary watcher"} {
		t.Run(phase, func(t *testing.T) {
			f := makeCustomResultFixture(t)
			*f.h.cfg.AutoPickUp = false
			switch phase {
			case "blocked":
				publishDaemonResult(t, f.rec.Invocation.ResultPath, "blocked")
			case "transition":
				f.rec.Phase = phase
				f.rec.Transition = &executionTransition{Status: ticket.StatusDone, Stage: "changed", Attempt: 0, Summary: "new summary",
					History: ticket.HistoryEntry{InvocationID: f.rec.Invocation.ID}}
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
			case "invalid journal":
				require.NoError(t, os.WriteFile(executionPath(f.h.cfg, f.tkt.ID), []byte("bad json"), 0o600))
			case "ordinary startup", "ordinary watcher":
				require.NoError(t, f.tkt.SetField("parent", ""))
				require.NoError(t, f.tkt.SetField("invocation_id", ""))
				require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
				f.write(t)
			}
			before := f.h.readTask(resultTicketID + ".md")
			require.NoError(t, os.Chmod(f.path, 0o444))
			t.Cleanup(func() { _ = os.Chmod(f.path, 0o644) })
			switch phase {
			case "ordinary startup":
				require.NoError(t, f.d.initialScan(f.h.tasksDir))
				f.tkt = f.d.tickets[f.tkt.ID].ticket
			case "ordinary watcher":
				f.d.handleFileChanged(f.path)
				f.tkt = f.d.tickets[f.tkt.ID].ticket
			default:
				f.reconcile(t)
			}
			assert.Equal(t, before.Clone(), f.tkt.Clone(), "rollback must restore typed fields and preserved YAML together")
			assert.Contains(t, f.d.running, f.tkt.ID)
			assert.Len(t, f.d.runningBranches, 1)
			assert.Equal(t, 1, f.d.sem.used)
			require.NoError(t, os.Chmod(f.path, 0o644))
			current := f.h.readTask(resultTicketID + ".md")
			current.SetBody(current.Body + "\nRecovery can persist again.\n")
			saveCustomTestTicket(t, current, f.path)
			events, unsubscribe := f.d.Subscribe()
			defer unsubscribe()
			f.d.handleEvent(watcher.Event{Op: watcher.OpChanged, Path: f.path})
			f.d.background.Wait()
			assert.Empty(t, f.d.running)
			assert.Zero(t, f.d.sem.used)
			stored := f.h.readTask(resultTicketID + ".md")
			assert.Empty(t, f.d.recoveredReservations)
			assert.Empty(t, f.d.runningBranches)
			assert.Empty(t, stored.RunningStatus)
			assert.Contains(t, stored.Body, "Recovery can persist again.")
			if strings.HasPrefix(phase, "ordinary") {
				assert.Equal(t, ticket.StatusTodo, stored.Status)
				info, err := f.d.GetTicket(stored.ID)
				require.NoError(t, err)
				assert.Equal(t, "todo", info.Status)
				assert.False(t, info.ExecutionActive)
				var updates []web.TicketInfo
				for len(events) > 0 {
					event := <-events
					if event.Type == "ticket_updated" && event.Ticket.ID == stored.ID {
						updates = append(updates, event.Ticket)
					}
				}
				require.NotEmpty(t, updates)
				assert.Equal(t, string(awaitingData), updates[0].Status)
				assert.True(t, updates[0].ExecutionActive)
				assert.Equal(t, "todo", updates[len(updates)-1].Status)
				assert.False(t, updates[len(updates)-1].ExecutionActive)
				assert.True(t, f.d.isSelfWrite(f.path))
				f.d.handleEvent(watcher.Event{Op: watcher.OpChanged, Path: f.path})
				assert.Empty(t, events, "recovery's own file event must remain suppressed")
				assert.Empty(t, f.d.queue)
			}
		})
	}
}

func TestCustomRunningReloadCapturedPolicy(t *testing.T) {
	for _, replacement := range []string{"new status", "removed pipeline"} {
		t.Run(replacement, func(t *testing.T) {
			h := newReloadHarness(t)
			initial := h.yaml(configOpts{statuses: "statuses: [awaiting_data]\n", pipelines: `  two-stage:
    - stage: step1
      agent: agent1
      running_status: awaiting_data
      on_success: next
      on_failure: pause
    - stage: step2
      agent: agent2
      on_success: done
      on_failure: pause
`})
			path := h.writeTicket("tst-reload-custom.md", h.taskMD("tst-reload-custom", "todo", "two-stage"))
			var d *Daemon
			calls := 0
			d = h.newDaemonWithConfig(t, initial, WithRunner(func(_ context.Context, _ RunnerParams) (process.Result, error) {
				calls++
				if calls == 1 {
					assert.Equal(t, awaitingData, h.readTask("tst-reload-custom.md").RunningStatus)
					opts := configOpts{statuses: "statuses: [processing_data]\n", pipelines: `  two-stage:
    - stage: step1
      agent: agent1
      on_success: done
      on_failure: pause
    - stage: step2
      agent: agent2
      running_status: processing_data
      on_success: done
      on_failure: pause
`}
					if replacement == "removed pipeline" {
						opts = configOpts{pipelines: `  unrelated:
    - stage: step1
      agent: agent1
      on_success: done
      on_failure: pause
`}
					}
					h.writeConfigAtomic(t, h.yaml(opts))
					require.NoError(t, d.reloadConfig())
					d.handleFileChanged(path)
					d.mu.Lock()
					assert.Equal(t, awaitingData, d.executions["tst-reload-custom"].expected.status)
					assert.Equal(t, awaitingData, d.executions["tst-reload-custom"].expected.runningStatus)
					d.mu.Unlock()
					rows := d.ListTickets(web.ListTicketsOptions{})
					require.Len(t, rows, 1)
					assert.True(t, rows[0].ExecutionActive)
					assert.Equal(t, string(awaitingData), rows[0].Status)
				} else {
					assert.Equal(t, ticket.Status("processing_data"), h.readTask("tst-reload-custom.md").RunningStatus)
				}
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now}, nil
			}))
			d.tickets["tst-reload-custom"] = newTicketState(h.readTask("tst-reload-custom.md"), path)
			d.runTicket(context.Background(), "tst-reload-custom")
			got := h.readTask("tst-reload-custom.md")
			assert.Equal(t, ticket.StatusTodo, got.Status, "captured on_success next survives replacement")
			assert.Empty(t, got.RunningStatus)
			assert.Equal(t, "step2", got.Stage)
			d.runTicket(context.Background(), got.ID)
			got = h.readTask("tst-reload-custom.md")
			if replacement == "removed pipeline" {
				assert.Equal(t, ticket.StatusPaused, got.Status)
				assert.Contains(t, got.LastError, "unknown pipeline")
				assert.Equal(t, 1, calls)
			} else {
				assert.Equal(t, ticket.StatusDone, got.Status)
				assert.Equal(t, 2, calls)
			}
		})
	}
}

func TestCustomRunningSupervisedFinalization(t *testing.T) {
	for _, failedWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("failedWrite=%t", failedWrite), func(t *testing.T) {
			f := newResultFixture(t)
			customRunningPipeline(f.h, "two-stage")
			f.h.cfg.Pipelines["two-stage"][1].OnSuccess = string(awaitingData)
			require.NoError(t, f.tkt.SetField("status", "todo"))
			f.write(t)
			if failedWrite {
				f.h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Run: `chmod 444 "$KONTORA_TICKET_FILE"`}}}
				t.Cleanup(func() { _ = os.Chmod(f.path, 0o644) })
			}
			d := f.h.newDaemon(f.h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				rec, err := loadExecution(f.h.cfg, f.tkt.ID)
				require.NoError(t, err)
				assert.Equal(t, awaitingData, rec.Status)
				assert.Equal(t, awaitingData, rec.RunningStatus)
				assert.Equal(t, "running", rec.Phase)
				publishDaemonResult(t, p.Env["KONTORA_RESULT_FILE"], "complete")
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationWindowGone}, nil
			}))
			d.windows.list = func(string) ([]string, error) { return nil, nil }
			d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
			d.runTicket(context.Background(), f.tkt.ID)
			if failedWrite {
				got := f.h.readTask(resultTicketID + ".md")
				assert.Equal(t, awaitingData, got.Status)
				assert.Equal(t, awaitingData, got.RunningStatus)
				assert.Contains(t, d.running, got.ID)
				rec, err := loadExecution(f.h.cfg, got.ID)
				require.NoError(t, err)
				assert.Equal(t, "transition", rec.Phase)
				d.handleFileChanged(f.path)
				assert.Equal(t, awaitingData, d.tickets[got.ID].ticket.RunningStatus)
				require.NoError(t, os.Chmod(f.path, 0o644))
				d.handleFileChanged(f.path)
				d.background.Wait()
			}
			got := f.h.readTask(resultTicketID + ".md")
			assert.Equal(t, awaitingData, got.Status)
			assert.Empty(t, got.RunningStatus)
			assert.False(t, got.IsRunning())
			require.Len(t, got.History, 1)
			assert.Equal(t, ticket.RunOutcomeComplete, got.History[0].RunOutcome)
			assert.Empty(t, d.running)
			assert.Zero(t, d.sem.used)
			assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
		})
	}
}

func TestCustomRunningRecoveryJournalWriteRollback(t *testing.T) {
	f := makeCustomResultFixture(t)
	before := f.tkt.Clone()
	dir := filepath.Dir(executionPath(f.h.cfg, f.tkt.ID))
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	f.d.mu.Lock()
	applied := f.d.applyRecoveredExecutionLocked(f.tkt, f.rec)
	f.d.mu.Unlock()
	assert.False(t, applied)
	assert.Equal(t, before, f.tkt.Clone())
	assert.Contains(t, f.d.running, f.tkt.ID)
	assert.Equal(t, 1, f.d.sem.used)
}

func TestCustomRunningDefaultsForSpecialClaims(t *testing.T) {
	for _, mode := range []string{"standalone", "annotation", "supervisor", "builtin rework", "pipeline default", "pipeline custom"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			customRunningPipeline(h, "two-stage")
			path := h.writeTicket("tst-default.md", h.taskMD("tst-default", "todo", "two-stage"))
			tk := h.readTask("tst-default.md")
			switch mode {
			case "standalone":
				require.NoError(t, tk.SetField("pipeline", ""))
			case "annotation":
				require.NoError(t, tk.SetField("annotation_return_status", "open"))
			case "supervisor":
				require.NoError(t, tk.SetField("kind", "epic"))
				require.NoError(t, tk.SetField("epic_execution", "agent"))
			case "builtin rework":
				h.cfg.ReworkIsBuiltin = true
				require.NoError(t, tk.SetField("stage", config.ReworkStageName))
			case "pipeline default":
				h.cfg.Pipelines["two-stage"][0].RunningStatus = ""
			}
			saveCustomTestTicket(t, tk, path)
			d := h.newDaemon(h.cfg)
			d.tickets[tk.ID] = newTicketState(tk, path)
			ctx, cancel, snapshot, filePath, branch, err := d.claimExecution(context.Background(), h.cfg, tk.ID, testLogger(t))
			require.NoError(t, err)
			defer d.finishOrdinaryRun(ctx, cancel, tk.ID, branch, filePath, testLogger(t))
			assert.True(t, snapshot.IsRunning())
			if mode == "pipeline custom" {
				assert.Equal(t, awaitingData, snapshot.Status)
				assert.Equal(t, awaitingData, snapshot.RunningStatus)
			} else {
				assert.Equal(t, ticket.StatusInProgress, snapshot.Status)
				assert.Empty(t, snapshot.RunningStatus)
			}
		})
	}
}

func TestCustomRunningJournalDefaults(t *testing.T) {
	f := newResultFixture(t)
	assert.True(t, executionMatches(f.tkt, f.rec), "old journals default to in_progress without a snapshot")
	require.NoError(t, f.tkt.SetRunningStatus(awaitingData))
	assert.False(t, executionMatches(f.tkt, f.rec), "legacy journal cannot adopt a custom-active execution")
	f.rec.Status, f.rec.RunningStatus = awaitingData, awaitingData
	assert.True(t, executionMatches(f.tkt, f.rec))
	require.NoError(t, f.tkt.SetField("running_status", ""))
	assert.False(t, executionMatches(f.tkt, f.rec))
}

func TestCustomRunningOperatorAndProjectionProtections(t *testing.T) {
	for _, operation := range []string{"pause", "cancel", "retry", "skip", "stage", "body", "secondary"} {
		t.Run(operation, func(t *testing.T) {
			h := newHarness(t)
			customRunningPipeline(h, "two-stage")
			path := h.writeTicket("tst-protected.md", h.taskMD("tst-protected", "todo", "two-stage"))
			d := h.newDaemon(h.cfg)
			d.tickets["tst-protected"] = newTicketState(h.readTask("tst-protected.md"), path)
			ctx, cancel, snapshot, filePath, branch, err := d.claimExecution(context.Background(), h.cfg, "tst-protected", testLogger(t))
			require.NoError(t, err)
			defer d.finishOrdinaryRun(ctx, cancel, snapshot.ID, branch, filePath, testLogger(t))
			switch operation {
			case "pause":
				require.NoError(t, d.PauseTicket(snapshot.ID))
				assert.Error(t, ctx.Err())
				assert.Empty(t, h.readTask("tst-protected.md").RunningStatus)
				assert.Error(t, d.SetStage(snapshot.ID, "step2"), "paused does not mean teardown finished")
			case "cancel":
				require.NoError(t, d.MoveTicket(snapshot.ID, "cancelled"))
				assert.Error(t, ctx.Err())
			case "retry":
				assert.Error(t, d.RetryTicket(snapshot.ID))
			case "skip":
				assert.Error(t, d.SkipStage(snapshot.ID))
			case "stage":
				assert.Error(t, d.SetStage(snapshot.ID, "step2"))
			case "body":
				err := d.UpdateTicket(snapshot.ID, web.UpdateTicketRequest{Body: new("unsafe replacement")})
				assert.Error(t, err)
			case "secondary":
				assert.Error(t, secondaryClaimRefusal(snapshot))
			}
		})
	}

	h := newHarness(t)
	customRunningPipeline(h, "one-stage")
	path := h.writeTicket("tst-visible.md", h.taskMD("tst-visible", "todo", "one-stage"))
	tk := h.readTask("tst-visible.md")
	require.NoError(t, tk.SetRunningStatus(awaitingData))
	d := h.newDaemon(h.cfg)
	d.tickets[tk.ID] = newTicketState(tk, path)
	next := *h.cfg
	next.Statuses = nil
	d.cfg.Store(&next)
	rows := d.ListTickets(web.ListTicketsOptions{})
	require.Len(t, rows, 1)
	assert.Equal(t, string(awaitingData), rows[0].Status)
	assert.True(t, rows[0].ExecutionActive)
	child := childInfo(&next, tk)
	assert.True(t, child.ExecutionActive)
	assert.Nil(t, child.CompletedAt)
	assert.Error(t, d.MoveTicket(tk.ID, string(awaitingData)), "fallback visibility cannot authorize undeclared moves")
	standalone := tk.Clone()
	require.NoError(t, standalone.SetField("pipeline", ""))
	require.NoError(t, standalone.SetField("started_at", "2026-01-01T00:00:00Z"))
	require.NoError(t, standalone.SetField("completed_at", "2026-01-01T01:00:00Z"))
	assert.False(t, simpleTicketEnded(standalone), "a stale completion stamp cannot turn a live custom execution into a legacy run")
}

func TestCustomRunningManagedEnrollmentRefused(t *testing.T) {
	f := newManagedFixture(t)
	path := filepath.Join(f.dir, "tst-001.md")
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	require.NoError(t, tk.SetRunningStatus(awaitingData))
	saveCustomTestTicket(t, tk, path)
	_, err = f.m.Enroll(tk.ID, "work", "objective", 1)
	require.Error(t, err)
	assert.Nil(t, f.cached)
}

func TestCustomRunningRecoveryUncertainTermination(t *testing.T) {
	f := makeCustomResultFixture(t)
	f.d.windows.list = func(string) ([]string, error) { return nil, errors.New("inspection unavailable") }
	before, err := os.ReadFile(f.path)
	require.NoError(t, err)
	got := f.reconcile(t)
	assert.True(t, got.IsRunning())
	assert.Equal(t, awaitingData, got.RunningStatus)
	after, err := os.ReadFile(f.path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, 1, f.d.sem.used)
	assert.Len(t, f.d.runningBranches, 1)
	f.d.windows.list = func(string) ([]string, error) { return nil, nil }
	f.d.handleFileChanged(f.path)
	assert.Empty(t, f.d.running)
	assert.Equal(t, ticket.StatusTodo, f.h.readTask(resultTicketID+".md").Status)
}

func TestCustomRunningPassiveEpicAndParkedSibling(t *testing.T) {
	h := newHarness(t)
	h.writeTicket("epc-custom.md", fmt.Sprintf("---\nid: epc-custom\nkind: epic\nstatus: open\npath: %s\n---\n# Epic\n", h.repoDir))
	path := h.writeTicket("tst-child.md", strings.Replace(h.taskMD("tst-child", "todo", ""), "kontora: true\n", "kontora: true\nparent: epc-custom\n", 1))
	tk := h.readTask("tst-child.md")
	require.NoError(t, tk.SetRunningStatus(awaitingData))
	saveCustomTestTicket(t, tk, path)
	d := h.newDaemon(h.cfg)
	// Avoid recovery: this test derives the passive epic against the live snapshot.
	d.tickets[tk.ID] = newTicketState(tk, path)
	d.tickets["epc-custom"] = newTicketState(h.readTask("epc-custom.md"), filepath.Join(h.tasksDir, "epc-custom.md"))
	d.mu.Lock()
	d.rederiveAllEpicsLocked()
	d.mu.Unlock()
	epic, err := d.GetTicket("epc-custom")
	require.NoError(t, err)
	assert.Equal(t, "in_progress", epic.Status)
	assert.False(t, epic.ExecutionActive, "a passive epic is not a process")
	require.Len(t, epic.Children, 1)
	assert.True(t, epic.Children[0].ExecutionActive)
	require.NoError(t, tk.SetField("status", string(awaitingData)))
	d.tickets[tk.ID] = newTicketState(tk, path)
	d.mu.Lock()
	d.rederiveAllEpicsLocked()
	d.mu.Unlock()
	epic, err = d.GetTicket("epc-custom")
	require.NoError(t, err)
	assert.Equal(t, "open", epic.Status)
	assert.False(t, epic.Children[0].ExecutionActive)
}
