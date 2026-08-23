package assistant

import (
	"bytes"
	"encoding/json"
	"io"
)

// streamLineMax bounds one line. A longer one is dropped whole rather than
// grown on the heap: a JSONL line cut in half would not parse anyway.
const streamLineMax = 1 << 20

// streamEventType is the record --include-partial-messages adds. Every other
// one is a message claude writes whole.
const streamEventType = "stream_event"

// StreamHandler is what a StreamWriter reports. Every callback is optional.
type StreamHandler struct {
	// Block starts a new text block; Text reported before it belonged to the
	// last one.
	Block func()
	Text  func(delta string)
	// Tool names a call whose arguments the agent has begun generating.
	Tool func(name string)
	// Seal reports that the block stopped growing.
	Seal func()
}

func (h StreamHandler) Live() bool {
	return h.Block != nil || h.Text != nil || h.Tool != nil || h.Seal != nil
}

// streamRecord is one line, decoded far enough to dispatch on.
type streamRecord struct {
	Type  string `json:"type"`
	Event struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}

// StreamWriter reports the partial-message records on the agent's stdout and
// passes every other line to a sink. It is the tee rather than an
// io.MultiWriter arm because the same parse decides which lines the log keeps.
//
// It holds no lock: exec's copy goroutine is the only writer, and cmd.Wait joins
// it before process.Run returns, so Close cannot overlap a Write.
type StreamWriter struct {
	sink io.Writer
	h    StreamHandler
	// buf is the line read so far, without its newline.
	buf []byte
	// skip drops the rest of a line that went past streamLineMax.
	skip bool
	// textIdx is the block index being written, or -1 outside one.
	textIdx int
}

func NewStreamWriter(sink io.Writer, h StreamHandler) *StreamWriter {
	return &StreamWriter{sink: sink, h: h, textIdx: -1}
}

// Write never reports short: refusing a chunk would kill the turn over a line
// the parser did not like.
func (w *StreamWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.grow(p)
			break
		}
		w.grow(p[:i])
		p = p[i+1:]
		if w.skip {
			w.skip = false
		} else {
			w.line(w.buf, true)
		}
		w.buf = w.buf[:0]
	}
	return n, nil
}

// Close flushes a trailing line that arrived with no newline.
func (w *StreamWriter) Close() error {
	if !w.skip && len(w.buf) > 0 {
		w.line(w.buf, false)
	}
	w.buf = w.buf[:0]
	w.skip = false
	return nil
}

func (w *StreamWriter) grow(b []byte) {
	if w.skip {
		return
	}
	if len(w.buf)+len(b) > streamLineMax {
		w.skip = true
		w.buf = w.buf[:0]
		return
	}
	w.buf = append(w.buf, b...)
}

// Malformed JSON reaches the sink too: a line this parser cannot use may still
// be the one that explains a failure.
func (w *StreamWriter) line(b []byte, eol bool) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &head); err != nil || head.Type != streamEventType {
		w.emit(b, eol)
		return
	}
	var rec streamRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		w.emit(b, eol)
		return
	}
	w.dispatch(rec)
}

func (w *StreamWriter) emit(b []byte, eol bool) {
	if w.sink == nil {
		return
	}
	_, _ = w.sink.Write(b)
	if eol {
		_, _ = w.sink.Write([]byte{'\n'})
	}
}

// thinking_delta, input_json_delta, message_start and message_delta are
// ignored: the tape has no rows for them, so the pane has nowhere to put them.
func (w *StreamWriter) dispatch(rec streamRecord) {
	ev := rec.Event
	switch ev.Type {
	case "content_block_start":
		switch ev.ContentBlock.Type {
		case "text":
			w.textIdx = ev.Index
			if w.h.Block != nil {
				w.h.Block()
			}
		case "tool_use":
			if w.h.Tool != nil {
				w.h.Tool(ev.ContentBlock.Name)
			}
		}
	case "content_block_delta":
		if ev.Delta.Type == "text_delta" && w.textIdx >= 0 && ev.Index == w.textIdx && w.h.Text != nil {
			w.h.Text(ev.Delta.Text)
		}
	case "content_block_stop":
		if w.textIdx >= 0 && ev.Index == w.textIdx {
			w.textIdx = -1
			w.seal()
		}
	case "message_stop":
		// The backstop for a message that ends without a content_block_stop.
		w.textIdx = -1
		w.seal()
	}
}

func (w *StreamWriter) seal() {
	if w.h.Seal != nil {
		w.h.Seal()
	}
}
