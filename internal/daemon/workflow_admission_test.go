package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"gopkg.in/yaml.v3"
)

func workflowAdmissionGraph() *config.WorkflowGraph {
	return &config.WorkflowGraph{Version: 2, Entry: "work", Nodes: []config.WorkflowNode{
		{ID: "work", Type: "agent", Stage: "step1", Agent: "agent1"},
		{ID: "budget", Type: "loop", Max: 2},
		{ID: "next", Type: "agent", Stage: "step2", Agent: "agent2"},
		{ID: "end", Type: "end", Status: "human_review"},
		{ID: "stop", Type: "end", Status: "paused"},
	}, Edges: []config.WorkflowEdge{
		{From: config.WorkflowPort{Node: "work", Port: "complete"}, To: "budget"},
		{From: config.WorkflowPort{Node: "work", Port: "failed"}, To: "stop"},
		{From: config.WorkflowPort{Node: "budget", Port: "repeat"}, To: "next"},
		{From: config.WorkflowPort{Node: "budget", Port: "exhausted"}, To: "stop"},
		{From: config.WorkflowPort{Node: "next", Port: "complete"}, To: "end"},
		{From: config.WorkflowPort{Node: "next", Port: "failed"}, To: "stop"},
	}}
}

func TestWorkflowAdmissionFingerprintOrdering(t *testing.T) {
	h := newHarness(t)
	tk, err := ticket.Parse(strings.NewReader(h.taskMD("tst-order", "todo", "graph")))
	require.NoError(t, err)
	const source = `version: 2
entry: review
nodes:
  - {id: review, type: agent, stage: step1, agent: agent1, verdicts: [approve, changes]}
  - {id: end, type: end, status: human_review}
  - {id: stop, type: end, status: paused}
edges:
  - {from: {node: review, port: complete}, to: end}
  - {from: {node: review, port: failed}, to: stop}
`
	var original config.WorkflowGraph
	require.NoError(t, yaml.Unmarshal([]byte(source), &original))
	want, err := workflowFingerprint(h.cfg, tk, &original)
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		nodes    bool
		edges    bool
		verdicts bool
	}{
		{name: "nodes", nodes: true},
		{name: "edges", edges: true},
		{name: "verdicts", verdicts: true},
		{name: "all", nodes: true, edges: true, verdicts: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var graph config.WorkflowGraph
			require.NoError(t, yaml.Unmarshal([]byte(source), &graph))
			if tc.verdicts {
				slices.Reverse(graph.Nodes[0].Verdicts)
			}
			if tc.nodes {
				slices.Reverse(graph.Nodes)
			}
			if tc.edges {
				slices.Reverse(graph.Edges)
			}
			data, err := yaml.Marshal(graph)
			require.NoError(t, err)
			var reordered config.WorkflowGraph
			require.NoError(t, yaml.Unmarshal(data, &reordered))
			got, err := workflowFingerprint(h.cfg, tk, &reordered)
			require.NoError(t, err)
			require.Equal(t, want, got)
			after, err := yaml.Marshal(reordered)
			require.NoError(t, err)
			require.Equal(t, string(data), string(after), "fingerprinting must not reorder the definition in place")
		})
	}
}

func TestWorkflowAdmissionFingerprintChanges(t *testing.T) {
	h := newHarness(t)
	tk, err := ticket.Parse(strings.NewReader(h.taskMD("tst-hash", "todo", "graph")))
	require.NoError(t, err)
	for _, tc := range []struct {
		name      string
		override  string
		unchanged bool
		change    func(*config.Config, *ticket.Ticket, *config.WorkflowGraph)
	}{
		{name: "stage prompt", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			s := c.Stages["step1"]
			s.Prompt += " changed"
			c.Stages["step1"] = s
		}},
		{name: "stage timeout", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			s := c.Stages["step1"]
			s.Timeout.Duration = time.Minute
			c.Stages["step1"] = s
		}},
		{name: "stage model", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			s := c.Stages["step1"]
			s.Model.Any = "new-model"
			c.Stages["step1"] = s
		}},
		{name: "stage effort", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			s := c.Stages["step1"]
			s.Effort.Any = "high"
			c.Stages["step1"] = s
		}},
		{name: "agent binary", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			a := c.Agents["agent1"]
			a.Binary = "other"
			c.Agents["agent1"] = a
		}},
		{name: "agent args", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			a := c.Agents["agent1"]
			a.Args = []string{"--new"}
			c.Agents["agent1"] = a
		}},
		{name: "agent environment", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			a := c.Agents["agent1"]
			a.Environment = map[string]string{"VALUE": "new"}
			c.Agents["agent1"] = a
		}},
		{name: "agent failure patterns", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			a := c.Agents["agent1"]
			a.FailurePatterns = []string{"new failure"}
			c.Agents["agent1"] = a
		}},
		{name: "override identity", change: func(_ *config.Config, tk *ticket.Ticket, _ *config.WorkflowGraph) { tk.Agent = "override" }},
		{name: "override config", override: "override", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			a := c.Agents["override"]
			a.Binary = "other"
			c.Agents["override"] = a
		}},
		{name: "overridden agent ignored", override: "override", unchanged: true, change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.Agents["agent1"] = config.Agent{Binary: "other"}
		}},
		{name: "worktree hook", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.Hooks[config.HookWorktreeCreated][0].Run = "echo changed"
		}},
		{name: "stage start hook", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.Hooks[config.HookStageStart][0].Run = "echo changed"
		}},
		{name: "stage end hook", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.Hooks[config.HookStageEnd][0].Run = "echo changed"
		}},
		{name: "project hook", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.Projects["repo"].Hooks[config.HookStageStart][0].Run = "echo changed"
		}},
		{name: "global system prompt", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.SystemPrompt = "new system prompt"
		}},
		{name: "global resume prompt", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.ResumePrompt = "new resume prompt"
		}},
		{name: "global environment", change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) { c.Environment["VALUE"] = "new" }},
		{name: "graph edge", change: func(_ *config.Config, _ *ticket.Ticket, g *config.WorkflowGraph) { g.Edges[0].To = "stop" }},
		{name: "graph loop allowance", change: func(_ *config.Config, _ *ticket.Ticket, g *config.WorkflowGraph) { g.Nodes[1].Max++ }},
		{name: "unreferenced stage", unchanged: true, change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.Stages["unused"] = config.Stage{Prompt: "new"}
		}},
		{name: "unreferenced agent", unchanged: true, change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.Agents["unused"] = config.Agent{Binary: "other"}
		}},
		{name: "other project hook", unchanged: true, change: func(c *config.Config, _ *ticket.Ticket, _ *config.WorkflowGraph) {
			c.Projects["other"] = config.Project{Path: filepath.Join(h.repoDir, "other"), Hooks: config.Hooks{config.HookStageEnd: {{Run: "echo changed"}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := h.defaultConfig("true", "true")
			cfg.Agents["override"] = cfg.Agents["agent1"]
			cfg.Hooks = config.Hooks{}
			for _, event := range []string{config.HookWorktreeCreated, config.HookStageStart, config.HookStageEnd} {
				cfg.Hooks[event] = []config.Hook{{Run: "true"}}
			}
			cfg.Projects = map[string]config.Project{"repo": {Path: h.repoDir, Hooks: config.Hooks{config.HookStageStart: {{Run: "true"}}}}}
			cfg.Environment = map[string]string{"VALUE": "old"}
			current := tk.Clone()
			current.Agent = tc.override
			graph := workflowAdmissionGraph()
			before, err := workflowFingerprint(cfg, current, graph)
			require.NoError(t, err)
			tc.change(cfg, current, graph)
			after, err := workflowFingerprint(cfg, current, graph)
			require.NoError(t, err)
			if tc.unchanged {
				require.Equal(t, before, after)
			} else {
				require.NotEqual(t, before, after)
			}
		})
	}
}

func TestWorkflowAdmissionCapturedConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
	}{
		{name: "stage", change: func(c *config.Config) { s := c.Stages["step1"]; s.Prompt = "changed prompt"; c.Stages["step1"] = s }},
		{name: "agent", change: func(c *config.Config) {
			a := c.Agents["agent1"]
			a.FailurePatterns = []string{"captured output"}
			c.Agents["agent1"] = a
		}},
		{name: "hook", change: func(c *config.Config) { c.Hooks[config.HookStageEnd][0].Run = "exit 7" }},
		{name: "system prompt", change: func(c *config.Config) { c.SystemPrompt = "changed system prompt" }},
		{name: "environment", change: func(c *config.Config) { c.Environment["ADMISSION_ENV"] = "changed" }},
		{name: "graph", change: func(c *config.Config) { c.Pipelines["graph"].Graph.Edges[0].To = "stop" }},
		{name: "legacy pipeline", change: func(c *config.Config) {
			c.Stages["next"] = c.Stages["step2"]
			c.Pipelines["graph"] = config.PipelineDefinition{Legacy: config.Pipeline{
				{Stage: "next", Agent: "agent2", OnSuccess: "done", OnFailure: "pause"},
			}}
		}},
		{name: "pipeline removed", change: func(c *config.Config) { delete(c.Pipelines, "graph") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.cfg
			cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: workflowAdmissionGraph()}
			cfg.Environment = map[string]string{"ADMISSION_ENV": "captured"}
			marker := filepath.Join(t.TempDir(), "stage-end")
			cfg.Hooks = config.Hooks{config.HookStageEnd: {{Run: fmt.Sprintf(`printf captured > %q`, marker), OnFailure: "pause"}}}
			data, err := json.Marshal(cfg)
			require.NoError(t, err)
			var next config.Config
			require.NoError(t, json.Unmarshal(data, &next))
			tc.change(&next)
			launched := make(chan RunnerParams, 1)
			release := make(chan struct{})
			var calls atomic.Int32
			d := h.newDaemon(cfg, WithRunner(func(ctx context.Context, p RunnerParams) (process.Result, error) {
				calls.Add(1)
				launched <- p
				select {
				case <-release:
				case <-ctx.Done():
					return process.Result{Termination: process.TerminationProcessGroupGone, Interruption: ctx.Err()}, ctx.Err()
				}
				var err error
				if p.Env[session.ResultVersionEnvVar] == "2" {
					var inv session.Invocation
					inv, err = session.InvocationFromEnv(func(key string) string { return p.Env[key] })
					if err == nil {
						_, err = session.PublishResult(inv.ResultPath, session.Result{Outcome: "complete", Summary: "captured completion"}, 2)
					}
				}
				if err == nil {
					err = os.WriteFile(p.LogFile, []byte("captured output\n"), 0o600)
				}
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, err
			}))
			d.graphWorkflowsReady = true
			path := h.writeTicket("tst-captured.md", h.taskMD("tst-captured", "todo", "graph"))
			d.tickets["tst-captured"] = newTicketState(h.readTask("tst-captured.md"), path)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); d.runTicket(ctx, "tst-captured") }()
			t.Cleanup(func() { cancel(); <-done })
			var launch RunnerParams
			select {
			case launch = <-launched:
			case <-time.After(10 * time.Second):
				t.Fatal("workflow runner did not start")
			}
			active := h.readTask("tst-captured.md")
			require.Equal(t, ticket.StatusInProgress, active.Status)
			require.Equal(t, "captured", launch.Env["ADMISSION_ENV"])
			require.Equal(t, "2", launch.Env[session.ResultVersionEnvVar])
			d.cfg.Store(&next)
			close(release)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("workflow invocation did not finish")
			}
			finished := h.readTask("tst-captured.md")
			require.Equal(t, ticket.StatusTodo, finished.Status)
			require.Equal(t, "next", finished.Stage)
			require.Empty(t, finished.InvocationID)
			require.Len(t, finished.History, 1)
			require.Equal(t, ticket.RunOutcomeComplete, finished.History[0].RunOutcome)
			require.Equal(t, active.InvocationID, finished.History[0].InvocationID)
			require.Equal(t, active.GraphState.ExecutionID, finished.GraphState.ExecutionID)
			require.Equal(t, active.GraphState.DefinitionFingerprint, finished.GraphState.DefinitionFingerprint)
			require.Equal(t, active.GraphState.Visit+1, finished.GraphState.Visit)
			require.Equal(t, active.GraphState.Revision+1, finished.GraphState.Revision)
			require.Equal(t, 1, finished.GraphState.LoopCounts["budget"])
			require.Len(t, finished.GraphState.Transitions, 2)
			markerData, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Equal(t, "captured", string(markerData))
			require.NoFileExists(t, executionPath(cfg, finished.ID))
			d.runTicket(context.Background(), finished.ID)
			paused := h.readTask("tst-captured.md")
			require.Equal(t, ticket.StatusPaused, paused.Status)
			require.Contains(t, paused.LastError, "workflow definition changed")
			require.Equal(t, finished.Stage, paused.Stage)
			require.Equal(t, finished.GraphState, paused.GraphState)
			require.Equal(t, finished.History, paused.History)
			require.Empty(t, paused.InvocationID)
			require.EqualValues(t, 1, calls.Load())
			require.Empty(t, d.running)
			require.Empty(t, d.runningBranches)
			require.Empty(t, d.executions)
		})
	}
}

func TestWorkflowAdmissionStaleCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ticket.GraphState)
	}{
		{name: "execution", change: func(s *ticket.GraphState) { s.ExecutionID = "replacement-execution" }},
		{name: "visit", change: func(s *ticket.GraphState) { s.VisitID = "replacement-visit" }},
		{name: "revision", change: func(s *ticket.GraphState) { s.Revision++ }},
		{name: "definition", change: func(s *ticket.GraphState) { s.DefinitionFingerprint = "replacement-definition" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: workflowAdmissionGraph()}
			path := h.writeTicket("tst-stale-graph.md", h.taskMD("tst-stale-graph", "todo", "graph"))
			var replacement []byte
			var calls int
			var d *Daemon
			d = h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls++
				require.NoError(t, d.mutateTicket("tst-stale-graph", func(current *ticket.Ticket) error {
					state := current.GraphState.Clone()
					tc.change(state)
					return current.SetField("graph_state", state)
				}))
				var err error
				replacement, err = os.ReadFile(path)
				require.NoError(t, err)
				inv, err := session.InvocationFromEnv(func(key string) string { return p.Env[key] })
				require.NoError(t, err)
				_, err = session.PublishResult(inv.ResultPath, session.Result{Outcome: "complete", Summary: "stale completion"}, 2)
				require.NoError(t, err)
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone}, nil
			}))
			d.graphWorkflowsReady = true
			d.tickets["tst-stale-graph"] = newTicketState(h.readTask("tst-stale-graph.md"), path)
			d.runTicket(context.Background(), "tst-stale-graph")
			require.Equal(t, 1, calls)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, string(replacement), string(data), "stale completion must not write the replacement ticket")
			got := h.readTask("tst-stale-graph.md")
			require.Equal(t, ticket.StatusInProgress, got.Status)
			require.Equal(t, "work", got.Stage)
			require.Empty(t, got.History)
			require.Empty(t, got.GraphState.Transitions)
			require.NotEmpty(t, got.InvocationID)
			require.Empty(t, d.running)
			require.Empty(t, d.runningBranches)
			require.Empty(t, d.executions)
		})
	}
}

func TestWorkflowAdmissionInterruptedCheck(t *testing.T) {
	for _, tc := range []struct {
		name       string
		invocation bool
		journal    bool
		reason     string
	}{
		{name: "invocation and journal", invocation: true, journal: true, reason: "journal requires recovery"},
		{name: "invocation only", invocation: true, reason: "recovery acknowledgement"},
		{name: "journal only", journal: true, reason: "journal requires recovery"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			graph := workflowAdmissionGraph()
			graph.Nodes[0] = config.WorkflowNode{ID: "work", Type: "check", Command: "exit 0", Timeout: config.Duration{Duration: time.Minute}}
			graph.Edges[0].From.Port = "pass"
			graph.Edges[1].From.Port = "fail"
			h.cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
			var calls int
			d := h.newDaemon(h.cfg, WithRunner(func(_ context.Context, p RunnerParams) (process.Result, error) {
				calls++
				require.Equal(t, "/bin/sh", p.Binary)
				require.Equal(t, []string{"-c", "exit 0"}, p.Args)
				now := time.Now()
				return process.Result{StartedAt: now, ExitedAt: now, Termination: process.TerminationProcessGroupGone, Interruption: context.DeadlineExceeded}, nil
			}))
			d.graphWorkflowsReady = true
			path := h.writeTicket("tst-interrupted.md", h.taskMD("tst-interrupted", "todo", "graph"))
			d.tickets["tst-interrupted"] = newTicketState(h.readTask("tst-interrupted.md"), path)
			d.runTicket(context.Background(), "tst-interrupted")
			interrupted := h.readTask("tst-interrupted.md")
			require.Equal(t, ticket.StatusPaused, interrupted.Status)
			require.NotEmpty(t, interrupted.InvocationID)
			require.Empty(t, interrupted.GraphState.Transitions)
			require.Empty(t, interrupted.GraphState.LoopCounts)
			require.Len(t, interrupted.History, 1)
			rec, err := loadExecution(h.cfg, interrupted.ID)
			require.NoError(t, err)
			require.Equal(t, "recovery_required", rec.Phase)
			require.Equal(t, interrupted.InvocationID, rec.Invocation.ID)
			require.Equal(t, interrupted.GraphState, rec.GraphBefore)
			if !tc.journal {
				require.NoError(t, os.Remove(executionPath(h.cfg, interrupted.ID)))
			}
			require.NoError(t, d.mutateTicket(interrupted.ID, func(current *ticket.Ticket) error {
				if !tc.invocation {
					if err := current.SetField("invocation_id", ""); err != nil {
						return err
					}
				}
				return current.SetField("status", "todo")
			}))
			d.runTicket(context.Background(), interrupted.ID)
			got := h.readTask("tst-interrupted.md")
			require.Equal(t, ticket.StatusPaused, got.Status)
			require.Contains(t, got.LastError, tc.reason)
			require.Equal(t, interrupted.Stage, got.Stage)
			require.Equal(t, interrupted.GraphState, got.GraphState)
			require.Equal(t, interrupted.History, got.History)
			if tc.invocation {
				require.Equal(t, interrupted.InvocationID, got.InvocationID)
			} else {
				require.Empty(t, got.InvocationID)
			}
			if tc.journal {
				gotRec, err := loadExecution(h.cfg, got.ID)
				require.NoError(t, err)
				require.Equal(t, rec, gotRec)
			}
			require.Equal(t, 1, calls)
			require.Empty(t, d.running)
			require.Empty(t, d.runningBranches)
			require.Empty(t, d.executions)
		})
	}
}
