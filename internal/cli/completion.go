package cli

import (
	"fmt"
	"io"
)

// Completion writes a shell completion script to w.
// Currently only "fish" is supported.
func Completion(shell string, w io.Writer) error {
	switch shell {
	case "fish":
		_, err := fmt.Fprint(w, fishCompletion)
		return err
	default:
		return fmt.Errorf("unsupported shell: %s (supported: fish)", shell)
	}
}

const fishCompletion = `# kontora fish completions
# Install: kontora completion fish | source
# Persist: kontora completion fish > ~/.config/fish/completions/kontora.fish

# Disable file completions by default
complete -c kontora -f

# Top-level commands
complete -c kontora -n __fish_use_subcommand -a ls -d 'List tickets'
complete -c kontora -n __fish_use_subcommand -a new -d 'Create a ticket'
complete -c kontora -n __fish_use_subcommand -a view -d 'Print ticket details'
complete -c kontora -n __fish_use_subcommand -a edit -d 'Open a ticket in $EDITOR'
complete -c kontora -n __fish_use_subcommand -a init -d 'Set up ticket for processing'
complete -c kontora -n __fish_use_subcommand -a done -d 'Close a ticket'
complete -c kontora -n __fish_use_subcommand -a note -d 'Append note to ticket'
complete -c kontora -n __fish_use_subcommand -a pause -d 'Pause a ticket'
complete -c kontora -n __fish_use_subcommand -a run -d 'Enqueue a ticket for processing'
complete -c kontora -n __fish_use_subcommand -a retry -d 'Re-queue a ticket'
complete -c kontora -n __fish_use_subcommand -a skip -d 'Skip to next pipeline stage'
complete -c kontora -n __fish_use_subcommand -a set-stage -d 'Move ticket to a specific pipeline stage'
complete -c kontora -n __fish_use_subcommand -a cancel -d 'Cancel a ticket'
complete -c kontora -n __fish_use_subcommand -a logs -d 'Show agent logs'
complete -c kontora -n __fish_use_subcommand -a attach -d 'Attach to running ticket'
complete -c kontora -n __fish_use_subcommand -a start -d 'Start the daemon'
complete -c kontora -n __fish_use_subcommand -a doctor -d 'Validate setup'
complete -c kontora -n __fish_use_subcommand -a config -d 'Show effective config'
complete -c kontora -n __fish_use_subcommand -a fmt -d 'Format stream-json from stdin'
complete -c kontora -n __fish_use_subcommand -a version -d 'Print version'
complete -c kontora -n __fish_use_subcommand -a completion -d 'Generate shell completions'

# completion subcommand
complete -c kontora -n '__fish_seen_subcommand_from completion' -a fish -d 'Fish shell'

# Flags: -config (commands that accept it)
set -l __kontora_config_cmds start doctor ls new view edit init run done note pause retry skip set-stage cancel logs attach config
for cmd in $__kontora_config_cmds
    complete -c kontora -n "__fish_seen_subcommand_from $cmd" -o config -d 'Config file path' -r -F
end

# Flags: new
complete -c kontora -n '__fish_seen_subcommand_from new' -o path -d 'Repository path' -r -F
complete -c kontora -n '__fish_seen_subcommand_from new' -o pipeline -d 'Pipeline name' -r

# Flags: ls
complete -c kontora -n '__fish_seen_subcommand_from ls' -l closed -d 'Show done/cancelled tickets'
complete -c kontora -n '__fish_seen_subcommand_from ls' -l all -d 'Show all tickets'
complete -c kontora -n '__fish_seen_subcommand_from ls' -l static -d 'Static table output'

# Flags: logs
complete -c kontora -n '__fish_seen_subcommand_from logs' -o stage -d 'Stage name' -r

# Flags: attach
complete -c kontora -n '__fish_seen_subcommand_from attach' -o rw -d 'Read-write mode'

# Dynamic ticket ID completion
function __kontora_ticket_ids
    kontora ls --closed --static 2>/dev/null | string match -r '^\s' | awk '{print $1}'
end
set -l __kontora_id_cmds view edit init run done note pause retry skip set-stage cancel logs attach
for cmd in $__kontora_id_cmds
    complete -c kontora -n "__fish_seen_subcommand_from $cmd" -a '(__kontora_ticket_ids)'
end
`
