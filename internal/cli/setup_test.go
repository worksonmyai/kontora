package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestBuildConfigYAML(t *testing.T) {
	cases := []struct {
		name     string
		ans      *SetupAnswers
		wantKeys []string // strings that must appear in output
	}{
		{
			name: "single agent no args",
			ans: &SetupAnswers{
				Agents:              map[string]agentArgs{"claude": {Binary: "claude"}},
				TicketsDir:          "~/.kontora/tickets",
				LogsDir:             "~/.kontora/logs",
				WorktreesDir:        "~/.kontora/worktrees",
				MaxConcurrentAgents: 3,
				WebEnabled:          true,
				WebPort:             8080,
			},
			wantKeys: []string{
				"tickets_dir: ~/.kontora/tickets",
				"binary: claude",
				"on_success: done",
				"on_failure: pause",
				"max_concurrent_agents: 3",
				"enabled: true",
				"port: 8080",
				"implement-review-commit:",
				"stage: implement",
				"stage: review",
				"stage: fix-review",
				"stage: commit",
			},
		},
		{
			name: "agent with args",
			ans: &SetupAnswers{
				Agents: map[string]agentArgs{
					"claude": {Binary: "claude", Args: "--dangerously-skip-permissions"},
				},
				TicketsDir:          "~/.kontora/tickets",
				LogsDir:             "~/.kontora/logs",
				WorktreesDir:        "~/.kontora/worktrees",
				MaxConcurrentAgents: 5,
				WebEnabled:          false,
				WebPort:             9090,
			},
			wantKeys: []string{
				"--dangerously-skip-permissions",
				"max_concurrent_agents: 5",
				"enabled: false",
			},
		},
		{
			name: "multiple agents",
			ans: &SetupAnswers{
				Agents: map[string]agentArgs{
					"claude":   {Binary: "claude", Args: "--flag"},
					"opencode": {Binary: "opencode"},
				},
				TicketsDir:          "/tmp/tickets",
				LogsDir:             "/tmp/logs",
				WorktreesDir:        "/tmp/worktrees",
				MaxConcurrentAgents: 2,
				WebEnabled:          true,
				WebPort:             8080,
			},
			wantKeys: []string{
				"binary: claude",
				"binary: opencode",
				"tickets_dir: /tmp/tickets",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := buildConfigYAML(tc.ans)
			for _, key := range tc.wantKeys {
				assert.Contains(t, yaml, key)
			}

			// Verify the generated YAML parses into a valid config
			cfg, err := config.LoadReader(strings.NewReader(yaml))
			require.NoError(t, err, "generated YAML:\n%s", yaml)
			assert.NotEmpty(t, cfg.Agents)
			assert.NotEmpty(t, cfg.Pipelines)
		})
	}
}

func TestWriteSetupConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "sub", "dir", "config.yaml")

	ans := &SetupAnswers{
		Agents:              map[string]agentArgs{"claude": {Binary: "claude"}},
		TicketsDir:          filepath.Join(tmpDir, "tickets"),
		LogsDir:             filepath.Join(tmpDir, "logs"),
		WorktreesDir:        filepath.Join(tmpDir, "worktrees"),
		MaxConcurrentAgents: 3,
		WebEnabled:          true,
		WebPort:             8080,
	}

	var buf bytes.Buffer
	require.NoError(t, writeSetupConfig(configPath, ans, &buf))

	// File was created
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "binary: claude")

	// Directories were created
	for _, dir := range []string{
		filepath.Join(tmpDir, "tickets"),
		filepath.Join(tmpDir, "logs"),
		filepath.Join(tmpDir, "worktrees"),
	} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	}

	// Output message
	assert.Contains(t, buf.String(), "Config written to")
}

func TestWriteSetupConfig_Validation(t *testing.T) {
	cases := []struct {
		name    string
		ans     *SetupAnswers
		wantErr string
	}{
		{
			name: "no agents",
			ans: &SetupAnswers{
				MaxConcurrentAgents: 3,
			},
			wantErr: "at least one agent",
		},
		{
			name: "zero concurrency",
			ans: &SetupAnswers{
				Agents:              map[string]agentArgs{"a": {Binary: "a"}},
				MaxConcurrentAgents: 0,
			},
			wantErr: "must be positive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := writeSetupConfig(filepath.Join(t.TempDir(), "config.yaml"), tc.ans, &buf)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestRunSetup_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	require.NoError(t, os.WriteFile(configPath, []byte("# existing"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, RunSetup(configPath, &buf))

	assert.Contains(t, buf.String(), "already exists")

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, "# existing", string(data))
}

func updateSetup(m setupModel, key string) setupModel {
	var msg tea.KeyMsg
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEscape}
	case "ctrl+c":
		msg = tea.KeyMsg{Type: tea.KeyCtrlC}
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	result, _ := m.Update(msg)
	return result.(setupModel)
}

func TestSetupModel_AgentSelection(t *testing.T) {
	m := initialSetupModel()
	assert.Equal(t, stepAgents, m.step)

	// Deselect all
	for _, name := range m.agentNames {
		m.agentChecked[name] = false
	}

	// Trying to advance without selection shows error
	m = updateSetup(m, "enter")
	assert.Equal(t, stepAgents, m.step, "should stay on agents step")
	assert.NotEmpty(t, m.err)

	// Select first agent
	m = updateSetup(m, " ")
	assert.True(t, m.agentChecked[m.agentNames[0]])

	// Navigate down
	m = updateSetup(m, "j")
	assert.Equal(t, 1, m.agentCursor)

	// Navigate up
	m = updateSetup(m, "k")
	assert.Equal(t, 0, m.agentCursor)

	// Advance to args
	m = updateSetup(m, "enter")
	assert.Equal(t, stepArgs, m.step)
}

func TestSetupModel_ArgsInput(t *testing.T) {
	m := initialSetupModel()
	// Force a known state: one agent selected
	for name := range m.agentChecked {
		m.agentChecked[name] = false
	}
	m.agentChecked["claude"] = true
	m = updateSetup(m, "enter") // advance to args
	assert.Equal(t, stepArgs, m.step)

	// Type some args
	m = updateSetup(m, "-")
	m = updateSetup(m, "-")
	m = updateSetup(m, "x")
	assert.Contains(t, m.argsInputs["claude"], "--x")

	// Backspace
	m = updateSetup(m, "backspace")
	assert.True(t, strings.HasSuffix(m.argsInputs["claude"], "--"), "backspace should remove last char")

	// Enter advances to dirs
	m = updateSetup(m, "enter")
	assert.Equal(t, stepDirs, m.step)
}

func TestSetupModel_DirsInput(t *testing.T) {
	m := initialSetupModel()
	for name := range m.agentChecked {
		m.agentChecked[name] = false
	}
	m.agentChecked["claude"] = true
	m = updateSetup(m, "enter") // -> args
	m = updateSetup(m, "enter") // -> dirs
	assert.Equal(t, stepDirs, m.step)

	// Should start editing first field
	assert.True(t, m.dirEditing)
	assert.Equal(t, 0, m.dirCursor)

	// Enter advances through each dir field
	m = updateSetup(m, "enter")
	assert.Equal(t, 1, m.dirCursor)

	m = updateSetup(m, "enter")
	assert.Equal(t, 2, m.dirCursor)

	// Final enter advances to settings
	m = updateSetup(m, "enter")
	assert.Equal(t, stepSettings, m.step)
}

func TestSetupModel_Settings(t *testing.T) {
	m := initialSetupModel()
	for name := range m.agentChecked {
		m.agentChecked[name] = false
	}
	m.agentChecked["claude"] = true
	m = updateSetup(m, "enter") // -> args
	m = updateSetup(m, "enter") // -> dirs
	m = updateSetup(m, "enter") // dir 0 -> 1
	m = updateSetup(m, "enter") // dir 1 -> 2
	m = updateSetup(m, "enter") // dir 2 -> settings
	assert.Equal(t, stepSettings, m.step)

	// First field is max_concurrent, enter advances
	m = updateSetup(m, "enter")
	assert.Equal(t, 1, m.settingCursor) // web_enabled

	// Space toggles web
	m = updateSetup(m, " ")
	assert.False(t, m.webEnabled)
	m = updateSetup(m, " ")
	assert.True(t, m.webEnabled)

	// Enter advances through web enabled -> web port
	m = updateSetup(m, "enter")
	assert.Equal(t, 2, m.settingCursor)

	// Enter on last field advances to pipelines
	m = updateSetup(m, "enter")
	assert.Equal(t, stepPipelines, m.step)

	// Enter on pipelines advances to confirm
	m = updateSetup(m, "enter")
	assert.Equal(t, stepConfirm, m.step)
}

func TestSetupModel_SettingsNavigation(t *testing.T) {
	m := setupModel{
		step:          stepSettings,
		settingCursor: 0,
		maxConcurrent: "3",
		webEnabled:    true,
		webPort:       "8080",
	}

	// Navigate down to web_enabled (cursor 1), then to web_port (cursor 2)
	m = updateSetup(m, "j")
	assert.Equal(t, 1, m.settingCursor)
	assert.False(t, m.settingEditing, "web_enabled is a toggle, not editing")

	m = updateSetup(m, "j")
	assert.Equal(t, 2, m.settingCursor)
	assert.False(t, m.settingEditing, "navigation should not auto-enable editing")

	// Navigate back up — should not get trapped
	m = updateSetup(m, "k")
	assert.Equal(t, 1, m.settingCursor)

	m = updateSetup(m, "k")
	assert.Equal(t, 0, m.settingCursor)
}

func TestSetupModel_Confirm(t *testing.T) {
	m := setupModel{step: stepConfirm, selectedAgents: []string{"claude"}, argsInputs: map[string]string{"claude": ""}, dirFields: [3]string{"a", "b", "c"}, dirLabels: [3]string{"d", "e", "f"}, maxConcurrent: "3", webPort: "8080", webEnabled: true}

	// View should contain summary
	view := m.View()
	assert.Contains(t, view, "Summary")
	assert.Contains(t, view, "claude")

	// 'y' confirms
	m = updateSetup(m, "y")
	assert.True(t, m.done)

	// 'n' cancels
	m2 := setupModel{step: stepConfirm, selectedAgents: []string{"claude"}, argsInputs: map[string]string{"claude": ""}, dirFields: [3]string{"a", "b", "c"}, dirLabels: [3]string{"d", "e", "f"}, maxConcurrent: "3", webPort: "8080"}
	m2 = updateSetup(m2, "n")
	assert.True(t, m2.cancelled)
}

func TestSetupModel_CtrlCCancels(t *testing.T) {
	m := initialSetupModel()
	m = updateSetup(m, "ctrl+c")
	assert.True(t, m.cancelled)
}

func TestSetupModel_Answers(t *testing.T) {
	m := setupModel{
		selectedAgents: []string{"claude"},
		argsInputs:     map[string]string{"claude": "--dangerously-skip-permissions"},
		dirFields:      [3]string{"~/.kontora/tickets", "~/.kontora/logs", "~/.kontora/worktrees"},
		maxConcurrent:  "5",
		webEnabled:     true,
		webPort:        "9090",
	}

	ans := m.answers()
	assert.Equal(t, 5, ans.MaxConcurrentAgents)
	assert.Equal(t, 9090, ans.WebPort)
	assert.True(t, ans.WebEnabled)
	assert.Equal(t, "claude", ans.Agents["claude"].Binary)
	assert.Equal(t, "--dangerously-skip-permissions", ans.Agents["claude"].Args)
}

func TestSetupModel_Answers_InvalidPort(t *testing.T) {
	cases := []struct {
		name     string
		port     string
		wantPort int
	}{
		{"too high", "99999", 8080},
		{"zero", "0", 8080},
		{"negative", "-1", 8080},
		{"empty", "", 8080},
		{"valid", "3000", 3000},
		{"max valid", "65535", 65535},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := setupModel{
				selectedAgents: []string{"claude"},
				argsInputs:     map[string]string{"claude": ""},
				maxConcurrent:  "3",
				webPort:        tc.port,
			}
			ans := m.answers()
			assert.Equal(t, tc.wantPort, ans.WebPort)
		})
	}
}
