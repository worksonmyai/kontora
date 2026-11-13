package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeGraphResult(t *testing.T) {
	for _, tt := range []struct {
		name, extra string
		valid       bool
	}{
		{"no verdict", "", true},
		{"declared verdict", `,"verdict":"changes"`, true},
		{"unicode verdict", `,"verdict":"готово 🐱"`, true},
		{"blank", `,"verdict":" "`, false},
		{"empty", `,"verdict":""`, false},
		{"null", `,"verdict":null`, false},
		{"number", `,"verdict":1`, false},
		{"duplicate", `,"verdict":"approve","verdict":"changes"`, false},
		{"escaped duplicate", `,"verdict":"approve","ver\u0064ict":"changes"`, false},
		{"case", `,"Verdict":"changes"`, false},
		{"nul", `,"verdict":"a\u0000b"`, false},
		{"surrogate", `,"verdict":"\ud800"`, false},
		{"identity override", `,"node_visit_id":"other"`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"outcome":"complete","summary":"Review finished"` + tt.extra + `}`)
			result, err := DecodeResult(payload, GraphResultVersion)
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "complete", result.Outcome)
			_, err = DecodeResult(payload)
			assert.Equal(t, tt.extra != "", err != nil)
		})
	}
	for _, version := range []int{0, -1, 3} {
		_, err := DecodeResult([]byte(`{"outcome":"complete","summary":"Done"}`), version)
		require.Error(t, err)
	}
	_, err := DecodeResult(nil, 1, 2)
	require.Error(t, err)
}

func TestValidateGraphResult(t *testing.T) {
	for _, tt := range []struct {
		name, outcome, verdict string
		declared               []string
		valid                  bool
	}{
		{"complete declared", "complete", "changes", []string{"approve", "changes"}, true},
		{"complete missing", "complete", "", []string{"approve"}, false},
		{"complete undeclared", "complete", "changes", []string{"approve"}, false},
		{"exact match", "complete", "Approve", []string{"approve"}, false},
		{"no declaration", "complete", "", nil, true},
		{"unexpected verdict", "complete", "approve", nil, false},
		{"blocked missing", "blocked", "", []string{"approve"}, true},
		{"blocked declared", "blocked", "approve", []string{"approve"}, true},
		{"blocked undeclared", "blocked", "changes", []string{"approve"}, false},
		{"blank declaration", "complete", "", []string{" "}, false},
		{"duplicate declaration", "complete", "approve", []string{"approve", "approve"}, false},
		{"invalid outcome", "success", "approve", []string{"approve"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := Result{Outcome: tt.outcome, Summary: "Done", NextStep: "Provide access", Verdict: tt.verdict}
			err := ValidateGraphResult(result, tt.declared)
			assert.Equal(t, !tt.valid, err != nil, "%v", err)
		})
	}
}

func TestGraphInvocationEnv(t *testing.T) {
	inv := Invocation{Version: GraphResultVersion, ID: "inv-1", TicketID: "kon-test", Stage: "second-review",
		ResultPath: filepath.Join(t.TempDir(), "result.json"), Verdicts: []string{"approve", "changes"},
		WorkflowExecutionID: "workflow-1", NodeVisitID: "visit-2", DefinitionFingerprint: "abc123"}
	env := inv.Env()
	assert.JSONEq(t, `["approve","changes"]`, env[ResultVerdictsEnvVar])
	got, err := InvocationFromEnv(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, inv, got)
	for _, raw := range []string{"", "null", `{}`, `[null]`, `[1]`, `[""]`, `[" "]`, `["a","a"]`, `["\ud800"]`, `["a\u0000b"]`, `["\u`, "[\"bad\xff\"]"} {
		t.Run(raw, func(t *testing.T) {
			_, err := InvocationFromEnv(func(k string) string {
				if k == ResultVerdictsEnvVar {
					return raw
				}
				return env[k]
			})
			require.Error(t, err)
		})
	}
	for _, key := range []string{WorkflowExecutionIDEnvVar, NodeVisitIDEnvVar, DefinitionFingerprintEnvVar} {
		_, err := InvocationFromEnv(func(k string) string {
			if k == key {
				return "../unsafe"
			}
			return env[k]
		})
		require.Error(t, err)
	}
	inv.Verdicts = nil
	inv.WorkflowExecutionID, inv.NodeVisitID, inv.DefinitionFingerprint = "", "", ""
	env = inv.Env()
	assert.Equal(t, "[]", env[ResultVerdictsEnvVar])
	got, err = InvocationFromEnv(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Empty(t, got.Verdicts)
	inv.Version = ResultVersion
	inv.Verdicts = []string{"stale"}
	inv.WorkflowExecutionID, inv.NodeVisitID, inv.DefinitionFingerprint = "stale", "stale", "stale"
	env = inv.Env()
	for _, key := range []string{ResultVerdictsEnvVar, WorkflowExecutionIDEnvVar, NodeVisitIDEnvVar, DefinitionFingerprintEnvVar} {
		assert.Empty(t, env[key])
	}
}

func TestPublishGraphResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	original := []byte(` {"summary":"Review finished","verdict":"changes","outcome":"complete"} `)
	require.NoError(t, os.WriteFile(path, original, 0o600))
	result := Result{Outcome: "complete", Summary: "Review finished", Verdict: "changes"}
	duplicate, err := PublishResult(path, result, GraphResultVersion)
	require.NoError(t, err)
	assert.True(t, duplicate)
	for _, verdict := range []string{"approve", ""} {
		conflict := result
		conflict.Verdict = verdict
		_, err := PublishResult(path, conflict, GraphResultVersion)
		require.ErrorContains(t, err, "conflicting")
	}
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, unchanged)
	_, err = ReadResult(path)
	require.Error(t, err)
	got, err := ReadResult(path, GraphResultVersion)
	require.NoError(t, err)
	assert.Equal(t, result, got)
	fresh := filepath.Join(t.TempDir(), "result.json")
	_, err = PublishResult(fresh, result)
	require.Error(t, err)
	assert.NoFileExists(t, fresh)
	_, err = PublishResult(fresh, result, GraphResultVersion)
	require.NoError(t, err)
	result.Verdict = strings.Repeat("v", MaxResultBytes)
	_, err = PublishResult(filepath.Join(t.TempDir(), "oversize.json"), result, GraphResultVersion)
	require.Error(t, err)
}

func TestGraphRecordRoundTrip(t *testing.T) {
	for _, graph := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "graph"}[graph], func(t *testing.T) {
			record := Record{Stage: "review", SessionID: "conversation-1"}
			if graph {
				record.WorkflowExecutionID, record.NodeVisitID, record.DefinitionFingerprint = "workflow-1", "visit-2", "abc123"
			}
			data, err := json.Marshal(record)
			require.NoError(t, err)
			if !graph {
				assert.NotContains(t, string(data), "workflow_execution_id")
				assert.NotContains(t, string(data), "node_visit_id")
				assert.NotContains(t, string(data), "definition_fingerprint")
			}
			path := filepath.Join(t.TempDir(), "review.session")
			require.NoError(t, os.WriteFile(path, data, 0o600))
			got, err := ReadRecord(path)
			require.NoError(t, err)
			assert.Equal(t, record, *got)
		})
	}
}
