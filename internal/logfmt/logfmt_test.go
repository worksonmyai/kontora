package logfmt

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
			name:  "tool_use prints name with > prefix",
			input: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Read"}]}}`,
			want:  "> Read\n",
		},
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
			name:  "unparseable lines skipped",
			input: "not json at all",
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

func TestFmtPi(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "model_change prints modelId",
			input: `{"type":"model_change","modelId":"gemini-3.1-pro-high"}`,
			want:  "[gemini-3.1-pro-high]\n",
		},
		{
			name:  "assistant text block",
			input: `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Hello world"}]}}`,
			want:  "Hello world\n",
		},
		{
			name:  "assistant toolCall with lowercase name",
			input: `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
			want:  "> bash go test ./...\n",
		},
		{
			name: "toolResult with toolName and content text blocks",
			input: strings.Join([]string{
				`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"file_path":"/foo/bar.go"}}]}}`,
				`{"type":"message","message":{"role":"toolResult","toolName":"read","content":[{"type":"text","text":"line1\nline2\nline3\n"}]}}`,
			}, "\n"),
			want: "> read /foo/bar.go\n  ⎿  Read 3 lines\n",
		},
		{
			name:  "user message is skipped",
			input: `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"fix the bug"}]}}`,
			want:  "",
		},
		{
			name:  "thinking block is skipped",
			input: `{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","text":"let me think..."},{"type":"text","text":"Done."}]}}`,
			want:  "Done.\n",
		},
		{
			name:  "session header is skipped",
			input: `{"type":"session","id":"sess-123"}`,
			want:  "",
		},
		{
			name: "full conversation",
			input: strings.Join([]string{
				`{"type":"model_change","modelId":"opus-4"}`,
				`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"fix tests"}]}}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"I'll check the tests."}]}}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"go test ./..."}}]}}`,
				`{"type":"message","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"PASS\n"}]}}`,
				`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"All tests pass."}]}}`,
			}, "\n"),
			want: "[opus-4]\nI'll check the tests.\n> bash go test ./...\n  ⎿  PASS\nAll tests pass.\n",
		},
		{
			name:  "unparseable lines skipped",
			input: "not json at all",
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, FmtPi(strings.NewReader(tc.input), &buf))
			assert.Equal(t, tc.want, buf.String())
		})
	}
}

func TestFormatToolArg(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		input    map[string]any
		want     string
	}{
		{"nil input", "Read", nil, ""},
		{"Read with path", "Read", map[string]any{"file_path": "/foo/bar.go"}, " /foo/bar.go"},
		{"Write with path", "Write", map[string]any{"file_path": "/foo/bar.go"}, " /foo/bar.go"},
		{"Edit with path", "Edit", map[string]any{"file_path": "/foo/bar.go", "old_string": "a", "new_string": "b"}, " /foo/bar.go"},
		{"Bash short", "Bash", map[string]any{"command": "ls"}, " ls"},
		{"Bash truncated", "Bash", map[string]any{"command": strings.Repeat("a", 100)}, " " + strings.Repeat("a", 80) + "..."},
		{"Glob", "Glob", map[string]any{"pattern": "**/*.go"}, " **/*.go"},
		{"Grep", "Grep", map[string]any{"pattern": "TODO"}, " TODO"},
		{"Ticket", "Ticket", map[string]any{"description": "do stuff"}, " do stuff"},
		{"unknown tool", "WebSearch", map[string]any{"query": "foo"}, ""},
		{"missing key", "Read", map[string]any{"other": "val"}, ""},
		{"lowercase bash (pi)", "bash", map[string]any{"command": "ls"}, " ls"},
		{"lowercase read (pi)", "read", map[string]any{"file_path": "/foo.go"}, " /foo.go"},
		{"lowercase edit (pi)", "edit", map[string]any{"file_path": "/bar.go"}, " /bar.go"},
		{"lowercase write (pi)", "write", map[string]any{"file_path": "/baz.go"}, " /baz.go"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatToolArg(tc.toolName, tc.input))
		})
	}
}

func TestFormatToolResultSummary(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		result   string
		want     string
	}{
		{"empty result", "Read", "", ""},
		{"Read", "Read", "a\nb\nc\n", "Read 3 lines"},
		{"Glob files", "Glob", "a.go\nb.go\n", "Found 2 files"},
		{"Glob empty", "Glob", "\n", "No files found"},
		{"Grep matches", "Grep", "file1:1:match\nfile2:2:match\n", "Found 2 matches"},
		{"Grep none", "Grep", "\n", "No matches found"},
		{"Bash no output", "Bash", "", ""},
		{"Bash single line", "Bash", "OK\n", "OK"},
		{"Bash multi line", "Bash", "line1\nline2\nline3\n", "line1 (+2 more lines)"},
		{"Bash long line", "Bash", strings.Repeat("x", 80) + "\n", strings.Repeat("x", 57) + "..."},
		{"Write", "Write", "ok", "File written"},
		{"Edit", "Edit", "ok", "File updated"},
		{"unknown single", "Agent", "hello", "hello"},
		{"unknown multi", "Agent", "a\nb\nc\n", "3 lines"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatToolResultSummary(tc.toolName, tc.result))
		})
	}
}
