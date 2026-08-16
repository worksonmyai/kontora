package cli

import (
	"fmt"
	"io"
	"strings"
)

// SupportedShells lists the shells Completion can generate a script for.
var SupportedShells = []string{"fish"}

// Completion writes a shell completion script to w.
func Completion(shell string, w io.Writer) error {
	if shell != "fish" {
		return fmt.Errorf("unsupported shell: %s (supported: %s)", shell, strings.Join(SupportedShells, ", "))
	}
	_, err := io.WriteString(w, fishCompletion())
	return err
}

// fishCompletion renders the fish script from the Commands table, so a command
// or flag added there is offered without a second edit here.
func fishCompletion() string {
	var b strings.Builder

	b.WriteString(`# kontora fish completions
# Install: kontora completion fish | source
# Persist: kontora completion fish > ~/.config/fish/completions/kontora.fish

# Disable file completions by default
complete -c kontora -f

# Top-level commands
`)
	for _, cmd := range Commands {
		fmt.Fprintf(&b, "complete -c kontora -n __fish_use_subcommand -a %s -d '%s'\n", cmd.Name, fishQuote(cmd.Desc))
	}
	fmt.Fprintf(&b, "complete -c kontora -n __fish_use_subcommand -a help -d '%s'\n", "Show usage")

	b.WriteString("\n# Subcommands\n")
	for _, cmd := range Commands {
		for _, sub := range cmd.Subcommands {
			fmt.Fprintf(&b, "complete -c kontora -n '__fish_seen_subcommand_from %s' -a %s\n", cmd.Name, sub)
		}
	}

	b.WriteString("\n# Per-command flags\n")
	for _, cmd := range Commands {
		flags := cmd.Flags
		if cmd.Config {
			flags = append(flags, Flag{Name: "config", Desc: "Config file path", Value: "path"})
		}
		if cmd.Remote {
			flags = append(flags,
				Flag{Name: "url", Desc: "Remote daemon URL (enables remote mode)", Value: "text"},
				Flag{Name: "token", Desc: "Bearer token for the remote daemon", Value: "text"},
			)
		}
		for _, f := range flags {
			b.WriteString(fishFlag(cmd.Name, f) + "\n")
		}
	}

	b.WriteString(`
# Dynamic ticket ID completion. -e/--entire is required: without it string match
# prints only the part the regex matched, which is the leading space.
function __kontora_ticket_ids
    kontora ls --closed --static 2>/dev/null | string match -re '^\s+\S' | awk '$1 != "ID" {print $1}'
end
`)
	var idCmds []string
	for _, cmd := range Commands {
		if cmd.TicketID {
			idCmds = append(idCmds, cmd.Name)
		}
	}
	fmt.Fprintf(&b, "set -l __kontora_id_cmds %s\n", strings.Join(idCmds, " "))
	b.WriteString(`for cmd in $__kontora_id_cmds
    complete -c kontora -n "__fish_seen_subcommand_from $cmd" -a '(__kontora_ticket_ids)'
end
`)

	return b.String()
}

// fishFlag renders one `complete` line. Long names use -l, single characters use
// -s, matching how fish distinguishes --flag from -f.
func fishFlag(cmd string, f Flag) string {
	var b strings.Builder
	fmt.Fprintf(&b, "complete -c kontora -n '__fish_seen_subcommand_from %s'", cmd)
	if len(f.Name) == 1 {
		fmt.Fprintf(&b, " -s %s", f.Name)
	} else {
		fmt.Fprintf(&b, " -l %s", f.Name)
	}
	if f.Short != "" {
		fmt.Fprintf(&b, " -s %s", f.Short)
	}
	fmt.Fprintf(&b, " -d '%s'", fishQuote(f.Desc))

	switch {
	case len(f.Choices) > 0:
		fmt.Fprintf(&b, " -x -a '%s'", strings.Join(f.Choices, " "))
	case f.Value == "path":
		b.WriteString(" -r -F")
	case f.TakesValue():
		b.WriteString(" -r")
	}
	return b.String()
}

// fishQuote escapes a description for a single-quoted fish string.
func fishQuote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}
