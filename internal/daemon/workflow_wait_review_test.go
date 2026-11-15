package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func TestWorkflowReleasedContinuationRestartAdmission(t *testing.T) {
	for _, kind := range []string{"human", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			f := workflowEventFixture(t, kind, false)
			if kind == "human" {
				state := f.tkt.GraphState.Clone()
				var err error
				state.Wait.Repository, err = worktree.CommonDir(f.tkt.Path)
				require.NoError(t, err)
				require.NoError(t, f.tkt.SetField("graph_state", state))
				f.write(t)
				_, err = f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
				require.NoError(t, err)
			} else {
				_, err := f.d.DeploymentRelease(web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "restart-event"})
				require.NoError(t, err)
			}
			released := f.h.readTask("tst-release.md")
			require.True(t, released.GraphState.Continuation)
			require.Nil(t, released.GraphState.Wait)
			f.d.reconcileControlTransitions()
			require.NoFileExists(t, workflowControlPath(f.h.cfg, released.ID))
			second := f.h.newDaemon(f.h.cfg)
			require.NoError(t, second.initialScan(f.h.tasksDir))
			second.reconcileWorkflowWaits(context.Background())
			require.True(t, second.queued[released.ID])
		})
	}
}

func TestWorkflowReleasedContinuationWorkspaceAdmission(t *testing.T) {
	for _, change := range []string{"unchanged", "missing", "relocated", "changed branch"} {
		t.Run(change, func(t *testing.T) {
			f := workflowEventFixture(t, "deployment", false)
			_, err := f.d.DeploymentRelease(web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "workspace-event"})
			require.NoError(t, err)
			released := f.h.readTask("tst-release.md")
			w := released.GraphState.WaitHistory[0]
			switch change {
			case "missing":
				require.NoError(t, os.RemoveAll(w.Worktree))
			case "relocated":
				eventTestGit(t, released.Path, "worktree", "move", w.Worktree, filepath.Join(f.h.wtDir, "relocated"))
			case "changed branch":
				eventTestGit(t, w.Worktree, "checkout", "-b", "changed-branch")
			}
			err = f.d.initializeWorkflow(f.h.cfg, released, f.h.cfg.Pipelines[released.Pipeline].Graph)
			if change == "unchanged" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errWorkflowNoSpawn)
				require.Equal(t, ticket.StatusPaused, released.Status)
				require.Contains(t, released.LastError, "worktree")
			}
		})
	}
}

func TestWorkflowReleasedContinuationRestartExecution(t *testing.T) {
	for _, kind := range []string{"human", "deployment"} {
		for _, blocked := range []bool{false, true} {
			name := kind + "/unblocked"
			if blocked {
				name = kind + "/dependency blocked"
			}
			t.Run(name, func(t *testing.T) {
				f := workflowEventFixture(t, kind, false)
				if kind == "human" {
					state := f.tkt.GraphState.Clone()
					var err error
					state.Wait.Repository, err = worktree.CommonDir(f.tkt.Path)
					require.NoError(t, err)
					require.NoError(t, f.tkt.SetField("graph_state", state))
				}
				if blocked {
					require.NoError(t, f.tkt.SetField("deps", []string{"tst-dep"}))
					f.h.writeTicket("tst-dep.md", f.h.taskMD("tst-dep", "open", "one-stage"))
				}
				f.write(t)
				if kind == "human" {
					_, err := f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
					require.NoError(t, err)
				} else {
					_, err := f.d.DeploymentRelease(web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "restart-execution"})
					require.NoError(t, err)
				}
				released := f.h.readTask("tst-release.md")
				require.Nil(t, released.GraphState.Wait)
				f.d.reconcileControlTransitions()
				require.NoFileExists(t, workflowControlPath(f.h.cfg, released.ID))
				expectedWorktree := ""
				if kind == "deployment" {
					var err error
					expectedWorktree, err = filepath.EvalSymlinks(released.GraphState.WaitHistory[0].Worktree)
					require.NoError(t, err)
				}
				var calls atomic.Int32
				second := f.h.newDaemon(f.h.cfg, WithFinalSummarySpawner(func(context.Context, FinalSummaryParams) (string, error) { return "summary", nil }), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
					calls.Add(1)
					if kind == "deployment" {
						assert.Equal(t, expectedWorktree, p.Dir)
					}
					return workflowWaitTestResult(newWorkflowWaitTestClock(), 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "complete", Summary: "continued after restart"})
				}))
				stop := startWorkflowWaitTestDaemon(t, second)
				if blocked {
					require.Eventually(t, func() bool {
						second.mu.Lock()
						defer second.mu.Unlock()
						return second.tickets[released.ID] != nil && second.tickets["tst-dep"] != nil
					}, 5*time.Second, 10*time.Millisecond)
					requireWorkflowWaitTestIdle(t, second)
					require.Zero(t, calls.Load())
					require.Equal(t, released.GraphState, f.h.readTask("tst-release.md").GraphState)
					require.NoError(t, second.MoveTicket("tst-dep", "done"))
				}
				got := f.h.waitForStatus("tst-release.md", ticket.StatusDone, 10*time.Second)
				requireWorkflowWaitTestIdle(t, second)
				stop()
				require.Equal(t, int32(1), calls.Load())
				require.Equal(t, released.GraphState.ExecutionID, got.GraphState.ExecutionID)
				require.Len(t, got.History, 1)
			})
		}
	}
}

func TestWorkflowReleasedContinuationCannotRecreateWorkspace(t *testing.T) {
	for _, kind := range []string{"human", "deployment"} {
		for _, change := range []string{"missing", "relocated", "changed branch"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				f := workflowEventFixture(t, kind, false)
				if kind == "human" {
					releaseReviewCaptureWorkspace(t, f)
					_, err := f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
					require.NoError(t, err)
				} else {
					_, err := f.d.DeploymentRelease(web.DeploymentReleaseRequest{Repository: f.h.repoDir, Environment: "production", Commit: f.tkt.GraphState.Wait.RequiredCommit, EventID: "workspace-admission"})
					require.NoError(t, err)
				}
				released := f.h.readTask("tst-release.md")
				w := released.GraphState.WaitHistory[0]
				require.True(t, f.d.queued[released.ID], "the checkout can change after queue admission")
				releaseReviewChangeWorkspace(t, f, change)
				f.d.runner = func(context.Context, RunnerParams) (process.Result, error) {
					t.Error("continuation must not launch in a recreated or changed checkout")
					return process.Result{}, nil
				}
				f.d.runTicket(context.Background(), released.ID)
				got := f.h.readTask("tst-release.md")
				require.Equal(t, ticket.StatusPaused, got.Status)
				require.Contains(t, got.LastError, "worktree")
				require.Empty(t, got.InvocationID)
				require.Equal(t, released.History, got.History)
				require.Equal(t, released.GraphState, got.GraphState)
				require.NoFileExists(t, executionPath(f.h.cfg, released.ID))
				if change == "changed branch" {
					require.Equal(t, "unexpected-release-branch", eventTestGit(t, w.Worktree, "branch", "--show-current"))
				} else {
					require.NoDirExists(t, w.Worktree, "admission must not recreate the missing checkout")
				}
			})
		}
	}
}

func TestWorkflowWaitTimeoutExhaustedLoop(t *testing.T) {
	for _, status := range []string{"done", "paused", "human_review"} {
		t.Run(status, func(t *testing.T) {
			f := workflowEventFixture(t, "human", false)
			graph := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph
			graph.Nodes[0].Timeout = config.Duration{Duration: time.Hour}
			graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "loop", Type: "loop"}, config.WorkflowNode{ID: "destination", Type: "end", Status: status})
			graph.Edges = append(graph.Edges, config.WorkflowEdge{From: config.WorkflowPort{Node: "gate", Port: "timeout"}, To: "loop"}, config.WorkflowEdge{From: config.WorkflowPort{Node: "loop", Port: "exhausted"}, To: "destination"})
			state := f.tkt.GraphState.Clone()
			fingerprint, err := workflowFingerprint(f.h.cfg, f.tkt, graph)
			require.NoError(t, err)
			state.DefinitionFingerprint = fingerprint
			require.NoError(t, f.tkt.SetField("graph_state", state))
			require.NoError(t, f.d.applyWaitReason(f.tkt, "deadline"))
			require.Equal(t, ticket.Status(status), f.tkt.Status)
			require.Nil(t, f.tkt.GraphState.Wait)
			require.Len(t, f.tkt.GraphState.WaitHistory, 1)
			require.Equal(t, "destination", f.tkt.Stage)
		})
	}
}

func TestWorkflowQueuedProbeReconciliationDeadline(t *testing.T) {
	h, d, clock, path := newWorkflowWaitTestFixture(t, "check")
	before := h.readTask("tst-wait.md")
	d.queueWorkflowProbe(before, path)
	require.Equal(t, time.Minute, d.nextWorkflowWait(clock.Now()))
	state := before.GraphState.Clone()
	state.Wait.Deadline = clock.Now().Add(20 * time.Second)
	require.NoError(t, before.SetField("graph_state", state))
	saveWorkflowWaitTestTicket(t, path, before)
	d.tickets[before.ID] = newTicketState(before, path)
	require.Equal(t, 20*time.Second, d.nextWorkflowWait(clock.Now()))
	clock.Advance(20 * time.Second)
	d.reconcileWorkflowWaits(context.Background())
	got := h.readTask("tst-wait.md")
	require.Equal(t, "ready", got.GraphState.Wait.Phase)
	require.Equal(t, "deadline", got.GraphState.Wake.Reason)
}
