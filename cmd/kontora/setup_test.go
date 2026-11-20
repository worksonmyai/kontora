package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/config"
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

func TestSetupRecipePreview(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		existing bool
		pipeline string
		stages   []string
		wantErr  string
	}{
		{name: "review with existing config", args: []string{"--recipe", "implement-review"}, existing: true, pipeline: "implement-review", stages: []string{"starter-implement", "starter-review"}},
		{name: "plan without config or store", args: []string{"--recipe=plan-only"}, pipeline: "plan-only", stages: []string{"starter-plan"}},
		{name: "empty", args: []string{"--recipe="}, wantErr: "--recipe requires a name"},
		{name: "omitted value", args: []string{"--recipe"}, wantErr: "flag needs an argument"},
		{name: "unknown", args: []string{"--recipe=unknown"}, wantErr: "unknown setup recipe"},
		{name: "agent conflict", args: []string{"--recipe=plan-only", "--agent"}, wantErr: "--recipe cannot be combined"},
		{name: "explicit false still conflicts", args: []string{"--agent=false", "--recipe=plan-only"}, wantErr: "--recipe cannot be combined"},
		{name: "config conflict", args: []string{"--config=", "--recipe=plan-only"}, wantErr: "--recipe cannot be combined"},
		{name: "extra argument", args: []string{"--recipe=plan-only", "extra"}, wantErr: "unexpected argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			storeDir := filepath.Join(dir, "tickets")
			ticketPath := filepath.Join(storeDir, "tst-1234.md")
			configData := fmt.Sprintf("tickets_dir: %q\nagents:\n  pi:\n    binary: pi\n", storeDir)
			const ticketData = "---\nid: tst-1234\nstatus: todo\n---\n# Existing ticket\n"
			if tc.existing {
				require.NoError(t, os.Mkdir(storeDir, 0o755))
				require.NoError(t, os.WriteFile(configPath, []byte(configData), 0o644))
				require.NoError(t, os.WriteFile(ticketPath, []byte(ticketData), 0o644))
			}

			stdout, stderr, err := runCLISeparated(t, []string{config.PathEnvVar + "=" + configPath, config.TicketsDirEnvVar + "=" + storeDir}, nil, append([]string{"setup"}, tc.args...)...)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Empty(t, stdout)
				assert.Contains(t, stderr, tc.wantErr)
				assert.NotContains(t, stderr, "needs an interactive terminal")
			} else {
				require.NoError(t, err, stderr)
				assert.Empty(t, stderr)
				var fragment struct {
					Stages    map[string]config.Stage
					Pipelines map[string]config.Pipeline
				}
				dec := yaml.NewDecoder(strings.NewReader(stdout))
				dec.KnownFields(true)
				require.NoError(t, dec.Decode(&fragment))
				require.ErrorIs(t, dec.Decode(new(any)), io.EOF)
				require.Len(t, fragment.Pipelines, 1)
				require.Len(t, fragment.Stages, len(tc.stages))
				steps := fragment.Pipelines[tc.pipeline]
				require.Len(t, steps, len(tc.stages))
				for i, stage := range tc.stages {
					assert.Equal(t, stage, steps[i].Stage)
					assert.Equal(t, "YOUR_AGENT", steps[i].Agent)
					assert.NotEmpty(t, fragment.Stages[stage].Prompt)
				}
			}

			if !tc.existing {
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)
				assert.Empty(t, entries)
				return
			}
			for path, want := range map[string]string{configPath: configData, ticketPath: ticketData} {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, want, string(data), path)
			}
			entries, err := os.ReadDir(storeDir)
			require.NoError(t, err)
			require.Len(t, entries, 1)
		})
	}
}
