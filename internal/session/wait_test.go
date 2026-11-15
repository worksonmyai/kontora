package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const checkWaitJSON = `{"kind":"check","command":"test -f DONE","poll":60000000000,"deadline":"2030-01-01T01:00:00Z"}`
const timerWaitJSON = `{"kind":"timer","until":"2030-01-01T01:00:00Z"}`

func waitingResult(t *testing.T) Result {
	t.Helper()
	result, err := DecodeResult([]byte(`{"outcome":"waiting","summary":"Collecting data","next_step":"Read collected data","wait":`+checkWaitJSON+`}`), WaitResultVersion)
	require.NoError(t, err)
	return result
}

func waitInvocation(t *testing.T) Invocation {
	t.Helper()
	return Invocation{Version: WaitResultVersion, ID: "inv-1", TicketID: "kon-test", Stage: "code", ResultPath: filepath.Join(t.TempDir(), "result.json"),
		WorkflowExecutionID: "execution-1", NodeVisitID: "visit-1", DefinitionFingerprint: "abc123", Verdicts: []string{"approve"}, WakeMax: 12, CheckTimeout: time.Minute}
}

func TestDecodeWaitRequest(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"timer", timerWaitJSON, true},
		{"check", checkWaitJSON, true},
		{"escaped key", strings.Replace(checkWaitJSON, "command", `comm\u0061nd`, 1), true},
		{"unicode", strings.Replace(checkWaitJSON, "test -f DONE", `echo \ud83d\udc31`, 1), true},
		{"past timer", strings.Replace(timerWaitJSON, "2030", "2000", 1), true},
		{"offset timestamp", strings.Replace(timerWaitJSON, "01:00:00Z", "01:00:00+02:00", 1), true},
		{"fractional timestamp", strings.Replace(timerWaitJSON, "01:00:00Z", "01:00:00.123456789Z", 1), true},
		{"null", `null`, false},
		{"array", `[]`, false},
		{"string", `"wait"`, false},
		{"empty", `{}`, false},
		{"unknown", strings.Replace(timerWaitJSON, "until", "Until", 1), false},
		{"extra", strings.Replace(timerWaitJSON, `"kind"`, `"extra":true,"kind"`, 1), false},
		{"duplicate", strings.Replace(timerWaitJSON, `"kind"`, `"kind":"timer","kind"`, 1), false},
		{"escaped duplicate", strings.Replace(checkWaitJSON, `"command"`, `"comm\u0061nd":"true","command"`, 1), false},
		{"missing until", `{"kind":"timer"}`, false},
		{"null kind", strings.Replace(timerWaitJSON, `"timer"`, `null`, 1), false},
		{"number kind", strings.Replace(timerWaitJSON, `"timer"`, `1`, 1), false},
		{"boolean until", strings.Replace(timerWaitJSON, `"2030-01-01T01:00:00Z"`, `true`, 1), false},
		{"null until", strings.Replace(timerWaitJSON, `"2030-01-01T01:00:00Z"`, `null`, 1), false},
		{"relative until", strings.Replace(timerWaitJSON, "2030-01-01T01:00:00Z", "tomorrow", 1), false},
		{"timezone missing", strings.Replace(timerWaitJSON, "01:00:00Z", "01:00:00", 1), false},
		{"zero until", strings.Replace(timerWaitJSON, "2030", "0001", 1), true},
		{"zero time", strings.Replace(timerWaitJSON, "2030-01-01T01:00:00Z", "0001-01-01T00:00:00Z", 1), false},
		{"timer command", strings.Replace(timerWaitJSON, `"kind"`, `"command":"","kind"`, 1), false},
		{"timer poll", strings.Replace(timerWaitJSON, `"kind"`, `"poll":0,"kind"`, 1), false},
		{"timer deadline", strings.Replace(timerWaitJSON, `"kind"`, `"deadline":"2030-01-01T02:00:00Z","kind"`, 1), false},
		{"check until", strings.Replace(checkWaitJSON, `"kind"`, `"until":"2030-01-01T02:00:00Z","kind"`, 1), false},
		{"missing check fields", `{"kind":"check"}`, false},
		{"blank command", strings.Replace(checkWaitJSON, "test -f DONE", " ", 1), false},
		{"null command", strings.Replace(checkWaitJSON, `"test -f DONE"`, `null`, 1), false},
		{"object command", strings.Replace(checkWaitJSON, `"test -f DONE"`, `{}`, 1), false},
		{"nul command", strings.Replace(checkWaitJSON, "test -f DONE", `echo \u0000`, 1), false},
		{"bad utf8", strings.Replace(checkWaitJSON, "test -f DONE", "bad\xff", 1), false},
		{"high surrogate", strings.Replace(checkWaitJSON, "test -f DONE", `\ud800`, 1), false},
		{"low surrogate", strings.Replace(checkWaitJSON, "test -f DONE", `\udfff`, 1), false},
		{"missing deadline", strings.Replace(checkWaitJSON, `,"deadline":"2030-01-01T01:00:00Z"`, "", 1), false},
		{"null deadline", strings.Replace(checkWaitJSON, `"2030-01-01T01:00:00Z"`, `null`, 1), false},
		{"zero poll", strings.Replace(checkWaitJSON, "60000000000", "0", 1), false},
		{"negative poll", strings.Replace(checkWaitJSON, "60000000000", "-1", 1), false},
		{"null poll", strings.Replace(checkWaitJSON, "60000000000", "null", 1), false},
		{"string poll", strings.Replace(checkWaitJSON, "60000000000", `"1m"`, 1), false},
		{"float poll", strings.Replace(checkWaitJSON, "60000000000", "1.1", 1), false},
		{"exponent poll", strings.Replace(checkWaitJSON, "60000000000", "1e9", 1), false},
		{"overflow poll", strings.Replace(checkWaitJSON, "60000000000", "9223372036854775808", 1), false},
		{"trailing object", timerWaitJSON + `{}`, false},
		{"truncated", timerWaitJSON[:len(timerWaitJSON)-1], false},
		{"oversize", strings.Replace(checkWaitJSON, "test -f DONE", strings.Repeat("x", MaxResultBytes), 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request WaitRequest
			err := json.Unmarshal([]byte(tc.input), &request)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NoError(t, ValidateWaitRequest(request))
			data, err := json.Marshal(request)
			require.NoError(t, err)
			var decoded WaitRequest
			require.NoError(t, json.Unmarshal(data, &decoded))
			require.Equal(t, request, decoded)
		})
	}
}

func TestValidateWaitRequestProgrammatic(t *testing.T) {
	at := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, request := range []WaitRequest{
		{}, {Kind: "human"}, {Kind: "timer"},
		{Kind: "timer", Until: at, Command: "true"},
		{Kind: "timer", Until: at, Poll: time.Second},
		{Kind: "timer", Until: at, Deadline: at},
		{Kind: "check", Command: "true", Poll: time.Second},
		{Kind: "check", Command: "true", Deadline: at},
		{Kind: "check", Command: "true", Poll: -time.Second, Deadline: at},
		{Kind: "check", Command: "bad\x00command", Poll: time.Second, Deadline: at},
		{Kind: "check", Command: "true", Poll: time.Second, Deadline: at, Until: at},
	} {
		require.Error(t, ValidateWaitRequest(request), "%+v", request)
	}
	timer := Result{Outcome: "waiting", Summary: "Pending", NextStep: "Continue", Wait: &WaitRequest{Kind: "timer", Until: at}}
	require.NoError(t, ValidateWaitResult(timer, nil))
	other := timer
	other.Wait = &WaitRequest{Kind: "timer", Until: at.In(time.FixedZone("other", 7200))}
	require.True(t, ResultsEqual(timer, other))
	other.Wait.Until = other.Wait.Until.Add(time.Second)
	require.False(t, ResultsEqual(timer, other))
	other.Wait = nil
	require.False(t, ResultsEqual(timer, other))
	require.False(t, ResultsEqual(other, timer))
	require.True(t, ResultsEqual(other, other))
	other.Outcome = "waiting"
	require.Error(t, ValidateWaitResult(other, nil))
	other = timer
	other.Verdict = "approve"
	require.Error(t, ValidateWaitResult(other, []string{"approve"}))
}

func TestDecodeWaitingResult(t *testing.T) {
	valid := `{"outcome":"waiting","summary":"Pending","next_step":"Continue","wait":` + timerWaitJSON + `}`
	for _, tc := range []struct{ name, input string }{
		{"missing next", strings.Replace(valid, `,"next_step":"Continue"`, "", 1)},
		{"blank next", strings.Replace(valid, "Continue", " ", 1)},
		{"missing summary", strings.Replace(valid, `,"summary":"Pending"`, "", 1)},
		{"missing wait", `{"outcome":"waiting","summary":"Pending","next_step":"Continue"}`},
		{"null wait", strings.Replace(valid, timerWaitJSON, `null`, 1)},
		{"verdict", strings.Replace(valid, `"outcome"`, `"verdict":"approve","outcome"`, 1)},
		{"empty verdict", strings.Replace(valid, `"outcome"`, `"verdict":"","outcome"`, 1)},
		{"complete wait", strings.Replace(valid, "waiting", "complete", 1)},
		{"blocked wait", strings.Replace(valid, "waiting", "blocked", 1)},
		{"duplicate wait", strings.Replace(valid, `"wait":`, `"wait":`+timerWaitJSON+`,"wait":`, 1)},
		{"escaped duplicate wait", strings.Replace(valid, `"wait":`, `"w\u0061it":`+timerWaitJSON+`,"wait":`, 1)},
		{"unknown top field", strings.Replace(valid, `"outcome"`, `"execution_id":"other","outcome"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeResult([]byte(tc.input), WaitResultVersion)
			require.Error(t, err)
		})
	}
	result, err := DecodeResult([]byte(valid), WaitResultVersion)
	require.NoError(t, err)
	require.NoError(t, ValidateWaitResult(result, []string{"approve"}))
	require.Error(t, ValidateGraphResult(result, nil))
	for _, version := range []int{ResultVersion, GraphResultVersion} {
		_, err := DecodeResult([]byte(valid), version)
		require.Error(t, err)
		_, err = PublishResult(filepath.Join(t.TempDir(), "result.json"), result, version)
		require.Error(t, err)
	}
	for _, outcome := range []string{"complete", "blocked"} {
		ordinary := Result{Outcome: outcome, Summary: "Finished", NextStep: "Next", Verdict: "approve"}
		require.NoError(t, ValidateWaitResult(ordinary, []string{"approve"}))
		data, err := json.Marshal(ordinary)
		require.NoError(t, err)
		_, err = DecodeResult(data, WaitResultVersion)
		require.NoError(t, err)
	}
}

func TestWaitInvocationCapabilities(t *testing.T) {
	inv := waitInvocation(t)
	env := inv.Env()
	require.Equal(t, "12", env[WakeMaxEnvVar])
	require.Equal(t, "1m0s", env[CheckTimeoutEnvVar])
	got, err := InvocationFromEnv(func(k string) string { return env[k] })
	require.NoError(t, err)
	require.Equal(t, inv, got)
	result := waitingResult(t)
	require.NoError(t, ValidateInvocationResult(result, inv))
	for _, tc := range []struct{ key, value string }{
		{WakeMaxEnvVar, ""}, {WakeMaxEnvVar, "0"}, {WakeMaxEnvVar, "-1"}, {WakeMaxEnvVar, "01"}, {WakeMaxEnvVar, "+1"}, {WakeMaxEnvVar, "1.5"}, {WakeMaxEnvVar, "99999999999999999999999"},
		{CheckTimeoutEnvVar, ""}, {CheckTimeoutEnvVar, "0s"}, {CheckTimeoutEnvVar, "-1s"}, {CheckTimeoutEnvVar, "60"}, {CheckTimeoutEnvVar, "forever"},
		{WorkflowExecutionIDEnvVar, ""}, {NodeVisitIDEnvVar, ""}, {DefinitionFingerprintEnvVar, ""}, {ResultTicketEnvVar, ""},
		{ResultAutomationEnvVar, "automation"}, {ResultRunNumberEnvVar, "1"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			_, err := InvocationFromEnv(func(k string) string {
				if k == tc.key {
					return tc.value
				}
				return env[k]
			})
			require.Error(t, err)
		})
	}
	for _, version := range []int{ResultVersion, GraphResultVersion} {
		old := inv
		old.Version = version
		oldEnv := old.Env()
		require.Empty(t, oldEnv[WakeMaxEnvVar])
		require.Empty(t, oldEnv[CheckTimeoutEnvVar])
		for _, capability := range []string{WakeMaxEnvVar, CheckTimeoutEnvVar} {
			_, err := InvocationFromEnv(func(k string) string {
				if k == capability {
					return env[k]
				}
				return oldEnv[k]
			})
			require.Error(t, err)
		}
		require.Error(t, ValidateInvocationResult(Result{Outcome: "complete", Summary: "done"}, old))
		require.Error(t, ValidateInvocationResult(result, old))
	}
	for _, change := range []func(*Invocation){
		func(i *Invocation) { i.WakeMax = 0 },
		func(i *Invocation) { i.WakeMax = -1 },
		func(i *Invocation) { i.CheckTimeout = 0 },
		func(i *Invocation) { i.CheckTimeout = -time.Second },
		func(i *Invocation) { i.WorkflowExecutionID = "" },
		func(i *Invocation) { i.NodeVisitID = "" },
		func(i *Invocation) { i.DefinitionFingerprint = "" },
		func(i *Invocation) { i.AutomationID = "auto" },
		func(i *Invocation) { i.RunNumber = 1 },
		func(i *Invocation) { i.TicketID = "" },
	} {
		bad := inv
		change(&bad)
		require.Error(t, ValidateInvocationResult(result, bad))
	}
	require.Error(t, ValidateInvocationResult(Result{Outcome: "complete", Summary: "done"}, inv), "complete still requires declared verdict")
	require.NoError(t, ValidateInvocationResult(Result{Outcome: "complete", Summary: "done", Verdict: "approve"}, inv))
}

func TestWaitPublicationSemanticEquality(t *testing.T) {
	result := waitingResult(t)
	path := filepath.Join(t.TempDir(), "result.json")
	duplicate, err := PublishResult(path, result, WaitResultVersion)
	require.NoError(t, err)
	require.False(t, duplicate)
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	decoded, err := ReadResult(path, WaitResultVersion)
	require.NoError(t, err)
	require.NotSame(t, result.Wait, decoded.Wait)
	decoded.Wait.Deadline = decoded.Wait.Deadline.In(time.FixedZone("other", 7200))
	require.True(t, ResultsEqual(result, decoded))
	duplicate, err = PublishResult(path, decoded, WaitResultVersion)
	require.NoError(t, err)
	require.True(t, duplicate)
	for _, change := range []func(*Result){
		func(r *Result) { r.Summary = "changed" },
		func(r *Result) { r.NextStep = "changed" },
		func(r *Result) { r.Wait.Command = "true" },
		func(r *Result) { r.Wait.Poll++ },
		func(r *Result) { r.Wait.Deadline = r.Wait.Deadline.Add(time.Second) },
	} {
		conflict, err := ReadResult(path, WaitResultVersion)
		require.NoError(t, err)
		change(&conflict)
		require.False(t, ResultsEqual(result, conflict))
		_, err = PublishResult(path, conflict, WaitResultVersion)
		require.ErrorContains(t, err, "conflicting")
	}
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, unchanged)
	assert.False(t, ResultsEqual(result, Result{Outcome: result.Outcome, Summary: result.Summary, NextStep: result.NextStep}))

	// Physical concurrent submissions are either identical or conflicting, never
	// replacement. This exercises decoded-pointer equality on the EEXIST path.
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			duplicate, err := PublishResult(path, result, WaitResultVersion)
			assert.NoError(t, err)
			assert.True(t, duplicate)
		})
	}
	wg.Wait()
}
