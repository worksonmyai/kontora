package assistant

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
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

	// pi wraps every fragment of a message in a message_update; the records
	// around it describe the turn.
	const (
		piTextStart = `{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"text_start","contentIndex":0}}`
		piTextEnd   = `{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"text_end","contentIndex":0,"content":"hello there"}}`
		piToolStart = `{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"toolcall_start","contentIndex":1}}`
		piToolDelta = `{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"toolcall_delta","contentIndex":1,"delta":"{\"cmd\":"}}`
		piToolEnd   = `{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"toolcall_end","contentIndex":1,"toolCall":{"type":"toolCall","id":"t1","name":"bash","arguments":{}}}}`
		piMsgEnd    = `{"type":"message_end","message":{"role":"assistant","content":[]}}`
		piUserEnd   = `{"type":"message_end","message":{"role":"user","content":"hi"}}`
		piErrEnd    = `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"429 rate limit"}}`
		piAbortEnd  = `{"type":"message_end","message":{"role":"assistant","stopReason":"aborted"}}`
		piTurnEnd   = `{"type":"turn_end","message":{"role":"assistant"},"toolResults":[]}`
		piAgentEnd  = `{"type":"agent_end","messages":[{"role":"user"}],"willRetry":false}`
		piEntry     = `{"type":"entry_appended","entry":{"type":"message"}}`
		piToolExec  = `{"type":"tool_execution_update","toolCallId":"t1","toolName":"bash","partialResult":{}}`
	)
	piDelta := func(i int, kind, text string) string {
		return `{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"` + kind +
			`","contentIndex":` + string(rune('0'+i)) + `,"delta":"` + text + `"}}`
	}

	tests := []struct {
		name      string
		kind      string
		lines     []string
		wantCalls []string
		wantSink  string
		// wantFailure is what the records said went wrong, empty for a turn
		// that ended cleanly.
		wantFailure string
	}{
		{
			name:      "a text block start, its deltas and its stop",
			kind:      config.AgentKindClaude,
			lines:     []string{blockStart, delta(0, "text_delta", "hello"), delta(0, "text_delta", " there"), blockStop, assistant},
			wantCalls: []string{"block", "text:hello", "text: there", "seal"},
			wantSink:  assistant + "\n",
		},
		{
			name:      "a tool_use block names the tool",
			kind:      config.AgentKindClaude,
			lines:     []string{blockStart, delta(0, "text_delta", "ok"), blockStop, toolStart},
			wantCalls: []string{"block", "text:ok", "seal", "tool:Bash"},
			wantSink:  "",
		},
		{
			name:      "message_stop seals a block that never stopped",
			kind:      config.AgentKindClaude,
			lines:     []string{blockStart, delta(0, "text_delta", "cut"), `{"type":"stream_event","event":{"type":"message_stop"}}`},
			wantCalls: []string{"block", "text:cut", "seal"},
			wantSink:  "",
		},
		{
			name:      "a delta at another index is not this block's",
			kind:      config.AgentKindClaude,
			lines:     []string{blockStart, delta(1, "text_delta", "nope"), delta(0, "text_delta", "yes")},
			wantCalls: []string{"block", "text:yes"},
			wantSink:  "",
		},
		{
			name:      "thinking and input_json deltas have no row to land in",
			kind:      config.AgentKindClaude,
			lines:     []string{blockStart, delta(0, "thinking_delta", "hm"), delta(0, "input_json_delta", "{"), messageDe},
			wantCalls: []string{"block"},
			wantSink:  "",
		},
		{
			name:      "every other record reaches the log untouched",
			kind:      config.AgentKindClaude,
			lines:     []string{assistant, `{"type":"system","subtype":"init"}`, `{"type":"result","is_error":false}`},
			wantCalls: nil,
			wantSink:  assistant + "\n" + `{"type":"system","subtype":"init"}` + "\n" + `{"type":"result","is_error":false}` + "\n",
		},
		{
			name:      "malformed JSON is logged and reported to nobody",
			kind:      config.AgentKindClaude,
			lines:     []string{`{"type":`, blockStart, `not json at all`},
			wantCalls: []string{"block"},
			wantSink:  `{"type":` + "\n" + "not json at all\n",
		},
		{
			name:      "a blank line is kept",
			kind:      config.AgentKindClaude,
			lines:     []string{"", assistant, ""},
			wantCalls: nil,
			wantSink:  "\n" + assistant + "\n\n",
		},
		{
			name:      "a text block start, its deltas and its end",
			kind:      config.AgentKindPi,
			lines:     []string{piTextStart, piDelta(0, "text_delta", "hello"), piDelta(0, "text_delta", " there"), piTextEnd, piTurnEnd},
			wantCalls: []string{"block", "text:hello", "text: there", "seal"},
			wantSink:  piTurnEnd + "\n",
		},
		{
			name:      "only toolcall_end carries the name",
			kind:      config.AgentKindPi,
			lines:     []string{piToolStart, piToolDelta, piToolEnd},
			wantCalls: []string{"tool:bash"},
			wantSink:  "",
		},
		{
			name:      "message_end seals a block that never ended",
			kind:      config.AgentKindPi,
			lines:     []string{piTextStart, piDelta(0, "text_delta", "cut"), piMsgEnd},
			wantCalls: []string{"block", "text:cut", "seal"},
			wantSink:  piMsgEnd + "\n",
		},
		{
			name:      "a delta at another index is not this block's",
			kind:      config.AgentKindPi,
			lines:     []string{piTextStart, piDelta(1, "text_delta", "nope"), piDelta(0, "text_delta", "yes")},
			wantCalls: []string{"block", "text:yes"},
			wantSink:  "",
		},
		{
			name:      "thinking deltas have no row to land in",
			kind:      config.AgentKindPi,
			lines:     []string{piTextStart, piDelta(0, "thinking_delta", "hm")},
			wantCalls: []string{"block"},
			wantSink:  "",
		},
		{
			name:      "every other record reaches the log untouched",
			kind:      config.AgentKindPi,
			lines:     []string{`{"type":"session","version":3,"id":"S"}`, `{"type":"agent_start"}`, piTurnEnd},
			wantCalls: nil,
			wantSink:  `{"type":"session","version":3,"id":"S"}` + "\n" + `{"type":"agent_start"}` + "\n" + piTurnEnd + "\n",
		},
		{
			name:      "a message_end outside a block seals nothing",
			kind:      config.AgentKindPi,
			lines:     []string{piMsgEnd, piTextStart, piDelta(0, "text_delta", "hi"), piTextEnd, piMsgEnd},
			wantCalls: []string{"block", "text:hi", "seal"},
			wantSink:  piMsgEnd + "\n" + piMsgEnd + "\n",
		},
		{
			name:      "malformed JSON is logged and reported to nobody",
			kind:      config.AgentKindPi,
			lines:     []string{`{"type":`, piTextStart, `not json at all`},
			wantCalls: []string{"block"},
			wantSink:  `{"type":` + "\n" + "not json at all\n",
		},
		{
			name:        "a stop reason of error fails the turn json mode exited 0 on",
			kind:        config.AgentKindPi,
			lines:       []string{piTextStart, piDelta(0, "text_delta", "part"), piErrEnd},
			wantCalls:   []string{"block", "text:part", "seal"},
			wantSink:    piErrEnd + "\n",
			wantFailure: "429 rate limit",
		},
		{
			name:        "an abort with no message names the stop reason",
			kind:        config.AgentKindPi,
			lines:       []string{piAbortEnd},
			wantCalls:   nil,
			wantSink:    piAbortEnd + "\n",
			wantFailure: "the turn stopped: aborted",
		},
		{
			name:      "a later clean message clears an earlier failure",
			kind:      config.AgentKindPi,
			lines:     []string{piErrEnd, piTextStart, piDelta(0, "text_delta", "ok"), piTextEnd, piMsgEnd},
			wantCalls: []string{"block", "text:ok", "seal"},
			wantSink:  piErrEnd + "\n" + piMsgEnd + "\n",
		},
		{
			name:      "a user message ending is not the turn failing",
			kind:      config.AgentKindPi,
			lines:     []string{piUserEnd, piTurnEnd},
			wantCalls: nil,
			wantSink:  piUserEnd + "\n" + piTurnEnd + "\n",
		},
		{
			name:      "the records that repeat the session stay out of the log",
			kind:      config.AgentKindPi,
			lines:     []string{piAgentEnd, piEntry, piToolExec, piTurnEnd},
			wantCalls: nil,
			wantSink:  piTurnEnd + "\n",
		},
	}

	// The same input fed a byte at a time, in odd chunks and whole must produce
	// the same callbacks and the same log: exec hands over arbitrary chunks,
	// never lines.
	chunks := []int{1, 7, 0}
	for _, tt := range tests {
		partial := streamEventType
		if tt.kind == config.AgentKindPi {
			partial = piUpdateType
		}
		for _, size := range chunks {
			t.Run(tt.kind+" "+tt.name, func(t *testing.T) {
				in := strings.Join(tt.lines, "\n") + "\n"
				var sink bytes.Buffer
				rec := &streamRecorder{}
				w := NewStreamWriter(tt.kind, &sink, rec.handler())

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
				assert.NotContains(t, sink.String(), partial)
				if tt.wantFailure == "" {
					assert.NoError(t, w.Failure())
				} else {
					assert.EqualError(t, w.Failure(), tt.wantFailure)
				}
			})
		}
	}
}

func TestStreamWriterClose(t *testing.T) {
	t.Run("flushes a line with no trailing newline, and adds none", func(t *testing.T) {
		var sink bytes.Buffer
		rec := &streamRecorder{}
		w := NewStreamWriter(config.AgentKindClaude, &sink, rec.handler())
		_, _ = w.Write([]byte(`{"type":"assistant"}`))
		require.NoError(t, w.Close())
		assert.Equal(t, `{"type":"assistant"}`, sink.String())
	})

	t.Run("a trailing delta still reaches the handler", func(t *testing.T) {
		var sink bytes.Buffer
		rec := &streamRecorder{}
		w := NewStreamWriter(config.AgentKindClaude, &sink, rec.handler())
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
	w := NewStreamWriter(config.AgentKindClaude, &sink, rec.handler())

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
