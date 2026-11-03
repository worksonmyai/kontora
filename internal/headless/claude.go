package headless

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Bare mode skips resource discovery but does not suppress tools or MCP by itself.
// It also requires API-key authentication; the adapter never falls back to a subscription login.
func claudeArgs() []string {
	return []string{
		"--bare", "--print", "--output-format", "stream-json", "--verbose",
		"--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--disable-slash-commands", "--no-session-persistence", "--no-chrome",
		"--system-prompt", SystemPrompt,
	}
}

func parseClaude(data []byte) ([]byte, Usage, error) {
	var usage Usage
	var decision []byte
	initialized, assistant, terminal := false, false, false
	var responseErr error
	err := jsonLines(data, func(obj map[string]json.RawMessage) (eventErr error) {
		// Failed assistant stops can precede a terminal report of paid usage.
		defer func() {
			if responseErr == nil {
				responseErr = eventErr
			}
			eventErr = nil
		}()
		if terminal {
			return errors.New("claude emitted data after its terminal result")
		}
		typ, err := field[string](obj, "type")
		if err != nil {
			return err
		}
		if typ == "result" {
			terminal = true
			if err := claudeUsage(obj, &usage); err != nil {
				return err
			}
		}
		if present(obj["error"]) {
			return errors.New("claude reported a provider or runtime error")
		}
		if raw := obj["aborted"]; present(raw) && !bytes.Equal(raw, []byte("false")) {
			return errors.New("claude response aborted")
		}
		switch typ {
		case "system":
			subtype, err := field[string](obj, "subtype")
			if err != nil {
				return err
			}
			if subtype != "init" || initialized {
				return fmt.Errorf("unexpected Claude system event %q", subtype)
			}
			if err := claudeInit(obj); err != nil {
				return err
			}
			initialized = true
		case "assistant":
			if !initialized {
				return errors.New("claude assistant precedes initialization")
			}
			if err := claudeAssistant(obj); err != nil {
				return err
			}
			assistant = true
		case "result":
			if err := claudeSuccess(obj); err != nil {
				return err
			}
			turns, err := field[int](obj, "num_turns")
			if err != nil || turns != 1 || !initialized || !assistant {
				return errors.New("claude result did not finish one fresh assistant turn")
			}
			text, err := field[string](obj, "result")
			if err != nil {
				return err
			}
			decision = []byte(text)
		default:
			return fmt.Errorf("unexpected Claude planner event %q", typ)
		}
		return nil
	})
	err = errors.Join(responseErr, err)
	if err == nil && !terminal {
		err = errors.New("claude stream has no terminal result")
	}
	if err != nil {
		decision = nil
	}
	return decision, usage, err
}

func claudeInit(obj map[string]json.RawMessage) error {
	for _, key := range []string{"tools", "mcp_servers"} {
		items, err := field[[]json.RawMessage](obj, key)
		if err != nil || len(items) != 0 {
			return fmt.Errorf("claude planner requires an empty %s list", key)
		}
	}
	return emptyClaudeLists(obj, "plugins", "plugin_errors", "mcp_server_errors", "skills", "agents")
}

func emptyClaudeLists(obj map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if present(obj[key]) {
			items, err := field[[]json.RawMessage](obj, key)
			if err != nil || len(items) != 0 {
				return fmt.Errorf("claude planner reports unexpected %s", key)
			}
		}
	}
	return nil
}

func claudeAssistant(obj map[string]json.RawMessage) error {
	if present(obj["parent_tool_use_id"]) || present(obj["agent_id"]) {
		return errors.New("claude planner emitted subagent output")
	}
	message, err := field[map[string]json.RawMessage](obj, "message")
	if err != nil {
		return err
	}
	if present(message["error"]) {
		return errors.New("claude assistant reported an error")
	}
	role, err := field[string](message, "role")
	if err != nil || role != "assistant" {
		return errors.New("invalid Claude assistant role")
	}
	blocks, err := field[[]struct {
		Type string `json:"type"`
	}](message, "content")
	if err != nil || len(blocks) == 0 {
		return errors.New("missing Claude assistant content")
	}
	for _, block := range blocks {
		if block.Type != "text" && block.Type != "thinking" && block.Type != "redacted_thinking" {
			return errors.New("claude planner emitted a tool or unknown content block")
		}
	}
	// Claude can split one provider message into several assistant records.
	if present(message["stop_reason"]) {
		reason, err := field[string](message, "stop_reason")
		if err != nil || reason != "end_turn" {
			return errors.New("claude assistant did not complete successfully")
		}
	}
	return nil
}

func claudeUsage(obj map[string]json.RawMessage, usage *Usage) error {
	var cost *float64
	if present(obj["total_cost_usd"]) {
		value, err := field[float64](obj, "total_cost_usd")
		if err != nil {
			return err
		}
		cost = &value
	}
	report := obj["modelUsage"]
	if !present(report) {
		report = obj["usage"]
	}
	return usage.add(report, cost)
}

func claudeSuccess(obj map[string]json.RawMessage) error {
	subtype, err := field[string](obj, "subtype")
	if err != nil || subtype != "success" {
		return errors.New("claude result is not successful")
	}
	isError, err := field[bool](obj, "is_error")
	if err != nil || isError {
		return errors.New("claude result lacks explicit non-error success")
	}
	if present(obj["api_error_status"]) {
		return errors.New("claude result reports a provider error status")
	}
	if err := emptyClaudeLists(obj, "errors", "permission_denials"); err != nil {
		return err
	}
	for _, key := range []string{"deferred_tool_use", "local_command", "resume_reason"} {
		if present(obj[key]) {
			return errors.New("claude result is deferred, resumed, or a local command")
		}
	}
	if present(obj["terminal_reason"]) {
		reason, err := field[string](obj, "terminal_reason")
		if err != nil || reason != "completed" {
			return errors.New("claude result is incomplete")
		}
	}
	reason, err := field[string](obj, "stop_reason")
	if err != nil || reason != "end_turn" {
		return errors.New("claude result lacks a successful stop reason")
	}
	return nil
}
