package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const waitWorkflowConfig = `
agents: {claude: {binary: claude}}
stages: {code: {prompt: implement}}
pipelines:
  work:
    version: 2
    entry: implement
    nodes:
      - {id: implement, type: agent, stage: code, agent: claude, wake: {max: 12}}
      - {id: deploy, type: wait, mode: deployment, environment: production, timeout: 168h}
      - {id: accept, type: end, status: done}
      - {id: stop, type: end, status: paused}
    edges:
      - {from: {node: implement, port: complete}, to: deploy}
      - {from: {node: implement, port: failed}, to: stop}
      - {from: {node: deploy, port: released}, to: accept}
      - {from: {node: deploy, port: timeout}, to: stop}
`

func TestWorkflowWaitConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement, want string }{
		{"deployment", "168h", "168h", ""},
		{"human", "mode: deployment, environment: production", "mode: human", ""},
		{"check timeout", "wake: {max: 12}", "wake: {max: 12, check_timeout: 5s}", ""},
		{"missing max", "wake: {max: 12}", "wake: {}", "wake.max must be positive"},
		{"zero max", "max: 12", "max: 0", "wake.max must be positive"},
		{"negative max", "max: 12", "max: -1", "wake.max must be positive"},
		{"negative check timeout", "wake: {max: 12}", "wake: {max: 12, check_timeout: -1s}", "check_timeout must be positive"},
		{"bad check timeout", "wake: {max: 12}", "wake: {max: 12, check_timeout: later}", "invalid duration"},
		{"unknown wake field", "wake: {max: 12}", "wake: {max: 12, surprise: 1}", "field surprise"},
		{"duplicate wake field", "wake: {max: 12}", "wake: {max: 12, max: 3}", "duplicate field"},
		{"wake on wait", "type: wait, mode:", "type: wait, wake: {max: 1}, mode:", "not supported"},
		{"agent mode", "type: agent, stage:", "type: agent, mode: human, stage:", "not supported"},
		{"missing mode", "mode: deployment, ", "", "wait mode must be"},
		{"bad mode", "mode: deployment", "mode: timer", "wait mode must be"},
		{"missing environment", "environment: production, ", "", "requires environment"},
		{"blank environment", "environment: production", "environment: ' '", "requires environment"},
		{"nul environment", "environment: production", `environment: "prod\0uction"`, "contains NUL"},
		{"human environment", "mode: deployment", "mode: human", "cannot specify environment"},
		{"negative timeout", "timeout: 168h", "timeout: -1s", "timeout must be positive"},
		{"bad timeout", "timeout: 168h", "timeout: later", "invalid duration"},
		{"wait command", "type: wait, mode:", "type: wait, command: '', mode:", "not supported"},
		{"wait stage", "type: wait, mode:", "type: wait, stage: code, mode:", "not supported"},
		{"released required", "      - {from: {node: deploy, port: released}, to: accept}\n", "", `port "released": missing destination`},
		{"timeout required", "      - {from: {node: deploy, port: timeout}, to: stop}\n", "", `port "timeout": missing destination`},
		{"timeout without duration", ", timeout: 168h", "", `unknown port "timeout"`},
		{"unsupported port", "port: released", "port: complete", "unknown port"},
		{"duplicate release", "      - {from: {node: deploy, port: released}, to: accept}", "      - {from: {node: deploy, port: released}, to: accept}\n      - {from: {node: deploy, port: released}, to: accept}", "duplicate edge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadReader(strings.NewReader(strings.ReplaceAll(waitWorkflowConfig, tc.old, tc.replacement)))
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			node, ok := cfg.Pipelines["work"].Graph.Node("implement")
			require.True(t, ok)
			require.Equal(t, 12, node.Wake.Max)
			if tc.name == "check timeout" {
				require.Equal(t, 5*time.Second, node.Wake.CheckTimeout.Duration)
			} else {
				require.Equal(t, time.Minute, node.Wake.CheckTimeout.Duration)
			}
			data, err := yaml.Marshal(cfg)
			require.NoError(t, err)
			reloaded, err := LoadReader(strings.NewReader(string(data)))
			require.NoError(t, err)
			require.Equal(t, cfg.Pipelines, reloaded.Pipelines)
		})
	}
}

func TestWorkflowWaitNoTimeout(t *testing.T) {
	input := strings.ReplaceAll(waitWorkflowConfig, ", timeout: 168h", "")
	input = strings.ReplaceAll(input, "      - {from: {node: deploy, port: timeout}, to: stop}\n", "")
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	resolved, err := cfg.Pipelines["work"].Resolve("deploy", "ignored")
	require.NoError(t, err)
	require.Equal(t, ResolvedNode{ID: "deploy", Kind: "wait"}, resolved)
}

func TestWorkflowWaitInMemoryValidation(t *testing.T) {
	for _, node := range []WorkflowNode{
		{ID: "wait", Type: "wait", Mode: "human", Wake: &WorkflowWake{Max: 1}},
		{ID: "wait", Type: "wait", Mode: "human", Command: "true"},
		{ID: "agent", Type: "agent", Stage: "code", Agent: "claude", Environment: "prod"},
		{ID: "check", Type: "check", Command: "true", Wake: &WorkflowWake{Max: 1}},
		{ID: "end", Type: "end", Status: "done", Mode: "human"},
	} {
		require.ErrorContains(t, (&Config{}).validateWorkflowNode(node), "not supported")
	}
}

func TestWorkflowPreWaitFingerprintEncoding(t *testing.T) {
	// This is the exact encoding used before wait fields existed. Keep both the
	// field names AND previously non-omitted empty fields fingerprint-compatible.
	graph := WorkflowGraph{Version: 2, Entry: "stop", Nodes: []WorkflowNode{{ID: "stop", Type: "end", Status: "done"}}}
	const before = `{"Version":2,"Entry":"stop","Nodes":[{"ID":"stop","Type":"end","Stage":"","Agent":"","Verdicts":null,"Command":"","Timeout":{"Duration":0},"When":null,"Max":0,"Status":"done"}],"Edges":null}`
	data, err := json.Marshal(graph)
	require.NoError(t, err)
	require.Equal(t, before, string(data))
	require.Equal(t, "28f8bcd56ff0ce2c3d0e9abfdb80a93535310d180b9f3cd834a6629feb2094c6", fmt.Sprintf("%x", sha256.Sum256(data)))
	graph.Nodes[0] = WorkflowNode{ID: "stop", Type: "wait", Mode: "deployment", Environment: "prod"}
	data, err = json.Marshal(graph)
	require.NoError(t, err)
	require.Contains(t, string(data), `"mode":"deployment"`)
	require.Contains(t, string(data), `"environment":"prod"`)
	graph.Nodes[0] = WorkflowNode{ID: "stop", Type: "agent", Wake: &WorkflowWake{Max: 2, CheckTimeout: Duration{Duration: time.Minute}}}
	data, err = json.Marshal(graph)
	require.NoError(t, err)
	require.Contains(t, string(data), `"wake":{"max":2,"check_timeout":{"Duration":60000000000}}`)
}

func TestWorkflowWakeZeroCheckTimeoutEncoding(t *testing.T) {
	data, err := json.Marshal(WorkflowWake{Max: 2})
	require.NoError(t, err)
	require.JSONEq(t, `{"max":2}`, string(data))
}
