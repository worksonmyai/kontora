package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = dur
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

type Config struct {
	TicketsDir          string              `yaml:"tickets_dir"`
	BranchPrefix        string              `yaml:"branch_prefix"`
	WorktreesDir        string              `yaml:"worktrees_dir"`
	LogsDir             string              `yaml:"logs_dir"`
	Editor              string              `yaml:"editor"`
	DefaultAgent        string              `yaml:"default_agent"`
	MaxConcurrentAgents int                 `yaml:"max_concurrent_agents"`
	AutoPickUp          *bool               `yaml:"auto_pick_up"`
	Web                 Web                 `yaml:"web"`
	Agents              map[string]Agent    `yaml:"agents"`
	Stages              map[string]Stage    `yaml:"stages"`
	Pipelines           map[string]Pipeline `yaml:"pipelines"`
	Statuses            []string            `yaml:"statuses"`
	Environment         map[string]string   `yaml:"environment"`
	Plannotator         Plannotator         `yaml:"plannotator"`

	// ReworkIsBuiltin is true when the rework stage was injected by
	// applyDefaults. It flips to false when the user defines their own
	// `stages.rework:` block, signalling the daemon to leave routing to the
	// user's pipeline/on_success config.
	ReworkIsBuiltin bool `yaml:"-"`
}

type Plannotator struct {
	Binary     string   `yaml:"binary"`
	Timeout    Duration `yaml:"timeout"`
	ReviewsDir string   `yaml:"reviews_dir"`
}

type Web struct {
	Enabled *bool  `yaml:"enabled"`
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
}

type Agent struct {
	Binary      string            `yaml:"binary"`
	Args        []string          `yaml:"args"`
	Environment map[string]string `yaml:"environment"`
}

func (a Agent) IsClaude() bool {
	return filepath.Base(a.Binary) == "claude"
}

func (a Agent) IsPi() bool {
	return filepath.Base(a.Binary) == "pi"
}

type Stage struct {
	Prompt  string   `yaml:"prompt"`
	Timeout Duration `yaml:"timeout"`
}

type Pipeline []PipelineStep

type PipelineStep struct {
	Stage      string `yaml:"stage"`
	Agent      string `yaml:"agent"`
	OnSuccess  string `yaml:"on_success"`
	OnFailure  string `yaml:"on_failure"`
	MaxRetries int    `yaml:"max_retries"`
}

// ErrNotFound is returned by Load when the config file does not exist.
var ErrNotFound = errors.New("config not found")

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, err
	}
	defer f.Close()
	return LoadReader(f)
}

func LoadReader(r io.Reader) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.TicketsDir == "" {
		c.TicketsDir = "~/.kontora/tickets"
	}
	if c.WorktreesDir == "" {
		c.WorktreesDir = "~/.kontora/worktrees"
	}
	if c.LogsDir == "" {
		c.LogsDir = "~/.kontora/logs"
	}
	if c.BranchPrefix == "" {
		c.BranchPrefix = "kontora"
	}
	if c.DefaultAgent == "" {
		if _, ok := c.Agents["claude"]; ok {
			c.DefaultAgent = "claude"
		} else if len(c.Agents) == 1 {
			for name := range c.Agents {
				c.DefaultAgent = name
			}
		}
	}
	if c.MaxConcurrentAgents == 0 {
		c.MaxConcurrentAgents = 3
	}
	if c.AutoPickUp == nil {
		c.AutoPickUp = new(true)
	}
	if c.Web.Enabled == nil {
		enabled := true
		c.Web.Enabled = &enabled
	}
	if c.Web.Host == "" {
		c.Web.Host = "127.0.0.1"
	}
	if c.Web.Port == 0 {
		c.Web.Port = 8080
	}

	if c.Plannotator.Binary == "" {
		c.Plannotator.Binary = "plannotator"
	}
	if c.Plannotator.Timeout.Duration == 0 {
		c.Plannotator.Timeout.Duration = 30 * time.Minute
	}
	if c.Plannotator.ReviewsDir == "" {
		c.Plannotator.ReviewsDir = "~/.kontora/plannotator-reviews"
	}

	if _, ok := c.Stages[ReworkStageName]; !ok {
		if c.Stages == nil {
			c.Stages = map[string]Stage{}
		}
		c.Stages[ReworkStageName] = defaultReworkStage()
		c.ReworkIsBuiltin = true
	}
}

var validStatusNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var builtinStatuses = map[string]bool{
	"open": true, "todo": true, "in_progress": true,
	"paused": true, "done": true, "cancelled": true,
}

var reservedKeywords = map[string]bool{
	"next": true, "retry": true, "back": true,
}

// IsCustomStatus returns true if s is a user-defined custom status.
func (c *Config) IsCustomStatus(s string) bool {
	return slices.Contains(c.Statuses, s)
}

func (c *Config) Validate() error {
	if _, ok := c.Agents[c.DefaultAgent]; !ok {
		if c.DefaultAgent == "" {
			return fmt.Errorf("default_agent: could not infer (set it explicitly or name an agent \"claude\")")
		}
		return fmt.Errorf("default_agent %q: not found in agents", c.DefaultAgent)
	}

	for name, agent := range c.Agents {
		if agent.Binary == "" {
			return fmt.Errorf("agent %q: binary is required", name)
		}
	}

	// Validate custom statuses.
	seen := make(map[string]bool, len(c.Statuses))
	for _, s := range c.Statuses {
		if !validStatusNameRe.MatchString(s) {
			return fmt.Errorf("custom status %q: must match [a-z][a-z0-9_]*", s)
		}
		if builtinStatuses[s] {
			return fmt.Errorf("custom status %q: clashes with built-in status", s)
		}
		if reservedKeywords[s] {
			return fmt.Errorf("custom status %q: clashes with reserved keyword", s)
		}
		if seen[s] {
			return fmt.Errorf("custom status %q: duplicate", s)
		}
		seen[s] = true
	}

	// Build valid on_success/on_failure sets dynamically.
	validOnSuccess := map[string]bool{"next": true, "done": true}
	validOnFailure := map[string]bool{"retry": true, "back": true, "pause": true}
	for _, s := range c.Statuses {
		validOnSuccess[s] = true
		validOnFailure[s] = true
	}

	for name, pipeline := range c.Pipelines {
		if len(pipeline) == 0 {
			return fmt.Errorf("pipeline %q: must have at least one stage", name)
		}
		seenStages := make(map[string]int, len(pipeline))
		for i, step := range pipeline {
			if prev, ok := seenStages[step.Stage]; ok {
				return fmt.Errorf("pipeline %q stage %d: duplicate stage %q (first used at stage %d)", name, i, step.Stage, prev)
			}
			seenStages[step.Stage] = i
			if _, ok := c.Stages[step.Stage]; !ok {
				return fmt.Errorf("pipeline %q stage %d: unknown stage %q", name, i, step.Stage)
			}
			if _, ok := c.Agents[step.Agent]; !ok {
				return fmt.Errorf("pipeline %q stage %d: unknown agent %q", name, i, step.Agent)
			}
			if !validOnSuccess[step.OnSuccess] {
				return fmt.Errorf("pipeline %q stage %d: invalid on_success %q (must be next, done, or a custom status)", name, i, step.OnSuccess)
			}
			if !validOnFailure[step.OnFailure] {
				return fmt.Errorf("pipeline %q stage %d: invalid on_failure %q (must be retry, back, pause, or a custom status)", name, i, step.OnFailure)
			}
			if step.OnFailure == "back" && i == 0 {
				return fmt.Errorf("pipeline %q stage %d: on_failure=back not allowed on first stage", name, i)
			}
		}
		last := pipeline[len(pipeline)-1]
		if last.OnSuccess == "next" {
			return fmt.Errorf("pipeline %q: last stage must not have on_success=next, got %q", name, last.OnSuccess)
		}
	}

	return nil
}

// ExpandTilde replaces a leading ~/ with the user's home directory.
func ExpandTilde(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
