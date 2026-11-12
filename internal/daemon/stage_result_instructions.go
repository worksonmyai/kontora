package daemon

func stageResultPrompts(p spawnAgentParams, rendered string) (prompt, brief string) {
	if p.annotation {
		return rendered, p.cfg.SystemPrompt
	}
	return rendered + stageResultInstructions(), stageBrief(p.cfg.SystemPrompt, p.ticketID, p.stageName)
}

// stageResultInstructions applies to ticket execution, not annotation-only runs.
// Keep it separate from supervision: reporting grants no Git or setup authority.
func stageResultInstructions() string {
	return `

## Mandatory stage result
Before ending, submit one terminal result using Pi's kontora_result({outcome, summary, next_step?}) tool, or the CLI:
  kontora result --input-file /absolute/path/result.json TICKET_ID
Use the actual current ticket ID. Do not supply or override invocation identity.
Create the JSON payload with a file-writing tool in a unique scratch file outside the repository.
The CLI also accepts stdin when --input-file is absent.
The payload has outcome "complete" or "blocked"; summary is required and blocked also requires next_step.
Complete means the current stage's assigned work, not the whole ticket.
A review stage can be complete with findings while the ticket still needs fixes.
Do not report complete while assigned implementation or required acceptance evidence remains unmet.
Fixture work alone is not complete if the assigned stage still requires production foundations or other acceptance evidence.
Report blocked when requirements or authority prevent further work, with the exact next step needed.
Missing or invalid results pause the same stage after a clean runner exit; a clean runner exit alone cannot advance it.
Notes, summaries, final prose, questions, and checkpoints are not terminal results.
Check the acknowledgement. Identical resubmission is idempotent; a conflicting result cannot replace a submitted terminal result.
Do not use notes as a fallback if submission fails.
A complete report does not override execution failures, timeouts, provider errors, hooks, child checks, or human acceptance.
Do not change your own lifecycle status.

## Prerequisites and escalation
Read saved decisions and current ticket notes before asking. Reuse applicable answers; investigate the missing prerequisite through the repository, tests, documentation, and history.
Resolve permitted local setup without asking. Continue independent authorized work where possible.
If a prerequisite remains unavailable, ask a concrete question naming the missing resource, authority, or scope decision.
Include attempted checks, concrete options, their costs, and a recommended default.
Save the question and answer together immediately on the current ticket, preserving selected options and free-text details.
Distinguish operator answers from assumptions and confirm persistence before continuing.
Do not weaken acceptance requirements without an explicit operator decision. Do not substitute emulation for required real-device evidence.
If no authorized work remains, submit blocked with a summary and actionable next_step, then stop.
Retry requires explicit operator action. Saving an answer, reading a notification, or dismissing attention does not restart work.
On an explicit Retry, inspect saved decisions, current children, branches, worktrees, dirty files, and interrupted Git operations before resuming.
Do not restart children blindly.
`
}
