package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validSetupConfig = `tickets_dir: ~/.kontora/tickets
agents:
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

func TestWriteAgentSetupPrompt(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the config path the brief is built for.
		setup   func(t *testing.T, dir string) string
		want    []string
		notWant []string
	}{
		{
			name: "missing config",
			setup: func(_ *testing.T, dir string) string {
				return filepath.Join(dir, "config.yaml")
			},
			want: []string{
				"State: missing",
				"There is no file at that path.",
				"## Interview",
				"## Validation rules",
				"## Apply procedure",
				"kontora doctor --config",
			},
			notWant: []string{"State: valid", "State: invalid", "Symlink target:"},
		},
		{
			name: "valid config",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "config.yaml")
				require.NoError(t, os.WriteFile(path, []byte(validSetupConfig), 0o644))
				return path
			},
			want: []string{
				"State: valid",
				"Change only what the user asks for.",
			},
			notWant: []string{"State: missing", "Symlink target:"},
		},
		{
			name: "invalid config",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "config.yaml")
				require.NoError(t, os.WriteFile(path, []byte("unknown: true\n"), 0o644))
				return path
			},
			want: []string{
				"State: invalid",
				"does not load",
				// The parse error itself, so the agent knows what to repair.
				"field unknown not found",
			},
			notWant: []string{"State: valid", "State: missing"},
		},
		{
			name: "symlinked config",
			setup: func(t *testing.T, dir string) string {
				target := filepath.Join(dir, "dotfiles", "config.yaml")
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
				require.NoError(t, os.WriteFile(target, []byte(validSetupConfig), 0o644))
				link := filepath.Join(dir, "config.yaml")
				require.NoError(t, os.Symlink(target, link))
				return link
			},
			want: []string{
				"State: valid",
				"Symlink target:",
				"dotfiles/config.yaml",
				"keep the link",
				"Write through the target",
			},
		},
		{
			name: "dangling symlink",
			setup: func(t *testing.T, dir string) string {
				link := filepath.Join(dir, "config.yaml")
				require.NoError(t, os.Symlink(filepath.Join(dir, "gone.yaml"), link))
				return link
			},
			want: []string{
				"State: missing",
				"Symlink target:",
				"gone.yaml",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t, t.TempDir())

			var buf bytes.Buffer
			require.NoError(t, WriteAgentSetupPrompt(path, &buf))
			out := buf.String()

			assert.Contains(t, out, path, "the brief must name the resolved config path")
			for _, want := range tc.want {
				assert.Contains(t, out, want)
			}
			for _, notWant := range tc.notWant {
				assert.NotContains(t, out, notWant)
			}
		})
	}
}

// The brief is printed to a terminal and often piped into another tool, so it
// must stay plain text and must not carry secrets across. The invalid case
// matters on its own: that is the one path where the brief quotes the loader,
// and the loader has seen the file.
func TestWriteAgentSetupPrompt_PlainAndSecretFree(t *testing.T) {
	sensitive := validSetupConfig + `
web:
  token: s3nt1nel-token-value
environment:
  MY_SECRET: s3nt1nel-env-value
`

	cases := []struct {
		name   string
		config string
		want   string
	}{
		{name: "config loads", config: sensitive, want: "State: valid"},
		{name: "config does not load", config: sensitive + "unknown: true\n", want: "State: invalid"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.config), 0o644))

			var buf bytes.Buffer
			require.NoError(t, WriteAgentSetupPrompt(path, &buf))
			out := buf.String()

			assert.Contains(t, out, tc.want)
			assert.NotContains(t, out, "s3nt1nel-token-value")
			assert.NotContains(t, out, "s3nt1nel-env-value")
			assert.NotContains(t, out, "MY_SECRET")
			assert.NotContains(t, out, "\x1b", "output must carry no ANSI escape sequences")

			// The brief shows stage prompt examples. Rendering must leave
			// Kontora's own template syntax alone, or the agent copies a broken
			// prompt into the config.
			assert.Contains(t, out, "{{ .Ticket.Description }}")
			assert.Contains(t, out, `{{ file "PLAN.md" }}`)

			// Rendering with unresolved fields would leave Go's error marker behind.
			assert.NotContains(t, out, "<no value>")
			assert.NotContains(t, out, "[[")
		})
	}
}
