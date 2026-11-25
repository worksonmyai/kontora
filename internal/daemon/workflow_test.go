package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"gopkg.in/yaml.v3"
)

func workflowTestGraph() *config.WorkflowGraph {
	return &config.WorkflowGraph{Version: 2, Entry: "implement", Nodes: []config.WorkflowNode{
		{ID: "implement", Type: "agent", Stage: "step1", Agent: "agent1"},
		{ID: "review", Type: "agent", Stage: "step1", Agent: "agent2", Verdicts: []string{"approve", "changes"}},
		{ID: "approved", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approve"}},
		{ID: "revise", Type: "loop", Max: 3},
		{ID: "accept", Type: "end", Status: "human_review"},
		{ID: "stop", Type: "end", Status: "paused"},
	}, Edges: []config.WorkflowEdge{
		{From: config.WorkflowPort{Node: "implement", Port: "complete"}, To: "review"},
		{From: config.WorkflowPort{Node: "implement", Port: "failed"}, To: "stop"},
		{From: config.WorkflowPort{Node: "review", Port: "complete"}, To: "approved"},
		{From: config.WorkflowPort{Node: "review", Port: "failed"}, To: "stop"},
		{From: config.WorkflowPort{Node: "approved", Port: "true"}, To: "accept"},
		{From: config.WorkflowPort{Node: "approved", Port: "false"}, To: "revise"},
		{From: config.WorkflowPort{Node: "revise", Port: "repeat"}, To: "implement"},
		{From: config.WorkflowPort{Node: "revise", Port: "exhausted"}, To: "stop"},
	}}
}

func loadWorkflowTestConfig(t *testing.T, cfg *config.Config) *config.Config {
	t.Helper()
	fixture := *cfg
	fixture.TmuxSession = "kontora-test-" + newSessionID()
	fixture.Web.Enabled = new(false)
	data, err := yaml.Marshal(&fixture)
	require.NoError(t, err)
	loaded, err := config.LoadReader(strings.NewReader(string(data)))
	require.NoError(t, err)
	require.Equal(t, new(false), loaded.Web.Enabled)
	return loaded
}

func TestWorkflowExecution(t *testing.T) {
	for _, tc := range []struct {
		name          string
		approveAt     int
		status        ticket.Status
		runs          int
		loops         int
		badVerdict    bool
		missingReport bool
		handoff       bool
		blocked       bool
		endStatus     string
		dirty         bool
		reason        string
		outcome       string
	}{
		{name: "approval", approveAt: 1, status: ticket.StatusHumanReview, runs: 2},
		{name: "done cleans worktree", approveAt: 1, endStatus: "done", status: ticket.StatusDone, runs: 2},
		{name: "done preserves dirty worktree", approveAt: 1, endStatus: "done", status: ticket.StatusDone, runs: 2, dirty: true},
		{name: "three repairs then approval", approveAt: 4, status: ticket.StatusHumanReview, runs: 8, loops: 3, handoff: true},
		{name: "exhaustion", status: ticket.StatusPaused, runs: 8, loops: 3, reason: "loop revise exhausted"},
		{name: "invalid end status", approveAt: 1, endStatus: "todo", status: ticket.StatusPaused, runs: 2, reason: "invalid end status"},
		{name: "undeclared verdict", status: ticket.StatusPaused, runs: 2, badVerdict: true, reason: "executable protocol error", outcome: "protocol_error"},
		{name: "missing report", status: ticket.StatusPaused, runs: 2, missingReport: true, reason: "executable protocol error", outcome: "protocol_error"},
		{name: "blocked review", status: ticket.StatusPaused, runs: 2, blocked: true, reason: "executable blocked", outcome: "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.cfg
			graph := workflowTestGraph()
			if tc.handoff {
				graph.Nodes[0].Handoff = config.WorkflowHandoffPrevious
			}
			cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
			cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			cfg.Agents["agent2"] = config.Agent{Binary: "claude"}
			cfg = loadWorkflowTestConfig(t, cfg)
			if tc.endStatus != "" {
				cfg.Pipelines["graph"].Graph.Nodes[4].Status = tc.endStatus
			}
			var spawns summarySpawns
			var mu sync.Mutex
			var launches []RunnerParams
			reviews := 0
			runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
				mu.Lock()
				launches = append(launches, p)
				verdict := ""
				if p.Env[session.ResultStageEnvVar] == "review" {
					reviews++
					verdict = "changes"
					if reviews == tc.approveAt {
						verdict = "approve"
					}
					if tc.badVerdict {
						verdict = "unknown"
					}
				}
				mu.Unlock()
				inv, err := session.InvocationFromEnv(func(key string) string { return p.Env[key] })
				if err != nil {
					return process.Result{}, err
				}
				report := session.Result{Outcome: "complete", Summary: "node finished", Verdict: verdict}
				if tc.blocked && verdict != "" {
					report = session.Result{Outcome: "blocked", Summary: "review blocked", NextStep: "inspect worktree"}
				}
				if !tc.missingReport || verdict == "" {
					_, err = session.PublishResult(inv.ResultPath, report, 2)
				}
				if tc.dirty {
					require.NoError(t, os.WriteFile(filepath.Join(p.Dir, "keep.txt"), []byte("work"), 0o600))
				}
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, err
			}
			d, collect := h.newMetricsDaemon(cfg, WithRunner(runner), WithFinalSummarySpawner(spawns.spawner("workflow summary")))
			h.writeTicket("tst-graph.md", h.taskMD("tst-graph", "todo", "graph"))
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			stop := sync.OnceFunc(func() { cancel(); require.NoError(t, <-errCh) })
			t.Cleanup(stop)
			got := h.waitForStatus("tst-graph.md", tc.status, 15*time.Second)
			require.Len(t, got.History, tc.runs)
			require.Equal(t, tc.loops, got.GraphState.LoopCounts["revise"])
			reason := tc.reason
			if tc.outcome != "" {
				reason += ": " + got.History[len(got.History)-1].OutcomeReason
			}
			require.Equal(t, reason, got.LastError)
			wtPath := filepath.Join(h.wtDir, h.repoName, "tst-graph")
			if tc.status == ticket.StatusDone && !tc.dirty {
				require.Eventually(t, func() bool { _, err := os.Stat(wtPath); return os.IsNotExist(err) }, 5*time.Second, 10*time.Millisecond)
			} else {
				require.DirExists(t, wtPath)
			}
			if tc.status == ticket.StatusDone || tc.status == ticket.StatusHumanReview {
				h.waitForFinalSummary("tst-graph.md", "workflow summary", 5*time.Second)
				require.Len(t, spawns.all(), 1)
			}
			visits := make(map[string]bool)
			for _, row := range got.History {
				require.Equal(t, "step1", row.Template)
				require.NotEmpty(t, row.InvocationID)
				require.NotEmpty(t, row.NodeVisitID)
				require.False(t, visits[row.NodeVisitID])
				visits[row.NodeVisitID] = true
			}
			waitForAgentsDone(t, d, 5*time.Second)
			stop()
			outcomes := map[string]int64{"success": int64(tc.runs)}
			if tc.outcome != "" {
				outcomes["success"]--
				outcomes[tc.outcome] = 1
			}
			requireWorkflowRunMetrics(t, collect(), outcomes,
				map[string]int64{"implement": int64(tc.runs / 2), "review": int64(tc.runs / 2)},
				map[string]int64{"agent1": int64(tc.runs / 2), "agent2": int64(tc.runs / 2)})
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, launches, tc.runs)
			logs := make(map[string]bool)
			for i, launch := range launches {
				args := strings.Join(launch.Args, "\n")
				if tc.handoff && i > 0 && i%2 == 0 {
					producer := got.History[i-1]
					require.Contains(t, args, fmt.Sprintf("Producer invocation: %q", producer.InvocationID))
					require.Contains(t, args, `Validated summary: "node finished"`)
					require.Contains(t, args, `Validated verdict: "changes"`)
				} else {
					require.NotContains(t, args, "Workflow predecessor evidence")
				}
				require.Equal(t, "2", launch.Env[session.ResultVersionEnvVar])
				require.NotContains(t, fmt.Sprint(launch.Args), "Supervised local delivery")
				require.False(t, logs[launch.LogFile])
				logs[launch.LogFile] = true
			}
		})
	}
}

func TestWorkflowAndLegacyExecution(t *testing.T) {
	for _, tc := range []struct {
		pipeline string
		stages   []string
		exits    []int
	}{
		{pipeline: "two-stage", stages: []string{"step1", "step2"}, exits: []int{0, 0}},
		{pipeline: "retry-stage", stages: []string{"step1", "step1"}, exits: []int{1, 0}},
	} {
		t.Run(tc.pipeline, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: workflowTestGraph()}
			h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			h.cfg.Agents["agent2"] = config.Agent{Binary: "claude"}
			cfg := loadWorkflowTestConfig(t, h.cfg)
			var spawns summarySpawns
			var mu sync.Mutex
			legacyCalls := 0
			d := h.newDaemon(cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				now := time.Now()
				result := process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}
				if p.Env[session.ResultVersionEnvVar] == "2" {
					verdict := ""
					if p.Env[session.ResultStageEnvVar] == "review" {
						verdict = "approve"
					}
					_, err := session.PublishResult(p.Env[session.ResultFileEnvVar], session.Result{Outcome: "complete", Summary: "node finished", Verdict: verdict}, 2)
					return result, err
				}
				mu.Lock()
				defer mu.Unlock()
				require.Less(t, legacyCalls, len(tc.exits))
				result.ExitCode = tc.exits[legacyCalls]
				legacyCalls++
				return result, nil
			}), WithFinalSummarySpawner(spawns.spawner("workflow summary")))
			h.writeTicket("tst-graph.md", h.taskMD("tst-graph", "todo", "graph"))
			h.writeTicket("tst-legacy.md", h.taskMD("tst-legacy", "todo", tc.pipeline))
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			stop := sync.OnceFunc(func() { cancel(); require.NoError(t, <-errCh) })
			t.Cleanup(stop)
			graph := h.waitForStatus("tst-graph.md", ticket.StatusHumanReview, 15*time.Second)
			legacy := h.waitForStatus("tst-legacy.md", ticket.StatusDone, 15*time.Second)
			h.waitForFinalSummary("tst-graph.md", "workflow summary", 5*time.Second)
			require.Len(t, spawns.all(), 1)
			waitForAgentsDone(t, d, 5*time.Second)
			stop()
			require.Len(t, graph.History, 2)
			require.Equal(t, "implement", graph.History[0].Stage)
			require.Equal(t, "review", graph.History[1].Stage)
			require.Empty(t, graph.GraphState.LoopCounts)
			require.Nil(t, legacy.GraphState)
			require.Len(t, legacy.History, len(tc.stages))
			for i, row := range legacy.History {
				require.Equal(t, tc.stages[i], row.Stage)
				require.Equal(t, tc.exits[i], row.ExitCode)
			}
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, len(tc.exits), legacyCalls)
		})
	}
}

func TestWorkflowCheck(t *testing.T) {
	for _, tc := range []struct {
		name         string
		exit         int
		interruption error
		runnerErr    error
		status       ticket.Status
		endStatus    string
		dirty        bool
	}{
		{name: "pass", status: ticket.StatusHumanReview},
		{name: "done cleans worktree", status: ticket.StatusDone, endStatus: "done"},
		{name: "done preserves dirty worktree", status: ticket.StatusDone, endStatus: "done", dirty: true},
		{name: "nonzero", exit: 9, status: ticket.StatusPaused},
		{name: "interrupted zero", interruption: context.DeadlineExceeded, status: ticket.StatusPaused},
		{name: "launch failure", runnerErr: fmt.Errorf("launch failed"), status: ticket.StatusPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.cfg
			cfg.Agents["override"] = config.Agent{Binary: "must-not-run", Args: []string{"must-not-use"}, Environment: map[string]string{"AGENT_ONLY": "yes"}}
			endStatus := "human_review"
			if tc.endStatus != "" {
				endStatus = tc.endStatus
			}
			cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{Version: 2, Entry: "test", Nodes: []config.WorkflowNode{
				{ID: "test", Type: "check", Command: "exit 0", Timeout: config.Duration{Duration: time.Minute}},
				{ID: "pass", Type: "end", Status: endStatus}, {ID: "fail", Type: "end", Status: "paused"},
			}, Edges: []config.WorkflowEdge{
				{From: config.WorkflowPort{Node: "test", Port: "pass"}, To: "pass"},
				{From: config.WorkflowPort{Node: "test", Port: "fail"}, To: "fail"},
			}}}
			cfg = loadWorkflowTestConfig(t, cfg)
			calls := make(chan RunnerParams, 1)
			d, collect := h.newMetricsDaemon(cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls <- p
				if tc.dirty {
					require.NoError(t, os.WriteFile(filepath.Join(p.Dir, "keep.txt"), []byte("work"), 0o600))
				}
				now := time.Now()
				if tc.runnerErr != nil {
					return process.Result{ExitCode: -1, ExitedAt: now, Termination: process.TerminationNotStarted}, tc.runnerErr
				}
				return process.Result{ExitCode: tc.exit, StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone, Interruption: tc.interruption}, nil
			}))
			path := h.writeTicket("tst-check.md", h.taskMD("tst-check", "todo", "graph"))
			stored := h.readTask("tst-check.md")
			require.NoError(t, stored.SetField("agent", "override"))
			require.NoError(t, d.writeTicket(stored, path, notify.OriginDaemon))
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			stop := sync.OnceFunc(func() { cancel(); require.NoError(t, <-errCh) })
			t.Cleanup(stop)
			got := h.waitForStatus("tst-check.md", tc.status, 10*time.Second)
			require.Len(t, got.History, 1)
			launch := <-calls
			require.Equal(t, "/bin/sh", launch.Binary)
			require.Equal(t, []string{"-c", "exit 0"}, launch.Args)
			require.Equal(t, time.Minute, launch.Timeout)
			require.NotContains(t, launch.Env, "AGENT_ONLY")
			require.False(t, launch.Interactive)
			require.Empty(t, launch.SessionID)
			wtRoot, err := filepath.EvalSymlinks(h.wtDir)
			require.NoError(t, err)
			expectedDir := filepath.Join(wtRoot, h.repoName, "tst-check")
			require.Equal(t, expectedDir, launch.Dir)
			if tc.status == ticket.StatusDone && !tc.dirty {
				require.Eventually(t, func() bool { _, err := os.Stat(expectedDir); return os.IsNotExist(err) }, 5*time.Second, 10*time.Millisecond)
			} else {
				require.DirExists(t, expectedDir)
			}
			if tc.interruption != nil || tc.runnerErr != nil {
				require.Empty(t, got.GraphState.Transitions)
				require.Equal(t, "test", got.Stage)
			}
			waitForAgentsDone(t, d, 5*time.Second)
			stop()
			measured := collect()
			if tc.interruption != nil {
				require.NotContains(t, measured, "kontora.stage.runs")
				require.NotContains(t, measured, "kontora.stage.duration")
			} else {
				outcome := "success"
				if tc.exit != 0 || tc.runnerErr != nil {
					outcome = "failure"
				}
				requireWorkflowRunMetrics(t, measured, map[string]int64{outcome: 1},
					map[string]int64{"test": 1}, map[string]int64{"": 1})
				require.Zero(t, histogramPoint(t, measured["kontora.stage.duration"]).Sum)
			}
		})
	}
}

func TestWorkflowCheckHandoffRetryWakeAndLoop(t *testing.T) {
	h := newHarness(t)
	h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
	edge := func(from, port, to string) config.WorkflowEdge {
		return config.WorkflowEdge{From: config.WorkflowPort{Node: from, Port: port}, To: to}
	}
	h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{Version: 2, Entry: "tests", Nodes: []config.WorkflowNode{
		{ID: "tests", Type: "check", Command: "true", Timeout: config.Duration{Duration: time.Minute}},
		{ID: "retry_budget", Type: "loop", Max: 2},
		{ID: "repair", Type: "agent", Stage: "step1", Agent: "agent1", Handoff: config.WorkflowHandoffPrevious, Wake: &config.WorkflowWake{Max: 1}},
		{ID: "stop", Type: "end", Status: "paused"},
	}, Edges: []config.WorkflowEdge{
		edge("tests", "fail", "retry_budget"), edge("tests", "pass", "stop"),
		edge("retry_budget", "repeat", "repair"), edge("retry_budget", "exhausted", "stop"),
		edge("repair", "complete", "tests"), edge("repair", "failed", "stop"),
	}}}
	h.cfg = loadWorkflowTestConfig(t, h.cfg)
	clock := newWorkflowWaitTestClock()
	var checks []RunnerParams
	var repairs []RunnerParams
	d := h.newDaemon(h.cfg, WithWorkflowWaitClock(clock), WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
		if p.Env[session.ResultStageEnvVar] == "tests" {
			checks = append(checks, p)
			require.NoError(t, os.WriteFile(p.LogFile, fmt.Appendf(nil, "diagnostic-from-check-%d", len(checks)), 0o600))
			return workflowWaitTestResult(clock, 1), nil
		}
		repairs = append(repairs, p)
		report := session.Result{Outcome: "complete", Summary: "repaired"}
		switch len(repairs) {
		case 1:
			report = session.Result{Outcome: "blocked", Summary: "need operator", NextStep: "retry"}
		case 2:
			report = session.Result{Outcome: "waiting", Summary: "waiting on a timer", NextStep: "continue repair", Wait: &session.WaitRequest{Kind: "timer", Until: clock.Now().Add(time.Hour)}}
		}
		return workflowWaitTestResult(clock, 0), publishWorkflowWaitTestResult(p, report)
	}))
	const id = "tst-handoff"
	name := id + ".md"
	path := h.writeTicket(name, h.taskMD(id, "todo", "graph"))
	d.tickets[id] = newTicketState(h.readTask(name), path)
	ctx := context.Background()
	d.runTicket(ctx, id)
	queued := h.readTask(name)
	require.Equal(t, "repair", queued.Stage)
	first := queued.GraphState.Handoff
	require.NotNil(t, first)
	require.Equal(t, queued.GraphState.VisitID, first.DestinationVisitID)
	require.Equal(t, queued.History[0].InvocationID, first.InvocationID)
	require.Equal(t, checks[0].LogFile, first.Check.LogPath)
	for _, detail := range []bool{false, true} {
		api, err := json.Marshal(app.BuildView(h.cfg, queued, detail))
		require.NoError(t, err)
		require.NotContains(t, string(api), first.Check.Output)
	}
	require.NoError(t, os.Remove(checks[0].LogFile))
	// Retry must reuse the frozen input even after the source log is deleted.
	d.runTicket(ctx, id)
	blocked := h.readTask(name)
	require.Equal(t, ticket.StatusPaused, blocked.Status)
	require.Equal(t, first, blocked.GraphState.Handoff)
	require.NoError(t, d.RetryTicket(id))
	d.runTicket(ctx, id)
	parked := h.readTask(name)
	require.NotNil(t, parked.GraphState.Wait)
	require.Equal(t, first, parked.GraphState.Handoff)
	clock.Advance(time.Hour)
	d.reconcileWorkflowWaits(ctx)
	require.Equal(t, first, h.readTask(name).GraphState.Handoff)
	d.runTicket(ctx, id) // wake and complete
	require.Nil(t, h.readTask(name).GraphState.Handoff, "new non-opted-in visit must clear live evidence")
	d.runTicket(ctx, id)
	second := h.readTask(name).GraphState.Handoff
	require.NotNil(t, second)
	require.NotEqual(t, first.InvocationID, second.InvocationID)
	require.NotEqual(t, first.DestinationVisitID, second.DestinationVisitID)
	d.runTicket(ctx, id) // visit repair again
	require.Len(t, repairs, 4)
	for _, p := range repairs[:3] {
		require.Equal(t, first.DestinationVisitID, p.Env[session.NodeVisitIDEnvVar])
		require.Contains(t, strings.Join(p.Args, "\n"), `Output excerpt: "diagnostic-from-check-1"`)
	}
	require.Contains(t, strings.Join(repairs[2].Args, "\n"), "Workflow wake handoff")
	require.Contains(t, strings.Join(repairs[3].Args, "\n"), `Output excerpt: "diagnostic-from-check-2"`)
	require.NotContains(t, strings.Join(repairs[3].Args, "\n"), first.Check.Output)
	history, err := json.Marshal(h.readTask(name).History)
	require.NoError(t, err)
	require.NotContains(t, string(history), "diagnostic-from-check")
}

func TestWorkflowCheckHandoffAfterStageEnd(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hookExit   int
		captureErr error
	}{
		{name: "freeze original bytes before hook"},
		{name: "warn and route without evidence", hookExit: 1},
		{name: "incomplete capture is unavailable", captureErr: errors.New("sink did not reach EOF")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			const id = "tst-hook-handoff"
			logDir := filepath.Join(h.cfg.LogsDir, id)
			cwd, err := os.Getwd()
			require.NoError(t, err)
			h.cfg.LogsDir, err = filepath.Rel(cwd, h.cfg.LogsDir)
			require.NoError(t, err)
			h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{
				Run:       fmt.Sprintf(`for path in %q/tests.*.*.log; do echo replaced > "$path"; done; exit %d`, logDir, tc.hookExit),
				OnFailure: config.HookOnFailureWarn,
			}}}
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{Version: 2, Entry: "tests", Nodes: []config.WorkflowNode{
				{ID: "tests", Type: "check", Command: "true", Timeout: config.Duration{Duration: time.Minute}},
				{ID: "repair", Type: "agent", Stage: "step1", Agent: "agent1", Handoff: config.WorkflowHandoffPrevious},
				{ID: "stop", Type: "end", Status: "paused"},
			}, Edges: []config.WorkflowEdge{
				{From: config.WorkflowPort{Node: "tests", Port: "pass"}, To: "stop"},
				{From: config.WorkflowPort{Node: "tests", Port: "fail"}, To: "repair"},
				{From: config.WorkflowPort{Node: "repair", Port: "complete"}, To: "stop"},
				{From: config.WorkflowPort{Node: "repair", Port: "failed"}, To: "stop"},
			}}}
			h.cfg = loadWorkflowTestConfig(t, h.cfg)
			var logPath string
			d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				require.True(t, p.WaitForLog)
				logPath = p.LogFile
				require.NoError(t, os.WriteFile(logPath, []byte("original test output"), 0o600))
				result := workflowWaitTestResult(newWorkflowWaitTestClock(), 1)
				result.LogCaptureError = tc.captureErr
				return result, nil
			}))
			name := id + ".md"
			path := h.writeTicket(name, h.taskMD(id, "todo", "graph"))
			d.tickets[id] = newTicketState(h.readTask(name), path)
			d.runTicket(t.Context(), id)
			saved := h.readTask(name)
			require.Equal(t, ticket.StatusTodo, saved.Status)
			require.Equal(t, "repair", saved.Stage)
			data, err := os.ReadFile(logPath)
			require.NoError(t, err)
			require.Equal(t, "replaced\n", string(data))
			if tc.hookExit != 0 {
				require.Nil(t, saved.GraphState.Handoff)
				return
			}
			require.NotNil(t, saved.GraphState.Handoff)
			capture := saved.GraphState.Handoff.Check
			require.NotNil(t, capture)
			require.Equal(t, 1, capture.ExitCode)
			require.True(t, filepath.IsAbs(capture.LogPath))
			require.Equal(t, filepath.Join(logDir, filepath.Base(logPath)), capture.LogPath)
			if tc.captureErr != nil {
				require.False(t, capture.OutputAvailable)
				require.Empty(t, capture.Output)
			} else {
				require.True(t, capture.OutputAvailable)
				require.Equal(t, "original test output", capture.Output)
			}
		})
	}
}

func TestWorkflowHandoffOutputBoundedUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invocation.log")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	out, available, truncated := readGraphHandoffOutput(path)
	require.Empty(t, out)
	require.True(t, available)
	require.False(t, truncated)

	prefix := "\xff" + strings.Repeat("a", graphHandoffOutputLimit-3)
	require.NoError(t, os.WriteFile(path, []byte(prefix+"雪 after limit"), 0o600))
	out, available, truncated = readGraphHandoffOutput(path)
	require.True(t, available)
	require.True(t, truncated)
	require.True(t, utf8.ValidString(out))
	require.Equal(t, strings.ToValidUTF8(prefix, "�"), out)
	require.Len(t, out, graphHandoffOutputLimit)

	// Invalid bytes may collapse to one replacement rune, but that must not
	// let bytes beyond the raw limit into the excerpt.
	invalid := strings.Repeat("\xff", graphHandoffOutputLimit-1)
	require.NoError(t, os.WriteFile(path, []byte(invalid+"ab"), 0o600))
	out, available, truncated = readGraphHandoffOutput(path)
	require.True(t, available)
	require.True(t, truncated)
	require.Equal(t, "�a", out)
}

func requireWorkflowRunMetrics(t *testing.T, measured map[string]metricdata.Metrics, outcomes, stages, agents map[string]int64) {
	t.Helper()
	runs, ok := measured["kontora.stage.runs"]
	require.True(t, ok, "committed workflow invocations must record stage runs")
	require.Equal(t, outcomes, sumByAttr(t, runs, "outcome"))
	require.Equal(t, stages, sumByAttr(t, runs, "stage"))
	require.Equal(t, agents, sumByAttr(t, runs, "agent"))
	require.Equal(t, map[string]int64{"graph": total(outcomes)}, sumByAttr(t, runs, "pipeline"))
	duration, ok := measured["kontora.stage.duration"]
	require.True(t, ok, "committed workflow invocations must record stage duration")
	hist, ok := duration.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	var count uint64
	for _, point := range hist.DataPoints {
		count += point.Count
	}
	require.Equal(t, uint64(total(outcomes)), count)
}

func TestWorkflowHookPairing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   string
		wantStart bool
		wantEnd   bool
	}{
		{name: "preparation save failure", failure: "prepare"},
		{name: "stage start failure", failure: "start"},
		{name: "argument conflict", failure: "args", wantStart: true, wantEnd: true},
		{name: "settings creation failure", failure: "settings", wantStart: true, wantEnd: true},
		{name: "binary lookup failure", failure: "lookup", wantStart: true, wantEnd: true},
		{name: "prompt rendering failure", failure: "prompt", wantStart: true, wantEnd: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: workflowTestGraph()}
			markerDir := t.TempDir()
			start, end := filepath.Join(markerDir, "start"), filepath.Join(markerDir, "end")
			h.cfg.Hooks = config.Hooks{
				config.HookStageStart: {{Run: fmt.Sprintf("pwd > %q", start)}},
				config.HookStageEnd:   {{Run: fmt.Sprintf("pwd > %q", end)}},
			}
			path := h.writeTicket("tst-hooks.md", h.taskMD("tst-hooks", "todo", "graph"))
			switch tc.failure {
			case "prepare":
				h.cfg.Hooks[config.HookWorktreeCreated] = []config.Hook{{Run: fmt.Sprintf("chmod 444 %q", path)}}
				t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0o644)) })
			case "start":
				h.cfg.Hooks[config.HookStageStart] = []config.Hook{{Run: "exit 1", OnFailure: "pause"}}
			case "args":
				h.cfg.Agents["override"] = config.Agent{Binary: "pi", Args: []string{"--model", "provider/model:high"}}
				h.cfg.Stages["step1"] = config.Stage{Prompt: "work", Effort: config.PerAgent{Any: "high"}}
				stored := h.readTask("tst-hooks.md")
				require.NoError(t, stored.SetField("agent", "override"))
				data, err := stored.Marshal()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o644))
			case "settings":
				t.Setenv("TMPDIR", filepath.Join(markerDir, "missing"))
			case "prompt":
				h.cfg.Stages["step1"] = config.Stage{Prompt: "{{"}
			}
			calls := 0
			d := h.newDaemon(h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				calls++
				return process.Result{}, nil
			}))
			if tc.failure == "lookup" {
				d.agentLookup = func(string) (string, error) { return "", fmt.Errorf("agent unavailable") }
			}
			d.tickets["tst-hooks"] = newTicketState(h.readTask("tst-hooks.md"), path)
			d.runTicket(context.Background(), "tst-hooks")
			require.Zero(t, calls)
			for _, marker := range []struct {
				path string
				want bool
			}{{start, tc.wantStart}, {end, tc.wantEnd}} {
				if !marker.want {
					require.NoFileExists(t, marker.path)
					continue
				}
				data, err := os.ReadFile(marker.path)
				require.NoError(t, err)
				wtPath, err := filepath.EvalSymlinks(filepath.Join(h.wtDir, h.repoName, "tst-hooks"))
				require.NoError(t, err)
				require.Equal(t, wtPath, strings.TrimSpace(string(data)))
			}
			if tc.failure != "prepare" {
				require.Equal(t, ticket.StatusPaused, h.readTask("tst-hooks.md").Status)
			}
		})
	}
}

func TestWorkflowFinalizationMetrics(t *testing.T) {
	for _, kind := range []string{"agent", "check"} {
		for _, mode := range []string{"replay", "hook failure replay", "shutdown replay", "stale exit"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				h := newHarness(t)
				node := config.WorkflowNode{ID: "work", Type: kind}
				port, agent := "pass", ""
				if kind == "agent" {
					node.Stage, node.Agent = "step1", "agent1"
					port, agent = "complete", "agent1"
					h.cfg.Agents[agent] = config.Agent{Binary: "claude"}
				} else {
					node.Command = "exit 0"
					node.Timeout = config.Duration{Duration: time.Minute}
				}
				h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: &config.WorkflowGraph{
					Version: 2, Entry: node.ID,
					Nodes: []config.WorkflowNode{node, {ID: "end", Type: "end", Status: "human_review"}},
					Edges: []config.WorkflowEdge{{From: config.WorkflowPort{Node: node.ID, Port: port}, To: "end"}},
				}}
				if mode != "stale exit" {
					command := `chmod 444 "$KONTORA_TICKET_FILE"`
					if mode == "hook failure replay" {
						command += "; exit 1"
					}
					h.cfg.Hooks = config.Hooks{config.HookStageEnd: {{Run: command, OnFailure: "pause"}}}
				}
				path := h.writeTicket("tst-metrics.md", h.taskMD("tst-metrics", "todo", "graph"))
				t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0o644)) })
				calls := 0
				d, collect := h.newMetricsDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
					calls++
					if kind == "agent" {
						_, err := session.PublishResult(p.Env[session.ResultFileEnvVar], session.Result{Outcome: "complete", Summary: "node finished"}, 2)
						require.NoError(t, err)
					}
					if mode == "stale exit" {
						current := h.readTask("tst-metrics.md")
						require.NoError(t, current.SetField("status", "open"))
						data, err := current.Marshal()
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(path, data, 0o644))
					}
					started := time.Now()
					return process.Result{StartedAt: started, ExitedAt: started.Add(2 * time.Second), Termination: process.TerminationProcessGroupGone}, nil
				}), WithFinalSummarySpawner(func(context.Context, FinalSummaryParams) (string, error) { return "summary", nil }))
				d.windows.list = func(string) ([]string, error) { return nil, nil }
				d.tickets["tst-metrics"] = newTicketState(h.readTask("tst-metrics.md"), path)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				d.runTicket(ctx, "tst-metrics")
				d.background.Wait()
				before := collect()
				require.NotContains(t, before, "kontora.stage.runs", "an uncommitted transition must not record a run")
				require.NotContains(t, before, "kontora.stage.duration")
				if mode == "stale exit" {
					require.Equal(t, ticket.StatusOpen, h.readTask("tst-metrics.md").Status)
					require.Equal(t, 1, calls)
					return
				}
				rec, err := loadExecution(h.cfg, "tst-metrics")
				require.NoError(t, err)
				require.Equal(t, "transition", rec.Phase)
				require.Contains(t, d.pendingFinalizations, rec.Invocation.ID)
				if mode == "shutdown replay" {
					cancel()
				}
				d.handleFileChanged(path)
				d.background.Wait()
				require.NotContains(t, collect(), "kontora.stage.runs", "a failed replay must not record a run")
				require.NoError(t, os.Chmod(path, 0o644))
				for range 3 {
					d.handleFileChanged(path)
					d.background.Wait()
				}
				got := h.readTask("tst-metrics.md")
				outcome, status := "success", ticket.StatusHumanReview
				if mode == "hook failure replay" {
					outcome, status = "failure", ticket.StatusPaused
				}
				require.Equal(t, status, got.Status)
				require.Len(t, got.History, 1)
				require.Empty(t, got.InvocationID)
				require.Empty(t, d.pendingFinalizations)
				require.NoFileExists(t, executionPath(h.cfg, got.ID))
				require.Equal(t, 1, calls)
				measured := collect()
				requireWorkflowRunMetrics(t, measured, map[string]int64{outcome: 1},
					map[string]int64{node.ID: 1}, map[string]int64{agent: 1})
				require.Equal(t, float64(2), histogramPoint(t, measured["kontora.stage.duration"]).Sum)
			})
		}
	}
}

func TestGraphResultInstructions(t *testing.T) {
	for _, verdicts := range [][]string{nil, {"needs changes", "approve"}, {"修正", "quote\"and\\slash"}} {
		text := graphResultInstructions(config.WorkflowNode{Verdicts: verdicts})
		if len(verdicts) == 0 {
			require.NotContains(t, text, "declared verdict")
			continue
		}
		encoded, err := json.Marshal(verdicts)
		require.NoError(t, err)
		require.Contains(t, text, "one declared verdict: "+string(encoded)+".")
	}
}

func TestWorkflowAdmission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		annotation bool
		supervisor bool
		status     ticket.Status
	}{
		{name: "terminal entry", status: ticket.StatusHumanReview},
		{name: "annotation refused", annotation: true, status: ticket.StatusPaused},
		{name: "epic supervisor refused", supervisor: true, status: ticket.StatusPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			graph := &config.WorkflowGraph{Version: 2, Entry: "end", Nodes: []config.WorkflowNode{{ID: "end", Type: "end", Status: "human_review"}}}
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
			h.cfg = loadWorkflowTestConfig(t, h.cfg)
			calls := make(chan struct{}, 1)
			d := h.newDaemon(h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				calls <- struct{}{}
				return process.Result{}, nil
			}))
			body := h.taskMD("tst-entry", "todo", "graph")
			if tc.annotation {
				body = strings.Replace(body, "status: todo", "status: todo\nannotation_return_status: open", 1)
			}
			if tc.supervisor {
				body = strings.Replace(body, "status: todo", "status: todo\nkind: epic\nepic_execution: agent", 1)
			}
			h.writeTicket("tst-entry.md", body)
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
			got := h.waitForStatus("tst-entry.md", tc.status, 10*time.Second)
			require.Empty(t, got.History)
			require.Empty(t, got.StartedAt)
			require.Empty(t, got.InvocationID)
			if tc.annotation || tc.supervisor {
				require.Contains(t, got.LastError, "graph workflows do not support annotation runs or epic supervisors")
			}
			select {
			case <-calls:
				t.Fatal("non-executable admission launched a runner")
			default:
			}
			d.mu.Lock()
			require.Empty(t, d.running)
			require.Empty(t, d.runningBranches)
			d.mu.Unlock()
		})
	}
}
