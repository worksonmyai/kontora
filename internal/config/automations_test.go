package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func configWithAutomation(body string) string {
	return "agents:\n  pi:\n    binary: pi\n  custom:\n    binary: /does/not/exist\ndefault_agent: pi\nautomations:\n  nightly-report:\n" + body
}

const automationBody = `    name: Nightly report
    prompt: '{{ .Automation.Name }} {{ .Run.Number }} {{ file "does-not-exist.md" }}'
    path: ~/missing-repository
    schedule: '0 3 * * *'
`

func TestAutomationsDefaultsAndNoFilesystemValidation(t *testing.T) {
	cfg, err := LoadReader(strings.NewReader(configWithAutomation(automationBody)))
	require.NoError(t, err)
	spec := cfg.Automations["nightly-report"]
	require.Equal(t, "pi", spec.Agent)
	require.Equal(t, "UTC", spec.Timezone)
	require.Equal(t, "30m", spec.Timeout)
	require.Equal(t, "worktree", spec.Workspace)
	require.Equal(t, "skip", spec.Overlap)
	require.False(t, spec.Enabled)
	require.Equal(t, "~/missing-repository", spec.Path)
	data, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	roundtrip, err := LoadReader(strings.NewReader(string(data)))
	require.NoError(t, err)
	require.Equal(t, cfg.Automations, roundtrip.Automations)
	_, err = LoadReader(strings.NewReader(configWithAutomation(automationBody + "    agent: custom\n    workspace: repo\n")))
	require.NoError(t, err)
}

func TestAutomationsValidation(t *testing.T) {
	cases := []struct{ name, body string }{
		{"unknown agent", automationBody + "    agent: absent\n"},
		{"unsupported model", automationBody + "    agent: custom\n    model: sonnet\n"},
		{"invalid template", strings.Replace(automationBody, "{{ .Automation.Name }}", "{{ if }}", 1)},
		{"ticket helper", strings.Replace(automationBody, "{{ .Automation.Name }}", "{{ plannotatorReview }}", 1)},
		{"missing name", strings.Replace(automationBody, "    name: Nightly report\n", "", 1)},
		{"missing path", strings.Replace(automationBody, "    path: ~/missing-repository\n", "", 1)},
		{"missing prompt", strings.Replace(automationBody, "    prompt: '{{ .Automation.Name }} {{ .Run.Number }} {{ file \"does-not-exist.md\" }}'\n", "", 1)},
		{"workspace", automationBody + "    workspace: shared\n"},
		{"overlap", automationBody + "    overlap: parallel\n"},
		{"timeout", automationBody + "    timeout: 0m\n"},
		{"timezone", automationBody + "    timezone: Local\n"},
		{"schedule", strings.Replace(automationBody, "0 3 * * *", "@hourly", 1)},
		{"unknown field", automationBody + "    instructions: old\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadReader(strings.NewReader(configWithAutomation(tc.body)))
			require.Error(t, err)
		})
	}
	_, err := LoadReader(strings.NewReader(strings.Replace(configWithAutomation(automationBody), "nightly-report:", "../nightly:", 1)))
	require.Error(t, err)
}
