package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// reloadHarness pairs the daemon test harness with a real config file on disk,
// which is what a reload re-reads. The daemon harness builds *config.Config
// structs directly, bypassing applyDefaults, so reload tests need YAML.
type reloadHarness struct {
	*testHarness
	configPath string
	logBuf     *syncBuffer
}

// syncBuffer is a bytes.Buffer safe to read while the daemon is still logging
// into it. A failing test dumps the log from the test goroutine while the
// daemon's goroutines are still running, which a bare bytes.Buffer would race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newReloadHarness(t *testing.T) *reloadHarness {
	t.Helper()
	h := newHarness(t)
	// The reload path folds the environment over the file, so a developer's
	// exported TICKETS_DIR would otherwise replace the harness's own store.
	t.Setenv(config.TicketsDirEnvVar, "")
	t.Setenv(config.LegacyTicketsDirEnvVar, "")
	rh := &reloadHarness{
		testHarness: h,
		configPath:  filepath.Join(t.TempDir(), "config.yaml"),
		logBuf:      &syncBuffer{},
	}
	// These tests drive a live daemon, so the reason for a failure is almost
	// always in its log and nowhere else.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", rh.logBuf.String())
		}
	})
	return rh
}

// configOpts overrides parts of the config file h.yaml renders. Anything left
// empty falls back to a default that points at the harness's temp dirs.
type configOpts struct {
	step1Prompt  string
	agent1Binary string
	agent1Args   string
	defaultAgent string
	branchPrefix string
	statuses     string
	environment  string
	plannotator  string
	extra        string
	pipelines    string
}

func (h *reloadHarness) yaml(o configOpts) string {
	if o.step1Prompt == "" {
		o.step1Prompt = "do step1 for {{ .Ticket.ID }}"
	}
	if o.agent1Binary == "" {
		o.agent1Binary = "true"
	}
	if o.defaultAgent == "" {
		o.defaultAgent = "agent1"
	}
	if o.branchPrefix == "" {
		o.branchPrefix = "kontora"
	}
	if o.pipelines == "" {
		o.pipelines = `  one-stage:
    - stage: step1
      agent: agent1
      on_success: done
      on_failure: pause
  two-stage:
    - stage: step1
      agent: agent1
      on_success: next
      on_failure: pause
    - stage: step2
      agent: agent2
      on_success: done
      on_failure: pause
`
	}
	return fmt.Sprintf(`tickets_dir: %s
worktrees_dir: %s
logs_dir: %s
branch_prefix: %s
default_agent: %s
max_concurrent_agents: 4
instance_name: test-instance
tmux_session: kontora-test
web:
  enabled: false
agents:
  agent1:
    binary: %s
%s  agent2:
    binary: "true"
stages:
  step1:
    prompt: %q
  step2:
    prompt: "do step2 for {{ .Ticket.ID }}"
pipelines:
%s%s%s%s%s`,
		h.tasksDir, h.wtDir, h.logsDir,
		o.branchPrefix, o.defaultAgent,
		o.agent1Binary, o.agent1Args,
		o.step1Prompt,
		o.pipelines,
		o.statuses, o.environment, o.plannotator, o.extra)
}

func (h *reloadHarness) writeConfig(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(h.configPath, []byte(content), 0o644))
}

// writeConfigAtomic replaces the config the way an editor save does. Tests that
// write while a reload may be reading need it: os.WriteFile truncates in place,
// so a concurrent reader can see the head of the new file and the tail of the
// old one. That is a torn file, not a torn config snapshot, and it would
// obscure what the concurrency tests are checking.
func (h *reloadHarness) writeConfigAtomic(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, h.writeConfigAtomicErr(content))
}

// writeConfigAtomicErr is writeConfigAtomic for a goroutine other than the
// test's own, which must not call require: FailNow off the test goroutine is
// undefined.
func (h *reloadHarness) writeConfigAtomicErr(content string) error {
	return atomicWriteFile(h.configPath, []byte(content), 0o644)
}

// newDaemonWithConfig writes content to the config file, loads it the way the
// daemon does at startup, and returns a daemon pointed at that file.
func (h *reloadHarness) newDaemonWithConfig(t *testing.T, content string, opts ...Option) *Daemon {
	t.Helper()
	h.writeConfig(t, content)
	cfg, err := config.Load(h.configPath)
	require.NoError(t, err, "initial config must load")
	cfg.ApplyEnvOverrides()
	cfg.ApplyServerEnvOverrides()

	base := make([]Option, 0, 7+len(opts))
	base = append(base,
		WithLogger(slog.New(slog.NewTextHandler(h.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithConfigPath(h.configPath),
		WithRunner(DirectRunner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
	)
	return New(cfg, append(base, opts...)...)
}

func TestReload_InvalidConfigKeepsRunning(t *testing.T) {
	h := newReloadHarness(t)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))
	before := d.config()

	tests := []struct {
		name    string
		content string
	}{
		{"unknown field", h.yaml(configOpts{extra: "unknown_field: x\n"})},
		{"project references a missing epic child pipeline", h.yaml(configOpts{extra: fmt.Sprintf("projects:\n  repo:\n    path: %s\n    epic_child_pipeline: missing\n", h.repoDir)})},
		{"negative supervisor capacity", h.yaml(configOpts{extra: "max_concurrent_epic_supervisors: -1\n"})},
		{"pipeline references a missing stage", h.yaml(configOpts{pipelines: `  one-stage:
    - stage: nonexistent
      agent: agent1
      on_success: done
      on_failure: pause
`})},
		{"truncated mid-write", "tickets_dir: /tmp\nagents:\n  agent1:\n    bin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.writeConfig(t, tc.content)
			require.Error(t, d.reloadConfig())
			assert.Same(t, before, d.config(), "running config must survive a rejected reload")
		})
	}

	// The next good save reloads.
	h.writeConfig(t, h.yaml(configOpts{branchPrefix: "recovered"}))
	require.NoError(t, d.reloadConfig())
	assert.Equal(t, "recovered", d.config().BranchPrefix)
}

func TestReload_GetConfigAdvertisesRuntimeIdentity(t *testing.T) {
	h := newReloadHarness(t)
	store := t.TempDir()
	alias := filepath.Join(t.TempDir(), "tickets")
	require.NoError(t, os.Symlink(store, alias))
	cwd, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(cwd, alias)
	require.NoError(t, err)
	initial := strings.Replace(h.yaml(configOpts{}), "tickets_dir: "+h.tasksDir, "tickets_dir: "+relative, 1)

	// Never contact the developer's default server.
	socketDir, err := os.MkdirTemp("", "kt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "socket")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "tmux"), []byte(`#!/bin/sh
[ "$#" = 6 ] && [ "$1" = -N ] && [ "$2" = display-message ] && [ "$3" = -p ] && [ "$4" = -t ] && [ "$5" = '=kontora-test:' ] && [ "$6" = '#{session_name}:#{socket_path}' ] || exit 99
printf 'kontora-test:%s\n' "$TEST_TMUX_SOCKET"
`), 0o755))
	t.Setenv("PATH", binDir)
	t.Setenv("TEST_TMUX_SOCKET", socket)
	d := h.newDaemonWithConfig(t, initial)
	require.NoError(t, d.initialScan(relative))
	startIdentityDiscovery(t, d, 10*time.Millisecond, time.Second)
	require.Eventually(t, func() bool {
		return d.GetConfig().TmuxSocket == socket
	}, 3*time.Second, 10*time.Millisecond)

	assertIdentity := func() {
		t.Helper()
		got := d.GetConfig()
		assert.Equal(t, mustEvalDir(t, store), got.TicketsDir)
		assert.True(t, filepath.IsAbs(got.TicketsDir))
		assert.Equal(t, "kontora-test", got.TmuxSession)
		assert.Equal(t, socket, got.TmuxSocket)
		data, err := json.Marshal(got)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(data, &fields))
		assert.Equal(t, got.TicketsDir, fields["tickets_dir"])
		assert.Equal(t, got.TmuxSession, fields["tmux_session"])
		assert.Equal(t, socket, fields["tmux_socket"])
	}
	assertIdentity()

	changed := strings.Replace(h.yaml(configOpts{branchPrefix: "reloaded"}), "tickets_dir: "+h.tasksDir, "tickets_dir: "+t.TempDir(), 1)
	changed = strings.Replace(changed, "tmux_session: kontora-test", "tmux_session: edited-session", 1)
	h.writeConfig(t, changed)
	require.NoError(t, d.reloadConfig())
	assert.Equal(t, "reloaded", d.config().BranchPrefix, "live fields still reload")
	assertIdentity()

	require.NoError(t, listener.Close())
	require.Eventually(t, func() bool {
		return d.GetConfig().TmuxSocket == ""
	}, 3*time.Second, 10*time.Millisecond)
	got := d.GetConfig()
	assert.Empty(t, got.TmuxSocket)
	data, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"tmux_socket"`)
}

func TestGetConfigOmitsUnavailableRuntimeIdentity(t *testing.T) {
	h := newReloadHarness(t)
	require.NoError(t, os.Remove(h.tasksDir))
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))
	require.Error(t, d.initialScan(h.tasksDir))
	require.NoError(t, os.Mkdir(h.tasksDir, 0o755))
	require.NoError(t, d.initialScan(h.tasksDir))
	got := d.GetConfig()
	assert.Empty(t, got.TicketsDir)
	assert.Empty(t, got.TmuxSocket)
	assert.Equal(t, d.tmuxSession, got.TmuxSession)
	data, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"tickets_dir"`)
	assert.NotContains(t, string(data), `"tmux_socket"`)
	data, err = json.Marshal(web.ConfigInfo{})
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"tmux_session"`, "runtime identity fields are optional")
}

func TestReload_NoConfigPath(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg) // no WithConfigPath
	require.ErrorIs(t, d.reloadConfig(), errNoConfigPath)
}

func TestReload_PinsRestartOnlyFields(t *testing.T) {
	h := newReloadHarness(t)
	metricsBefore := `metrics:
  enabled: true
  endpoint: localhost:4318
  insecure: true
  interval: 30s
  headers:
    authorization: metrics-secret
notifications:
  attempts: 2
  timeout: 5s
  backoff: 2s
  channels:
    tg:
      type: telegram
      secret_env: KONTORA_TG_BEFORE
      chat_id: "1"
  default: [tg]
`
	metricsAfter := `max_concurrent_epic_supervisors: 8
metrics:
  enabled: false
  endpoint: collector.example:4318
  insecure: false
  interval: 5s
  headers:
    authorization: rotated-secret
notifications:
  enabled: false
  attempts: 5
  timeout: 30s
  backoff: 10s
  channels:
    tg:
      type: telegram
      secret_env: KONTORA_TG_ROTATED
      chat_id: "2"
  default: []
`
	reviewsDir := t.TempDir()
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{extra: metricsBefore, plannotator: "plannotator:\n  reviews_dir: " + reviewsDir + "\n"}))
	before := d.config()

	otherDir := t.TempDir()
	changed := h.yaml(configOpts{extra: metricsAfter, plannotator: "plannotator:\n  reviews_dir: " + otherDir + "\n"}) + "\n"
	changed = strings.Replace(changed, "tickets_dir: "+h.tasksDir, "tickets_dir: "+otherDir, 1)
	changed = strings.Replace(changed, "worktrees_dir: "+h.wtDir, "worktrees_dir: "+otherDir, 1)
	changed = strings.Replace(changed, "logs_dir: "+h.logsDir, "logs_dir: "+otherDir, 1)
	changed = strings.Replace(changed, "max_concurrent_agents: 4", "max_concurrent_agents: 8", 1)
	changed = strings.Replace(changed, "instance_name: test-instance", "instance_name: other-instance", 1)
	changed = strings.Replace(changed, "tmux_session: kontora-test", "tmux_session: kontora-other", 1)
	changed = strings.Replace(changed, "  enabled: false", "  enabled: false\n  host: 0.0.0.0\n  port: 9090\n  token: secret\n  allowed_hosts: [kontora.example]", 1)

	h.writeConfig(t, changed)
	require.NoError(t, d.reloadConfig())

	got := d.config()
	assert.NotSame(t, before, got, "a valid reload must publish a new config")
	assert.Equal(t, h.tasksDir, got.TicketsDir)
	assert.Equal(t, h.wtDir, got.WorktreesDir)
	assert.Equal(t, h.logsDir, got.LogsDir)
	assert.Equal(t, "test-instance", got.InstanceName)
	assert.Equal(t, "kontora-test", got.TmuxSession)
	assert.Equal(t, 8, got.MaxConcurrentAgents)
	assert.Equal(t, 8, got.MaxConcurrentEpicSupervisors)
	assert.Equal(t, reviewsDir, got.Plannotator.ReviewsDir)
	assert.Equal(t, before.Web, got.Web, "the web block is pinned whole")
	assert.Equal(t, before.Metrics, got.Metrics, "the metrics block is pinned whole")
	assert.Equal(t, "localhost:4318", got.Metrics.Endpoint, "the running exporter's endpoint survives the reload")
	assert.False(t, *got.Notifications.Enabled)
	assert.Equal(t, "KONTORA_TG_ROTATED", got.Notifications.Channels["tg"].SecretEnv)

	logs := h.logBuf.String()
	for _, field := range []string{
		"tickets_dir", "worktrees_dir", "logs_dir", "instance_name", "tmux_session",
		"plannotator.reviews_dir", "web.host", "web.port", "web.token", "web.allowed_hosts",
		"metrics.enabled", "metrics.endpoint", "metrics.headers", "metrics.interval", "metrics.insecure",
	} {
		assert.Contains(t, logs, "field="+field, "expected a restart-only warning for %s", field)
	}
	assert.NotContains(t, logs, "secret", "the web token and metrics header values must never be logged")
	assert.NotContains(t, logs, "KONTORA_TG_", "notification credentials must never be logged")
	assert.NotContains(t, logs, "field=notifications.")
	assert.NotContains(t, logs, "field=max_concurrent_")
}

func TestReload_PinsSecondaryTicketsDirs(t *testing.T) {
	first, second := t.TempDir(), filepath.Join(t.TempDir(), "unavailable")
	for _, tc := range []struct {
		name    string
		before  []string
		after   []string
		warning bool
	}{
		{"defaults", nil, nil, false},
		{"empty equals default", nil, []string{}, false},
		{"add", nil, []string{first}, true},
		{"remove", []string{first}, nil, true},
		{"replace with unavailable", []string{first}, []string{second}, true},
		{"reorder", []string{first, second}, []string{second, first}, true},
		{"unchanged", []string{first, second}, []string{first, second}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReloadHarness(t)
			listYAML := func(dirs []string) string {
				encoded, err := json.Marshal(dirs)
				require.NoError(t, err)
				return "secondary_tickets_dirs: " + string(encoded) + "\n"
			}
			d := h.newDaemonWithConfig(t, h.yaml(configOpts{extra: listYAML(tc.before)}))
			before := d.config()
			h.writeConfig(t, h.yaml(configOpts{branchPrefix: "feature", extra: listYAML(tc.after)}))
			require.NoError(t, d.reloadConfig())
			got := d.config()
			assert.Equal(t, before.SecondaryTicketsDirs, got.SecondaryTicketsDirs)
			assert.Equal(t, "feature", got.BranchPrefix, "live fields still apply")
			if tc.warning {
				assert.Contains(t, h.logBuf.String(), "field=secondary_tickets_dirs")
			} else {
				assert.NotContains(t, h.logBuf.String(), "field=secondary_tickets_dirs")
			}
			if len(got.SecondaryTicketsDirs) > 0 {
				got.SecondaryTicketsDirs[0] = "changed snapshot"
				assert.Equal(t, tc.before, before.SecondaryTicketsDirs, "reload must not share the source slice with the previous snapshot")
			}
			_, err := os.Stat(second)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestReload_ValidatesSecondarySourcesAfterOverrides(t *testing.T) {
	for _, tc := range []struct {
		name        string
		env         string
		useFlag     bool
		flagAliases bool
		fileAliases bool
		duplicate   bool
	}{
		{name: "file alias", fileAliases: true},
		{name: "environment introduces alias", env: config.TicketsDirEnvVar},
		{name: "legacy environment introduces alias", env: config.LegacyTicketsDirEnvVar},
		{name: "flag introduces alias", useFlag: true, flagAliases: true},
		{name: "flag removes file and environment aliases", fileAliases: true, env: config.TicketsDirEnvVar, useFlag: true},
		{name: "duplicate retained secondary", duplicate: true},
		{name: "duplicate after environment alias", env: config.TicketsDirEnvVar, duplicate: true},
		{name: "duplicate after flag alias", useFlag: true, flagAliases: true, duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReloadHarness(t)
			secondary := t.TempDir()
			list := fmt.Sprintf("secondary_tickets_dirs: [%q]\n", secondary)
			flagPrimary := h.tasksDir
			var options []Option
			if tc.useFlag {
				options = append(options, WithConfigOverride(func(c *config.Config) {
					c.SetTicketsDir(flagPrimary)
				}))
			}
			d := h.newDaemonWithConfig(t, h.yaml(configOpts{extra: list}), options...)
			before := d.config()
			if tc.env != "" {
				t.Setenv(tc.env, secondary)
			}
			if tc.flagAliases {
				flagPrimary = secondary
			}
			candidateList := list
			if tc.duplicate {
				other := t.TempDir()
				candidateList = fmt.Sprintf("secondary_tickets_dirs: [%q, %q, %q, %q]\n", secondary, h.tasksDir, other, other)
			}
			changed := h.yaml(configOpts{branchPrefix: "feature", extra: candidateList})
			if tc.fileAliases {
				changed = strings.Replace(changed, "tickets_dir: "+h.tasksDir, "tickets_dir: "+secondary, 1)
			}
			h.writeConfig(t, changed)
			err := d.reloadConfig()
			if tc.duplicate {
				require.ErrorContains(t, err, "aliases secondary_tickets_dirs[2]")
				assert.ErrorContains(t, err, "secondary_tickets_dirs[3]")
				assert.Same(t, before, d.config(), "invalid sources must not publish any live changes")
			} else {
				require.NoError(t, err)
				got := d.config()
				assert.NotSame(t, before, got)
				assert.Equal(t, "feature", got.BranchPrefix)
				assert.Equal(t, before.TicketsDir, got.TicketsDir)
				assert.Equal(t, before.SecondaryTicketsDirs, got.SecondaryTicketsDirs)
			}
		})
	}
}

// TestReload_ReappliesConfigOverride covers `kontora start --address/--port`.
// The flags are applied in memory and never written to the file, so a reload
// must re-apply them. Without that, the reloaded config carries the file's own
// host and port, and pinRestartOnly warns on every reload about a change the
// operator did not make and a restart would not honour.
func TestReload_ReappliesConfigOverride(t *testing.T) {
	h := newReloadHarness(t)
	override := func(c *config.Config) {
		c.Web.Host = "127.0.0.1"
		c.Web.Port = 9000
	}
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}), WithConfigOverride(override))
	require.Equal(t, 9000, d.config().Web.Port, "the daemon starts on the flag's port")

	// An unrelated edit: the web block on disk is untouched.
	h.writeConfig(t, h.yaml(configOpts{branchPrefix: "feature"}))
	require.NoError(t, d.reloadConfig())

	got := d.config()
	assert.Equal(t, "feature", got.BranchPrefix, "live fields still apply")
	assert.Equal(t, "127.0.0.1", got.Web.Host)
	assert.Equal(t, 9000, got.Web.Port)
	assert.NotContains(t, h.logBuf.String(), "field=web.port",
		"a flag override is not an on-disk change and must not warn")
	assert.NotContains(t, h.logBuf.String(), "field=web.host",
		"a flag override is not an on-disk change and must not warn")
}

func TestReload_AppliesLiveFields(t *testing.T) {
	tests := []struct {
		name   string
		opts   configOpts
		verify func(t *testing.T, cfg *config.Config)
	}{
		{
			name: "statuses",
			opts: configOpts{statuses: "statuses:\n  - waiting_external\n"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.True(t, cfg.IsCustomStatus("waiting_external"))
			},
		},
		{
			name: "environment",
			opts: configOpts{environment: "environment:\n  FOO: bar\n"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "bar", cfg.Environment["FOO"])
			},
		},
		{
			name: "default_agent",
			opts: configOpts{defaultAgent: "agent2"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "agent2", cfg.DefaultAgent)
			},
		},
		{
			name: "branch_prefix",
			opts: configOpts{branchPrefix: "feature"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "feature", cfg.BranchPrefix)
			},
		},
		{
			name: "plannotator live fields",
			opts: configOpts{plannotator: "plannotator:\n  binary: /opt/plannotator\n  timeout: 5m\n"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "/opt/plannotator", cfg.Plannotator.Binary)
				assert.Equal(t, 5*time.Minute, cfg.Plannotator.Timeout.Duration)
			},
		},
		{
			name: "companion without assistant agent",
			opts: configOpts{extra: "assistant:\n  companion:\n    enabled: true\n    name: Miso\n"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, config.Companion{Enabled: true, Name: "Miso"}, cfg.Assistant.Companion)
				assert.False(t, cfg.Assistant.Enabled())
			},
		},
		{
			name: "pipeline stage list",
			opts: configOpts{pipelines: `  one-stage:
    - stage: step2
      agent: agent2
      on_success: done
      on_failure: pause
`},
			verify: func(t *testing.T, cfg *config.Config) {
				require.Len(t, cfg.Pipelines["one-stage"], 1)
				assert.Equal(t, "step2", cfg.Pipelines["one-stage"][0].Stage)
			},
		},
		{
			name: "agent definition",
			opts: configOpts{agent1Binary: "echo", agent1Args: "    args: [\"hello\"]\n"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "echo", cfg.Agents["agent1"].Binary)
				assert.Equal(t, []string{"hello"}, cfg.Agents["agent1"].Args)
			},
		},
		{
			name: "epic child pipeline",
			opts: configOpts{extra: "projects:\n  repo:\n    path: /tmp/reload-project\n    epic_child_pipeline: two-stage\n"},
			verify: func(t *testing.T, cfg *config.Config) {
				pipeline, _ := cfg.ResolveTicketDefaults("/tmp/reload-project", "", "", true)
				assert.Equal(t, "two-stage", pipeline)
			},
		},
		{
			name: "stage prompt",
			opts: configOpts{step1Prompt: "rewritten prompt"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "rewritten prompt", cfg.Stages["step1"].Prompt)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newReloadHarness(t)
			d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))

			h.writeConfig(t, h.yaml(tc.opts))
			require.NoError(t, d.reloadConfig())
			tc.verify(t, d.config())
		})
	}
}

func TestReload_EpicChildDefaultsPreserveAssignments(t *testing.T) {
	h := newReloadHarness(t)
	projectConfig := func(pipeline string) string {
		return h.yaml(configOpts{extra: fmt.Sprintf("projects:\n  repo:\n    path: %s\n    pipeline: one-stage\n    epic_child_pipeline: %s\n", h.repoDir, pipeline)})
	}
	d := h.newDaemonWithConfig(t, projectConfig("one-stage"))
	h.writeTicket("epc-parent.md", fmt.Sprintf("---\nid: epc-parent\nkind: epic\nstatus: open\npath: %s\n---\n# Parent\n", h.repoDir))
	require.NoError(t, d.initialScan(h.tasksDir))
	before, err := d.CreateTicket(web.CreateTicketRequest{Title: "Before reload", Parent: "epc-parent", Path: h.repoDir, Status: "open"})
	require.NoError(t, err)
	assert.Equal(t, "one-stage", before.Pipeline)
	beforeBytes, err := os.ReadFile(filepath.Join(h.tasksDir, before.ID+".md"))
	require.NoError(t, err)

	h.writeConfig(t, projectConfig("two-stage"))
	require.NoError(t, d.reloadConfig())
	after, err := d.CreateTicket(web.CreateTicketRequest{Title: "After reload", Parent: "epc-parent", Path: h.repoDir, Status: "open"})
	require.NoError(t, err)
	assert.Equal(t, "two-stage", after.Pipeline)
	stored := h.readTask(before.ID + ".md")
	assert.Equal(t, "one-stage", stored.Pipeline)
	afterBytes, err := os.ReadFile(filepath.Join(h.tasksDir, before.ID+".md"))
	require.NoError(t, err)
	assert.Equal(t, beforeBytes, afterBytes)
}

func TestReloadAppliesCheckpointCompactionThreshold(t *testing.T) {
	h := newReloadHarness(t)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))

	h.writeConfig(t, h.yaml(configOpts{
		agent1Binary: "pi",
		agent1Args:   "    checkpoint_compaction_tokens: 150000\n",
	}))
	require.NoError(t, d.reloadConfig())

	assert.Equal(t, 150000, d.config().Agents["agent1"].CheckpointCompactionTokens)
}

// TestReload_AppliesStageAndAgent runs a ticket after a reload and asserts the
// agent it spawns uses both the new prompt and the new agent definition.
func TestReload_AppliesStageAndAgent(t *testing.T) {
	h := newReloadHarness(t)

	var mu sync.Mutex
	var params []RunnerParams
	capturing := func(ctx context.Context, p RunnerParams) (process.Result, error) {
		mu.Lock()
		params = append(params, p)
		mu.Unlock()
		return DirectRunner(ctx, p)
	}

	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}), WithRunner(capturing))

	reloadedAgent := successfulAgentScript(t)
	h.writeConfig(t, h.yaml(configOpts{
		step1Prompt:  "REWRITTEN PROMPT for {{ .Ticket.ID }}",
		agent1Binary: reloadedAgent,
		agent1Args:   "    args: [\"--reloaded\"]\n",
	}))
	require.NoError(t, d.reloadConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-rel1.md", h.taskMD("tst-rel1", "todo", "one-stage"))
	h.waitForStatus("tst-rel1.md", ticket.StatusDone, 15*time.Second)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, params, 1)
	assert.Equal(t, reloadedAgent, params[0].Binary, "the next stage must use the reloaded binary")
	joined := strings.Join(params[0].Args, " ")
	assert.Contains(t, joined, "--reloaded", "the next stage must use the reloaded args")
	assert.Contains(t, joined, "REWRITTEN PROMPT", "the next stage must use the reloaded prompt")

	cancel()
	require.NoError(t, <-errCh)
}

// TestReload_UsesConsistentSnapshot reloads paired prompt/agent fixtures while
// stages run, and asserts no stage ever mixes the prompt of one fixture with
// the agent binary of the other.
func TestReload_UsesConsistentSnapshot(t *testing.T) {
	h := newReloadHarness(t)
	success := successfulAgentScript(t)

	fixture := func(tag string) string {
		return h.yaml(configOpts{
			step1Prompt:  tag + " prompt for {{ .Ticket.ID }}",
			agent1Binary: success,
			agent1Args:   fmt.Sprintf("    args: [%q]\n", tag),
		})
	}

	var mu sync.Mutex
	var mixed []string
	capturing := func(ctx context.Context, p RunnerParams) (process.Result, error) {
		joined := strings.Join(p.Args, " ")
		mu.Lock()
		switch {
		case strings.Contains(joined, "fixtureA") && !strings.Contains(joined, "fixtureA prompt"),
			strings.Contains(joined, "fixtureB") && !strings.Contains(joined, "fixtureB prompt"):
			mixed = append(mixed, joined)
		}
		mu.Unlock()
		return DirectRunner(ctx, p)
	}

	d := h.newDaemonWithConfig(t, fixture("fixtureA"), WithRunner(capturing))

	// Outlives the wait below, so a slow machine fails on the ticket that did
	// not finish rather than on the daemon that was stopped under it.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Flip the pair while tickets are being picked up. The loop is joined in a
	// cleanup rather than only at the end of the test: a wait below can fail the
	// test first, and a goroutine still writing files and logging after the test
	// returns panics the binary, taking every later test in the package with it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 20 {
			tag := "fixtureA"
			if i%2 == 1 {
				tag = "fixtureB"
			}
			if err := h.writeConfigAtomicErr(fixture(tag)); err != nil {
				t.Errorf("reload %d: writing the config failed: %v", i, err)
				return
			}
			if err := d.reloadConfig(); err != nil {
				t.Errorf("reload %d failed: %v", i, err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { <-done })

	for i := range 6 {
		id := fmt.Sprintf("tst-snap%d", i)
		h.writeTicket(id+".md", h.taskMD(id, "todo", "one-stage"))
	}
	// The six tickets run four at a time, so they finish as a group rather than
	// in the order waited on here. One deadline for the whole set: a budget per
	// ticket would give the last one to finish whatever is left of the first
	// one's, which on a loaded machine is nothing.
	deadline := time.Now().Add(60 * time.Second)
	for i := range 6 {
		h.waitForStatus(fmt.Sprintf("tst-snap%d.md", i), ticket.StatusDone, time.Until(deadline))
	}
	<-done

	mu.Lock()
	assert.Empty(t, mixed, "a stage must never mix a prompt and an agent from different config versions")
	mu.Unlock()

	cancel()
	require.NoError(t, <-errCh)
}

func TestReload_StrandedTicketPauses(t *testing.T) {
	tests := []struct {
		name       string
		reloadOpts configOpts
		wantError  string
	}{
		{
			name: "removed pipeline",
			reloadOpts: configOpts{pipelines: `  other:
    - stage: step2
      agent: agent2
      on_success: done
      on_failure: pause
`},
			wantError: `unknown pipeline "one-stage"`,
		},
		{
			name: "removed stage",
			reloadOpts: configOpts{pipelines: `  one-stage:
    - stage: step2
      agent: agent2
      on_success: done
      on_failure: pause
`},
			wantError: `stage "step1" not found in pipeline`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newReloadHarness(t)
			d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))

			// The ticket is on pipeline one-stage / stage step1 before the reload.
			h.writeTicket("tst-strand.md", `---
id: tst-strand
kontora: true
status: todo
pipeline: one-stage
stage: step1
path: `+h.repoDir+`
created: 2026-01-01T00:00:00Z
---
# Stranded ticket
`)

			h.writeConfig(t, h.yaml(tc.reloadOpts))
			require.NoError(t, d.reloadConfig())

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()

			result := h.waitForStatus("tst-strand.md", ticket.StatusPaused, 15*time.Second)
			assert.Contains(t, result.LastError, tc.wantError)

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

func TestReload_OnSIGHUP(t *testing.T) {
	h := newReloadHarness(t)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	h.writeConfig(t, h.yaml(configOpts{branchPrefix: "via-sighup"}))
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGHUP))

	waitForConfig(t, d, 10*time.Second, func(cfg *config.Config) bool {
		return cfg.BranchPrefix == "via-sighup"
	})

	cancel()
	require.NoError(t, <-errCh)
}

func TestReload_OnFileWrite(t *testing.T) {
	h := newReloadHarness(t)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	t.Run("in-place write", func(t *testing.T) {
		h.writeConfig(t, h.yaml(configOpts{branchPrefix: "written-in-place"}))
		waitForConfig(t, d, 10*time.Second, func(cfg *config.Config) bool {
			return cfg.BranchPrefix == "written-in-place"
		})
	})

	t.Run("atomic rename", func(t *testing.T) {
		require.NoError(t, atomicWriteFile(h.configPath, []byte(h.yaml(configOpts{branchPrefix: "renamed-over"})), 0o644))
		waitForConfig(t, d, 10*time.Second, func(cfg *config.Config) bool {
			return cfg.BranchPrefix == "renamed-over"
		})
	})

	t.Run("lock file and temp files trigger nothing", func(t *testing.T) {
		before := d.config()
		dir := filepath.Dir(h.configPath)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "lock"), []byte("1"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora-config-abc.tmp"), []byte("x"), 0o644))
		time.Sleep(500 * time.Millisecond)
		assert.Same(t, before, d.config(), "unrelated files in the config dir must not reload")
	})

	cancel()
	require.NoError(t, <-errCh)
}

// TestReload_OnSymlinkedConfigTargetWrite covers the dotfiles setup: the config
// path is a symlink into another directory and the edit reaches the target.
// The daemon watches both directories, so the write reloads on macOS (kqueue
// follows the link) and on Linux (the target's own directory is watched).
func TestReload_OnSymlinkedConfigTargetWrite(t *testing.T) {
	h := newReloadHarness(t)

	// Point the harness's config path at a symlink whose target lives in a
	// different directory, and start the daemon on the symlink. The harness
	// writes through the link (os.WriteFile follows it), so the target receives
	// the content and the link stays a link.
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "dotfiles-config.yaml")
	link := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(target, nil, 0o644))
	require.NoError(t, os.Symlink(target, link))
	h.configPath = link

	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// Edit through the target, the way an editor opening the dotfiles file does.
	require.NoError(t, os.WriteFile(target, []byte(h.yaml(configOpts{branchPrefix: "via-symlink"})), 0o644))
	waitForConfig(t, d, 10*time.Second, func(cfg *config.Config) bool {
		return cfg.BranchPrefix == "via-symlink"
	})

	// The symlink must survive: nothing in the reload path rewrites it.
	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the config symlink must still be a symlink")

	cancel()
	require.NoError(t, <-errCh)
}

func TestReload_SerializesConcurrentTriggers(t *testing.T) {
	h := newReloadHarness(t)

	// Every version tags three fields with the same string, so a snapshot that
	// mixes versions is visible as a disagreement between them.
	tagged := func(tag string) string {
		return h.yaml(configOpts{
			branchPrefix: tag,
			step1Prompt:  tag + " prompt",
			agent1Args:   fmt.Sprintf("    args: [%q]\n", tag),
		})
	}
	assertWholeSnapshot := func(cfg *config.Config) {
		t.Helper()
		assert.Equal(t, cfg.BranchPrefix+" prompt", cfg.Stages["step1"].Prompt,
			"published snapshot mixes config versions")
		require.Len(t, cfg.Agents["agent1"].Args, 1)
		assert.Equal(t, cfg.BranchPrefix, cfg.Agents["agent1"].Args[0],
			"published snapshot mixes config versions")
	}

	d := h.newDaemonWithConfig(t, tagged("initial"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// Overlap SIGHUP, direct calls, and file-watcher events on the same file.
	var wg sync.WaitGroup
	for i := range 10 {
		h.writeConfigAtomic(t, tagged(fmt.Sprintf("tag%d", i)))

		wg.Go(func() {
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGHUP)
		})
		wg.Go(func() {
			if err := d.reloadConfig(); err != nil {
				t.Errorf("direct reload failed: %v", err)
			}
		})

		assertWholeSnapshot(d.config())
	}
	wg.Wait()

	// Let the debounced watcher events arrive, then check the settled config.
	time.Sleep(500 * time.Millisecond)
	assertWholeSnapshot(d.config())
	assert.Equal(t, "tag9", d.config().BranchPrefix)

	cancel()
	require.NoError(t, <-errCh)
}

func TestConfigWatchDirs(t *testing.T) {
	t.Run("regular file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

		dirs, paths := configWatchDirs(path)
		require.Len(t, dirs, 1, "a config whose own path is not a symlink needs one watcher")
		assert.Equal(t, mustEvalDir(t, dir), mustEvalDir(t, dirs[0]))
		assert.Contains(t, paths, path)
	})

	t.Run("symlink to another directory", func(t *testing.T) {
		linkDir := t.TempDir()
		targetDir := t.TempDir()
		target := filepath.Join(targetDir, "real.yaml")
		require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))
		link := filepath.Join(linkDir, "config.yaml")
		require.NoError(t, os.Symlink(target, link))

		dirs, paths := configWatchDirs(link)
		require.Len(t, dirs, 2, "the symlink target's directory must also be watched")
		assert.Equal(t, filepath.Dir(link), dirs[0])
		assert.Len(t, paths, 2)
		assert.Contains(t, paths, link)
		resolved, err := filepath.EvalSymlinks(link)
		require.NoError(t, err)
		assert.Contains(t, paths, resolved)
	})

	t.Run("empty path", func(t *testing.T) {
		dirs, paths := configWatchDirs("")
		assert.Empty(t, dirs)
		assert.Empty(t, paths)
	})
}

func mustEvalDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return resolved
}

func waitForConfig(t *testing.T, d *Daemon, timeout time.Duration, pred func(*config.Config) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred(d.config()) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the config to reload (branch_prefix=%q)", d.config().BranchPrefix)
}

// TestReload_TicketsDirEnvBeatsFileEdit locks in the order: the environment is
// folded over the file before the restart-only pin, so both sides of the pin see
// the same value and editing tickets_dir on disk changes nothing and warns about
// nothing.
func TestReload_TicketsDirEnvBeatsFileEdit(t *testing.T) {
	h := newReloadHarness(t)
	envDir := t.TempDir()
	t.Setenv(config.TicketsDirEnvVar, envDir)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))
	require.Equal(t, envDir, d.config().TicketsDir, "the environment must win at startup")
	require.NoError(t, d.initialScan(d.config().TicketsDir))

	otherDir := t.TempDir()
	changed := strings.Replace(h.yaml(configOpts{}), "tickets_dir: "+h.tasksDir, "tickets_dir: "+otherDir, 1)
	h.writeConfig(t, changed)
	require.NoError(t, d.reloadConfig())

	assert.Equal(t, envDir, d.config().TicketsDir)
	assert.Equal(t, mustEvalDir(t, envDir), d.GetConfig().TicketsDir,
		"runtime identity must advertise the effective environment-selected store")
	assert.NotContains(t, h.logBuf.String(), "tickets_dir",
		"both sides of the pin see the environment's value, so there is nothing to warn about")
}

func TestReload_BroadcastsConfigReloaded(t *testing.T) {
	h := newReloadHarness(t)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))

	ch, unsub := d.Subscribe()
	defer unsub()

	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"a successful reload emits", h.yaml(configOpts{branchPrefix: "changed"}), false},
		{"a rejected one does not", h.yaml(configOpts{extra: "unknown_field: x\n"}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.writeConfig(t, tc.content)
			err := d.reloadConfig()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			var eventTypes []string
			for len(ch) > 0 {
				ev := <-ch
				eventTypes = append(eventTypes, ev.Type)
				assert.Empty(t, ev.Ticket.ID)
			}
			if tc.wantErr {
				assert.Empty(t, eventTypes, "a rejected reload must not broadcast")
			} else {
				assert.ElementsMatch(t, []string{"config_reloaded", "automations_updated"}, eventTypes)
			}
		})
	}
}

func TestReload_AutomationChangesBroadcastUpdates(t *testing.T) {
	for _, action := range []string{"pause", "delete", "rejected"} {
		t.Run(action, func(t *testing.T) {
			_, d, def, _ := automationFixture(t)
			before, err := os.ReadFile(d.configPath)
			require.NoError(t, err)
			spec := def.Spec
			spec.Enabled = false
			after, err := automationYAML(before, def.Key, &spec)
			require.NoError(t, err)
			switch action {
			case "delete":
				after, err = automationYAML(before, def.Key, nil)
				require.NoError(t, err)
			case "rejected":
				after = append(after, []byte("unknown_field: x\n")...)
			}
			require.NoError(t, os.WriteFile(d.configPath, after, 0o640))
			events, unsubscribe := d.Subscribe()
			defer unsubscribe()
			require.Empty(t, d.automationWake)

			err = d.reloadConfig()
			if action == "rejected" {
				require.Error(t, err)
				assert.Empty(t, events, "a rejected reload must not broadcast")
				assert.Empty(t, d.automationWake, "a rejected reload must not wake the scheduler")
				current, err := d.GetAutomation(def.ID)
				require.NoError(t, err)
				assert.Equal(t, def, current)
				return
			}
			require.NoError(t, err)
			require.Len(t, d.automationWake, 1, "a successful reload must wake the scheduler")
			var eventTypes []string
			for len(events) > 0 {
				event := <-events
				eventTypes = append(eventTypes, event.Type)
				assert.Empty(t, event.Ticket.ID)
			}
			assert.ElementsMatch(t, []string{"automations_updated", "config_reloaded"}, eventTypes)
			rows, err := d.ListAutomations()
			require.NoError(t, err)
			if action == "delete" {
				assert.Empty(t, rows)
			} else {
				require.Len(t, rows, 1)
				assert.False(t, rows[0].Enabled)
			}
		})
	}
}

// TestReload_ProjectCanNameANewChannel covers the two halves of a notification
// config: projects and channels both reload live. Adding a channel and pointing
// a project at it passes validation, and the running dispatcher can use the
// channel. The project's list shadows the default that was working.
func TestReload_ProjectCanNameANewChannel(t *testing.T) {
	h := newReloadHarness(t)
	before := `notifications:
  channels:
    tg:
      type: telegram
      secret_env: KONTORA_TG
      chat_id: "1"
  default: [tg]
`
	after := before + `projects:
  api:
    path: ` + h.repoDir + `
    notify_channels: [ops]
`
	after = strings.Replace(after, `  default: [tg]`, `    ops:
      type: webhook
      url: https://ops.example.invalid/ingest
  default: [tg]`, 1)

	d := h.newDaemonWithConfig(t, h.yaml(configOpts{extra: before}))
	h.writeConfig(t, h.yaml(configOpts{extra: after}))
	require.NoError(t, d.reloadConfig())

	assert.Equal(t, []string{"ops"}, d.config().Projects["api"].NotifyChannels,
		"projects reload live")
	logs := h.logBuf.String()
	assert.Contains(t, d.config().Notifications.Channels, "ops")
	assert.NotContains(t, logs, "silent until a restart")
}
