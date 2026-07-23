package config

import "time"

// ReworkStageName is the stage name used for the built-in plannotator-driven
// rework loop. The daemon treats it specially unless the user overrides it.
const ReworkStageName = "rework"

const defaultReworkPrompt = `Ticket: {{ .Ticket.Title }}

The reviewer requested changes. Their feedback:

{{ plannotatorReview }}

Apply the changes and continue the work.`

func defaultReworkStage() Stage {
	return Stage{
		Prompt:  defaultReworkPrompt,
		Timeout: Duration{Duration: 30 * time.Minute},
	}
}

// DefaultFailurePatterns are applied to any agent that does not declare its own
// failure_patterns. They match the error surfaces coding agents print when the
// underlying LLM API fails but the process still exits cleanly: quota/usage
// limits, lost auth, context overflow, and provider rate-limit/billing errors.
//
// Patterns are anchored or phrase-specific on purpose so they match an agent's
// error UI, not source code or agent prose — implementing a rate limiter or
// discussing quotas must not pause the ticket. Bare words like "rate limit" or
// "quota exceeded" are deliberately excluded for that reason.
var DefaultFailurePatterns = []string{
	`(?im)^\s*API Error:`,                // Claude Code: 4xx/5xx, overloaded (529), stream/socket timeouts, ECONNRESET
	`(?i)Please run /login`,              // Claude Code auth: "Not logged in" / "Invalid API key" · Please run /login
	`(?i)usage limit reached`,            // "Claude AI usage limit reached"
	`(?i)You've hit your (usage )?limit`, // Claude usage/session limit stop
	`(?i)Prompt is too long`,             // context window exceeded
	`(?i)insufficient_quota`,             // OpenAI-backed agents: billing error code
	`(?i)exceeded your current quota`,    // OpenAI-backed agents: billing message
	`(?i)Rate limit reached for`,         // OpenAI-backed agents: "Rate limit reached for <model>"
}
