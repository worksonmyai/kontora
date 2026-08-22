package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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
`
	metricsAfter := `metrics:
  enabled: false
  endpoint: collector.example:4318
  insecure: false
  interval: 5s
  headers:
    authorization: rotated-secret
`
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{extra: metricsBefore}))
	before := d.config()

	otherDir := t.TempDir()
	changed := h.yaml(configOpts{extra: metricsAfter}) + "\n"
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
	assert.Equal(t, 4, got.MaxConcurrentAgents)
	assert.Equal(t, before.Web, got.Web, "the web block is pinned whole")
	assert.Equal(t, before.Metrics, got.Metrics, "the metrics block is pinned whole")
	assert.Equal(t, "localhost:4318", got.Metrics.Endpoint, "the running exporter's endpoint survives the reload")

	logs := h.logBuf.String()
	for _, field := range []string{
		"tickets_dir", "worktrees_dir", "logs_dir", "instance_name", "tmux_session",
		"max_concurrent_agents", "web.host", "web.port", "web.token", "web.allowed_hosts",
		"metrics.enabled", "metrics.endpoint", "metrics.headers", "metrics.interval", "metrics.insecure",
	} {
		assert.Contains(t, logs, "field="+field, "expected a restart-only warning for %s", field)
	}
	assert.NotContains(t, logs, "secret", "the web token and metrics header values must never be logged")
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
			name: "plannotator block",
			opts: configOpts{plannotator: "plannotator:\n  binary: /opt/plannotator\n  timeout: 5m\n  reviews_dir: /tmp/reviews\n"},
			verify: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "/opt/plannotator", cfg.Plannotator.Binary)
				assert.Equal(t, 5*time.Minute, cfg.Plannotator.Timeout.Duration)
				assert.Equal(t, "/tmp/reviews", cfg.Plannotator.ReviewsDir)
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

	h.writeConfig(t, h.yaml(configOpts{
		step1Prompt:  "REWRITTEN PROMPT for {{ .Ticket.ID }}",
		agent1Binary: "echo",
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
	assert.Equal(t, "echo", params[0].Binary, "the next stage must use the reloaded binary")
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

	fixture := func(tag string) string {
		return h.yaml(configOpts{
			step1Prompt:  tag + " prompt for {{ .Ticket.ID }}",
			agent1Binary: "echo",
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
			wantError: "evaluate pickup failed",
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
