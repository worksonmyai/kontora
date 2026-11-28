package cli

import (
	"flag"
	"fmt"
	"strings"
)

// LookupCommand finds a verb in the command table. A two-word name such as
// "config edit" names a subcommand.
func LookupCommand(name string) (Command, bool) {
	parent, child, nested := strings.Cut(name, " ")
	for _, cmd := range Commands {
		if cmd.Name != parent {
			continue
		}
		if !nested {
			return cmd, true
		}
		for _, sub := range cmd.Subcommands {
			if sub.Name == child {
				return sub, true
			}
		}
	}
	return Command{}, false
}

// WriteUsage prints `kontora VERB -h` for the verb fs is named after, to fs's
// output. Go's default lists only the flags, so a reader could not tell which
// positional comes first, as in `dep`, or that the verb has subcommands.
func WriteUsage(fs *flag.FlagSet) {
	w, name := fs.Output(), fs.Name()
	cmd, ok := LookupCommand(name)
	if !ok {
		fmt.Fprintf(w, "Usage of %s:\n", name)
		fs.PrintDefaults()
		return
	}
	hasFlags := false
	fs.VisitAll(func(*flag.Flag) { hasFlags = true })
	synopsis := []string{"kontora", name}
	if hasFlags {
		synopsis = append(synopsis, "[flags]")
	}
	if cmd.TicketID {
		synopsis = append(synopsis, "TICKET_ID")
	}
	if cmd.Args != "" {
		synopsis = append(synopsis, cmd.Args)
	}
	fmt.Fprintf(w, "Usage: %s\n\n%s\n", strings.Join(synopsis, " "), cmd.Desc)
	if len(cmd.Subcommands) > 0 {
		fmt.Fprintf(w, "\nCommands:\n")
		for _, sub := range cmd.Subcommands {
			fmt.Fprintf(w, "  %-10s %s\n", sub.Name, sub.Desc)
		}
	}
	if hasFlags {
		fmt.Fprintf(w, "\nFlags:\n")
		fs.PrintDefaults()
	}
	verb, _, _ := strings.Cut(name, " ")
	fmt.Fprintf(w, "\nReference: kontora skills show cli %q\n", "kontora "+verb)
}
