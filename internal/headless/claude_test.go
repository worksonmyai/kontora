package headless

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type event = map[string]any

func streamEvents(events []event) string {
	var stream strings.Builder
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			panic(err)
		}
		stream.Write(data)
		stream.WriteByte('\n')
	}
	return stream.String()
}

func claudeEvents(decision string) []event {
	return []event{
		{"type": "system", "subtype": "init", "tools": []any{}, "mcp_servers": []any{}, "plugins": []any{}, "session_id": "fresh-session", "model": "test-model"},
		{"type": "assistant", "parent_tool_use_id": nil, "message": event{"role": "assistant", "content": []any{event{"type": "text", "text": decision}}, "stop_reason": "end_turn"}},
		{"type": "result", "subtype": "success", "is_error": false, "result": decision, "stop_reason": "end_turn", "num_turns": 1, "terminal_reason": "completed", "permission_denials": []any{}, "total_cost_usd": 0.02, "usage": event{"input_tokens": 20, "output_tokens": 10}, "modelUsage": event{"test-model": event{"inputTokens": 20, "outputTokens": 10, "costUSD": 0.02}}},
	}
}

func claudeStream(decision string) string { return streamEvents(claudeEvents(decision)) }

func TestClaudeTerminalValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func([]event) []event
	}{
		{"success", func(e []event) []event { return e }},
		{"split assistant message", func(e []event) []event { e[1]["message"].(event)["stop_reason"] = nil; return e }},
		{"no result", func(e []event) []event { return e[:2] }},
		{"nested provider error", func(e []event) []event { e[1]["message"].(event)["error"] = "provider error"; return e }},
		{"result only", func(e []event) []event { return e[2:] }},
		{"no assistant", func(e []event) []event { return []event{e[0], e[2]} }},
		{"no init", func(e []event) []event { return e[1:] }},
		{"tools loaded", func(e []event) []event { e[0]["tools"] = []string{"Read"}; return e }},
		{"missing tools declaration", func(e []event) []event { delete(e[0], "tools"); return e }},
		{"MCP loaded", func(e []event) []event {
			e[0]["mcp_servers"] = []event{{"name": "local", "status": "connected"}}
			return e
		}},
		{"plugin loaded", func(e []event) []event { e[0]["plugins"] = []event{{"name": "sandbox"}}; return e }},
		{"skill loaded", func(e []event) []event { e[0]["skills"] = []string{"skill"}; return e }},
		{"hook executed", func(e []event) []event { return append([]event{{"type": "system", "subtype": "hook_response"}}, e...) }},
		{"provider error assistant", func(e []event) []event { e[1]["error"] = "rate_limit"; return e }},
		{"provider retry", func(e []event) []event {
			return append(e[:1], append([]event{{"type": "system", "subtype": "api_retry", "error": "overloaded"}}, e[1:]...)...)
		}},
		{"assistant aborted", func(e []event) []event { e[1]["aborted"] = true; return e }},
		{"assistant truncated", func(e []event) []event { e[1]["message"].(event)["stop_reason"] = "max_tokens"; return e }},
		{"assistant truncated with failed result", func(e []event) []event {
			e[1]["message"].(event)["stop_reason"] = "max_tokens"
			e[2]["subtype"], e[2]["is_error"] = "error_max_turns", true
			return e
		}},
		{"tool call", func(e []event) []event {
			e[1]["message"].(event)["content"] = []event{{"type": "tool_use", "name": "Bash"}}
			return e
		}},
		{"subagent", func(e []event) []event { e[1]["parent_tool_use_id"] = "tool"; return e }},
		{"result subtype error", func(e []event) []event { e[2]["subtype"] = "error_max_turns"; return e }},
		{"result is error", func(e []event) []event { e[2]["is_error"] = true; return e }},
		{"result missing error flag", func(e []event) []event { delete(e[2], "is_error"); return e }},
		{"result null error flag", func(e []event) []event { e[2]["is_error"] = nil; return e }},
		{"result wrong error flag", func(e []event) []event { e[2]["is_error"] = "false"; return e }},
		{"result provider status", func(e []event) []event { e[2]["api_error_status"] = 429; return e }},
		{"result errors", func(e []event) []event { e[2]["errors"] = []string{"authentication failed"}; return e }},
		{"result permission denial", func(e []event) []event { e[2]["permission_denials"] = []event{{"tool_name": "Read"}}; return e }},
		{"no completed stop", func(e []event) []event { delete(e[2], "stop_reason"); return e }},
		{"length stop", func(e []event) []event { e[2]["stop_reason"] = "max_tokens"; return e }},
		{"hook blocked result", func(e []event) []event { e[2]["num_turns"] = 0; delete(e[2], "terminal_reason"); return e }},
		{"hook stopped result", func(e []event) []event { e[2]["terminal_reason"] = "hook_stopped"; return e }},
		{"multiple turns", func(e []event) []event { e[2]["num_turns"] = 2; return e }},
		{"deferred result", func(e []event) []event { e[2]["deferred_tool_use"] = event{"name": "Read"}; return e }},
		{"local command result", func(e []event) []event { e[2]["local_command"] = "compact"; return e }},
		{"resumed result", func(e []event) []event { e[2]["resume_reason"] = "interrupted_turn"; return e }},
		{"missing result text", func(e []event) []event { delete(e[2], "result"); return e }},
		{"trailing error", func(e []event) []event { return append(e, event{"type": "error", "error": "late error"}) }},
		{"duplicate result", func(e []event) []event { return append(e, e[2]) }},
		{"negative cost", func(e []event) []event { e[2]["total_cost_usd"] = -1; return e }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := streamEvents(tc.change(claudeEvents(testDecision)))
			result, err := fakeAdapter(stream).Run(context.Background(), testProfile("claude"), testTurn())
			success := tc.name == "success" || tc.name == "split assistant message"
			if success {
				if err != nil || string(result.Decision) != testDecision {
					t.Fatalf("valid Claude response failed: %v", err)
				}
			} else if err == nil || result.Decision != nil {
				t.Fatalf("unsafe Claude response authorized: %s, %v", result.Decision, err)
			}
			if success || strings.HasPrefix(tc.name, "assistant truncated") || tc.name == "duplicate result" {
				if result.Usage.CostUSD == nil || *result.Usage.CostUSD != 0.02 || len(result.Usage.Reports) != 1 {
					t.Fatalf("usage not retained exactly once: %+v", result.Usage)
				}
			}
		})
	}
}

func TestClaudeUnknownCost(t *testing.T) {
	e := claudeEvents(testDecision)
	delete(e[2], "total_cost_usd")
	result, err := fakeAdapter(streamEvents(e)).Run(context.Background(), testProfile("claude"), testTurn())
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.CostUSD != nil || len(result.Usage.Reports) == 0 {
		t.Fatal("unknown Claude cost was treated as zero")
	}
}

func TestTransportFraming(t *testing.T) {
	for _, kind := range []string{"pi", "claude"} {
		t.Run(kind, func(t *testing.T) {
			valid := piStream(testDecision)
			if kind == "claude" {
				valid = claudeStream(testDecision)
			}
			prefix := valid
			cost := 0.02
			if kind == "pi" {
				prefix = streamEvents(piEvents(testDecision)[:10])
				cost = 0.01
			}
			for _, tc := range []struct {
				name, stream string
				valid, usage bool
			}{
				{"valid", valid, true, true},
				{"CRLF", strings.ReplaceAll(valid, "\n", "\r\n"), true, true},
				{"empty", "", false, false},
				{"missing LF", strings.TrimSuffix(valid, "\n"), false, kind == "pi"},
				{"incomplete record", valid + `{"type":`, false, true},
				{"truncated after usage", prefix + `{"type":"turn_end","message":`, false, true},
				{"unframed usage record", strings.TrimSuffix(prefix, "\n"), false, false},
				{"empty record", valid + "\n", false, true},
				{"array record", "[]\n" + valid, false, false},
				{"null record", "null\n" + valid, false, false},
				{"two records on line", "{} {}\n" + valid, false, false},
				{"duplicate field", "{\"type\":\"ignored\",\"type\":\"session\"}\n" + valid, false, false},
				{"nested duplicate", "{\"type\":\"session\",\"nested\":{\"key\":1,\"key\":2}}\n" + valid, false, false},
				{"escaped duplicate", "{\"type\":\"session\",\"t\\u0079pe\":\"session\"}\n" + valid, false, false},
				{"invalid UTF-8", string([]byte{0xff}) + "\n" + valid, false, false},
				{"deep nesting", `{"nested":` + strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66) + "}\n" + valid, false, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					result, err := fakeAdapter(tc.stream).Run(context.Background(), testProfile(kind), testTurn())
					if tc.valid && err != nil {
						t.Fatal(err)
					}
					if !tc.valid && (err == nil || result.Decision != nil) {
						t.Fatalf("invalid transport accepted: %v", err)
					}
					if tc.usage {
						if result.Usage.CostUSD == nil || *result.Usage.CostUSD != cost || len(result.Usage.Reports) != 1 {
							t.Fatalf("complete-prefix usage lost: %+v", result.Usage)
						}
					} else if len(result.Usage.Reports) != 0 || result.Usage.CostUSD != nil {
						t.Fatalf("usage from an invalid or unframed record: %+v", result.Usage)
					}
				})
			}
		})
	}
}
