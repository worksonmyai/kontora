package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	Web                 Web                 `yaml:"web"`
	Agents              map[string]Agent    `yaml:"agents"`
	Roles               map[string]Role     `yaml:"roles"`
	Pipelines           map[string]Pipeline `yaml:"pipelines"`
	Environment         map[string]string   `yaml:"environment"`
	Summarizer          *Summarizer         `yaml:"summarizer"`
}

type Summarizer struct {
	Binary  string   `yaml:"binary"`
	Args    []string `yaml:"args"`
	Prompt  string   `yaml:"prompt"`
	Timeout Duration `yaml:"timeout"`
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

type Role struct {
	Prompt  string   `yaml:"prompt"`
	Timeout Duration `yaml:"timeout"`
}

type Pipeline []Stage

type Stage struct {
	Role       string `yaml:"role"`
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
	if c.Summarizer != nil && c.Summarizer.Timeout.Duration == 0 {
		c.Summarizer.Timeout.Duration = 30 * time.Second
	}
}

var validOnSuccess = map[string]bool{"next": true, "done": true}
var validOnFailure = map[string]bool{"retry": true, "back": true, "pause": true}

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

	for name, pipeline := range c.Pipelines {
		if len(pipeline) == 0 {
			return fmt.Errorf("pipeline %q: must have at least one stage", name)
		}
		seen := make(map[string]int, len(pipeline))
		for i, stage := range pipeline {
			if prev, ok := seen[stage.Role]; ok {
				return fmt.Errorf("pipeline %q stage %d: duplicate role %q (first used at stage %d)", name, i, stage.Role, prev)
			}
			seen[stage.Role] = i
			if _, ok := c.Roles[stage.Role]; !ok {
				return fmt.Errorf("pipeline %q stage %d: unknown role %q", name, i, stage.Role)
			}
			if _, ok := c.Agents[stage.Agent]; !ok {
				return fmt.Errorf("pipeline %q stage %d: unknown agent %q", name, i, stage.Agent)
			}
			if !validOnSuccess[stage.OnSuccess] {
				return fmt.Errorf("pipeline %q stage %d: invalid on_success %q (must be next or done)", name, i, stage.OnSuccess)
			}
			if !validOnFailure[stage.OnFailure] {
				return fmt.Errorf("pipeline %q stage %d: invalid on_failure %q (must be retry, back, or pause)", name, i, stage.OnFailure)
			}
			if stage.OnFailure == "back" && i == 0 {
				return fmt.Errorf("pipeline %q stage %d: on_failure=back not allowed on first stage", name, i)
			}
		}
		last := pipeline[len(pipeline)-1]
		if last.OnSuccess != "done" {
			return fmt.Errorf("pipeline %q: last stage must have on_success=done, got %q", name, last.OnSuccess)
		}
	}

	if c.Summarizer != nil {
		if c.Summarizer.Binary == "" {
			return fmt.Errorf("summarizer: binary is required")
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
