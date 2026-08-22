package assistant

import (
	"slices"
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
	DecisionDelete Decision = "delete"
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
// caught by classifySegment before this map is read.
var kontoraReadVerbs = map[string]bool{
	"ls": true, "view": true, "search": true, "logs": true, "activity": true,
	"changes": true, "stats": true, "sessions": true, "config": true,
	"help": true, "version": true, "fmt": true, "completion": true,
}

// kontoraWriteVerbs are the CLI verbs that change a ticket.
var kontoraWriteVerbs = map[string]bool{
	"new": true, "update": true, "init": true, "run": true, "pause": true,
	"retry": true, "cancel": true, "done": true, "move": true, "skip": true,
	"set-stage": true, "note": true, "summary": true, "dep": true, "undep": true,
	"link": true, "unlink": true, "archive": true,
}

// shellChainOps separate one command from the next in a shell line. The bare
// "&" is listed after "&&" so the two-character operator splits first, and it
// has to be listed at all: without it a line that starts with a read verb and
// backgrounds it hides everything after the ampersand from the split.
var shellChainOps = []string{"&&", "||", ";", "&", "|", "\n"}

// shellIndirection is what makes a command line unreadable to the split below:
// what runs inside a substitution or on the far side of a redirection is not a
// segment this can classify.
var shellIndirection = []string{"$(", "`", ">", "<"}

// Classify reads one tool call and reports what it would do. It is fail-closed:
// a tool with no rule, and a shell command that is not plainly a kontora read
// verb, both count as a write. The agent has read tools of its own for anything
// it only needs to look at, so nothing useful is lost by refusing to reason
// about arbitrary shell.
func Classify(tool string, input map[string]any) Decision {
	name := strings.ToLower(strings.TrimSpace(tool))
	if readTools[name] {
		return DecisionRead
	}
	if name == "bash" {
		return classifyBash(stringField(input, "command"))
	}
	return DecisionWrite
}

// classifyBash reads a whole command line. Every command in it must be a
// kontora read verb for the line to be a read, and one kontora delete anywhere
// in it makes the line a delete.
func classifyBash(command string) Decision {
	command = strings.TrimSpace(command)
	if command == "" {
		return DecisionWrite
	}
	for _, op := range shellIndirection {
		if strings.Contains(command, op) {
			return DecisionWrite
		}
	}
	worst := DecisionRead
	for _, segment := range splitAny(command, shellChainOps) {
		switch classifySegment(segment) {
		case DecisionDelete:
			return DecisionDelete
		case DecisionWrite:
			worst = DecisionWrite
		}
	}
	return worst
}

// classifySegment reads one command. Anything that is not `kontora <verb>` is a
// write, including an empty segment, which only a malformed line produces.
func classifySegment(segment string) Decision {
	fields := strings.Fields(segment)
	if len(fields) < 2 {
		return DecisionWrite
	}
	binary := fields[0]
	if i := strings.LastIndexByte(binary, '/'); i >= 0 {
		binary = binary[i+1:]
	}
	if binary != "kontora" {
		return DecisionWrite
	}
	switch verb := fields[1]; {
	case verb == "delete":
		return DecisionDelete
	case verb == "config" && slices.Contains(fields[2:], "edit"):
		// `kontora config edit` writes the daemon config, and headless it also
		// hangs on an editor that never returns.
		return DecisionWrite
	case kontoraReadVerbs[verb]:
		return DecisionRead
	case kontoraWriteVerbs[verb]:
		return DecisionWrite
	default:
		return DecisionWrite
	}
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
		if d == DecisionDelete {
			return VerdictPark
		}
		return VerdictAllow
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

// splitAny splits s on every one of seps.
func splitAny(s string, seps []string) []string {
	parts := []string{s}
	for _, sep := range seps {
		var next []string
		for _, p := range parts {
			next = append(next, strings.Split(p, sep)...)
		}
		parts = next
	}
	return parts
}

// stringField reads a string out of a decoded JSON object, or "" when the key
// is absent or holds something else.
func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
