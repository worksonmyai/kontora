package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func TestResultLaunchBindingWriteFailureRecovers(t *testing.T) {
	for _, previous := range []string{"", "inv-result"} {
		t.Run("previous="+previous, func(t *testing.T) {
			f := newResultFixture(t)
			require.NoError(t, f.tkt.SetField("invocation_id", previous))
			f.write(t)
			require.NoError(t, os.Chmod(f.path, 0o444))
			t.Cleanup(func() { _ = os.Chmod(f.path, 0o644) })
			params := RunnerParams{Env: map[string]string{}}
			_, err := f.d.bindExecution(f.tkt, spawnAgentParams{cfg: f.h.cfg, ticketID: f.tkt.ID,
				filePath: f.path, stageName: f.tkt.Stage, wtPath: f.h.wtDir}, &params, f.rec.EpicID)
			require.Error(t, err)
			assert.Empty(t, params.Env["KONTORA_INVOCATION_ID"], "failed binding cannot authorize a runner")
			assert.False(t, params.RequireTermination)
			rec, err := loadExecution(f.h.cfg, f.tkt.ID)
			require.NoError(t, err)
			assert.Equal(t, "preparing", rec.Phase)
			assert.Equal(t, previous, f.h.readTask(resultTicketID+".md").InvocationID)
			require.NoError(t, os.Chmod(f.path, 0o644))
			for restart := range 2 {
				d := f.h.newDaemon(f.h.cfg)
				d.windows.list = func(string) ([]string, error) { return nil, nil }
				require.NoError(t, d.initialScan(f.h.tasksDir))
				got := f.h.readTask(resultTicketID + ".md")
				assert.Equal(t, ticket.StatusTodo, got.Status, "restart %d", restart)
				assert.Empty(t, got.InvocationID)
				assert.Empty(t, got.History)
				assert.Empty(t, d.running)
				assert.Empty(t, d.sem)
				assert.Empty(t, d.runningBranches)
			}
		})
	}
}

// Write the disk states at each write boundary without timing a process crash.
func prepareLaunchRecovery(t *testing.T, f *resultFixture, previous string, bound bool) {
	t.Helper()
	f.rec.Invocation.ID = "inv-prepared"
	f.rec.Invocation.ResultPath = filepath.Join(f.h.logsDir, f.tkt.ID, "results", f.rec.Invocation.ID+".json")
	f.rec.Phase = "preparing"
	f.rec.PreviousInvocationID = new(previous)
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	id := previous
	if bound {
		id = f.rec.Invocation.ID
	}
	require.NoError(t, f.tkt.SetField("invocation_id", id))
	f.write(t)
}

func TestResultLaunchPreparationCrashBoundaries(t *testing.T) {
	for _, previous := range []string{"", "inv-result"} {
		for _, bound := range []bool{false, true} {
			t.Run(fmt.Sprintf("previous=%s/bound=%t", previous, bound), func(t *testing.T) {
				f := newResultFixture(t)
				prepareLaunchRecovery(t, f, previous, bound)
				// A preparation cannot have launched. Neither report can become history.
				publishDaemonResult(t, f.rec.Invocation.ResultPath, "blocked")
				before, err := os.ReadFile(f.path)
				require.NoError(t, err)
				for range 2 {
					d := f.h.newDaemon(f.h.cfg)
					d.windows = windowOps{
						list: func(string) ([]string, error) { return []string{f.tkt.ID}, nil },
						kill: func(string, string) error { return errors.New("old window remains live") },
					}
					require.NoError(t, d.initialScan(f.h.tasksDir))
					after, err := os.ReadFile(f.path)
					require.NoError(t, err)
					assert.Equal(t, before, after)
					assert.Contains(t, d.running, f.tkt.ID)
					assert.Len(t, d.sem, 1)
					assert.Len(t, d.runningBranches, 1)
				}
				for range 2 {
					d := f.h.newDaemon(f.h.cfg)
					d.windows.list = func(string) ([]string, error) { return nil, nil }
					require.NoError(t, d.initialScan(f.h.tasksDir))
					got := f.h.readTask(resultTicketID + ".md")
					assert.Equal(t, ticket.StatusTodo, got.Status)
					assert.Empty(t, got.InvocationID)
					assert.Empty(t, got.History)
					assert.Empty(t, d.running)
					assert.Empty(t, d.sem)
					assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
				}
			})
		}
	}
}

func TestResultLaunchLegacyUnboundJournalRecovers(t *testing.T) {
	f := newResultFixture(t)
	require.NoError(t, f.tkt.DeleteField("invocation_id"))
	f.write(t)
	publishDaemonResult(t, f.rec.Invocation.ResultPath, "blocked")
	f.d.windows.list = func(string) ([]string, error) { return nil, errors.New("inspection unavailable") }
	assert.Equal(t, ticket.StatusInProgress, f.reconcile(t).Status)
	assert.Contains(t, f.d.running, f.tkt.ID)
	f.d.windows.list = func(string) ([]string, error) { return nil, nil }
	got := f.reconcile(t)
	assert.Equal(t, ticket.StatusTodo, got.Status)
	assert.Empty(t, got.InvocationID)
	assert.Empty(t, got.History, "an unbound journal cannot attribute a report")
	assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
	assert.Empty(t, f.d.running)
	assert.Empty(t, f.d.sem)
}

func TestResultLaunchCommitsBeforeRunner(t *testing.T) {
	f := newResultFixture(t)
	require.NoError(t, f.tkt.SetField("status", "todo"))
	f.write(t)
	calls := 0
	d := f.h.newDaemon(f.h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls++
		rec, err := loadExecution(f.h.cfg, f.tkt.ID)
		require.NoError(t, err)
		bound := f.h.readTask(resultTicketID + ".md")
		assert.Equal(t, "running", rec.Phase)
		require.NotNil(t, rec.PreviousInvocationID)
		assert.Equal(t, "inv-result", *rec.PreviousInvocationID)
		assert.Equal(t, rec.Invocation.ID, bound.InvocationID)
		assert.Equal(t, rec.Invocation.ID, p.Env["KONTORA_INVOCATION_ID"])
		assert.True(t, executionMatches(bound, rec))
		publishDaemonResult(t, rec.Invocation.ResultPath, "complete")
		now := time.Now()
		return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationWindowGone}, nil
	}))
	d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	d.runTicket(context.Background(), f.tkt.ID)
	assert.Equal(t, 1, calls)
	assert.Equal(t, ticket.StatusDone, f.h.readTask(resultTicketID+".md").Status)
}

func TestResultLaunchPreparationKeepsMismatchGuard(t *testing.T) {
	for _, change := range []string{"different identity", "different lifecycle", "missing snapshot", "committed launch", "foreign claim", "managed"} {
		t.Run(change, func(t *testing.T) {
			f := newResultFixture(t)
			prepareLaunchRecovery(t, f, "inv-result", false)
			switch change {
			case "different identity":
				require.NoError(t, f.tkt.SetField("invocation_id", "inv-unrelated"))
			case "different lifecycle":
				require.NoError(t, f.tkt.SetField("attempt", 3))
			case "missing snapshot":
				f.rec.PreviousInvocationID = nil
			case "committed launch":
				f.rec.Phase = "running"
			case "foreign claim":
				require.NoError(t, f.tkt.SetField("claimed_by", "another-instance"))
			case "managed":
				require.NoError(t, f.tkt.SetField("managed", ticket.ManagedOwnership{OwnerID: f.d.instanceName}))
			}
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			f.write(t)
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			f.d.windows.list = func(string) ([]string, error) {
				t.Error("an unrelated identity cannot authorize window inspection")
				return nil, nil
			}
			f.reconcile(t)
			after, err := os.ReadFile(f.path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.FileExists(t, executionPath(f.h.cfg, f.tkt.ID))
			if change == "foreign claim" || change == "managed" {
				assert.Empty(t, f.d.running)
			} else {
				assert.Contains(t, f.d.running, f.tkt.ID)
				assert.Len(t, f.d.sem, 1)
			}
		})
	}
}

func TestResultLaunchPreparationPreservesOperatorState(t *testing.T) {
	for _, bound := range []bool{false, true} {
		for _, status := range []ticket.Status{ticket.StatusPaused, ticket.StatusTodo} {
			t.Run(fmt.Sprintf("bound=%t/%s", bound, status), func(t *testing.T) {
				f := newResultFixture(t)
				prepareLaunchRecovery(t, f, "inv-result", bound)
				require.NoError(t, f.tkt.SetField("status", string(status)))
				f.write(t)
				got := f.reconcile(t)
				assert.Equal(t, status, got.Status)
				assert.Empty(t, got.InvocationID)
				assert.Empty(t, got.History)
				assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
				assert.Empty(t, f.d.running)
			})
		}
	}
}

func TestResultLegacyStartupOverridesRetainResources(t *testing.T) {
	for _, supervisor := range []bool{false, true} {
		for _, journal := range []bool{true, false} {
			for _, status := range []ticket.Status{ticket.StatusPaused, ticket.StatusTodo} {
				t.Run(fmt.Sprintf("supervisor=%t/journal=%t/%s", supervisor, journal, status), func(t *testing.T) {
					f := newResultFixture(t)
					f.h.cfg.MaxConcurrentAgents, f.h.cfg.MaxConcurrentEpicSupervisors = 1, 1
					if supervisor {
						f.rec.EpicID = f.tkt.ID
						require.NoError(t, f.tkt.SetField("kind", "epic"))
						require.NoError(t, f.tkt.SetField("epic_execution", "agent"))
					}
					wt, _, err := f.d.worktrees.Create(worktree.CreateOpts{RepoPath: f.rec.RepoPath,
						RepoName: f.h.repoName, TaskID: f.tkt.ID, Branch: f.rec.Branch})
					require.NoError(t, err)
					f.rec.Worktree = wt
					require.NoError(t, saveExecution(f.h.cfg, f.rec))
					if !journal {
						require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
						require.NoError(t, f.tkt.DeleteField("invocation_id"))
					}
					require.NoError(t, f.tkt.SetField("status", string(status)))
					f.write(t)
					before, err := os.ReadFile(f.path)
					require.NoError(t, err)
					calls := 0
					d := f.h.newDaemon(f.h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
						calls++
						assert.Equal(t, wt, p.Dir)
						now := time.Now()
						return process.Result{ExitCode: 7, StartedAt: now, ExitedAt: now, Termination: process.TerminationWindowGone}, nil
					}))
					d.windows = windowOps{
						list: func(string) ([]string, error) { return []string{f.tkt.ID}, nil },
						kill: func(string, string) error { return errors.New("old runner remains alive") },
					}
					require.NoError(t, d.initialScan(f.h.tasksDir))
					d.cleanOrphanedWindows()
					after, err := os.ReadFile(f.path)
					require.NoError(t, err)
					assert.Equal(t, before, after, "recovery must preserve explicit operator state")
					common, err := worktree.CommonDir(f.h.repoDir)
					require.NoError(t, err)
					assert.Contains(t, d.running, f.tkt.ID)
					assert.Equal(t, f.tkt.ID, d.runningBranches[common+"\x00"+f.tkt.Branch])
					pool := d.ticketPool(f.tkt)
					assert.Len(t, pool, 1)
					assert.Zero(t, d.availablePoolSlotsLocked(pool))
					if status == ticket.StatusPaused {
						require.NoError(t, d.RetryTicket(f.tkt.ID))
					}
					d.runTicket(context.Background(), f.tkt.ID)
					assert.Zero(t, calls, "Retry cannot reuse a worktree while the old window remains alive")
					d.windows.list = func(string) ([]string, error) { return nil, nil }
					d.handleFileChanged(f.path)
					d.background.Wait()
					assert.Empty(t, d.running)
					assert.Empty(t, pool)
					assert.Empty(t, d.runningBranches)
					require.DirExists(t, wt)
				})
			}
		}
	}
}

func TestResultLegacyStartupDisabledTicketRetainsResources(t *testing.T) {
	for _, status := range []ticket.Status{ticket.StatusPaused, ticket.StatusTodo, ticket.StatusInProgress} {
		t.Run(string(status), func(t *testing.T) {
			f := newResultFixture(t)
			require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
			require.NoError(t, f.tkt.DeleteField("invocation_id"))
			require.NoError(t, f.tkt.SetField("status", string(status)))
			require.NoError(t, f.tkt.SetField("kontora", false))
			f.write(t)
			f.d.windows = windowOps{
				list: func(string) ([]string, error) { return []string{f.tkt.ID}, nil },
				kill: func(string, string) error { return errors.New("old runner remains alive") },
			}
			require.NoError(t, f.d.initialScan(f.h.tasksDir))
			assert.Contains(t, f.d.running, f.tkt.ID, "disabling pickup does not confirm runner termination")
			assert.Len(t, f.d.sem, 1)
			got := f.h.readTask(resultTicketID + ".md")
			assert.Equal(t, status, got.Status)
			assert.False(t, got.Kontora)
			f.d.windows.list = func(string) ([]string, error) { return nil, nil }
			f.d.handleFileChanged(f.path)
			assert.Empty(t, f.d.running)
			assert.Empty(t, f.d.sem)
			got = f.h.readTask(resultTicketID + ".md")
			assert.Equal(t, status, got.Status)
			assert.False(t, got.Kontora)
		})
	}
}

func TestResultLegacyStartupRequiresConfirmedAbsence(t *testing.T) {
	for _, inspection := range []string{"list error", "kill returns but window remains"} {
		for _, status := range []ticket.Status{ticket.StatusPaused, ticket.StatusTodo} {
			t.Run(inspection+"/"+string(status), func(t *testing.T) {
				f := newResultFixture(t)
				require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
				require.NoError(t, f.tkt.DeleteField("invocation_id"))
				require.NoError(t, f.tkt.SetField("status", string(status)))
				f.write(t)
				before, err := os.ReadFile(f.path)
				require.NoError(t, err)
				for range 2 {
					d := f.h.newDaemon(f.h.cfg)
					d.windows = windowOps{
						list: func(string) ([]string, error) {
							if inspection == "list error" {
								return nil, errors.New("inspection unavailable")
							}
							return []string{f.tkt.ID}, nil
						},
						kill: func(string, string) error { return nil },
					}
					require.NoError(t, d.initialScan(f.h.tasksDir))
					assert.Contains(t, d.running, f.tkt.ID)
					after, err := os.ReadFile(f.path)
					require.NoError(t, err)
					assert.Equal(t, before, after)
					d.windows.list = func(string) ([]string, error) { return nil, nil }
					d.handleFileChanged(f.path)
					d.handleFileChanged(filepath.Join(f.h.tasksDir, "epc-result.md"))
					assert.Equal(t, status, f.h.readTask(resultTicketID+".md").Status)
					assert.Empty(t, d.running)
					assert.Empty(t, d.sem)
					assert.Empty(t, d.runningBranches)
				}
			})
		}
	}
}

func TestResultLegacyStartupProtectsForeignAuthority(t *testing.T) {
	for _, authority := range []string{"foreign claim", "managed"} {
		for _, status := range []ticket.Status{ticket.StatusPaused, ticket.StatusTodo} {
			t.Run(authority+"/"+string(status), func(t *testing.T) {
				f := newResultFixture(t)
				require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
				require.NoError(t, f.tkt.DeleteField("invocation_id"))
				require.NoError(t, f.tkt.SetField("status", string(status)))
				if authority == "managed" {
					require.NoError(t, f.tkt.SetField("managed", ticket.ManagedOwnership{OwnerID: f.d.instanceName}))
				} else {
					require.NoError(t, f.tkt.SetField("claimed_by", "another-instance"))
				}
				f.write(t)
				before, err := os.ReadFile(f.path)
				require.NoError(t, err)
				d := f.h.newDaemon(f.h.cfg)
				d.windows = windowOps{
					list: func(string) ([]string, error) { return []string{f.tkt.ID}, nil },
					kill: func(string, string) error { t.Error("foreign authority must survive orphan cleanup"); return nil },
				}
				require.NoError(t, d.initialScan(f.h.tasksDir))
				d.cleanOrphanedWindows()
				after, err := os.ReadFile(f.path)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				assert.Empty(t, d.running)
				assert.Empty(t, d.sem)
			})
		}
	}
}
