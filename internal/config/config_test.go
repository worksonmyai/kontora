package config

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
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
	assert.Equal(t, BranchNaming{Mode: BranchNamingModeSlug}, cfg.BranchNaming)
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
		name           string
		topYAML        string
		agentYAML      string
		wantResume     *bool
		wantPrompt     string
		wantAnnotation string
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
		{
			name:           "custom annotation prompt",
			topYAML:        "annotation_prompt: Rewrite the ticket",
			wantAnnotation: "Rewrite the ticket",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadReader(strings.NewReader(fmt.Sprintf(base, tt.topYAML, tt.agentYAML)))
			require.NoError(t, err)
			assert.Equal(t, tt.wantResume, cfg.Agents["claude"].Resume)
			assert.Equal(t, tt.wantPrompt, cfg.ResumePrompt)
			assert.Equal(t, tt.wantAnnotation, cfg.AnnotationPrompt)
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
		wantKind   string
	}{
		{name: "bare pi", agent: Agent{Binary: "pi"}, wantPi: true, wantKind: AgentKindPi},
		{name: "bare claude", agent: Agent{Binary: "claude"}, wantClaude: true, wantKind: AgentKindClaude},
		{name: "absolute path pi", agent: Agent{Binary: "/opt/homebrew/bin/pi"}, wantPi: true, wantKind: AgentKindPi},
		{name: "absolute path claude", agent: Agent{Binary: "/opt/homebrew/bin/claude"}, wantClaude: true, wantKind: AgentKindClaude},
		{name: "other binary", agent: Agent{Binary: "programmator", Args: []string{"start"}}},
		{
			name:     "nono wrapped pi",
			agent:    Agent{Binary: "nono", Args: []string{"run", "-s", "--profile", "pi", "--", "pi", "--model", "grafana-retention/claude-fable-5"}},
			wantPi:   true,
			wantKind: AgentKindPi,
		},
		{
			name:       "nono wrapped claude",
			agent:      Agent{Binary: "nono", Args: []string{"run", "--profile", "claude", "--", "claude", "--dangerously-skip-permissions"}},
			wantClaude: true,
			wantKind:   AgentKindClaude,
		},
		{
			name:     "op wrapped pi",
			agent:    Agent{Binary: "op", Args: []string{"run", "--", "pi"}},
			wantPi:   true,
			wantKind: AgentKindPi,
		},
		{
			name:     "nono wrapped pi with path",
			agent:    Agent{Binary: "nono", Args: []string{"run", "--", "/opt/homebrew/bin/pi"}},
			wantPi:   true,
			wantKind: AgentKindPi,
		},
		{name: "nono without separator", agent: Agent{Binary: "nono", Args: []string{"run", "pi"}}},
		{name: "nono with trailing separator", agent: Agent{Binary: "nono", Args: []string{"run", "--"}}},
		{name: "pi flag value is not the wrapped binary", agent: Agent{Binary: "nono", Args: []string{"run", "--profile", "pi", "--", "claude"}}, wantClaude: true, wantKind: AgentKindClaude},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantPi, tt.agent.IsPi())
			assert.Equal(t, tt.wantClaude, tt.agent.IsClaude())
			assert.Equal(t, tt.wantKind, tt.agent.Kind())
		})
	}
}

func TestModelUnmarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Model
		wantErr string
	}{
		{name: "scalar", input: "haiku", want: Model{Any: "haiku"}},
		{name: "empty", input: "", want: Model{}},
		{
			name:  "mapping",
			input: "{claude: haiku, pi: anthropic/claude-haiku-4-5}",
			want:  Model{ByAgent: map[string]string{"claude": "haiku", "pi": "anthropic/claude-haiku-4-5"}},
		},
		{name: "sequence", input: "[haiku, sonnet]", wantErr: "invalid model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out struct {
				Model Model `yaml:"model"`
			}
			err := yaml.Unmarshal([]byte("model: "+tt.input+"\n"), &out)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Model)
		})
	}
}

// TestModelRoundTrip covers the `kontora config` output: it is re-encoded from
// the loaded config and must load again through the strict decoder.
func TestModelRoundTrip(t *testing.T) {
	const src = `tickets_dir: ~/org/tickets
summary_model:
  claude: haiku
agents:
  claude:
    binary: claude
stages:
  commit:
    prompt: Commit.
    model: haiku
  review:
    prompt: Review.
    model:
      claude: sonnet
  implement:
    prompt: Implement.
pipelines:
  default:
    - stage: commit
      agent: claude
      on_success: done
      on_failure: pause
`
	cfg, err := LoadReader(strings.NewReader(src))
	require.NoError(t, err)

	out, err := yaml.Marshal(cfg)
	require.NoError(t, err)

	again, err := LoadReader(bytes.NewReader(out))
	require.NoError(t, err)

	assert.Equal(t, Model{Any: "haiku"}, again.Stages["commit"].Model)
	assert.Equal(t, Model{ByAgent: map[string]string{"claude": "sonnet"}}, again.Stages["review"].Model)
	assert.Equal(t, Model{ByAgent: map[string]string{"claude": "haiku"}}, again.SummaryModel)
	// A stage with no model prints `model: null`, which the strict decoder has
	// to read back as no model at all: every stage in a real config has one.
	assert.Equal(t, Model{}, again.Stages["implement"].Model)
}

func TestModelFor(t *testing.T) {
	claudeAgent := Agent{Binary: "claude"}
	piAgent := Agent{Binary: "nono", Args: []string{"run", "--", "pi"}}
	otherAgent := Agent{Binary: "programmator"}

	tests := []struct {
		name      string
		model     Model
		agentName string
		agent     Agent
		want      string
	}{
		{name: "unset", agentName: "claude", agent: claudeAgent},
		{name: "scalar applies to every agent", model: Model{Any: "haiku"}, agentName: "pi-grafana-opus-5", agent: piAgent, want: "haiku"},
		{
			name:      "agent name",
			model:     Model{ByAgent: map[string]string{"pi-grafana-opus-5": "anthropic/claude-haiku-4-5"}},
			agentName: "pi-grafana-opus-5", agent: piAgent, want: "anthropic/claude-haiku-4-5",
		},
		{
			name:      "agent kind",
			model:     Model{ByAgent: map[string]string{"pi": "anthropic/claude-haiku-4-5"}},
			agentName: "pi-grafana-opus-5", agent: piAgent, want: "anthropic/claude-haiku-4-5",
		},
		{
			name:      "agent name beats the kind it collides with",
			model:     Model{ByAgent: map[string]string{"claude": "haiku", "pi": "anthropic/claude-haiku-4-5"}},
			agentName: "claude", agent: piAgent, want: "haiku",
		},
		{
			name:      "map without a matching key falls back to nothing",
			model:     Model{ByAgent: map[string]string{"claude": "haiku"}},
			agentName: "pi-grafana-opus-5", agent: piAgent,
		},
		{
			name:      "map and scalar together",
			model:     Model{Any: "haiku", ByAgent: map[string]string{"pi": "anthropic/claude-haiku-4-5"}},
			agentName: "claude", agent: claudeAgent, want: "haiku",
		},
		{
			name:      "an agent with no kind takes only its name or the scalar",
			model:     Model{ByAgent: map[string]string{"": "haiku"}},
			agentName: "programmator", agent: otherAgent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.model.For(tt.agentName, tt.agent))
		})
	}
}

func TestAgentArgsWithModel(t *testing.T) {
	tests := []struct {
		name  string
		agent Agent
		model string
		want  []string
	}{
		{
			name:  "no model leaves the arguments alone",
			agent: Agent{Binary: "pi", Args: []string{"--model", "anthropic/claude-opus-5"}},
			want:  []string{"--model", "anthropic/claude-opus-5"},
		},
		{
			name:  "plain binary without a configured model",
			agent: Agent{Binary: "claude", Args: []string{"--dangerously-skip-permissions"}},
			model: "haiku",
			want:  []string{"--dangerously-skip-permissions", "--model", "haiku"},
		},
		{
			name:  "replaces the configured model",
			agent: Agent{Binary: "pi", Args: []string{"--model", "anthropic/claude-opus-5", "--yolo"}},
			model: "anthropic/claude-haiku-4-5",
			want:  []string{"--yolo", "--model", "anthropic/claude-haiku-4-5"},
		},
		{
			name:  "replaces the joined form",
			agent: Agent{Binary: "claude", Args: []string{"--model=opus", "--verbose"}},
			model: "haiku",
			want:  []string{"--verbose", "--model", "haiku"},
		},
		{
			name:  "wrapper keeps its own arguments",
			agent: Agent{Binary: "nono", Args: []string{"run", "--profile", "agent", "--", "pi", "--model", "anthropic/claude-opus-5"}},
			model: "anthropic/claude-haiku-4-5",
			want:  []string{"run", "--profile", "agent", "--", "pi", "--model", "anthropic/claude-haiku-4-5"},
		},
		{
			name:  "a cycling list is left alone",
			agent: Agent{Binary: "pi", Args: []string{"--models", "sonnet,haiku"}},
			model: "haiku",
			want:  []string{"--models", "sonnet,haiku", "--model", "haiku"},
		},
		{
			name:  "no arguments at all",
			agent: Agent{Binary: "claude"},
			model: "haiku",
			want:  []string{"--model", "haiku"},
		},
		{
			name:  "a trailing model flag takes no value with it",
			agent: Agent{Binary: "claude", Args: []string{"--verbose", "--model"}},
			model: "haiku",
			want:  []string{"--verbose", "--model", "haiku"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configured := slices.Clone(tt.agent.Args)
			got := tt.agent.ArgsWithModel(tt.model)
			assert.Equal(t, tt.want, got)
			// A shared backing array would let one stage's override rewrite the
			// arguments every later run of that agent is spawned with.
			assert.Equal(t, configured, tt.agent.Args, "configured args must not be mutated")
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

func TestLoadBranchNaming(t *testing.T) {
	cfg, err := Load("testdata/branch_naming.yaml")
	require.NoError(t, err)
	assert.Equal(t, BranchNaming{Mode: BranchNamingModeSlug}, cfg.BranchNaming)
	assert.Equal(t, BranchNaming{Mode: BranchNamingModeOff}, cfg.Projects["legacy"].BranchNaming)

	tests := []struct {
		name    string
		fixture string
		mode    string
	}{
		{name: "llm mode", fixture: "branch_naming_llm.yaml", mode: "llm"},
		{name: "unknown project mode", fixture: "project_branch_naming_unknown.yaml", mode: "automatic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", tt.fixture))
			require.Error(t, err)
			assert.ErrorContains(t, err, "branch_naming.mode")
			assert.ErrorContains(t, err, tt.mode)
		})
	}
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

func TestLoadMetrics(t *testing.T) {
	base := `
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
	tests := []struct {
		name    string
		metrics string
		wantErr string
		check   func(t *testing.T, m Metrics)
	}{
		{
			name: "absent section defaults to off with a 60s interval",
			check: func(t *testing.T, m Metrics) {
				require.NotNil(t, m.Enabled)
				assert.False(t, *m.Enabled)
				assert.Equal(t, 60*time.Second, m.Interval.Duration)
				assert.Empty(t, m.Endpoint)
			},
		},
		{
			name: "enabled without an endpoint is valid",
			metrics: `
metrics:
  enabled: true
`,
			check: func(t *testing.T, m Metrics) {
				require.NotNil(t, m.Enabled)
				assert.True(t, *m.Enabled)
				assert.Empty(t, m.Endpoint, "an empty endpoint leaves the address to OTEL_EXPORTER_OTLP_*")
				assert.Equal(t, 60*time.Second, m.Interval.Duration)
			},
		},
		{
			name: "full section round-trips",
			metrics: `
metrics:
  enabled: true
  endpoint: localhost:4318
  insecure: true
  interval: 10s
  headers:
    authorization: Bearer tok
`,
			check: func(t *testing.T, m Metrics) {
				assert.Equal(t, "localhost:4318", m.Endpoint)
				assert.Equal(t, 10*time.Second, m.Interval.Duration)
				assert.Equal(t, map[string]string{"authorization": "Bearer tok"}, m.Headers)
				insecure, conflict := m.ResolveInsecure()
				assert.True(t, insecure)
				assert.False(t, conflict)
			},
		},
		{
			name: "an http endpoint overrides insecure",
			metrics: `
metrics:
  enabled: true
  endpoint: http://collector:4318
`,
			check: func(t *testing.T, m Metrics) {
				assert.Equal(t, "http", m.EndpointScheme())
				insecure, conflict := m.ResolveInsecure()
				assert.True(t, insecure, "the scheme decides, not the unset insecure field")
				assert.False(t, conflict)
			},
		},
		{
			name: "an https endpoint with insecure true conflicts",
			metrics: `
metrics:
  enabled: true
  endpoint: https://collector:4318
  insecure: true
`,
			check: func(t *testing.T, m Metrics) {
				insecure, conflict := m.ResolveInsecure()
				assert.False(t, insecure, "the scheme wins")
				assert.True(t, conflict)
			},
		},
		{
			name: "a zero interval reads as unset and takes the default",
			metrics: `
metrics:
  enabled: true
  interval: 0s
`,
			check: func(t *testing.T, m Metrics) {
				assert.Equal(t, 60*time.Second, m.Interval.Duration)
			},
		},
		{
			name: "a negative interval is rejected when enabled",
			metrics: `
metrics:
  enabled: true
  interval: -5s
`,
			wantErr: "metrics.interval",
		},
		{
			name: "a non-positive interval is ignored while disabled",
			metrics: `
metrics:
  enabled: false
  interval: -5s
`,
			check: func(t *testing.T, m Metrics) {
				assert.Equal(t, -5*time.Second, m.Interval.Duration)
			},
		},
		{
			name: "an unsupported endpoint scheme is rejected",
			metrics: `
metrics:
  enabled: true
  endpoint: grpc://collector:4317
`,
			wantErr: `scheme "grpc" is not supported`,
		},
		{
			name: "an unknown metrics field is rejected",
			metrics: `
metrics:
  enabled: true
  compression: gzip
`,
			wantErr: "field compression not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadReader(strings.NewReader(base + tt.metrics))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			tt.check(t, cfg.Metrics)
		})
	}
}

// TestValidateMetricsInterval covers the zero interval a YAML load can never
// produce, because applyDefaults fills it in. A config assembled in memory
// (the daemon's own tests, the TUI) skips defaults and reaches Validate raw.
func TestValidateMetricsInterval(t *testing.T) {
	cfg := &Config{
		DefaultAgent: "a",
		TmuxSession:  "kontora",
		BranchNaming: BranchNaming{Mode: BranchNamingModeSlug},
		Agents:       map[string]Agent{"a": {Binary: "agent-bin"}},
		Metrics:      Metrics{Enabled: new(true)},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metrics.interval")
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
	assert.Equal(t, Project{Path: "~/projects/sigil"}, cfg.Projects["sigil"])

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
		{
			name:    "stage model is a sequence",
			fixture: "stage_model_sequence.yaml",
			wantErr: []string{"invalid model", "a pattern or a map of agent name or agent kind to a pattern"},
		},
		{
			name:    "stage model names an unknown agent",
			fixture: "stage_model_unknown_agent.yaml",
			wantErr: []string{`stage "commit"`, `model "unknown-agent"`, "neither a configured agent nor an agent kind"},
		},
		{
			name:    "summary model names an unknown agent",
			fixture: "summary_model_unknown_agent.yaml",
			wantErr: []string{"summary_model", `model "unknown-agent"`, "neither a configured agent nor an agent kind"},
		},
		{
			name:    "stage model on an agent that takes no model flag",
			fixture: "stage_model_unsupported_agent.yaml",
			wantErr: []string{`stage "commit"`, `model "haiku"`, `agent "programmator"`},
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

func TestBranchNamingFor(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		BranchNaming: BranchNaming{Mode: BranchNamingModeSlug},
		Projects: map[string]Project{
			"api":    {Path: "~/projects/api", BranchNaming: BranchNaming{Mode: BranchNamingModeOff}},
			"worker": {Path: "~/projects/worker"},
		},
	}

	tests := []struct {
		name   string
		lookup string
		want   string
	}{
		{name: "project mode overrides global mode", lookup: "~/projects/api", want: BranchNamingModeOff},
		{name: "project mode by absolute path", lookup: filepath.Join(home, "projects", "api"), want: BranchNamingModeOff},
		{name: "project without mode uses global mode", lookup: "~/projects/worker", want: BranchNamingModeSlug},
		{name: "unmatched project uses global mode", lookup: "~/projects/other", want: BranchNamingModeSlug},
		{name: "empty path uses global mode", lookup: "", want: BranchNamingModeSlug},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			assert.Equal(t, BranchNaming{Mode: tt.want}, cfg.BranchNamingFor(tt.lookup))
		})
	}

	assert.Equal(t, BranchNaming{Mode: BranchNamingModeSlug}, (&Config{}).BranchNamingFor("/repos/api"))
}

func TestBranchPrefixFor(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		BranchPrefix: "kontora",
		Projects: map[string]Project{
			"kontora": {Path: "~/projects/kontora", BranchPrefix: "feature"},
			"sigil":   {Path: "~/projects/sigil"},
		},
	}

	cases := []struct {
		name   string
		lookup string
		want   string
	}{
		{name: "project prefix", lookup: "~/projects/kontora", want: "feature"},
		{name: "project prefix by absolute path", lookup: filepath.Join(home, "projects", "kontora"), want: "feature"},
		{name: "project without a prefix falls back", lookup: "~/projects/sigil", want: "kontora"},
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

func TestLoadHooks(t *testing.T) {
	cfg, err := Load("testdata/hooks_valid.yaml")
	require.NoError(t, err)

	global := cfg.Hooks[HookWorktreeCreated]
	require.Len(t, global, 1)
	assert.Equal(t, "copy claude settings", global[0].Name)
	assert.Equal(t, 5*time.Minute, global[0].Timeout.Duration)
	assert.Equal(t, HookOnFailurePause, global[0].OnFailure)

	project := cfg.Projects["kontora"].Hooks
	require.Len(t, project[HookWorktreeCreated], 1)
	assert.Equal(t, 30*time.Second, project[HookWorktreeCreated][0].Timeout.Duration)
	assert.Equal(t, HookOnFailurePause, project[HookWorktreeCreated][0].OnFailure)

	require.Len(t, project[HookStageEnd], 1)
	assert.Equal(t, 5*time.Minute, project[HookStageEnd][0].Timeout.Duration)
	assert.Equal(t, HookOnFailureWarn, project[HookStageEnd][0].OnFailure)
}

func TestLoadInvalidHooks(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		wantErr []string
	}{
		{
			name:    "unknown event",
			fixture: "hooks_unknown_event.yaml",
			wantErr: []string{`hooks: unknown event "worktree_destroyed"`, "stage_start"},
		},
		{
			name:    "blank run",
			fixture: "hooks_missing_run.yaml",
			wantErr: []string{"hooks stage_start[1]", "run is required"},
		},
		{
			name:    "unsupported on_failure",
			fixture: "hooks_bad_on_failure.yaml",
			wantErr: []string{`project "kontora" hooks stage_end[0]`, `on_failure "ignore"`},
		},
		{
			name:    "negative timeout",
			fixture: "hooks_negative_timeout.yaml",
			wantErr: []string{"hooks worktree_created[0]", "must not be negative"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", tt.fixture))
			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestHooksFor(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		Hooks: Hooks{
			HookWorktreeCreated: {{Name: "global", Run: "echo global"}},
			HookStageEnd:        {{Name: "global end", Run: "echo end"}},
		},
		Projects: map[string]Project{
			"kontora": {Path: "~/projects/kontora", Hooks: Hooks{
				HookWorktreeCreated: {{Name: "project", Run: "echo project"}},
			}},
			"sigil": {Path: "~/projects/sigil"},
		},
	}

	tests := []struct {
		name     string
		repoPath string
		event    string
		want     []string
	}{
		{
			name:     "global runs before project",
			repoPath: "~/projects/kontora",
			event:    HookWorktreeCreated,
			want:     []string{"global", "project"},
		},
		{
			name:     "project without hooks for the event runs global only",
			repoPath: "~/projects/kontora",
			event:    HookStageEnd,
			want:     []string{"global end"},
		},
		{
			name:     "unmatched repository runs global only",
			repoPath: "~/projects/other",
			event:    HookWorktreeCreated,
			want:     []string{"global"},
		},
		{
			name:     "project without hooks runs global only",
			repoPath: "~/projects/sigil",
			event:    HookWorktreeCreated,
			want:     []string{"global"},
		},
		{
			name:     "event with no hooks resolves to nothing",
			repoPath: "~/projects/kontora",
			event:    HookStageStart,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)

			var names []string
			for _, h := range cfg.HooksFor(tt.repoPath, tt.event) {
				names = append(names, h.Name)
			}
			assert.Equal(t, tt.want, names)
		})
	}
}
