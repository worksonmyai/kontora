package cli

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestWorkflowRecoveryFlags(t *testing.T) {
	for _, tc := range []struct {
		id, revision string
		want         uint64
		fail         bool
	}{
		{"execution", "0", 0, false},
		{"execution", "18446744073709551615", 18446744073709551615, false},
		{"", "12", 0, true},
		{" ", "12", 0, true},
		{"execution", "", 0, true},
		{"execution", "-1", 0, true},
		{"execution", "1.5", 0, true},
		{"execution", "18446744073709551616", 0, true},
	} {
		t.Run(tc.id+"/"+tc.revision, func(t *testing.T) {
			got, err := ParseWorkflowRecovery(tc.id, tc.revision)
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func cliWorkflowConfig() *config.Config {
	return &config.Config{Pipelines: map[string]config.PipelineDefinition{
		"graph": {Graph: &config.WorkflowGraph{Version: 2, Entry: "write", Nodes: []config.WorkflowNode{
			{ID: "stop", Type: "end", Status: "paused"},
			{ID: "write", Type: "agent", Stage: "code", Agent: "coder"},
			{ID: "budget", Type: "loop", Max: 3},
		}, Edges: []config.WorkflowEdge{{From: config.WorkflowPort{Node: "budget", Port: "repeat"}, To: "write"}}}},
	}}
}

func TestWorkflowInitSelection(t *testing.T) {
	cfg := cliWorkflowConfig()
	for _, tc := range []struct {
		stage string
		fail  bool
	}{{"", false}, {"write", false}, {"stop", true}, {"code", true}} {
		t.Run(tc.stage, func(t *testing.T) {
			stage, err := startingStage(cfg, "graph", EnableOpts{Stage: tc.stage})
			if tc.fail {
				require.ErrorContains(t, err, "entry")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "write", stage)
			}
		})
	}
	assert.Equal(t, []string{"graph · entry write · 3 nodes"}, pipelineDescs(cfg, []string{"graph"}))
}

func TestWorkflowViewUsesActualTransitions(t *testing.T) {
	cfg := cliWorkflowConfig()
	cfg.TicketsDir = t.TempDir()
	tkt, err := ticket.ParseBytes([]byte("---\nid: graph-1\nkontora: true\npipeline: graph\nstage: write\nstatus: paused\nattempt: 1\nlast_error: inspected\n---\n# Graph\n"))
	require.NoError(t, err)
	require.NoError(t, tkt.SetField("graph_state", &ticket.GraphState{
		Version: 1, ExecutionID: "execution", Revision: 9, DefinitionFingerprint: "fingerprint",
		Visit: 3, VisitID: "execution.3", LoopCounts: map[string]int{"budget": 2},
		NodeTemplates: map[string]string{"write": "code"}, LoopLimits: map[string]int{"budget": 3},
		Transitions: []ticket.GraphTransition{
			{From: "budget", Port: "repeat", To: "write", LoopCount: 1, Revision: 4},
			{From: "budget", Port: "repeat", To: "write", LoopCount: 2, Revision: 8},
		},
	}))
	data, err := tkt.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.TicketsDir, "graph-1.md"), data, 0o600))
	var out bytes.Buffer
	require.NoError(t, View(cfg, "graph-1", &out))
	for _, want := range []string{"execution: execution (revision 9)", "node:      write", "template:  code", "visit:     3 (execution.3)", "attempt:   1 (within this visit)", "budget 2/3", "loop count 1", "loop count 2", "inspected"} {
		assert.Contains(t, out.String(), want)
	}
	assert.NotContains(t, out.String(), "stop ->")
	assert.NotContains(t, out.String(), "stage:     ")
}

func TestWorkflowDefinitionHasNoPositionalProgress(t *testing.T) {
	cfg := cliWorkflowConfig()
	view := app.BuildView(cfg, &ticket.Ticket{Pipeline: "graph"}, true)
	var out bytes.Buffer
	PrintWorkflow(&out, view.GraphDefinition, view.GraphExecution)
	assert.Contains(t, out.String(), "execution: not started")
	assert.Contains(t, out.String(), "write (agent) template code")
	assert.Contains(t, out.String(), "budget.repeat -> write")
	assert.NotContains(t, out.String(), "0%")
}

func TestWorkflowLoopLimitDisplay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		max   int
		limit string
	}{{"saved limit", 3, "3"}, {"missing snapshot", 0, "unknown"}} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			PrintWorkflow(&out, nil, &app.GraphExecutionView{Loops: []app.GraphLoopView{{NodeID: "budget", Used: 2, Max: tc.max}}})
			assert.Contains(t, out.String(), "budget 2/"+tc.limit)
		})
	}
}

func TestWorkflowControlsRequireDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	server.Close()
	cfg := cliWorkflowConfig()
	cfg.TicketsDir = t.TempDir()
	cfg.Web.Host = host
	cfg.Web.Port, err = strconv.Atoi(port)
	require.NoError(t, err)
	path := filepath.Join(cfg.TicketsDir, "graph-1.md")
	original := []byte("---\nid: graph-1\nstatus: paused\npipeline: graph\n---\n# Graph\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	assert.ErrorContains(t, WorkflowRestart(cfg, "graph-1"), "daemon not reachable")
	assert.ErrorContains(t, WorkflowRecover(cfg, "graph-1", "execution", 0), "daemon not reachable")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestWorkflowUpdateCannotDiscardIdentity(t *testing.T) {
	cfg := cliWorkflowConfig()
	cfg.Pipelines["legacy"] = config.PipelineDefinition{Legacy: config.Pipeline{{Stage: "code"}}}
	for _, pipeline := range []string{"legacy", "", "none"} {
		t.Run(pipeline, func(t *testing.T) {
			cfg.TicketsDir = t.TempDir()
			tkt, err := ticket.ParseBytes([]byte("---\nid: graph-1\nstatus: paused\npipeline: graph\nstage: write\n---\n# Graph\n"))
			require.NoError(t, err)
			require.NoError(t, tkt.SetField("graph_state", &ticket.GraphState{Version: 1, ExecutionID: "execution"}))
			data, err := tkt.Marshal()
			require.NoError(t, err)
			path := filepath.Join(cfg.TicketsDir, "graph-1.md")
			require.NoError(t, os.WriteFile(path, data, 0o600))
			err = Update(cfg, "graph-1", web.UpdateTicketRequest{Pipeline: &pipeline})
			require.ErrorIs(t, err, app.ErrInvalidState)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, data, got)
		})
	}
}

func TestWorkflowCommandMetadata(t *testing.T) {
	for _, name := range []string{"workflow-restart", "workflow-recover"} {
		found := false
		for _, command := range Commands {
			if command.Name != name {
				continue
			}
			found = true
			assert.True(t, command.Remote)
			assert.True(t, command.Config)
			assert.True(t, command.TicketID)
			assert.False(t, command.Store, "daemon-only actions must not advertise --tickets-dir")
		}
		assert.True(t, found, name)
	}
}
