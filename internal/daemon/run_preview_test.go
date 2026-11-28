package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestGetTicketRunPreviewSavedSettings(t *testing.T) {
	h := newHarness(t)
	cfg := h.cfg
	cfg.AutoPickUp = new(false)
	cfg.ReworkIsBuiltin = true
	cfg.Plannotator.ReviewsDir = t.TempDir()
	t.Setenv("HOME", filepath.Dir(h.repoDir))
	savedRepo := "~/" + filepath.Base(h.repoDir)
	cfg.Projects = map[string]config.Project{"project": {Path: h.repoDir, BranchPrefix: "project", Agent: "project-agent"}}
	cfg.Agents["project-agent"] = config.Agent{Binary: "true"}
	cfg.Pipelines["two-stage"].Legacy[0].Agent = "agent2"
	cfg.Pipelines["declared-rework"] = config.PipelineDefinition{Legacy: config.Pipeline{{Stage: "rework", Agent: "agent2", OnSuccess: "done", OnFailure: "pause"}}}
	d := h.newDaemon(cfg)

	const id = "tst-preview"
	const generatedBranch = "project/saved-run-tst-preview"
	for _, tc := range []struct {
		name, pipeline, branch, agent, kind, errPart string
		stages                                       []string
		fields                                       map[string]any
		notes, omitted                               bool
	}{
		{name: "first step without saved stage uses its own agent even when blocked", pipeline: "two-stage", branch: generatedBranch,
			agent: "agent2", stages: []string{"step1", "step2"}, fields: map[string]any{"deps": []string{"missing-dep"}}},
		{name: "saved stage and branch", pipeline: "two-stage", branch: "topic/existing", agent: "agent2", stages: []string{"step2"},
			fields: map[string]any{"stage": "step2", "branch": "topic/existing"}},
		{name: "agent override", pipeline: "two-stage", branch: generatedBranch, agent: "agent1", stages: []string{"step1", "step2"},
			fields: map[string]any{"agent": "agent1"}},
		{name: "built-in rework uses history", pipeline: "two-stage", branch: generatedBranch, agent: "agent1", kind: "rework", stages: []string{"rework"},
			fields: map[string]any{"stage": "rework", "history": []ticket.HistoryEntry{{Stage: "step1", Agent: "agent2"}, {Stage: "step2", Agent: "agent1"}}}},
		{name: "declared rework uses pipeline", pipeline: "declared-rework", branch: generatedBranch, agent: "agent2", stages: []string{"rework"},
			fields: map[string]any{"stage": "rework"}},
		{name: "removed pipeline", pipeline: "removed", branch: generatedBranch,
			errPart: `unknown pipeline "removed"; select a configured pipeline`},
		{name: "removed stage", pipeline: "two-stage", branch: generatedBranch, fields: map[string]any{"stage": "removed"},
			errPart: `unknown pipeline stage "removed" in pipeline "two-stage"; select a configured stage`},
		{name: "removed agent", pipeline: "two-stage", branch: generatedBranch, fields: map[string]any{"agent": "removed"},
			errPart: `unknown agent "removed"; select a configured agent`},
		{name: "standalone uses daemon default", branch: generatedBranch, agent: "agent1"},
		{name: "pending annotation does not run the pipeline or create a branch", pipeline: "two-stage", agent: "agent2", kind: "annotation", notes: true,
			fields: map[string]any{"annotation_return_status": "open", "history": []ticket.HistoryEntry{{Stage: "step1", Agent: "agent2"}}}},
		{name: "missing annotations send ticket back without an agent", pipeline: "two-stage", kind: "annotation",
			fields: map[string]any{"annotation_return_status": "open", "path": ""}},
		{name: "unmanaged", omitted: true, fields: map[string]any{"kontora": false}},
		{name: "epic", omitted: true, fields: map[string]any{"kind": "epic"}},
		{name: "queued", omitted: true, fields: map[string]any{"status": "todo"}},
		{name: "scheduled", omitted: true, fields: map[string]any{"scheduled_at": "2099-01-01T10:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tk := parseRunPreviewTicket(t, h, id, tc.pipeline, tc.fields, savedRepo)
			path := storeRunPreviewTicket(t, h, d, tk)
			if tc.notes {
				file := annotationsPath(cfg.Plannotator.ReviewsDir, id)
				require.NoError(t, os.WriteFile(file, []byte("Use a shorter title."), 0o644))
				t.Cleanup(func() { require.NoError(t, os.Remove(file)) })
			}
			info, err := d.GetTicket(id)
			require.NoError(t, err)
			if tc.omitted {
				assert.NotContains(t, runPreviewJSON(t, info), "run_preview")
				return
			}
			stages := tc.stages
			if stages == nil {
				stages = []string{}
			}
			repo := h.repoDir
			if tk.Path == "" {
				repo = filepath.Dir(path)
			}
			preview := info.RunPreview
			require.NotNil(t, preview)
			assert.Equal(t, tc.branch, preview.Branch)
			assert.Equal(t, repo, preview.Repo)
			if tc.errPart == "" {
				assert.Empty(t, preview.Error)
			} else {
				assert.Contains(t, preview.Error, tc.errPart)
			}
			want := map[string]any{"stages": stages, "branch": tc.branch, "repo": repo}
			if tc.agent != "" {
				want["agent"] = tc.agent
			}
			if tc.kind != "" {
				want["kind"] = tc.kind
			}
			if tc.errPart != "" {
				want["error"] = preview.Error
			}
			encoded, err := json.Marshal(want)
			require.NoError(t, err)
			assert.JSONEq(t, string(encoded), string(runPreviewJSON(t, info)["run_preview"]))
			listed := d.ListTickets(web.ListTicketsOptions{})
			require.Len(t, listed, 1)
			assert.NotContains(t, runPreviewJSON(t, listed[0]), "run_preview")
		})
	}
}

func TestGetTicketRunPreviewWorkflow(t *testing.T) {
	h := newHarness(t)
	cfg := h.cfg
	marker := filepath.Join(t.TempDir(), "condition-ran")
	graph := &config.WorkflowGraph{Version: 2, Entry: "implement", Nodes: []config.WorkflowNode{
		{ID: "implement", Type: "agent", Stage: "step1", Agent: "agent1"},
		{ID: "verify", Type: "check", Command: "true", Timeout: config.Duration{Duration: time.Second}},
		{ID: "passed", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", Equals: 0}},
		{ID: "probe", Type: "condition", Command: fmt.Sprintf("touch %q", marker), Timeout: config.Duration{Duration: time.Second}},
		{ID: "again", Type: "loop", Max: 1},
		{ID: "review", Type: "agent", Stage: "step2", Agent: "agent2"},
		{ID: "approval", Type: "wait", Mode: "human"},
		{ID: "finish", Type: "end", Status: "done"},
	}, Edges: []config.WorkflowEdge{
		{From: config.WorkflowPort{Node: "implement", Port: "complete"}, To: "verify"},
		{From: config.WorkflowPort{Node: "implement", Port: "failed"}, To: "finish"},
		{From: config.WorkflowPort{Node: "verify", Port: "pass"}, To: "passed"},
		{From: config.WorkflowPort{Node: "verify", Port: "fail"}, To: "passed"},
		{From: config.WorkflowPort{Node: "passed", Port: "true"}, To: "probe"},
		{From: config.WorkflowPort{Node: "passed", Port: "false"}, To: "finish"},
		{From: config.WorkflowPort{Node: "probe", Port: "true"}, To: "again"},
		{From: config.WorkflowPort{Node: "probe", Port: "false"}, To: "finish"},
		{From: config.WorkflowPort{Node: "again", Port: "repeat"}, To: "review"},
		{From: config.WorkflowPort{Node: "again", Port: "exhausted"}, To: "approval"},
		{From: config.WorkflowPort{Node: "review", Port: "complete"}, To: "finish"},
		{From: config.WorkflowPort{Node: "review", Port: "failed"}, To: "finish"},
		{From: config.WorkflowPort{Node: "approval", Port: "released"}, To: "finish"},
	}}
	cfg.Pipelines["graph"] = config.PipelineDefinition{Graph: graph}
	d := h.newDaemon(cfg)

	const id = "tst-graph-preview"
	for _, tc := range []struct {
		name, stage, override, wantNode, kind, agent, errPart string
		saved, passed, exhausted, stale, waiting              bool
	}{
		{name: "new execution starts at entry", wantNode: "implement", kind: "graph", agent: "agent1"},
		{name: "resume a saved agent node", saved: true, stage: "review", wantNode: "review", kind: "graph", agent: "agent2"},
		{name: "check ignores saved agent override", saved: true, stage: "verify", override: "agent2", wantNode: "verify", kind: "check"},
		{name: "control cursor advances to its next agent", saved: true, stage: "again", wantNode: "review", kind: "graph", agent: "agent2"},
		{name: "exhausted loop reaches a wait without an agent", saved: true, exhausted: true, stage: "again", wantNode: "approval", kind: "wait"},
		{name: "condition uses saved check result to terminate without an agent", saved: true, stage: "passed", wantNode: "finish", kind: "end"},
		{name: "saved command condition ignores agent override", saved: true, passed: true, stage: "probe", override: "agent2", wantNode: "probe", kind: "condition"},
		{name: "control cursor stops at command condition", saved: true, passed: true, stage: "passed", wantNode: "probe", kind: "condition"},
		{name: "active wait does not start an agent", saved: true, waiting: true, stage: "approval", kind: "wait", errPart: "before continuation"},
		{name: "new graph cannot start at a saved non-entry stage", stage: "review", kind: "graph", errPart: "must start at its entry"},
		{name: "unknown saved node", saved: true, stage: "removed", kind: "graph", errPart: `workflow node "removed": unknown graph cursor`},
		{name: "saved end node requires a restart", saved: true, stage: "finish", kind: "graph", errPart: "use workflow-restart"},
		{name: "changed definition", saved: true, stage: "review", stale: true, kind: "graph", errPart: "workflow definition changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tk := parseRunPreviewTicket(t, h, id, "graph", map[string]any{"stage": tc.stage, "agent": tc.override}, h.repoDir)
			if tc.saved {
				fingerprint, err := workflowFingerprint(cfg, tk, graph)
				require.NoError(t, err)
				state := &ticket.GraphState{Version: 2, DefinitionFingerprint: fingerprint, ExecutionID: "saved", VisitID: "saved.1", Visit: 1, Revision: 10}
				if tc.exhausted {
					state.LoopCounts = map[string]int{"again": 1}
				}
				if tc.stage == "passed" || tc.stage == "probe" {
					outcome, code := "failed", 1
					if tc.passed {
						outcome, code = "complete", 0
					}
					state.LastResult = &ticket.GraphResult{NodeID: "verify", VisitID: state.VisitID, InvocationID: "prior", Outcome: outcome, ExitCode: &code}
				}
				if tc.stale {
					state.DefinitionFingerprint = "different"
				}
				if tc.waiting {
					state.Wait = &ticket.WorkflowWait{ID: "waiting", Kind: "human", Phase: "pending", ExecutionID: "saved", NodeID: "approval", VisitID: "saved.1"}
				}
				require.NoError(t, tk.SetField("graph_state", state))
			}
			path := storeRunPreviewTicket(t, h, d, tk)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			stateBefore := tk.GraphState.Clone()

			info, err := d.GetTicket(id)
			require.NoError(t, err)
			require.NotNil(t, info.RunPreview)
			preview := info.RunPreview
			assert.Equal(t, tc.kind, preview.Kind)
			if tc.errPart != "" {
				assert.Contains(t, preview.Error, tc.errPart)
				assert.Equal(t, []string{}, preview.Stages)
			} else {
				assert.Empty(t, preview.Error)
				assert.Equal(t, []string{tc.wantNode}, preview.Stages)
			}
			assert.Equal(t, tc.agent, preview.Agent)
			if tc.agent == "" {
				assert.NotContains(t, string(runPreviewJSON(t, info)["run_preview"]), `"agent":`)
			}

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Equal(t, stateBefore, tk.GraphState)
			assert.Equal(t, tc.stage, tk.Stage)
			assert.Equal(t, ticket.StatusOpen, tk.Status)
			assert.Empty(t, d.running)
			assert.Empty(t, d.executions)
			assert.NoFileExists(t, marker)
			_, err = loadExecution(cfg, id)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func parseRunPreviewTicket(t *testing.T, h *testHarness, id, pipeline string, fields map[string]any, repo string) *ticket.Ticket {
	t.Helper()
	tk, err := ticket.ParseBytes([]byte(h.taskMD(id, "open", pipeline)))
	require.NoError(t, err)
	tk.SetBody("# Saved run\n")
	require.NoError(t, tk.SetField("path", repo))
	for key, value := range fields {
		require.NoError(t, tk.SetField(key, value))
	}
	return tk
}

func storeRunPreviewTicket(t *testing.T, h *testHarness, d *Daemon, tk *ticket.Ticket) string {
	t.Helper()
	data, err := tk.Marshal()
	require.NoError(t, err)
	path := h.writeTicket(tk.ID+".md", string(data))
	d.tickets[tk.ID] = newTicketState(tk, path)
	return path
}

func runPreviewJSON(t *testing.T, info web.TicketInfo) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(info)
	require.NoError(t, err)
	var projection map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &projection))
	return projection
}
