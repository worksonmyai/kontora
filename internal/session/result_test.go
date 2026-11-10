package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeResult(t *testing.T) {
	tests := []struct {
		name, input string
		valid       bool
	}{
		{"complete", `{"outcome":"complete","summary":"Done."}`, true},
		{"blocked", `{"outcome":"blocked","summary":"No access.","next_step":"Grant access."}`, true},
		{"optional next step", `{"outcome":"complete","summary":"Done.","next_step":""}`, true},
		{"unicode", `{"outcome":"complete","summary":"Готово 🐱"}`, true},
		{"surrogate pair", `{"outcome":"complete","summary":"\ud83d\udc31"}`, true},
		{"literal escape", `{"outcome":"complete","summary":"\\ud800"}`, true},
		{"lone high surrogate", `{"outcome":"complete","summary":"\ud800"}`, false},
		{"lone low surrogate", `{"outcome":"complete","summary":"\udc00"}`, false},
		{"bad surrogate pair", `{"outcome":"complete","summary":"\ud800\u0041"}`, false},
		{"empty", ``, false},
		{"null", `null`, false},
		{"array", `[]`, false},
		{"missing outcome", `{"summary":"Done."}`, false},
		{"missing summary", `{"outcome":"complete"}`, false},
		{"blank summary", `{"outcome":"complete","summary":" \n\t"}`, false},
		{"blocked missing next", `{"outcome":"blocked","summary":"No."}`, false},
		{"blocked blank next", `{"outcome":"blocked","summary":"No.","next_step":" "}`, false},
		{"unknown outcome", `{"outcome":"success","summary":"Done."}`, false},
		{"unknown key", `{"outcome":"complete","summary":"Done.","version":1}`, false},
		{"cannot override invocation", `{"outcome":"complete","summary":"Done.","invocation_id":"other"}`, false},
		{"cannot override ticket", `{"outcome":"complete","summary":"Done.","ticket_id":"other"}`, false},
		{"wrong case", `{"Outcome":"complete","summary":"Done."}`, false},
		{"duplicate key", `{"outcome":"blocked","outcome":"complete","summary":"Done."}`, false},
		{"escaped duplicate", `{"outcome":"complete","summary":"a","summ\u0061ry":"b"}`, false},
		{"null field", `{"outcome":"complete","summary":null}`, false},
		{"null optional", `{"outcome":"complete","summary":"Done.","next_step":null}`, false},
		{"number field", `{"outcome":"complete","summary":1}`, false},
		{"nested field", `{"outcome":"complete","summary":{}}`, false},
		{"trailing object", `{"outcome":"complete","summary":"Done."}{}`, false},
		{"trailing garbage", `{"outcome":"complete","summary":"Done."}x`, false},
		{"escaped nul", `{"outcome":"complete","summary":"a\u0000b"}`, false},
		{"raw nul", "{\"outcome\":\"complete\",\"summary\":\"a\x00b\"}", false},
		{"invalid utf8", "{\"outcome\":\"complete\",\"summary\":\"\xff\"}", false},
		{"too large", `{"outcome":"complete","summary":"` + strings.Repeat("a", MaxResultBytes) + `"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeResult([]byte(tt.input))
			if !tt.valid {
				require.Error(t, err)
				assert.Equal(t, Result{}, got)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, got.Outcome)
			assert.NotEmpty(t, got.Summary)
		})
	}
	prefix, suffix := `{"outcome":"complete","summary":"`, `"}`
	exact := prefix + strings.Repeat("a", MaxResultBytes-len(prefix)-len(suffix)) + suffix
	_, err := DecodeResult([]byte(exact))
	require.NoError(t, err)
}

func TestInvocationEnv(t *testing.T) {
	inv := Invocation{Version: ResultVersion, ID: "inv-123", TicketID: "kon-test", Stage: "implement", ResultPath: filepath.Join(t.TempDir(), "result.json")}
	env := inv.Env()
	assert.Equal(t, map[string]string{
		"KONTORA_RESULT_AUTOMATION": "", "KONTORA_RESULT_RUN_NUMBER": "",
		"KONTORA_RESULT_VERSION": "1", "KONTORA_INVOCATION_ID": "inv-123", "KONTORA_RESULT_TICKET": "kon-test", "KONTORA_RESULT_STAGE": "implement", "KONTORA_RESULT_FILE": inv.ResultPath,
	}, env)
	got, err := InvocationFromEnv(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, inv, got)
	for _, stage := range []string{"implement", "code review", "проверка кода", "code\u00a0review", " code review ", "review [security]*?"} {
		t.Run("stage "+stage, func(t *testing.T) {
			stageInv := inv
			stageInv.Stage = stage
			stageEnv := stageInv.Env()
			got, err := InvocationFromEnv(func(k string) string { return stageEnv[k] })
			require.NoError(t, err)
			assert.Equal(t, stageInv, got)
		})
	}
	for key, value := range env {
		if value == "" {
			continue
		}
		t.Run("missing "+key, func(t *testing.T) {
			_, err := InvocationFromEnv(func(k string) string {
				if k == key {
					return ""
				}
				return env[k]
			})
			require.Error(t, err)
		})
	}
	for _, tt := range []struct{ key, value string }{
		{ResultVersionEnvVar, "2"}, {ResultVersionEnvVar, "01"}, {ResultVersionEnvVar, "garbage"},
		{InvocationIDEnvVar, "../old"}, {InvocationIDEnvVar, `a\b`}, {InvocationIDEnvVar, "."}, {InvocationIDEnvVar, ".."},
		{InvocationIDEnvVar, "bad\x00id"}, {InvocationIDEnvVar, "bad\xffid"}, {InvocationIDEnvVar, "bad id"}, {InvocationIDEnvVar, "bad\u00a0id"}, {InvocationIDEnvVar, "bad\nid"},
		{InvocationIDEnvVar, "inv*"}, {InvocationIDEnvVar, "inv?"}, {InvocationIDEnvVar, "inv[1]"},
		{ResultTicketEnvVar, "../ticket"}, {ResultTicketEnvVar, `a\b`}, {ResultTicketEnvVar, "."}, {ResultTicketEnvVar, ".."},
		{ResultTicketEnvVar, "bad\x00id"}, {ResultTicketEnvVar, "bad\xffid"}, {ResultTicketEnvVar, "bad id"}, {ResultTicketEnvVar, "bad\u00a0id"}, {ResultTicketEnvVar, "bad\nid"},
		{ResultTicketEnvVar, "kon-*"}, {ResultTicketEnvVar, "kon-?"}, {ResultTicketEnvVar, "kon-[1]"},
		{ResultStageEnvVar, "."}, {ResultStageEnvVar, ".."}, {ResultStageEnvVar, "a/b"}, {ResultStageEnvVar, `a\b`}, {ResultStageEnvVar, "../review"}, {ResultStageEnvVar, "/review"},
		{ResultStageEnvVar, " "}, {ResultStageEnvVar, "\t\n"}, {ResultStageEnvVar, "bad\x00stage"}, {ResultStageEnvVar, "bad\xffstage"},
		{ResultFileEnvVar, "relative.json"}, {ResultFileEnvVar, "/"}, {ResultFileEnvVar, "/tmp/../result"}, {ResultFileEnvVar, "/tmp/bad\x00"},
	} {
		t.Run(fmt.Sprintf("%s=%q", tt.key, tt.value), func(t *testing.T) {
			_, err := InvocationFromEnv(func(k string) string {
				if k == tt.key {
					return tt.value
				}
				return env[k]
			})
			require.Error(t, err)
		})
	}
	_, err = InvocationFromEnv(nil)
	require.Error(t, err)
}

func TestAutomationInvocationEnv(t *testing.T) {
	inv := Invocation{Version: ResultVersion, ID: "inv-automation-1", AutomationID: "auto-123", RunNumber: 42, ResultPath: filepath.Join(t.TempDir(), "result.json")}
	env := inv.Env()
	got, err := InvocationFromEnv(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, inv, got)
	assert.Equal(t, "auto-123", got.SubjectID())
	assert.Empty(t, env[ResultTicketEnvVar])
	assert.Empty(t, env[ResultStageEnvVar])
	assert.Equal(t, "42", env[ResultRunNumberEnvVar])
	for _, tt := range []struct{ key, value string }{
		{ResultAutomationEnvVar, ""}, {ResultAutomationEnvVar, "../auto"}, {ResultAutomationEnvVar, "bad id"},
		{ResultRunNumberEnvVar, ""}, {ResultRunNumberEnvVar, "0"}, {ResultRunNumberEnvVar, "01"},
		{ResultRunNumberEnvVar, "-1"}, {ResultRunNumberEnvVar, "+1"}, {ResultRunNumberEnvVar, "18446744073709551616"},
		{ResultTicketEnvVar, "fake-ticket"}, {ResultStageEnvVar, "fake-stage"},
	} {
		t.Run(fmt.Sprintf("%s=%q", tt.key, tt.value), func(t *testing.T) {
			_, err := InvocationFromEnv(func(k string) string {
				if k == tt.key {
					return tt.value
				}
				return env[k]
			})
			require.Error(t, err)
		})
	}
	assert.Equal(t, "ticket", (Invocation{TicketID: "ticket"}).SubjectID())
}

func TestPublishResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invocation", "result.json")
	result := Result{Outcome: "blocked", Summary: "No access.", NextStep: "Grant access."}
	duplicate, err := PublishResult(path, result)
	require.NoError(t, err)
	assert.False(t, duplicate)
	got, err := ReadResult(path)
	require.NoError(t, err)
	assert.Equal(t, result, got)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	duplicate, err = PublishResult(path, result)
	require.NoError(t, err)
	assert.True(t, duplicate)
	_, err = PublishResult(path, Result{Outcome: "complete", Summary: "Changed."})
	require.Error(t, err)
	got, err = ReadResult(path)
	require.NoError(t, err)
	assert.Equal(t, result, got)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestPublishResultMaximumSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	overhead := len(`{"outcome":"complete","summary":""}`)
	result := Result{Outcome: "complete", Summary: strings.Repeat("a", MaxResultBytes-overhead)}
	_, err := PublishResult(path, result)
	require.NoError(t, err)
	got, err := ReadResult(path)
	require.NoError(t, err)
	assert.Equal(t, result, got)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.EqualValues(t, MaxResultBytes, info.Size())
}

func TestPublishResultDecodedDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	original := []byte(" {\n\"summary\":\"Done.\",\"outcome\":\"complete\",\"next_step\":\"\"}\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	duplicate, err := PublishResult(path, Result{Outcome: "complete", Summary: "Done."})
	require.NoError(t, err)
	assert.True(t, duplicate)
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, unchanged)
}

func TestResultFilesRejectInvalid(t *testing.T) {
	for _, input := range []string{`{}`, strings.Repeat("a", MaxResultBytes+1)} {
		t.Run(fmt.Sprint(len(input)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			require.NoError(t, os.WriteFile(path, []byte(input), 0o600))
			_, err := ReadResult(path)
			require.Error(t, err)
			_, err = PublishResult(path, Result{Outcome: "complete", Summary: "Done."})
			require.Error(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, input, string(data))
		})
	}
	path := filepath.Join(t.TempDir(), "result.json")
	for _, result := range []Result{{}, {Outcome: "complete", Summary: "bad\xff"}, {Outcome: "complete", Summary: "bad\x00"}, {Outcome: "complete", Summary: strings.Repeat("a", MaxResultBytes)}} {
		_, err := PublishResult(path, result)
		require.Error(t, err)
		assert.NoFileExists(t, path)
	}
	_, err := ReadResult(filepath.Join(t.TempDir(), "missing.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = ReadResult(t.TempDir())
	require.Error(t, err)
	for _, path := range []string{"", "relative.json", "/"} {
		_, err := ReadResult(path)
		require.Error(t, err)
		_, err = PublishResult(path, Result{Outcome: "complete", Summary: "Done."})
		require.Error(t, err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	data, err := json.Marshal(Result{Outcome: "complete", Summary: "Done."})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(target, data, 0o600))
	require.NoError(t, os.Symlink(target, path))
	_, err = ReadResult(path)
	require.Error(t, err)
	_, err = PublishResult(path, Result{Outcome: "complete", Summary: "Done."})
	require.Error(t, err)
}

func TestPublishResultConcurrent(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			const n = 24
			var wg sync.WaitGroup
			type submission struct {
				result    Result
				duplicate bool
				err       error
			}
			submissions := make([]submission, n)
			for i := range n {
				wg.Go(func() {
					result := Result{Outcome: "complete", Summary: "Done."}
					if conflict {
						result.Summary = fmt.Sprint(i)
					}
					duplicate, err := PublishResult(path, result)
					submissions[i] = submission{result, duplicate, err}
				})
			}
			wg.Wait()
			winner, err := ReadResult(path)
			require.NoError(t, err)
			published := 0
			for _, s := range submissions {
				if s.err == nil {
					assert.Equal(t, winner, s.result)
					if !s.duplicate {
						published++
					}
				} else {
					require.True(t, conflict)
				}
			}
			assert.Equal(t, 1, published)
		})
	}
}
