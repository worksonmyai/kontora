package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
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
				"on_success: human_review",
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
				"default_agent: claude",
			},
		},
		{
			// Nothing here is named "claude" and there is more than one agent, so
			// the config cannot infer default_agent. Setup has to write it.
			name: "multiple agents without claude",
			ans: &SetupAnswers{
				Agents: map[string]agentArgs{
					"pi":       {Binary: "pi", Args: "-p --no-session"},
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
				"binary: pi",
				"binary: opencode",
				"default_agent: opencode",
			},
		},
		{
			name: "a chosen pipeline becomes default_pipeline",
			ans: &SetupAnswers{
				Agents:              map[string]agentArgs{"claude": {Binary: "claude"}},
				TicketsDir:          "/tmp/tickets",
				LogsDir:             "/tmp/logs",
				WorktreesDir:        "/tmp/worktrees",
				MaxConcurrentAgents: 3,
				WebEnabled:          true,
				WebPort:             8080,
				DefaultPipeline:     "implement-review-commit",
			},
			wantKeys: []string{"default_pipeline: implement-review-commit"},
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
			assert.Contains(t, cfg.Agents, cfg.DefaultAgent)
			assert.Equal(t, tc.ans.DefaultPipeline, cfg.DefaultPipeline)
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

// Every rejected setup must also leave the disk untouched: a config directory
// or a runtime directory left behind means the next run finds half a setup.
func TestWriteSetupConfig_Validation(t *testing.T) {
	// The answer directories are built per case, under that case's temp dir.
	answers := func(dir string, agents map[string]agentArgs, concurrency int) *SetupAnswers {
		return &SetupAnswers{
			Agents:              agents,
			TicketsDir:          filepath.Join(dir, "tickets"),
			LogsDir:             filepath.Join(dir, "logs"),
			WorktreesDir:        filepath.Join(dir, "worktrees"),
			MaxConcurrentAgents: concurrency,
			WebEnabled:          true,
			WebPort:             8080,
		}
	}

	cases := []struct {
		name    string
		ans     func(dir string) *SetupAnswers
		wantErr string
	}{
		{
			name:    "no agents",
			ans:     func(dir string) *SetupAnswers { return answers(dir, nil, 3) },
			wantErr: "at least one agent",
		},
		{
			name: "zero concurrency",
			ans: func(dir string) *SetupAnswers {
				return answers(dir, map[string]agentArgs{"a": {Binary: "a"}}, 0)
			},
			wantErr: "must be positive",
		},
		{
			// "none" is the project-default opt-out sentinel, so an agent by that
			// name builds YAML that does not load.
			name: "generated config does not load",
			ans: func(dir string) *SetupAnswers {
				return answers(dir, map[string]agentArgs{config.NoneSentinel: {Binary: "none"}}, 3)
			},
			wantErr: "generated config is invalid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "sub", "config.yaml")
			ans := tc.ans(dir)

			var buf bytes.Buffer
			require.ErrorContains(t, writeSetupConfig(configPath, ans, &buf), tc.wantErr)

			for _, path := range []string{
				configPath,
				filepath.Dir(configPath),
				ans.TicketsDir,
				ans.LogsDir,
				ans.WorktreesDir,
			} {
				_, err := os.Stat(path)
				assert.ErrorIs(t, err, os.ErrNotExist, "%s must not be created", path)
			}
		})
	}
}

func TestRunSetup_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	require.NoError(t, os.WriteFile(configPath, []byte("# existing"), 0o644))

	var buf bytes.Buffer
	require.NoError(t, RunSetup(configPath, &buf))

	out := buf.String()
	assert.Contains(t, out, "already exists")
	assert.Contains(t, out, configPath)
	assert.Contains(t, out, "kontora setup --agent")
	assert.Contains(t, out, "kontora config edit")

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, "# existing", string(data))
}

// newSetupInputs builds the three directory fields a test model needs.
func newSetupInputs(a, b, c string) [3]textinput.Model {
	return [3]textinput.Model{newSetupInput(a), newSetupInput(b), newSetupInput(c)}
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
	case "shift+tab":
		msg = tea.KeyMsg{Type: tea.KeyShiftTab}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEscape}
	case "ctrl+c":
		msg = tea.KeyMsg{Type: tea.KeyCtrlC}
	case " ":
		// A real space key carries its rune, which is what a text field inserts.
		msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "left":
		msg = tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		msg = tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+u":
		msg = tea.KeyMsg{Type: tea.KeyCtrlU}
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
	assert.Contains(t, m.argsInputs["claude"].Value(), "--x")

	// Backspace
	m = updateSetup(m, "backspace")
	assert.True(t, strings.HasSuffix(m.argsInputs["claude"].Value(), "--"), "backspace should remove last char")

	// Enter advances to dirs
	m = updateSetup(m, "enter")
	assert.Equal(t, stepDirs, m.step)
}

// TestSetupModel_TextFieldEditing covers what the hand-rolled fields could not
// do: move the cursor, cut the line, and leave the field without losing the
// wizard.
func TestSetupModel_TextFieldEditing(t *testing.T) {
	// Walk to the directories step with one agent selected.
	atDirs := func(t *testing.T) setupModel {
		t.Helper()
		m := initialSetupModel()
		for name := range m.agentChecked {
			m.agentChecked[name] = false
		}
		m.agentChecked["claude"] = true
		m = updateSetup(m, "enter") // -> args
		m = updateSetup(m, "enter") // -> dirs
		require.Equal(t, stepDirs, m.step)
		return m
	}

	t.Run("a character goes in at the cursor, not at the end", func(t *testing.T) {
		m := atDirs(t)
		before := m.dirFields[0].Value()

		m = updateSetup(m, "left")
		m = updateSetup(m, "X")

		want := before[:len(before)-1] + "X" + before[len(before)-1:]
		assert.Equal(t, want, m.dirFields[0].Value())
	})

	t.Run("ctrl+u clears to the start of the line", func(t *testing.T) {
		m := atDirs(t)

		m = updateSetup(m, "ctrl+u")

		assert.Empty(t, m.dirFields[0].Value())
	})

	t.Run("esc leaves the field and keeps every answer", func(t *testing.T) {
		m := atDirs(t)
		dirs := [3]string{m.dirFields[0].Value(), m.dirFields[1].Value(), m.dirFields[2].Value()}

		m = updateSetup(m, "esc")

		assert.False(t, m.cancelled, "esc must not cancel the wizard")
		assert.Equal(t, stepDirs, m.step)
		assert.False(t, m.dirEditing)
		for i, want := range dirs {
			assert.Equal(t, want, m.dirFields[i].Value())
		}
		assert.Equal(t, "--dangerously-skip-permissions", m.argsInputs["claude"].Value())

		// And enter puts the cursor back in the field.
		m = updateSetup(m, "enter")
		assert.True(t, m.dirEditing)
	})

	t.Run("a letter cannot reach a numeric field", func(t *testing.T) {
		m := atDirs(t)
		m = updateSetup(m, "enter") // dir 0 -> 1
		m = updateSetup(m, "enter") // dir 1 -> 2
		m = updateSetup(m, "enter") // dir 2 -> settings
		require.Equal(t, stepSettings, m.step)

		m = updateSetup(m, "x")
		assert.Equal(t, "3", m.maxConcurrent.Value())

		m = updateSetup(m, "5")
		assert.Equal(t, "35", m.maxConcurrent.Value())
	})

	t.Run("shift+tab still steps back from a field the user left", func(t *testing.T) {
		m := atDirs(t)
		m = updateSetup(m, "esc")
		require.False(t, m.dirEditing)

		m = updateSetup(m, "shift+tab")
		assert.Equal(t, stepArgs, m.step, "the first directory steps back to the args")

		m = updateSetup(m, "esc")
		m = updateSetup(m, "shift+tab")
		assert.Equal(t, stepAgents, m.step, "the first agent steps back to the agent list")
	})

	t.Run("text wider than one key cannot reach a numeric field", func(t *testing.T) {
		atSettings := func(t *testing.T) setupModel {
			t.Helper()
			m := atDirs(t)
			for range 3 {
				m = updateSetup(m, "enter")
			}
			require.Equal(t, stepSettings, m.step)
			return m
		}

		cases := []struct {
			name string
			key  tea.KeyMsg
			want string
		}{
			{
				// A fast burst arrives as one event, so the key is not one rune.
				name: "a coalesced burst",
				key:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9x")},
				want: "3",
			},
			{
				name: "a bracketed paste",
				key:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc"), Paste: true},
				want: "3",
			},
			{
				name: "a multi-byte rune",
				key:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("é")},
				want: "3",
			},
			{
				name: "digits still go in",
				key:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("42")},
				want: "342",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := atSettings(t)

				result, _ := m.Update(tc.key)
				m = result.(setupModel)

				// The confirm screen prints this value and answers() writes it,
				// so the two only agree while it stays a number.
				assert.Equal(t, tc.want, m.maxConcurrent.Value())
				assert.Equal(t, tc.want, strconv.Itoa(m.answers().MaxConcurrentAgents),
					"the confirm screen shows the field, so the written config has to be the same number")
			})
		}
	})
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
		maxConcurrent: newSetupInput("3"),
		webEnabled:    true,
		webPort:       newSetupInput("8080"),
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

func TestSetupModel_PipelineChoice(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{name: "the first choice needs no navigation", want: "default"},
		{name: "j selects the multi-stage pipeline", keys: []string{"j"}, want: "implement-review-commit"},
		{name: "the last choice runs no pipeline", keys: []string{"j", "j"}, want: ""},
		{name: "the cursor stops at the last choice", keys: []string{"j", "j", "j"}, want: ""},
		{name: "the cursor stops at the first choice", keys: []string{"k"}, want: "default"},
		{name: "k walks back up", keys: []string{"j", "j", "k"}, want: "implement-review-commit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := setupModel{step: stepPipelines, selectedAgents: []string{"claude"}, argsInputs: map[string]textinput.Model{"claude": newSetupInput("")}}
			for _, k := range tc.keys {
				m = updateSetup(m, k)
			}

			view := m.View()
			assert.Contains(t, view, "Which one do new tickets run?")
			// Every choice carries its own lines, so adding one to the table is
			// all it takes to list it.
			for _, p := range setupPipelines {
				if p.name != "" {
					assert.Contains(t, view, p.name)
				}
				for _, line := range p.desc {
					assert.Contains(t, view, line)
				}
			}

			m = updateSetup(m, "enter")
			assert.Equal(t, stepConfirm, m.step)
			assert.Equal(t, tc.want, m.answers().DefaultPipeline)
		})
	}
}

func TestSetupModel_Confirm(t *testing.T) {
	m := setupModel{step: stepConfirm, selectedAgents: []string{"claude"}, argsInputs: map[string]textinput.Model{"claude": newSetupInput("")}, dirFields: newSetupInputs("a", "b", "c"), dirLabels: [3]string{"d", "e", "f"}, maxConcurrent: newSetupInput("3"), webPort: newSetupInput("8080"), webEnabled: true}

	// View should contain summary
	view := m.View()
	assert.Contains(t, view, "Summary")
	assert.Contains(t, view, "claude")

	// 'y' confirms
	m = updateSetup(m, "y")
	assert.True(t, m.done)

	// 'n' cancels
	m2 := setupModel{step: stepConfirm, selectedAgents: []string{"claude"}, argsInputs: map[string]textinput.Model{"claude": newSetupInput("")}, dirFields: newSetupInputs("a", "b", "c"), dirLabels: [3]string{"d", "e", "f"}, maxConcurrent: newSetupInput("3"), webPort: newSetupInput("8080")}
	m2 = updateSetup(m2, "n")
	assert.True(t, m2.cancelled)
}

func TestSetupModel_CtrlCCancels(t *testing.T) {
	// A focused text field must not swallow it either.
	steps := []int{stepAgents, stepArgs, stepDirs, stepSettings, stepPipelines, stepConfirm}

	for _, step := range steps {
		t.Run(fmt.Sprintf("step %d", step), func(t *testing.T) {
			m := initialSetupModel()
			m.step = step
			m.selectedAgents = []string{"claude"}
			m.argsInputs["claude"] = newSetupInput("")
			m.argsEditing = step == stepArgs
			m.dirEditing = step == stepDirs
			m.settingEditing = step == stepSettings

			m = updateSetup(m, "ctrl+c")

			assert.True(t, m.cancelled)
			assert.False(t, m.done, "a cancelled wizard writes no config")
		})
	}
}

func TestSetupModel_Answers(t *testing.T) {
	m := setupModel{
		selectedAgents: []string{"claude"},
		argsInputs:     map[string]textinput.Model{"claude": newSetupInput("--dangerously-skip-permissions")},
		dirFields:      newSetupInputs("~/.kontora/tickets", "~/.kontora/logs", "~/.kontora/worktrees"),
		maxConcurrent:  newSetupInput("5"),
		webEnabled:     true,
		webPort:        newSetupInput("9090"),
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
				argsInputs:     map[string]textinput.Model{"claude": newSetupInput("")},
				maxConcurrent:  newSetupInput("3"),
				webPort:        newSetupInput(tc.port),
			}
			ans := m.answers()
			assert.Equal(t, tc.wantPort, ans.WebPort)
		})
	}
}
