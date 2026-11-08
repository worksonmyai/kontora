package assistant

import (
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
)

// Decision is what one tool call would do to Kontora's state.
type Decision string

const (
	// DecisionRead has no side effects.
	DecisionRead Decision = "read"
	// DecisionWrite changes something.
	DecisionWrite Decision = "write"
	// DecisionDelete removes a ticket. It is separated from a write because it
	// is the one change with no cheap undo, so it is held even in auto.
	DecisionDelete  Decision = "delete"
	DecisionUnknown Decision = "unknown"
)

// Verdict is what the gate answers a tool call with.
type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictDeny  Verdict = "deny"
	// VerdictPark holds the call until a person approves or skips it.
	VerdictPark Verdict = "park"
)

// readTools name a tool that cannot change anything. Both agents' spellings are
// covered: claude sends CamelCase names, pi lowercase ones, and Classify
// lowercases before the lookup.
var readTools = map[string]bool{
	"read":         true,
	"glob":         true,
	"grep":         true,
	"ls":           true,
	"list":         true,
	"websearch":    true,
	"webfetch":     true,
	"notebookread": true,
	"bashoutput":   true,
	// The agent's own scratch list. It touches nothing outside the session.
	"todowrite": true,
}

// kontoraReadVerbs are the CLI verbs that only print. `config` is here for its
// printing form only: `config edit` opens $EDITOR on the daemon config and is
// caught by classifyCall before this map is read.
var kontoraReadVerbs = map[string]bool{
	"ls": true, "view": true, "search": true, "logs": true, "activity": true,
	"changes": true, "stats": true, "sessions": true, "config": true,
	"help": true, "version": true, "fmt": true, "completion": true,
	"skills": true,
}

// kontoraWriteVerbs are the CLI verbs that change a ticket.
var kontoraWriteVerbs = map[string]bool{
	"new": true, "update": true, "init": true, "run": true, "schedule": true, "pause": true,
	"retry": true, "cancel": true, "done": true, "move": true, "skip": true,
	"set-stage": true, "note": true, "summary": true, "dep": true, "undep": true,
	"link": true, "unlink": true, "archive": true,
}

var writeTools = map[string]bool{
	"edit": true, "write": true, "notebookedit": true,
}

// Classify requires approval for tools and shell syntax it cannot classify.
func Classify(tool string, input map[string]any) Decision {
	name := strings.ToLower(strings.TrimSpace(tool))
	if readTools[name] {
		return DecisionRead
	}
	if name == "bash" {
		return classifyBash(stringField(input, "command"))
	}
	if writeTools[name] {
		return DecisionWrite
	}
	return DecisionUnknown
}

// Resolve maps an autonomy mode and a classification to what the gate does.
// An autonomy the caller never validated parks rather than deciding on its own:
// a person answering is neither a silent denial nor a silent write.
func Resolve(autonomy string, d Decision) Verdict {
	if d == DecisionRead {
		return VerdictAllow
	}
	switch autonomy {
	case config.AutonomyRead:
		return VerdictDeny
	case config.AutonomyAuto:
		if d == DecisionWrite {
			return VerdictAllow
		}
		return VerdictPark
	default:
		return VerdictPark
	}
}

// DenyReason is what the agent is told when a write is refused, so it stops
// retrying the call rather than reading the block as a transient failure.
func DenyReason(d Decision, autonomy string) string {
	switch {
	case autonomy == config.AutonomyRead:
		return "This assistant thread is read-only. Do not retry: tell the user what you would have run and ask them to switch the thread to ask or auto."
	case d == DecisionDelete:
		return "The user skipped this deletion. Do not retry it."
	default:
		return "The user skipped this change. Do not retry it."
	}
}

// stringField reads a string out of a decoded JSON object, or "" when the key
// is absent or holds something else.
func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
