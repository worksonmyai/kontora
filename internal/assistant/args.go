package assistant

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/config"
)

// TurnSpec describes one headless assistant turn. It is a sibling of the
// daemon's buildAgentArgs rather than an overload of it: that one is
// ticket-shaped, with a tmux wait channel, a compaction channel and a resume
// record, none of which a turn has.
type TurnSpec struct {
	// Prompt is the user's message. It goes last, as the positional argument.
	Prompt string
	// SystemPrompt is appended to the agent's own.
	SystemPrompt string
	// SessionID is the session this turn runs against. Resume decides whether
	// it opens that session or continues it.
	SessionID string
	Resume    bool
	// SessionDir is where pi keeps the thread's session JSONL. Unused by claude,
	// which keys its own files by working directory.
	SessionDir string
	// GateFile is the per-turn settings JSON (claude) or extension (pi) that
	// routes every tool call through the gate.
	GateFile string
	// AddDirs are the directories claude may reach outside its cwd. A thread
	// runs in the tickets dir, so run logs and worktrees need naming.
	AddDirs []string
}

// BuildArgs returns the argument list for one turn. A model or effort the agent
// cannot run together is rejected here as well as in config validation, because
// the assistant's model and effort are only paired with the agent at spawn time.
func BuildArgs(agentCfg config.Agent, model, effort string, spec TurnSpec) ([]string, error) {
	if err := agentCfg.CheckEffort(model, effort); err != nil {
		return nil, err
	}
	args := agentCfg.ArgsWith(model, effort)

	switch agentCfg.Kind() {
	case config.AgentKindClaude:
		if spec.GateFile != "" {
			args = append(args, "--settings", spec.GateFile)
		}
		if len(spec.AddDirs) > 0 {
			// --add-dir is variadic, so it has to be followed by another flag
			// rather than by the positional prompt.
			args = append(args, "--add-dir")
			args = append(args, spec.AddDirs...)
		}
		if spec.SystemPrompt != "" {
			args = append(args, "--append-system-prompt", spec.SystemPrompt)
		}
		args = append(args, "--print", "--output-format", "stream-json", "--verbose")
		if spec.Resume {
			args = append(args, "-r", spec.SessionID)
		} else {
			args = append(args, "--session-id", spec.SessionID)
		}
	case config.AgentKindPi:
		if spec.GateFile != "" {
			args = append(args, "-e", spec.GateFile)
		}
		if spec.SystemPrompt != "" {
			args = append(args, "--append-system-prompt", spec.SystemPrompt)
		}
		// pi resumes by naming the same session id every turn, so there is no
		// separate resume flag. The transcript is read from --session-dir, not
		// from stdout: --mode json emits wire events, which is a different
		// shape from the session records logfmt.EventsPi parses.
		args = append(args, "--print", "--session-id", spec.SessionID, "--session-dir", spec.SessionDir)
	default:
		return nil, fmt.Errorf("agent %s: the assistant only supports claude and pi", agentCfg.Binary)
	}

	if spec.Prompt != "" {
		args = append(args, spec.Prompt)
	}
	return args, nil
}
