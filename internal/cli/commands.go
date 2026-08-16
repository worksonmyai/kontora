package cli

// Commands is the CLI's command table: the one description of every top-level
// verb and the flags it takes. Both the usage text and the shell completions
// are generated from it, so a new command cannot appear in one and be missing
// from the other.
var Commands = []Command{
	{Name: "ls", Desc: "List tickets (TUI on TTY, static table otherwise)", Config: true, Remote: true, Flags: []Flag{
		{Name: "closed", Desc: "Show done/cancelled tickets"},
		{Name: "static", Desc: "Print static table instead of interactive TUI"},
	}},
	{Name: "new", Desc: "Create a ticket", Config: true, Remote: true, Args: "TITLE", Flags: []Flag{
		{Name: "path", Desc: "Repository path", Value: "path"},
		{Name: "pipeline", Desc: "Pipeline name, or \"none\"", Value: "pipeline"},
		{Name: "agent", Desc: "Agent name, or \"none\"", Value: "agent"},
		{Name: "branch", Desc: "Work branch name", Value: "text"},
		{Name: "base-branch", Desc: "Branch the work branch starts from", Value: "text"},
	}},
	{Name: "view", Desc: "Print ticket details", Config: true, Remote: true, TicketID: true},
	{Name: "edit", Desc: "Open a ticket in $EDITOR", Config: true, TicketID: true},
	{Name: "update", Desc: "Update ticket body/frontmatter fields", Config: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "body-file", Desc: "Read ticket body from a file ('-' for stdin)", Value: "path"},
		{Name: "pipeline", Desc: "Set pipeline (\"\" or \"none\" clears it)", Value: "pipeline"},
		{Name: "path", Desc: "Set repository path", Value: "path"},
		{Name: "agent", Desc: "Set agent override (\"\" or \"none\" clears it)", Value: "agent"},
		{Name: "branch", Desc: "Set branch (\"\" clears it)", Value: "text"},
		{Name: "base-branch", Desc: "Set base branch (\"\" clears it)", Value: "text"},
	}},
	{Name: "delete", Desc: "Delete a ticket file", Config: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "f", Desc: "Confirm deletion (required)"},
		{Name: "yes", Desc: "Confirm deletion (required)"},
	}},
	{Name: "init", Desc: "Set up a ticket for daemon processing", Config: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "pipeline", Desc: "Pipeline name, or \"none\" (required in remote mode)", Value: "pipeline"},
		{Name: "path", Desc: "Repository path (required in remote mode)", Value: "path"},
		{Name: "agent", Desc: "Agent name, or \"none\"", Value: "agent"},
		{Name: "stage", Desc: "Starting pipeline stage", Value: "stage"},
		{Name: "status", Desc: "Initial status", Value: "text", Choices: []string{"open", "todo"}},
	}},
	{Name: "run", Desc: "Enqueue a ticket for processing", Config: true, Remote: true, TicketID: true},
	{Name: "done", Desc: "Mark a ticket as done", Config: true, Remote: true, TicketID: true},
	{Name: "move", Desc: "Move a ticket to any status, including custom ones", Config: true, Remote: true, TicketID: true, Args: "STATUS"},
	{Name: "note", Desc: "Append a note to a ticket", Config: true, Remote: true, TicketID: true, Args: "[TEXT]"},
	{Name: "summary", Desc: "Set a ticket's summary", Config: true, Remote: true, TicketID: true, Args: "[TEXT]"},
	{Name: "pause", Desc: "Pause a running ticket", Config: true, Remote: true, TicketID: true},
	{Name: "retry", Desc: "Re-queue a paused ticket", Config: true, Remote: true, TicketID: true},
	{Name: "skip", Desc: "Skip to the next pipeline stage", Config: true, Remote: true, TicketID: true},
	{Name: "set-stage", Desc: "Move ticket to a specific pipeline stage", Config: true, Remote: true, TicketID: true, Args: "STAGE"},
	{Name: "cancel", Desc: "Cancel a ticket", Config: true, Remote: true, TicketID: true},
	{Name: "archive", Desc: "Archive old done/cancelled tickets", Config: true, Flags: []Flag{
		{Name: "days", Desc: "Age threshold in days (required)", Value: "text"},
		{Name: "dry-run", Desc: "List what would be archived, write nothing"},
		{Name: "path", Desc: "Only archive tickets for this repository path", Value: "path"},
		{Name: "project", Desc: "Only archive tickets for this configured project", Value: "project"},
		{Name: "status", Desc: "Only archive tickets with this status", Value: "text", Choices: []string{"done", "cancelled"}},
		{Name: "yes", Short: "y", Desc: "Skip the confirmation prompt"},
	}},
	{Name: "logs", Desc: "Show agent logs for a ticket", Config: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "stage", Desc: "Stage name", Value: "stage"},
	}},
	{Name: "attach", Desc: "Attach to a running ticket", Config: true, Remote: true, TicketID: true, Flags: []Flag{
		{Name: "rw", Desc: "Attach in read-write mode"},
	}},
	{Name: "start", Desc: "Start the daemon", Config: true, Flags: []Flag{
		{Name: "address", Desc: "Web server listen address", Value: "text"},
		{Name: "port", Desc: "Web server port", Value: "text"},
	}},
	{Name: "setup", Desc: "Create the configuration file", Config: true, Flags: []Flag{
		{Name: "agent", Desc: "Print setup instructions for a coding agent"},
	}},
	{Name: "doctor", Desc: "Validate prerequisites and configuration", Config: true},
	{Name: "config", Desc: "Show effective configuration", Config: true, Remote: true, Subcommands: []string{"edit"}},
	{Name: "fmt", Desc: "Format stream-json from stdin"},
	{Name: "version", Desc: "Print version"},
	{Name: "completion", Desc: "Generate shell completions", Subcommands: []string{"fish"}},
}

// Command is one top-level verb.
type Command struct {
	Name string
	Desc string
	// TicketID is true when the first positional is a ticket ID, which is what
	// makes the verb eligible for dynamic ID completion.
	TicketID bool
	// Args names the positionals after the ticket ID, for the usage line.
	Args string
	// Config is true when the verb reads the local config file, and so takes
	// --config.
	Config bool
	// Remote is true when the verb can drive a remote daemon, and so takes
	// --url and --token.
	Remote bool
	// Subcommands are literal words that may follow the verb.
	Subcommands []string
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
