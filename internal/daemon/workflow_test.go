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
	}{
		{name: "approval", approveAt: 1, status: ticket.StatusHumanReview, runs: 2},
		{name: "done cleans worktree", approveAt: 1, endStatus: "done", status: ticket.StatusDone, runs: 2},
		{name: "done preserves dirty worktree", approveAt: 1, endStatus: "done", status: ticket.StatusDone, runs: 2, dirty: true},
		{name: "three repairs then approval", approveAt: 4, status: ticket.StatusHumanReview, runs: 8, loops: 3},
		{name: "exhaustion", status: ticket.StatusPaused, runs: 8, loops: 3},
		{name: "undeclared verdict", status: ticket.StatusPaused, runs: 2, badVerdict: true},
		{name: "missing report", status: ticket.StatusPaused, runs: 2, missingReport: true},
		{name: "blocked review", status: ticket.StatusPaused, runs: 2, blocked: true},
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
			d := h.newDaemon(cfg, WithRunner(runner), WithFinalSummarySpawner(spawns.spawner("workflow summary")))
			d.graphWorkflowsReady = true
			h.writeTicket("tst-graph.md", h.taskMD("tst-graph", "todo", "graph"))
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
			got := h.waitForStatus("tst-graph.md", tc.status, 15*time.Second)
			require.Len(t, got.History, tc.runs)
			require.Equal(t, tc.loops, got.GraphState.LoopCounts["revise"])
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
			d := h.newDaemon(cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls <- p
				if tc.dirty {
					require.NoError(t, os.WriteFile(filepath.Join(p.Dir, "keep.txt"), []byte("work"), 0o600))
				}
				now := time.Now()
				return process.Result{ExitCode: tc.exit, StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone, Interruption: tc.interruption}, tc.runnerErr
			}))
			d.graphWorkflowsReady = true
			path := h.writeTicket("tst-check.md", h.taskMD("tst-check", "todo", "graph"))
			stored := h.readTask("tst-check.md")
			require.NoError(t, stored.SetField("agent", "override"))
			require.NoError(t, d.writeTicket(stored, path, notify.OriginDaemon))
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
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
		})
	}
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
