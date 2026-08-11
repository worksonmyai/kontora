package cli

import (
	"bytes"
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
			name:  "fish covers the setup command and its flags",
			shell: "fish",
			want: []string{
				"-a setup -d 'Create the configuration file'",
				// --config is offered on setup, so the list it comes from has
				// to name the command.
				"set -l __kontora_config_cmds start setup ",
				"'__fish_seen_subcommand_from setup' -l agent",
			},
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
