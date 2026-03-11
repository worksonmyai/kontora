package logfmt

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const maxScanBuf = 1 << 20 // 1 MB

// Fmt reads Claude stream-json or session JSONL from r and writes
// human-readable formatted output to w. It processes every line until
// EOF to avoid sending SIGPIPE to the upstream process.
//
// Session JSONL differs from stream-json in tool result entries: session
// format stores tool results as message.content[] with tool_result blocks
// (containing tool_use_id + content) instead of top-level tool_name/tool_result.
func Fmt(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, maxScanBuf), maxScanBuf)

	var lastToolID string
	toolNames := make(map[string]string) // tool_use_id → tool_name

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
				fmt.Fprintf(w, "[%s]\n", ev.Model)
			}
		case "assistant":
			for _, block := range ev.Message.Content {
				switch block.Type {
				case "text":
					fmt.Fprintf(w, "%s\n", block.Text)
				case "tool_use":
					if block.ID != "" {
						toolNames[block.ID] = block.Name
					}
					if block.ID != lastToolID {
						lastToolID = block.ID
						arg := formatToolArg(block.Name, block.Input)
						fmt.Fprintf(w, "> %s%s\n", block.Name, arg)
					}
				}
			}
		case "user":
			if ev.ToolName != "" {
				// Stream-json format: tool_name and tool_result at top level.
				if summary := formatToolResultSummary(ev.ToolName, ev.ToolResult); summary != "" {
					fmt.Fprintf(w, "  ⎿  %s\n", summary)
				}
			} else {
				// Session JSONL format: tool results in message.content[].
				for _, block := range ev.Message.Content {
					if block.Type != "tool_result" {
						continue
					}
					toolName := toolNames[block.ToolUseID]
					resultText := extractToolResultText(block.Content)
					if summary := formatToolResultSummary(toolName, resultText); summary != "" {
						fmt.Fprintf(w, "  ⎿  %s\n", summary)
					}
				}
			}
		}
	}
	return scanner.Err()
}

type streamEvent struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	Model      string `json:"model"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolResult string `json:"tool_result,omitempty"`
	Message    struct {
		Content []contentBlock `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"` // session JSONL: tool_result blocks
	Content   json.RawMessage `json:"content,omitempty"`     // session JSONL: tool result content (string or array)
}

// extractToolResultText extracts the result text from a tool_result content
// field. The content can be a plain string or an array of content blocks
// like [{"type":"text","text":"..."}].
func extractToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func formatToolArg(toolName string, input map[string]any) string {
	if input == nil {
		return ""
	}
	switch strings.ToLower(toolName) {
	case "read", "write", "edit":
		if path, ok := input["file_path"].(string); ok {
			return " " + path
		}
	case "bash":
		if cmd, ok := input["command"].(string); ok {
			if len(cmd) > 80 {
				cmd = cmd[:80] + "..."
			}
			return " " + cmd
		}
	case "glob":
		if pattern, ok := input["pattern"].(string); ok {
			return " " + pattern
		}
	case "grep":
		if pattern, ok := input["pattern"].(string); ok {
			return " " + pattern
		}
	case "ticket":
		if desc, ok := input["description"].(string); ok {
			return " " + desc
		}
	}
	return ""
}

// FmtPi reads pi session JSONL from r and writes human-readable formatted
// output to w. Pi's format uses tree-structured entries with roles (user,
// assistant, toolResult) rather than Claude's stream-json type field.
func FmtPi(r io.Reader, w io.Writer) error {
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
			if ev.ModelID != "" {
				fmt.Fprintf(w, "[%s]\n", ev.ModelID)
			}
		case "message":
			switch ev.Message.Role {
			case "assistant":
				for _, block := range ev.Message.Content {
					switch block.Type {
					case "text":
						if block.Text != "" {
							fmt.Fprintf(w, "%s\n", block.Text)
						}
					case "toolCall":
						arg := formatToolArg(block.Name, block.Arguments)
						fmt.Fprintf(w, "> %s%s\n", block.Name, arg)
					}
				}
			case "toolResult":
				resultText := piToolResultText(ev.Message.Content)
				if summary := formatToolResultSummary(ev.Message.ToolName, resultText); summary != "" {
					fmt.Fprintf(w, "  ⎿  %s\n", summary)
				}
			}
		}
	}
	return scanner.Err()
}

type piEntry struct {
	Type    string `json:"type"`
	ModelID string `json:"modelId,omitempty"`
	Message struct {
		Role     string           `json:"role"`
		ToolName string           `json:"toolName,omitempty"` // toolResult messages
		Content  []piContentBlock `json:"content"`
	} `json:"message"`
}

type piContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"` // toolCall arguments
}

// piToolResultText extracts the result text from pi's toolResult content blocks.
// Pi stores results as [{"type":"text","text":"..."}] arrays.
func piToolResultText(blocks []piContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func formatToolResultSummary(toolName, result string) string {
	if result == "" {
		return ""
	}

	lines := strings.Split(result, "\n")
	lineCount := len(lines)
	if lineCount > 0 && lines[lineCount-1] == "" {
		lineCount--
	}

	switch strings.ToLower(toolName) {
	case "read":
		return fmt.Sprintf("Read %d lines", lineCount)
	case "glob":
		fileCount := 0
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				fileCount++
			}
		}
		if fileCount == 0 {
			return "No files found"
		}
		return fmt.Sprintf("Found %d files", fileCount)
	case "grep":
		matchCount := 0
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				matchCount++
			}
		}
		if matchCount == 0 {
			return "No matches found"
		}
		return fmt.Sprintf("Found %d matches", matchCount)
	case "bash":
		firstLine := strings.TrimSpace(lines[0])
		if len(firstLine) > 60 {
			firstLine = firstLine[:57] + "..."
		}
		if lineCount == 1 {
			return firstLine
		}
		return fmt.Sprintf("%s (+%d more lines)", firstLine, lineCount-1)
	case "write":
		return "File written"
	case "edit":
		return "File updated"
	default:
		if lineCount <= 1 {
			return strings.TrimSpace(result)
		}
		return fmt.Sprintf("%d lines", lineCount)
	}
}
