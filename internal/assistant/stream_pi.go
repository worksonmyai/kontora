package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// piUpdateType is the record --mode json wraps one fragment of a message
	// in.
	piUpdateType = "message_update"
	piEndType    = "message_end"
)

// piBulkTypes are the records the turn log drops on top of the fragments; the
// rest describe the turn, and the log keeps them. agent_end carries the whole
// session, so a turn's log would otherwise grow with the thread rather than
// with the turn; entry_appended and tool_execution_update repeat what the
// session file the pane reads already holds.
var piBulkTypes = map[string]bool{
	"agent_end":             true,
	"entry_appended":        true,
	"tool_execution_update": true,
}

// piEvent is the fragment inside a message_update.
type piEvent struct {
	Type  string `json:"type"`
	Index int    `json:"contentIndex"`
	Delta string `json:"delta"`
	Tool  struct {
		Name string `json:"name"`
	} `json:"toolCall"`
}

// piMessage is the message a message_end closes, decoded far enough to tell a
// finished turn from a failed one.
type piMessage struct {
	Role       string `json:"role"`
	StopReason string `json:"stopReason"`
	Error      string `json:"errorMessage"`
}

// piRecord is one line of pi's --mode json output, decoded far enough to
// dispatch on.
type piRecord struct {
	Type    string    `json:"type"`
	Event   piEvent   `json:"assistantMessageEvent"`
	Message piMessage `json:"message"`
}

// piDecoder reads pi's --mode json wire events. textIdx is the content index
// being written, or -1 outside a text block; pi numbers the indexes from zero
// again in every message.
type piDecoder struct {
	textIdx int
	err     error
}

func (d *piDecoder) failure() error { return d.err }

func (d *piDecoder) decode(b []byte, h StreamHandler) bool {
	var rec piRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return true
	}
	switch rec.Type {
	case piUpdateType:
		d.dispatch(rec.Event, h)
		return false
	case piEndType:
		d.stopped(rec.Message)
		// The backstop for a message that ends without a text_end. Every
		// message closes with one, tool results included, so only an open
		// block is sealed.
		if d.textIdx >= 0 {
			d.textIdx = -1
			h.seal()
		}
	}
	return !piBulkTypes[rec.Type]
}

// stopped reads the failure pi's json mode does not otherwise report: only
// `--mode text` inspects the last message's stopReason, prints errorMessage and
// exits 1, so without this a provider error or an aborted turn would exit 0
// with no reply and no error. The last assistant message decides, as it does in
// pi's own text mode.
func (d *piDecoder) stopped(m piMessage) {
	if m.Role != "assistant" {
		return
	}
	switch m.StopReason {
	case "error", "aborted":
		if m.Error != "" {
			d.err = errors.New(m.Error)
		} else {
			d.err = fmt.Errorf("the turn stopped: %s", m.StopReason)
		}
	default:
		d.err = nil
	}
}

// thinking_*, toolcall_start and toolcall_delta are ignored: the tape has no
// rows for them, and toolcall_start carries no name to put in one.
func (d *piDecoder) dispatch(ev piEvent, h StreamHandler) {
	switch ev.Type {
	case "text_start":
		d.textIdx = ev.Index
		if h.Block != nil {
			h.Block()
		}
	case "text_delta":
		if d.textIdx >= 0 && ev.Index == d.textIdx && h.Text != nil {
			h.Text(ev.Delta)
		}
	case "text_end":
		if d.textIdx >= 0 && ev.Index == d.textIdx {
			d.textIdx = -1
			h.seal()
		}
	case "toolcall_end":
		if h.Tool != nil {
			h.Tool(ev.Tool.Name)
		}
	}
}
