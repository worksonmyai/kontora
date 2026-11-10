package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/tmux"
)

func TestAutomationExecutionRejectsStaleResultAndLateCallbacks(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "late callbacks", true: "preexisting result"}[stale], func(t *testing.T) {
			_, d, definition, cfg := frozenAutomationFixture(t, "repo")
			run := freezeTestAutomation(t, d, definition, cfg, "run")
			if stale {
				require.NoError(t, os.WriteFile(run.ResultPath, []byte(`{"outcome":"complete","summary":"stale"}`), 0600))
			}
			var params RunnerParams
			d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
				params = p
				if p.OnReady != nil {
					p.OnReady()
				}
				return process.Result{Termination: process.TerminationProcessGroupGone}, os.WriteFile(p.Env["KONTORA_RESULT_FILE"], []byte(`{"outcome":"complete","summary":"current"}`), 0600)
			}
			require.True(t, executeFrozenAutomation(d, run))
			saved, err := d.automationStore.GetRun(run.AutomationID, run.N)
			require.NoError(t, err)
			if stale {
				require.Empty(t, params.Binary)
				require.Equal(t, "failed", saved.Outcome)
				require.Contains(t, saved.Result, "already exists")
				return
			}
			require.Equal(t, "complete", saved.Outcome)
			params.OnReady()
			require.ErrorIs(t, params.OnWindowCreated(tmux.WindowIdentity{WindowID: "@999"}), automation.ErrConflict)
			after, err := d.automationStore.GetRun(run.AutomationID, run.N)
			require.NoError(t, err)
			require.Equal(t, saved, after)
			_, err = os.Stat(filepath.Join(filepath.Dir(run.ResultPath), "terminal.json"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestAutomationExecutionUsesFrozenFailurePatternsOnRawCapture(t *testing.T) {
	_, d, definition, cfg := frozenAutomationFixture(t, "repo")
	agent := cfg.Agents[definition.Agent]
	agent.FailurePatterns = []string{"ACCEPTED_PATTERN"}
	cfg.Agents[definition.Agent] = agent
	run := freezeTestAutomation(t, d, definition, cfg, "run")
	agent.FailurePatterns = []string{"EDITED_PATTERN"}
	cfg.Agents[definition.Agent] = agent
	d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
		if err := os.WriteFile(p.LogFile, []byte("ACCEPTED_PATTERN"), 0o600); err != nil {
			return process.Result{Termination: process.TerminationNotStarted}, err
		}
		return process.Result{Termination: process.TerminationProcessGroupGone}, os.WriteFile(p.Env["KONTORA_RESULT_FILE"], []byte(`{"outcome":"complete","summary":"not authoritative"}`), 0o600)
	}
	require.True(t, executeFrozenAutomation(d, run))
	saved, err := d.automationStore.GetRun(run.AutomationID, run.N)
	require.NoError(t, err)
	require.Equal(t, "failed", saved.Outcome)
	require.Contains(t, saved.Result, "ACCEPTED_PATTERN")
}

func TestAutomationExecutionLateStopOverridesSuccessfulResult(t *testing.T) {
	_, d, definition, cfg := frozenAutomationFixture(t, "repo")
	run := freezeTestAutomation(t, d, definition, cfg, "run")
	var stopErr error
	d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
		_, stopErr = d.StopAutomation(run.AutomationID, run.N)
		p.OnReady()
		err := os.WriteFile(p.Env["KONTORA_RESULT_FILE"], []byte(`{"outcome":"complete","summary":"too late"}`), 0600)
		return process.Result{ExitCode: 0, Termination: process.TerminationProcessGroupGone}, err
	}
	require.True(t, executeFrozenAutomation(d, run))
	require.NoError(t, stopErr)
	saved, err := d.automationStore.GetRun(run.AutomationID, run.N)
	require.NoError(t, err)
	require.Equal(t, "terminal", saved.Phase)
	require.Equal(t, "failed", saved.Outcome)
	require.Equal(t, "stopped by user", saved.Result)
	require.Equal(t, 143, *saved.ExitCode)
	require.Equal(t, 0, *saved.ObservedExitCode)
}

func TestAutomationExecutionRetainsRepoLeaseAfterUncertainTermination(t *testing.T) {
	_, d, definition, cfg := frozenAutomationFixture(t, "repo")
	first := freezeTestAutomation(t, d, definition, cfg, "first")
	launch, err := readAutomationLaunch(first)
	require.NoError(t, err)
	d.runner = func(context.Context, RunnerParams) (process.Result, error) {
		return process.Result{}, process.ErrTerminationUncertain
	}
	require.False(t, executeFrozenAutomation(d, first))
	saved, err := d.automationStore.GetRun(first.AutomationID, first.N)
	require.NoError(t, err)
	require.Equal(t, "recovery_required", saved.Phase)
	require.Empty(t, saved.Outcome)
	require.True(t, saved.CompletedAt.IsZero())
	require.NoError(t, d.retainAutomationRepoLock(saved))
	t.Cleanup(func() { d.releaseAutomationRepoLock(first) })
	second := first
	second.InvocationID = "other-invocation"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, d.acquireAutomationRepoLock(ctx, second, launch), context.DeadlineExceeded)
	d.releaseAutomationRepoLock(first)
	require.NoError(t, d.acquireAutomationRepoLock(context.Background(), second, launch))
	d.releaseAutomationRepoLock(second)
}

func TestAutomationResultRequiresExplicitCurrentSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	run := automation.Run{ResultPath: path, Spec: automation.Spec{Timeout: "1m"}}
	cases := []struct {
		name, payload   string
		result          process.Result
		runnerErr       error
		outcome, reason string
	}{
		{name: "missing", result: process.Result{Termination: process.TerminationProcessGroupGone}, outcome: "failed", reason: "result protocol"},
		{name: "malformed", payload: `{"outcome":"complete"}`, result: process.Result{Termination: process.TerminationProcessGroupGone}, outcome: "failed", reason: "result protocol"},
		{name: "blocked", payload: `{"outcome":"blocked","summary":"Need access","next_step":"Grant access"}`, result: process.Result{Termination: process.TerminationProcessGroupGone}, outcome: "failed", reason: "Grant access"},
		{name: "interrupted zero", payload: `{"outcome":"complete","summary":"done"}`, result: process.Result{Termination: process.TerminationProcessGroupGone, Interruption: context.DeadlineExceeded}, outcome: "failed", reason: "timeout after 1m"},
		{name: "not started", payload: `{"outcome":"complete","summary":"done"}`, result: process.Result{Termination: process.TerminationNotStarted}, outcome: "failed", reason: "did not start"},
		{name: "nonzero", payload: `{"outcome":"complete","summary":"done"}`, result: process.Result{ExitCode: 7, Termination: process.TerminationProcessGroupGone}, outcome: "failed", reason: "code 7"},
		{name: "runner error", payload: `{"outcome":"complete","summary":"done"}`, runnerErr: errors.New("launch failed"), outcome: "failed", reason: "launch failed"},
		{name: "complete", payload: `{"outcome":"complete","summary":"done"}`, result: process.Result{Termination: process.TerminationProcessGroupGone}, outcome: "complete", reason: "done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.payload != "" {
				require.NoError(t, os.WriteFile(path, []byte(tc.payload), 0600))
			}
			outcome, reason := automationResult(context.Background(), run, tc.result, tc.runnerErr)
			require.Equal(t, tc.outcome, outcome)
			require.Contains(t, reason, tc.reason)
		})
	}
}

func TestAutomationPricingPreservesUnknownAndUsesEffectiveModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	require.Nil(t, automationRunCost(path, "claude-haiku-4-5"))
	require.NoError(t, os.WriteFile(path, []byte(completeCostSidecar("", 1_000_000, 0, 0, 0)), 0600))
	require.Equal(t, "1.000000", *automationRunCost(path, "claude-haiku-4-5"))
	require.NoError(t, os.WriteFile(path, []byte(completeCostSidecar("claude-sonnet-4-6", 1_000_000, 0, 0, 0)), 0600))
	require.Equal(t, "3.000000", *automationRunCost(path, "claude-haiku-4-5"))
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"totals":{"input":1,"output":0,"cache_create":0,"cache_read":0},"partial":["usage"],"events":[]}`), 0600))
	require.Nil(t, automationRunCost(path, "claude-haiku-4-5"))
}

func TestAutomationLastRunUsesPreviousAttemptAndLocalDate(t *testing.T) {
	_, d, definition, cfg := frozenAutomationFixture(t, "repo")
	spec := definition.Spec
	spec.Timezone = "America/Los_Angeles"
	spec.Prompt = `{{ .Run.Date }}|{{ .LastRun.Number }}|{{ .LastRun.Result }}|{{ .Automation.Prompt }}`
	cfg.Automations[definition.Key] = spec
	require.NoError(t, d.automationStore.Reconcile(cfg.Automations, time.Now()))
	definition, err := d.automationStore.Get(definition.ID)
	require.NoError(t, err)
	previous, _, err := d.automationStore.Allocate(definition.ID, "manual", "previous", time.Time{}, time.Time{}, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	_, err = d.automationStore.UpdateRun(definition.ID, previous.N, func(r *automation.Run) error {
		r.Phase = "terminal"
		r.Outcome = "complete"
		r.StartedAt = r.AcceptedAt
		r.CompletedAt = time.Now()
		r.Result = "prior attempt"
		return nil
	})
	require.NoError(t, err)
	skipped, _, err := d.automationStore.Allocate(definition.ID, "manual", "skipped", time.Time{}, time.Time{}, time.Now())
	require.NoError(t, err)
	_, err = d.skipAutomationLocked(skipped, "overlap")
	require.NoError(t, err)
	run := freezeTestAutomation(t, d, definition, cfg, "current")
	run.StartedAt = time.Date(2025, 1, 2, 1, 0, 0, 0, time.UTC)
	run.Worktree = t.TempDir()
	rendered, err := d.renderAutomationPrompt(run)
	require.NoError(t, err)
	require.Equal(t, "2025-01-01|1|prior attempt|"+spec.Prompt, rendered)
}
