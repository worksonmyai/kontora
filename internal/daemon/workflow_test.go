package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

func TestWorkflowExecution(t *testing.T) {
	for _, tc := range []struct {
		name          string
		approveAt     int
		status        ticket.Status
		runs          int
		loops         int
		badVerdict    bool
		missingReport bool
		blocked       bool
		endStatus     string
		dirty         bool
		reason        string
		outcome       string
	}{
		{name: "approval", approveAt: 1, status: ticket.StatusHumanReview, runs: 2},
		{name: "done cleans worktree", approveAt: 1, endStatus: "done", status: ticket.StatusDone, runs: 2},
		{name: "done preserves dirty worktree", approveAt: 1, endStatus: "done", status: ticket.StatusDone, runs: 2, dirty: true},
		{name: "three repairs then approval", approveAt: 4, status: ticket.StatusHumanReview, runs: 8, loops: 3},
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
			if tc.endStatus != "" {
				graph.Nodes[4].Status = tc.endStatus
			}
			cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
			cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			cfg.Agents["agent2"] = config.Agent{Binary: "claude"}
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
			d.graphWorkflowsReady = true
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
			for _, launch := range launches {
				require.Equal(t, "2", launch.Env[session.ResultVersionEnvVar])
				require.NotContains(t, fmt.Sprint(launch.Args), "Supervised local delivery")
				require.False(t, logs[launch.LogFile])
				logs[launch.LogFile] = true
			}
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
			d.graphWorkflowsReady = true
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
			d.graphWorkflowsReady = true
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
				d.graphWorkflowsReady = true
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
		name   string
		ready  bool
		entry  string
		status ticket.Status
	}{
		{name: "normal pickup refused", entry: "test", status: ticket.StatusPaused},
		{name: "terminal entry", ready: true, entry: "end", status: ticket.StatusHumanReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			graph := &config.WorkflowGraph{Version: 2, Entry: "end", Nodes: []config.WorkflowNode{{ID: "end", Type: "end", Status: "human_review"}}}
			if tc.entry == "test" {
				graph = workflowTestGraph()
			}
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
			calls := make(chan struct{}, 1)
			d := h.newDaemon(h.cfg, WithRunner(func(context.Context, RunnerParams) (process.Result, error) {
				calls <- struct{}{}
				return process.Result{}, nil
			}))
			d.graphWorkflowsReady = tc.ready
			h.writeTicket("tst-entry.md", h.taskMD("tst-entry", "todo", "graph"))
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
			got := h.waitForStatus("tst-entry.md", tc.status, 10*time.Second)
			require.Empty(t, got.History)
			require.Empty(t, got.StartedAt)
			require.Empty(t, got.InvocationID)
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
