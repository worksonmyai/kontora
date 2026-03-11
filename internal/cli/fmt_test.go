package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFmt(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "system init prints model name",
			input: `{"type":"system","subtype":"init","model":"claude-opus-4-6"}`,
			want:  "[claude-opus-4-6]\n",
		},
		{
			name:  "assistant text block",
			input: `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`,
			want:  "Hello world\n",
		},
		{
			name:  "assistant text block multiline",
			input: `{"type":"assistant","message":{"content":[{"type":"text","text":"line1\nline2"}]}}`,
			want:  "line1\nline2\n",
		},
		{
			name:  "tool_use prints name with > prefix",
			input: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Read"}]}}`,
			want:  "> Read\n",
		},
		{
			name:  "tool_use with file_path argument",
			input: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}`,
			want:  "> Read /tmp/foo.go\n",
		},
		{
			name:  "tool_use Bash with command",
			input: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Bash","input":{"command":"go test ./..."}}]}}`,
			want:  "> Bash go test ./...\n",
		},
		{
			name:  "tool_use Bash truncates long command",
			input: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Bash","input":{"command":"` + strings.Repeat("x", 100) + `"}}]}}`,
			want:  "> Bash " + strings.Repeat("x", 80) + "...\n",
		},
		{
			name:  "tool_use Glob with pattern",
			input: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Glob","input":{"pattern":"**/*.go"}}]}}`,
			want:  "> Glob **/*.go\n",
		},
		{
			name:  "tool_use Grep with pattern",
			input: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Grep","input":{"pattern":"formatTool"}}]}}`,
			want:  "> Grep formatTool\n",
		},
		{
			name:  "tool_use Edit with file_path",
			input: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Edit","input":{"file_path":"/tmp/foo.go","old_string":"a","new_string":"b"}}]}}`,
			want:  "> Edit /tmp/foo.go\n",
		},
		{
			name: "tool_use deduplicated by block ID",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Read"}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Read"}]}}`,
			}, "\n"),
			want: "> Read\n",
		},
		{
			name: "different tool_use IDs not deduplicated",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Read"}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Read"}]}}`,
			}, "\n"),
			want: "> Read\n> Read\n",
		},
		{
			name:  "thinking blocks skipped",
			input: `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"let me think"}]}}`,
			want:  "",
		},
		{
			name:  "user message without tool_name skipped",
			input: `{"type":"user"}`,
			want:  "",
		},
		{
			name:  "user tool result Read",
			input: `{"type":"user","tool_name":"Read","tool_result":"line1\nline2\nline3\n"}`,
			want:  "  ⎿  Read 3 lines\n",
		},
		{
			name:  "user tool result Bash single line",
			input: `{"type":"user","tool_name":"Bash","tool_result":"OK\n"}`,
			want:  "  ⎿  OK\n",
		},
		{
			name:  "user tool result Bash multi line",
			input: `{"type":"user","tool_name":"Bash","tool_result":"first\nsecond\nthird\n"}`,
			want:  "  ⎿  first (+2 more lines)\n",
		},
		{
			name:  "user tool result Bash no output",
			input: `{"type":"user","tool_name":"Bash","tool_result":""}`,
			want:  "",
		},
		{
			name:  "user tool result Glob",
			input: `{"type":"user","tool_name":"Glob","tool_result":"a.go\nb.go\n"}`,
			want:  "  ⎿  Found 2 files\n",
		},
		{
			name:  "user tool result Grep no matches",
			input: `{"type":"user","tool_name":"Grep","tool_result":"\n"}`,
			want:  "  ⎿  No matches found\n",
		},
		{
			name:  "user tool result Write",
			input: `{"type":"user","tool_name":"Write","tool_result":"done"}`,
			want:  "  ⎿  File written\n",
		},
		{
			name:  "user tool result Edit",
			input: `{"type":"user","tool_name":"Edit","tool_result":"done"}`,
			want:  "  ⎿  File updated\n",
		},
		{
			name:  "rate_limit_event skipped",
			input: `{"type":"rate_limit_event"}`,
			want:  "",
		},
		{
			name:  "result skipped",
			input: `{"type":"result","subtype":"success","total_cost_usd":0.5}`,
			want:  "",
		},
		{
			name:  "system ticket_started skipped",
			input: `{"type":"system","subtype":"ticket_started"}`,
			want:  "",
		},
		{
			name:  "unparseable lines skipped",
			input: "not json at all",
			want:  "",
		},
		{
			name:  "empty lines skipped",
			input: "\n\n",
			want:  "",
		},
		{
			name: "mixed content with tool args and results",
			input: strings.Join([]string{
				`{"type":"system","subtype":"init","model":"claude-sonnet-4-6"}`,
				`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"I will read the file."}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_abc","name":"Bash","input":{"command":"go test ./..."}}]}}`,
				`{"type":"user","tool_name":"Bash","tool_result":"PASS\n"}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}`,
				`{"type":"result","subtype":"success"}`,
			}, "\n"),
			want: "[claude-sonnet-4-6]\nI will read the file.\n> Bash go test ./...\n  ⎿  PASS\nDone.\n",
		},
		{
			name:  "multiple content blocks in one message",
			input: `{"type":"assistant","message":{"content":[{"type":"text","text":"First"},{"type":"tool_use","id":"toolu_x","name":"Edit","input":{"file_path":"/tmp/x.go"}}]}}`,
			want:  "First\n> Edit /tmp/x.go\n",
		},
		// Session JSONL format test cases.
		{
			name: "session JSONL tool result with string content",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_abc","name":"Glob","input":{"pattern":"**/*.go"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc","content":"a.go\nb.go\n"}]}}`,
			}, "\n"),
			want: "> Glob **/*.go\n  ⎿  Found 2 files\n",
		},
		{
			name: "session JSONL tool result with array content",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_abc","name":"Bash","input":{"command":"echo hello"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc","content":[{"type":"text","text":"hello\n"}]}]}}`,
			}, "\n"),
			want: "> Bash echo hello\n  ⎿  hello\n",
		},
		{
			name:  "session JSONL unknown tool_use_id falls back to default summary",
			input: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_unknown","content":"some result"}]}}`,
			want:  "  ⎿  some result\n",
		},
		{
			name: "session JSONL mixed with stream-json",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/tmp/f.go"}}]}}`,
				`{"type":"user","tool_name":"Read","tool_result":"line1\nline2\n"}`,
				`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"/tmp/f.go"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"done"}]}}`,
			}, "\n"),
			want: "> Read /tmp/f.go\n  ⎿  Read 2 lines\n> Edit /tmp/f.go\n  ⎿  File updated\n",
		},
		{
			name: "session JSONL progress and queue-operation entries skipped",
			input: strings.Join([]string{
				`{"type":"progress","data":{"type":"agent_progress"}}`,
				`{"type":"queue-operation","operation":"enqueue"}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}`,
			}, "\n"),
			want: "Hello\n",
		},
		{
			name:  "session JSONL user text message skipped",
			input: `{"type":"user","message":{"role":"user","content":"how are you?"}}`,
			want:  "",
		},
		{
			name:  "session JSONL file-history-snapshot skipped",
			input: `{"type":"file-history-snapshot","messageId":"abc"}`,
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, Fmt(strings.NewReader(tc.input), &buf))
			assert.Equal(t, tc.want, buf.String())
		})
	}
}

func TestFmtLargeLines(t *testing.T) {
	bigText := strings.Repeat("x", 900_000)
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + bigText + `"}]}}`

	var buf bytes.Buffer
	require.NoError(t, Fmt(strings.NewReader(input), &buf))
	assert.Contains(t, buf.String(), bigText)
}

func TestFmtReadsUntilEOF(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"early"}]}}`,
		`{"type":"user"}`,
		`{"type":"user"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"late"}]}}`,
	}, "\n")

	var buf bytes.Buffer
	require.NoError(t, Fmt(strings.NewReader(input), &buf))
	assert.Contains(t, buf.String(), "late")
}
