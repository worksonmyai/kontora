package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"gopkg.in/yaml.v3"
)

func automationFixture(t *testing.T) (*testHarness, *Daemon, automation.Definition, time.Time) {
	t.Helper()
	h := newHarness(t)
	for _, key := range []string{"KONTORA_TICKETS_DIR", "TICKETS_DIR"} {
		t.Setenv(key, "")
	}
	h.cfg.AutoPickUp = new(false)
	h.cfg.TmuxSession = fmt.Sprintf("auto-test-%x", sha256.Sum256([]byte(t.Name())))[:50]
	h.cfg.Web.Enabled = new(false)
	h.cfg.Automations = map[string]automation.Spec{"daily-report": {
		Name: "Daily report", Prompt: "Inspect the repository.", Path: h.repoDir, Agent: "agent1", Schedule: "1m", Enabled: true,
	}}
	path := filepath.Join(t.TempDir(), "config.yaml")
	b, err := yaml.Marshal(h.cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append([]byte("# operator configuration\n"), b...), 0o640))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	h.cfg = cfg
	d := h.newDaemon(cfg, WithConfigPath(path))
	closeStore := d.startAutomations(h.tasksDir)
	t.Cleanup(closeStore)
	require.NoError(t, d.automationErr)
	require.NoError(t, d.automationStore.Reconcile(cfg.Automations, time.Now()))
	rows, err := d.ListAutomations()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return h, d, rows[0], time.Now().UTC().Truncate(time.Second)
}

func automationClose(t *testing.T, d *Daemon) {
	t.Helper()
	d.automationMu.Lock()
	defer d.automationMu.Unlock()
	for _, execution := range d.automationExecutions {
		execution.cancel()
	}
	if d.automationStore != nil {
		require.NoError(t, d.automationStore.Close())
		d.automationStore = nil
	}
	if d.automationLogLock != nil {
		require.NoError(t, d.automationLogLock.Close())
		d.automationLogLock = nil
	}
}

func automationAssertNoTickets(t *testing.T, h *testHarness) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(h.tasksDir, "*.md"))
	require.NoError(t, err)
	assert.Empty(t, files)
}

// Run one dispatch job without a tmux server or the daemon's background scheduler.
func automationTakeJob(t *testing.T, d *Daemon) *automationDispatchJob {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	require.NotEmpty(t, d.automationJobs)
	job := d.automationJobs[0]
	d.automationJobs = d.automationJobs[1:]
	return job
}

func TestAutomationOverlap(t *testing.T) {
	for _, trigger := range []string{"manual", "schedule"} {
		for _, overlap := range []string{"skip", "queue", "replace"} {
			t.Run(trigger+"/"+overlap, func(t *testing.T) {
				h, d, def, now := automationFixture(t)
				spec := def.Spec
				spec.Overlap = overlap
				def, err := d.UpdateAutomation(def.ID, def.Revision, spec)
				require.NoError(t, err)
				first, err := d.RunAutomation(def.ID, "first")
				require.NoError(t, err)
				var second automation.Run
				if trigger == "manual" {
					second, err = d.RunAutomation(def.ID, "second")
				} else {
					now = def.Next
					require.NoError(t, d.automationTick(now))
					runs, e := d.AutomationRuns(def.ID)
					require.NoError(t, e)
					require.Len(t, runs, 2)
					second = runs[0]
				}
				require.NoError(t, err)
				assert.Equal(t, trigger, second.Trigger)
				assert.Equal(t, uint64(2), second.N)
				assert.Equal(t, spec, second.Spec)
				saved, err := d.GetAutomation(def.ID)
				require.NoError(t, err)
				assert.Equal(t, def.Revision, saved.Revision, "cursor and admission must not change the editable revision")
				if trigger == "schedule" {
					assert.Equal(t, now, second.ScheduledFor)
					assert.True(t, saved.Next.After(now))
					require.NoError(t, d.automationTick(now))
					runs, e := d.AutomationRuns(def.ID)
					require.NoError(t, e)
					require.Len(t, runs, 2)
				}
				first, err = d.automationStore.GetRun(def.ID, first.N)
				require.NoError(t, err)
				switch overlap {
				case "skip":
					assert.Equal(t, "terminal", second.Phase)
					assert.Equal(t, "skipped", second.Outcome)
					assert.Equal(t, "queued", first.Phase)
				case "queue":
					assert.Equal(t, "queued", second.Phase)
					assert.Equal(t, "queued", first.Phase)
				case "replace":
					assert.Equal(t, "queued", second.Phase)
					assert.Equal(t, "stopping", first.Phase)
					assert.Contains(t, first.StopReason, "replaced")
				}
				automationAssertNoTickets(t, h)
			})
		}
	}
}

func TestAutomationOverlapQueueBounds(t *testing.T) {
	for _, overlap := range []string{"queue", "replace"} {
		t.Run(overlap, func(t *testing.T) {
			_, d, def, now := automationFixture(t)
			spec := def.Spec
			spec.Overlap = overlap
			def, err := d.UpdateAutomation(def.ID, def.Revision, spec)
			require.NoError(t, err)
			first, err := d.RunAutomation(def.ID, "first")
			require.NoError(t, err)
			second, err := d.RunAutomation(def.ID, "second")
			require.NoError(t, err)
			third, err := d.RunAutomation(def.ID, "third")
			require.NoError(t, err)
			runs, err := d.AutomationRuns(def.ID)
			require.NoError(t, err)
			require.Len(t, runs, 3)
			if overlap == "queue" {
				assert.Equal(t, "terminal", third.Phase)
				assert.Equal(t, "skipped", third.Outcome)
				assert.Contains(t, third.Result, "queue is full")
			} else {
				second, err = d.automationStore.GetRun(def.ID, second.N)
				require.NoError(t, err)
				assert.Equal(t, "terminal", second.Phase)
				assert.Equal(t, "skipped", second.Outcome)
				assert.Contains(t, second.Result, "superseded")
				second = third
			}
			// The pending run cannot start until cancellation of the first run is confirmed.
			require.NoError(t, d.automationTick(now))
			assert.Len(t, d.automationJobs, 1)
			_, err = d.StopAutomation(def.ID, first.N)
			require.NoError(t, err)
			assert.True(t, automationTakeJob(t, d).run(context.Background()))
			first, err = d.automationStore.GetRun(def.ID, first.N)
			require.NoError(t, err)
			assert.Equal(t, "terminal", first.Phase)
			require.NoError(t, d.automationTick(now))
			require.Len(t, d.automationJobs, 1)
			assert.Equal(t, second.N, d.automationExecutions[def.ID].run.N)
		})
	}
}

func TestAutomationManualReceiptsAndFrozenSpec(t *testing.T) {
	h, d, def, _ := automationFixture(t)
	first, err := d.RunAutomation(def.ID, "same-request")
	require.NoError(t, err)
	replay, err := d.RunAutomation(def.ID, "same-request")
	require.NoError(t, err)
	assert.Equal(t, first, replay)
	assert.Len(t, d.automationJobs, 1)
	spec := def.Spec
	spec.Prompt = "New instructions"
	spec.Enabled = false
	def, err = d.UpdateAutomation(def.ID, def.Revision, spec)
	require.NoError(t, err)
	replay, err = d.RunAutomation(def.ID, "same-request")
	require.NoError(t, err)
	assert.Equal(t, first, replay)
	assert.NotEqual(t, spec, replay.Spec)
	next, err := d.RunAutomation(def.ID, "different-request")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), next.N)
	assert.Equal(t, spec, next.Spec)
	_, err = d.StopAutomation(def.ID, first.N)
	require.NoError(t, err)
	assert.True(t, automationTakeJob(t, d).run(context.Background()))
	automationClose(t, d)
	closeStore := d.startAutomations(h.tasksDir)
	defer closeStore()
	replay, err = d.RunAutomation(def.ID, "same-request")
	require.NoError(t, err)
	assert.Equal(t, first.N, replay.N)
	assert.Equal(t, first.InvocationID, replay.InvocationID)
	assert.Equal(t, "terminal", replay.Phase)
	runs, err := d.AutomationRuns(def.ID)
	require.NoError(t, err)
	assert.Len(t, runs, 2)
	automationAssertNoTickets(t, h)
}

func TestAutomationScheduleDisposition(t *testing.T) {
	for _, tc := range []struct {
		name           string
		enabled        bool
		age            time.Duration
		phase, outcome string
		count          int
	}{
		{"due", true, 0, "queued", "", 1},
		{"recent slot", true, 30 * time.Second, "queued", "", 1},
		{"missed slot", true, 10 * time.Minute, "terminal", "skipped", 1},
		{"paused", false, 0, "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d, def, _ := automationFixture(t)
			if !tc.enabled {
				spec := def.Spec
				spec.Enabled = false
				var err error
				def, err = d.UpdateAutomation(def.ID, def.Revision, spec)
				require.NoError(t, err)
			}
			slot := def.Next
			now := slot.Add(tc.age)
			require.NoError(t, d.automationTick(now))
			require.NoError(t, d.automationTick(now))
			runs, err := d.AutomationRuns(def.ID)
			require.NoError(t, err)
			require.Len(t, runs, tc.count)
			saved, err := d.GetAutomation(def.ID)
			require.NoError(t, err)
			assert.Equal(t, def.Revision, saved.Revision)
			if tc.count > 0 {
				assert.Equal(t, tc.phase, runs[0].Phase)
				assert.Equal(t, tc.outcome, runs[0].Outcome)
				assert.Equal(t, slot, runs[0].ScheduledFor)
				assert.Equal(t, slot.Add(tc.age.Truncate(time.Minute)+time.Minute), saved.Next)
			}
			automationAssertNoTickets(t, h)
		})
	}
}

func TestAutomationStopBeforeLaunch(t *testing.T) {
	h, d, def, _ := automationFixture(t)
	first, err := d.RunAutomation(def.ID, "run")
	require.NoError(t, err)
	stopped, err := d.StopAutomation(def.ID, first.N)
	require.NoError(t, err)
	assert.Equal(t, "stopping", stopped.Phase)
	assert.True(t, automationTakeJob(t, d).run(context.Background()))
	stopped, err = d.StopAutomation(def.ID, first.N)
	require.NoError(t, err)
	assert.Equal(t, "terminal", stopped.Phase)
	assert.Equal(t, "skipped", stopped.Outcome)
	_, err = d.StopAutomation(def.ID, 999)
	require.ErrorIs(t, err, automation.ErrNotFound)
	automationAssertNoTickets(t, h)
}

func TestAutomationExecution(t *testing.T) {
	for _, tc := range []struct {
		name, script, outcome, reason string
		exit                          int
	}{
		{"complete", `printf '%s' '{"outcome":"complete","summary":"Report ready"}' > "$KONTORA_RESULT_FILE"; echo agent-output`, "complete", "Report ready", 0},
		{"blocked", `printf '%s' '{"outcome":"blocked","summary":"Need access","next_step":"Grant access"}' > "$KONTORA_RESULT_FILE"`, "failed", "Grant access", 0},
		{"missing result", `true`, "failed", "result protocol", 0},
		{"invalid result", `printf '%s' '{"outcome":"complete"}' > "$KONTORA_RESULT_FILE"`, "failed", "result protocol", 0},
		{"nonzero exit", `exit 7`, "failed", "exited with code 7", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d, def, _ := automationFixture(t)
			cfg := *d.config()
			cfg.Agents = map[string]config.Agent{"agent1": {Binary: "/bin/sh", Args: []string{"-c", tc.script}}}
			d.cfg.Store(&cfg)
			run, err := d.RunAutomation(def.ID, "run")
			require.NoError(t, err)
			assert.True(t, automationTakeJob(t, d).run(context.Background()))
			run, err = d.automationStore.GetRun(def.ID, run.N)
			require.NoError(t, err)
			assert.Equal(t, "terminal", run.Phase)
			assert.Equal(t, tc.outcome, run.Outcome)
			assert.Contains(t, run.Result, tc.reason)
			require.NotNil(t, run.ExitCode)
			assert.Equal(t, tc.exit, *run.ExitCode)
			require.NotNil(t, run.ObservedExitCode)
			assert.Equal(t, tc.exit, *run.ObservedExitCode)
			assert.Nil(t, run.Cost, "missing usage must not appear as zero spend")
			assert.False(t, run.StartedAt.IsZero())
			assert.False(t, run.CompletedAt.IsZero())
			assert.NotEmpty(t, run.BaseCommit)
			assert.Equal(t, fmt.Sprintf("auto/%s-%d", def.Key, run.N), run.Branch)
			lines, raw, err := d.AutomationLog(def.ID, run.N)
			require.NoError(t, err)
			assert.NotEmpty(t, lines)
			assert.Contains(t, raw, tc.reason)
			if tc.name == "complete" {
				assert.Contains(t, raw, "agent-output")
			}
			require.NotEmpty(t, run.LogPath)
			_, err = os.Stat(run.LogPath)
			require.NoError(t, err)
			automationAssertNoTickets(t, h)
		})
	}
}

func TestAutomationExecutionCancellation(t *testing.T) {
	for _, reason := range []string{"stop", "timeout"} {
		t.Run(reason, func(t *testing.T) {
			h, d, def, _ := automationFixture(t)
			if reason == "timeout" {
				spec := def.Spec
				spec.Timeout = "1s"
				spec.Workspace = "repo"
				var err error
				def, err = d.UpdateAutomation(def.ID, def.Revision, spec)
				require.NoError(t, err)
			}
			ready := make(chan RunnerParams, 1)
			d.runner = func(ctx context.Context, p RunnerParams) (process.Result, error) {
				if p.OnReady != nil {
					p.OnReady()
				}
				ready <- p
				<-ctx.Done()
				return process.Result{ExitCode: 143, Termination: process.TerminationProcessGroupGone}, ctx.Err()
			}
			run, err := d.RunAutomation(def.ID, "run")
			require.NoError(t, err)
			done := make(chan bool, 1)
			job := automationTakeJob(t, d)
			go func() { done <- job.run(context.Background()) }()
			var p RunnerParams
			select {
			case p = <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("runner did not start")
			}
			assert.True(t, p.RequireTermination)
			assert.True(t, p.StableWindowIdentity)
			if reason == "stop" {
				stopped, e := d.StopAutomation(def.ID, run.N)
				require.NoError(t, e)
				assert.Equal(t, "stopping", stopped.Phase)
			}
			select {
			case stopped := <-done:
				assert.True(t, stopped)
			case <-time.After(5 * time.Second):
				t.Fatal("automation did not cancel")
			}
			run, err = d.automationStore.GetRun(def.ID, run.N)
			require.NoError(t, err)
			assert.Equal(t, "terminal", run.Phase)
			assert.Equal(t, "failed", run.Outcome)
			if reason == "stop" {
				assert.Contains(t, run.Result, "stopped by user")
			} else {
				assert.Contains(t, run.Result, "timeout after 1s")
			}
			automationAssertNoTickets(t, h)
		})
	}
}

func TestAutomationPromptContext(t *testing.T) {
	h, d, def, _ := automationFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.repoDir, "context.txt"), []byte("repository context"), 0o600))
	previous, _, err := d.automationStore.Allocate(def.ID, "manual", "previous", time.Time{}, time.Time{}, time.Now().Add(-2*time.Minute))
	require.NoError(t, err)
	_, err = d.automationStore.UpdateRun(def.ID, previous.N, func(r *automation.Run) error {
		r.StartedAt = time.Now().Add(-time.Minute)
		r.CompletedAt = time.Now()
		r.Phase = "terminal"
		r.Outcome = "complete"
		r.Result = "Previous report"
		return nil
	})
	require.NoError(t, err)
	skipped, _, err := d.automationStore.Allocate(def.ID, "manual", "skipped", time.Time{}, time.Time{}, time.Now())
	require.NoError(t, err)
	_, err = d.skipAutomationLocked(skipped, "overlap")
	require.NoError(t, err)
	spec := def.Spec
	spec.Prompt = `{{ .Automation.Key }}|{{ .Run.Number }}|{{ .Run.Trigger }}|{{ .Run.Branch }}|{{ .LastRun.Result }}|{{ file "context.txt" }}`
	spec.Workspace = "repo"
	def, err = d.UpdateAutomation(def.ID, def.Revision, spec)
	require.NoError(t, err)
	captured := make(chan RunnerParams, 1)
	d.runner = func(_ context.Context, p RunnerParams) (process.Result, error) {
		captured <- p
		return process.Result{ExitCode: 0, Termination: process.TerminationProcessGroupGone}, nil
	}
	run, err := d.RunAutomation(def.ID, "current")
	require.NoError(t, err)
	assert.True(t, automationTakeJob(t, d).run(context.Background()))
	p := <-captured
	require.NotEmpty(t, p.Args)
	saved, err := d.automationStore.GetRun(def.ID, run.N)
	require.NoError(t, err)
	assert.Contains(t, p.Args[len(p.Args)-1], fmt.Sprintf("daily-report|3|manual|%s|Previous report|repository context", saved.Branch))
	assert.Contains(t, p.Args[len(p.Args)-1], "kontora result --input-file")
	assert.Contains(t, p.Args[len(p.Args)-1], def.ID)
	repo, err := filepath.EvalSymlinks(h.repoDir)
	require.NoError(t, err)
	assert.Equal(t, repo, p.Dir)
	assert.Equal(t, def.ID, p.Env["KONTORA_AUTOMATION_ID"])
	assert.Equal(t, "3", p.Env["KONTORA_RUN_NUMBER"])
	assert.Equal(t, def.ID, p.Env["KONTORA_RESULT_AUTOMATION"])
	assert.Equal(t, "3", p.Env["KONTORA_RESULT_RUN_NUMBER"])
	assert.Empty(t, p.Env["KONTORA_TICKET_ID"])
	assert.Empty(t, p.Env["KONTORA_RESULT_TICKET"])
	assert.Empty(t, p.Env["KONTORA_RESULT_STAGE"])
	assert.Equal(t, run.InvocationID, p.Env["KONTORA_INVOCATION_ID"])
	automationAssertNoTickets(t, h)
}

func TestAutomationPromptRenderFailure(t *testing.T) {
	for _, tc := range []struct{ name, template string }{{"ticket", `{{ .Ticket.ID }}`}, {"file", `{{ file "missing.txt" }}`}} {
		t.Run(tc.name, func(t *testing.T) {
			h, d, def, _ := automationFixture(t)
			input := def.Spec
			input.Prompt = tc.template
			input.Workspace = "repo"
			def, err := d.UpdateAutomation(def.ID, def.Revision, input)
			require.NoError(t, err)
			called := false
			d.runner = func(context.Context, RunnerParams) (process.Result, error) {
				called = true
				return process.Result{}, nil
			}
			run, err := d.RunAutomation(def.ID, "run")
			require.NoError(t, err)
			assert.True(t, automationTakeJob(t, d).run(context.Background()))
			run, err = d.automationStore.GetRun(def.ID, run.N)
			require.NoError(t, err)
			assert.Equal(t, "terminal", run.Phase)
			assert.Equal(t, "failed", run.Outcome)
			assert.Contains(t, run.Result, "template")
			assert.False(t, called, "rendering must fail before an agent starts")
			automationAssertNoTickets(t, h)
		})
	}
}

func TestAutomationRecoveryBlocksOverlap(t *testing.T) {
	h, d, def, _ := automationFixture(t)
	first, err := d.RunAutomation(def.ID, "first")
	require.NoError(t, err)
	_, err = d.automationStore.UpdateRun(def.ID, first.N, func(r *automation.Run) error { r.Phase = "running"; r.StartedAt = time.Now(); return nil })
	require.NoError(t, err)
	automationClose(t, d)
	stop := d.startAutomations(h.tasksDir)
	defer stop()
	d.recoverAutomationRuns()
	first, err = d.automationStore.GetRun(def.ID, first.N)
	require.NoError(t, err)
	assert.Equal(t, "recovery_required", first.Phase)
	assert.Empty(t, first.Outcome)
	assert.Contains(t, first.Result, "termination recovery")
	next, err := d.RunAutomation(def.ID, "next")
	require.NoError(t, err)
	assert.Equal(t, "terminal", next.Phase)
	assert.Equal(t, "skipped", next.Outcome)
	_, err = d.StopAutomation(def.ID, first.N)
	require.NoError(t, err)
	d.background.Wait()
	first, err = d.automationStore.GetRun(def.ID, first.N)
	require.NoError(t, err)
	assert.Equal(t, "recovery_required", first.Phase, "missing terminal identity cannot prove termination")
	automationAssertNoTickets(t, h)
}

func TestAutomationDaemonLifecycle(t *testing.T) {
	h, seed, def, _ := automationFixture(t)
	automationClose(t, seed)
	cfg := *h.cfg
	cfg.Agents = map[string]config.Agent{"agent1": {Binary: "/bin/sh", Args: []string{"-c", `printf '%s' '{"outcome":"complete","summary":"Done"}' > "$KONTORA_RESULT_FILE"`}}}
	d := h.newDaemon(&cfg, WithConfigPath(seed.configPath))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	require.Eventually(t, func() bool { _, err := d.ListAutomations(); return err == nil }, 5*time.Second, 10*time.Millisecond)
	run, err := d.RunAutomation(def.ID, "lifecycle")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		runs, e := d.AutomationRuns(def.ID)
		return e == nil && len(runs) > 0 && runs[0].N == run.N && runs[0].Phase == "terminal"
	}, 5*time.Second, 10*time.Millisecond)
	runs, err := d.AutomationRuns(def.ID)
	require.NoError(t, err)
	assert.Equal(t, "complete", runs[0].Outcome, runs[0].Result)
	automationAssertNoTickets(t, h)
}

func TestAutomationScopeUnavailable(t *testing.T) {
	h, d, _, _ := automationFixture(t)
	other := h.newDaemon(h.cfg, WithConfigPath(d.configPath), WithAutomationRoot(d.automationRoot))
	closeStore := other.startAutomations(h.tasksDir)
	defer closeStore()
	_, err := other.ListAutomations()
	require.ErrorIs(t, err, automation.ErrUnavailable)
	assert.Contains(t, strings.ToLower(err.Error()), "lock")
}
