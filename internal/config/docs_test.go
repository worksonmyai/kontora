package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/prompt"
)

func TestDocumentedConfigs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		heading string
	}{
		{"minimal", "../../docs/configuration.md", "## Minimal example"},
		{"full", "../../docs/configuration.md", "## Full example"},
		{"agent setup", "../cli/setup_agent_prompt.md", "## Config shape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(tc.path)
			require.NoError(t, err)
			_, section, ok := strings.Cut(string(data), tc.heading+"\n")
			require.True(t, ok, "missing example heading")
			section, _, _ = strings.Cut(section, "\n## ")
			_, example, ok := strings.Cut(section, "```yaml\n")
			require.True(t, ok, "missing YAML example")
			example, _, ok = strings.Cut(example, "\n```")
			require.True(t, ok, "unterminated YAML example")

			cfg, err := config.LoadReader(strings.NewReader(example))
			require.NoError(t, err)
			require.NotEmpty(t, cfg.DefaultPipeline)
			pipeline := cfg.Pipelines[cfg.DefaultPipeline].Legacy
			require.NotEmpty(t, pipeline)
			assert.Equal(t, "human_review", pipeline[len(pipeline)-1].OnSuccess)
			first := cfg.Stages[pipeline[0].Stage]
			assert.Contains(t, first.Prompt, "{{ .Ticket.Description }}")
			assert.Contains(t, first.Prompt, "Do NOT commit or push")
			assert.Positive(t, first.Timeout.Duration)

			workDir := t.TempDir()
			for name, stage := range cfg.Stages {
				rendered, err := prompt.Render(stage.Prompt, prompt.Data{Ticket: prompt.TicketData{
					ID: "test-1234", Title: "Example task", Description: "Example requirements",
				}}, workDir)
				require.NoError(t, err, "stage %s", name)
				assert.NotEmpty(t, rendered, "stage %s", name)
			}
		})
	}
}
