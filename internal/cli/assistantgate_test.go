package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubAsker records what the gate asked the daemon and answers with what the
// case wants.
type stubAsker struct {
	allow  bool
	reason string
	err    error
	tool   string
	input  map[string]any
	thread string
	nonce  string
	calls  int
}

func (s *stubAsker) Ask(thread, nonce, tool string, input map[string]any) (bool, string, error) {
	s.calls++
	s.thread, s.nonce, s.tool, s.input = thread, nonce, tool, input
	return s.allow, s.reason, s.err
}

func TestAssistantGate(t *testing.T) {
	tests := []struct {
		name       string
		stdin      string
		env        map[string]string
		asker      stubAsker
		wantAllow  bool
		wantReason string
		wantAsked  bool
	}{
		{
			name:      "an approved call is allowed",
			stdin:     `{"tool_name":"Bash","tool_input":{"command":"kontora ls"}}`,
			env:       map[string]string{AssistantThreadEnv: "t1", AssistantNonceEnv: "n"},
			asker:     stubAsker{allow: true},
			wantAllow: true,
			wantAsked: true,
		},
		{
			name:       "a refused call carries the reason",
			stdin:      `{"tool_name":"Bash","tool_input":{"command":"kontora run kon-7d21"}}`,
			env:        map[string]string{AssistantThreadEnv: "t1", AssistantNonceEnv: "n"},
			asker:      stubAsker{allow: false, reason: "read-only"},
			wantReason: "read-only",
			wantAsked:  true,
		},
		{
			name:       "a daemon that cannot be reached denies",
			stdin:      `{"tool_name":"Bash","tool_input":{}}`,
			env:        map[string]string{AssistantThreadEnv: "t1", AssistantNonceEnv: "n"},
			asker:      stubAsker{err: errors.New("connection refused")},
			wantReason: "could not be reached",
			wantAsked:  true,
		},
		{
			name:       "an unreadable payload denies without asking",
			stdin:      `not json`,
			env:        map[string]string{AssistantThreadEnv: "t1", AssistantNonceEnv: "n"},
			asker:      stubAsker{allow: true},
			wantReason: "could not read the tool call",
		},
		{
			name:       "a run with no thread or nonce denies without asking",
			stdin:      `{"tool_name":"Bash","tool_input":{}}`,
			asker:      stubAsker{allow: true},
			wantReason: "not configured for this run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var out bytes.Buffer
			require.NoError(t, AssistantGate(strings.NewReader(tt.stdin), &out, &tt.asker))

			var decoded struct {
				HookSpecificOutput struct {
					HookEventName            string `json:"hookEventName"`
					PermissionDecision       string `json:"permissionDecision"`
					PermissionDecisionReason string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
			assert.Equal(t, "PreToolUse", decoded.HookSpecificOutput.HookEventName)

			want := "deny"
			if tt.wantAllow {
				want = "allow"
			}
			assert.Equal(t, want, decoded.HookSpecificOutput.PermissionDecision)
			if tt.wantReason != "" {
				assert.Contains(t, decoded.HookSpecificOutput.PermissionDecisionReason, tt.wantReason)
			}
			if tt.wantAsked {
				require.Equal(t, 1, tt.asker.calls)
				assert.Equal(t, "t1", tt.asker.thread)
				assert.Equal(t, "n", tt.asker.nonce)
				assert.Equal(t, "Bash", tt.asker.tool)
			} else {
				assert.Zero(t, tt.asker.calls, "the daemon must not be asked when the gate already knows the answer")
			}
		})
	}
}
