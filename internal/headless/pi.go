package headless

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

func piArgs() []string {
	return []string{
		"--print", "--mode", "json", "--no-session", "--no-tools", "--no-mcp",
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes",
		"--no-context-files", "--no-approve", "--offline",
		"--system-prompt", SystemPrompt,
	}
}

type piMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Usage        json.RawMessage `json:"usage"`
}

func piAssistant(raw json.RawMessage) (piMessage, []byte, error) {
	var message piMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return message, nil, err
	}
	if message.Role != "assistant" || message.StopReason != "stop" || message.ErrorMessage != "" {
		return message, nil, errors.New("pi assistant did not complete successfully")
	}
	var text bytes.Buffer
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
		default:
			return message, nil, errors.New("pi planner emitted a tool or unknown content block")
		}
	}
	if text.Len() == 0 {
		return message, nil, errors.New("pi assistant has no final text")
	}
	return message, text.Bytes(), nil
}

func parsePi(data []byte) ([]byte, Usage, error) {
	var usage Usage
	var decision []byte
	var completed []json.RawMessage
	header, running, ended, settled := false, false, false, false
	messages := 0
	err := jsonLines(data, func(obj map[string]json.RawMessage) error {
		if settled {
			return errors.New("pi emitted data after agent_settled")
		}
		typ, err := field[string](obj, "type")
		if err != nil {
			return err
		}
		if !header && typ != "session" {
			return errors.New("pi stream lacks its session header")
		}
		switch typ {
		case "session":
			if header {
				return errors.New("duplicate Pi session header")
			}
			if err := piHeader(obj); err != nil {
				return err
			}
			header = true
		case "agent_start":
			if running || ended {
				return errors.New("unexpected additional Pi agent run")
			}
			running = true
		case "message_end":
			if !running {
				return errors.New("pi message outside an agent run")
			}
			raw, err := field[json.RawMessage](obj, "message")
			if err != nil {
				return err
			}
			if messages != 0 {
				return errors.New("pi message follows the final assistant")
			}
			text, err := piCompleted(raw, &usage)
			if err != nil {
				return err
			}
			completed = append(completed, bytes.Clone(raw))
			if text != nil {
				messages++
				decision = text
			}
		case "agent_end":
			if !running || messages != 1 {
				return errors.New("pi agent_end lacks a final assistant response")
			}
			if err := piEnd(obj, completed); err != nil {
				return err
			}
			running, ended = false, true
		case "agent_settled":
			aborted, err := field[bool](obj, "aborted")
			if err != nil || aborted || !ended {
				return errors.New("pi did not settle successfully")
			}
			settled = true
		case "message_update", "turn_start", "message_start", "turn_end":
			if !running {
				return errors.New("pi progress outside an agent run")
			}
			return piProgress(typ, obj, decision, messages, &usage)
		default:
			// Retries, compaction, extension entries, tool execution, and queued work
			// cannot belong to a single tool-disabled planner response.
			return fmt.Errorf("unexpected Pi planner event %q", typ)
		}
		return nil
	})
	if err == nil && !settled {
		err = errors.New("pi stream has no successful agent_settled")
	}
	if err != nil {
		decision = nil
	}
	return decision, usage, err
}

func piHeader(obj map[string]json.RawMessage) error {
	version, err := field[int](obj, "version")
	if err != nil || version != 3 {
		return errors.New("unsupported Pi session format")
	}
	id, err := field[string](obj, "id")
	if err != nil || id == "" {
		return errors.New("pi session has no identity")
	}
	return nil
}

func piCompleted(raw json.RawMessage, usage *Usage) ([]byte, error) {
	var metadata struct {
		Role       string            `json:"role"`
		ToolsAdded []json.RawMessage `json:"toolsAdded"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}
	if len(metadata.ToolsAdded) != 0 {
		return nil, errors.New("pi message enabled tools")
	}
	if metadata.Role == "user" || metadata.Role == "system" {
		return nil, nil
	}
	message, text, messageErr := piAssistant(raw)
	if err := piUsage(message.Usage, usage); err != nil {
		return nil, err
	}
	return text, messageErr
}

func piUsage(raw json.RawMessage, usage *Usage) error {
	var report struct {
		Cost *struct {
			Total *float64 `json:"total"`
		} `json:"cost"`
	}
	if present(raw) {
		if err := json.Unmarshal(raw, &report); err != nil {
			return err
		}
	}
	var cost *float64
	if report.Cost != nil {
		cost = report.Cost.Total
	}
	// Pi usage is cumulative for the single assistant message, not an added charge.
	latest := Usage{Reports: usage.Reports}
	err := latest.add(raw, cost)
	*usage = latest
	return err
}

func piEnd(obj map[string]json.RawMessage, completed []json.RawMessage) error {
	retry, err := field[bool](obj, "willRetry")
	if err != nil || retry {
		return errors.New("pi agent_end is not terminal")
	}
	generated, err := field[[]json.RawMessage](obj, "messages")
	if err != nil || len(generated) != len(completed) {
		return errors.New("pi agent_end has unexpected generated messages")
	}
	// agent_end includes the initial system and user messages, not just the response.
	for i := range generated {
		if !sameJSON(generated[i], completed[i]) {
			return errors.New("pi agent_end disagrees with message_end")
		}
	}
	return nil
}

func piProgress(typ string, obj map[string]json.RawMessage, decision []byte, messages int, usage *Usage) error {
	switch typ {
	case "message_update":
		if messages != 0 {
			return errors.New("pi update follows the final assistant")
		}
		if present(obj["usage"]) {
			if err := piUsage(obj["usage"], usage); err != nil {
				return err
			}
		}
		return piUpdate(obj)
	case "message_start":
		message, err := field[map[string]json.RawMessage](obj, "message")
		if err != nil {
			return err
		}
		role, err := field[string](message, "role")
		if err != nil {
			return err
		}
		if present(message["toolsAdded"]) {
			tools, err := field[[]json.RawMessage](message, "toolsAdded")
			if err != nil || len(tools) != 0 {
				return errors.New("pi message start enabled tools")
			}
		}
		if role == "user" || role == "system" {
			return nil
		}
		if role != "assistant" || len(decision) != 0 {
			return errors.New("unexpected Pi message start")
		}
		var partial piMessage
		if err := json.Unmarshal(obj["message"], &partial); err != nil {
			return err
		}
		for _, block := range partial.Content {
			if block.Type != "text" && block.Type != "thinking" {
				return errors.New("pi partial response declared a tool")
			}
		}
	case "turn_end":
		tools, err := field[[]json.RawMessage](obj, "toolResults")
		if err != nil || len(tools) != 0 {
			return errors.New("pi turn included tool results")
		}
		_, text, err := piAssistant(obj["message"])
		if err != nil || !bytes.Equal(text, decision) {
			return errors.New("pi turn_end disagrees with final assistant")
		}
	}
	return nil
}

func piUpdate(obj map[string]json.RawMessage) error {
	event, err := field[map[string]json.RawMessage](obj, "assistantMessageEvent")
	if err != nil {
		return err
	}
	typ, err := field[string](event, "type")
	if err != nil {
		return err
	}
	switch typ {
	case "start", "text_start", "text_delta", "text_end", "thinking_start", "thinking_delta", "thinking_end":
		return nil
	case "done":
		reason, err := field[string](event, "reason")
		if err != nil || reason != "stop" {
			return errors.New("pi provider stream did not stop successfully")
		}
		return nil
	default:
		return fmt.Errorf("unexpected Pi provider event %q", typ)
	}
}
