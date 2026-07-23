package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// apiErrorMarker gates JSON parsing to lines that could carry the error flag,
// so oversized tool-result lines are skipped cheaply instead of parsed.
var apiErrorMarker = []byte("isApiErrorMessage")

// scanClaudeSessionError scans a Claude session JSONL file for a synthetic
// API-error message. Claude Code writes these entries (isApiErrorMessage=true,
// model="<synthetic>") when it gives up on a turn: usage/quota limits, API
// errors (429/500/529), auth failures ("Not logged in", "Invalid API key"),
// "Prompt is too long", and connection drops. In interactive mode this still
// fires the Stop hook, so kontora sees a clean exit and would otherwise treat
// the failed run as success.
//
// It returns the error text of the last such message and true if one is found.
// The whole message is parsed leniently so a flag is honored even if the
// content shape is unexpected.
func scanClaudeSessionError(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	// Read line by line without a size cap: a single oversized line (e.g. a
	// large tool result) must not abort the scan and hide a later error entry.
	reader := bufio.NewReader(f)
	var reason string
	var found bool
	for {
		line, readErr := reader.ReadBytes('\n')
		if bytes.Contains(line, apiErrorMarker) {
			var ev sessionErrEntry
			if json.Unmarshal(line, &ev) == nil && ev.IsAPIErrorMessage {
				found = true
				if t := errMessageText(ev.Message); t != "" {
					reason = t
				} else if reason == "" {
					reason = "agent reported an API error"
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	return reason, found
}

type sessionErrEntry struct {
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	Message           json.RawMessage `json:"message"`
}

// errMessageText pulls the first text block out of a session message, tolerating
// both the array-of-blocks content shape and a bare string.
func errMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil || len(m.Content) == 0 {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Text != "" {
				return b.Text
			}
		}
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	return ""
}

// matchFailurePatterns reports the first configured pattern that matches the
// agent output. Patterns are validated at config load, so a compile error here
// is treated as a non-match rather than a hard failure.
func matchFailurePatterns(content string, patterns []string) (string, bool) {
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		if re.MatchString(content) {
			return fmt.Sprintf("output matched failure pattern %q", p), true
		}
	}
	return "", false
}
