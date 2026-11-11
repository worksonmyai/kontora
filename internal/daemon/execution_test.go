package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestExecutionClaimRespectsDiskCancellation(t *testing.T) {
	for _, mode := range []string{"pipeline", "simple", "epic", "annotation", "rework"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			var launched, prepared bool
			d := h.newDaemon(h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				launched = true
				return process.Result{}, nil
			}), WithExecutionPreparer(func(context.Context, string, string) error { prepared = true; return nil }))
			path := h.writeTicket("tst-claim.md", h.taskMD("tst-claim", "todo", "two-stage"))
			tk := h.readTask("tst-claim.md")
			switch mode {
			case "simple":
				require.NoError(t, tk.SetField("pipeline", ""))
			case "epic":
				require.NoError(t, tk.SetField("kind", "epic"))
				require.NoError(t, tk.SetField("epic_execution", "agent"))
			case "annotation":
				require.NoError(t, tk.SetField("annotation_return_status", "open"))
			case "rework":
				h.cfg.ReworkIsBuiltin = true
				require.NoError(t, tk.SetField("stage", config.ReworkStageName))
			}
			data, err := tk.Marshal()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o644))
			d.tickets[tk.ID] = newTicketState(tk, path)
			require.NoError(t, cli.Cancel(h.cfg, tk.ID))
			d.runTicket(context.Background(), tk.ID)
			got := h.readTask("tst-claim.md")
			assert.Equal(t, ticket.StatusCancelled, got.Status)
			assert.Empty(t, got.Branch)
			assert.Nil(t, got.StartedAt)
			assert.False(t, launched)
			assert.False(t, prepared)
			assert.Empty(t, d.running)
			assert.Empty(t, d.runningBranches)
			assert.Empty(t, d.executions)
		})
	}
}

func TestExecutionExceptionalFailuresPreserveMutations(t *testing.T) {
	for _, mode := range []string{"pipeline", "simple"} {
		for _, failure := range []string{"runner", "timeout", "provider", "cancel-during-hook"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				h := newHarness(t)
				pipelineName := "two-stage"
				if mode == "simple" {
					pipelineName = ""
				}
				path := h.writeTicket("tst-failure.md", h.taskMD("tst-failure", "todo", pipelineName))
				cancelFile := filepath.Join(t.TempDir(), "cancelled.md")
				if failure == "cancel-during-hook" {
					h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Run: "cp " + cancelFile + " \"$KONTORA_TICKET_FILE\""}}}
				}
				agent := h.cfg.Agents["agent1"]
				agent.FailurePatterns = []string{"provider rejected"}
				h.cfg.Agents["agent1"] = agent
				var d *Daemon
				d = h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
					require.NoError(t, d.AddNote("tst-failure", web.AddNoteRequest{Text: "saved instruction"}))
					require.NoError(t, d.SetSummary("tst-failure", "saved summary"))
					now := time.Now()
					result := process.Result{StartedAt: now, ExitedAt: now.Add(time.Second), Termination: process.TerminationProcessGroupGone}
					switch failure {
					case "timeout":
						return result, context.DeadlineExceeded
					case "provider":
						require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
						require.NoError(t, os.WriteFile(p.LogFile, []byte("provider rejected\n"), 0o644))
						return result, nil
					case "cancel-during-hook":
						current := h.readTask("tst-failure.md")
						require.NoError(t, current.SetField("status", "cancelled"))
						data, err := current.Marshal()
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(cancelFile, data, 0o644))
					}
					return result, errors.New("runner broke")
				}))
				d.tickets["tst-failure"] = newTicketState(h.readTask("tst-failure.md"), path)
				d.runTicket(context.Background(), "tst-failure")
				got := h.readTask("tst-failure.md")
				want := ticket.StatusPaused
				if failure == "cancel-during-hook" {
					want = ticket.StatusCancelled
				}
				assert.Equal(t, want, got.Status)
				assert.Contains(t, got.Body, "saved instruction")
				assert.Equal(t, "saved summary", got.Summary)
				if failure == "cancel-during-hook" {
					assert.Empty(t, got.History, "a hook's lifecycle override still wins")
				} else {
					require.Len(t, got.History, 1)
					assert.Equal(t, ticket.RunOutcomeFailed, got.History[0].RunOutcome)
					assert.Equal(t, "saved summary", got.History[0].Summary)
					assert.NotEmpty(t, got.History[0].InvocationID)
				}
				assert.Empty(t, d.running)
				assert.Empty(t, d.runningBranches)
				assert.Empty(t, d.executions)
			})
		}
	}
}

func TestExecutionResultRejectsChangedIdentity(t *testing.T) {
	for _, change := range []struct {
		name, key string
		value     any
	}{
		{"stage", "stage", "step2"}, {"todo", "status", "todo"}, {"foreign owner", "claimed_by", "another-daemon"}, {"attempt", "attempt", 4},
	} {
		t.Run(change.name, func(t *testing.T) {
			h := newHarness(t)
			path := h.writeTicket("tst-stale.md", h.taskMD("tst-stale", "todo", "two-stage"))
			var d *Daemon
			d = h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				publishCompleteResult(t, p)
				require.NoError(t, d.mutateTicket("tst-stale", func(tk *ticket.Ticket) error { return tk.SetField(change.key, change.value) }))
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, nil
			}))
			d.tickets["tst-stale"] = newTicketState(h.readTask("tst-stale.md"), path)
			d.runTicket(context.Background(), "tst-stale")
			got := h.readTask("tst-stale.md")
			assert.Empty(t, got.History)
			assert.NotEqual(t, ticket.StatusDone, got.Status)
			switch change.key {
			case "stage":
				assert.Equal(t, "step2", got.Stage)
			case "status":
				assert.Equal(t, ticket.StatusTodo, got.Status)
			case "claimed_by":
				assert.Equal(t, "another-daemon", got.ClaimedBy)
			case "attempt":
				assert.Equal(t, 4, got.Attempt)
			}
		})
	}
}

func TestExecutionPreparationOwnership(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		cancel     bool
		retained   int
		wantStatus ticket.Status
	}{
		{name: "uncertain inspection", err: errors.Join(process.ErrTerminationUncertain, errors.New("inspection failed")), retained: 1, wantStatus: ticket.StatusPaused},
		{name: "cleanup error with confirmed absence", err: errors.New("cleanup failed after confirmed absence"), wantStatus: ticket.StatusPaused},
		{name: "disk cancellation during preparation", cancel: true, wantStatus: ticket.StatusCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			path := h.writeTicket("tst-preflight.md", h.taskMD("tst-preflight", "todo", "one-stage"))
			launched := false
			d := h.newDaemon(h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				launched = true
				return process.Result{}, nil
			}), WithExecutionPreparer(func(context.Context, string, string) error {
				if tc.cancel {
					require.NoError(t, cli.Cancel(h.cfg, "tst-preflight"))
				}
				return tc.err
			}))
			d.tickets["tst-preflight"] = newTicketState(h.readTask("tst-preflight.md"), path)
			d.runTicket(context.Background(), "tst-preflight")
			assert.False(t, launched)
			got := h.readTask("tst-preflight.md")
			assert.Equal(t, tc.wantStatus, got.Status)
			if tc.err != nil {
				assert.Contains(t, got.LastError, "replacement cleanup failed")
			}
			assert.Len(t, d.running, tc.retained)
			assert.Len(t, d.runningBranches, tc.retained)
			assert.Len(t, d.executions, tc.retained)
			_, err := os.Stat(d.worktrees.Path(h.repoName, "tst-preflight"))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func TestExecutionClaimRejectsStaleWatcherSnapshot(t *testing.T) {
	h := newHarness(t)
	path := h.writeTicket("tst-observed.md", h.taskMD("tst-observed", "todo", "one-stage"))
	before := h.readTask("tst-observed.md")
	observed, err := os.Stat(path)
	require.NoError(t, err)
	d := h.newDaemon(h.cfg)
	d.tickets[before.ID] = newTicketState(before, path)
	ctx, cancel, claimed, filePath, branchKey, err := d.claimExecution(context.Background(), h.cfg, before.ID, testLogger(t))
	require.NoError(t, err)
	defer d.finishOrdinaryRun(ctx, cancel, before.ID, branchKey, filePath, testLogger(t))
	d.adoptObservedTicket(before, path, observed)
	assert.NoError(t, ctx.Err(), "an obsolete watcher read cannot cancel the newly claimed execution")
	assert.Equal(t, ticket.StatusInProgress, d.tickets[before.ID].ticket.Status)
	assert.Equal(t, claimed.Branch, d.tickets[before.ID].ticket.Branch)
	assert.Empty(t, d.queued)
}

func TestExecutionClaimRejectsFreshDependency(t *testing.T) {
	h := newHarness(t)
	path := h.writeTicket("tst-dependent.md", h.taskMD("tst-dependent", "todo", "one-stage"))
	h.writeTicket("tst-prereq.md", h.taskMD("tst-prereq", "open", "one-stage"))
	launched := false
	d := h.newDaemon(h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
		launched = true
		return process.Result{}, nil
	}))
	d.tickets["tst-dependent"] = newTicketState(h.readTask("tst-dependent.md"), path)
	current := h.readTask("tst-dependent.md")
	require.NoError(t, current.SetField("deps", []string{"tst-prereq"}))
	data, err := current.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	d.runTicket(context.Background(), "tst-dependent")
	got := h.readTask("tst-dependent.md")
	assert.False(t, launched)
	assert.Equal(t, ticket.StatusTodo, got.Status)
	assert.Equal(t, []string{"tst-prereq"}, got.Deps)
	assert.Empty(t, d.running)
}
