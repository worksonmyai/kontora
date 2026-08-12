package config

import (
	"errors"
	"fmt"
	"io"
	"maps"
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

// Model selects the model an agent runs with. It reads either a bare pattern
// that applies to every agent, or a map from agent name or agent kind to a
// pattern, because a model name is not portable: claude takes "haiku" where pi
// takes "anthropic/claude-haiku-4-5".
type Model struct {
	Any     string
	ByAgent map[string]string
}

func (m *Model) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&m.Any)
	}
	if value.Kind == yaml.MappingNode {
		return value.Decode(&m.ByAgent)
	}
	return fmt.Errorf("invalid model on line %d: want a pattern or a map of agent name or agent kind to a pattern", value.Line)
}

// MarshalYAML keeps `kontora config` output loadable: the printed config is
// decoded with KnownFields(true), so a shape this type cannot read back would
// make the output a broken paste target.
func (m Model) MarshalYAML() (any, error) {
	if len(m.ByAgent) > 0 {
		return m.ByAgent, nil
	}
	if m.Any != "" {
		return m.Any, nil
	}
	return nil, nil
}

// For returns the pattern configured for one agent, or "" when none applies.
// An exact agent name wins over the agent's kind, so a map can name one agent
// among several of the same kind.
func (m Model) For(agentName string, a Agent) string {
	if v, ok := m.ByAgent[agentName]; ok {
		return v
	}
	if kind := a.Kind(); kind != "" {
		if v, ok := m.ByAgent[kind]; ok {
			return v
		}
	}
	return m.Any
}

type Config struct {
	TicketsDir          string              `yaml:"tickets_dir"`
	BranchPrefix        string              `yaml:"branch_prefix"`
	BranchNaming        BranchNaming        `yaml:"branch_naming"`
	WorktreesDir        string              `yaml:"worktrees_dir"`
	LogsDir             string              `yaml:"logs_dir"`
	Editor              string              `yaml:"editor"`
	DefaultAgent        string              `yaml:"default_agent"`
	MaxConcurrentAgents int                 `yaml:"max_concurrent_agents"`
	AutoPickUp          *bool               `yaml:"auto_pick_up"`
	InstanceName        string              `yaml:"instance_name"`
	TmuxSession         string              `yaml:"tmux_session"`
	Web                 Web                 `yaml:"web"`
	Agents              map[string]Agent    `yaml:"agents"`
	Stages              map[string]Stage    `yaml:"stages"`
	Pipelines           map[string]Pipeline `yaml:"pipelines"`
	Projects            map[string]Project  `yaml:"projects"`
	Statuses            []string            `yaml:"statuses"`
	Environment         map[string]string   `yaml:"environment"`
	Plannotator         Plannotator         `yaml:"plannotator"`
	Metrics             Metrics             `yaml:"metrics"`

	// SummaryModel selects the model the final summary pass runs with, resolved
	// against the agent that ran the last stage. It is one top-level field rather
	// than a per-stage one because the final summary is the only pass that spawns
	// an agent of its own: a run summary is written in-band by the stage agent.
	SummaryModel Model `yaml:"summary_model"`

	// ResumePrompt replaces the built-in prompt sent to an agent whose stage a
	// daemon restart interrupted. It is a stage prompt template with the same
	// `{{ .Ticket.* }}` fields.
	ResumePrompt string `yaml:"resume_prompt"`

	// AnnotationPrompt replaces the built-in prompt sent to the run a submitted
	// set of Plannotator annotations schedules. It is a stage prompt template. See
	// defaultAnnotationPrompt for the restrictions the built-in one states.
	AnnotationPrompt string `yaml:"annotation_prompt"`

	// ReworkIsBuiltin is true when the rework stage was injected by
	// applyDefaults. It flips to false when the user defines their own
	// `stages.rework:` block, signalling the daemon to leave routing to the
	// user's pipeline/on_success config.
	ReworkIsBuiltin bool `yaml:"-"`
}

const (
	BranchNamingModeOff  = "off"
	BranchNamingModeSlug = "slug"

	// BranchNamingModeDefault is the mode an unset branch_naming.mode means.
	BranchNamingModeDefault = BranchNamingModeSlug
)

type BranchNaming struct {
	Mode string `yaml:"mode"`
}

type Plannotator struct {
	Binary     string   `yaml:"binary"`
	Timeout    Duration `yaml:"timeout"`
	ReviewsDir string   `yaml:"reviews_dir"`
}

// Metrics configures OTLP metric export. Disabled by default: the daemon
// builds a no-op meter provider and records nothing.
type Metrics struct {
	Enabled *bool `yaml:"enabled"`
	// Endpoint is a bare host:port or a full URL. Empty leaves the address to
	// the SDK's own OTEL_EXPORTER_OTLP_* handling.
	Endpoint string            `yaml:"endpoint"`
	Headers  map[string]string `yaml:"headers"`
	Interval Duration          `yaml:"interval"`
	// Insecure sends over plain HTTP. An explicit scheme on Endpoint decides
	// instead; see ResolveInsecure.
	Insecure bool `yaml:"insecure"`
}

// EndpointScheme returns the URL scheme Endpoint states explicitly, lowercased,
// or "" for an empty endpoint or a bare host:port. url.Parse cannot tell those
// apart: it reads "localhost:4318" as scheme "localhost".
func (m Metrics) EndpointScheme() string {
	scheme, _, ok := strings.Cut(m.Endpoint, "://")
	if !ok {
		return ""
	}
	return strings.ToLower(scheme)
}

// ResolveInsecure reports whether the exporter talks plain HTTP, and whether
// the endpoint contradicts the insecure field. An explicit scheme wins, the
// same way the scheme of OTEL_EXPORTER_OTLP_ENDPOINT wins inside the exporter.
// Only `https://` together with `insecure: true` counts as a contradiction:
// insecure's zero value is false, so a plain `http://` endpoint under an unset
// insecure is the ordinary case, not a disagreement.
func (m Metrics) ResolveInsecure() (insecure, conflict bool) {
	switch m.EndpointScheme() {
	case "http":
		return true, false
	case "https":
		return false, m.Insecure
	default:
		return m.Insecure, false
	}
}

type Web struct {
	Enabled *bool  `yaml:"enabled"`
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	// Token, when non-empty, gates /api/ and /ws/ requests with a shared
	// bearer token. Empty (the default) disables auth and preserves the
	// original open-access behavior. This is a daemon-side setting only; the
	// CLI's own KONTORA_TOKEN is deliberately not folded in here to avoid a
	// stray env var silently locking down a local daemon.
	Token string `yaml:"token"`
}

type Agent struct {
	Binary      string            `yaml:"binary"`
	Args        []string          `yaml:"args"`
	Environment map[string]string `yaml:"environment"`
	// FailurePatterns are regexes matched against the agent's output log after
	// it exits. A match pauses the ticket even when the agent exited cleanly —
	// catching agents that report errors (quota, API failures) without a
	// non-zero exit code. When unset, DefaultFailurePatterns apply; set an
	// explicit list to override them, or [] to disable detection for this agent.
	// Claude also gets structural detection from its session JSONL regardless.
	FailurePatterns []string `yaml:"failure_patterns"`
	// Resume, when false, makes every stage this agent runs start a new
	// conversation even after a daemon restart interrupted one. Unset means
	// resume is on for Claude and pi, the two CLIs whose resume flags Kontora
	// knows. Any other agent ignores this field and always starts fresh.
	Resume *bool `yaml:"resume"`
}

// wrapperBinaries are launcher commands that run the real agent binary after
// a "--" separator (e.g. `nono run --profile pi -- pi ...`, `op run -- pi ...`).
var wrapperBinaries = map[string]bool{"nono": true, "op": true}

// effectiveBinary returns the binary that actually runs the agent. When the
// configured binary is a known wrapper, the first argument after the "--"
// separator is the real agent binary.
func (a Agent) effectiveBinary() string {
	if !wrapperBinaries[filepath.Base(a.Binary)] {
		return a.Binary
	}
	for i, arg := range a.Args {
		if arg == "--" && i+1 < len(a.Args) {
			return a.Args[i+1]
		}
	}
	return a.Binary
}

func (a Agent) IsClaude() bool {
	return filepath.Base(a.effectiveBinary()) == "claude"
}

func (a Agent) IsPi() bool {
	return filepath.Base(a.effectiveBinary()) == "pi"
}

// The agent CLIs Kontora knows the flags of. Any other agent has no kind.
const (
	AgentKindClaude = "claude"
	AgentKindPi     = "pi"
)

// Kind reports which CLI an agent runs, or "" when it is one Kontora does not
// know how to pass flags to.
func (a Agent) Kind() string {
	switch {
	case a.IsClaude():
		return AgentKindClaude
	case a.IsPi():
		return AgentKindPi
	}
	return ""
}

// ArgsWithModel returns the agent's arguments with model selected. It replaces
// the configured `--model <v>` and `--model=<v>` rather than appending a second
// pair, so the result does not depend on how each CLI resolves a repeated flag.
// Only the segment after a wrapper's "--" is rewritten: the arguments before it
// belong to nono or op. The match is exact, so pi's `--models` cycling list is
// left alone.
func (a Agent) ArgsWithModel(model string) []string {
	if model == "" {
		return slices.Clone(a.Args)
	}

	agentOwned := 0
	if wrapperBinaries[filepath.Base(a.Binary)] {
		for i, arg := range a.Args {
			if arg == "--" {
				agentOwned = i + 1
				break
			}
		}
	}

	out := make([]string, 0, len(a.Args)+2)
	out = append(out, a.Args[:agentOwned]...)
	for i := agentOwned; i < len(a.Args); i++ {
		switch arg := a.Args[i]; {
		case arg == "--model":
			i++ // its value goes with it
		case strings.HasPrefix(arg, "--model="):
		default:
			out = append(out, arg)
		}
	}
	return append(out, "--model", model)
}

type Stage struct {
	Prompt  string   `yaml:"prompt"`
	Timeout Duration `yaml:"timeout"`
	// Model overrides the model the stage's agent runs with, so a cheap stage
	// need not duplicate an agent entry to change model.
	Model Model `yaml:"model"`
}

// NoneSentinel is the literal pipeline or agent value that opts a ticket out of
// its project's default. Validate rejects a pipeline or an agent named "none",
// so the sentinel can never collide with a real definition.
const NoneSentinel = "none"

// ClearNone maps the sentinel to a blank value and passes everything else
// through, so every command that takes a pipeline or an agent name reads "none"
// the same way: leave the field empty.
func ClearNone(value string) string {
	if value == NoneSentinel {
		return ""
	}
	return value
}

// Project names a repository and the defaults that apply to its tickets.
// Every default is optional; an entry may set any of them, or none.
type Project struct {
	Path         string       `yaml:"path"`
	Pipeline     string       `yaml:"pipeline"`
	Agent        string       `yaml:"agent"`
	BranchPrefix string       `yaml:"branch_prefix"`
	BranchNaming BranchNaming `yaml:"branch_naming"`
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

// osHostname is indirected so tests can exercise the instance_name fallback
// when hostname lookup fails.
var osHostname = os.Hostname

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

// ServerTokenEnvVar overrides web.token for the daemon. Unlike the CLI's own
// KONTORA_TOKEN (a client credential), this is read only by `kontora start`, so
// it can carry the token from a secret-backed env var without the risk the
// config comment warns about: a stray env var silently locking down a daemon.
const ServerTokenEnvVar = "KONTORA_WEB_TOKEN"

// ApplyServerEnvOverrides folds daemon-only environment overrides into the
// config after it is loaded. Today that is just the web token, so deployments
// can inject it from a secret instead of writing it into the on-disk config.
func (c *Config) ApplyServerEnvOverrides() {
	if v := strings.TrimSpace(os.Getenv(ServerTokenEnvVar)); v != "" {
		c.Web.Token = v
	}
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
	if c.BranchNaming.Mode == "" {
		c.BranchNaming.Mode = BranchNamingModeDefault
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
	if c.InstanceName == "" {
		// The instance name identifies this daemon when several run against the
		// same synced tickets_dir. Default to the hostname; fall back to
		// "default" when the OS can't report one. Two machines that share a
		// hostname must set instance_name explicitly, or the claim protection
		// can't tell them apart.
		if host, err := osHostname(); err == nil && host != "" {
			c.InstanceName = host
		} else {
			c.InstanceName = "default"
		}
	}
	if c.TmuxSession == "" {
		c.TmuxSession = defaultTmuxSession
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

	if c.Metrics.Enabled == nil {
		c.Metrics.Enabled = new(false)
	}
	if c.Metrics.Interval.Duration == 0 {
		c.Metrics.Interval.Duration = 60 * time.Second
	}

	// Agents with no explicit failure_patterns get the built-in defaults. A nil
	// slice means the key was absent; an explicit [] (non-nil, empty) opts out.
	for name, agent := range c.Agents {
		if agent.FailurePatterns == nil {
			agent.FailurePatterns = DefaultFailurePatterns
			c.Agents[name] = agent
		}
	}

	if _, ok := c.Stages[ReworkStageName]; !ok {
		if c.Stages == nil {
			c.Stages = map[string]Stage{}
		}
		c.Stages[ReworkStageName] = defaultReworkStage()
		c.ReworkIsBuiltin = true
	}

	// human_review used to be shipped as a custom status in the default config.
	// Drop it from user-declared statuses so old configs keep loading after it
	// became a built-in.
	c.Statuses = slices.DeleteFunc(c.Statuses, func(s string) bool { return s == "human_review" })
}

var validStatusNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// defaultTmuxSession duplicates tmux.DefaultSessionName because config imports
// no internal packages. Change both together.
const defaultTmuxSession = "kontora"

// validTmuxSessionRe rejects the characters tmux itself cannot address (`.`,
// `:`) and the ones that would break out of the wait-for channel name, which
// the daemon interpolates raw into a JSON string and a shell command. A leading
// `-` is rejected too: the session name starts the channel name, and tmux reads
// `tmux wait-for -S -x-tst-001` as an unknown flag, so the Stop hook could never
// signal an interactive agent's completion.
var validTmuxSessionRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)

// TmuxSessionName returns the tmux session this config addresses. It is the
// single place an empty value becomes the default, so the daemon, CLI, TUI, and
// web cannot disagree. Configs built in memory never run applyDefaults, which
// is why the fallback lives here and not at each caller.
func (c *Config) TmuxSessionName() string {
	if c.TmuxSession == "" {
		return defaultTmuxSession
	}
	return c.TmuxSession
}

var builtinStatuses = map[string]bool{
	"open": true, "todo": true, "in_progress": true,
	"paused": true, "human_review": true, "done": true, "cancelled": true,
	"archived": true,
}

var reservedKeywords = map[string]bool{
	"next": true, "retry": true, "back": true,
}

// IsCustomStatus returns true if s is a user-defined custom status.
func (c *Config) IsCustomStatus(s string) bool {
	return slices.Contains(c.Statuses, s)
}

// boardStatuses are the built-in statuses that map to a board column.
// archived is intentionally excluded: archived tickets are hidden from the board.
var boardStatuses = map[string]bool{
	"open": true, "todo": true, "in_progress": true,
	"paused": true, "human_review": true, "done": true, "cancelled": true,
}

// IsBoardStatus reports whether s maps to a board column (a non-archived
// built-in status or a configured custom status). Tickets with any other
// status (e.g. foreign "closed"/"tombstone") are hidden from the board list.
func (c *Config) IsBoardStatus(s string) bool {
	return boardStatuses[s] || c.IsCustomStatus(s)
}

// editableStatuses are the built-in statuses in which a ticket may be edited.
// in_progress is excluded because an agent owns the file, and the terminal
// statuses because the work is over.
var editableStatuses = map[string]bool{
	"open": true, "todo": true, "paused": true, "human_review": true,
}

// StatusAllowsEdit reports whether a ticket in status s may have its body and
// frontmatter edited: by `kontora update`, by the web API, or by an annotation
// pass, which ends in a ticket-body edit. One set, so the CLI, the daemon and
// the dashboard cannot disagree about when a ticket is editable.
func (c *Config) StatusAllowsEdit(s string) bool {
	return editableStatuses[s] || c.IsCustomStatus(s)
}

func (c *Config) Validate() error {
	if err := validateBranchNamingMode(c.BranchNaming.Mode); err != nil {
		return err
	}

	if _, ok := c.Agents[c.DefaultAgent]; !ok {
		if c.DefaultAgent == "" {
			return fmt.Errorf("default_agent: could not infer (set it explicitly or name an agent \"claude\")")
		}
		return fmt.Errorf("default_agent %q: not found in agents", c.DefaultAgent)
	}

	if !validTmuxSessionRe.MatchString(c.TmuxSession) {
		return fmt.Errorf("tmux_session %q: must be 1-64 characters from [A-Za-z0-9_-] and must not start with %q", c.TmuxSession, "-")
	}

	for name, agent := range c.Agents {
		if name == NoneSentinel {
			return fmt.Errorf("agent %q: name is reserved for the project-default opt-out", name)
		}
		if agent.Binary == "" {
			return fmt.Errorf("agent %q: binary is required", name)
		}
		for _, p := range agent.FailurePatterns {
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("agent %q: invalid failure_pattern %q: %w", name, p, err)
			}
		}
	}

	if err := c.validateMetrics(); err != nil {
		return err
	}

	if err := c.validateModels(); err != nil {
		return err
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

	if err := c.validatePipelines(); err != nil {
		return err
	}

	return c.validateProjects()
}

func (c *Config) validatePipelines() error {
	// Build valid on_success/on_failure sets dynamically.
	validOnSuccess := map[string]bool{"next": true, "done": true, "human_review": true}
	validOnFailure := map[string]bool{"retry": true, "back": true, "pause": true, "human_review": true}
	for _, s := range c.Statuses {
		validOnSuccess[s] = true
		validOnFailure[s] = true
	}

	for name, pipeline := range c.Pipelines {
		if name == NoneSentinel {
			return fmt.Errorf("pipeline %q: name is reserved for the project-default opt-out", name)
		}
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
			// A step names both the stage and the agent, so the pair can be
			// rejected here. The same pair reached through a ticket's own agent
			// field is only visible at spawn time, and pauses the ticket.
			stepAgent := c.Agents[step.Agent]
			if model := c.Stages[step.Stage].Model.For(step.Agent, stepAgent); model != "" && stepAgent.Kind() == "" {
				return fmt.Errorf("pipeline %q stage %d: stage %q sets model %q, which agent %q (%s) takes no flag for",
					name, i, step.Stage, model, step.Agent, stepAgent.Binary)
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

// validateModels checks every model map for keys that name nothing. Stages are
// checked in name order so the error always names the same stage, whatever
// order the YAML map decoded in.
func (c *Config) validateModels() error {
	for _, name := range slices.Sorted(maps.Keys(c.Stages)) {
		if err := c.validateModelKeys(c.Stages[name].Model); err != nil {
			return fmt.Errorf("stage %q: %w", name, err)
		}
	}
	if err := c.validateModelKeys(c.SummaryModel); err != nil {
		return fmt.Errorf("summary_model: %w", err)
	}
	return nil
}

func (c *Config) validateModelKeys(m Model) error {
	for _, key := range slices.Sorted(maps.Keys(m.ByAgent)) {
		if _, ok := c.Agents[key]; ok {
			continue
		}
		if key == AgentKindClaude || key == AgentKindPi {
			continue
		}
		return fmt.Errorf("model %q: neither a configured agent nor an agent kind (%s, %s)",
			key, AgentKindClaude, AgentKindPi)
	}
	return nil
}

// validateMetrics checks the export settings that only matter once export is
// on. A disabled section is left alone: nothing reads it, and rejecting a
// half-written one would keep the daemon from starting over a subsystem the
// user has switched off.
func (c *Config) validateMetrics() error {
	if c.Metrics.Enabled == nil || !*c.Metrics.Enabled {
		return nil
	}
	if c.Metrics.Interval.Duration <= 0 {
		return fmt.Errorf("metrics.interval %s: must be positive", c.Metrics.Interval)
	}
	// An empty endpoint hands the address to the SDK's OTEL_EXPORTER_OTLP_*
	// handling, and a bare host:port is resolved against metrics.insecure. Only
	// a stated scheme can be wrong.
	switch scheme := c.Metrics.EndpointScheme(); scheme {
	case "", "http", "https":
		return nil
	default:
		return fmt.Errorf("metrics.endpoint %q: scheme %q is not supported (use http or https, or a bare host:port)", c.Metrics.Endpoint, scheme)
	}
}

// validateProjects checks entries in name order so the duplicate-path error
// always names the same pair, whatever order the YAML map decoded in.
func (c *Config) validateProjects() error {
	pathOwner := make(map[string]string, len(c.Projects))
	for _, name := range slices.Sorted(maps.Keys(c.Projects)) {
		p := c.Projects[name]
		if p.Path == "" {
			return fmt.Errorf("project %q: path is required", name)
		}
		if p.Pipeline != "" {
			if _, ok := c.Pipelines[p.Pipeline]; !ok {
				return fmt.Errorf("project %q: unknown pipeline %q", name, p.Pipeline)
			}
		}
		if p.Agent != "" {
			if _, ok := c.Agents[p.Agent]; !ok {
				return fmt.Errorf("project %q: unknown agent %q", name, p.Agent)
			}
		}
		if err := validateBranchNamingMode(p.BranchNaming.Mode); err != nil {
			return fmt.Errorf("project %q: %w", name, err)
		}
		// ProjectFor matches on the normalized path, so two entries that expand
		// to the same directory would make the lookup pick one at random.
		norm := NormalizeRepoPath(p.Path)
		if owner, ok := pathOwner[norm]; ok {
			return fmt.Errorf("project %q: path %s duplicates project %q", name, norm, owner)
		}
		pathOwner[norm] = name
	}
	return nil
}

// ProjectFor returns the project configured for repoPath. Both sides are
// compared after tilde expansion and filepath.Clean, so "~/projects/kontora",
// the same directory written in absolute form, and a trailing slash all reach
// the same entry.
//
// Only the complete path matches. A ticket pointing at a subdirectory of a
// configured project does not inherit that project's defaults, and symlinks are
// not resolved: a silent ancestor match would be hard to predict.
func (c *Config) ProjectFor(repoPath string) (string, Project, bool) {
	if len(c.Projects) == 0 || repoPath == "" {
		return "", Project{}, false
	}
	want := NormalizeRepoPath(repoPath)
	for name, p := range c.Projects {
		if NormalizeRepoPath(p.Path) == want {
			return name, p, true
		}
	}
	return "", Project{}, false
}

// ApplyProjectDefaults resolves the pipeline and agent a ticket for repoPath
// should carry. A blank field takes the matching project's default; the literal
// "none" clears the field and skips that default, so a standalone ticket stays
// reachable inside a configured project. The two fields resolve independently:
// opting the pipeline out still inherits the project agent.
func (c *Config) ApplyProjectDefaults(repoPath, pipeline, agent string) (resolvedPipeline, resolvedAgent string) {
	pipelineOptOut := pipeline == NoneSentinel
	pipeline = ClearNone(pipeline)
	agentOptOut := agent == NoneSentinel
	agent = ClearNone(agent)

	_, project, ok := c.ProjectFor(repoPath)
	if !ok {
		return pipeline, agent
	}
	if pipeline == "" && !pipelineOptOut {
		pipeline = project.Pipeline
	}
	if agent == "" && !agentOptOut {
		agent = project.Agent
	}
	return pipeline, agent
}

// BranchPrefixFor returns the branch prefix branches for repoPath are named
// with: the prefix of the project that owns the path, or the top-level
// branch_prefix when the project sets none or the path belongs to no project.
func (c *Config) BranchPrefixFor(repoPath string) string {
	if _, project, ok := c.ProjectFor(repoPath); ok && project.BranchPrefix != "" {
		return project.BranchPrefix
	}
	return c.BranchPrefix
}

// BranchNamingFor returns the branch naming mode for repoPath. A project mode
// overrides the top-level mode. Empty in-memory configs use the default mode.
func (c *Config) BranchNamingFor(repoPath string) BranchNaming {
	mode := c.BranchNaming.Mode
	if mode == "" {
		mode = BranchNamingModeDefault
	}
	if _, project, ok := c.ProjectFor(repoPath); ok && project.BranchNaming.Mode != "" {
		mode = project.BranchNaming.Mode
	}
	return BranchNaming{Mode: mode}
}

func validateBranchNamingMode(mode string) error {
	switch mode {
	case "", BranchNamingModeOff, BranchNamingModeSlug:
		return nil
	default:
		return fmt.Errorf("branch_naming.mode %q: must be %q or %q", mode, BranchNamingModeOff, BranchNamingModeSlug)
	}
}

// NormalizeRepoPath is the form in which repository paths are compared: tilde
// expanded and cleaned. Clients that must match a project against a path typed
// by a user need the same form, since only the daemon host knows its home
// directory.
func NormalizeRepoPath(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Clean(ExpandTilde(path))
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
