package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Environment variables the daemon sets on an assistant turn's agent. The
// address and the nonce together are what lets the gate tell this turn's agent
// from an unrelated local process on a tokenless loopback daemon.
const (
	AssistantURLEnv    = "KONTORA_URL"
	AssistantTokenEnv  = "KONTORA_TOKEN"
	AssistantThreadEnv = "KONTORA_ASSISTANT_THREAD"
	AssistantNonceEnv  = "KONTORA_ASSISTANT_NONCE"
)

// claudeHookInput is the PreToolUse payload claude writes on the hook's stdin.
// Only the two fields the classifier reads are decoded.
type claudeHookInput struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

// AssistantGateAsker is the daemon call the gate makes. It is an interface so
// the command can be tested without a daemon.
type AssistantGateAsker interface {
	Ask(thread, nonce, tool string, input map[string]any) (allow bool, reason string, err error)
}

// AssistantGate reads one PreToolUse payload, asks the daemon whether the call
// may run, and writes claude's decision.
//
// It is fail-closed in both directions: a payload it cannot read and a daemon
// it cannot reach both come back as a denial, because the alternative is a
// read-only thread that writes whenever the gate is broken. The reason names
// what went wrong, so the agent reports it rather than retrying.
func AssistantGate(in io.Reader, out io.Writer, ask AssistantGateAsker) error {
	thread := os.Getenv(AssistantThreadEnv)
	nonce := os.Getenv(AssistantNonceEnv)

	var payload claudeHookInput
	if err := json.NewDecoder(in).Decode(&payload); err != nil {
		return writeClaudeDecision(out, false, "The Kontora assistant gate could not read the tool call: "+err.Error())
	}
	if thread == "" || nonce == "" {
		return writeClaudeDecision(out, false, "The Kontora assistant gate is not configured for this run, so no tool call can be approved.")
	}

	allow, reason, err := ask.Ask(thread, nonce, payload.ToolName, payload.ToolInput)
	if err != nil {
		return writeClaudeDecision(out, false, "The Kontora assistant gate could not be reached: "+err.Error())
	}
	return writeClaudeDecision(out, allow, reason)
}

// writeClaudeDecision writes the PreToolUse hook output claude reads. An
// explicit allow is what lets a headless run use a tool at all: without it the
// permission system would stop and wait for a prompt nobody can answer.
func writeClaudeDecision(out io.Writer, allow bool, reason string) error {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	body := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}
