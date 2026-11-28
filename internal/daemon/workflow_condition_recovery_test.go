package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func newConditionRecoveryFixture(t *testing.T) (*resultFixture, *workflowConditionJournal) {
	t.Helper()
	f := newWorkflowRecoveryFixture(t)
	cfg := f.h.cfg
	require.NoError(t, os.Remove(executionPath(cfg, f.tkt.ID)))
	require.NoError(t, os.Remove(filepath.Join(f.h.tasksDir, "epc-result.md")))
	edge := func(from, port, to string) config.WorkflowEdge {
		return config.WorkflowEdge{From: config.WorkflowPort{Node: from, Port: port}, To: to}
	}
	graph := &config.WorkflowGraph{
		Version: 2, Entry: "producer",
		Nodes: []config.WorkflowNode{
			{ID: "producer", Type: "check", Command: "exit 0"},
			{ID: "decide", Type: "condition", Command: "test -f ready", Timeout: config.Duration{Duration: time.Minute}},
			{ID: "yes", Type: "end", Status: "human_review"},
			{ID: "no", Type: "end", Status: "paused"},
		},
		Edges: []config.WorkflowEdge{edge("producer", "pass", "decide"), edge("producer", "fail", "no"), edge("decide", "true", "yes"), edge("decide", "false", "no")},
	}
	cfg.Pipelines[f.tkt.Pipeline] = config.PipelineDefinition{Graph: graph}
	fingerprint, err := workflowFingerprint(cfg, f.tkt, graph)
	require.NoError(t, err)
	s := &ticket.GraphState{
		Version: 1, ExecutionID: "condition-run", Revision: 3, DefinitionFingerprint: fingerprint,
		Visit: 1, VisitID: "condition-run.1",
		LastResult:  &ticket.GraphResult{NodeID: "producer", VisitID: "condition-run.1", InvocationID: f.tkt.InvocationID, Outcome: "complete", ExitCode: new(0)},
		Transitions: []ticket.GraphTransition{{From: "producer", Port: "pass", To: "decide", Revision: 2}},
	}
	require.NoError(t, f.tkt.SetField("graph_state", s))
	require.NoError(t, f.tkt.SetField("history", []ticket.HistoryEntry{{Stage: "producer", InvocationID: f.tkt.InvocationID, Summary: "saved evidence"}}))
	require.NoError(t, f.tkt.SetField("stage", "decide"))
	require.NoError(t, f.tkt.SetField("invocation_id", ""))
	f.write(t)
	f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	f.d.runner = rejectConditionRecoveryRunner(t)
	f.d.windows.list = func(string) ([]string, error) {
		t.Fatal("condition recovery must not use a session's window names")
		return nil, nil
	}
	f.d.windows.inspectIdentity = func(tmux.WindowIdentity) (bool, error) {
		t.Fatal("recovery must not inspect a runner window in this state")
		return false, nil
	}
	j := &workflowConditionJournal{
		TicketID: f.tkt.ID, ExecutionID: s.ExecutionID, NodeID: f.tkt.Stage, EvaluationID: "evaluation-1",
		Revision: s.Revision, Fingerprint: fingerprint, Path: f.path, RepoPath: f.tkt.Path, Branch: f.tkt.Branch,
		Worktree: f.h.wtDir, Instance: f.d.instanceName, Phase: "launched", Window: new(conditionTestWindow),
		LogPath: filepath.Join(f.h.logsDir, f.tkt.ID, "conditions", "decide.evaluation-1.log"),
	}
	return f, j
}

var conditionTestWindow = tmux.WindowIdentity{SocketPath: "/tmp/condition-tmux", ServerPID: 123, ServerStartedAt: 456, SessionID: "$1", WindowID: "@2"}

func rejectConditionRecoveryRunner(t *testing.T) RunnerFunc {
	t.Helper()
	return func(context.Context, RunnerParams) (process.Result, error) {
		t.Fatal("journal reconciliation must not launch a runner")
		return process.Result{}, nil
	}
}

func requireConditionRecoveryReservation(t *testing.T, f *resultFixture) {
	t.Helper()
	require.Equal(t, 1, f.d.sem.used)
	require.Contains(t, f.d.recoveredReservations, f.tkt.ID)
	require.Equal(t, f.tkt.ID, f.d.runningBranches[f.rec.RepoCommonDir+"\x00"+f.tkt.Branch])
	require.Empty(t, f.d.queuedIDs())
}

func TestWorkflowConditionRecoveryRunningWithoutJournal(t *testing.T) {
	f, _ := newConditionRecoveryFixture(t)
	before := f.tkt.Clone()
	require.NoError(t, f.d.initialScan(f.h.tasksDir))
	got := f.h.readTask(resultTicketID + ".md")
	require.Equal(t, ticket.StatusTodo, got.Status)
	require.Equal(t, before.Stage, got.Stage)
	require.Equal(t, before.GraphState, got.GraphState)
	require.Equal(t, before.History, got.History)
	require.Empty(t, got.InvocationID)
	require.Equal(t, []string{got.ID}, f.d.queuedIDs())
	require.Zero(t, f.d.sem.used)
	require.NoFileExists(t, executionPath(f.h.cfg, got.ID))
}

func TestWorkflowConditionRecoveryUnreadableOrForeignJournal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		foreign bool
	}{{"unreadable", false}, {"other instance", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f, j := newConditionRecoveryFixture(t)
			path := workflowConditionPath(f.h.cfg, j.TicketID)
			if tc.foreign {
				j.Instance = "other-instance"
				require.NoError(t, saveWorkflowCondition(f.h.cfg, j))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(`{"phase":`), 0o600))
			}
			originalJournal, err := os.ReadFile(path)
			require.NoError(t, err)
			before := f.tkt.Clone()
			for range 2 {
				got := f.reconcile(t)
				if tc.foreign {
					require.Equal(t, before.Status, got.Status)
					require.Equal(t, before.LastError, got.LastError)
				} else {
					require.Equal(t, ticket.StatusPaused, got.Status)
					require.Contains(t, got.LastError, "workflow-recover cannot acknowledge an unreadable journal")
				}
				require.Equal(t, before.GraphState, got.GraphState)
				require.Equal(t, before.History, got.History)
				requireConditionRecoveryReservation(t, f)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, originalJournal, data)
			}
		})
	}
}

func TestWorkflowConditionRecoveryStaleJournal(t *testing.T) {
	for _, phase := range []string{"launched", "stopped"} {
		t.Run(phase, func(t *testing.T) {
			f, j := newConditionRecoveryFixture(t)
			j.Phase = phase
			inspections := 0
			if phase == "stopped" {
				j.Result = &pipeline.GraphConditionResult{NodeID: j.NodeID, EvaluationID: j.EvaluationID}
			} else {
				f.d.windows.inspectIdentity = func(id tmux.WindowIdentity) (bool, error) {
					require.Equal(t, conditionTestWindow, id)
					inspections++
					return false, nil
				}
			}
			require.NoError(t, saveWorkflowCondition(f.h.cfg, j))
			state := f.tkt.GraphState.Clone()
			state.Revision++
			require.NoError(t, f.tkt.SetField("graph_state", state))
			f.write(t)
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			f.reconcile(t)
			after, err := os.ReadFile(f.path)
			require.NoError(t, err)
			require.Equal(t, before, after, "a stale journal cannot change the newer cursor")
			if phase == "launched" {
				require.Equal(t, 1, inspections, "even a stale launch needs termination confirmation")
			}
			require.NoFileExists(t, workflowConditionPath(f.h.cfg, j.TicketID))
			require.Empty(t, f.d.queuedIDs())
			require.Zero(t, f.d.sem.used)
		})
	}
}

func TestWorkflowConditionRecoveryLaunchedJournal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed bool
	}{{"confirmed", true}, {"unconfirmed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f, j := newConditionRecoveryFixture(t)
			require.NoError(t, saveWorkflowCondition(f.h.cfg, j))
			killed := false
			f.d.tmuxSession = "new-session"
			f.d.windows.inspectIdentity = func(id tmux.WindowIdentity) (bool, error) {
				require.Equal(t, *j.Window, id)
				return !killed || !tc.confirmed, nil
			}
			f.d.windows.killIdentity = func(id tmux.WindowIdentity) error {
				require.Equal(t, *j.Window, id)
				require.False(t, killed)
				killed = true
				return nil
			}
			before := f.tkt.Clone()
			got := f.reconcile(t)
			require.True(t, killed)
			require.Equal(t, before.Stage, got.Stage)
			require.Equal(t, before.GraphState, got.GraphState)
			require.Equal(t, before.History, got.History)
			require.Empty(t, got.InvocationID)
			if tc.confirmed {
				require.Equal(t, ticket.StatusTodo, got.Status)
				require.Equal(t, []string{got.ID}, f.d.queuedIDs())
				require.NoFileExists(t, workflowConditionPath(f.h.cfg, got.ID))
				require.Zero(t, f.d.sem.used)
				return
			}
			require.Equal(t, ticket.StatusPaused, got.Status)
			require.Contains(t, got.LastError, "termination is unconfirmed")
			require.Contains(t, got.LastError, j.LogPath)
			stored, err := loadWorkflowCondition(f.h.cfg, got.ID)
			require.NoError(t, err)
			j.Phase = "recovery_required"
			require.Equal(t, j, stored)
			requireConditionRecoveryReservation(t, f)
			require.Equal(t, got.GraphState, f.reconcile(t).GraphState)
			requireConditionRecoveryReservation(t, f)
		})
	}
}

func TestWorkflowConditionRecoveryUnreadableTicket(t *testing.T) {
	f, j := newConditionRecoveryFixture(t)
	require.NoError(t, saveWorkflowCondition(f.h.cfg, j))
	require.NoError(t, os.WriteFile(f.path, []byte("---\nid: [\n---\n"), 0o600))
	require.NoError(t, f.d.initialScan(f.h.tasksDir))
	requireConditionRecoveryReservation(t, f)
	require.FileExists(t, workflowConditionPath(f.h.cfg, j.TicketID))
}

func TestWorkflowConditionRecoveryStoppedReplay(t *testing.T) {
	f, j := newConditionRecoveryFixture(t)
	before := f.tkt.Clone()
	j.Phase = "stopped"
	j.Result = &pipeline.GraphConditionResult{NodeID: j.NodeID, EvaluationID: j.EvaluationID, ExitCode: 1}
	require.NoError(t, saveWorkflowCondition(f.h.cfg, j))
	got := f.reconcile(t)
	require.Equal(t, ticket.StatusPaused, got.Status)
	require.Equal(t, "no", got.Stage)
	expected := before.GraphState.Clone()
	expected.Revision++
	expected.Transitions = append(expected.Transitions, ticket.GraphTransition{From: j.NodeID, Port: "false", To: "no", Revision: expected.Revision, EvaluationID: j.EvaluationID})
	require.Equal(t, expected, got.GraphState)
	require.Equal(t, before.History, got.History)
	require.Empty(t, got.InvocationID)
	require.NoFileExists(t, workflowConditionPath(f.h.cfg, got.ID))
	require.Zero(t, f.d.sem.used)

	// Model a crash after committing the ticket but before unlinking the journal.
	require.NoError(t, saveWorkflowCondition(f.h.cfg, j))
	committed, err := os.ReadFile(f.path)
	require.NoError(t, err)
	f.reconcile(t)
	data, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.Equal(t, committed, data)
	require.NoFileExists(t, workflowConditionPath(f.h.cfg, got.ID))
	require.Empty(t, f.d.queuedIDs())
}

func TestWorkflowConditionRecoveryRequiresAcknowledgement(t *testing.T) {
	f, j := newConditionRecoveryFixture(t)
	j.Phase = "recovery_required"
	j.Result = &pipeline.GraphConditionResult{NodeID: j.NodeID, EvaluationID: j.EvaluationID, TerminationUncertain: true}
	require.NoError(t, saveWorkflowCondition(f.h.cfg, j))
	for range 2 {
		require.NoError(t, f.d.initialScan(f.h.tasksDir))
		requireConditionRecoveryReservation(t, f)
	}
	paused := f.h.readTask(resultTicketID + ".md")
	require.Equal(t, ticket.StatusPaused, paused.Status)
	require.Equal(t, f.tkt.GraphState, paused.GraphState)
	require.Contains(t, paused.LastError, "workflow-recover")
	before, err := os.ReadFile(f.path)
	require.NoError(t, err)
	err = f.d.RetryTicket(paused.ID)
	require.ErrorIs(t, err, web.ErrInvalidState)
	require.ErrorContains(t, err, "cannot retry while a workflow condition journal exists")
	require.ErrorContains(t, f.d.DeleteTicket(paused.ID), "cannot delete ticket")
	pickup := paused.Clone()
	require.NoError(t, pickup.SetField("status", "todo"))
	err = f.d.initializeWorkflow(f.h.cfg, pickup, f.h.cfg.Pipelines[pickup.Pipeline].Graph)
	require.ErrorIs(t, err, errWorkflowNoSpawn)
	require.Equal(t, ticket.StatusPaused, pickup.Status)
	require.Contains(t, pickup.LastError, "resolve the workflow condition journal")
	require.Equal(t, paused.GraphState, pickup.GraphState)
	require.ErrorIs(t, f.d.WorkflowRecover(paused.ID, j.ExecutionID, j.Revision-1), web.ErrInvalidState)
	requireConditionRecoveryReservation(t, f)
	after, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	retained, err := loadWorkflowCondition(f.h.cfg, paused.ID)
	require.NoError(t, err)
	require.Equal(t, j, retained)

	require.NoError(t, f.d.WorkflowRecover(paused.ID, j.ExecutionID, j.Revision))
	ack := f.h.readTask(resultTicketID + ".md")
	expected := paused.GraphState.Clone()
	expected.Revision++
	require.Equal(t, expected, ack.GraphState)
	require.Equal(t, paused.History, ack.History)
	require.Equal(t, j.NodeID, ack.Stage)
	require.Equal(t, ticket.StatusPaused, ack.Status, "acknowledgement must not start another evaluation")
	require.Empty(t, ack.InvocationID)
	require.NoFileExists(t, workflowConditionPath(f.h.cfg, ack.ID))
	require.Zero(t, f.d.sem.used)
	require.NotContains(t, f.d.recoveredReservations, ack.ID)
	require.NotContains(t, f.d.running, ack.ID)
	require.Empty(t, f.d.runningBranches)
	require.Empty(t, f.d.queuedIDs())

	require.NoError(t, f.d.RetryTicket(ack.ID))
	retried := f.h.readTask(resultTicketID + ".md")
	expected.Revision++
	require.Equal(t, expected, retried.GraphState)
	require.Equal(t, ticket.StatusTodo, retried.Status)
	require.Equal(t, []string{ack.ID}, f.d.queuedIDs())
}

func TestWorkflowConditionRecoveryAcknowledgementReplay(t *testing.T) {
	for _, name := range []string{"stale journal", "before writing ticket", "after writing ticket", "journal unlink fails"} {
		t.Run(name, func(t *testing.T) {
			f, j := newConditionRecoveryFixture(t)
			require.NoError(t, f.tkt.SetField("status", string(ticket.StatusPaused)))
			state := f.tkt.GraphState.Clone()
			switch name {
			case "stale journal":
				j.Phase = "recovery_required"
				state.ExecutionID = "newer-execution"
				require.NoError(t, f.tkt.SetField("last_error", "newer execution paused for review"))
			case "before writing ticket", "journal unlink fails":
				j.Phase = "acknowledged"
			case "after writing ticket":
				j.Phase = "acknowledged"
				state.Revision++
				require.NoError(t, f.tkt.SetField("last_error", "Condition recovery acknowledged. No work was started."))
			}
			require.NoError(t, f.tkt.SetField("graph_state", state))
			f.write(t)
			require.NoError(t, saveWorkflowCondition(f.h.cfg, j))
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			journalDir := filepath.Dir(workflowConditionPath(f.h.cfg, j.TicketID))
			if name == "journal unlink fails" {
				require.NoError(t, os.Chmod(journalDir, 0o500))
				t.Cleanup(func() { require.NoError(t, os.Chmod(journalDir, 0o700)) })
			}
			require.NoError(t, f.d.initialScan(f.h.tasksDir))
			if name == "journal unlink fails" {
				requireConditionRecoveryReservation(t, f)
				require.FileExists(t, workflowConditionPath(f.h.cfg, j.TicketID))
				require.NoError(t, os.Chmod(journalDir, 0o700))
				f.d.reconcileConditionFinalizations()
			}
			if name == "stale journal" {
				requireConditionRecoveryReservation(t, f)
				require.NoError(t, f.d.WorkflowRecover(j.TicketID, state.ExecutionID, state.Revision))
			}
			if name == "before writing ticket" || name == "journal unlink fails" {
				got := f.h.readTask(resultTicketID + ".md")
				require.Equal(t, state.Revision+1, got.GraphState.Revision)
				require.Equal(t, state.LastResult, got.GraphState.LastResult)
				require.Contains(t, got.LastError, "Condition recovery acknowledged")
			} else {
				after, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			require.NoFileExists(t, workflowConditionPath(f.h.cfg, j.TicketID))
			require.Zero(t, f.d.sem.used)
			require.Empty(t, f.d.queuedIDs())
		})
	}
}

func TestWorkflowConditionRecoveryShutdown(t *testing.T) {
	for _, definitionChanged := range []bool{false, true} {
		name := "unchanged definition"
		if definitionChanged {
			name = "removed definition"
		}
		t.Run(name, func(t *testing.T) {
			f, _ := newConditionRecoveryFixture(t)
			_, _, err := f.d.worktrees.Create(worktree.CreateOpts{RepoPath: f.tkt.Path, RepoName: f.h.repoName, TaskID: f.tkt.ID, Branch: f.tkt.Branch})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.d.runner = func(task context.Context, p RunnerParams) (process.Result, error) {
				require.NoError(t, p.OnWindowCreated(conditionTestWindow))
				cancel()
				<-task.Done()
				return process.Result{ExitCode: -1, Termination: process.TerminationWindowGone}, task.Err()
			}
			node, ok := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph.Node(f.tkt.Stage)
			require.True(t, ok)
			f.d.runGraphCondition(ctx, ctx, f.h.cfg, f.d.log, f.tkt, f.path, node)
			j, err := loadWorkflowCondition(f.h.cfg, f.tkt.ID)
			require.NoError(t, err)
			require.Equal(t, "launched", j.Phase)
			require.Equal(t, new(conditionTestWindow), j.Window)
			require.Equal(t, f.tkt.GraphState, f.h.readTask(resultTicketID+".md").GraphState)

			if definitionChanged {
				delete(f.h.cfg.Pipelines, f.tkt.Pipeline)
			}
			restarted := f.h.newDaemon(f.h.cfg, WithRunner(rejectConditionRecoveryRunner(t)))
			restarted.windows.inspectIdentity = func(id tmux.WindowIdentity) (bool, error) {
				require.Equal(t, conditionTestWindow, id)
				return false, nil
			}
			require.NoError(t, restarted.initialScan(f.h.tasksDir))
			got := f.h.readTask(resultTicketID + ".md")
			require.Equal(t, f.tkt.GraphState, got.GraphState)
			if definitionChanged {
				require.Equal(t, ticket.StatusPaused, got.Status)
				require.Contains(t, got.LastError, "workflow definition changed")
				require.Empty(t, restarted.queuedIDs())
			} else {
				require.Equal(t, ticket.StatusTodo, got.Status)
				require.Equal(t, []string{got.ID}, restarted.queuedIDs())
			}
			require.NoFileExists(t, workflowConditionPath(f.h.cfg, got.ID))
			require.Zero(t, restarted.sem.used)
		})
	}
}

func TestWorkflowConditionRecoveryRetriesFailedTicketWrite(t *testing.T) {
	f, _ := newConditionRecoveryFixture(t)
	wt, _, err := f.d.worktrees.Create(worktree.CreateOpts{RepoPath: f.tkt.Path, RepoName: f.h.repoName, TaskID: f.tkt.ID, Branch: f.tkt.Branch})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	f.d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls++
		require.Equal(t, wt, p.Dir)
		launched, err := loadWorkflowCondition(f.h.cfg, f.tkt.ID)
		require.NoError(t, err)
		require.Equal(t, "launched", launched.Phase)
		require.Equal(t, p.LogFile, launched.LogPath)
		// Fail the ticket write after the runner returns; the stopped result must survive.
		require.NoError(t, os.Chmod(f.path, 0o444))
		t.Cleanup(func() { require.NoError(t, os.Chmod(f.path, 0o644)) })
		return process.Result{Termination: process.TerminationProcessGroupGone}, nil
	}
	node, ok := f.h.cfg.Pipelines[f.tkt.Pipeline].Graph.Node(f.tkt.Stage)
	require.True(t, ok)
	f.d.runGraphCondition(ctx, ctx, f.h.cfg, f.d.log, f.tkt, f.path, node)
	cancel()
	before := f.h.readTask(resultTicketID + ".md")
	require.True(t, before.IsRunning())
	stopped, err := loadWorkflowCondition(f.h.cfg, before.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", stopped.Phase)
	require.NotNil(t, stopped.Result)
	require.NoError(t, os.Chmod(f.path, 0o644))

	requireConditionRecoveryReservation(t, f)
	f.d.reconcileConditionFinalizations()
	got := f.h.readTask(resultTicketID + ".md")
	require.Equal(t, 1, calls)
	require.Equal(t, ticket.StatusHumanReview, got.Status)
	require.Equal(t, "yes", got.Stage)
	require.Equal(t, before.History, got.History)
	require.Equal(t, before.GraphState.LastResult, got.GraphState.LastResult)
	require.Len(t, got.GraphState.Transitions, len(before.GraphState.Transitions)+1)
	require.Equal(t, stopped.EvaluationID, got.GraphState.Transitions[len(before.GraphState.Transitions)].EvaluationID)
	require.NoFileExists(t, workflowConditionPath(f.h.cfg, got.ID))
	require.Zero(t, f.d.sem.used)
}
