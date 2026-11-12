package cli

// Commands is the CLI's command table: the one description of every verb
// and the flags it takes. Both the usage text and the shell completions
// are generated from it, so a new command cannot appear in one and be missing
// from the other.
var Commands = []Command{
	{Name: "ls", Group: "Tickets", Desc: "List tickets (TUI on TTY, static table otherwise)", Config: true, Store: true, Remote: true, Flags: []Flag{
		{Name: "closed", Desc: "Show done/cancelled/closed tickets"},
		{Name: "archived", Desc: "Show archived tickets"},
		{Name: "static", Desc: "Print static table instead of interactive TUI"},
		{Name: "status", Desc: "Only show tickets in this status", Value: "text"},
		{Name: "project", Desc: "Only show tickets for this configured project", Value: "project"},
		{Name: "path", Desc: "Only show tickets for this repository path", Value: "path"},
		{Name: "ready", Desc: "Only show todo tickets whose dependencies are all closed"},
		{Name: "blocked", Desc: "Only show todo tickets waiting on a dependency"},
		{Name: "limit", Desc: "Print at most N tickets", Value: "text"},
		{Name: "json", Desc: "Print JSON instead of a table"},
	}},
	{Name: "search", Group: "Tickets", Desc: "Search tickets by regex across frontmatter and body", Config: true, Store: true, Args: "QUERY", Flags: []Flag{
		{Name: "i", Desc: "Case-insensitive match (default is smart-case)"},
		{Name: "s", Desc: "Case-sensitive match"},
		{Name: "F", Desc: "Treat the query as a literal string, not a regex"},
		{Name: "l", Desc: "Print matching ticket IDs only"},
		{Name: "m", Desc: "Max matching lines per ticket, 0 for all", Value: "text"},
		{Name: "body", Desc: "Search the markdown body only, not the frontmatter"},
		{Name: "json", Desc: "Print results as JSON"},
		{Name: "status", Desc: "Only search tickets with this status", Value: "text"},
		{Name: "project", Desc: "Only search tickets for this configured project", Value: "project"},
		{Name: "path", Desc: "Only search tickets for this repository path", Value: "path"},
		{Name: "pipeline", Desc: "Only search tickets for this pipeline", Value: "pipeline"},
		{Name: "agent", Desc: "Only search tickets for this agent", Value: "agent"},
	}},
	{Name: "new", Group: "Tickets", Desc: "Create a ticket", Config: true, Store: true, Remote: true, Args: "TITLE", Flags: []Flag{
		{Name: "path", Desc: "Repository path", Value: "path"},
		{Name: "pipeline", Desc: "Pipeline name, or \"none\"", Value: "pipeline"},
		{Name: "agent", Desc: "Agent name, or \"none\"", Value: "agent"},
		{Name: "notify", Desc: "Comma-separated notification statuses (\"\" or \"none\" skips the project default)", Value: "text"},
		{Name: "notify-channels", Desc: "Comma-separated notification channels, or \"none\" to disable delivery", Value: "text"},
		{Name: "branch", Desc: "Work branch name", Value: "text"},
		{Name: "base-branch", Desc: "Branch the work branch starts from", Value: "text"},
		{Name: "status", Desc: "Initial status", Value: "text", Choices: []string{"open", "todo"}},
		{Name: "kind", Desc: "Ticket kind", Value: "text", Choices: []string{"epic"}},
		{Name: "parent", Desc: "Epic to file this ticket under", Value: "ticket"},
		{Name: "at", Desc: "Schedule pickup for an RFC 3339 instant", Value: "text"},
		{Name: "after", Desc: "Schedule pickup this long from now, e.g. 24h", Value: "text"},
		{Name: "description-file", Desc: "Read the description from a file ('-' for stdin)", Value: "path"},
		{Name: "quiet", Desc: "Print only the new ticket ID"},
	}},
	{Name: "view", Group: "Tickets", Desc: "Print ticket details", Config: true, Store: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "body", Desc: "Print only the ticket body"},
		{Name: "notes", Desc: "Print the Notes section and all later sections"},
		{Name: "last", Desc: "Print the last N notes and all later sections", Value: "text"},
	}},
	{Name: "edit", Group: "Tickets", Desc: "Open a ticket in $EDITOR", Config: true, Store: true, TicketID: true},
	{Name: "update", Group: "Tickets", Desc: "Update ticket body/frontmatter fields", Config: true, Store: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "body-file", Desc: "Read ticket body from a file ('-' for stdin)", Value: "path"},
		{Name: "pipeline", Desc: "Set pipeline (\"\" or \"none\" clears it)", Value: "pipeline"},
		{Name: "path", Desc: "Set repository path", Value: "path"},
		{Name: "agent", Desc: "Set agent override (\"\" or \"none\" clears it)", Value: "agent"},
		{Name: "branch", Desc: "Set branch (\"\" clears it)", Value: "text"},
		{Name: "base-branch", Desc: "Set base branch (\"\" clears it)", Value: "text"},
		{Name: "notify", Desc: "Set comma-separated notification statuses (\"\" or \"none\" clears it)", Value: "text"},
		{Name: "notify-channels", Desc: "Set comma-separated notification channels (\"\" clears it, \"none\" disables delivery)", Value: "text"},
	}},
	{Name: "delete", Group: "Tickets", Desc: "Delete a ticket file", Config: true, Store: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "f", Desc: "Confirm deletion (required)"},
		{Name: "yes", Desc: "Confirm deletion (required)"},
	}},
	{Name: "init", Group: "Tickets", Desc: "Set up a ticket for daemon processing", Config: true, Store: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "pipeline", Desc: "Pipeline name, or \"none\" (required in remote mode)", Value: "pipeline"},
		{Name: "path", Desc: "Repository path (required in remote mode)", Value: "path"},
		{Name: "agent", Desc: "Agent name, or \"none\"", Value: "agent"},
		{Name: "stage", Desc: "Starting pipeline stage", Value: "stage"},
		{Name: "status", Desc: "Initial status", Value: "text", Choices: []string{"open", "todo"}},
	}},
	{Name: "run", Group: "Running work", Desc: "Enqueue a ticket for processing", Config: true, Store: true, Remote: true, TicketID: true},
	{Name: "schedule", Group: "Running work", Desc: "Set or clear a ticket's future pickup time", Config: true, Store: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "at", Desc: "Pickup time as an RFC 3339 instant", Value: "text"},
		{Name: "after", Desc: "Pickup time this long from now, e.g. 24h", Value: "text"},
		{Name: "clear", Desc: "Remove the schedule and leave the ticket open"},
	}},
	{Name: "done", Group: "Running work", Desc: "Mark a ticket as done", Config: true, Store: true, Remote: true, TicketID: true},
	{Name: "move", Group: "Running work", Desc: "Move a ticket to any status, including custom ones", Config: true, Store: true, Remote: true, TicketID: true, Args: "STATUS"},
	{Name: "note", Group: "Tickets", Desc: "Append a note to a ticket", Config: true, Store: true, Remote: true, TicketID: true, Args: "[TEXT]"},
	{Name: "summary", Group: "Tickets", Desc: "Set a ticket's summary", Config: true, Store: true, Remote: true, TicketID: true, Args: "[TEXT]"},
	{Name: "pause", Group: "Running work", Desc: "Pause a running ticket", Config: true, Store: true, Remote: true, TicketID: true},
	{Name: "retry", Group: "Running work", Desc: "Re-queue a paused ticket", Config: true, Store: true, Remote: true, TicketID: true},
	{Name: "skip", Group: "Running work", Desc: "Skip to the next pipeline stage", Config: true, Store: true, Remote: true, TicketID: true},
	{Name: "set-stage", Group: "Running work", Desc: "Move ticket to a specific pipeline stage", Config: true, Store: true, Remote: true, TicketID: true, Args: "STAGE"},
	{Name: "cancel", Group: "Running work", Desc: "Cancel a ticket", Config: true, Store: true, Remote: true, TicketID: true},
	{Name: "dep", Group: "Tickets", Desc: "Make a ticket wait on another one", Config: true, Store: true, Remote: true, TicketID: true, Args: "DEPENDENCY_ID"},
	{Name: "undep", Group: "Tickets", Desc: "Drop a dependency from a ticket", Config: true, Store: true, Remote: true, TicketID: true, Args: "DEPENDENCY_ID"},
	{Name: "link", Group: "Tickets", Desc: "Relate a ticket to one or more others", Config: true, Store: true, Remote: true, TicketID: true, Args: "TICKET_ID..."},
	{Name: "unlink", Group: "Tickets", Desc: "Remove a relation between tickets", Config: true, Store: true, Remote: true, TicketID: true, Args: "TICKET_ID..."},
	{Name: "archive", Group: "Tickets", Desc: "Archive old done/cancelled tickets", Config: true, Store: true, Flags: []Flag{
		{Name: "days", Desc: "Required: age threshold in days", Value: "text"},
		{Name: "dry-run", Desc: "List what would be archived, write nothing"},
		{Name: "path", Desc: "Only archive tickets for this repository path", Value: "path"},
		{Name: "project", Desc: "Only archive tickets for this configured project", Value: "project"},
		{Name: "status", Desc: "Only archive tickets with this status", Value: "text", Choices: []string{"done", "cancelled", "closed"}},
		{Name: "yes", Short: "y", Desc: "Skip the confirmation prompt"},
	}},
	{Name: "logs", Group: "Inspecting a run", Desc: "Show agent logs for a ticket", Config: true, Store: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "stage", Desc: "Stage name", Value: "stage"},
	}},
	{Name: "activity", Group: "Inspecting a run", Desc: "Show a stage run's transcript", Config: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "stage", Desc: "Stage name", Value: "stage"},
		{Name: "run", Desc: "Run number within the stage", Value: "text"},
	}},
	{Name: "sessions", Group: "Inspecting a run", Desc: "Print the file paths behind a ticket's runs", Config: true, Store: true, TicketID: true, Flags: []Flag{
		{Name: "stage", Desc: "Only this stage", Value: "stage"},
		{Name: "run", Desc: "Only this run number within the stage", Value: "text"},
		{Name: "logs", Desc: "Print the stage log paths instead of the session files"},
		{Name: "events", Desc: "Print the activity sidecar paths instead of the session files"},
		{Name: "all", Desc: "Print every file of every run"},
	}},
	{Name: "changes", Group: "Inspecting a run", Desc: "Show a ticket's commits and changed files", Config: true, Remote: true, TicketID: true},
	{Name: "stats", Group: "Inspecting a run", Desc: "Show throughput and cycle-time figures", Config: true, Remote: true, Flags: []Flag{
		{Name: "range", Desc: "Window to report on", Value: "text", Choices: []string{"1d", "1w", "30d", "90d", "all"}},
		{Name: "project", Desc: "Only count tickets for this configured project", Value: "project"},
		{Name: "pipeline", Desc: "Only count tickets for this pipeline", Value: "pipeline"},
	}},
	{Name: "estimate-compaction", Group: "Other", Desc: "Estimate token savings from checkpoint compaction", Config: true, Flags: []Flag{
		{Name: "logs-dir", Desc: "Path to logs directory (overrides config logs_dir)", Value: "path"},
		{Name: "stage", Desc: "Pipeline stage to analyze", Value: "stage"},
		{Name: "thresholds", Desc: "Comma-separated token thresholds", Value: "text"},
		{Name: "top", Desc: "Number of top sessions to show", Value: "text"},
	}},
	{Name: "result", Group: "Other", Desc: "Report a terminal stage result for the current invocation", TicketID: true, Flags: []Flag{
		{Name: "input-file", Desc: "Absolute path to result JSON (default: stdin)", Value: "path"},
		{Name: "verdict", Desc: "Declared workflow verdict (result version 2)", Value: "text"},
	}},
	{Name: "phase-complete", Group: "Other", Desc: "Signal a phase boundary from inside a checkpointing run", TicketID: true, Flags: []Flag{
		{Name: "completed", Desc: "The phase that just finished", Value: "text"},
		{Name: "next", Desc: "The phase to begin next", Value: "text"},
	}},
	{Name: "review", Group: "Running work", Desc: "Open a ticket's diff in Plannotator on the daemon host", Config: true, Remote: true, TicketID: true},
	{Name: "annotate", Group: "Running work", Desc: "Open a ticket's markdown in Plannotator on the daemon host", Config: true, Remote: true, TicketID: true},
	{Name: "automation", Group: "Running work", Desc: "Manage and inspect scheduled agent automations", Subcommands: automationCommands()},
	{Name: "attach", Group: "Running work", Desc: "Attach to a running ticket or automation", Config: true, Store: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "rw", Desc: "Attach in read-write mode"},
		{Name: "operator-token", Desc: "Operator credential (defaults to KONTORA_OPERATOR_TOKEN)", Value: "text"},
	}},
	{Name: "start", Group: "Getting started", Desc: "Start the daemon", Config: true, Store: true, Flags: []Flag{
		{Name: "address", Desc: "Web server listen address", Value: "text"},
		{Name: "port", Desc: "Web server port", Value: "text"},
	}},
	{Name: "setup", Group: "Getting started", Desc: "Create the configuration file", Config: true, Flags: []Flag{
		{Name: "agent", Desc: "Print setup instructions for a coding agent"},
	}},
	{Name: "doctor", Group: "Getting started", Desc: "Validate prerequisites and configuration", Config: true},
	{Name: "config", Group: "Other", Desc: "Show effective configuration", Config: true, Remote: true, Subcommands: []Command{{Name: "edit", Desc: "Edit configuration", Config: true, Remote: true}}},
	{Name: "fmt", Group: "Other", Desc: "Format stream-json from stdin"},
	{Name: "skills", Group: "Other", Desc: "Show the built-in reference docs", Subcommands: []Command{{Name: "list", Desc: "List topics or sections", Args: "[TOPIC]"}, {Name: "show", Desc: "Show a topic or section", Args: "TOPIC [SECTION]"}}},
	{Name: "version", Group: "Getting started", Desc: "Print version"},
	{Name: "completion", Group: "Other", Desc: "Generate shell completions", Subcommands: []Command{{Name: "bash", Desc: "Generate bash completions"}, {Name: "fish", Desc: "Generate fish completions"}, {Name: "zsh", Desc: "Generate zsh completions"}}},
}

func automationCommands() []Command {
	name := Flag{Name: "name", Desc: "Display name", Value: "text"}
	enabled := Flag{Name: "enabled", Desc: "Enable scheduling (default false)"}
	schedule := Flag{Name: "schedule", Desc: "Numeric five-field cron or whole-minute interval", Value: "text"}
	timezone := Flag{Name: "timezone", Desc: "IANA timezone (default UTC)", Value: "text"}
	spec := []Flag{
		name,
		{Name: "prompt-file", Desc: "Prompt template file ('-' for stdin)", Value: "path"},
		{Name: "path", Desc: "Repository path on the daemon host", Value: "path"},
		{Name: "agent", Desc: "Configured agent name", Value: "agent"},
		{Name: "model", Desc: "Model override (empty clears it)", Value: "text"},
		schedule,
		timezone,
		{Name: "workspace", Desc: "Workspace mode (default worktree)", Value: "text", Choices: []string{"worktree", "repo"}},
		{Name: "overlap", Desc: "Overlap policy (default skip)", Value: "text", Choices: []string{"skip", "queue", "replace"}},
		{Name: "timeout", Desc: "Positive run timeout (default 30m)", Value: "text"},
		enabled,
	}
	updateSpec := append([]Flag{}, spec...)
	for i := range updateSpec {
		switch updateSpec[i].Name {
		case "enabled":
			updateSpec[i].Desc = "Enable scheduling; omitted preserves the current value"
		case "timezone":
			updateSpec[i].Desc = "IANA timezone; omitted preserves the current value"
		case "workspace":
			updateSpec[i].Desc = "worktree or repo; omitted preserves the current value"
		case "overlap":
			updateSpec[i].Desc = "skip, queue or replace; omitted preserves the current value"
		case "timeout":
			updateSpec[i].Desc = "Positive run timeout; omitted preserves the current value"
		}
	}
	requestID := Flag{Name: "request-id", Desc: "Stable request identity for retries", Value: "text"}
	key := Flag{Name: "key", Desc: "Configuration key (derived from name when omitted)", Value: "text"}
	revision := Flag{Name: "revision", Desc: "Expected positive definition revision", Value: "text"}
	run := Flag{Name: "run", Desc: "Positive run number (required)", Value: "text"}
	children := []Command{
		{Name: "ls", Desc: "List automations", Flags: []Flag{
			{Name: "state", Desc: "Scheduling state (default all)", Value: "text", Choices: []string{"all", "enabled", "paused"}},
			{Name: "query", Desc: "Case-insensitive text filter", Value: "text"},
			{Name: "project", Desc: "Exact configured project filter", Value: "project"},
			{Name: "agent", Desc: "Exact agent filter", Value: "agent"},
		}},
		{Name: "view", Desc: "Show specification, revision, runs, and statistics", Args: "REF"},
		{Name: "new", Desc: "Create an automation with scheduling disabled by default", Flags: append(append([]Flag{}, spec...), key, requestID)},
		{Name: "update", Desc: "Update explicitly supplied fields; preserve omitted fields", Args: "REF", Flags: append(updateSpec, revision)},
		{Name: "duplicate", Desc: "Copy an automation with scheduling disabled by default", Args: "REF", Flags: []Flag{name, key, enabled, requestID}},
		{Name: "delete", Desc: "Delete a definition and retain history and logs", Args: "REF", Flags: []Flag{
			{Name: "yes", Desc: "Confirm deletion (required unless -f)"},
			{Name: "f", Desc: "Confirm deletion (alternative to --yes)"}, revision,
		}},
		{Name: "pause", Desc: "Disable scheduling without cancelling accepted runs", Args: "REF", Flags: []Flag{revision}},
		{Name: "resume", Desc: "Enable future scheduled runs", Args: "REF", Flags: []Flag{revision}},
		{Name: "run", Desc: "Request a manual run, including while paused", Args: "REF", Flags: []Flag{requestID}},
		{Name: "stop", Desc: "Request termination of an exact run", Args: "REF", Flags: []Flag{run}},
		{Name: "runs", Desc: "Show a page of run history", Args: "REF", Flags: []Flag{
			{Name: "offset", Desc: "History offset (default 0)", Value: "text"},
			{Name: "limit", Desc: "Page size from 1 through 200 (default 20)", Value: "text"},
		}},
		{Name: "logs", Desc: "Show composed logs for an exact run", Args: "REF", Flags: []Flag{run, {Name: "raw", Desc: "Print unchanged text (incompatible with --json)"}}},
		{Name: "preview", Desc: "Preview four future occurrences using the daemon clock", Flags: []Flag{schedule, timezone}},
		{Name: "terminal", Desc: "Inspect server-resolved terminal metadata for an exact run", Args: "REF", Flags: []Flag{run}},
		{Name: "attach", Desc: "Attach to the current or specified run (read-only by default)", Args: "REF", Flags: []Flag{
			{Name: "run", Desc: "Positive run number (default current run)", Value: "text"},
			{Name: "rw", Desc: "Attach in read-write mode"},
		}},
	}
	for i := range children {
		children[i].Config = true
		children[i].Remote = true
		children[i].Flags = append(children[i].Flags, Flag{Name: "operator-token", Desc: "Operator credential (defaults to KONTORA_OPERATOR_TOKEN)", Value: "text"})
		if children[i].Name != "attach" {
			children[i].Flags = append(children[i].Flags, Flag{Name: "json", Desc: "Print API objects as JSON"})
		}
	}
	return children
}

// CommandGroups are the headings `kontora help` prints, in the order it prints
// them. Every entry in Commands names one.
var CommandGroups = []string{"Getting started", "Tickets", "Running work", "Inspecting a run", "Other"}

// Command is one verb.
type Command struct {
	Name string
	Desc string
	// Group names a CommandGroups heading for top-level verbs; children leave it empty.
	Group string
	// TicketID is true when the first positional is a ticket ID, which is what
	// makes the verb eligible for dynamic ID completion.
	TicketID bool
	// Args names positionals for usage, after the ticket ID when TicketID is true.
	Args string
	// Config is true when the verb reads the local config file, and so takes
	// --config.
	Config bool
	// Store is true when the verb reads or writes the local ticket store, and so
	// takes --tickets-dir. A verb that only drives the daemon does not: the
	// daemon owns the store, so pointing the client elsewhere would be a lie.
	Store bool
	// Remote is true when the verb can drive a remote daemon, and so takes
	// --url and --token.
	Remote bool
	// Subcommands describe one level of child commands, including offline help.
	Subcommands []Command
	Flags       []Flag
}

// Flag is one command-scoped flag. A flag with an empty Value is a boolean.
type Flag struct {
	Name  string
	Short string
	Desc  string
	// Value names the kind of argument the flag takes: "path" completes files,
	// anything else is an opaque string.
	Value string
	// Choices are the only valid values, offered as completions.
	Choices []string
}

// TakesValue reports whether the flag is followed by an argument.
func (f Flag) TakesValue() bool { return f.Value != "" }
