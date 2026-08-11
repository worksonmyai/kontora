package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCLI gives the subprocess a pipe for stdin and stdout, so plain `setup`
// always takes the non-interactive path here. The wizard itself is covered by
// the model tests in internal/cli.
func TestSetupCommand(t *testing.T) {
	validConfig := `agents:
  claude:
    binary: claude
stages:
  code:
    prompt: Write code.
pipelines:
  default:
    - stage: code
      agent: claude
      on_success: human_review
      on_failure: pause
`

	cases := []struct {
		name string
		// config returns the file to write at the config path, or "" for none.
		config string
		// path builds the --config value, defaulting to a file in a fresh temp
		// dir. Only the unusable-path case needs its own.
		path    func(t *testing.T, dir string) string
		args    []string
		env     []string
		wantErr bool
		// wantPath asserts the output names the resolved config path, which is
		// how a case proves --config reached the command.
		wantPath bool
		want     []string
		notWant  []string
	}{
		{
			// The state classification itself is covered by the unit table in
			// internal/cli. This case only proves the flags reach it.
			name:     "agent brief for the path --config names",
			args:     []string{"setup", "--agent"},
			wantPath: true,
			want:     []string{"Kontora setup brief", "State: missing", "kontora doctor --config"},
			notWant:  []string{"State: valid"},
		},
		{
			name:    "wizard without a terminal",
			args:    []string{"setup"},
			wantErr: true,
			want:    []string{"needs an interactive terminal", "kontora setup --agent"},
		},
		{
			// A config path that cannot be stat'ed is not a missing config. The
			// wizard must not start, and the real error has to surface.
			name: "config path is unusable",
			path: func(t *testing.T, dir string) string {
				blocker := filepath.Join(dir, "not-a-dir")
				require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))
				return filepath.Join(blocker, "config.yaml")
			},
			args:    []string{"setup"},
			wantErr: true,
			want:    []string{"not a directory"},
			notWant: []string{"needs an interactive terminal"},
		},
		{
			name:   "existing config is left alone",
			config: validConfig,
			args:   []string{"setup"},
			want:   []string{"already exists", "kontora setup --agent", "kontora config edit"},
		},
		{
			// --agent takes no value, so "claude" lands as a positional argument
			// and the brief must not print as if it had been understood.
			name:    "positional argument rejected",
			args:    []string{"setup", "--agent", "claude"},
			wantErr: true,
			want:    []string{`unexpected argument "claude"`},
			notWant: []string{"Kontora setup brief"},
		},
		{
			name:    "rejected in remote mode",
			args:    []string{"setup", "--agent"},
			env:     []string{"KONTORA_URL=http://127.0.0.1:1"},
			wantErr: true,
			want:    []string{"not available in remote mode"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if tc.path != nil {
				configPath = tc.path(t, dir)
			}
			if tc.config != "" {
				require.NoError(t, os.WriteFile(configPath, []byte(tc.config), 0o644))
			}

			// --config goes in right after the verb: Go's flag package stops at
			// the first positional argument, so appending it would leave the
			// positional-argument case pointed at the real default config path.
			args := append([]string{tc.args[0], "--config", configPath}, tc.args[1:]...)
			out, err := runCLI(t, tc.env, args...)
			if tc.wantErr {
				require.Error(t, err, out)
			} else {
				require.NoError(t, err, out)
			}

			if tc.wantPath {
				assert.Contains(t, out, configPath)
			}
			for _, want := range tc.want {
				assert.Contains(t, out, want)
			}
			for _, notWant := range tc.notWant {
				assert.NotContains(t, out, notWant)
			}

			// setup --agent only reads. Nothing here may create or change a file.
			if tc.config == "" {
				_, statErr := os.Stat(configPath)
				assert.Error(t, statErr, "setup must not create a config")
				return
			}
			data, readErr := os.ReadFile(configPath)
			require.NoError(t, readErr)
			assert.Equal(t, tc.config, string(data), "existing config must stay byte-for-byte")
		})
	}
}
