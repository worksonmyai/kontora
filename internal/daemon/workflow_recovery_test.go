package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/hook"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func newWorkflowRecoveryFixture(t *testing.T) *resultFixture {
	t.Helper()
	f := newResultFixture(t)
	f.h.cfg.Pipelines[f.tkt.Pipeline] = config.PipelineDefinition{Graph: &config.WorkflowGraph{
		Version: 1, Entry: "step2", Nodes: []config.WorkflowNode{
			{ID: "step2", Type: "agent", Stage: "step1", Agent: "agent1", Verdicts: []string{"approve", "revise"}},
		},
	}}
	require.NoError(t, f.tkt.DeleteField("parent"))
	require.NoError(t, f.tkt.SetField("graph_state", &ticket.GraphState{
		Version: 1, ExecutionID: "workflow-result", Revision: 7, DefinitionFingerprint: "fingerprint-result",
		VisitID: "visit-4", Visit: 4, LoopCounts: map[string]int{"review-loop": 2},
	}))
	f.rec.EpicID = ""
	f.rec.GraphBefore = f.tkt.GraphState.Clone()
	f.rec.NodeKind, f.rec.Template = "agent", "step1"
	f.rec.Invocation.Version = session.GraphResultVersion
	f.rec.Invocation.Verdicts = []string{"approve", "revise"}
	f.rec.Invocation.WorkflowExecutionID = f.tkt.GraphState.ExecutionID
	f.rec.Invocation.NodeVisitID = f.tkt.GraphState.VisitID
	f.rec.Invocation.DefinitionFingerprint = f.tkt.GraphState.DefinitionFingerprint
	common, err := worktree.CommonDir(f.h.repoDir)
	require.NoError(t, err)
	f.rec.RepoCommonDir = common
	f.write(t)
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	f.d.tickets[f.tkt.ID] = newTicketState(f.tkt, f.path)
	return f
}

func TestWorkflowOrdinaryInvocationJournal(t *testing.T) {
	for _, kind := range []string{"agent", "check"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkflowRecoveryFixture(t)
			f.h.cfg.Pipelines[f.tkt.Pipeline].Graph.Nodes[0].Type = kind
			params := RunnerParams{}
			p := spawnAgentParams{cfg: f.h.cfg, ticketID: f.tkt.ID, filePath: f.path,
				stageName: f.tkt.Stage, agentName: "agent1", graphState: f.tkt.GraphState.Clone()}
			if kind == "check" {
				// A check never launches this agent, so its CLI mode is irrelevant.
				p.agentCfg = config.Agent{Binary: "pi", Args: []string{"--print"}}
			}
			rec, err := f.d.bindExecution(f.tkt, p, &params, "")
			require.NoError(t, err)
			require.NotNil(t, rec)
			assert.Empty(t, rec.EpicID, "journaling must not create supervision authority")
			scope, err := f.d.supervision(f.tkt, false)
			require.NoError(t, err)
			assert.Empty(t, scope)
			assert.Equal(t, "bound", rec.Phase)
			assert.Empty(t, rec.Worktree, "binding precedes worktree preparation")
			assert.Equal(t, kind, rec.NodeKind)
			assert.True(t, params.RequireTermination)
			assert.Equal(t, kind != "check", params.TerminalReported != nil)
			assert.Equal(t, f.tkt.InvocationID, rec.Invocation.ID)
			assert.Equal(t, f.tkt.GraphState, rec.GraphBefore)
			inv, err := session.InvocationFromEnv(func(key string) string { return params.Env[key] })
			require.NoError(t, err)
			assert.Equal(t, session.GraphResultVersion, inv.Version)
			assert.Equal(t, rec.Invocation, inv)
			loaded, err := loadExecution(f.h.cfg, f.tkt.ID)
			require.NoError(t, err)
			assert.Equal(t, rec, loaded)
			p.graphState.LoopCounts["review-loop"] = 99
			assert.Equal(t, 2, rec.GraphBefore.LoopCounts["review-loop"])
		})
	}
}

func TestWorkflowAgentValidationAndEnvironmentClearing(t *testing.T) {
	f := newWorkflowRecoveryFixture(t)
	params := RunnerParams{Env: f.rec.Invocation.Env()}
	p := spawnAgentParams{cfg: f.h.cfg, ticketID: f.tkt.ID, filePath: f.path, stageName: f.tkt.Stage,
		agentCfg: config.Agent{Binary: "pi", Args: []string{"--print"}}}
	_, err := f.d.bindExecution(f.tkt, p, &params, "")
	require.ErrorContains(t, err, "requires TUI mode")

	// Graph state alone does not turn a legacy definition into a graph run.
	f.h.cfg.Pipelines[f.tkt.Pipeline] = config.PipelineDefinition{Legacy: config.Pipeline{{Stage: "step2", Agent: "agent1"}}}
	rec, err := f.d.bindExecution(f.tkt, p, &params, "")
	require.NoError(t, err)
	assert.Nil(t, rec)
	for key := range f.rec.Invocation.Env() {
		assert.Empty(t, params.Env[key], key)
	}
}

func TestWorkflowResultClassification(t *testing.T) {
	for _, tc := range []struct {
		name, payload, failure, outcome, verdict string
		exit                                     int
	}{
		{name: "declared verdict", payload: `{"outcome":"complete","summary":"reviewed","verdict":"revise"}`, outcome: "complete", verdict: "revise"},
		{name: "missing verdict", payload: `{"outcome":"complete","summary":"reviewed"}`, outcome: "protocol_error"},
		{name: "undeclared verdict beats failure", payload: `{"outcome":"complete","summary":"reviewed","verdict":"other"}`, exit: 7, failure: "runner failed", outcome: "protocol_error"},
		{name: "blocked beats failure", payload: `{"outcome":"blocked","summary":"need credentials","next_step":"ask operator"}`, exit: 7, failure: "runner failed", outcome: "blocked"},
		{name: "blocked cannot smuggle verdict", payload: `{"outcome":"blocked","summary":"need credentials","next_step":"ask operator","verdict":"other"}`, outcome: "protocol_error"},
		{name: "complete cannot hide nonzero", payload: `{"outcome":"complete","summary":"reviewed","verdict":"approve"}`, exit: 7, outcome: "failed"},
		{name: "missing report", outcome: "protocol_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			if tc.payload != "" {
				require.NoError(t, os.WriteFile(path, []byte(tc.payload), 0o600))
			}
			rec := &executionRecord{NodeKind: "agent", Invocation: session.Invocation{
				Version: session.GraphResultVersion, ResultPath: path, Verdicts: []string{"approve", "revise"},
			}}
			run := agentRun{Result: process.Result{ExitCode: tc.exit, Termination: process.TerminationWindowGone}}
			classifyExecution(rec, &run, tc.failure)
			assert.Equal(t, tc.outcome, run.Outcome)
			assert.Equal(t, tc.verdict, run.Verdict)
			assert.Equal(t, tc.payload != "", run.Reported)
			assert.Equal(t, process.TerminationWindowGone, rec.RunnerTermination)
		})
	}
	t.Run("check has no report protocol", func(t *testing.T) {
		rec := &executionRecord{NodeKind: "check", Invocation: session.Invocation{Version: session.GraphResultVersion, ResultPath: t.TempDir()}}
		run := agentRun{Outcome: "complete", Result: process.Result{Termination: process.TerminationProcessGroupGone}}
		classifyExecution(rec, &run, "")
		assert.Same(t, rec, run.Execution)
		assert.Equal(t, "complete", run.Outcome)
		assert.False(t, run.Reported)
	})
}

func TestWorkflowRecoveryNeverRepeatsInterruptedEffects(t *testing.T) {
	for _, phase := range []string{"bound", "running", "finalizing", "recovery_required"} {
		t.Run(phase, func(t *testing.T) {
			f := newWorkflowRecoveryFixture(t)
			f.rec.Phase = phase
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			_, err := session.PublishResult(f.rec.Invocation.ResultPath, session.Result{
				Outcome: "complete", Summary: "done", Verdict: "approve",
			}, session.GraphResultVersion)
			require.NoError(t, err)
			got := f.reconcile(t)
			assert.Equal(t, ticket.StatusPaused, got.Status)
			assert.Equal(t, f.rec.Invocation.ID, got.InvocationID)
			assert.Equal(t, f.rec.GraphBefore, got.GraphState)
			assert.Empty(t, got.History, "a report is not a committed graph transition")
			assert.Contains(t, got.LastError, "inspect before Retry")
			rec, err := loadExecution(f.h.cfg, got.ID)
			require.NoError(t, err)
			assert.Equal(t, "recovery_required", rec.Phase)
			assert.Equal(t, ticket.StatusPaused, f.reconcile(t).Status)
			assert.Zero(t, f.d.sem.used, "confirmed termination releases capacity, not the execution identity")
		})
	}
}

func TestWorkflowRecoveryInterruptedBinding(t *testing.T) {
	f := newWorkflowRecoveryFixture(t)
	previous := f.tkt.InvocationID
	f.rec.PreviousInvocationID = &previous
	f.rec.Invocation.ID = "interrupted-binding"
	f.rec.Invocation.ResultPath = filepath.Join(f.h.logsDir, f.tkt.ID, "results", f.rec.Invocation.ID+".json")
	f.rec.Phase = "bound"
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	got := f.reconcile(t)
	assert.Equal(t, ticket.StatusPaused, got.Status)
	assert.Equal(t, f.rec.Invocation.ID, got.InvocationID)
	assert.Equal(t, f.rec.GraphBefore, got.GraphState)
	assert.Empty(t, got.History)
	rec, err := loadExecution(f.h.cfg, got.ID)
	require.NoError(t, err)
	assert.Equal(t, "recovery_required", rec.Phase)
	assert.Equal(t, got.InvocationID, rec.Invocation.ID)
}

func TestWorkflowHookRecoveryRetainsResourcesWithoutTmux(t *testing.T) {
	for _, phase := range []string{"bound", "running", "finalizing", "transition", "recovery_required"} {
		t.Run(phase, func(t *testing.T) {
			f := newWorkflowRecoveryFixture(t)
			f.rec.Phase, f.rec.HookTermination = phase, "pending"
			require.NoError(t, saveExecution(f.h.cfg, f.rec))
			f.d.windows.list = func(string) ([]string, error) {
				t.Fatal("absence of a tmux runner cannot prove hook termination")
				return nil, nil
			}
			for range 2 {
				got := f.reconcile(t)
				assert.Equal(t, ticket.StatusPaused, got.Status)
				assert.Equal(t, f.rec.Invocation.ID, got.InvocationID)
				assert.Contains(t, got.LastError, "hook termination is unconfirmed")
				assert.Equal(t, 1, f.d.sem.used)
				assert.Contains(t, f.d.recoveredReservations, got.ID)
				assert.Equal(t, got.ID, f.d.runningBranches[f.rec.RepoCommonDir+"\x00"+f.rec.Branch])
				rec, err := loadExecution(f.h.cfg, got.ID)
				require.NoError(t, err)
				assert.Equal(t, "pending", rec.HookTermination)
				assert.Equal(t, "recovery_required", rec.Phase)
			}
		})
	}
}

func TestWorkflowRecoveryPreservesOperatorState(t *testing.T) {
	for _, pendingHook := range []bool{false, true} {
		for _, change := range []string{"cancel", "open", "new revision", "new invocation", "foreign claim", "imported ownership"} {
			t.Run(fmt.Sprintf("%s/pending-hook-%t", change, pendingHook), func(t *testing.T) {
				f := newWorkflowRecoveryFixture(t)
				if pendingHook {
					f.rec.HookTermination = "pending"
				}
				require.NoError(t, saveExecution(f.h.cfg, f.rec))
				switch change {
				case "cancel":
					require.NoError(t, f.tkt.SetField("status", "cancelled"))
				case "open":
					require.NoError(t, f.tkt.SetField("status", "open"))
				case "new revision":
					state := f.tkt.GraphState.Clone()
					state.Revision++
					require.NoError(t, f.tkt.SetField("graph_state", state))
				case "new invocation":
					require.NoError(t, f.tkt.SetField("invocation_id", "new-invocation"))
					f.d.windows.list = func(string) ([]string, error) {
						t.Fatal("stale graph journal must not inspect a replacement invocation's window")
						return []string{f.tkt.ID}, nil
					}
				case "foreign claim":
					require.NoError(t, f.tkt.SetField("claimed_by", "other-instance"))
				case "imported ownership":
					require.NoError(t, f.tkt.SetField("managed", &ticket.ManagedOwnership{}))
				}
				f.write(t)
				before := f.tkt.Clone()
				var got *ticket.Ticket
				if change == "foreign claim" || change == "imported ownership" {
					require.NoError(t, os.Remove(filepath.Join(f.h.tasksDir, "epc-result.md")))
					f.d.windows.list = func(string) ([]string, error) {
						t.Fatal("foreign ownership does not authorize runner recovery")
						return nil, nil
					}
					ticketBefore, err := os.ReadFile(f.path)
					require.NoError(t, err)
					journalBefore, err := os.ReadFile(executionPath(f.h.cfg, f.tkt.ID))
					require.NoError(t, err)
					for range 2 {
						require.NoError(t, f.d.initialScan(f.h.tasksDir))
						assert.Equal(t, pendingHook, f.d.sem.used > 0)
						if pendingHook {
							assert.Equal(t, 1, f.d.sem.used)
							assert.Contains(t, f.d.recoveredReservations, f.tkt.ID)
							assert.Equal(t, f.tkt.ID, f.d.runningBranches[f.rec.RepoCommonDir+"\x00"+f.rec.Branch])
						} else {
							assert.NotContains(t, f.d.recoveredReservations, f.tkt.ID)
							assert.NotContains(t, f.d.runningBranches, f.rec.RepoCommonDir+"\x00"+f.rec.Branch)
						}
					}
					ticketAfter, err := os.ReadFile(f.path)
					require.NoError(t, err)
					assert.Equal(t, ticketBefore, ticketAfter)
					journalAfter, err := os.ReadFile(executionPath(f.h.cfg, f.tkt.ID))
					require.NoError(t, err)
					assert.Equal(t, journalBefore, journalAfter)
					got = f.h.readTask(resultTicketID + ".md")
				} else {
					got = f.reconcile(t)
				}
				assert.Equal(t, before.Status, got.Status)
				assert.Equal(t, before.InvocationID, got.InvocationID)
				assert.Equal(t, before.GraphState, got.GraphState)
				assert.Empty(t, got.History)
				assert.Equal(t, pendingHook || change == "new invocation", f.d.sem.used > 0)
				if change == "new invocation" {
					assert.Contains(t, f.d.recoveredReservations, got.ID)
					assert.Equal(t, got.ID, f.d.runningBranches[f.rec.RepoCommonDir+"\x00"+f.rec.Branch])
					rec, err := loadExecution(f.h.cfg, got.ID)
					require.NoError(t, err)
					assert.Equal(t, f.rec, rec)
					assert.Equal(t, got.InvocationID, f.reconcile(t).InvocationID)
				}
			})
		}
	}
}

func TestWorkflowMissingJournalPauses(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) {
			f := newWorkflowRecoveryFixture(t)
			require.NoError(t, os.Remove(executionPath(f.h.cfg, f.tkt.ID)))
			if !bound {
				require.NoError(t, f.tkt.SetField("invocation_id", ""))
				f.write(t)
			}
			got := f.reconcile(t)
			assert.Equal(t, ticket.StatusPaused, got.Status)
			assert.NotEmpty(t, got.InvocationID)
			assert.Contains(t, got.LastError, "missing journal")
			rec, err := loadExecution(f.h.cfg, got.ID)
			require.NoError(t, err)
			assert.Equal(t, "recovery_required", rec.Phase)
			assert.Equal(t, got.InvocationID, rec.Invocation.ID)
		})
	}
}

func TestWorkflowTransitionReplayUsesFrozenState(t *testing.T) {
	for _, alreadyCommitted := range []bool{false, true} {
		t.Run(fmt.Sprint(alreadyCommitted), func(t *testing.T) {
			f := newWorkflowRecoveryFixture(t)
			after := f.tkt.Clone()
			state := after.GraphState.Clone()
			state.Revision, state.Visit, state.VisitID = 10, 5, "visit-5"
			state.LoopCounts["review-loop"] = 3
			exit := 0
			state.LastResult = &ticket.GraphResult{NodeID: "step2", VisitID: f.rec.Invocation.NodeVisitID,
				InvocationID: f.rec.Invocation.ID, Outcome: "complete", Verdict: "revise", ExitCode: &exit}
			state.Transitions = append(state.Transitions, ticket.GraphTransition{
				From: "review-loop", Port: "again", To: "step2", LoopCount: 3, Revision: 10,
			})
			require.NoError(t, after.SetField("graph_state", state))
			require.NoError(t, after.SetField("status", "todo"))
			now := time.Date(2026, 1, 1, 1, 0, 1, 0, time.UTC)
			run := agentRun{Execution: f.rec, Outcome: "complete", Verdict: "revise",
				Result: process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationWindowGone}}
			appendRunHistory(after, run, "agent1", "step2", 1)
			require.NoError(t, persistTransition(f.h.cfg, f.rec, after))
			frozen := f.rec.Transition.GraphState.Clone()
			after.GraphState.LoopCounts["review-loop"] = 99
			assert.Equal(t, 3, f.rec.Transition.GraphState.LoopCounts["review-loop"], "the journal owns a deep clone")
			require.NoError(t, after.SetField("graph_state", frozen.Clone()))
			require.NoError(t, after.SetField("invocation_id", ""))
			if alreadyCommitted {
				f.tkt = after
				f.write(t)
				assert.True(t, committedExecutionMatches(after, f.rec))
				changed := after.Clone()
				changed.GraphState.LoopCounts["review-loop"]++
				assert.False(t, committedExecutionMatches(changed, f.rec), "all graph state, not just identity, must match")
			} else {
				require.NoError(t, os.Chmod(f.path, 0o444))
				assert.Equal(t, ticket.StatusInProgress, f.reconcile(t).Status)
				assert.Equal(t, 1, f.d.sem.used, "failed replay retains its reservation and journal")
				assert.FileExists(t, executionPath(f.h.cfg, f.tkt.ID))
				require.NoError(t, os.Chmod(f.path, 0o644))
			}
			got := f.reconcile(t)
			assert.Equal(t, ticket.StatusTodo, got.Status)
			assert.Equal(t, frozen, got.GraphState)
			assert.Empty(t, got.InvocationID)
			require.Len(t, got.History, 1)
			assert.Equal(t, f.rec.Invocation.WorkflowExecutionID, got.History[0].WorkflowExecutionID)
			assert.Equal(t, f.rec.Invocation.NodeVisitID, got.History[0].NodeVisitID)
			assert.Equal(t, f.rec.Invocation.DefinitionFingerprint, got.History[0].DefinitionFingerprint)
			assert.Equal(t, "step1", got.History[0].Template)
			assert.NoFileExists(t, executionPath(f.h.cfg, got.ID))
			assert.Zero(t, f.d.sem.used)
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			f.reconcile(t)
			data, err := os.ReadFile(f.path)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(data), "reconciliation must not evaluate the loop again")
		})
	}
}

func TestWorkflowExecutionIdentity(t *testing.T) {
	f := newWorkflowRecoveryFixture(t)
	assert.True(t, executionMatches(f.tkt, f.rec))
	for _, change := range []func(*ticket.GraphState){
		func(s *ticket.GraphState) { s.ExecutionID = "other-execution" },
		func(s *ticket.GraphState) { s.VisitID = "other-visit" },
		func(s *ticket.GraphState) { s.DefinitionFingerprint = "other-definition" },
		func(s *ticket.GraphState) { s.Revision++ },
	} {
		changed := f.tkt.Clone()
		change(changed.GraphState)
		assert.False(t, executionMatches(changed, f.rec))
		assert.False(t, executionLifecycleMatches(changed, f.rec))
	}
}

func TestWorkflowHookJournalAndWorktreeFailure(t *testing.T) {
	f := newWorkflowRecoveryFixture(t)
	f.rec.Phase = "bound"
	require.NoError(t, saveExecution(f.h.cfg, f.rec))
	f.h.cfg.Hooks = config.Hooks{config.HookWorktreeCreated: {{Name: "prepare", Run: fmt.Sprintf(
		`grep -q '"hook_termination":"pending"' %q && exit 7`, executionPath(f.h.cfg, f.tkt.ID)), OnFailure: "pause"}}}
	require.NoError(t, os.MkdirAll(f.h.wtDir, 0o755))
	marker := filepath.Join(f.h.wtDir, "keep-worktree")
	require.NoError(t, os.WriteFile(marker, []byte("do not remove"), 0o600))
	ctx := context.WithValue(context.Background(), ordinaryReservationKey{}, &atomic.Bool{})
	ctx = context.WithValue(ctx, workflowExecutionKey{}, f.rec)
	ok := f.d.runWorktreeCreatedHooks(ctx, f.h.cfg, f.d.log, f.tkt, f.path, hook.Context{
		TicketID: f.tkt.ID, TicketFile: f.path, RepoPath: f.h.repoDir, Worktree: f.h.wtDir,
	}, true)
	assert.False(t, ok)
	assert.FileExists(t, marker, "graph hook failure must not remove or recreate its worktree")
	assert.True(t, worktreeUnprepared(f.h.cfg, f.tkt.ID, f.h.wtDir))
	got := f.h.readTask(resultTicketID + ".md")
	assert.Equal(t, ticket.StatusPaused, got.Status)
	assert.Contains(t, got.LastError, "exited with code 7", "the hook must observe its pending journal before running")
	rec, err := loadExecution(f.h.cfg, f.tkt.ID)
	require.NoError(t, err)
	assert.Equal(t, "confirmed", rec.HookTermination, "a nonzero exit still confirms hook shutdown")
	assert.False(t, ordinaryReservationRetained(ctx))
}
