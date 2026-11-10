package daemon

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
)

func frozenAutomationFixture(t *testing.T, workspace string) (*testHarness, *Daemon, automation.Definition, *config.Config) {
	t.Helper()
	h, d, definition, _ := automationFixture(t)
	cfg := *d.config()
	spec := definition.Spec
	spec.Workspace = workspace
	cfg.Automations = map[string]automation.Spec{definition.Key: spec}
	cfg.Agents = maps.Clone(cfg.Agents)
	require.NoError(t, d.automationStore.Reconcile(cfg.Automations, time.Now()))
	d.cfg.Store(&cfg)
	definition, err := d.automationStore.Get(definition.ID)
	require.NoError(t, err)
	return h, d, definition, &cfg
}

func freezeTestAutomation(t *testing.T, d *Daemon, definition automation.Definition, cfg *config.Config, request string) automation.Run {
	t.Helper()
	run, replay, err := d.automationStore.Allocate(definition.ID, "manual", request, time.Time{}, time.Time{}, time.Now())
	require.NoError(t, err)
	require.False(t, replay)
	run, err = d.freezeAutomationRun(run, cfg)
	require.NoError(t, err)
	return run
}

func executeFrozenAutomation(d *Daemon, run automation.Run) bool {
	ctx, cancel := context.WithCancel(context.Background())
	execution := &automationExecution{run: run, ctx: ctx, cancel: cancel}
	d.automationMu.Lock()
	d.automationExecutions[run.AutomationID] = execution
	d.automationMu.Unlock()
	return d.executeAutomation(execution)
}

func TestAutomationLaunchFreezesSelectedConfiguration(t *testing.T) {
	h, d, definition, cfg := frozenAutomationFixture(t, "repo")
	marker := filepath.Join(t.TempDir(), "hook")
	cfg.Environment = map[string]string{"LAUNCH_VALUE": "global", "ERASE_ME": "global", "KONTORA_RESULT_TICKET": "wrong-ticket", "KONTORA_OPERATOR_TOKEN": "wrong-credential"}
	cfg.Web.Token = "unrelated-web-secret"
	cfg.Agents[definition.Agent] = config.Agent{Binary: "pi", Args: []string{"--model", "claude-haiku-4-5", "--thinking", "low"}, Effort: "medium", Environment: map[string]string{"LAUNCH_VALUE": "selected", "ERASE_ME": ""}, FailurePatterns: []string{}}
	cfg.Agents["unselected"] = config.Agent{Binary: "pi", Environment: map[string]string{"SECRET": "unrelated-agent-secret"}}
	cfg.Hooks = config.Hooks{config.HookStageStart: {{Run: `printf '%s|%s|%s|%s|%s' "$LAUNCH_VALUE" "$KONTORA_AUTOMATION_ID" "$KONTORA_TICKET_ID" "$KONTORA_RESULT_FILE" "$KONTORA_STAGE" > ` + marker}}}
	cfg.Projects = map[string]config.Project{"frozen-project": {Path: h.repoDir, Hooks: config.Hooks{config.HookStageStart: {{Run: `printf '|project' >> ` + marker}}}}}
	d.agentLookup = func(string) (string, error) { return "/bin/true", nil }
	t.Setenv("KONTORA_RESULT_FILE", "/not/this/run")
	t.Setenv("KONTORA_MANAGED_NONCE", "inherited-credential")
	t.Setenv("KONTORA_CHECKPOINT_FILE", "inherited-checkpoint")
	run := freezeTestAutomation(t, d, definition, cfg, "frozen")
	launch, err := readAutomationLaunch(run)
	require.NoError(t, err)
	require.Equal(t, "claude-haiku-4-5", launch.Model)
	require.Equal(t, "medium", launch.Effort)
	require.Equal(t, "pi", launch.Agent.Binary)
	require.Equal(t, "/bin/true", launch.Executable)
	require.Equal(t, "frozen-project", launch.Project)
	require.Len(t, launch.Hooks[config.HookStageStart], 2)
	raw, err := os.ReadFile(filepath.Join(launch.Paths.Dir, "launch.json"))
	require.NoError(t, err)
	for _, secret := range []string{"unrelated-web-secret", "unrelated-agent-secret", "wrong-credential", "inherited-credential", "inherited-checkpoint"} {
		require.NotContains(t, string(raw), secret)
	}
	for _, path := range []string{launch.Paths.Dir, filepath.Join(launch.Paths.Dir, "launch.json"), launch.Paths.Capture, launch.Paths.Composed} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		mode := os.FileMode(0600)
		if info.IsDir() {
			mode = 0700
		}
		require.Equal(t, mode, info.Mode().Perm())
	}
	cfg.Environment["LAUNCH_VALUE"] = "changed"
	cfg.Agents[definition.Agent] = config.Agent{Binary: "not-the-accepted-agent", Environment: map[string]string{"LAUNCH_VALUE": "changed"}}
	cfg.Hooks[config.HookStageStart][0].Run = "exit 9"
	cfg.Projects = nil
	cfg.LogsDir = t.TempDir()
	d.agentLookup = func(string) (string, error) { return "", errors.New("execution must not look up an edited agent") }
	var captured RunnerParams
	d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
		captured = p
		return process.Result{Termination: process.TerminationProcessGroupGone}, os.WriteFile(p.Env["KONTORA_RESULT_FILE"], []byte(`{"outcome":"complete","summary":"frozen launch"}`), 0600)
	}
	require.True(t, executeFrozenAutomation(d, run))
	require.Equal(t, "/bin/true", captured.Binary)
	require.Equal(t, "selected", captured.Env["LAUNCH_VALUE"])
	require.Equal(t, "", captured.Env["ERASE_ME"])
	for _, key := range []string{"KONTORA_RESULT_TICKET", "KONTORA_TICKET_ID", "KONTORA_STAGE", "KONTORA_OPERATOR_TOKEN", "KONTORA_MANAGED_NONCE", "KONTORA_CHECKPOINT_FILE"} {
		require.Empty(t, captured.Env[key])
	}
	require.Equal(t, run.InvocationID, captured.Env["KONTORA_INVOCATION_ID"])
	require.Contains(t, strings.Join(captured.Args, " "), "--model claude-haiku-4-5 --thinking medium")
	require.NotEmpty(t, captured.SessionDir)
	hookOutput, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "selected|"+run.AutomationID+"||||project", string(hookOutput))
	saved, err := d.automationStore.GetRun(run.AutomationID, run.N)
	require.NoError(t, err)
	require.Equal(t, "complete", saved.Outcome)
	require.Equal(t, run.LogPath, saved.LogPath)
	_, err = os.Stat(filepath.Join(launch.Paths.Dir, "agent-extension.mjs"))
	require.NoError(t, err)
}

func TestAutomationLaunchMissingSnapshotFailsClosed(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(map[bool]string{false: "unfrozen allocation", true: "missing snapshot"}[frozen], func(t *testing.T) {
			_, d, definition, cfg := frozenAutomationFixture(t, "repo")
			run, _, err := d.automationStore.Allocate(definition.ID, "manual", "run", time.Time{}, time.Time{}, time.Now())
			require.NoError(t, err)
			if frozen {
				run, err = d.freezeAutomationRun(run, cfg)
				require.NoError(t, err)
				require.NoError(t, os.Remove(filepath.Join(filepath.Dir(run.ResultPath), "launch.json")))
			}
			called := false
			d.runner = func(context.Context, RunnerParams) (process.Result, error) {
				called = true
				return process.Result{}, nil
			}
			require.True(t, executeFrozenAutomation(d, run))
			require.False(t, called)
			saved, err := d.automationStore.GetRun(run.AutomationID, run.N)
			require.NoError(t, err)
			require.Equal(t, "failed", saved.Outcome)
			require.Contains(t, saved.Result, "frozen launch")
		})
	}
}

func TestAutomationLaunchAcceptanceFailureIsDurable(t *testing.T) {
	_, d, definition, cfg := frozenAutomationFixture(t, "repo")
	delete(cfg.Agents, definition.Agent)
	run, _, err := d.automationStore.Allocate(definition.ID, "manual", "request", time.Time{}, time.Time{}, time.Now())
	require.NoError(t, err)
	run, err = d.freezeAutomationRun(run, cfg)
	require.ErrorContains(t, err, "not configured")
	require.Equal(t, "terminal", run.Phase)
	require.Equal(t, "failed", run.Outcome)
	_, err = os.Stat(filepath.Join(filepath.Dir(run.ResultPath), "launch.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	replay, found, err := d.automationStore.Allocate(definition.ID, "manual", "request", time.Time{}, time.Time{}, time.Now())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, run, replay)
}

func TestAutomationLaunchSnapshotBindingAndPrivacy(t *testing.T) {
	_, d, definition, cfg := frozenAutomationFixture(t, "repo")
	run := freezeTestAutomation(t, d, definition, cfg, "first")
	path := filepath.Join(filepath.Dir(run.ResultPath), "launch.json")
	require.NoError(t, os.Chmod(path, 0644))
	_, err := readAutomationLaunch(run)
	require.ErrorContains(t, err, "private")
	require.NoError(t, os.Chmod(path, 0600))
	mismatch := run
	mismatch.InvocationID = "different-invocation"
	_, err = readAutomationLaunch(mismatch)
	require.ErrorContains(t, err, "identity")
	_, err = d.freezeAutomationRun(run, cfg)
	require.ErrorIs(t, err, automation.ErrConflict)
	second := freezeTestAutomation(t, d, definition, cfg, "second")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(second.ResultPath), "launch.json"), raw, 0600))
	_, err = readAutomationLaunch(second)
	require.ErrorContains(t, err, "identity")
}

func TestAutomationRepoLockCanonicalCheckoutAndCancellation(t *testing.T) {
	h, d, definition, cfg := frozenAutomationFixture(t, "repo")
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(h.repoDir, alias))
	otherSpec := definition.Spec
	otherSpec.Path = alias
	cfg.Automations["alias-report"] = otherSpec
	require.NoError(t, d.automationStore.Reconcile(cfg.Automations, time.Now()))
	rows, err := d.automationStore.List()
	require.NoError(t, err)
	var other automation.Definition
	for _, row := range rows {
		if row.Key == "alias-report" {
			other = row
		}
	}
	require.NotEmpty(t, other.ID)
	first := freezeTestAutomation(t, d, definition, cfg, "first")
	second := freezeTestAutomation(t, d, other, cfg, "second")
	a, err := readAutomationLaunch(first)
	require.NoError(t, err)
	b, err := readAutomationLaunch(second)
	require.NoError(t, err)
	require.Equal(t, a.Checkout, b.Checkout)
	require.NoError(t, d.acquireAutomationRepoLock(context.Background(), first, a))
	t.Cleanup(func() { d.releaseAutomationRepoLock(first); d.releaseAutomationRepoLock(second) })
	require.NoError(t, d.retainAutomationRepoLock(first))
	d.releaseAutomationRepoLock(second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, d.acquireAutomationRepoLock(ctx, second, b), context.DeadlineExceeded)
	d.releaseAutomationRepoLock(first)
	require.NoError(t, d.acquireAutomationRepoLock(context.Background(), second, b))
}
