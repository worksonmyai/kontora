package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

const (
	conditionTestID      = "tst-condition"
	conditionTestFile    = conditionTestID + ".md"
	conditionTestCommand = `test -s "release notes.txt"`
	conditionTestTimeout = 17 * time.Second
)

func conditionTestProducerCheck() config.WorkflowNode {
	return config.WorkflowNode{ID: "produce", Type: "check", Command: "true", Timeout: config.Duration{Duration: time.Minute}}
}

func conditionTestNode(id string) config.WorkflowNode {
	return config.WorkflowNode{ID: id, Type: "condition", Command: conditionTestCommand,
		Timeout: config.Duration{Duration: conditionTestTimeout}}
}

func conditionTestEdge(from, port, to string) config.WorkflowEdge {
	return config.WorkflowEdge{From: config.WorkflowPort{Node: from, Port: port}, To: to}
}

func conditionTestGraph(producer config.WorkflowNode, successPort, failurePort string) *config.WorkflowGraph {
	return &config.WorkflowGraph{Version: 2, Entry: producer.ID, Nodes: []config.WorkflowNode{
		producer, conditionTestNode("decide"),
		{ID: "review", Type: "end", Status: "human_review"},
		{ID: "stop", Type: "end", Status: "paused"},
	}, Edges: []config.WorkflowEdge{
		conditionTestEdge(producer.ID, successPort, "decide"), conditionTestEdge(producer.ID, failurePort, "stop"),
		conditionTestEdge("decide", "true", "review"), conditionTestEdge("decide", "false", "stop"),
	}}
}

func TestWorkflowCommandConditionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		exit         int
		interruption error
		runnerErr    error
		termination  process.TerminationEvidence
		stage        string
		status       ticket.Status
		operatorStop bool
		port         string
		reason       string
	}{
		{name: "exit 0", stage: "review", status: ticket.StatusHumanReview, port: "true"},
		{name: "exit 1", exit: 1, stage: "stop", status: ticket.StatusPaused, port: "false"},
		{name: "exit 2 then retry", exit: 2, stage: "decide", status: ticket.StatusPaused, reason: "exited with code 2"},
		{name: "timeout", interruption: context.DeadlineExceeded, stage: "decide", status: ticket.StatusPaused, reason: "timed out"},
		{name: "runner failure", runnerErr: errors.New("runner failed"), stage: "decide", status: ticket.StatusPaused, reason: "infrastructure error"},
		{name: "runner did not start", termination: process.TerminationNotStarted, runnerErr: errors.New("prepare failed"), stage: "decide", status: ticket.StatusPaused, reason: "expected log path"},
		{name: "termination uncertain", runnerErr: process.ErrTerminationUncertain, stage: "decide", status: ticket.StatusPaused, reason: "termination is unconfirmed"},
		{name: "operator pause", stage: "decide", status: ticket.StatusPaused, operatorStop: true},
		{name: "operator cancel", stage: "decide", status: ticket.StatusCancelled, operatorStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Agents["override"] = config.Agent{Binary: "claude", Args: []string{"--agent-arg"}, Environment: map[string]string{"AGENT_ONLY": "override"}}
			h.cfg.Environment = map[string]string{"CONDITION_GLOBAL": "global", session.ResultFileEnvVar: filepath.Join(t.TempDir(), "stale-result.json")}
			t.Setenv(session.InvocationIDEnvVar, "inherited-invocation")
			hooks := filepath.Join(t.TempDir(), "hooks")
			h.cfg.Hooks = config.Hooks{
				config.HookWorktreeCreated: {{Run: fmt.Sprintf(`echo "created:$KONTORA_STAGE" >> %q`, hooks)}},
				config.HookStageStart:      {{Run: fmt.Sprintf(`echo "start:$KONTORA_STAGE" >> %q`, hooks)}},
				config.HookStageEnd:        {{Run: fmt.Sprintf(`echo "end:$KONTORA_STAGE" >> %q`, hooks)}},
			}
			graph := conditionTestGraph(config.WorkflowNode{ID: "produce", Type: "agent", Stage: "step1", Agent: "agent1", Verdicts: []string{"approved"}}, "complete", "failed")
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
			cfg := loadWorkflowTestConfig(t, h.cfg)
			clock := newWorkflowWaitTestClock()
			path := h.writeTicket(conditionTestFile, h.taskMD(conditionTestID, "todo", "graph"))
			initial := h.readTask(conditionTestFile)
			require.NoError(t, initial.SetField("agent", "override"))
			saveWorkflowWaitTestTicket(t, path, initial)

			type evaluation struct {
				params  RunnerParams
				journal *workflowConditionJournal
				stored  *ticket.Ticket
			}
			productions := make(chan RunnerParams, 4)
			evaluations := make(chan evaluation, 4)
			var attempts atomic.Int32
			var summaries summarySpawns
			d := h.newDaemon(cfg, WithFinalSummarySpawner(summaries.spawner("summary")), WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
				if p.Binary == "claude" {
					select {
					case productions <- p:
					case <-ctx.Done():
						return workflowWaitTestResult(clock, 0), ctx.Err()
					}
					return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, session.Result{Outcome: "complete", Summary: "notes ready", Verdict: "approved"})
				}
				result := workflowWaitTestResult(clock, tc.exit)
				if tc.termination != "" {
					result.Termination = tc.termination
				} else if err := p.OnWindowCreated(conditionTestWindow); err != nil {
					return result, err
				}
				j, err := loadWorkflowCondition(cfg, p.TicketID)
				if err != nil {
					return result, err
				}
				stored, err := ticket.ParseFile(path)
				if err != nil {
					return result, err
				}
				if _, err := os.Stat(executionPath(cfg, p.TicketID)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("condition must not have an invocation journal; stat error: %v", err)
				}
				select {
				case evaluations <- evaluation{params: p, journal: j, stored: stored}:
				case <-ctx.Done():
					return result, ctx.Err()
				}
				if result.Termination != process.TerminationNotStarted {
					if err := writeWorkflowWaitTestLog(p, "condition evaluation "+j.EvaluationID); err != nil {
						return result, err
					}
				}
				if attempts.Add(1) == 2 && tc.exit == 2 {
					result.ExitCode = 0
					return result, nil
				}
				if tc.operatorStop {
					<-ctx.Done()
					return result, ctx.Err()
				}
				result.Interruption = tc.interruption
				return result, tc.runnerErr
			}))
			startWorkflowWaitTestDaemon(t, d)
			if tc.operatorStop {
				require.Eventually(t, func() bool { return len(evaluations) == 1 }, 10*time.Second, 10*time.Millisecond)
				require.ErrorIs(t, d.DeleteTicket(conditionTestID), web.ErrInvalidState)
				require.FileExists(t, path)
				if tc.status == ticket.StatusPaused {
					require.NoError(t, d.PauseTicket(conditionTestID))
				} else {
					require.NoError(t, d.MoveTicket(conditionTestID, string(tc.status)))
				}
			}
			got := h.waitForStatus(conditionTestFile, tc.status, 10*time.Second)
			uncertain := errors.Is(tc.runnerErr, process.ErrTerminationUncertain)
			if uncertain {
				d.mu.Lock()
				_, retained := d.recoveredReservations[conditionTestID]
				used := d.sem.used
				d.mu.Unlock()
				require.True(t, retained)
				require.Equal(t, 1, used)
				require.ErrorIs(t, d.RetryTicket(conditionTestID), web.ErrInvalidState)
			} else {
				requireWorkflowWaitTestIdle(t, d)
			}
			require.Equal(t, tc.stage, got.Stage, got.LastError)
			require.Len(t, productions, 1)
			producer := <-productions
			require.Len(t, evaluations, 1)
			first := <-evaluations
			p, j := first.params, first.journal
			require.True(t, first.stored.IsRunning())
			require.Empty(t, first.stored.InvocationID)
			require.Empty(t, got.InvocationID)
			require.Len(t, got.History, 1)
			require.Equal(t, producer.Env[session.InvocationIDEnvVar], got.History[0].InvocationID)
			require.Equal(t, first.stored.History, got.History)
			require.Equal(t, first.stored.GraphState.LastResult, got.GraphState.LastResult)
			require.Equal(t, "/bin/sh", p.Binary)
			require.Equal(t, []string{"-c", conditionTestCommand}, p.Args)
			require.Equal(t, conditionTestTimeout, p.Timeout)
			require.True(t, p.RequireTermination)
			require.True(t, p.WaitForLog)
			require.False(t, p.Interactive)
			require.Empty(t, p.SessionID)
			if tc.status != ticket.StatusCancelled {
				require.DirExists(t, producer.Dir)
			}
			require.Equal(t, producer.Dir, p.Dir)
			require.Equal(t, "override", producer.Env["AGENT_ONLY"])
			require.NotContains(t, p.Env, "AGENT_ONLY")
			for k, v := range map[string]string{
				config.TicketEnvVar: conditionTestID, config.StageEnvVar: "decide", config.AgentEnvVar: "",
				"KONTORA_WORKTREE": producer.Dir, "KONTORA_REPO_PATH": h.repoDir, "KONTORA_BRANCH": got.Branch, "KONTORA_BASE_BRANCH": "main",
				session.WorkflowExecutionIDEnvVar: got.GraphState.ExecutionID, "CONDITION_GLOBAL": "global",
				"KONTORA_LAST_NODE": "produce", "KONTORA_LAST_OUTCOME": "complete", "KONTORA_LAST_EXIT_CODE": "0",
				"KONTORA_LAST_VERDICT": "approved", "KONTORA_LAST_LOG": producer.LogFile,
			} {
				require.Equal(t, v, p.Env[k], k)
			}
			for key := range (session.Invocation{}).Env() {
				if key != session.WorkflowExecutionIDEnvVar {
					require.Contains(t, p.Env, key, "must clear inherited invocation variables")
					require.Empty(t, p.Env[key], key)
				}
			}
			require.Equal(t, "launched", j.Phase)
			require.Equal(t, "decide", j.NodeID)
			require.Equal(t, got.GraphState.ExecutionID, j.ExecutionID)
			require.Equal(t, filepath.Join(cfg.LogsDir, conditionTestID, "conditions", "decide."+j.EvaluationID+".log"), p.LogFile)
			require.Equal(t, j.LogPath, p.LogFile)
			if tc.termination == process.TerminationNotStarted {
				require.NoFileExists(t, p.LogFile)
				require.Nil(t, j.Window)
				require.Contains(t, got.LastError, "See the daemon log for the runner error.")
			} else {
				log, err := os.ReadFile(p.LogFile)
				require.NoError(t, err)
				require.Equal(t, "condition evaluation "+j.EvaluationID, string(log))
				require.Equal(t, new(conditionTestWindow), j.Window)
			}
			if uncertain {
				retained, err := loadWorkflowCondition(cfg, conditionTestID)
				require.NoError(t, err)
				require.Equal(t, "recovery_required", retained.Phase)
			} else {
				require.NoFileExists(t, workflowConditionPath(cfg, conditionTestID))
			}
			if tc.port != "" {
				require.Empty(t, got.LastError)
				require.Len(t, got.GraphState.Transitions, 2)
				edge := got.GraphState.Transitions[1]
				require.Equal(t, tc.port, edge.Port)
				require.Equal(t, j.EvaluationID, edge.EvaluationID)
			} else {
				if tc.operatorStop {
					require.Empty(t, got.LastError)
				} else {
					require.Contains(t, got.LastError, tc.reason)
					require.Contains(t, got.LastError, p.LogFile)
				}
				require.Equal(t, first.stored.GraphState.Transitions, got.GraphState.Transitions)
			}
			if tc.exit == 2 {
				require.NoError(t, d.RetryTicket(conditionTestID))
				retried := h.waitForStatus(conditionTestFile, ticket.StatusHumanReview, 10*time.Second)
				requireWorkflowWaitTestIdle(t, d)
				require.Empty(t, productions, "retry must not rerun the producer")
				require.Len(t, evaluations, 1)
				second := <-evaluations
				require.Equal(t, p.Env, second.params.Env)
				require.Equal(t, j.ExecutionID, second.journal.ExecutionID)
				require.NotEqual(t, j.EvaluationID, second.journal.EvaluationID)
				require.NotEqual(t, p.LogFile, second.params.LogFile)
				require.FileExists(t, p.LogFile, "retry must retain the failed evaluation's log")
				require.FileExists(t, second.params.LogFile)
				require.Equal(t, got.History, retried.History)
				require.Equal(t, got.GraphState.LastResult, retried.GraphState.LastResult)
				require.Len(t, retried.GraphState.Transitions, 2)
				require.Equal(t, "true", retried.GraphState.Transitions[1].Port)
				require.Equal(t, second.journal.EvaluationID, retried.GraphState.Transitions[1].EvaluationID)
			}
			hookLog, err := os.ReadFile(hooks)
			require.NoError(t, err)
			require.Equal(t, "created:produce\nstart:produce\nend:produce\n", string(hookLog), "condition must not run hooks")
		})
	}
}

func TestWorkflowCommandConditionOccupiesSlot(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConcurrentAgents = 1
	graph := conditionTestGraph(conditionTestProducerCheck(), "pass", "fail")
	h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
	cfg := loadWorkflowTestConfig(t, h.cfg)
	clock := newWorkflowWaitTestClock()
	started, release := make(chan struct{}, 1), make(chan struct{})
	var summaries summarySpawns
	d := h.newDaemon(cfg, WithFinalSummarySpawner(summaries.spawner("summary")), WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
		result := workflowWaitTestResult(clock, 0)
		if p.Env[config.StageEnvVar] == "decide" {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
				return result, ctx.Err()
			}
		}
		return result, nil
	}))
	h.writeTicket(conditionTestFile, h.taskMD(conditionTestID, "todo", "graph"))
	startWorkflowWaitTestDaemon(t, d)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("condition did not start")
	}
	h.writeTicket("tst-other.md", h.taskMD("tst-other", "todo", "one-stage"))
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.sem.used == 1 && d.queued["tst-other"]
	}, 5*time.Second, 10*time.Millisecond, "the condition must hold the only slot while another ticket is queued")
	require.Equal(t, ticket.StatusTodo, h.readTask("tst-other.md").Status)
	close(release)
	h.waitForStatus(conditionTestFile, ticket.StatusHumanReview, 10*time.Second)
	h.waitForStatus("tst-other.md", ticket.StatusDone, 10*time.Second)
	requireWorkflowWaitTestIdle(t, d)
}

func TestWorkflowCommandConditionAfterWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		port string
		done bool
	}{
		{name: "release forwards receipt to agent", port: "released"},
		{name: "timeout resumes through condition", port: "timeout"},
		{name: "release to done cleans checkout and permits delete", port: "released", done: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.AutoPickUp = new(false)
			h.cfg.MaxConcurrentAgents = 1
			h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			h.cfg.Stages["step1"] = config.Stage{Prompt: `{{ with .Wake }}Wake reason: {{ .Reason }}{{ with .Receipt }}; release receipt: {{ .ID }}{{ end }}{{ end }}`}
			target, status := "consume", ticket.StatusHumanReview
			if tc.done {
				target, status = "done", ticket.StatusDone
			}
			graph := &config.WorkflowGraph{Version: 2, Entry: "produce", Nodes: []config.WorkflowNode{
				conditionTestProducerCheck(),
				{ID: "gate", Type: "wait", Mode: "human", Timeout: config.Duration{Duration: time.Hour}},
				conditionTestNode("after-release"), conditionTestNode("after-timeout"),
				{ID: "consume", Type: "agent", Stage: "step1", Agent: "agent1", Handoff: config.WorkflowHandoffPrevious},
				{ID: "review", Type: "end", Status: "human_review"}, {ID: "stop", Type: "end", Status: "paused"},
			}, Edges: []config.WorkflowEdge{
				conditionTestEdge("produce", "pass", "gate"), conditionTestEdge("produce", "fail", "stop"),
				conditionTestEdge("gate", "released", "after-release"), conditionTestEdge("gate", "timeout", "after-timeout"),
				conditionTestEdge("after-release", "true", target), conditionTestEdge("after-release", "false", "stop"),
				conditionTestEdge("after-timeout", "true", "consume"), conditionTestEdge("after-timeout", "false", "stop"),
				conditionTestEdge("consume", "complete", "review"), conditionTestEdge("consume", "failed", "stop"),
			}}
			if tc.done {
				graph.Nodes = append(graph.Nodes, config.WorkflowNode{ID: "done", Type: "end", Status: "done"})
			}
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
			cfg := loadWorkflowTestConfig(t, h.cfg)
			clock := newWorkflowWaitTestClock()
			conditions, agents := make(chan RunnerParams, 4), make(chan RunnerParams, 4)
			var summaries summarySpawns
			d := h.newDaemon(cfg, WithWorkflowWaitClock(clock), WithFinalSummarySpawner(summaries.spawner("summary")), WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
				result := workflowWaitTestResult(clock, 0)
				switch p.Env[config.StageEnvVar] {
				case "produce":
					return result, writeWorkflowWaitTestLog(p, "diagnostic from producer")
				case "after-release", "after-timeout":
					select {
					case conditions <- p:
					case <-ctx.Done():
						return result, ctx.Err()
					}
				case "consume":
					select {
					case agents <- p:
					case <-ctx.Done():
						return result, ctx.Err()
					}
					return result, publishWorkflowWaitTestResult(p, session.Result{Outcome: "complete", Summary: "consumed"})
				default:
					return result, fmt.Errorf("unexpected runner: %v", p.Args)
				}
				return result, nil
			}))
			path := h.writeTicket(conditionTestFile, h.taskMD(conditionTestID, "open", "graph"))
			startWorkflowWaitTestDaemon(t, d)
			require.Eventually(t, func() bool { _, err := d.GetTicket(conditionTestID); return err == nil }, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, d.RunTicket(conditionTestID))
			parked := awaitWorkflowWaitTestTicket(t, h, conditionTestID, func(tkt *ticket.Ticket) bool {
				return tkt.Status == ticket.StatusPaused && tkt.GraphState != nil && tkt.GraphState.Wait != nil
			})
			requireWorkflowWaitTestIdle(t, d)
			require.Empty(t, conditions, "condition must not run before the wait resolves")
			require.Empty(t, agents)
			w := parked.GraphState.Wait
			require.DirExists(t, w.Worktree)
			conditionNode := "after-timeout"
			var receipt *ticket.WorkflowReceipt
			if tc.port == "released" {
				conditionNode = "after-release"
				var err error
				receipt, err = d.WorkflowRelease(conditionTestID, eventHumanRequest(&resultFixture{tkt: parked}))
				require.NoError(t, err)
				require.NotEmpty(t, receipt.ID)
			} else {
				clock.Advance(time.Hour)
				d.signalWorkflowWait()
			}
			got := h.waitForStatus(conditionTestFile, status, 10*time.Second)
			requireWorkflowWaitTestIdle(t, d)
			require.Nil(t, got.GraphState.Wait)
			require.Len(t, got.GraphState.WaitHistory, 1)
			require.Equal(t, w.ID, got.GraphState.WaitHistory[0].ID)
			require.Equal(t, receipt, got.GraphState.WaitHistory[0].Receipt)
			require.Len(t, conditions, 1)
			condition := <-conditions
			require.Equal(t, w.Worktree, condition.Dir)
			require.Equal(t, "produce", condition.Env["KONTORA_LAST_NODE"])
			require.GreaterOrEqual(t, len(got.GraphState.Transitions), 3)
			waitEdge, conditionEdge := got.GraphState.Transitions[1], got.GraphState.Transitions[2]
			require.Equal(t, "gate", waitEdge.From)
			require.Equal(t, tc.port, waitEdge.Port)
			require.Equal(t, conditionNode, waitEdge.To)
			require.Equal(t, conditionNode, condition.Env[config.StageEnvVar])
			require.Equal(t, conditionNode, conditionEdge.From)
			require.Equal(t, "true", conditionEdge.Port)
			require.Equal(t, target, conditionEdge.To)
			require.NotEmpty(t, conditionEdge.EvaluationID)
			if tc.done {
				require.Empty(t, agents)
				require.Equal(t, parked.History, got.History)
				require.False(t, got.CompletedAt.IsZero())
				require.Eventually(t, func() bool { _, err := os.Stat(w.Worktree); return errors.Is(err, os.ErrNotExist) }, 5*time.Second, 10*time.Millisecond)
				require.NoError(t, d.DeleteTicket(conditionTestID))
				require.NoFileExists(t, path)
				return
			}
			require.Len(t, agents, 1)
			prompt := renderedPrompt(<-agents)
			require.Contains(t, prompt, "diagnostic from producer")
			require.Len(t, got.History, 2)
			wake := got.History[1].Wake
			require.NotNil(t, wake)
			require.Equal(t, w.ID, wake.WaitID)
			require.Equal(t, receipt, wake.Receipt)
			if receipt == nil {
				require.Equal(t, "deadline", wake.Reason)
				require.Contains(t, prompt, "Wake reason: deadline")
				require.NotContains(t, prompt, "release receipt:")
			} else {
				require.Equal(t, "released", wake.Reason)
				require.Contains(t, prompt, "release receipt: "+receipt.ID)
			}
		})
	}
}
