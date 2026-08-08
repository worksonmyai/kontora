package logfmt

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvents(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		assert func(t *testing.T, tape Tape)
	}{
		{
			name:  "system init emits a model banner",
			input: `{"type":"system","subtype":"init","model":"claude-opus-4-6"}`,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, "claude-opus-4-6", tape.Model)
				require.Len(t, tape.Events, 1)
				assert.Equal(t, "model", tape.Events[0].Kind)
				assert.Equal(t, "claude-opus-4-6", tape.Events[0].Model)
			},
		},
		{
			name:  "assistant model emits one banner for repeated messages",
			input: `{"type":"assistant","message":{"model":"m1","content":[{"type":"text","text":"a"}]}}` + "\n" + `{"type":"assistant","message":{"model":"m1","content":[{"type":"text","text":"b"}]}}`,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, []string{"model", "text", "text"}, kinds(tape))
			},
		},
		{
			name:  "assistant text carries timestamp and usage",
			input: `{"type":"assistant","timestamp":"2026-08-07T19:43:52Z","message":{"content":[{"type":"text","text":"Hello"}],"usage":{"input_tokens":2,"output_tokens":224,"cache_creation_input_tokens":20029,"cache_read_input_tokens":18677}}}`,
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				e := tape.Events[0]
				assert.Equal(t, "text", e.Kind)
				assert.Equal(t, "Hello", e.Text)
				require.NotNil(t, e.Time)
				assert.Equal(t, time.Date(2026, 8, 7, 19, 43, 52, 0, time.UTC), e.Time.UTC())
				require.NotNil(t, e.Usage)
				assert.Equal(t, Usage{Input: 2, Output: 224, CacheCreate: 20029, CacheRead: 18677}, *e.Usage)
				assert.Equal(t, Usage{Input: 2, Output: 224, CacheCreate: 20029, CacheRead: 18677}, tape.Totals)
			},
		},
		{
			name: "usage sums across messages",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"text","text":"a"}],"usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"b"}],"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}}`,
			}, "\n"),
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, Usage{Input: 11, Output: 22, CacheCreate: 33, CacheRead: 44}, tape.Totals)
			},
		},
		{
			name: "tool call and result land on one event",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_abc","name":"Glob","input":{"pattern":"**/*.go"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc","content":"a.go\nb.go\n"}]}}`,
			}, "\n"),
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				e := tape.Events[0]
				assert.Equal(t, "tool", e.Kind)
				assert.Equal(t, "toolu_abc", e.ID)
				assert.Equal(t, "Glob", e.Tool)
				assert.Equal(t, " **/*.go", e.Arg)
				assert.Equal(t, "Found 2 files", e.Summary)
				assert.Equal(t, "a.go\nb.go\n", e.Result)
				assert.False(t, e.IsError)
			},
		},
		{
			name: "tool result marks is_error",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x.go"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"File does not exist.","is_error":true}]}}`,
			}, "\n"),
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				assert.True(t, tape.Events[0].IsError)
				assert.Equal(t, "File does not exist.", tape.Events[0].Result)
			},
		},
		{
			name: "stream-json result attaches to the newest unanswered call",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi"}}]}}`,
				`{"type":"user","tool_name":"Bash","tool_result":"hi\n"}`,
			}, "\n"),
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				assert.Equal(t, "hi", tape.Events[0].Summary)
			},
		},
		{
			name:  "a result whose call never appeared is dropped",
			input: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"unknown","content":"orphan"}]}}`,
			assert: func(t *testing.T, tape Tape) {
				assert.Empty(t, tape.Events)
			},
		},
		{
			name: "a repeated tool_use id is emitted once",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read"}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read"}]}}`,
			}, "\n"),
			assert: func(t *testing.T, tape Tape) {
				assert.Len(t, tape.Events, 1)
			},
		},
		{
			name: "an oversized result is truncated and marked",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cat big"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + strings.Repeat("é", MaxResultBytes) + `"}]}}`,
			}, "\n"),
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				e := tape.Events[0]
				assert.True(t, e.Truncated)
				assert.LessOrEqual(t, len(e.Result), MaxResultBytes)
				assert.True(t, utf8.ValidString(e.Result), "truncated result must stay valid UTF-8")
			},
		},
		{
			name:  "a tape past the event cap is capped and marked",
			input: repeatLines(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}`, MaxTapeEvents+10),
			assert: func(t *testing.T, tape Tape) {
				assert.True(t, tape.Truncated)
				assert.Len(t, tape.Events, MaxTapeEvents)
			},
		},
		{
			name:  "unparseable lines are skipped",
			input: "not json at all",
			assert: func(t *testing.T, tape Tape) {
				assert.Empty(t, tape.Events)
				assert.False(t, tape.Truncated)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tape, err := Events(strings.NewReader(tc.input))
			require.NoError(t, err)
			assert.Equal(t, TapeVersion, tape.Version)
			assert.Equal(t, "claude", tape.Agent)
			assert.Empty(t, tape.Partial, "claude tapes fill every dimension")
			tc.assert(t, tape)
		})
	}
}

// TestEventsGolden reads a trimmed real Claude session so the verified field
// names stay in the repo: a renamed key would fail here rather than silently
// reporting null timestamps and zero tokens.
func TestEventsGolden(t *testing.T) {
	f, err := os.Open("testdata/claude-session.jsonl")
	require.NoError(t, err)
	defer f.Close()

	tape, err := Events(f)
	require.NoError(t, err)

	assert.Equal(t, "claude-opus-4-6", tape.Model)
	assert.Equal(t, Usage{Input: 9, Output: 373, CacheCreate: 20209, CacheRead: 96269}, tape.Totals)
	assert.Equal(t, []string{"model", "text", "tool", "tool"}, kinds(tape))

	text := tape.Events[1]
	require.NotNil(t, text.Time)
	assert.Equal(t, "I'll run the test suite.", text.Text)

	bash := tape.Events[2]
	assert.Equal(t, "Bash", bash.Tool)
	assert.Equal(t, " go test ./...", bash.Arg)
	assert.Equal(t, "ok  \tgithub.com/worksonmyai/kontora/internal/logfmt\t0.312s", bash.Summary)
	assert.False(t, bash.IsError)

	read := tape.Events[3]
	assert.Equal(t, "Read", read.Tool)
	assert.True(t, read.IsError)
	assert.Equal(t, "File does not exist.", read.Result)
}

func TestEventsPi(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		assert func(t *testing.T, tape Tape)
	}{
		{
			name:  "model_change emits a banner",
			input: `{"type":"model_change","modelId":"gemini-3.1-pro-high"}`,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, "gemini-3.1-pro-high", tape.Model)
				assert.Equal(t, []string{"model"}, kinds(tape))
			},
		},
		{
			name: "tool call and result land on one event",
			input: strings.Join([]string{
				`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"file_path":"/foo/bar.go"}}]}}`,
				`{"type":"message","message":{"role":"toolResult","toolName":"read","content":[{"type":"text","text":"line1\nline2\nline3\n"}]}}`,
			}, "\n"),
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				e := tape.Events[0]
				assert.Equal(t, "read", e.Tool)
				assert.Equal(t, " /foo/bar.go", e.Arg)
				assert.Equal(t, "Read 3 lines", e.Summary)
				assert.Nil(t, e.Time)
				assert.Nil(t, e.Usage)
			},
		},
		{
			name:  "thinking blocks are skipped",
			input: `{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","text":"hmm"},{"type":"text","text":"Done."}]}}`,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, []string{"text"}, kinds(tape))
			},
		},
		{
			name:  "user messages are skipped",
			input: `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"fix the bug"}]}}`,
			assert: func(t *testing.T, tape Tape) {
				assert.Empty(t, tape.Events)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tape, err := EventsPi(strings.NewReader(tc.input))
			require.NoError(t, err)
			assert.Equal(t, "pi", tape.Agent)
			assert.Equal(t, []string{PartialTime, PartialUsage, PartialIsError}, tape.Partial,
				"pi's session format verifies none of these dimensions")
			assert.Equal(t, Usage{}, tape.Totals)
			tc.assert(t, tape)
		})
	}
}

func kinds(tape Tape) []string {
	out := make([]string, len(tape.Events))
	for i, e := range tape.Events {
		out[i] = e.Kind
	}
	return out
}

func repeatLines(line string, n int) string {
	var b strings.Builder
	for range n {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
