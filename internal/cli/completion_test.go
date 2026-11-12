package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompletion(t *testing.T) {
	cases := []struct {
		name    string
		shell   string
		wantErr string
		want    []string
		notWant []string
	}{
		{
			name:  "fish setup and its boolean --agent flag",
			shell: "fish",
			want: []string{
				"-a setup -d 'Create the configuration file'",
				"'__kontora_command setup' -l agent",
				"'__kontora_command setup' -l config",
			},
		},
		{
			name:  "fish flags that take a value are marked -r",
			shell: "fish",
			want: []string{
				"'__kontora_command logs' -l stage -d 'Stage name' -r",
				"'__kontora_command archive' -l status -d 'Only archive tickets with this status' -x -a 'done cancelled closed'",
				"'__kontora_command delete' -s f",
				"'__kontora_command schedule' -l at -d 'Pickup time as an RFC 3339 instant' -r",
				"'__kontora_command schedule' -l clear",
			},
		},
		{
			name:  "fish remote flags reach every remote-capable command",
			shell: "fish",
			want: []string{
				"'__kontora_command move' -l url",
				"'__kontora_command move' -l token",
			},
		},
		{
			name:  "--tickets-dir reaches store commands and no others",
			shell: "fish",
			want: []string{
				"'__kontora_command move' -l tickets-dir",
				"'__kontora_command ls' -l tickets-dir",
			},
			notWant: []string{
				"'__kontora_command stats' -l tickets-dir",
				"'__kontora_command changes' -l tickets-dir",
			},
		},
		{
			name:  "fish ticket IDs are extracted from whole lines, not the matched space",
			shell: "fish",
			// Without -e/--entire, string match prints only the matched
			// whitespace and the function yields nothing at all.
			want: []string{"string match -re", `awk '$1 != "ID" {print $1}'`},
		},
		{
			name:  "fish looks up ticket IDs through the config already on the line",
			shell: "fish",
			want: []string{
				"function __kontora_config_flag",
				"kontora ls --closed --static (__kontora_config_flag)",
			},
		},
		{
			name:  "fish offers every shell the completion subcommand generates",
			shell: "fish",
			want: []string{
				"-a bash -d 'Generate bash completions'",
				"-a fish -d 'Generate fish completions'",
				"-a zsh -d 'Generate zsh completions'",
			},
		},
		{
			name:  "zsh is an autoloadable compdef file that also works when sourced",
			shell: "zsh",
			want: []string{
				"#compdef kontora",
				"__kontora_config_flag()",
				"__kontora_ticket_ids()",
				"_arguments -C '1: :->command' '*:: :->args'",
				"compdef _kontora kontora",
			},
		},
		{
			name:  "zsh flag specs carry the value type",
			shell: "zsh",
			want: []string{
				"'--status=[Initial status]:value:(open todo)'",
				"'--config=[Config file path]:path:_files'",
				"'--stage=[Stage name]:value:'",
				"'--agent[Print setup instructions for a coding agent]'",
			},
		},
		{
			name:  "zsh positionals complete ticket IDs and literal subcommands",
			shell: "zsh",
			want: []string{
				"'1:ticket:__kontora_ticket_ids'",
				"'1:subcommand:(bash fish zsh)'",
				"'1:subcommand:(edit)'",
			},
		},
		{
			name:  "zsh completes the positionals after the first one",
			shell: "zsh",
			// Without a rest spec zsh falls back to the flags and inserts their
			// common prefix, rewriting the line to "kontora move kon-aaa --".
			want: []string{
				"'1:ticket:__kontora_ticket_ids' \\\n                        '*:ticket:__kontora_ticket_ids'",
				"'1:ticket:__kontora_ticket_ids' \\\n                        '*:arg:_default'",
			},
		},
		{
			name:  "zsh spells a single-character flag with one dash",
			shell: "zsh",
			want: []string{
				"'-i[Case-insensitive match (default is smart-case)]'",
				"'-m=[Max matching lines per ticket, 0 for all]:value:'",
			},
		},
		{
			name:  "zsh excludes the other spelling of a flag that has both",
			shell: "zsh",
			want: []string{
				"'(-y --yes)-y[Skip the confirmation prompt]'",
				"'(-y --yes)--yes[Skip the confirmation prompt]'",
			},
		},
		{
			name:  "bash registers a completion function over COMP_WORDS",
			shell: "bash",
			want: []string{
				"complete -F _kontora kontora",
				`cur="${COMP_WORDS[COMP_CWORD]}"`,
				`prev="${COMP_WORDS[COMP_CWORD-1]}"`,
				"__kontora_ticket_ids()",
				`COMPREPLY=( $(compgen -W "$(__kontora_ticket_ids)" -- "$cur") )`,
			},
		},
		{
			name:  "bash spells a single-character flag with one dash",
			shell: "bash",
			want:  []string{`flags="-i -s -F -l -m --body`},
		},
		{
			name:  "bash completes flag values by what the flag accepts",
			shell: "bash",
			want: []string{
				`--days|--project) return ;;`,
				`--status) COMPREPLY=( $(compgen -W "done cancelled closed" -- "$cur") ); return ;;`,
				`--path|--config|--tickets-dir) __kontora_files; return ;;`,
				`COMPREPLY=( $(compgen -W "bash fish zsh" -- "$cur") )`,
			},
		},
		{
			name:  "bash reads --flag=value in both the split and the unsplit form",
			shell: "bash",
			// bash 5 splits the word on "=", bash 3.2 does not.
			want: []string{
				`if [[ "$cur" == -*=* ]]; then`,
				`prev="${cur%%=*}"`,
				`if [[ "${COMP_WORDS[i+1]}" == "=" ]]; then`,
			},
		},
		{
			name:  "bash offers a positional only where the verb takes one",
			shell: "bash",
			want: []string{
				"__kontora_positional()",
				`pos=$(__kontora_positional "$value_flags")`,
				"        dep|undep|link|unlink)\n            COMPREPLY=( $(compgen -W \"$(__kontora_ticket_ids)\" -- \"$cur\") )",
				"        completion)\n            if (( pos == 1 )); then",
			},
		},
		{
			name:  "bash keeps a filename with a space in one candidate",
			shell: "bash",
			want:  []string{"__kontora_files()", `local IFS=$'\n'`},
		},
		{
			name:    "unsupported shell names the ones that work",
			shell:   "powershell",
			wantErr: "unsupported shell: powershell (supported: bash, fish, zsh)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := Completion(tc.shell, &buf)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			for _, want := range tc.want {
				assert.Contains(t, buf.String(), want)
			}
			for _, notWant := range tc.notWant {
				assert.NotContains(t, buf.String(), notWant)
			}
		})
	}
}

// TestEveryCommandIsGrouped guards the usage text: a verb whose Group is blank
// or misspelled is printed under no heading at all.
func TestEveryCommandIsGrouped(t *testing.T) {
	for _, cmd := range Commands {
		t.Run(cmd.Name, func(t *testing.T) {
			assert.Contains(t, CommandGroups, cmd.Group)
		})
	}
}

// TestCompletionCoversEveryCommand is the guard against the lists drifting
// apart: in every shell, every verb in the table has to be offered, every
// ticket-taking verb has to complete ticket IDs, and every flag has to appear.
func TestCompletionCoversEveryCommand(t *testing.T) {
	shells := []struct {
		shell string
		// offered lists the verbs the script completes after "kontora ", so a
		// verb named anywhere else in the script cannot stand in for one.
		offered func(script string) []string
		// ticketIDs lists the verbs that get dynamic ticket ID completion.
		ticketIDs func(script string) []string
		// scope narrows the script to the part that speaks for one verb.
		scope func(script, cmd string) string
		flag  func(f Flag) string
	}{
		{
			shell:     "fish",
			offered:   fishOfferedCommands,
			ticketIDs: func(script string) []string { return words(lineWithPrefix(script, "set -l __kontora_id_cmds ")) },
			scope:     fishScope,
			flag:      func(f Flag) string { return fmt.Sprintf(" -%s %s ", flagKind(f.Name), f.Name) },
		},
		{
			shell:     "zsh",
			offered:   zshOfferedCommands,
			ticketIDs: zshTicketIDCommands,
			scope: func(script, cmd string) string {
				return caseBranch(script, "                "+cmd+")\n", "\n                    ;;\n")
			},
			flag: zshFlagMatch,
		},
		{
			shell:     "bash",
			offered:   bashOfferedCommands,
			ticketIDs: bashTicketIDCommands,
			scope:     func(script, cmd string) string { return caseBranch(script, "        "+cmd+")\n", "\n            ;;\n") },
			flag:      func(f Flag) string { return dashed(f.Name) },
		},
	}

	for _, sh := range shells {
		t.Run(sh.shell, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, Completion(sh.shell, &buf))
			script := buf.String()
			offered := sh.offered(script)
			require.NotEmpty(t, offered, "the script offers no commands at all")
			ids := sh.ticketIDs(script)
			require.NotEmpty(t, ids, "the script offers ticket IDs to no command at all")

			for _, cmd := range Commands {
				t.Run(cmd.Name, func(t *testing.T) {
					assert.Contains(t, offered, cmd.Name, "command is not offered")
					if cmd.TicketID {
						assert.Contains(t, ids, cmd.Name, "command takes a ticket ID but does not complete one")
					}
					if len(cmd.Flags) == 0 {
						return
					}
					scoped := sh.scope(script, cmd.Name)
					require.NotEmpty(t, scoped, "command has flags but no section of its own")
					for _, f := range cmd.Flags {
						assert.Contains(t, scoped, sh.flag(f), "flag is not offered")
					}
				})
			}
		})
	}
}

// fishOfferedCommands lists the verbs offered when no subcommand has been typed.
func fishOfferedCommands(script string) []string {
	const marker = "-n __fish_use_subcommand -a "
	var names []string
	for line := range strings.SplitSeq(script, "\n") {
		if _, rest, ok := strings.Cut(line, marker); ok && len(words(rest)) > 0 {
			names = append(names, words(rest)[0])
		}
	}
	return names
}

// fishScope keeps only the lines bound to one verb. Without the binding a flag
// that any other verb happens to share would satisfy the assertion.
func fishScope(script, cmd string) string {
	marker := fmt.Sprintf("'__kontora_command %s'", strings.ReplaceAll(cmd, "/", " "))
	var b strings.Builder
	for line := range strings.SplitSeq(script, "\n") {
		if strings.Contains(line, marker) {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// zshOfferedCommands lists the entries of the commands array _describe offers.
func zshOfferedCommands(script string) []string {
	body := caseBranch(script, "    commands=(\n", "    )\n")
	var names []string
	for line := range strings.SplitSeq(body, "\n") {
		if name, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "'"), ":"); ok {
			names = append(names, name)
		}
	}
	return names
}

// zshTicketIDCommands lists the verbs whose _arguments block completes a ticket
// ID. zsh has no shared list; each verb declares its own positional.
func zshTicketIDCommands(script string) []string {
	var names []string
	for _, cmd := range Commands {
		if strings.Contains(caseBranch(script, "                "+cmd.Name+")\n", "\n                    ;;\n"), "__kontora_ticket_ids") {
			names = append(names, cmd.Name)
		}
	}
	return names
}

// zshFlagMatch is the spec zsh needs for one flag. A flag that takes a value
// carries the "=" that makes --flag=value parse as that flag.
func zshFlagMatch(f Flag) string {
	if f.TakesValue() {
		return dashed(f.Name) + "=["
	}
	return dashed(f.Name) + "["
}

// bashOfferedCommands reads the compgen word list of the COMP_CWORD == 1 branch,
// which is the only place a bare "kontora <TAB>" gets its candidates from.
func bashOfferedCommands(script string) []string {
	line := lineWithPrefix(script, `        COMPREPLY=( $(compgen -W "`)
	_, rest, _ := strings.Cut(line, `-W "`)
	list, _, _ := strings.Cut(rest, `"`)
	return words(list)
}

// bashTicketIDCommands lists the verbs of every case branch that completes
// ticket IDs, splitting the "a|b|c)" pattern the branch is labelled with.
func bashTicketIDCommands(script string) []string {
	var names []string
	pattern := ""
	for line := range strings.SplitSeq(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ")") && !strings.Contains(trimmed, "(") {
			pattern = strings.TrimSuffix(trimmed, ")")
		}
		if strings.Contains(line, `compgen -W "$(__kontora_ticket_ids)"`) {
			names = append(names, strings.Split(pattern, "|")...)
		}
	}
	return names
}

func words(s string) []string { return strings.Fields(s) }

func lineWithPrefix(script, prefix string) string {
	for line := range strings.SplitSeq(script, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// caseBranch returns the body of the case branch that starts with start. The
// end marker is the branch's own ";;", indented apart from any nested one.
func caseBranch(script, start, end string) string {
	_, rest, ok := strings.Cut(script, start)
	if !ok {
		return ""
	}
	body, _, _ := strings.Cut(rest, end)
	return body
}

func TestAutomationCommandMetadata(t *testing.T) {
	var parent Command
	for _, cmd := range Commands {
		if cmd.Name == "automation" {
			parent = cmd
		}
		if cmd.Name == "attach" {
			assert.Contains(t, commandFlags(cmd), Flag{Name: "operator-token", Desc: "Operator credential (defaults to KONTORA_OPERATOR_TOKEN)", Value: "text"})
		}
	}
	expected := map[string][]string{
		"ls": {"state", "query", "project", "agent"}, "view": {},
		"new":       {"name", "prompt-file", "path", "agent", "model", "schedule", "timezone", "workspace", "overlap", "timeout", "enabled", "key", "request-id"},
		"update":    {"name", "prompt-file", "path", "agent", "model", "schedule", "timezone", "workspace", "overlap", "timeout", "enabled", "revision"},
		"duplicate": {"name", "key", "enabled", "request-id"},
		"delete":    {"yes", "f", "revision"}, "pause": {"revision"}, "resume": {"revision"},
		"run": {"request-id"}, "stop": {"run"}, "runs": {"offset", "limit"},
		"logs": {"run", "raw"}, "preview": {"schedule", "timezone"}, "terminal": {"run"}, "attach": {"run", "rw"},
	}
	require.Len(t, parent.Subcommands, len(expected))
	for _, child := range parent.Subcommands {
		t.Run(child.Name, func(t *testing.T) {
			want, ok := expected[child.Name]
			require.True(t, ok)
			want = append(append([]string{}, want...), "operator-token", "config", "url", "token")
			if child.Name != "attach" {
				want = append(want, "json")
			}
			var names []string
			for _, f := range commandFlags(child) {
				names = append(names, f.Name)
				if child.Name == "update" {
					switch f.Name {
					case "enabled", "timeout", "timezone", "workspace", "overlap":
						assert.Contains(t, f.Desc, "omitted preserves the current value")
						assert.NotContains(t, f.Desc, "default")
					}
				}
			}
			assert.ElementsMatch(t, want, names)
			assert.False(t, child.TicketID)
			assert.False(t, child.Store)
			assert.NotEmpty(t, child.Desc)
			if child.Name != "ls" && child.Name != "new" && child.Name != "preview" {
				assert.Equal(t, "REF", child.Args)
			}
		})
	}
}

func TestCompletionChildScopes(t *testing.T) {
	for _, shell := range SupportedShells {
		t.Run(shell, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, Completion(shell, &buf))
			for _, scope := range completionScopes() {
				if !strings.Contains(scope.Name, "/") {
					continue
				}
				var body string
				switch shell {
				case "fish":
					body = fishScope(buf.String(), scope.Name)
				case "bash":
					body = caseBranch(buf.String(), "        "+scope.Name+")\n", "\n            ;;\n")
				case "zsh":
					body = caseBranch(buf.String(), "                "+scope.Name+")\n", "\n                    ;;\n")
				}
				for _, f := range commandFlags(scope) {
					match := dashed(f.Name)
					if shell == "fish" {
						match = " -" + flagKind(f.Name) + " " + f.Name + " "
					}
					assert.Contains(t, body, match, scope.Name)
				}
				if scope.Name == "automation/update" && shell != "bash" {
					assert.NotContains(t, body, "default false")
					assert.NotContains(t, body, "default 30m")
					assert.Contains(t, body, "omitted preserves the current value")
				}
				if strings.HasPrefix(scope.Name, "automation/") {
					assert.NotContains(t, body, "tickets-dir", scope.Name)
					assert.NotContains(t, body, "__kontora_ticket_ids", scope.Name)
				}
			}
		})
	}
}

func completionScript(t *testing.T, shell string) (string, string) {
	t.Helper()
	bin, err := exec.LookPath(shell)
	if err != nil {
		t.Skip(shell + " is not installed")
	}
	var buf bytes.Buffer
	require.NoError(t, Completion(shell, &buf))
	path := filepath.Join(t.TempDir(), "completion."+shell)
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return bin, path
}

func TestCompletionShellSyntax(t *testing.T) {
	for _, shell := range SupportedShells {
		t.Run(shell, func(t *testing.T) {
			bin, path := completionScript(t, shell)
			out, err := exec.Command(bin, "-n", path).CombinedOutput()
			require.NoError(t, err, string(out))
		})
	}
}

func TestAutomationCompletionZshRuntime(t *testing.T) {
	bin, script := completionScript(t, "zsh")
	for _, child := range automationCommands() {
		t.Run(child.Name, func(t *testing.T) {
			program := `compdef() { :; }
source "$1"
_arguments() {
    if [[ $1 == -C ]]; then
        state=args
        words=("${words[@]:1}")
        (( CURRENT-- ))
        return 1
    fi
    printf '%s\n' "$@"
}
kontora() { print NETWORK_LOOKUP; }
words=(kontora automation "$2" --)
CURRENT=${#words}
_kontora`
			out, err := exec.Command(bin, "-c", program, "completion-test", script, child.Name).CombinedOutput()
			require.NoError(t, err, string(out))
			for _, flag := range commandFlags(child) {
				assert.Contains(t, string(out), zshFlagSpec(flag, dashed(flag.Name)))
			}
			assert.NotContains(t, string(out), "tickets-dir")
			assert.NotContains(t, string(out), "__kontora_ticket_ids")
			assert.NotContains(t, string(out), "NETWORK_LOOKUP")
			if child.Name == "new" || child.Name == "update" {
				assert.Contains(t, string(out), "--prompt-file=[Prompt template file ('-' for stdin)]:path:_files")
			}
		})
	}
}

func TestAutomationCompletionRuntime(t *testing.T) {
	for _, shell := range []string{"bash", "fish"} {
		t.Run(shell, func(t *testing.T) {
			bin, script := completionScript(t, shell)
			prompt := filepath.Join(t.TempDir(), "prompt with spaces.md")
			require.NoError(t, os.WriteFile(prompt, nil, 0o600))
			cases := []struct {
				name         string
				words        []string
				want, absent []string
			}{
				{"children", []string{"automation", ""}, []string{"ls", "attach", "preview", "terminal"}, []string{"view-ticket"}},
				{"list flags", []string{"automation", "ls", "--"}, []string{"--state", "--query", "--agent", "--json", "--config", "--url", "--token", "--operator-token"}, []string{"--tickets-dir", "--closed", "--run", "--revision"}},
				{"update flags after reference", []string{"automation", "update", "report", "--"}, []string{"--revision", "--enabled", "--prompt-file"}, []string{"--key", "--request-id", "--tickets-dir"}},
				{"attach flags", []string{"automation", "attach", "report", "--"}, []string{"--run", "--rw", "--operator-token"}, []string{"--json", "--revision", "--tickets-dir"}},
				{"state values", []string{"automation", "ls", "--state", ""}, []string{"all", "enabled", "paused"}, nil},
				{"workspace values", []string{"automation", "new", "--workspace", ""}, []string{"worktree", "repo"}, nil},
				{"overlap values", []string{"automation", "update", "report", "--overlap", ""}, []string{"skip", "queue", "replace"}, nil},
				{"prompt path", []string{"automation", "new", "--prompt-file", strings.TrimSuffix(prompt, ".md")}, []string{prompt}, nil},
				{"reference", []string{"automation", "view", ""}, nil, []string{"NETWORK_LOOKUP", "kon-1234"}},
				{"config edit", []string{"config", "edit", "--"}, []string{"--config", "--url", "--token"}, []string{"--tickets-dir"}},
				{"skills children", []string{"skills", ""}, []string{"list", "show"}, nil},
				{"completion children", []string{"completion", ""}, []string{"bash", "fish", "zsh"}, nil},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					quoted := []string{zshQuote("kontora")}
					for _, word := range tc.words {
						quoted = append(quoted, zshQuote(word))
					}
					var program string
					if shell == "bash" {
						program = `source "$1"; kontora() { echo NETWORK_LOOKUP >&2; echo ' kon-1234'; }; COMP_WORDS=(` + strings.Join(quoted, " ") + `); COMP_CWORD=$((${#COMP_WORDS[@]}-1)); _kontora; printf '%s\n' "${COMPREPLY[@]}"`
					} else {
						line := "kontora"
						for _, word := range tc.words {
							if word == "" {
								line += " "
							} else {
								line += " " + zshQuote(word)
							}
						}
						program = `source $argv[1]; function kontora; echo NETWORK_LOOKUP >&2; echo ' kon-1234'; end; complete -C ` + zshQuote(line)
					}
					args := []string{"-c", program, "completion-test", script}
					if shell == "fish" {
						args = []string{"-c", program, script}
					}
					out, err := exec.Command(bin, args...).CombinedOutput()
					require.NoError(t, err, string(out))
					var candidates []string
					for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
						candidate, _, _ := strings.Cut(line, "\t")
						candidates = append(candidates, candidate)
					}
					for _, want := range tc.want {
						assert.Contains(t, candidates, want)
					}
					for _, absent := range tc.absent {
						assert.NotContains(t, candidates, absent)
					}
					assert.NotContains(t, string(out), "NETWORK_LOOKUP")
				})
			}
		})
	}
}

func flagKind(name string) string {
	if len(name) == 1 {
		return "s"
	}
	return "l"
}
