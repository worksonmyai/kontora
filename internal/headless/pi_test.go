package headless

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func piEvents(decision string) []event {
	system := event{"role": "system", "content": SystemPrompt, "timestamp": 1}
	user := event{"role": "user", "content": "snapshot", "timestamp": 2}
	assistant := event{
		"role": "assistant", "content": []any{event{"type": "text", "text": decision}},
		"stopReason": "stop", "timestamp": 3, "provider": "anthropic", "model": "test-model",
		"usage": event{"input": 20, "output": 10, "totalTokens": 30, "cost": event{"total": 0.01}},
	}
	return []event{
		{"type": "session", "version": 3, "id": "fresh-session", "cwd": "/private/planner"},
		{"type": "agent_start"},
		{"type": "turn_start"},
		{"type": "message_start", "message": system},
		{"type": "message_end", "message": system},
		{"type": "message_start", "message": user},
		{"type": "message_end", "message": user},
		{"type": "message_start", "message": event{"role": "assistant", "content": []any{}, "stopReason": "pending"}},
		{"type": "message_update", "assistantMessageEvent": event{"type": "text_delta", "contentIndex": 0, "delta": decision}},
		{"type": "message_end", "message": assistant},
		{"type": "turn_end", "message": assistant, "toolResults": []any{}},
		{"type": "agent_end", "messages": []any{system, user, assistant}, "willRetry": false},
		{"type": "agent_settled", "aborted": false},
	}
}

func piStream(decision string) string { return streamEvents(piEvents(decision)) }

func TestPiTerminalValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func([]event) []event
	}{
		{"success", func(e []event) []event { return e }},
		{"no settled", func(e []event) []event { return e[:12] }},
		{"partial tool declaration", func(e []event) []event {
			e[7]["message"].(event)["content"] = []event{{"type": "toolCall", "name": "read"}}
			return e
		}},
		{"partial system tools", func(e []event) []event {
			e[3]["message"] = event{"role": "system", "content": "", "toolsAdded": []event{{"name": "read"}}}
			return e
		}},
		{"provider done error", func(e []event) []event {
			e[8]["assistantMessageEvent"] = event{"type": "done", "reason": "error"}
			return e
		}},
		{"provider done success", func(e []event) []event {
			e[8]["assistantMessageEvent"] = event{"type": "done", "reason": "stop"}
			return e
		}},
		{"no agent end", func(e []event) []event { return append(e[:11], e[12]) }},
		{"no final assistant", func(e []event) []event { return append(e[:9], e[11:]...) }},
		{"only settled", func(e []event) []event { return []event{e[0], e[12]} }},
		{"no header", func(e []event) []event { return e[1:] }},
		{"bad header", func(e []event) []event { e[0]["version"] = 99; return e }},
		{"no session identity", func(e []event) []event { delete(e[0], "id"); return e }},
		{"settled aborted", func(e []event) []event { e[12]["aborted"] = true; return e }},
		{"settled missing aborted", func(e []event) []event { delete(e[12], "aborted"); return e }},
		{"settled null aborted", func(e []event) []event { e[12]["aborted"] = nil; return e }},
		{"settled string aborted", func(e []event) []event { e[12]["aborted"] = "false"; return e }},
		{"retry pending", func(e []event) []event { e[11]["willRetry"] = true; return e }},
		{"no retry declaration", func(e []event) []event { delete(e[11], "willRetry"); return e }},
		{"agent end differs", func(e []event) []event { e[11]["messages"] = []any{event{"role": "assistant"}}; return e }},
		{"message error", func(e []event) []event { e[9]["message"].(event)["stopReason"] = "error"; return e }},
		{"message aborted", func(e []event) []event { e[9]["message"].(event)["stopReason"] = "aborted"; return e }},
		{"message length", func(e []event) []event { e[9]["message"].(event)["stopReason"] = "length"; return e }},
		{"message pending", func(e []event) []event { e[9]["message"].(event)["stopReason"] = "pending"; return e }},
		{"message tool use", func(e []event) []event { e[9]["message"].(event)["stopReason"] = "toolUse"; return e }},
		{"error despite stop", func(e []event) []event { e[9]["message"].(event)["errorMessage"] = "provider error"; return e }},
		{"no final text", func(e []event) []event { e[9]["message"].(event)["content"] = []any{}; return e }},
		{"tool call block", func(e []event) []event {
			e[9]["message"].(event)["content"] = []event{{"type": "toolCall", "name": "read"}}
			return e
		}},
		{"system enabled tools", func(e []event) []event { e[4]["message"].(event)["toolsAdded"] = []event{{"name": "read"}}; return e }},
		{"tool result", func(e []event) []event { e[10]["toolResults"] = []event{{"role": "toolResult"}}; return e }},
		{"turn end differs", func(e []event) []event {
			e[10]["message"] = event{"role": "assistant", "stopReason": "stop", "content": []event{{"type": "text", "text": "{}"}}}
			return e
		}},
		{"tool execution", func(e []event) []event {
			return append(e[:9], append([]event{{"type": "tool_execution_start", "toolName": "read"}}, e[9:]...)...)
		}},
		{"streaming error", func(e []event) []event {
			e[8]["assistantMessageEvent"] = event{"type": "error", "reason": "error"}
			return e
		}},
		{"streaming tool call", func(e []event) []event {
			e[8]["assistantMessageEvent"] = event{"type": "toolcall_start", "toolName": "read"}
			return e
		}},
		{"automatic retry", func(e []event) []event {
			return append(e[:9], append([]event{{"type": "auto_retry_end", "success": false, "finalError": "failed"}}, e[9:]...)...)
		}},
		{"compaction", func(e []event) []event {
			return append(e[:9], append([]event{{"type": "compaction_start", "reason": "overflow"}}, e[9:]...)...)
		}},
		{"extension entry", func(e []event) []event {
			return append(e[:9], append([]event{{"type": "entry_appended", "entry": event{}}}, e[9:]...)...)
		}},
		{"trailing event", func(e []event) []event { return append(e, event{"type": "agent_start"}) }},
		{"duplicate settled", func(e []event) []event { return append(e, e[12]) }},
		{"negative cost", func(e []event) []event {
			e[9]["message"].(event)["usage"].(event)["cost"] = event{"total": -1}
			return e
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := streamEvents(tc.change(piEvents(testDecision)))
			result, err := fakeAdapter(stream).Run(context.Background(), testProfile("pi"), testTurn())
			if tc.name == "success" || tc.name == "provider done success" {
				if err != nil || string(result.Decision) != testDecision {
					t.Fatalf("valid Pi response failed: %v", err)
				}
				if len(result.Usage.Reports) != 1 || result.Usage.CostUSD == nil || *result.Usage.CostUSD != 0.01 {
					t.Fatalf("usage not retained: %+v", result.Usage)
				}
			} else if err == nil || result.Decision != nil {
				t.Fatalf("unsafe Pi response authorized: %s, %v", result.Decision, err)
			}
		})
	}
}

func TestPiStreamingUsage(t *testing.T) {
	report := func(cost any) event {
		return event{"input": 20, "output": 4, "totalTokens": 24, "cost": event{"total": cost}}
	}
	for _, tc := range []struct {
		name       string
		updates    []event
		finalUsage event
		finish     bool
		failedStop bool
		wantCost   *float64
		wantReport event
	}{
		{"interrupted", []event{report(0.004)}, nil, false, false, new(0.004), report(0.004)},
		{"repeated cumulative updates", []event{report(0.002), report(0.004), report(0.004)}, nil, false, false, new(0.004), report(0.004)},
		{"final supersedes updates", []event{report(0.002), report(0.004)}, nil, true, false, new(0.01), nil},
		{"final supersedes unknown cost", []event{{"input": 20}}, nil, true, false, new(0.01), nil},
		{"final cost unknown", []event{report(0.004)}, event{"input": 20}, true, false, nil, event{"input": 20}},
		{"final usage absent", []event{report(0.004)}, event{}, true, false, nil, report(0.004)},
		{"provider failed stop", []event{report(0.004)}, nil, false, true, new(0.004), report(0.004)},
		{"unknown partial cost", []event{{"input": 20}}, nil, false, false, nil, event{"input": 20}},
		{"invalid partial cost", []event{report(-1)}, nil, false, false, nil, report(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := piEvents(testDecision)
			if tc.finalUsage != nil {
				if len(tc.finalUsage) == 0 {
					delete(e[9]["message"].(event), "usage")
				} else {
					e[9]["message"].(event)["usage"] = tc.finalUsage
				}
			}
			stream := streamEvents(e[:8])
			for _, usage := range tc.updates {
				providerEvent := event{"type": "text_delta", "contentIndex": 0, "delta": "partial"}
				if tc.failedStop {
					providerEvent = event{"type": "done", "reason": "length"}
				}
				stream += streamEvents([]event{{"type": "message_update", "usage": usage, "assistantMessageEvent": providerEvent}})
			}
			if tc.finish {
				stream += streamEvents(e[9:])
			} else {
				stream += `{"type":"message_end","message":`
			}
			result, err := fakeAdapter(stream).Run(context.Background(), testProfile("pi"), testTurn())
			if tc.finish {
				if err != nil || string(result.Decision) != testDecision {
					t.Fatalf("complete response rejected: %v", err)
				}
			} else if err == nil || result.Decision != nil {
				t.Fatalf("incomplete response authorized: %v", err)
			}
			if tc.wantCost == nil {
				if result.Usage.CostUSD != nil {
					t.Fatalf("unknown or invalid cost treated as known: %+v", result.Usage)
				}
			} else if result.Usage.CostUSD == nil || *result.Usage.CostUSD != *tc.wantCost {
				t.Fatalf("cumulative cost: got %+v, want %v", result.Usage, *tc.wantCost)
			}
			wantReport := tc.wantReport
			if wantReport == nil {
				wantReport = e[9]["message"].(event)["usage"].(event)
			}
			raw, err := json.Marshal(wantReport)
			if err != nil {
				t.Fatal(err)
			}
			wantReports := len(tc.updates)
			if tc.finish && e[9]["message"].(event)["usage"] != nil {
				wantReports++
			}
			if len(result.Usage.Reports) != wantReports || !sameJSON(result.Usage.Reports[wantReports-1], raw) {
				t.Fatalf("cumulative usage reports not retained: %+v", result.Usage)
			}
		})
	}
}

func TestPiUnknownUsageAndUnicodeFraming(t *testing.T) {
	for _, unknown := range []string{"usage", "cost", "total"} {
		t.Run(unknown, func(t *testing.T) {
			e := piEvents(testDecision)
			message := e[9]["message"].(event)
			switch unknown {
			case "usage":
				delete(message, "usage")
			case "cost":
				delete(message["usage"].(event), "cost")
			case "total":
				delete(message["usage"].(event)["cost"].(event), "total")
			}
			result, err := fakeAdapter(streamEvents(e)).Run(context.Background(), testProfile("pi"), testTurn())
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage.CostUSD != nil {
				t.Fatal("unknown Pi cost treated as zero")
			}
		})
	}
	decision := `{"version":1,"turn_id":"a` + "\u2028\u2029" + `b","snapshot_id":"s","actions":[]}`
	stream := piStream(decision)
	stream = strings.ReplaceAll(strings.ReplaceAll(stream, `\u2028`, "\u2028"), `\u2029`, "\u2029")
	result, err := fakeAdapter(stream).Run(context.Background(), testProfile("pi"), testTurn())
	if err != nil || string(result.Decision) != decision {
		t.Fatalf("Unicode separators split a JSONL record: %v", err)
	}
}
