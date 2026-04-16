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
