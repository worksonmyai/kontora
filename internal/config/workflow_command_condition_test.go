package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestWorkflowCommandConditionFields(t *testing.T) {
	const predicate = ", when: {field: verdict, equals: approve}"
	const command = ", command: 'test -f ready'"
	when := &WorkflowCondition{Field: "verdict", Equals: "approve"}
	for _, tc := range []struct {
		name, fields string
		node         WorkflowNode
		wantErr      string
		wantTimeout  time.Duration
	}{
		{name: "both", fields: predicate + command, node: WorkflowNode{When: when, Command: "test -f ready"}, wantErr: "exactly one of when or command"},
		{name: "neither", wantErr: "exactly one of when or command"},
		{name: "blank command", fields: `, command: " \t "`, node: WorkflowNode{Command: " \t "}, wantErr: "command must not be blank"},
		{name: "NUL in command", fields: `, command: "test -f ready\0"`, node: WorkflowNode{Command: "test -f ready\x00"}, wantErr: "command contains NUL"},
		{name: "negative timeout", fields: command + ", timeout: -1s", node: WorkflowNode{Command: "test -f ready", Timeout: Duration{-time.Second}}, wantErr: "timeout must be positive"},
		{name: "timeout with when", fields: predicate + ", timeout: 1s", node: WorkflowNode{When: when, Timeout: Duration{time.Second}}, wantErr: "timeout requires command"},
		{name: "command with default timeout", fields: command, node: WorkflowNode{Command: "test -f ready"}, wantTimeout: time.Minute},
		{name: "command with positive timeout", fields: command + ", timeout: 5s", node: WorkflowNode{Command: "test -f ready", Timeout: Duration{5 * time.Second}}, wantTimeout: 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(t *testing.T, cfg *Config, err error) {
				t.Helper()
				if tc.wantErr != "" {
					require.ErrorContains(t, err, `node "approved"`)
					require.ErrorContains(t, err, tc.wantErr)
					return
				}
				require.NoError(t, err)
				node, ok := cfg.Pipelines["work"].Graph.Node("approved")
				require.True(t, ok)
				require.Equal(t, tc.node.Command, node.Command)
				require.Nil(t, node.When)
				require.Equal(t, tc.wantTimeout, node.Timeout.Duration)
			}
			t.Run("YAML", func(t *testing.T) {
				cfg, err := LoadReader(strings.NewReader(strings.Replace(workflowConfig, predicate, tc.fields, 1)))
				check(t, cfg, err)
			})
			t.Run("memory", func(t *testing.T) {
				cfg, err := LoadReader(strings.NewReader(workflowConfig))
				require.NoError(t, err)
				node := tc.node
				node.ID, node.Type = "approved", "condition"
				cfg.Pipelines["work"].Graph.Nodes[3] = node
				check(t, cfg, cfg.Validate())
			})
		})
	}

	for _, tc := range []struct{ name, fields, want string }{
		{"zero timeout with command", command + ", timeout: 0s", "timeout must be positive"},
		{"zero timeout with when", predicate + ", timeout: 0s", "timeout requires command"},
		{"null when before command", ", when: null" + command, "exactly one of when or command"},
		{"null when after command", command + ", when: null", "exactly one of when or command"},
		{"null when without command", ", when: null", "exactly one of when or command"},
	} {
		t.Run("YAML/"+tc.name, func(t *testing.T) {
			_, err := LoadReader(strings.NewReader(strings.Replace(workflowConfig, predicate, tc.fields, 1)))
			require.ErrorContains(t, err, `node "approved"`)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestWorkflowCommandConditionEntry(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		change      func(*WorkflowGraph)
		want        string
	}{
		{name: "direct command entry", entry: "approved", want: "command condition requires an agent or check before it"},
		{name: "wait before command at entry", entry: "gate", change: func(g *WorkflowGraph) {
			g.Nodes = append(g.Nodes, WorkflowNode{ID: "gate", Type: "wait", Mode: "human"})
			g.Edges = append(g.Edges, WorkflowEdge{From: WorkflowPort{Node: "gate", Port: "released"}, To: "approved"})
		}, want: "command condition requires an agent or check before it"},
		{name: "entry loop repeats through command before producer", entry: "revise", change: func(g *WorkflowGraph) {
			g.Edges[4].To = "accept"    // review.complete
			g.Edges[7].To = "implement" // approved.false
			g.Edges[8].To = "approved"  // revise.repeat
		}, want: "command condition requires an agent or check before it"},
		{name: "entry loop repeats through an agent before exhaustion", entry: "revise", change: func(g *WorkflowGraph) {
			g.Edges[7].To = "stop"     // approved.false
			g.Edges[9].To = "approved" // revise.exhausted
		}},
		{name: "command condition and loop form a control-only cycle", entry: "implement", change: func(g *WorkflowGraph) {
			g.Edges[8].To = "approved" // revise.repeat
		}, want: "control-only workflow cycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadReader(strings.NewReader(workflowConfig))
			require.NoError(t, err)
			g := cfg.Pipelines["work"].Graph
			g.Entry = tc.entry
			g.Nodes[3].When, g.Nodes[3].Command = nil, "test -f ready"
			if tc.change != nil {
				tc.change(g)
			}
			check := func(t *testing.T, err error) {
				t.Helper()
				if tc.want == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tc.want)
				}
			}
			t.Run("YAML", func(t *testing.T) {
				data, err := yaml.Marshal(cfg)
				require.NoError(t, err)
				_, err = LoadReader(strings.NewReader(string(data)))
				check(t, err)
			})
			t.Run("memory", func(t *testing.T) { check(t, cfg.Validate()) })
		})
	}
}
