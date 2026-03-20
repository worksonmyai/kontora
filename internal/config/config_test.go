package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLoadValid(t *testing.T) {
	cfg, err := Load("testdata/valid.yaml")
	require.NoError(t, err)

	assert.Equal(t, "~/org/tickets", cfg.TicketsDir)
	assert.Equal(t, "~/kontora/worktrees", cfg.WorktreesDir)
	assert.Equal(t, 3, cfg.MaxConcurrentAgents)

	// Branch prefix
	assert.Equal(t, "myprefix", cfg.BranchPrefix)

	// Environment
	assert.Equal(t, map[string]string{
		"CLAUDE_CODE_MAX_TURNS": "50",
		"MY_CUSTOM_VAR":         "hello",
	}, cfg.Environment)

	// Agents
	assert.Len(t, cfg.Agents, 2)
	sonnet := cfg.Agents["claude-sonnet"]
	assert.Equal(t, "claude", sonnet.Binary)

	// Roles
	assert.Len(t, cfg.Roles, 4)
	plan := cfg.Roles["plan"]
	assert.Equal(t, 10*time.Minute, plan.Timeout.Duration)

	// Pipelines
	pipeline := cfg.Pipelines["default"]
	assert.Len(t, pipeline, 4)
	assert.Equal(t, 2, pipeline[1].MaxRetries)
}

func TestLoadMinimalDefaults(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	require.NoError(t, err)

	assert.Equal(t, "~/.kontora/worktrees", cfg.WorktreesDir)
	assert.Equal(t, 3, cfg.MaxConcurrentAgents)
}

func TestLoadUnknownRole(t *testing.T) {
	_, err := Load("testdata/unknown_role.yaml")
	require.ErrorContains(t, err, "unknown role")
}

func TestLoadUnknownAgent(t *testing.T) {
	_, err := Load("testdata/unknown_agent.yaml")
	require.ErrorContains(t, err, "unknown agent")
}

func TestLoadBackOnFirstStage(t *testing.T) {
	_, err := Load("testdata/back_on_first.yaml")
	require.ErrorContains(t, err, "back")
}

func TestLoadInvalidOnSuccess(t *testing.T) {
	_, err := Load("testdata/invalid_on_success.yaml")
	require.ErrorContains(t, err, "invalid on_success")
}

func TestLoadInvalidOnFailure(t *testing.T) {
	_, err := Load("testdata/invalid_on_failure.yaml")
	require.ErrorContains(t, err, "invalid on_failure")
}

func TestLoadMissingTicketsDir(t *testing.T) {
	cfg, err := Load("testdata/missing_tickets_dir.yaml")
	require.NoError(t, err)
	assert.Equal(t, "~/.kontora/tickets", cfg.TicketsDir)
}

func TestLoadMissingAgentBinary(t *testing.T) {
	_, err := Load("testdata/missing_agent_binary.yaml")
	require.ErrorContains(t, err, "binary")
}

func TestDurationParsing(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{name: "minutes", input: "10m", want: 10 * time.Minute},
		{name: "seconds", input: "30s", want: 30 * time.Second},
		{name: "mixed", input: "1h30m", want: 90 * time.Minute},
		{name: "invalid", input: "not-a-duration", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d Duration
			node := &yaml.Node{Kind: yaml.ScalarNode, Value: tt.input}
			err := d.UnmarshalYAML(node)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, d.Duration)
		})
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("testdata/does_not_exist.yaml")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "does_not_exist.yaml")
}

func TestLoadMalformedYAML(t *testing.T) {
	_, err := Load("testdata/malformed.yaml")
	require.Error(t, err)
}

func TestLoadLastStageNotDone(t *testing.T) {
	_, err := Load("testdata/last_stage_next.yaml")
	require.ErrorContains(t, err, "last stage must have on_success=done")
}

func TestLoadDuplicateRoleInPipeline(t *testing.T) {
	_, err := Load("testdata/duplicate_role.yaml")
	require.ErrorContains(t, err, "duplicate role")
}

func TestLoadUnknownDefaultAgent(t *testing.T) {
	input := `
tickets_dir: /tmp/tasks
default_agent: nonexistent
agents:
  a:
    binary: agent-bin
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: a
      on_success: done
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, `default_agent "nonexistent": not found in agents`)
}

func TestDefaultAgentDefault(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	require.NoError(t, err)
	assert.Equal(t, "claude", cfg.DefaultAgent)
}

func TestDefaultAgentSingleInference(t *testing.T) {
	input := `
agents:
  my-agent:
    binary: my-agent-bin
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: my-agent
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "my-agent", cfg.DefaultAgent)
}

func TestDefaultAgentMultipleNoClaudeError(t *testing.T) {
	input := `
agents:
  agent-a:
    binary: a-bin
  agent-b:
    binary: b-bin
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: agent-a
      on_success: done
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, "could not infer")
}

func TestLoadMinimalDefaultsBranchPrefix(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	require.NoError(t, err)
	assert.Equal(t, "kontora", cfg.BranchPrefix)
}

func TestLoadWebConfig(t *testing.T) {
	input := `
tickets_dir: /tmp/tasks
default_agent: a
web:
  enabled: true
  host: 0.0.0.0
  port: 9090
agents:
  a:
    binary: agent-bin
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: a
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, cfg.Web.Enabled)
	assert.True(t, *cfg.Web.Enabled)
	assert.Equal(t, "0.0.0.0", cfg.Web.Host)
	assert.Equal(t, 9090, cfg.Web.Port)
}

func TestLoadWebConfigDefaults(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	require.NoError(t, err)
	require.NotNil(t, cfg.Web.Enabled)
	assert.True(t, *cfg.Web.Enabled)
	assert.Equal(t, "127.0.0.1", cfg.Web.Host)
	assert.Equal(t, 8080, cfg.Web.Port)
}

func TestAgentEnvironment(t *testing.T) {
	input := `
agents:
  claude:
    binary: claude
    environment:
      CLAUDE_CONFIG_DIR: /custom/config
      MY_VAR: hello
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: claude
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"CLAUDE_CONFIG_DIR": "/custom/config",
		"MY_VAR":            "hello",
	}, cfg.Agents["claude"].Environment)
}

func TestSummarizerConfig(t *testing.T) {
	input := `
agents:
  claude:
    binary: claude
summarizer:
  binary: claude
  args: ["-m", "haiku", "-p"]
  prompt: "Summarize ticket {{.TicketID}}"
  timeout: 45s
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: claude
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, cfg.Summarizer)
	assert.Equal(t, "claude", cfg.Summarizer.Binary)
	assert.Equal(t, []string{"-m", "haiku", "-p"}, cfg.Summarizer.Args)
	assert.Equal(t, "Summarize ticket {{.TicketID}}", cfg.Summarizer.Prompt)
	assert.Equal(t, 45*time.Second, cfg.Summarizer.Timeout.Duration)
}

func TestSummarizerConfig_MissingBinary(t *testing.T) {
	input := `
agents:
  claude:
    binary: claude
summarizer:
  args: ["-m", "haiku"]
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: claude
      on_success: done
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, "summarizer: binary is required")
}

func TestSummarizerConfig_DefaultTimeout(t *testing.T) {
	input := `
agents:
  claude:
    binary: claude
summarizer:
  binary: my-summarizer
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: claude
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, cfg.Summarizer)
	assert.Equal(t, 30*time.Second, cfg.Summarizer.Timeout.Duration)
}

func TestSummarizerConfig_Nil(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	require.NoError(t, err)
	assert.Nil(t, cfg.Summarizer)
}

func TestLoadReaderValid(t *testing.T) {
	input := `
tickets_dir: /tmp/tasks
default_agent: a
agents:
  a:
    binary: agent-bin
roles:
  s:
    prompt: do stuff
pipelines:
  p:
    - role: s
      agent: a
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "/tmp/tasks", cfg.TicketsDir)
}
