package assistant

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/worksonmyai/kontora/internal/config"
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

func (h StreamHandler) seal() {
	if h.Seal != nil {
		h.Seal()
	}
}

// streamDecoder reads one agent's wire records. decode reports what a line
// carried to the handler and returns whether the turn log keeps the line.
// failure reports a turn the records said went wrong, for an agent whose exit
// code does not.
type streamDecoder interface {
	decode(b []byte, h StreamHandler) bool
	failure() error
}

// streamRecord is one line of claude's stream-json, decoded far enough to
// dispatch on.
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
	dec  streamDecoder
	// buf is the line read so far, without its newline.
	buf []byte
	// skip drops the rest of a line that went past streamLineMax.
	skip bool
}

// NewStreamWriter picks the decoder for kind. An unknown one cannot reach it:
// BuildArgs rejects the agent before the turn spawns.
func NewStreamWriter(kind string, sink io.Writer, h StreamHandler) *StreamWriter {
	w := &StreamWriter{sink: sink, h: h}
	switch kind {
	case config.AgentKindPi:
		w.dec = &piDecoder{textIdx: -1}
	default:
		w.dec = &claudeDecoder{textIdx: -1}
	}
	return w
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

// Failure reports what the wire said went wrong, once the process has exited.
func (w *StreamWriter) Failure() error { return w.dec.failure() }

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

func (w *StreamWriter) line(b []byte, eol bool) {
	if w.dec.decode(b, w.h) {
		w.emit(b, eol)
	}
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

// claudeDecoder reads claude's stream-json. textIdx is the block index being
// written, or -1 outside one.
type claudeDecoder struct {
	textIdx int
}

// claude reports a failed turn through its exit code, so nothing is read off
// the wire.
func (d *claudeDecoder) failure() error { return nil }

// Malformed JSON reaches the sink too: a line this parser cannot use may still
// be the one that explains a failure.
func (d *claudeDecoder) decode(b []byte, h StreamHandler) bool {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &head); err != nil || head.Type != streamEventType {
		return true
	}
	var rec streamRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return true
	}
	d.dispatch(rec, h)
	return false
}

// thinking_delta, input_json_delta, message_start and message_delta are
// ignored: the tape has no rows for them, so the pane has nowhere to put them.
func (d *claudeDecoder) dispatch(rec streamRecord, h StreamHandler) {
	ev := rec.Event
	switch ev.Type {
	case "content_block_start":
		switch ev.ContentBlock.Type {
		case "text":
			d.textIdx = ev.Index
			if h.Block != nil {
				h.Block()
			}
		case "tool_use":
			if h.Tool != nil {
				h.Tool(ev.ContentBlock.Name)
			}
		}
	case "content_block_delta":
		if ev.Delta.Type == "text_delta" && d.textIdx >= 0 && ev.Index == d.textIdx && h.Text != nil {
			h.Text(ev.Delta.Text)
		}
	case "content_block_stop":
		if d.textIdx >= 0 && ev.Index == d.textIdx {
			d.textIdx = -1
			h.seal()
		}
	case "message_stop":
		// The backstop for a message that ends without a content_block_stop.
		d.textIdx = -1
		h.seal()
	}
}
