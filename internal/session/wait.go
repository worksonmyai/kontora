package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// WaitRequest is an invocation-bound request, not authority to change a ticket.
// NextStep in the enclosing Result holds the continuation instructions.
type WaitRequest struct {
	Kind     string        `yaml:"kind" json:"kind"`
	Until    time.Time     `yaml:"until,omitempty" json:"until,omitzero"`
	Command  string        `yaml:"command,omitempty" json:"command,omitempty"`
	Poll     time.Duration `yaml:"poll,omitempty" json:"poll,omitempty"`
	Deadline time.Time     `yaml:"deadline,omitempty" json:"deadline,omitzero"`
}

// MarshalJSON omits zero times (time.Time's JSON marshaler ignores omitempty).
func (w WaitRequest) MarshalJSON() ([]byte, error) {
	var until, deadline *time.Time
	if !w.Until.IsZero() {
		until = &w.Until
	}
	if !w.Deadline.IsZero() {
		deadline = &w.Deadline
	}
	return json.Marshal(struct {
		Kind     string        `json:"kind"`
		Until    *time.Time    `json:"until,omitempty"`
		Command  string        `json:"command,omitempty"`
		Poll     time.Duration `json:"poll,omitempty"`
		Deadline *time.Time    `json:"deadline,omitempty"`
	}{w.Kind, until, w.Command, w.Poll, deadline})
}

// UnmarshalJSON deliberately does not use ordinary struct decoding: that would
// accept duplicate keys, null scalars, case-insensitive names and bad Unicode.
func (w *WaitRequest) UnmarshalJSON(data []byte) error {
	if len(data) > MaxResultBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("wait must be size-limited UTF-8 without NUL")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("wait must be a JSON object")
	}
	var out WaitRequest
	seen := make(map[string]bool)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return fmt.Errorf("decoding wait field: %w", err)
		}
		key, ok := token.(string)
		if !ok || (key != "kind" && key != "until" && key != "command" && key != "poll" && key != "deadline") {
			return fmt.Errorf("unknown wait field %q", token)
		}
		if seen[key] {
			return fmt.Errorf("duplicate wait field %q", key)
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("decoding wait %s: %w", key, err)
		}
		if err := out.decodeJSONField(key, raw); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("closing wait object: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected data after wait object")
	}
	if out.Kind == "timer" && (seen["command"] || seen["poll"] || seen["deadline"]) || out.Kind == "check" && seen["until"] {
		return fmt.Errorf("wait fields do not match kind %q", out.Kind)
	}
	if err := ValidateWaitRequest(out); err != nil {
		return err
	}
	*w = out
	return nil
}

func (w *WaitRequest) decodeJSONField(key string, raw json.RawMessage) error {
	if key == "poll" {
		// ParseInt rejects null, strings, fractions, exponents and overflow.
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return fmt.Errorf("wait poll must be an integer duration in nanoseconds")
		}
		w.Poll = time.Duration(n)
		return nil
	}
	if len(raw) == 0 || raw[0] != '"' || !validJSONUnicode(raw) {
		return fmt.Errorf("wait %s must be a valid Unicode string", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !validResultText(value) {
		return fmt.Errorf("wait %s must be UTF-8 without NUL", key)
	}
	switch key {
	case "kind":
		w.Kind = value
	case "command":
		w.Command = value
	case "until", "deadline":
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return fmt.Errorf("wait %s must be an absolute RFC3339 timestamp", key)
		}
		if key == "until" {
			w.Until = parsed
		} else {
			w.Deadline = parsed
		}
	}
	return nil
}

// ValidateWaitRequest checks structure, not wall-clock freshness. Expired waits
// are valid requests; the daemon applies timer/deadline precedence durably.
func ValidateWaitRequest(w WaitRequest) error {
	switch w.Kind {
	case "timer":
		if w.Until.IsZero() || w.Command != "" || w.Poll != 0 || !w.Deadline.IsZero() {
			return fmt.Errorf("timer wait requires only until")
		}
	case "check":
		if !validResultText(w.Command) || strings.TrimSpace(w.Command) == "" || w.Poll <= 0 || w.Deadline.IsZero() || !w.Until.IsZero() {
			return fmt.Errorf("check wait requires command, positive poll and deadline, without until")
		}
	default:
		return fmt.Errorf("wait kind must be timer or check")
	}
	return nil
}

// ValidateWaitResult validates the version-3 payload and graph verdict contract.
// Use ValidateInvocationResult to also enforce captured invocation capabilities.
func ValidateWaitResult(result Result, verdicts []string) error {
	if err := validateResult(result); err != nil {
		return err
	}
	return validateGraphVerdict(result, verdicts)
}

// ValidateInvocationResult binds validation to the daemon-captured protocol and
// capabilities. Wake allowance exhaustion is checked against persisted state by
// the daemon, not by model-supplied data.
func ValidateInvocationResult(result Result, inv Invocation) error {
	if err := validateVersionResult(result, inv.Version); err != nil {
		return err
	}
	if inv.Version != WaitResultVersion && (inv.WakeMax != 0 || inv.CheckTimeout != 0) {
		return fmt.Errorf("wait capabilities require version 3")
	}
	if inv.Version == ResultVersion {
		return nil
	}
	if inv.Version == WaitResultVersion {
		if inv.WakeMax <= 0 || inv.CheckTimeout <= 0 || inv.AutomationID != "" || inv.RunNumber != 0 || !safeInvocationComponent(inv.TicketID) {
			return fmt.Errorf("version 3 requires ticket graph wait capabilities")
		}
		for _, id := range []string{inv.WorkflowExecutionID, inv.NodeVisitID, inv.DefinitionFingerprint} {
			if !safeInvocationComponent(id) {
				return fmt.Errorf("version 3 requires graph execution identity")
			}
		}
	}
	return validateGraphVerdict(result, inv.Verdicts)
}

func validateVersionResult(result Result, version int) error {
	if _, err := resultVersion([]int{version}); err != nil {
		return err
	}
	if version != WaitResultVersion && (result.Wait != nil || result.Outcome == "waiting") {
		return fmt.Errorf("waiting requires result version 3")
	}
	if version == ResultVersion && result.Verdict != "" {
		return fmt.Errorf("version 1 result cannot contain a verdict")
	}
	return validateResult(result)
}

// ResultsEqual compares terminal publication content, including equivalent
// timestamp instants with different locations and independently decoded pointers.
func ResultsEqual(a, b Result) bool {
	if a.Outcome != b.Outcome || a.Summary != b.Summary || a.NextStep != b.NextStep || a.Verdict != b.Verdict {
		return false
	}
	if a.Wait == nil || b.Wait == nil {
		return a.Wait == nil && b.Wait == nil
	}
	x, y := a.Wait, b.Wait
	return x.Kind == y.Kind && x.Command == y.Command && x.Poll == y.Poll && x.Until.Equal(y.Until) && x.Deadline.Equal(y.Deadline)
}
