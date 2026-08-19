package cli

import (
	"bytes"
	"fmt"
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
	}{
		{
			name:  "setup and its boolean --agent flag",
			shell: "fish",
			want: []string{
				"-a setup -d 'Create the configuration file'",
				"'__fish_seen_subcommand_from setup' -l agent",
				"'__fish_seen_subcommand_from setup' -l config",
			},
		},
		{
			name:  "flags that take a value are marked -r",
			shell: "fish",
			want: []string{
				"'__fish_seen_subcommand_from logs' -l stage -d 'Stage name' -r",
				"'__fish_seen_subcommand_from archive' -l status -d 'Only archive tickets with this status' -x -a 'done cancelled closed'",
				"'__fish_seen_subcommand_from delete' -s f",
			},
		},
		{
			name:  "remote flags reach every remote-capable command",
			shell: "fish",
			want: []string{
				"'__fish_seen_subcommand_from move' -l url",
				"'__fish_seen_subcommand_from move' -l token",
			},
		},
		{
			name:  "ticket IDs are extracted from whole lines, not the matched space",
			shell: "fish",
			// Without -e/--entire, string match prints only the matched
			// whitespace and the function yields nothing at all.
			want: []string{"string match -re", `awk '$1 != "ID" {print $1}'`},
		},
		{
			name:    "unsupported shell",
			shell:   "bash",
			wantErr: "unsupported shell",
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
		})
	}
}

// TestCompletionCoversEveryCommand is the guard against the two lists drifting
// apart: every verb in the table has to be offered, and every ticket-taking
// verb has to be in the dynamic ID list.
func TestCompletionCoversEveryCommand(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, Completion("fish", &buf))
	script := buf.String()

	idLine := ""
	for line := range strings.SplitSeq(script, "\n") {
		if strings.HasPrefix(line, "set -l __kontora_id_cmds ") {
			idLine = line
		}
	}
	require.NotEmpty(t, idLine, "the dynamic ticket ID list is missing")

	for _, cmd := range Commands {
		t.Run(cmd.Name, func(t *testing.T) {
			assert.Contains(t, script, fmt.Sprintf("-a %s -d ", cmd.Name), "command is not offered")
			if cmd.TicketID {
				assert.Contains(t, idLine, " "+cmd.Name, "command takes a ticket ID but is not in the ID list")
			}
			for _, f := range cmd.Flags {
				assert.Contains(t, script, fmt.Sprintf("'__fish_seen_subcommand_from %s' -%s %s ", cmd.Name, flagKind(f.Name), f.Name))
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
