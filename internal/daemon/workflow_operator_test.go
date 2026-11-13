package daemon

import (
	"context"
	"os"
	"sync"
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
)

func TestWorkflowOperatorRestartIntegration(t *testing.T) {
	h := newHarness(t)
	h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: workflowTestGraph()}
	h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
	h.cfg.Agents["agent2"] = config.Agent{Binary: "claude"}
	h.cfg = loadWorkflowTestConfig(t, h.cfg)
	var approve atomic.Bool
	var spawns summarySpawns
	var mu sync.Mutex
	logs := make(map[string]bool)
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		mu.Lock()
		require.False(t, logs[p.LogFile], "restart must not overwrite an earlier execution log")
		logs[p.LogFile] = true
		mu.Unlock()
		inv, err := session.InvocationFromEnv(func(key string) string { return p.Env[key] })
		require.NoError(t, err)
		verdict := ""
		if inv.Stage == "review" {
			verdict = "changes"
			if approve.Load() {
				verdict = "approve"
			}
		}
		_, err = session.PublishResult(inv.ResultPath, session.Result{Outcome: "complete", Summary: "fake node finished", Verdict: verdict}, 2)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p.LogFile, []byte(inv.ID), 0o600))
		now := time.Now()
		return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, nil
	}), WithFinalSummarySpawner(spawns.spawner("workflow summary")))
	h.writeTicket("tst-restart.md", h.taskMD("tst-restart", "todo", "graph"))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-errCh) }()
	before := h.waitForStatus("tst-restart.md", ticket.StatusPaused, 15*time.Second)
	waitForAgentsDone(t, d, 5*time.Second)
	require.Len(t, before.History, 8)
	require.Equal(t, 3, before.GraphState.LoopCounts["revise"])
	for _, action := range []func() error{
		func() error { return d.RetryTicket(before.ID) },
		func() error { return d.MoveTicket(before.ID, "todo") },
		func() error { return d.SkipStage(before.ID) },
		func() error { return d.SetStage(before.ID, "implement") },
	} {
		require.ErrorIs(t, action(), web.ErrInvalidState)
	}
	require.NoError(t, d.MoveTicket(before.ID, "open"))
	require.ErrorIs(t, d.RunTicket(before.ID), web.ErrInvalidState)
	approve.Store(true)
	require.NoError(t, d.WorkflowRestart(before.ID))
	after := h.waitForStatus("tst-restart.md", ticket.StatusHumanReview, 10*time.Second)
	h.waitForFinalSummary("tst-restart.md", "workflow summary", 5*time.Second)
	require.Len(t, spawns.all(), 1)
	waitForAgentsDone(t, d, 5*time.Second)
	require.Len(t, after.History, 10)
	assert.Equal(t, before.History, after.History[:8])
	assert.NotEqual(t, before.GraphState.ExecutionID, after.GraphState.ExecutionID)
	assert.Empty(t, after.GraphState.LoopCounts)
	require.Len(t, after.GraphState.PreviousExecutions, 1)
	assert.Equal(t, before.GraphState.Transitions, after.GraphState.PreviousExecutions[0].Transitions)
	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, logs, 10)
	for path := range logs {
		require.FileExists(t, path)
	}
}

func TestWorkflowInterruptedCheckRecoveryIntegration(t *testing.T) {
	h := newHarness(t)
	h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{
		Version: 2, Entry: "test", Nodes: []config.WorkflowNode{
			{ID: "test", Type: "check", Command: "exit 0", Timeout: config.Duration{Duration: time.Minute}},
			{ID: "accept", Type: "end", Status: "human_review"},
			{ID: "stop", Type: "end", Status: "paused"},
		}, Edges: []config.WorkflowEdge{
			{From: config.WorkflowPort{Node: "test", Port: "pass"}, To: "accept"},
			{From: config.WorkflowPort{Node: "test", Port: "fail"}, To: "stop"},
		},
	}}
	h.cfg = loadWorkflowTestConfig(t, h.cfg)
	calls := 0
	d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		calls++
		require.Equal(t, "/bin/sh", p.Binary)
		now := time.Now()
		result := process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}
		if calls == 1 {
			result.Interruption = context.Canceled
		}
		return result, nil
	}))
	path := h.writeTicket("tst-recover.md", h.taskMD("tst-recover", "todo", "graph"))
	d.tickets["tst-recover"] = newTicketState(h.readTask("tst-recover.md"), path)
	d.runTicket(context.Background(), "tst-recover")
	before := h.readTask("tst-recover.md")
	require.Equal(t, ticket.StatusPaused, before.Status)
	require.NotEmpty(t, before.InvocationID)
	require.Empty(t, before.GraphState.Transitions)
	require.ErrorIs(t, d.RetryTicket(before.ID), web.ErrInvalidState)
	require.NoError(t, d.WorkflowRecover(before.ID, before.GraphState.ExecutionID, before.GraphState.Revision))
	require.NoError(t, d.RetryTicket(before.ID))
	d.runTicket(context.Background(), before.ID)
	after := h.readTask("tst-recover.md")
	require.Equal(t, ticket.StatusHumanReview, after.Status)
	require.Len(t, after.History, 2)
	assert.Equal(t, before.GraphState.ExecutionID, after.GraphState.ExecutionID)
	assert.Equal(t, before.GraphState.VisitID, after.History[1].NodeVisitID)
	assert.NotEqual(t, after.History[0].InvocationID, after.History[1].InvocationID)
	assert.Equal(t, "pass", after.GraphState.Transitions[0].Port)
	assert.Equal(t, 2, calls)
}
