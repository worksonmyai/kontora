package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func TestResultMalformedStartupProtectsJournalResources(t *testing.T) {
	for _, supervisor := range []bool{false, true} {
		t.Run(fmt.Sprintf("supervisor=%t", supervisor), func(t *testing.T) {
			f := newResultFixture(t)
			f.h.cfg.MaxConcurrentAgents, f.h.cfg.MaxConcurrentEpicSupervisors = 1, 1
			if supervisor {
				f.rec.EpicID = f.tkt.ID
				require.NoError(t, f.tkt.SetField("kind", "epic"))
				require.NoError(t, f.tkt.SetField("epic_execution", "agent"))
			}
			common, err := worktree.CommonDir(f.rec.RepoPath)
			require.NoError(t, err)
			f.rec.RepoCommonDir = common
			wt, _, err := f.d.worktrees.Create(worktree.CreateOpts{RepoPath: f.rec.RepoPath, RepoName: f.h.repoName, TaskID: f.tkt.ID, Branch: f.rec.Branch})
			require.NoError(t, err)
			f.rec.Worktree = wt
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			malformed := []byte("---\nid: tst-result\nstatus: [unterminated\n---\n")
			require.NoError(t, os.WriteFile(f.path, malformed, 0o600))
			started := false
			d := f.h.newDaemon(f.h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				started = true
				return process.Result{}, errors.New("unexpected launch")
			}))
			kills := 0
			d.windows = windowOps{
				list: func(string) ([]string, error) { return []string{f.tkt.ID}, nil },
				kill: func(string, string) error { kills++; return errors.New("termination unconfirmed") },
			}
			for range 2 {
				require.NoError(t, d.initialScan(f.h.tasksDir))
				d.cleanOrphanedWindows()
			}
			assert.Zero(t, kills, "an unreadable ticket cannot authorize killing a shared-session window")
			assert.Contains(t, d.running, f.tkt.ID)
			pool := d.sem
			if supervisor {
				pool = d.epicSem
			}
			assert.Equal(t, 1, pool.used)
			assert.Equal(t, 0, d.availablePoolSlotsLocked(pool))
			assert.Equal(t, f.tkt.ID, d.runningBranches[common+"\x00"+f.rec.Branch])
			after, err := os.ReadFile(f.path)
			require.NoError(t, err)
			assert.Equal(t, malformed, after)
			other := f.tkt.Clone()
			require.NoError(t, other.SetField("id", "tst-contender"))
			require.NoError(t, other.SetField("status", "todo"))
			require.NoError(t, other.SetField("invocation_id", ""))
			data, err := other.Marshal()
			require.NoError(t, err)
			otherPath := f.h.writeTicket(other.ID+".md", string(data))
			d.tickets[other.ID] = newTicketState(other, otherPath)
			d.runTicket(context.Background(), other.ID)
			assert.False(t, started, "the contender must not reuse the original worktree")
			assert.Contains(t, f.h.readTask(other.ID+".md").LastError, "in use by ticket")
			require.DirExists(t, wt)
			f.write(t)
			d.handleFileChanged(f.path)
			assert.Positive(t, kills)
			assert.Contains(t, d.running, f.tkt.ID, "repair does not prove termination")
			assert.Equal(t, 1, pool.used)
			d.windows.list = func(string) ([]string, error) { return nil, nil }
			d.handleFileChanged(f.path)
			assert.Equal(t, ticket.StatusTodo, f.h.readTask(resultTicketID+".md").Status)
			assert.Empty(t, d.running)
			assert.Zero(t, pool.used)
			assert.Empty(t, d.runningBranches)
		})
	}
}

func TestResultMalformedStartupRejectsUntrustedJournals(t *testing.T) {
	for _, kind := range []string{"foreign instance", "wrong artifact", "malformed journal", "wrong filename"} {
		t.Run(kind, func(t *testing.T) {
			f := newResultFixture(t)
			switch kind {
			case "foreign instance":
				f.rec.Instance = "another-instance"
			case "wrong artifact":
				f.rec.Invocation.ResultPath = filepath.Join(f.h.logsDir, "foreign.json")
			}
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			if kind == "malformed journal" {
				require.NoError(t, os.WriteFile(executionPath(f.h.cfg, f.tkt.ID), []byte("{"), 0o600))
			}
			require.NoError(t, os.WriteFile(f.path, []byte("---\nstatus: [\n---\n"), 0o600))
			if kind == "wrong filename" {
				require.NoError(t, os.Rename(f.path, filepath.Join(f.h.tasksDir, "alias.md")))
			}
			d := f.h.newDaemon(f.h.cfg)
			d.windows = windowOps{
				list: func(string) ([]string, error) { return []string{f.tkt.ID}, nil },
				kill: func(string, string) error { t.Error("foreign or unverified execution must survive"); return nil },
			}
			require.NoError(t, d.initialScan(f.h.tasksDir))
			d.cleanOrphanedWindows()
			assert.Empty(t, d.running)
			assert.Zero(t, d.sem.used)
			assert.Zero(t, d.epicSem.used)
			assert.Empty(t, d.runningBranches)
			assert.FileExists(t, executionPath(f.h.cfg, f.tkt.ID))
		})
	}
}

func TestResultLiveUncertainReservationCanRecover(t *testing.T) {
	for _, scope := range []string{"ordinary", "child", "supervisor"} {
		for _, rework := range []bool{false, true} {
			if scope == "supervisor" && rework {
				continue
			}
			for _, dispatched := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/rework=%t/dispatched=%t", scope, rework, dispatched), func(t *testing.T) {
					f := newScopedRecoveryFixture(t, scope)
					f.h.cfg.MaxConcurrentAgents, f.h.cfg.MaxConcurrentEpicSupervisors = 1, 1
					f.h.cfg.AutoPickUp = new(false)
					require.NoError(t, f.tkt.SetField("status", "todo"))
					if rework {
						f.h.cfg.ReworkIsBuiltin = true
						f.h.cfg.Stages[config.ReworkStageName] = config.Stage{Prompt: "Address review feedback"}
						require.NoError(t, f.tkt.SetField("stage", config.ReworkStageName))
					}
					f.write(t)
					d := f.h.newDaemon(f.h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
						return process.Result{}, process.ErrTerminationUncertain
					}))
					var inspection atomic.Int32
					d.windows = windowOps{
						list: func(string) ([]string, error) {
							switch inspection.Load() {
							case 0:
								return nil, errors.New("inspection unavailable")
							case 1:
								return []string{f.tkt.ID}, nil
							default:
								return nil, nil
							}
						},
						kill: func(string, string) error { return errors.New("termination unconfirmed") },
					}
					d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
					if dispatched {
						d.enqueue(f.tkt)
						ctx, cancel := context.WithCancel(context.Background())
						var wg sync.WaitGroup
						wg.Go(func() { d.dispatch(ctx, &wg) })
						t.Cleanup(func() { cancel(); wg.Wait() })
						require.Eventually(t, func() bool {
							d.mu.Lock()
							defer d.mu.Unlock()
							_, retained := d.recoveredReservations[f.tkt.ID]
							return retained
						}, 5*time.Second, 10*time.Millisecond)
						cancel()
						wg.Wait()
					} else {
						d.runTicket(context.Background(), f.tkt.ID)
					}
					pool := d.ticketPool(f.tkt)
					reservation, exists := d.recoveredReservations[f.tkt.ID]
					require.True(t, exists)
					assert.Same(t, pool, reservation.pool)
					require.Equal(t, 1, pool.used, "the dispatcher token must transfer, not duplicate or disappear")
					d.handleFileChanged(f.path)
					assert.Contains(t, d.liveRuns, f.tkt.ID)
					require.NoError(t, d.PauseTicket(f.tkt.ID))
					d.background.Wait()
					require.NoError(t, d.RetryTicket(f.tkt.ID))
					inspection.Store(1)
					d.handleFileChanged(f.path)
					assert.Contains(t, d.running, f.tkt.ID)
					assert.Equal(t, 1, pool.used, "Pause and Retry do not prove that the runner stopped")
					assert.Len(t, d.runningBranches, 1)
					inspection.Store(2)
					d.handleFileChanged(f.path)
					d.background.Wait()
					assert.Empty(t, d.running)
					assert.Zero(t, d.sem.used)
					assert.Zero(t, d.epicSem.used)
					assert.Empty(t, d.runningBranches)
					assert.Empty(t, d.liveRuns)
					assert.Empty(t, d.recoveredReservations)
					got := f.h.readTask(resultTicketID + ".md")
					assert.Equal(t, ticket.StatusTodo, got.Status)
					assert.Empty(t, got.InvocationID)
					assert.Empty(t, got.History)
				})
			}
		}
	}
}

func TestResultLiveUncertainInspectionReleasesReservation(t *testing.T) {
	for _, trigger := range []string{"watcher", "callback"} {
		t.Run(trigger, func(t *testing.T) {
			f := newResultFixture(t)
			require.NoError(t, f.tkt.SetField("status", "todo"))
			f.write(t)
			d := f.h.newDaemon(f.h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				publishDaemonResult(t, p.Env["KONTORA_RESULT_FILE"], "complete")
				return process.Result{}, process.ErrTerminationUncertain
			}))
			d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
			d.runTicket(context.Background(), f.tkt.ID)
			rec, err := loadExecution(f.h.cfg, f.tkt.ID)
			require.NoError(t, err)
			require.Contains(t, d.running, f.tkt.ID)
			d.windows.list = func(string) ([]string, error) { return nil, nil }
			d.killAll()
			d.background.Wait()
			require.Contains(t, d.running, f.tkt.ID, "shutdown must leave retained execution recovery to startup")
			if trigger == "callback" {
				d.running[f.tkt.ID]()
			} else {
				d.handleFileChanged(f.path)
			}
			d.background.Wait()
			got := f.h.readTask(resultTicketID + ".md")
			assert.Equal(t, ticket.StatusTodo, got.Status, "a report does not prove validation completed")
			assert.Equal(t, rec.Invocation.ID, got.InvocationID)
			assert.Empty(t, got.History)
			assert.Empty(t, d.running)
			assert.Zero(t, d.sem.used)
			assert.Empty(t, d.runningBranches)
			assert.Empty(t, d.liveRuns)
		})
	}
}

func TestResultStartupCommittedTransitionCleanup(t *testing.T) {
	for _, mismatch := range []string{"none", "status", "stage", "attempt", "pipeline", "branch", "path", "summary", "last_error", "completed_at", "history", "duplicate history", "invocation_id", "claimed_by", "managed", "epic_execution"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newResultFixture(t)
			wt, _, err := f.d.worktrees.Create(worktree.CreateOpts{RepoPath: f.rec.RepoPath, RepoName: f.h.repoName, TaskID: f.tkt.ID, Branch: f.rec.Branch})
			require.NoError(t, err)
			f.rec.Worktree = wt
			ended := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
			f.rec.Phase = "transition"
			f.rec.Transition = &executionTransition{Status: ticket.StatusDone, Stage: f.tkt.Stage, Attempt: f.tkt.Attempt,
				CompletedAt: &ended, Summary: "validated summary", History: ticket.HistoryEntry{InvocationID: f.rec.Invocation.ID, RunOutcome: ticket.RunOutcomeComplete}}
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			f.d.mu.Lock()
			require.True(t, f.d.applyRecoveredExecutionLocked(f.tkt, f.rec))
			require.NoError(t, f.tkt.SetField("invocation_id", ""))
			require.NoError(t, f.d.writeTicketLocked(f.tkt, f.path, notify.OriginDaemon))
			f.d.mu.Unlock()
			switch mismatch {
			case "none":
			case "status":
				require.NoError(t, f.tkt.SetField(mismatch, "paused"))
			case "attempt":
				require.NoError(t, f.tkt.SetField(mismatch, 9))
			case "completed_at":
				require.NoError(t, f.tkt.SetField(mismatch, ended.Add(time.Hour).Format(time.RFC3339)))
			case "history":
				row := f.rec.Transition.History
				row.OutcomeReason = "operator changed history"
				require.NoError(t, f.tkt.SetField("history", []ticket.HistoryEntry{row}))
			case "duplicate history":
				row := f.rec.Transition.History
				require.NoError(t, f.tkt.SetField("history", []ticket.HistoryEntry{row, row}))
			case "managed":
				require.NoError(t, f.tkt.SetField(mismatch, map[string]any{}))
			case "epic_execution":
				require.NoError(t, f.tkt.SetField("kind", "epic"))
				require.NoError(t, f.tkt.SetField("epic_execution", "agent"))
			default:
				require.NoError(t, f.tkt.SetField(mismatch, "operator-edit"))
			}
			f.write(t)
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			hookMarker := filepath.Join(t.TempDir(), "hook-ran")
			f.h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Name: "must-not-repeat", Run: "touch " + hookMarker}}}
			for range 2 {
				d, collect := f.h.newMetricsDaemon(f.h.cfg)
				d.windows.list = func(string) ([]string, error) { return nil, nil }
				require.NoError(t, d.initialScan(f.h.tasksDir))
				d.background.Wait()
				after, err := os.ReadFile(f.path)
				require.NoError(t, err)
				assert.Equal(t, before, after, "a committed transition must not rewrite operator state or history")
				assert.NotContains(t, collect(), "kontora.stage.runs", "recovery must not count a live run again")
			}
			assert.NoFileExists(t, hookMarker)
			if mismatch == "none" {
				assert.NoDirExists(t, wt)
				assert.NoFileExists(t, executionPath(f.h.cfg, f.tkt.ID))
			} else {
				assert.DirExists(t, wt, "only the exact committed transition authorizes cleanup")
			}
		})
	}
}

func TestResultLiveFinalizationKeepsJournalThroughCleanup(t *testing.T) {
	f := newResultFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.d.rememberFinalization(f.rec, func(*ticket.Ticket) {
		calls.Add(1)
		close(started)
		<-release
	})
	var wg sync.WaitGroup
	wg.Go(func() { f.d.completeFinalization(f.rec, f.tkt) })
	<-started
	assert.FileExists(t, executionPath(f.h.cfg, f.tkt.ID), "cleanup must finish before the journal is retired")
	close(release)
	wg.Wait()
	f.d.completeFinalization(f.rec, f.tkt)
	assert.EqualValues(t, 1, calls.Load())
	assert.NoFileExists(t, executionPath(f.h.cfg, f.tkt.ID))
}
