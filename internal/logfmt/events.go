package logfmt

import (
	"bufio"
	"encoding/json"
	"io"
	"slices"
	"time"
	"unicode/utf8"
)

// TapeVersion is the schema version of a Tape. Bump it when a field changes
// meaning; readers of an older sidecar can then refuse it.
const TapeVersion = 1

const (
	// MaxResultBytes bounds one stored tool result.
	MaxResultBytes = 16 << 10
	// MaxTapeEvents bounds a whole tape.
	MaxTapeEvents = 5000
)

// Partial dimension names. A tape lists the dimensions it could not fill, and
// the UI hides the corresponding affordance rather than showing a zero or a
// wrong colour.
const (
	PartialTime    = "time"
	PartialUsage   = "usage"
	PartialIsError = "is_error"
)

// Usage is the token count of a run, summed over the four categories a Claude
// session record reports. There is no monetary figure: the session record
// carries no pricing.
type Usage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheCreate int `json:"cache_create"`
	CacheRead   int `json:"cache_read"`
}

func (u *Usage) add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheCreate += o.CacheCreate
	u.CacheRead += o.CacheRead
}

// Event is one row of a transcript. Kind is "model", "text", or "tool".
type Event struct {
	Time  *time.Time `json:"time,omitempty"`
	Kind  string     `json:"kind"`
	Model string     `json:"model,omitempty"`
	Text  string     `json:"text,omitempty"`
	// ID is the agent's tool-use id. It keys expansion in the UI.
	ID   string `json:"id,omitempty"`
	Tool string `json:"tool,omitempty"`
	// Arg and Summary hold the same bytes Fmt writes on the "> Tool arg" and
	// "  ⎿  summary" lines.
	Arg       string `json:"arg,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Result    string `json:"result,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
}

// Tape is the structured form of one agent run.
type Tape struct {
	Version int    `json:"version"`
	Agent   string `json:"agent"`
	Model   string `json:"model,omitempty"`
	Totals  Usage  `json:"totals"`
	// Partial names the dimensions this agent's session format does not
	// provide. An empty list means every dimension is trustworthy.
	Partial   []string `json:"partial,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Events    []Event  `json:"events"`
}

// Events reads Claude stream-json or session JSONL from r and returns it as a
// Tape. It walks the same shapes as Fmt and reuses formatToolArg and
// formatToolResultSummary, so Arg and Summary match the plaintext byte for
// byte. Fmt is deliberately not rewritten on top of this: its exact output is
// matched against user-authored failure_patterns.
func Events(r io.Reader) (Tape, error) {
	tape := Tape{Version: TapeVersion, Agent: "claude", Events: []Event{}}
	b := tapeBuilder{tape: &tape}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, maxScanBuf), maxScanBuf)

	var lastToolID string
	byToolID := make(map[string]int) // tool_use_id → index into tape.Events

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "system":
			if ev.Subtype == "init" && ev.Model != "" {
				b.model(ev.Model, ev.Timestamp)
			}
		case "assistant":
			// Session JSONL carries no system/init entry, so the model is only
			// visible on the assistant message itself.
			b.model(ev.Message.Model, ev.Timestamp)
			usage := ev.Message.Usage.total()
			tape.Totals.add(usage)
			pending := &usage

			for _, block := range ev.Message.Content {
				switch block.Type {
				case "text":
					b.append(Event{Kind: "text", Time: ev.Timestamp, Text: block.Text, Usage: takeUsage(&pending)})
				case "tool_use":
					if block.ID == lastToolID {
						continue
					}
					lastToolID = block.ID
					idx := b.append(Event{
						Kind:  "tool",
						Time:  ev.Timestamp,
						ID:    block.ID,
						Tool:  block.Name,
						Arg:   formatToolArg(block.Name, block.Input),
						Usage: takeUsage(&pending),
					})
					if idx >= 0 && block.ID != "" {
						byToolID[block.ID] = idx
					}
				}
			}
		case "user":
			if ev.ToolName != "" {
				// Stream-json: tool_name and tool_result at the top level, with
				// no id to match on, so the newest unanswered call is the one.
				b.result(b.lastUnanswered(), ev.ToolName, ev.ToolResult, false)
				continue
			}
			for _, block := range ev.Message.Content {
				if block.Type != "tool_result" {
					continue
				}
				idx, ok := byToolID[block.ToolUseID]
				if !ok {
					idx = -1
				}
				b.result(idx, toolNameAt(&tape, idx), extractToolResultText(block.Content), block.IsError)
			}
		}
	}
	return tape, scanner.Err()
}

// EventsPi reads pi session JSONL from r and returns it as a Tape. Pi's
// session format carries no timestamps, no token usage, and no error flag on
// tool results, so every pi tape declares those three dimensions partial. The
// keys are not guessed: adding tags for fields that may not exist would fail
// silently, reporting every timestamp null and every token count zero.
func EventsPi(r io.Reader) (Tape, error) {
	tape := Tape{
		Version: TapeVersion,
		Agent:   "pi",
		Partial: []string{PartialTime, PartialUsage, PartialIsError},
		Events:  []Event{},
	}
	b := tapeBuilder{tape: &tape}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, maxScanBuf), maxScanBuf)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev piEntry
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "model_change":
			b.model(ev.ModelID, nil)
		case "message":
			switch ev.Message.Role {
			case "assistant":
				for _, block := range ev.Message.Content {
					switch block.Type {
					case "text":
						if block.Text != "" {
							b.append(Event{Kind: "text", Text: block.Text})
						}
					case "toolCall":
						b.append(Event{
							Kind: "tool",
							Tool: block.Name,
							Arg:  formatToolArg(block.Name, block.Arguments),
						})
					}
				}
			case "toolResult":
				b.result(b.lastUnanswered(), ev.Message.ToolName, piTextBlocks(ev.Message.Content), false)
			}
		}
	}
	return tape, scanner.Err()
}

// tapeBuilder appends events under the whole-tape cap.
type tapeBuilder struct{ tape *Tape }

// append adds e and returns its index, or -1 when the tape is already full.
func (b *tapeBuilder) append(e Event) int {
	if len(b.tape.Events) >= MaxTapeEvents {
		b.tape.Truncated = true
		return -1
	}
	b.tape.Events = append(b.tape.Events, e)
	return len(b.tape.Events) - 1
}

// model records the run's model and emits a banner whenever it changes,
// mirroring the "[model]" lines Fmt writes.
func (b *tapeBuilder) model(name string, ts *time.Time) {
	if name == "" || name == b.tape.Model {
		return
	}
	b.tape.Model = name
	b.append(Event{Kind: "model", Time: ts, Model: name})
}

// lastUnanswered returns the index of the newest tool event still without a
// result, or -1 when there is none. Formats without a tool-use id on the
// result attach to it.
func (b *tapeBuilder) lastUnanswered() int {
	for i, e := range slices.Backward(b.tape.Events) {
		if e.Kind != "tool" {
			continue
		}
		if e.Summary == "" && e.Result == "" {
			return i
		}
		return -1
	}
	return -1
}

// result attaches a tool result to the event at idx. A result whose call was
// dropped by the tape cap, or whose id never appeared, is discarded: the
// plaintext log still carries it.
func (b *tapeBuilder) result(idx int, toolName, text string, isError bool) {
	if idx < 0 || idx >= len(b.tape.Events) {
		return
	}
	e := &b.tape.Events[idx]
	e.Summary = formatToolResultSummary(toolName, text)
	e.Result, e.Truncated = truncateUTF8(text, MaxResultBytes)
	e.IsError = isError
}

func toolNameAt(t *Tape, idx int) string {
	if idx < 0 || idx >= len(t.Events) {
		return ""
	}
	return t.Events[idx].Tool
}

// takeUsage hands the entry's usage to the first event it produces, so a
// reader can attribute tokens to a turn rather than only to the run total.
func takeUsage(pending **Usage) *Usage {
	u := *pending
	*pending = nil
	return u
}

// truncateUTF8 cuts s to at most limit bytes without splitting a rune.
func truncateUTF8(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
