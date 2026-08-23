package assistant

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// record collects everything a StreamWriter reports, in order, so a table can
// assert on the whole callback sequence rather than on one field at a time.
type streamRecorder struct {
	calls []string
	text  strings.Builder
}

func (r *streamRecorder) handler() StreamHandler {
	return StreamHandler{
		Block: func() { r.calls = append(r.calls, "block") },
		Text: func(d string) {
			r.calls = append(r.calls, "text:"+d)
			r.text.WriteString(d)
		},
		Tool: func(n string) { r.calls = append(r.calls, "tool:"+n) },
		Seal: func() { r.calls = append(r.calls, "seal") },
	}
}

func TestStreamWriter(t *testing.T) {
	const (
		blockStart = `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`
		toolStart  = `{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Bash","input":{}}}}`
		blockStop  = `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`
		messageDe  = `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}`
		assistant  = `{"type":"assistant","message":{"content":[{"type":"text","text":"hello there"}]}}`
	)
	delta := func(i int, kind, text string) string {
		return `{"type":"stream_event","event":{"type":"content_block_delta","index":` +
			string(rune('0'+i)) + `,"delta":{"type":"` + kind + `","text":"` + text + `"}}}`
	}

	tests := []struct {
		name      string
		lines     []string
		wantCalls []string
		wantSink  string
	}{
		{
			name:      "a text block start, its deltas and its stop",
			lines:     []string{blockStart, delta(0, "text_delta", "hello"), delta(0, "text_delta", " there"), blockStop, assistant},
			wantCalls: []string{"block", "text:hello", "text: there", "seal"},
			wantSink:  assistant + "\n",
		},
		{
			name:      "a tool_use block names the tool",
			lines:     []string{blockStart, delta(0, "text_delta", "ok"), blockStop, toolStart},
			wantCalls: []string{"block", "text:ok", "seal", "tool:Bash"},
			wantSink:  "",
		},
		{
			name:      "message_stop seals a block that never stopped",
			lines:     []string{blockStart, delta(0, "text_delta", "cut"), `{"type":"stream_event","event":{"type":"message_stop"}}`},
			wantCalls: []string{"block", "text:cut", "seal"},
			wantSink:  "",
		},
		{
			name:      "a delta at another index is not this block's",
			lines:     []string{blockStart, delta(1, "text_delta", "nope"), delta(0, "text_delta", "yes")},
			wantCalls: []string{"block", "text:yes"},
			wantSink:  "",
		},
		{
			name:      "thinking and input_json deltas have no row to land in",
			lines:     []string{blockStart, delta(0, "thinking_delta", "hm"), delta(0, "input_json_delta", "{"), messageDe},
			wantCalls: []string{"block"},
			wantSink:  "",
		},
		{
			name:      "every other record reaches the log untouched",
			lines:     []string{assistant, `{"type":"system","subtype":"init"}`, `{"type":"result","is_error":false}`},
			wantCalls: nil,
			wantSink:  assistant + "\n" + `{"type":"system","subtype":"init"}` + "\n" + `{"type":"result","is_error":false}` + "\n",
		},
		{
			name:      "malformed JSON is logged and reported to nobody",
			lines:     []string{`{"type":`, blockStart, `not json at all`},
			wantCalls: []string{"block"},
			wantSink:  `{"type":` + "\n" + "not json at all\n",
		},
		{
			name:      "a blank line is kept",
			lines:     []string{"", assistant, ""},
			wantCalls: nil,
			wantSink:  "\n" + assistant + "\n\n",
		},
	}

	// The same input fed a byte at a time, in odd chunks and whole must produce
	// the same callbacks and the same log: exec hands over arbitrary chunks,
	// never lines.
	chunks := []int{1, 7, 0}
	for _, tt := range tests {
		for _, size := range chunks {
			t.Run(tt.name, func(t *testing.T) {
				in := strings.Join(tt.lines, "\n") + "\n"
				var sink bytes.Buffer
				rec := &streamRecorder{}
				w := NewStreamWriter(&sink, rec.handler())

				if size == 0 {
					n, err := w.Write([]byte(in))
					require.NoError(t, err)
					assert.Equal(t, len(in), n)
				} else {
					for i := 0; i < len(in); i += size {
						end := min(i+size, len(in))
						n, err := w.Write([]byte(in[i:end]))
						require.NoError(t, err)
						assert.Equal(t, end-i, n)
					}
				}
				require.NoError(t, w.Close())

				assert.Equal(t, tt.wantCalls, rec.calls)
				assert.Equal(t, tt.wantSink, sink.String())
				assert.NotContains(t, sink.String(), streamEventType)
			})
		}
	}
}

func TestStreamWriterClose(t *testing.T) {
	t.Run("flushes a line with no trailing newline, and adds none", func(t *testing.T) {
		var sink bytes.Buffer
		rec := &streamRecorder{}
		w := NewStreamWriter(&sink, rec.handler())
		_, _ = w.Write([]byte(`{"type":"assistant"}`))
		require.NoError(t, w.Close())
		assert.Equal(t, `{"type":"assistant"}`, sink.String())
	})

	t.Run("a trailing delta still reaches the handler", func(t *testing.T) {
		var sink bytes.Buffer
		rec := &streamRecorder{}
		w := NewStreamWriter(&sink, rec.handler())
		_, _ = w.Write([]byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tail"}}}`))
		require.NoError(t, w.Close())
		assert.Equal(t, "tail", rec.text.String())
		assert.Empty(t, sink.String())
	})
}

func TestStreamWriterOverLongLine(t *testing.T) {
	var sink bytes.Buffer
	rec := &streamRecorder{}
	w := NewStreamWriter(&sink, rec.handler())

	_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", streamLineMax) + `"}` + "\n"))
	_, _ = w.Write([]byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}` + "\n"))
	_, _ = w.Write([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"after"}}}` + "\n"))
	require.NoError(t, w.Close())

	// The over-long line is dropped whole rather than half-written, and the
	// writer resynchronises at the next newline.
	assert.Empty(t, sink.String())
	assert.Equal(t, "after", rec.text.String())
}

func TestStreamHandlerLive(t *testing.T) {
	assert.False(t, StreamHandler{}.Live())
	assert.True(t, StreamHandler{Text: func(string) {}}.Live())
}
