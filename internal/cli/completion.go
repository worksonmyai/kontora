package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// SupportedShells lists the shells Completion can generate a script for.
var SupportedShells = []string{"bash", "fish", "zsh"}

// Completion writes a shell completion script to w.
func Completion(shell string, w io.Writer) error {
	var script string
	switch shell {
	case "bash":
		script = bashCompletion()
	case "fish":
		script = fishCompletion()
	case "zsh":
		script = zshCompletion()
	default:
		return fmt.Errorf("unsupported shell: %s (supported: %s)", shell, strings.Join(SupportedShells, ", "))
	}
	_, err := io.WriteString(w, script)
	return err
}

// commandFlags returns the flags the verb accepts, including the ones the
// dispatcher adds for every config-reading, store-backed or remote-capable command.
func commandFlags(cmd Command) []Flag {
	flags := slices.Clone(cmd.Flags)
	if cmd.Config {
		flags = append(flags, Flag{Name: "config", Desc: "Config file path", Value: "path"})
	}
	if cmd.Store {
		flags = append(flags, Flag{Name: "tickets-dir", Desc: "Tickets directory (overrides config and environment)", Value: "path"})
	}
	if cmd.Remote {
		flags = append(flags,
			Flag{Name: "url", Desc: "Remote daemon URL (enables remote mode)", Value: "text"},
			Flag{Name: "token", Desc: "Bearer token for the remote daemon", Value: "text"},
		)
	}
	return flags
}

// ticketIDCommands returns the verbs whose first positional is a ticket ID.
func ticketIDCommands() []string {
	var names []string
	for _, cmd := range Commands {
		if cmd.TicketID {
			names = append(names, cmd.Name)
		}
	}
	return names
}

// restTicketIDCommands returns the verbs whose positionals after the first are
// also ticket IDs.
func restTicketIDCommands() []string {
	var names []string
	for _, cmd := range Commands {
		if restTakesTicketIDs(cmd) {
			names = append(names, cmd.Name)
		}
	}
	return names
}

// restTakesTicketIDs reports whether the positionals after the first are ticket
// IDs. Args spells those with an _ID suffix ("DEPENDENCY_ID", "TICKET_ID...").
func restTakesTicketIDs(cmd Command) bool {
	return strings.HasSuffix(strings.TrimSuffix(cmd.Args, "..."), "_ID")
}

// completionCommands is the Commands table plus the help verb the dispatcher
// handles itself, so no generator has to name help on its own.
func completionCommands() []Command {
	return append(slices.Clone(Commands), Command{Name: "help", Desc: "Show usage"})
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
	for _, cmd := range completionCommands() {
		fmt.Fprintf(&b, "complete -c kontora -n __fish_use_subcommand -a %s -d '%s'\n", cmd.Name, fishQuote(cmd.Desc))
	}

	b.WriteString("\n# Subcommands\n")
	for _, cmd := range Commands {
		for _, sub := range cmd.Subcommands {
			fmt.Fprintf(&b, "complete -c kontora -n '__fish_seen_subcommand_from %s' -a %s\n", cmd.Name, sub)
		}
	}

	b.WriteString("\n# Per-command flags\n")
	for _, cmd := range Commands {
		for _, f := range commandFlags(cmd) {
			b.WriteString(fishFlag(cmd.Name, f) + "\n")
		}
	}

	b.WriteString(`
# __kontora_config_flag repeats the --config the user already typed, so the
# ticket list comes from the same config file the command will use.
function __kontora_config_flag
    set -l tokens (commandline -opc)
    for i in (seq (count $tokens))
        switch $tokens[$i]
            case '--config=*'
                set -l value (string replace -- --config= '' $tokens[$i])
                if test -n "$value"
                    printf '%s\n' "--config=$value"
                end
                return
            case --config
                # The index has to be a plain variable: fish rejects a command
                # substitution inside an index in a quoted string.
                set -l next (math $i + 1)
                if test $next -le (count $tokens)
                    printf '%s\n' "--config=$tokens[$next]"
                end
                return
        end
    end
end

# Dynamic ticket ID completion. -e/--entire is required: without it string match
# prints only the part the regex matched, which is the leading space.
function __kontora_ticket_ids
    kontora ls --closed --static (__kontora_config_flag) 2>/dev/null | string match -re '^\s+\S' | awk '$1 != "ID" {print $1}'
end
`)
	fmt.Fprintf(&b, "set -l __kontora_id_cmds %s\n", strings.Join(ticketIDCommands(), " "))
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

// zshCompletion renders the zsh script from the Commands table. The output is
// an autoloadable _kontora file that also works when sourced directly.
func zshCompletion() string {
	var b strings.Builder

	b.WriteString(`#compdef kontora
# kontora zsh completions
# Install: kontora completion zsh > "${fpath[1]}/_kontora"
# Session: source <(kontora completion zsh)

# __kontora_config_flag repeats the --config the user already typed, so the
# ticket list comes from the same config file the command will use.
__kontora_config_flag() {
    local i
    for (( i = 1; i <= ${#words[@]}; i++ )); do
        case ${words[i]} in
            --config=?*) print -r -- ${words[i]}; return ;;
            --config) [[ -n ${words[i+1]} ]] && print -r -- --config=${words[i+1]}; return ;;
        esac
    done
    return 0
}

__kontora_ticket_ids() {
    local -a ids cfg
    cfg=( ${(z)"$(__kontora_config_flag)"} )
    ids=( ${(f)"$(kontora ls --closed --static $cfg 2>/dev/null | awk '/^[[:space:]]+[^[:space:]]/ && $1 != "ID" { print $1 }')"} )
    _describe -t tickets ticket ids
}

_kontora() {
    local context state state_descr line curcontext=$curcontext
    typeset -A opt_args
    local -a commands
    commands=(
`)
	for _, cmd := range completionCommands() {
		fmt.Fprintf(&b, "        %s\n", zshQuote(cmd.Name+":"+cmd.Desc))
	}
	b.WriteString(`    )

    _arguments -C '1: :->command' '*:: :->args' && return

    case $state in
        command)
            _describe -t commands 'kontora command' commands && return
            ;;
        args)
            case $words[1] in
`)
	for _, cmd := range Commands {
		specs := zshSpecs(cmd)
		if len(specs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "                %s)\n                    _arguments \\\n", cmd.Name)
		for i, spec := range specs {
			cont := " \\"
			if i == len(specs)-1 {
				cont = ""
			}
			fmt.Fprintf(&b, "                        %s%s\n", spec, cont)
		}
		b.WriteString("                    ;;\n")
	}
	b.WriteString(`            esac
            ;;
    esac
}

if [[ $funcstack[1] == _kontora ]]; then
    _kontora "$@"
else
    compdef _kontora kontora
fi
`)
	return b.String()
}

// zshSpecs renders the _arguments specs for one verb: its flags first, then the
// positionals it takes.
func zshSpecs(cmd Command) []string {
	var specs []string
	for _, f := range commandFlags(cmd) {
		for _, name := range flagSpellings(f) {
			specs = append(specs, zshQuote(zshFlagSpec(f, name)))
		}
	}
	switch {
	case cmd.TicketID:
		specs = append(specs, "'1:ticket:__kontora_ticket_ids'")
	case len(cmd.Subcommands) > 0:
		specs = append(specs, fmt.Sprintf("'1:subcommand:(%s)'", strings.Join(cmd.Subcommands, " ")))
	}
	// Without a rest spec zsh has nothing left to offer past the first
	// positional and completes the common prefix of the flags instead, which
	// rewrites the line to "kontora move kon-aaa --".
	switch {
	case restTakesTicketIDs(cmd):
		specs = append(specs, "'*:ticket:__kontora_ticket_ids'")
	case cmd.Args != "":
		specs = append(specs, "'*:arg:_default'")
	}
	return specs
}

// flagSpellings returns every dashed spelling of a flag, short form first.
func flagSpellings(f Flag) []string {
	names := []string{dashed(f.Name)}
	if f.Short != "" {
		names = append([]string{dashed(f.Short)}, names...)
	}
	return names
}

// dashed spells a flag the way the shells expect: one dash for a single
// character, two for a word.
func dashed(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// zshFlagSpec renders one _arguments spec. A flag with two spellings carries an
// exclusion list so zsh stops offering the other one.
func zshFlagSpec(f Flag, name string) string {
	var b strings.Builder
	if f.Short != "" {
		fmt.Fprintf(&b, "(%s)", strings.Join(flagSpellings(f), " "))
	}
	b.WriteString(name)
	if f.TakesValue() {
		// The "=" suffix is what makes zsh recognise --flag=value, which Go's
		// flag package accepts. Without it that word counts as a positional and
		// every later completion on the line is off by one.
		b.WriteString("=")
	}
	fmt.Fprintf(&b, "[%s]", zshEscape(f.Desc))
	switch {
	case len(f.Choices) > 0:
		fmt.Fprintf(&b, ":value:(%s)", strings.Join(f.Choices, " "))
	case f.Value == "path":
		b.WriteString(":path:_files")
	case f.TakesValue():
		b.WriteString(":value:")
	}
	return b.String()
}

// zshEscape hides the brackets that would end an _arguments description early.
func zshEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(s)
}

// zshQuote wraps a spec in single quotes.
func zshQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// bashCompletion renders the bash script from the Commands table. kontora takes
// no flags before the verb, so the verb is always COMP_WORDS[1].
func bashCompletion() string {
	var b strings.Builder

	b.WriteString(`# kontora bash completions
# Install: kontora completion bash > ~/.local/share/bash-completion/completions/kontora
# Session: source <(kontora completion bash)

# __kontora_config_flag repeats the --config the user already typed, so the
# ticket list comes from the same config file the command will use. The default
# COMP_WORDBREAKS splits on "=", so --config=path arrives as three words.
__kontora_config_flag() {
    local i
    for (( i = 1; i < ${#COMP_WORDS[@]}; i++ )); do
        case "${COMP_WORDS[i]}" in
            --config=?*) printf '%s' "${COMP_WORDS[i]}"; return ;;
            --config)
                if [[ "${COMP_WORDS[i+1]}" == "=" ]]; then
                    [[ -n "${COMP_WORDS[i+2]}" ]] && printf '%s' "--config=${COMP_WORDS[i+2]}"
                elif [[ -n "${COMP_WORDS[i+1]}" ]]; then
                    printf '%s' "--config=${COMP_WORDS[i+1]}"
                fi
                return ;;
        esac
    done
    return 0
}

__kontora_ticket_ids() {
    kontora ls --closed --static $(__kontora_config_flag) 2>/dev/null |
        awk '/^[[:space:]]+[^[:space:]]/ && $1 != "ID" { print $1 }'
}

# __kontora_files completes a path. IFS keeps a name containing spaces in one
# candidate; compopt, which bash 3.2 does not have, adds the trailing slash.
__kontora_files() {
    local IFS=$'\n'
    COMPREPLY=( $(compgen -f -- "$cur") )
    [[ $(type -t compopt) == builtin ]] && compopt -o filenames 2>/dev/null
    return 0
}

# __kontora_positional prints the 1-based index of the word under the cursor
# among the verb's positionals, skipping flags and the values they take.
__kontora_positional() {
    local value_flags=" $1 " i n=0 w
    for (( i = 2; i < COMP_CWORD; i++ )); do
        w="${COMP_WORDS[i]}"
        case "$w" in
            =) ;;
            -*)
                if [[ "$w" != *=* && "$value_flags" == *" $w "* ]]; then
                    if [[ "${COMP_WORDS[i+1]}" == "=" ]]; then
                        i=$((i+2))
                    else
                        i=$((i+1))
                    fi
                fi
                ;;
            *) n=$((n+1)) ;;
        esac
    done
    printf '%s' "$((n+1))"
}

_kontora() {
    local cur prev cmd flags value_flags pos
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    # --flag=value is one word in bash 3.2 and three in bash 5. Either way the
    # flag has to end up in prev and its value in cur.
    if [[ "$cur" == -*=* ]]; then
        prev="${cur%%=*}"
        cur="${cur#*=}"
    elif [[ COMP_CWORD -gt 1 && "$cur" == "=" ]]; then
        cur=""
    elif [[ COMP_CWORD -gt 1 && "$prev" == "=" ]]; then
        prev="${COMP_WORDS[COMP_CWORD-2]}"
    fi

    if (( COMP_CWORD == 1 )); then
`)
	var names []string
	for _, cmd := range completionCommands() {
		names = append(names, cmd.Name)
	}
	fmt.Fprintf(&b, "        COMPREPLY=( $(compgen -W %q -- \"$cur\") )\n", strings.Join(names, " "))
	b.WriteString(`        return
    fi

    cmd="${COMP_WORDS[1]}"
    flags=""
    value_flags=""

    case "$cmd" in
`)
	for _, cmd := range Commands {
		flags := commandFlags(cmd)
		if len(flags) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        %s)\n", cmd.Name)
		for _, line := range bashValueCase(flags) {
			b.WriteString("            " + line + "\n")
		}
		var spellings, valued []string
		for _, f := range flags {
			spellings = append(spellings, flagSpellings(f)...)
			if f.TakesValue() {
				valued = append(valued, flagSpellings(f)...)
			}
		}
		fmt.Fprintf(&b, "            flags=%q\n", strings.Join(spellings, " "))
		fmt.Fprintf(&b, "            value_flags=%q\n            ;;\n", strings.Join(valued, " "))
	}
	b.WriteString(`    esac

    if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
        return
    fi

    pos=$(__kontora_positional "$value_flags")

    case "$cmd" in
`)
	// The rest-argument verbs come first: their branch takes ticket IDs at
	// every position, so it must not fall into the first-positional one. An
	// empty list would render as a bare ")" and break the case.
	if rest := restTicketIDCommands(); len(rest) > 0 {
		fmt.Fprintf(&b, "        %s)\n            COMPREPLY=( $(compgen -W \"$(__kontora_ticket_ids)\" -- \"$cur\") )\n            ;;\n",
			strings.Join(rest, "|"))
	}
	if ids := ticketIDCommands(); len(ids) > 0 {
		fmt.Fprintf(&b, "        %s)\n            if (( pos == 1 )); then\n                COMPREPLY=( $(compgen -W \"$(__kontora_ticket_ids)\" -- \"$cur\") )\n            fi\n            ;;\n",
			strings.Join(ids, "|"))
	}
	for _, cmd := range Commands {
		if len(cmd.Subcommands) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        %s)\n            if (( pos == 1 )); then\n                COMPREPLY=( $(compgen -W %q -- \"$cur\") )\n            fi\n            ;;\n",
			cmd.Name, strings.Join(cmd.Subcommands, " "))
	}
	b.WriteString(`    esac
    return 0
}

complete -F _kontora kontora
`)
	return b.String()
}

// bashValueCase renders the `case "$prev"` that completes a flag's argument.
// Flags are grouped by what they accept so the branch stays one line each.
func bashValueCase(flags []Flag) []string {
	var paths []string
	var opaque []string
	choices := map[string][]string{}
	var choiceOrder []string
	for _, f := range flags {
		if !f.TakesValue() {
			continue
		}
		names := flagSpellings(f)
		switch {
		case len(f.Choices) > 0:
			key := strings.Join(f.Choices, " ")
			if _, ok := choices[key]; !ok {
				choiceOrder = append(choiceOrder, key)
			}
			choices[key] = append(choices[key], names...)
		case f.Value == "path":
			paths = append(paths, names...)
		default:
			opaque = append(opaque, names...)
		}
	}
	if len(paths) == 0 && len(opaque) == 0 && len(choiceOrder) == 0 {
		return nil
	}

	lines := []string{`case "$prev" in`}
	for _, key := range choiceOrder {
		lines = append(lines, fmt.Sprintf("    %s) COMPREPLY=( $(compgen -W %q -- \"$cur\") ); return ;;",
			strings.Join(choices[key], "|"), key))
	}
	if len(paths) > 0 {
		lines = append(lines, fmt.Sprintf(`    %s) __kontora_files; return ;;`, strings.Join(paths, "|")))
	}
	if len(opaque) > 0 {
		// The flag wants a value we cannot guess; returning stops bash from
		// offering the command's flags in the value position.
		lines = append(lines, fmt.Sprintf(`    %s) return ;;`, strings.Join(opaque, "|")))
	}
	return append(lines, "esac")
}
