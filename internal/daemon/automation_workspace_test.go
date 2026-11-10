package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
)

func TestAutomationWorkspacePrepareFailureCleanup(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "dirty"}[dirty], func(t *testing.T) {
			_, d, definition, cfg := frozenAutomationFixture(t, "worktree")
			command := "exit 1"
			if dirty {
				command = "printf preserve > untracked-result; exit 1"
			}
			cfg.Hooks = config.Hooks{config.HookStageStart: {{Run: command, OnFailure: config.HookOnFailurePause}}}
			run := freezeTestAutomation(t, d, definition, cfg, "failure")
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
			require.Contains(t, saved.Result, "exited with code 1")
			if dirty {
				data, err := os.ReadFile(filepath.Join(saved.Worktree, "untracked-result"))
				require.NoError(t, err)
				require.Equal(t, "preserve", string(data))
			} else {
				_, err := os.Stat(saved.Worktree)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestAutomationWorkspaceTimeoutIncludesHooks(t *testing.T) {
	_, d, definition, cfg := frozenAutomationFixture(t, "repo")
	spec := definition.Spec
	spec.Timeout = "100ms"
	cfg.Automations[definition.Key] = spec
	require.NoError(t, d.automationStore.Reconcile(cfg.Automations, time.Now()))
	definition, err := d.automationStore.Get(definition.ID)
	require.NoError(t, err)
	cfg.Hooks = config.Hooks{config.HookStageStart: {{Run: "sleep 5", Timeout: config.Duration{Duration: time.Minute}, OnFailure: config.HookOnFailureWarn}}}
	run := freezeTestAutomation(t, d, definition, cfg, "timeout")
	called := false
	d.runner = func(context.Context, RunnerParams) (process.Result, error) {
		called = true
		return process.Result{}, nil
	}
	started := time.Now()
	require.True(t, executeFrozenAutomation(d, run))
	require.False(t, called)
	require.Less(t, time.Since(started), 3*time.Second)
	saved, err := d.automationStore.GetRun(run.AutomationID, run.N)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.Outcome)
	require.Contains(t, saved.Result, "timeout after 100ms")
}

func TestAutomationWorkspaceUncancellableOperationIsUncertain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- automationWorkspaceOperation(ctx, func() error { close(started); <-finish; return nil })
	}()
	<-started
	cancel()
	err := <-done
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, process.ErrTerminationUncertain)
	close(finish)
}

func TestAutomationWorkspaceStageEndStopKeepsLogicalStop(t *testing.T) {
	_, d, definition, cfg := frozenAutomationFixture(t, "repo")
	marker := filepath.Join(t.TempDir(), "end-hook-started")
	cfg.Hooks = config.Hooks{config.HookStageEnd: {{Run: "touch " + marker + "; sleep 5", OnFailure: config.HookOnFailureWarn}}}
	run := freezeTestAutomation(t, d, definition, cfg, "stop")
	d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
		return process.Result{Termination: process.TerminationProcessGroupGone}, os.WriteFile(p.Env["KONTORA_RESULT_FILE"], []byte(`{"outcome":"complete","summary":"agent done"}`), 0600)
	}
	done := make(chan bool, 1)
	go func() { done <- executeFrozenAutomation(d, run) }()
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 5*time.Second, 10*time.Millisecond)
	pending, err := d.automationStore.GetRun(run.AutomationID, run.N)
	require.NoError(t, err)
	require.Equal(t, automationHookPending, pending.Termination)
	_, err = d.StopAutomation(run.AutomationID, run.N)
	require.NoError(t, err)
	select {
	case stopped := <-done:
		require.True(t, stopped)
	case <-time.After(5 * time.Second):
		t.Fatal("end hook ignored stop")
	}
	saved, err := d.automationStore.GetRun(run.AutomationID, run.N)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.Outcome)
	require.Equal(t, "stopped by user", saved.Result)
	require.Equal(t, 143, *saved.ExitCode)
	require.Equal(t, 0, *saved.ObservedExitCode)
}
