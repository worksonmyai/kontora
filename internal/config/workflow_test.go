package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const workflowConfig = `
agents:
  claude: {binary: claude}
stages:
  code: {prompt: implement}
  review: {prompt: review}
statuses: [waiting_external, researching]
pipelines:
  work:
    version: 2
    entry: implement
    nodes:
      - {id: implement, type: agent, stage: code, agent: claude}
      - {id: test, type: check, command: go test ./..., timeout: 1m}
      - {id: review, type: agent, stage: review, agent: claude, verdicts: [approve, changes]}
      - {id: approved, type: condition, when: {field: verdict, equals: approve}}
      - {id: revise, type: loop, max: 3}
      - {id: accept, type: end, status: human_review}
      - {id: stop, type: end, status: paused}
    edges:
      - {from: {node: implement, port: complete}, to: test}
      - {from: {node: implement, port: failed}, to: stop}
      - {from: {node: test, port: pass}, to: review}
      - {from: {node: test, port: fail}, to: revise}
      - {from: {node: review, port: complete}, to: approved}
      - {from: {node: review, port: failed}, to: stop}
      - {from: {node: approved, port: "true"}, to: accept}
      - {from: {node: approved, port: "false"}, to: revise}
      - {from: {node: revise, port: repeat}, to: implement}
      - {from: {node: revise, port: exhausted}, to: stop}
`

func TestWorkflowConfig(t *testing.T) {
	cfg, err := LoadReader(strings.NewReader(workflowConfig))
	require.NoError(t, err)
	definition := cfg.Pipelines["work"]
	require.True(t, definition.IsGraph())
	assert.Nil(t, definition.Legacy)
	graph := definition.Graph
	assert.Equal(t, 2, graph.Version)
	assert.Equal(t, "implement", graph.Entry)
	node, ok := graph.Node("test")
	require.True(t, ok)
	assert.Equal(t, time.Minute, node.Timeout.Duration)
	_, ok = graph.Node("missing")
	assert.False(t, ok)
	target, ok := graph.Target("revise", "repeat")
	require.True(t, ok)
	assert.Equal(t, "implement", target)
	_, ok = graph.Target("revise", "missing")
	assert.False(t, ok)
	_, ok = graph.Target("missing", "repeat")
	assert.False(t, ok)

	data, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	reloaded, err := LoadReader(strings.NewReader(string(data)))
	require.NoError(t, err)
	assert.Equal(t, definition, reloaded.Pipelines["work"])
	assert.NotContains(t, string(data), "legacy:")
	assert.NotContains(t, string(data), "graph:")
}

func TestWorkflowValidation(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement, want string
	}{
		{"version", "version: 2", "version: 1", "unsupported workflow version"},
		{"missing version", "version: 2", "", "unsupported workflow version"},
		{"entry", "entry: implement", "entry: missing", "unknown node"},
		{"duplicate nodes list", "entry: implement", "entry: implement\n    nodes: []", "duplicate field"},
		{"unknown graph field", "version: 2", "version: 2\n    surprise: true", "field surprise"},
		{"unknown node field", "id: implement, type: agent", "id: implement, type: agent, surprise: true", "field surprise"},
		{"unknown condition field", "field: verdict, equals: approve", "field: verdict, equals: approve, surprise: true", "field surprise"},
		{"unknown edge field", "to: test}", "to: test, surprise: true}", "field surprise"},
		{"unknown port field", "node: implement, port: complete", "node: implement, port: complete, surprise: true", "field surprise"},
		{"agent command even empty", "type: agent, stage: code", "type: agent, command: '', stage: code", "not supported"},
		{"check agent even empty", "type: check, command:", "type: check, agent: '', command:", "not supported"},
		{"condition max even zero", "type: condition, when:", "type: condition, max: 0, when:", "not supported"},
		{"loop verdicts even empty", "type: loop, max: 3", "type: loop, verdicts: [], max: 3", "not supported"},
		{"end timeout even zero", "type: end, status: paused", "type: end, timeout: 0s, status: paused", "not supported"},
		{"duplicate graph field", "version: 2", "version: 2\n    version: 2", "duplicate field"},
		{"duplicate node field", "id: implement, type:", "id: implement, id: implement, type:", "duplicate field"},
		{"duplicate condition field", "field: verdict, equals: approve", "field: verdict, equals: approve, equals: changes", "duplicate field"},
		{"duplicate edge field", "to: test}", "to: test, to: review}", "duplicate field"},
		{"duplicate port field", "node: implement, port: complete", "node: implement, port: complete, port: failed", "duplicate field"},
		{"unknown kind", "type: loop", "type: fork", "unknown type"},
		{"unknown stage", "stage: code", "stage: unknown", "unknown stage"},
		{"unknown agent", "agent: claude", "agent: unknown", "unknown agent"},
		{"duplicate verdict", "[approve, changes]", "[approve, approve]", "duplicate verdict"},
		{"empty verdict", "[approve, changes]", "[approve, '']", "empty or duplicate verdict"},
		{"NUL verdict", "[approve, changes]", `["approve\0", changes]`, "contains NUL"},
		{"NUL in later verdict", "[approve, changes]", `[approve, "changes\0"]`, "contains NUL"},
		{"verdict with spaces", "[approve, changes]", "[approve, 'needs changes']", ""},
		{"unicode verdict", "[approve, changes]", "[approve, '変更が必要']", ""},
		{"empty command", "command: go test ./...", "command: ' '", "command is required"},
		{"missing timeout", ", timeout: 1m", "", "timeout must be positive"},
		{"negative timeout", "timeout: 1m", "timeout: -1s", "timeout must be positive"},
		{"bad timeout", "timeout: 1m", "timeout: tomorrow", "invalid duration"},
		{"missing condition", ", when: {field: verdict, equals: approve}", "", "when is required"},
		{"unknown condition", "field: verdict", "field: summary", "unknown condition field"},
		{"missing equals", ", equals: approve", "", "verdict equals"},
		{"null equals", "equals: approve", "equals: null", "verdict equals"},
		{"typed verdict", "equals: approve", "equals: true", "verdict equals"},
		{"typed exit code", "field: verdict, equals: approve", "field: exit_code, equals: '0'", "exit_code equals"},
		{"float exit code", "field: verdict, equals: approve", "field: exit_code, equals: 0.0", "exit_code equals"},
		{"missing max", ", max: 3", "", "max must be positive"},
		{"negative max", "max: 3", "max: -1", "max must be positive"},
		{"zero max", "max: 3", "max: 0", "max must be positive"},
		{"unsafe ID", "implement", "UpperCase", "invalid or reserved"},
		{"path ID", "implement", "../code", "invalid or reserved"},
		{"unicode ID", "implement", "café", "invalid or reserved"},
		{"reserved default", "implement", "default", "invalid or reserved"},
		{"reserved annotation", "implement", "annotation", "invalid or reserved"},
		{"reserved rework", "implement", "rework", "invalid or reserved"},
		{"reserved suffix", "implement", "code-annotation", "invalid or reserved"},
		{"duplicate ID", "id: test", "id: implement", "duplicate workflow node ID"},
		{"missing source", "node: test, port: pass", "node: missing, port: pass", "edge source"},
		{"missing target", "to: test}", "to: missing}", "edge target"},
		{"unknown port", "port: complete", "port: success", "unknown port"},
		{"end port", "node: test, port: pass", "node: accept, port: pass", "unknown port"},
		{"missing port", "      - {from: {node: test, port: pass}, to: review}\n", "", "missing destination"},
		{"duplicate edge", "      - {from: {node: test, port: pass}, to: review}", "      - {from: {node: test, port: pass}, to: review}\n      - {from: {node: test, port: pass}, to: review}", "duplicate edge"},
		{"conflicting edge", "      - {from: {node: test, port: pass}, to: review}", "      - {from: {node: test, port: pass}, to: review}\n      - {from: {node: test, port: pass}, to: stop}", "duplicate edge"},
		{"unreachable", "    edges:", "      - {id: unused, type: end, status: done}\n    edges:", "unreachable node"},
		{"disconnected executable nodes", "port: complete}, to: test", "port: complete}, to: implement", "unreachable node"},
		{"cycle bypassing loop", "port: failed}, to: stop", "port: failed}, to: implement", "cycle bypasses loop.repeat"},
		{"control cycle", "port: repeat}, to: implement", "port: repeat}, to: approved", "control-only workflow cycle"},
		{"unknown end status", "status: paused", "status: missing", "invalid end status"},
		{"todo end status", "status: paused", "status: todo", "invalid end status"},
		{"open end status", "status: paused", "status: open", "invalid end status"},
		{"running end status", "status: paused", "status: in_progress", "invalid end status"},
		{"closed end status", "status: paused", "status: closed", "invalid end status"},
		{"archived end status", "status: paused", "status: archived", "invalid end status"},
		{"graph merge", "version: 2", "version: 2\n    <<: {entry: implement}", "merge keys"},
		{"node merge", "id: implement, type: agent", "id: implement, <<: {type: agent}", "merge keys"},
		{"unused anchor", "entry: implement", "entry: &start implement", ""},
		{"numeric exit code", "field: verdict, equals: approve", "field: exit_code, equals: 0", ""},
		{"custom parked status", "status: paused", "status: waiting_external", ""},
		{"done status", "status: paused", "status: done", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, workflowConfig, tc.old)
			input := strings.ReplaceAll(workflowConfig, tc.old, tc.replacement)
			_, err := LoadReader(strings.NewReader(input))
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestWorkflowAliases(t *testing.T) {
	for _, input := range []string{
		strings.ReplaceAll(strings.Replace(workflowConfig, "entry: implement", "entry: &start implement", 1), "node: implement", "node: *start"),
		strings.Replace(workflowConfig, "  work:", "  work: &work", 1) + "  alias: *work\n",
		strings.Replace(workflowConfig, "    version: 2", "    version: &version 2", 1) + "  alias: {version: *version, entry: stop, nodes: [], edges: []}\n",
	} {
		_, err := LoadReader(strings.NewReader(input))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "alias")
	}
}

func TestWorkflowCustomActiveEndStatus(t *testing.T) {
	input := strings.ReplaceAll(workflowConfig, "status: paused", "status: researching") + `
  legacy:
    - {stage: code, agent: claude, running_status: researching, on_success: done, on_failure: pause}
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, "not a parked status")
}

func TestWorkflowEndOnly(t *testing.T) {
	cfg, err := LoadReader(strings.NewReader("agents: {claude: {binary: claude}}\npipelines: {work: {version: 2, entry: stop, nodes: [{id: stop, type: end, status: done}], edges: []}}"))
	require.NoError(t, err)
	assert.True(t, cfg.Pipelines["work"].IsGraph())
}

func TestPipelineResolve(t *testing.T) {
	cfg, err := LoadReader(strings.NewReader(workflowConfig))
	require.NoError(t, err)
	legacy := PipelineDefinition{Legacy: Pipeline{{Stage: "code", Agent: "claude"}}}
	for _, tc := range []struct {
		name       string
		definition PipelineDefinition
		id         string
		override   string
		want       ResolvedNode
	}{
		{"legacy", legacy, "code", "", ResolvedNode{"code", "agent", "code", "claude"}},
		{"legacy override", legacy, "code", "pi", ResolvedNode{"code", "agent", "code", "pi"}},
		{"graph agent", cfg.Pipelines["work"], "implement", "", ResolvedNode{"implement", "agent", "code", "claude"}},
		{"graph agent override", cfg.Pipelines["work"], "implement", "pi", ResolvedNode{"implement", "agent", "code", "pi"}},
		{"check ignores override", cfg.Pipelines["work"], "test", "pi", ResolvedNode{ID: "test", Kind: "check"}},
		{"condition ignores override", cfg.Pipelines["work"], "approved", "pi", ResolvedNode{ID: "approved", Kind: "condition"}},
		{"loop ignores override", cfg.Pipelines["work"], "revise", "pi", ResolvedNode{ID: "revise", Kind: "loop"}},
		{"end ignores override", cfg.Pipelines["work"], "accept", "pi", ResolvedNode{ID: "accept", Kind: "end"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := tc.definition.Resolve(tc.id, tc.override)
			require.NoError(t, err)
			assert.Equal(t, tc.want, resolved)
		})
	}
	for _, definition := range []PipelineDefinition{legacy, cfg.Pipelines["work"]} {
		_, err := definition.Resolve("missing", "pi")
		require.Error(t, err)
	}
}

func TestLegacyPipelineUnionRoundTrip(t *testing.T) {
	for _, fixture := range []string{"testdata/valid.yaml", "testdata/minimal.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			cfg, err := Load(fixture)
			require.NoError(t, err)
			for _, definition := range cfg.Pipelines {
				assert.False(t, definition.IsGraph())
				list, err := yaml.Marshal(definition.Legacy)
				require.NoError(t, err)
				union, err := yaml.Marshal(definition)
				require.NoError(t, err)
				assert.Equal(t, string(list), string(union))
			}
			data, err := yaml.Marshal(cfg)
			require.NoError(t, err)
			reloaded, err := LoadReader(strings.NewReader(string(data)))
			require.NoError(t, err)
			assert.Equal(t, cfg.Pipelines, reloaded.Pipelines)
		})
	}
}

func TestWorkflowDefinitionValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"empty graph", func(c *Config) { c.Pipelines["work"].Graph.Nodes = nil }, "at least one node"},
		{"empty ID", func(c *Config) { c.Pipelines["work"].Graph.Nodes[0].ID = "" }, "invalid or reserved"},
		{"missing entry", func(c *Config) { c.Pipelines["work"].Graph.Entry = "" }, "unknown node"},
		{"in-memory wrong kind field", func(c *Config) { c.Pipelines["work"].Graph.Nodes[1].Agent = "claude" }, "not supported"},
		{"mixed representations", func(c *Config) {
			p := c.Pipelines["work"]
			p.Legacy = Pipeline{}
			c.Pipelines["work"] = p
		}, "both legacy stages and a graph"},
		{"repeated template", func(c *Config) { c.Pipelines["work"].Graph.Nodes[2].Stage = "code" }, ""},
		{"multiple finite loops", func(c *Config) {
			g := c.Pipelines["work"].Graph
			g.Nodes = append(g.Nodes, WorkflowNode{ID: "retry-test", Type: "loop", Max: 2})
			g.Edges[3].To = "retry-test"
			g.Edges = append(g.Edges,
				WorkflowEdge{From: WorkflowPort{Node: "retry-test", Port: "repeat"}, To: "test"},
				WorkflowEdge{From: WorkflowPort{Node: "retry-test", Port: "exhausted"}, To: "stop"})
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadReader(strings.NewReader(workflowConfig))
			require.NoError(t, err)
			tc.change(cfg)
			err = cfg.Validate()
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestWorkflowAndLegacyCoexist(t *testing.T) {
	input := workflowConfig + `
  legacy: &legacy
    - &step {stage: code, agent: claude, on_success: done, on_failure: pause}
  legacy-alias: *legacy
  legacy-merged: [{<<: *step}]
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.True(t, cfg.Pipelines["work"].IsGraph())
	assert.Equal(t, cfg.Pipelines["legacy"], cfg.Pipelines["legacy-alias"])
	assert.Equal(t, cfg.Pipelines["legacy"], cfg.Pipelines["legacy-merged"])
	data, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	reloaded, err := LoadReader(strings.NewReader(string(data)))
	require.NoError(t, err)
	assert.Equal(t, cfg.Pipelines, reloaded.Pipelines)
}

func TestLegacyPipelineUnionStrictness(t *testing.T) {
	const prefix = "agents: {claude: {binary: claude}}\nstages: {code: {prompt: code}}\npipelines:\n"
	for _, tc := range []struct{ name, pipelines, want string }{
		{"unknown field", "  work: [{stage: code, agent: claude, on_success: done, on_failure: pause, surprise: true}]", "field surprise"},
		{"duplicate field", "  work: [{stage: code, stage: code, agent: claude, on_success: done, on_failure: pause}]", "already defined"},
		{"duplicate pipeline", "  work: []\n  work: []", "already defined"},
		{"aliases and merges", "  work: &work\n    - &step {stage: code, agent: claude, on_success: done, on_failure: pause}\n  alias: *work\n  merged: [{<<: *step}]", ""},
		{"unknown merged field", "  work: [{<<: {stage: code, agent: claude, on_success: done, on_failure: pause, surprise: true}}]", "field surprise"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadReader(strings.NewReader(prefix + tc.pipelines))
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}
