package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestWorkflowControlWaitUnderSaturation(t *testing.T) {
	h := newHarness(t)
	h.cfg.Pipelines["control"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{Version: 2, Entry: "approval", Nodes: []config.WorkflowNode{{ID: "approval", Type: "wait", Mode: "human"}, {ID: "done", Type: "end", Status: "done"}}, Edges: []config.WorkflowEdge{{From: config.WorkflowPort{Node: "approval", Port: "released"}, To: "done"}}}}
	d := h.newDaemon(h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
		t.Error("control wait launched a runner")
		return process.Result{}, nil
	}))
	d.sem.used = d.sem.limit
	h.writeTicket("tst-control.md", h.taskMD("tst-control", "todo", "control"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	parked := h.waitForStatus("tst-control.md", ticket.StatusPaused, 5*time.Second)
	require.NotNil(t, parked.GraphState.Wait)
	require.Equal(t, "human", parked.GraphState.Wait.Kind)
	require.Zero(t, parked.GraphState.Visit)
	require.Empty(t, parked.History)
	d.mu.Lock()
	require.Equal(t, d.sem.limit, d.sem.used)
	d.mu.Unlock()
}

func TestWorkflowReleasedContinuationControls(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	_, err := f.d.WorkflowRelease(f.tkt.ID, eventHumanRequest(f))
	require.NoError(t, err)
	ready := f.h.readTask("tst-release.md")
	require.True(t, ready.HasWorkflowWaitAuthority())
	require.Nil(t, ready.GraphState.Wait)
	require.Error(t, f.d.DeleteTicket(ready.ID))
	require.Error(t, f.d.MoveTicket(ready.ID, "done"))
	require.NoError(t, f.d.PauseTicket(ready.ID))
	paused := f.h.readTask("tst-release.md")
	require.Equal(t, ticket.StatusPaused, paused.Status)
	require.True(t, paused.GraphState.Continuation)
	require.NoError(t, f.d.RetryTicket(ready.ID))
	require.Equal(t, ticket.StatusTodo, f.h.readTask("tst-release.md").Status)
	require.NoError(t, f.d.MoveTicket(ready.ID, "cancelled"))
	require.False(t, f.h.readTask("tst-release.md").HasWorkflowWaitAuthority())
}

func TestWorkflowControlTimeoutFrozenRecovery(t *testing.T) {
	h := newHarness(t)
	h.cfg.Pipelines["control"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{Version: 2, Entry: "approval", Nodes: []config.WorkflowNode{{ID: "approval", Type: "wait", Mode: "human", Timeout: config.Duration{Duration: time.Hour}}, {ID: "done", Type: "end", Status: "done"}}, Edges: []config.WorkflowEdge{{From: config.WorkflowPort{Node: "approval", Port: "released"}, To: "done"}, {From: config.WorkflowPort{Node: "approval", Port: "timeout"}, To: "done"}}}}
	clock := newWorkflowWaitTestClock()
	first := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock))
	path := h.writeTicket("tst-control.md", h.taskMD("tst-control", "todo", "control"))
	first.tickets["tst-control"] = newTicketState(h.readTask("tst-control.md"), path)
	first.runTicket(context.Background(), "tst-control")
	require.Equal(t, ticket.StatusPaused, h.readTask("tst-control.md").Status)
	require.NoError(t, os.Chmod(path, 0444))
	t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0644)) })
	clock.Advance(time.Hour)
	first.reconcileWorkflowWaits(context.Background())
	data, err := os.ReadFile(workflowControlPath(h.cfg, "tst-control"))
	require.NoError(t, err)
	var journal workflowControlTransition
	require.NoError(t, json.Unmarshal(data, &journal))
	journal.Path = journalTicketAlias(t, path)
	data, err = json.Marshal(journal)
	require.NoError(t, err)
	require.NoError(t, atomicWriteFile(workflowControlPath(h.cfg, "tst-control"), data, 0600))
	require.Equal(t, ticket.StatusDone, journal.Status)
	require.NotNil(t, journal.CompletedAt)
	require.Equal(t, ticket.StatusPaused, h.readTask("tst-control.md").Status)
	require.NoError(t, os.Chmod(path, 0644))
	clock.Advance(time.Hour)
	second := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock))
	require.NoError(t, second.initialScan(h.tasksDir))
	second.reconcileControlTransitions()
	got := h.readTask("tst-control.md")
	require.Equal(t, ticket.StatusDone, got.Status)
	require.Equal(t, journal.CompletedAt, got.CompletedAt)
	require.Equal(t, journal.After, got.GraphState)
	require.Empty(t, got.History)
	require.NoFileExists(t, workflowControlPath(h.cfg, got.ID))
}

func TestWorkflowReleaseRejectsCompetingAcceptedReceipt(t *testing.T) {
	f := workflowEventFixture(t, "human", false)
	req := eventHumanRequest(f)
	receipt := &ticket.WorkflowReceipt{ID: workflowEventKey("human", req.RequestID), RequestID: req.RequestID, Reason: req.Reason, AcceptedAt: time.Now().UTC()}
	application, err := f.d.releaseApplication(f.tkt, f.path, receipt)
	require.NoError(t, err)
	event := &workflowEvent{Version: 1, Kind: "human", Human: &req, TicketID: f.tkt.ID, Receipt: receipt, Applications: []*workflowEventApplication{application}}
	require.NoError(t, saveWorkflowEvent(f.d.workflowEventPath("human", req.RequestID), event))
	competing := req
	competing.RequestID = "different-request"
	_, err = f.d.WorkflowRelease(f.tkt.ID, competing)
	require.Error(t, err)
	got, err := f.d.WorkflowRelease(f.tkt.ID, req)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.Len(t, f.h.readTask("tst-release.md").GraphState.Transitions, 1)
}

func TestWorkflowWaitChangedDefinitionOrWorkspace(t *testing.T) {
	for _, change := range []string{"configuration", "missing worktree", "changed branch"} {
		t.Run(change, func(t *testing.T) {
			h, d, _, _ := newWorkflowWaitTestFixture(t, "check")
			before := h.readTask("tst-wait.md")
			w := before.GraphState.Wait.Clone()
			switch change {
			case "configuration":
				next := *d.config()
				next.SystemPrompt += "changed"
				d.cfg.Store(&next)
			case "missing worktree":
				require.NoError(t, os.Rename(w.Worktree, w.Worktree+".removed"))
			case "changed branch":
				eventTestGit(t, w.Worktree, "checkout", "-b", "unexpected")
			}
			d.reconcileWorkflowWaits(context.Background())
			got := h.readTask("tst-wait.md")
			require.Equal(t, ticket.StatusPaused, got.Status)
			require.True(t, got.GraphState.Wait.Disarmed)
			require.NotEmpty(t, got.LastError)
			require.Equal(t, w.ID, got.GraphState.Wait.ID)
			require.Equal(t, w.Deadline, got.GraphState.Wait.Deadline)
			require.Equal(t, w.NextPoll, got.GraphState.Wait.NextPoll)
			require.Empty(t, d.workflowProbeJobs)
		})
	}
}

func TestWorkflowWaitBranchAndCleanupReservation(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprint(alias), func(t *testing.T) {
			h, d, _, _ := newWorkflowWaitTestFixture(t, "timer")
			waiting := h.readTask("tst-wait.md")
			w := waiting.GraphState.Wait
			path := h.writeTicket("tst-conflict.md", h.taskMD("tst-conflict", "todo", "one-stage"))
			other := h.readTask("tst-conflict.md")
			require.NoError(t, other.SetField("branch", waiting.Branch))
			if alias {
				repoAlias := filepath.Join(t.TempDir(), "repository")
				require.NoError(t, os.Symlink(h.repoDir, repoAlias))
				require.NoError(t, other.SetField("path", repoAlias))
			}
			saveWorkflowWaitTestTicket(t, path, other)
			d.tickets[other.ID] = newTicketState(other, path)
			d.runner = func(context.Context, RunnerParams) (process.Result, error) {
				t.Error("reserved branch launched another runner")
				return process.Result{}, nil
			}
			d.runTicket(context.Background(), other.ID)
			refused := h.readTask("tst-conflict.md")
			require.Equal(t, ticket.StatusPaused, refused.Status)
			require.Contains(t, refused.LastError, "in use")
			d.removeWorktreeAt(d.ticketLog(other.ID), h.repoDir, w.Worktree)
			d.cleanupWorktree(d.ticketLog(other.ID), refused)
			require.DirExists(t, w.Worktree)
			require.Equal(t, waiting.GraphState, h.readTask("tst-wait.md").GraphState)
		})
	}
}

func TestWorkflowWaitStoppedProbeRecovery(t *testing.T) {
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			h, _, clock, path := newWorkflowWaitTestFixture(t, "check")
			before := h.readTask("tst-wait.md")
			state := before.GraphState.Clone()
			state.Revision++
			state.Wait.Phase, state.Wait.PollID = "probing", "stopped-poll"
			require.NoError(t, before.SetField("graph_state", state))
			saveWorkflowWaitTestTicket(t, path, before)
			completed := clock.Now()
			w := state.Wait
			j := &workflowProbeJournal{TicketID: before.ID, ExecutionID: state.ExecutionID, WaitID: w.ID, PollID: w.PollID, Revision: state.Revision,
				Path: path, Repository: w.Repository, Branch: w.Branch, RepoPath: before.Path, Fingerprint: state.DefinitionFingerprint, Phase: "stopped", Active: state.Clone(),
				Result: &workflowProbeResult{ExitCode: exit, Stopped: true, CompletedAt: completed, Output: "durable output"}}
			require.NoError(t, saveWorkflowProbe(h.cfg, j))
			clock.Advance(time.Minute)
			second := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				t.Error("completed probe repeated")
				return workflowWaitTestResult(clock, 0), nil
			}))
			require.NoError(t, second.initialScan(h.tasksDir))
			second.recoverWorkflowProbes()
			got := h.readTask("tst-wait.md")
			require.Equal(t, 1, got.GraphState.Wait.PollCount)
			require.Equal(t, "durable output", got.GraphState.Wait.Output)
			if exit == 0 {
				require.Equal(t, "ready", got.GraphState.Wait.Phase)
				require.True(t, got.GraphState.Continuation)
			} else {
				require.Equal(t, completed.Add(w.Poll), got.GraphState.Wait.NextPoll)
			}
			require.NoFileExists(t, workflowProbePath(h.cfg, before.ID))
			require.Zero(t, second.sem.used)
		})
	}
}

func TestWorkflowWaitCancelledUncertainProbeReservation(t *testing.T) {
	h, first, clock, path := newWorkflowWaitTestFixture(t, "check")
	before := h.readTask("tst-wait.md")
	branchKey := before.GraphState.Wait.Repository + "\x00" + before.Branch
	first.runner = func(context.Context, RunnerParams) (process.Result, error) {
		require.NoError(t, first.MoveTicket(before.ID, string(ticket.StatusCancelled)))
		result := workflowWaitTestResult(clock, 0)
		result.Termination = process.TerminationUnknown
		return result, process.ErrTerminationUncertain
	}
	first.sem.used++
	require.False(t, first.runWorkflowProbe(context.Background(), &workflowProbeJob{snapshot: before, path: path}))
	cancelled := h.readTask("tst-wait.md")
	require.Nil(t, cancelled.GraphState.Wait)
	holder, ok := first.branchHolderLocked(branchKey)
	require.True(t, ok)
	require.Equal(t, before.ID, holder)
	require.True(t, first.workflowWorktreeReserved(before.Path, before.GraphState.Wait.Worktree))
	second := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock))
	require.NoError(t, second.initialScan(h.tasksDir))
	second.recoverWorkflowProbes()
	require.Equal(t, 1, second.sem.used)
	holder, ok = second.branchHolderLocked(branchKey)
	require.True(t, ok)
	require.Equal(t, before.ID, holder)
	require.NoError(t, second.WorkflowRecover(cancelled.ID, cancelled.GraphState.ExecutionID, cancelled.GraphState.Revision))
	require.Zero(t, second.sem.used)
	require.NoFileExists(t, workflowProbePath(h.cfg, cancelled.ID))
	require.Equal(t, ticket.StatusCancelled, h.readTask("tst-wait.md").Status)
}
