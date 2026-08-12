package logfmt

import (
	"fmt"
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
	const ts = `"timestamp":"2026-08-03T18:12:46.340Z"`
	wantTime := time.Date(2026, 8, 3, 18, 12, 46, 340_000_000, time.UTC)
	// usageJSON writes the usage object a real record carries, including the two
	// keys that must not be summed.
	usageJSON := func(in, out, cacheWrite, cacheRead int) string {
		return fmt.Sprintf(`"usage":{"input":%d,"output":%d,"cacheWrite":%d,"cacheRead":%d,`+
			`"totalTokens":%d,"cacheWrite1h":%d,"reasoning":%d}`,
			in, out, cacheWrite, cacheRead, in+out+cacheWrite+cacheRead, cacheWrite, out)
	}

	cases := []struct {
		name        string
		input       string
		wantPartial []string
		assert      func(t *testing.T, tape Tape)
	}{
		{
			name:        "model_change emits a banner",
			input:       `{"type":"model_change","modelId":"gemini-3.1-pro-high",` + ts + `}`,
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, "gemini-3.1-pro-high", tape.Model)
				assert.Equal(t, []string{"model"}, kinds(tape))
			},
		},
		{
			name: "tool call and result land on one event",
			input: strings.Join([]string{
				`{"type":"message",` + ts + `,"message":{"role":"assistant",` + usageJSON(1, 2, 3, 4) + `,"content":[{"type":"toolCall","name":"read","arguments":{"file_path":"/foo/bar.go"}}]}}`,
				`{"type":"message",` + ts + `,"message":{"role":"toolResult","toolName":"read","isError":false,"content":[{"type":"text","text":"line1\nline2\nline3\n"}]}}`,
			}, "\n"),
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				e := tape.Events[0]
				assert.Equal(t, "read", e.Tool)
				assert.Equal(t, " /foo/bar.go", e.Arg)
				assert.Equal(t, "Read 3 lines", e.Summary)
				assert.False(t, e.IsError)
				require.NotNil(t, e.Time)
				assert.Equal(t, wantTime, e.Time.UTC())
				assert.Equal(t, &Usage{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4}, e.Usage)
			},
		},
		{
			name:        "thinking blocks are skipped",
			input:       `{"type":"message",` + ts + `,"message":{"role":"assistant",` + usageJSON(0, 0, 0, 0) + `,"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Done."}]}}`,
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, []string{"text"}, kinds(tape))
			},
		},
		{
			name:        "user messages are skipped",
			input:       `{"type":"message",` + ts + `,"message":{"role":"user","content":[{"type":"text","text":"fix the bug"}]}}`,
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				assert.Empty(t, tape.Events)
			},
		},
		{
			name: "usage sums across assistant records",
			input: strings.Join([]string{
				`{"type":"message",` + ts + `,"message":{"role":"assistant",` + usageJSON(2, 217, 89, 37496) + `,"content":[{"type":"text","text":"one"}]}}`,
				`{"type":"message",` + ts + `,"message":{"role":"assistant",` + usageJSON(5, 10, 15, 20) + `,"content":[{"type":"text","text":"two"}]}}`,
			}, "\n"),
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, Usage{Input: 7, Output: 227, CacheCreate: 104, CacheRead: 37516}, tape.Totals)
			},
		},
		{
			name: "a summarising call spends tokens outside a message",
			input: strings.Join([]string{
				`{"type":"compaction",` + ts + `,"summary":"...","tokensBefore":1,` + usageJSON(12, 8, 3, 5) + `}`,
				`{"type":"branch_summary",` + ts + `,"fromId":"abc","summary":"...",` + usageJSON(12, 8, 3, 5) + `}`,
			}, "\n"),
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, Usage{Input: 24, Output: 16, CacheCreate: 6, CacheRead: 10}, tape.Totals)
				assert.Empty(t, tape.Events, "summarising produces no transcript row")
			},
		},
		{
			name: "every emitted event kind carries its timestamp",
			input: strings.Join([]string{
				`{"type":"model_change","modelId":"claude-opus-5",` + ts + `}`,
				`{"type":"message",` + ts + `,"message":{"role":"assistant",` + usageJSON(1, 1, 1, 1) + `,"content":[{"type":"text","text":"hi"},{"type":"toolCall","name":"bash","arguments":{"command":"ls"}}]}}`,
			}, "\n"),
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, []string{"model", "text", "tool"}, kinds(tape))
				for _, e := range tape.Events {
					require.NotNil(t, e.Time, "kind %q", e.Kind)
					assert.Equal(t, wantTime, e.Time.UTC())
				}
			},
		},
		{
			name: "a failed tool result marks its call",
			input: strings.Join([]string{
				`{"type":"message",` + ts + `,"message":{"role":"assistant",` + usageJSON(1, 1, 1, 1) + `,"content":[{"type":"toolCall","name":"read","arguments":{"file_path":"/gone.go"}}]}}`,
				`{"type":"message",` + ts + `,"message":{"role":"toolResult","toolName":"read","isError":true,"content":[{"type":"text","text":"File does not exist."}]}}`,
			}, "\n"),
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				assert.True(t, tape.Events[0].IsError)
			},
		},
		{
			name:  "a compaction without usage declares usage partial",
			input: `{"type":"compaction",` + ts + `,"summary":"...","tokensBefore":1}`,
			// usage is optional on a compaction entry, and every session written
			// before mid-2026 leaves it off, so the tape cannot claim it counted
			// every model call.
			wantPartial: []string{PartialUsage},
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, Usage{}, tape.Totals)
			},
		},
		{
			name:  "an empty usage object counts as a measured zero",
			input: `{"type":"compaction",` + ts + `,"summary":"...","tokensBefore":1,"usage":{}}`,
			// Only a wholly absent key means "this record was not counted". A
			// present object with no figures in it is a call that spent nothing.
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, Usage{}, tape.Totals)
			},
		},
		{
			name: "a session with no measurement keys declares all three partial",
			input: strings.Join([]string{
				`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"file_path":"/foo/bar.go"}}]}}`,
				`{"type":"message","message":{"role":"toolResult","toolName":"read","content":[{"type":"text","text":"line1\n"}]}}`,
			}, "\n"),
			wantPartial: []string{PartialTime, PartialUsage, PartialIsError},
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, Usage{}, tape.Totals)
				require.Len(t, tape.Events, 1)
				assert.Nil(t, tape.Events[0].Time)
				assert.Nil(t, tape.Events[0].Usage)
				assert.False(t, tape.Events[0].IsError)
			},
		},
		{
			name: "one record without a timestamp declares time partial for the tape",
			input: strings.Join([]string{
				`{"type":"message",` + ts + `,"message":{"role":"assistant",` + usageJSON(1, 1, 1, 1) + `,"content":[{"type":"text","text":"one"}]}}`,
				`{"type":"message","message":{"role":"assistant",` + usageJSON(1, 1, 1, 1) + `,"content":[{"type":"text","text":"two"}]}}`,
			}, "\n"),
			wantPartial: []string{PartialTime},
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 2)
				assert.NotNil(t, tape.Events[0].Time)
				assert.Nil(t, tape.Events[1].Time)
			},
		},
		{
			name: "a model banner without a timestamp declares time partial",
			// The banner is the only event a model_change produces, so nothing
			// else in the tape would flag the missing timestamp.
			input:       `{"type":"model_change","modelId":"claude-opus-5"}`,
			wantPartial: []string{PartialTime},
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, []string{"model"}, kinds(tape))
			},
		},
		{
			name: "a re-typed timestamp costs the time dimension, not the record",
			// json.Unmarshal fails a record as a unit. Decoded strictly, a pi
			// version that wrote the timestamp as epoch millis would take the
			// text, the tokens and the tool result down with it.
			input:       `{"type":"message","timestamp":1754870864744,"message":{"role":"assistant",` + usageJSON(1, 2, 3, 4) + `,"content":[{"type":"text","text":"still here"}]}}`,
			wantPartial: []string{PartialTime},
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				assert.Equal(t, "still here", tape.Events[0].Text)
				assert.Nil(t, tape.Events[0].Time)
				assert.Equal(t, Usage{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4}, tape.Totals)
			},
		},
		{
			name:        "a re-typed token count costs the usage dimension, not the record",
			input:       `{"type":"message",` + ts + `,"message":{"role":"assistant","usage":{"input":"2","output":217},"content":[{"type":"text","text":"still here"}]}}`,
			wantPartial: []string{PartialUsage},
			assert: func(t *testing.T, tape Tape) {
				require.Len(t, tape.Events, 1)
				assert.Equal(t, "still here", tape.Events[0].Text)
				assert.Nil(t, tape.Events[0].Usage)
				assert.Equal(t, Usage{}, tape.Totals)
			},
		},
		{
			name: "a total that disagrees with the four categories declares usage partial",
			// pi writes its own sum on every record. A rename of one inner key
			// would decode as a silent zero, and only the checksum catches it.
			input:       `{"type":"message",` + ts + `,"message":{"role":"assistant","usage":{"input":2,"output":217,"cacheWrote":89,"cacheRead":37496,"totalTokens":37804},"content":[{"type":"text","text":"hi"}]}}`,
			wantPartial: []string{PartialUsage},
			assert: func(t *testing.T, tape Tape) {
				assert.Equal(t, Usage{}, tape.Totals, "a short count must not be reported as the run's spend")
			},
		},
		{
			name: "a file this decoder cannot read at all declares every dimension partial",
			// No event is left to judge the time dimension by, and an empty tape
			// would otherwise hand Stats a measured zero.
			input:       "not json at all\nnor is this",
			wantPartial: []string{PartialTime, PartialUsage, PartialIsError},
			assert: func(t *testing.T, tape Tape) {
				assert.Empty(t, tape.Events)
			},
		},
		{
			name:        "an assistant record with only thinking still counts its usage",
			input:       `{"type":"message",` + ts + `,"message":{"role":"assistant",` + usageJSON(1, 2, 3, 4) + `,"content":[{"type":"thinking","thinking":"hmm"}]}}`,
			wantPartial: nil,
			assert: func(t *testing.T, tape Tape) {
				assert.Empty(t, tape.Events, "thinking produces no transcript row")
				assert.Equal(t, Usage{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4}, tape.Totals,
					"the tokens are real even when no event can carry them")
			},
		},
		{
			name:        "a tool result with no open call is dropped",
			input:       `{"type":"message",` + ts + `,"message":{"role":"toolResult","toolName":"read","isError":true,"content":[{"type":"text","text":"boom"}]}}`,
			wantPartial: nil,
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
			assert.Equal(t, tc.wantPartial, tape.Partial)
			tc.assert(t, tape)
		})
	}
}

// TestEventsPiGolden reads a trimmed real pi 0.84.1 session so the verified
// field names stay in the repo: a renamed key would fail here rather than
// silently reporting null timestamps and zero tokens.
func TestEventsPiGolden(t *testing.T) {
	f, err := os.Open("testdata/pi-session.jsonl")
	require.NoError(t, err)
	defer f.Close()

	tape, err := EventsPi(f)
	require.NoError(t, err)

	assert.Equal(t, "claude-opus-5", tape.Model)
	assert.Empty(t, tape.Partial)
	// The four categories are disjoint, so they sum to the totalTokens each
	// record reports: 37804 + 52035 + 69391 + 155.
	assert.Equal(t, Usage{Input: 68598, Output: 1655, CacheCreate: 3662, CacheRead: 85470}, tape.Totals)
	assert.Equal(t, []string{"model", "text", "tool", "tool"}, kinds(tape))

	text := tape.Events[1]
	require.NotNil(t, text.Time)
	assert.Equal(t, "I'll run the test suite.", text.Text)
	// A record's usage goes to the first event it produces; every later event of
	// the same record carries none.
	assert.Equal(t, &Usage{Input: 2, Output: 217, CacheCreate: 89, CacheRead: 37496}, text.Usage)

	bash := tape.Events[2]
	assert.Equal(t, "bash", bash.Tool)
	assert.Equal(t, " go test ./...", bash.Arg)
	assert.Equal(t, "ok  \tgithub.com/worksonmyai/kontora/internal/logfmt\t0.312s", bash.Summary)
	assert.False(t, bash.IsError)
	assert.Nil(t, bash.Usage)

	read := tape.Events[3]
	assert.Equal(t, "read", read.Tool)
	assert.True(t, read.IsError)
	assert.Equal(t, "File does not exist.", read.Result)
	// Summarised through the tool's own name, so a renamed toolName key would
	// fall to the default branch and write "1 lines" instead.
	assert.Equal(t, "Read 1 lines", read.Summary)
	assert.Equal(t, &Usage{Input: 2, Output: 521, CacheCreate: 3568, CacheRead: 47944}, read.Usage)
}

func TestStableCount(t *testing.T) {
	assistant := func(blocks string) string {
		return `{"type":"assistant","message":{"content":[` + blocks + `]}}`
	}
	toolUse := func(id, name string) string {
		return `{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":{"command":"ls"}}`
	}
	toolResult := func(id string) string {
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}]}}`
	}

	cases := []struct {
		name  string
		input []string
		want  int
	}{
		{
			name:  "a tape without tools is stable to its end",
			input: []string{assistant(`{"type":"text","text":"a"}`), assistant(`{"type":"text","text":"b"}`)},
			want:  2,
		},
		{
			name: "every tool answered leaves nothing pending",
			input: []string{
				assistant(toolUse("t1", "Bash")),
				toolResult("t1"),
				assistant(toolUse("t2", "Read")),
				toolResult("t2"),
			},
			want: 2,
		},
		{
			name: "one pending tool freezes the tape at its index",
			input: []string{
				assistant(toolUse("t1", "Bash")),
				toolResult("t1"),
				assistant(toolUse("t2", "Read")),
				assistant(`{"type":"text","text":"still working"}`),
			},
			want: 1, // the answered tool; the pending one sits at index 1
		},
		{
			name: "two tools from one turn freeze at the earlier one",
			input: []string{
				assistant(toolUse("t1", "Bash") + `,` + toolUse("t2", "Read")),
				toolResult("t2"),
			},
			want: 0, // t1 is still pending even though t2 already returned
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tape, err := Events(strings.NewReader(strings.Join(tc.input, "\n")))
			require.NoError(t, err)
			assert.Equal(t, tc.want, tape.StableCount())
			assert.LessOrEqual(t, tape.StableCount(), len(tape.Events))
		})
	}

	t.Run("an empty tape is stable", func(t *testing.T) {
		assert.Equal(t, 0, Tape{}.StableCount())
	})
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
