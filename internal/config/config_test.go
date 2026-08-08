package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
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

	// Stages: 4 user-defined plus the built-in "rework" stage.
	assert.Len(t, cfg.Stages, 5)
	plan := cfg.Stages["plan"]
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
	require.NotNil(t, cfg.AutoPickUp)
	assert.True(t, *cfg.AutoPickUp, "auto_pick_up should default to true")
}

func TestApplyServerEnvOverridesSetsWebToken(t *testing.T) {
	t.Setenv(ServerTokenEnvVar, "secret-from-env")

	cfg := &Config{}
	cfg.ApplyServerEnvOverrides()

	assert.Equal(t, "secret-from-env", cfg.Web.Token)
}

func TestApplyServerEnvOverridesBlankKeepsConfigToken(t *testing.T) {
	t.Setenv(ServerTokenEnvVar, "   ")

	cfg := &Config{Web: Web{Token: "from-file"}}
	cfg.ApplyServerEnvOverrides()

	assert.Equal(t, "from-file", cfg.Web.Token)
}

func TestAutoPickUpExplicitFalse(t *testing.T) {
	input := `
auto_pick_up: false
agents:
  claude:
    binary: claude
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
      agent: claude
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, cfg.AutoPickUp)
	assert.False(t, *cfg.AutoPickUp)
}

func TestInstanceNameDefault(t *testing.T) {
	orig := osHostname
	t.Cleanup(func() { osHostname = orig })

	tests := []struct {
		name     string
		explicit string
		hostname func() (string, error)
		want     string
	}{
		{
			name:     "defaults to hostname",
			hostname: func() (string, error) { return "alpha", nil },
			want:     "alpha",
		},
		{
			name:     "explicit value preserved",
			explicit: "alpha",
			hostname: func() (string, error) { return "beta", nil },
			want:     "alpha",
		},
		{
			name:     "hostname lookup fails",
			hostname: func() (string, error) { return "", errors.New("no hostname") },
			want:     "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			osHostname = tt.hostname
			cfg := &Config{InstanceName: tt.explicit}
			cfg.applyDefaults()
			assert.Equal(t, tt.want, cfg.InstanceName)
		})
	}
}

func TestLoadUnknownStage(t *testing.T) {
	_, err := Load("testdata/unknown_stage.yaml")
	require.ErrorContains(t, err, "unknown stage")
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

func TestDefaultFailurePatternsApplied(t *testing.T) {
	base := `
agents:
  claude:
    binary: claude%s
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
      agent: claude
      on_success: done
      on_failure: pause
`
	tests := []struct {
		name      string
		agentYAML string
		want      []string
	}{
		{
			name:      "unset gets defaults",
			agentYAML: "",
			want:      DefaultFailurePatterns,
		},
		{
			name:      "explicit list overrides defaults",
			agentYAML: "\n    failure_patterns:\n      - custom",
			want:      []string{"custom"},
		},
		{
			name:      "empty list disables detection",
			agentYAML: "\n    failure_patterns: []",
			want:      []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadReader(strings.NewReader(fmt.Sprintf(base, tt.agentYAML)))
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Agents["claude"].FailurePatterns)
		})
	}
}

func TestLoadResumeSettings(t *testing.T) {
	base := `%s
agents:
  claude:
    binary: claude%s
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
      agent: claude
      on_success: done
      on_failure: pause
`
	tests := []struct {
		name       string
		topYAML    string
		agentYAML  string
		wantResume *bool
		wantPrompt string
	}{
		{name: "both unset"},
		{
			name:       "resume disabled for the agent",
			agentYAML:  "\n    resume: false",
			wantResume: new(false),
		},
		{
			name:       "resume enabled for the agent",
			agentYAML:  "\n    resume: true",
			wantResume: new(true),
		},
		{
			name:       "custom resume prompt",
			topYAML:    "resume_prompt: Continue the interrupted work",
			wantPrompt: "Continue the interrupted work",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadReader(strings.NewReader(fmt.Sprintf(base, tt.topYAML, tt.agentYAML)))
			require.NoError(t, err)
			assert.Equal(t, tt.wantResume, cfg.Agents["claude"].Resume)
			assert.Equal(t, tt.wantPrompt, cfg.ResumePrompt)
		})
	}
}

func TestDefaultFailurePatternsCompile(t *testing.T) {
	for _, p := range DefaultFailurePatterns {
		_, err := regexp.Compile(p)
		assert.NoErrorf(t, err, "default pattern %q must compile", p)
	}
}

func TestLoadFailurePatterns(t *testing.T) {
	tmpl := `
agents:
  claude:
    binary: claude
    failure_patterns:
      - %q
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
      agent: claude
      on_success: done
      on_failure: pause
`
	tests := []struct {
		name    string
		pattern string
		wantErr string
	}{
		{name: "valid regex", pattern: `usage limit reached`},
		{name: "valid anchored regex", pattern: `^API Error:`},
		{name: "invalid regex", pattern: `[unterminated`, wantErr: "invalid failure_pattern"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := fmt.Sprintf(tmpl, tt.pattern)
			cfg, err := LoadReader(strings.NewReader(input))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{tt.pattern}, cfg.Agents["claude"].FailurePatterns)
		})
	}
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

func TestAgentKindDetection(t *testing.T) {
	tests := []struct {
		name       string
		agent      Agent
		wantPi     bool
		wantClaude bool
	}{
		{name: "bare pi", agent: Agent{Binary: "pi"}, wantPi: true},
		{name: "bare claude", agent: Agent{Binary: "claude"}, wantClaude: true},
		{name: "absolute path pi", agent: Agent{Binary: "/opt/homebrew/bin/pi"}, wantPi: true},
		{name: "absolute path claude", agent: Agent{Binary: "/opt/homebrew/bin/claude"}, wantClaude: true},
		{name: "other binary", agent: Agent{Binary: "programmator", Args: []string{"start"}}},
		{
			name:   "nono wrapped pi",
			agent:  Agent{Binary: "nono", Args: []string{"run", "-s", "--profile", "pi", "--", "pi", "--model", "acme-gateway/claude-fable-5"}},
			wantPi: true,
		},
		{
			name:       "nono wrapped claude",
			agent:      Agent{Binary: "nono", Args: []string{"run", "--profile", "claude", "--", "claude", "--dangerously-skip-permissions"}},
			wantClaude: true,
		},
		{
			name:   "op wrapped pi",
			agent:  Agent{Binary: "op", Args: []string{"run", "--", "pi"}},
			wantPi: true,
		},
		{
			name:   "nono wrapped pi with path",
			agent:  Agent{Binary: "nono", Args: []string{"run", "--", "/opt/homebrew/bin/pi"}},
			wantPi: true,
		},
		{name: "nono without separator", agent: Agent{Binary: "nono", Args: []string{"run", "pi"}}},
		{name: "nono with trailing separator", agent: Agent{Binary: "nono", Args: []string{"run", "--"}}},
		{name: "pi flag value is not the wrapped binary", agent: Agent{Binary: "nono", Args: []string{"run", "--profile", "pi", "--", "claude"}}, wantClaude: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantPi, tt.agent.IsPi())
			assert.Equal(t, tt.wantClaude, tt.agent.IsClaude())
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
	require.ErrorContains(t, err, "last stage must not have on_success=next")
}

func TestLoadDuplicateStageInPipeline(t *testing.T) {
	_, err := Load("testdata/duplicate_stage.yaml")
	require.ErrorContains(t, err, "duplicate stage")
}

func TestLoadUnknownDefaultAgent(t *testing.T) {
	input := `
tickets_dir: /tmp/tasks
default_agent: nonexistent
agents:
  a:
    binary: agent-bin
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
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
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
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
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
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

func TestLoadMinimalDefaultsTmuxSession(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	require.NoError(t, err)
	assert.Equal(t, "kontora", cfg.TmuxSession)
	assert.Equal(t, "kontora", cfg.TmuxSessionName())

	// Configs built in memory skip applyDefaults, so TmuxSessionName has to
	// carry the fallback as well. The daemon, CLI, TUI, and web all read it.
	assert.Equal(t, "kontora", (&Config{}).TmuxSessionName())
}

// A tmux session name reaches both a JSON string literal and a shell command
// through the wait-for channel the daemon writes into the agent's hook
// settings, so anything outside [A-Za-z0-9_-] has to be rejected at load. A
// leading "-" goes too: it makes the channel name look like a tmux flag.
func TestValidateTmuxSession(t *testing.T) {
	cases := []struct {
		name    string
		session string
		wantErr bool
	}{
		{name: "default", session: "kontora"},
		{name: "hyphen", session: "kontora-work"},
		{name: "underscore and digit", session: "k_1"},
		{name: "64 characters", session: strings.Repeat("a", 64)},
		{name: "empty", session: "", wantErr: true},
		{name: "leading hyphen", session: "-x", wantErr: true},
		{name: "dot", session: "a.b", wantErr: true},
		{name: "colon", session: "a:b", wantErr: true},
		{name: "space", session: "a b", wantErr: true},
		{name: "double quote", session: `a"b`, wantErr: true},
		{name: "backtick", session: "a`b", wantErr: true},
		{name: "command substitution", session: "$(x)", wantErr: true},
		{name: "65 characters", session: strings.Repeat("a", 65), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				DefaultAgent: "a",
				TmuxSession:  tc.session,
				Agents:       map[string]Agent{"a": {Binary: "agent-bin"}},
			}
			err := cfg.Validate()
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "tmux_session")
			assert.Contains(t, err.Error(), "[A-Za-z0-9_-]")
		})
	}
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
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
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
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
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

func TestLoadCustomStatuses(t *testing.T) {
	cfg, err := Load("testdata/custom_statuses.yaml")
	require.NoError(t, err)
	assert.Equal(t, []string{"review", "qa"}, cfg.Statuses)
	assert.True(t, cfg.IsCustomStatus("review"))
	assert.True(t, cfg.IsCustomStatus("qa"))
	assert.False(t, cfg.IsCustomStatus("done"))
}

func TestHumanReviewFirstClass(t *testing.T) {
	// human_review works in on_success/on_failure without being declared
	// under statuses, and IsCustomStatus returns false for it (it's built-in).
	input := `
agents:
  a:
    binary: bin
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: human_review
      on_failure: human_review
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.False(t, cfg.IsCustomStatus("human_review"))
	assert.Empty(t, cfg.Statuses)
}

func TestHumanReviewDeclaredStatusDropped(t *testing.T) {
	// Old configs listing human_review under statuses should keep loading.
	input := `
agents:
  a:
    binary: bin
statuses: [human_review, review]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: human_review
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, []string{"review"}, cfg.Statuses)
}

func TestCustomStatusClashBuiltin(t *testing.T) {
	input := `
agents:
  a:
    binary: bin
statuses: [done]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: done
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, "clashes with built-in status")
}

func TestCustomStatusClashArchived(t *testing.T) {
	input := `
agents:
  a:
    binary: bin
statuses: [archived]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: done
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, "clashes with built-in status")
}

func TestCustomStatusClashReserved(t *testing.T) {
	input := `
agents:
  a:
    binary: bin
statuses: [next]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: done
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, "clashes with reserved keyword")
}

func TestCustomStatusDuplicate(t *testing.T) {
	input := `
agents:
  a:
    binary: bin
statuses: [review, review]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: done
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, "duplicate")
}

func TestCustomStatusInvalidFormat(t *testing.T) {
	input := `
agents:
  a:
    binary: bin
statuses: [Review]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: done
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.ErrorContains(t, err, "must match")
}

func TestCustomStatusOnSuccessReference(t *testing.T) {
	input := `
agents:
  a:
    binary: bin
statuses: [review]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: review
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "review", cfg.Pipelines["p"][0].OnSuccess)
}

func TestCustomStatusOnFailureReference(t *testing.T) {
	input := `
agents:
  a:
    binary: bin
statuses: [needs_fix]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: done
      on_failure: needs_fix
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "needs_fix", cfg.Pipelines["p"][0].OnFailure)
}

func TestCustomStatusLastStageNotNext(t *testing.T) {
	input := `
agents:
  a:
    binary: bin
statuses: [review]
stages:
  s:
    prompt: x
pipelines:
  p:
    - stage: s
      agent: a
      on_success: review
      on_failure: pause
`
	_, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
}

func TestPlannotatorDefaults(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	require.NoError(t, err)

	assert.Equal(t, "plannotator", cfg.Plannotator.Binary)
	assert.Equal(t, 30*time.Minute, cfg.Plannotator.Timeout.Duration)
	assert.Equal(t, "~/.kontora/plannotator-reviews", cfg.Plannotator.ReviewsDir)
}

func TestPlannotatorOverride(t *testing.T) {
	input := `
plannotator:
  binary: /usr/local/bin/plannotator
  timeout: 10m
  reviews_dir: /tmp/reviews
agents:
  claude:
    binary: claude
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
      agent: claude
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "/usr/local/bin/plannotator", cfg.Plannotator.Binary)
	assert.Equal(t, 10*time.Minute, cfg.Plannotator.Timeout.Duration)
	assert.Equal(t, "/tmp/reviews", cfg.Plannotator.ReviewsDir)
}

func TestDefaultReworkStageMerged(t *testing.T) {
	cfg, err := Load("testdata/minimal.yaml")
	require.NoError(t, err)

	rework, ok := cfg.Stages["rework"]
	require.True(t, ok, "built-in rework stage should be merged when absent")
	assert.Contains(t, rework.Prompt, "plannotatorReview")
	assert.Equal(t, 30*time.Minute, rework.Timeout.Duration)
	assert.True(t, cfg.ReworkIsBuiltin)
}

func TestUserReworkStageWins(t *testing.T) {
	input := `
agents:
  claude:
    binary: claude
stages:
  s:
    prompt: do stuff
  rework:
    prompt: "custom rework prompt"
    timeout: 5m
pipelines:
  p:
    - stage: s
      agent: claude
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "custom rework prompt", cfg.Stages["rework"].Prompt)
	assert.Equal(t, 5*time.Minute, cfg.Stages["rework"].Timeout.Duration)
	assert.False(t, cfg.ReworkIsBuiltin, "user-provided rework should not be marked as built-in")
}

func TestLoadReaderValid(t *testing.T) {
	input := `
tickets_dir: /tmp/tasks
default_agent: a
agents:
  a:
    binary: agent-bin
stages:
  s:
    prompt: do stuff
pipelines:
  p:
    - stage: s
      agent: a
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "/tmp/tasks", cfg.TicketsDir)
}

func TestLoadProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load("testdata/projects.yaml")
	require.NoError(t, err)

	require.Len(t, cfg.Projects, 2)
	assert.Equal(t, Project{Path: "~/projects/widget-api"}, cfg.Projects["widget-api"])

	name, p, ok := cfg.ProjectFor(filepath.Join(home, "projects", "kontora"))
	require.True(t, ok)
	assert.Equal(t, "kontora", name)
	assert.Equal(t, Project{Path: "~/projects/kontora", Pipeline: "implement", Agent: "claude", BranchPrefix: "feature"}, p)
}

func TestLoadProjectsRejected(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		wantErr []string
	}{
		{
			name:    "missing path",
			fixture: "project_missing_path.yaml",
			wantErr: []string{`project "kontora"`, "path is required"},
		},
		{
			name:    "unknown pipeline",
			fixture: "project_unknown_pipeline.yaml",
			wantErr: []string{`project "kontora"`, `unknown pipeline "missing-pipeline"`},
		},
		{
			name:    "unknown agent",
			fixture: "project_unknown_agent.yaml",
			wantErr: []string{`project "kontora"`, `unknown agent "missing-agent"`},
		},
		{
			name:    "duplicate normalized path",
			fixture: "project_duplicate_path.yaml",
			wantErr: []string{`project "second"`, "projects/foo", `duplicates project "first"`},
		},
		{
			name:    "reserved pipeline name",
			fixture: "reserved_pipeline_name.yaml",
			wantErr: []string{`pipeline "none"`, "reserved"},
		},
		{
			name:    "reserved agent name",
			fixture: "reserved_agent_name.yaml",
			wantErr: []string{`agent "none"`, "reserved"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			_, err := Load(filepath.Join("testdata", tc.fixture))
			require.Error(t, err)
			for _, want := range tc.wantErr {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestProjectFor(t *testing.T) {
	home := t.TempDir()
	abs := filepath.Join(home, "projects", "kontora")

	configured := &Config{Projects: map[string]Project{
		"kontora": {Path: "~/projects/kontora", Pipeline: "implement", Agent: "claude"},
	}}

	cases := []struct {
		name     string
		cfg      *Config
		lookup   string
		wantName string
	}{
		{name: "tilde form", cfg: configured, lookup: "~/projects/kontora", wantName: "kontora"},
		{name: "absolute form", cfg: configured, lookup: abs, wantName: "kontora"},
		{name: "trailing slash", cfg: configured, lookup: abs + "/", wantName: "kontora"},
		{name: "subdirectory does not match", cfg: configured, lookup: filepath.Join(abs, "internal")},
		{name: "parent does not match", cfg: configured, lookup: filepath.Dir(abs)},
		{name: "empty lookup", cfg: configured, lookup: ""},
		{name: "no projects configured", cfg: &Config{}, lookup: abs},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", home)

			name, p, ok := tc.cfg.ProjectFor(tc.lookup)
			if tc.wantName == "" {
				assert.False(t, ok)
				assert.Empty(t, name)
				assert.Equal(t, Project{}, p)
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.wantName, name)
			assert.Equal(t, "implement", p.Pipeline)
			assert.Equal(t, "claude", p.Agent)
		})
	}
}

func TestBranchPrefixFor(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		BranchPrefix: "kontora",
		Projects: map[string]Project{
			"kontora": {Path: "~/projects/kontora", BranchPrefix: "feature"},
			"widget-api":   {Path: "~/projects/widget-api"},
		},
	}

	cases := []struct {
		name   string
		lookup string
		want   string
	}{
		{name: "project prefix", lookup: "~/projects/kontora", want: "feature"},
		{name: "project prefix by absolute path", lookup: filepath.Join(home, "projects", "kontora"), want: "feature"},
		{name: "project without a prefix falls back", lookup: "~/projects/widget-api", want: "kontora"},
		{name: "unconfigured path falls back", lookup: "~/projects/other", want: "kontora"},
		{name: "no path falls back", lookup: "", want: "kontora"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", home)

			assert.Equal(t, tc.want, cfg.BranchPrefixFor(tc.lookup))
		})
	}
}
