package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/tmux"
)

func recoveryAllocate(t *testing.T, d *Daemon, id, request, phase string) automation.Run {
	t.Helper()
	run, _, err := d.automationStore.Allocate(id, "manual", request, time.Time{}, time.Time{}, time.Now())
	require.NoError(t, err)
	if phase != "queued" {
		run, err = d.automationStore.UpdateRun(id, run.N, func(r *automation.Run) error {
			r.Phase, r.StartedAt = phase, time.Now()
			return nil
		})
		require.NoError(t, err)
	}
	return run
}

func recoveryArtifact(t *testing.T, d *Daemon, run automation.Run, id tmux.WindowIdentity) automation.Run {
	t.Helper()
	run, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
		r.WindowID, r.Session, r.Window = id.WindowID, "owned-session", "run"
		return nil
	})
	require.NoError(t, err)
	path := filepath.Join(automationRunDir(d.config(), run), "terminal.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	b, err := json.Marshal(id)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return run
}

func recoveryAbsentTmux(t *testing.T) tmux.WindowIdentity {
	t.Helper()
	dir := t.TempDir()
	id := tmux.WindowIdentity{SocketPath: "/tmp/automation-test-socket", ServerPID: os.Getpid(), ServerStartedAt: 123, SessionID: "$1", WindowID: "@1"}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%d\t123'\n", id.ServerPID)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return id
}

func TestAutomationRecoveryIncludesRetiredRuns(t *testing.T) {
	_, d, live, _ := automationFixture(t)
	specs := map[string]automation.Spec{live.Key: live.Spec, "retired": live.Spec}
	require.NoError(t, d.automationStore.Reconcile(specs, time.Now()))
	rows, err := d.automationStore.List()
	require.NoError(t, err)
	var retired automation.Definition
	for _, row := range rows {
		if row.Key == "retired" {
			retired = row
		}
	}
	require.NotEmpty(t, retired.ID)
	queued := recoveryAllocate(t, d, live.ID, "live-queued", "queued")
	retiredQueued := recoveryAllocate(t, d, retired.ID, "retired-queued", "queued")
	retiredRunning := recoveryAllocate(t, d, retired.ID, "retired-running", "running")
	spec := live.Spec
	spec.Prompt = "Updated definition must not replace the queued snapshot."
	require.NoError(t, d.automationStore.Reconcile(map[string]automation.Spec{live.Key: spec}, time.Now()))
	d.recoverAutomationRuns()
	got, err := d.automationStore.GetRun(live.ID, queued.N)
	require.NoError(t, err)
	assert.Equal(t, queued, got)
	got, err = d.automationStore.GetRun(retired.ID, retiredQueued.N)
	require.NoError(t, err)
	assert.Equal(t, "terminal", got.Phase)
	assert.Equal(t, "skipped", got.Outcome)
	got, err = d.automationStore.GetRun(retired.ID, retiredRunning.N)
	require.NoError(t, err)
	assert.Equal(t, "recovery_required", got.Phase)
	assert.Len(t, d.sem, 1)
	assert.NotNil(t, d.automationExecutions[retired.ID])
	assert.Nil(t, d.automationExecutions[live.ID])
}

func TestAutomationRecoveryCapacityBeyondLimit(t *testing.T) {
	_, d, firstDef, _ := automationFixture(t)
	d.sem = make(chan struct{}, 1)
	require.NoError(t, d.automationStore.Reconcile(map[string]automation.Spec{firstDef.Key: firstDef.Spec, "second": firstDef.Spec}, time.Now()))
	rows, err := d.automationStore.List()
	require.NoError(t, err)
	for _, row := range rows {
		recoveryAllocate(t, d, row.ID, "interrupted", "running")
	}
	d.recoverAutomationRuns()
	require.Len(t, d.automationExecutions, 2)
	var tokenRun, debtRun automation.Run
	for _, e := range d.automationExecutions {
		if e.reserved {
			tokenRun = e.run
		} else {
			debtRun = e.run
		}
	}
	require.NotEmpty(t, tokenRun.AutomationID)
	require.NotEmpty(t, debtRun.AutomationID)
	d.mu.Lock()
	assert.Equal(t, -1, d.availablePoolSlotsLocked(d.sem))
	d.mu.Unlock()
	id := recoveryAbsentTmux(t)
	tokenRun = recoveryArtifact(t, d, tokenRun, id)
	d.recoverAutomationRun(tokenRun)
	assert.Empty(t, d.sem)
	d.mu.Lock()
	assert.Zero(t, d.availablePoolSlotsLocked(d.sem), "an unconfirmed process must still block the free channel slot")
	d.mu.Unlock()
	// A retry without evidence must not discard the remaining reservation.
	d.recoverAutomationRun(debtRun)
	d.mu.Lock()
	assert.Zero(t, d.availablePoolSlotsLocked(d.sem))
	d.mu.Unlock()
	debtRun = recoveryArtifact(t, d, debtRun, id)
	d.recoverAutomationRun(debtRun)
	d.mu.Lock()
	assert.Equal(t, 1, d.availablePoolSlotsLocked(d.sem))
	assert.Empty(t, d.recoveredReservations)
	d.mu.Unlock()
	assert.Empty(t, d.automationExecutions)
}

func TestAutoStaleRecovery(t *testing.T) {
	_, d, def, _ := automationFixture(t)
	d.sem = make(chan struct{}, 1)
	old := recoveryAllocate(t, d, def.ID, "old", "recovery_required")
	next := recoveryAllocate(t, d, def.ID, "next", "running")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	successor := &automationExecution{run: next, ctx: ctx, cancel: cancel, reserved: true}
	d.automationExecutions[def.ID] = successor
	d.sem <- struct{}{}
	old = recoveryArtifact(t, d, old, recoveryAbsentTmux(t))
	d.recoverAutomationRun(old)
	d.recoverAutomationRun(old)
	assert.Len(t, d.sem, 1)
	assert.Same(t, successor, d.automationExecutions[def.ID])
	assert.True(t, successor.reserved)
	assert.NoError(t, ctx.Err())
	current, err := d.automationStore.GetRun(def.ID, next.N)
	require.NoError(t, err)
	assert.Equal(t, "running", current.Phase)
	current, err = d.automationStore.GetRun(def.ID, old.N)
	require.NoError(t, err)
	assert.Equal(t, "terminal", current.Phase)
}

func TestAutoRecoveryTargetRace(t *testing.T) {
	for _, mutation := range []string{"artifact", "window"} {
		t.Run(mutation, func(t *testing.T) {
			_, d, def, _ := automationFixture(t)
			id, queried, release := terminalInspectionStub(t, false, true)
			run := recoveryArtifact(t, d, recoveryAllocate(t, d, def.ID, "recover", "recovery_required"), id)
			done := make(chan struct{})
			go func() { defer close(done); d.recoverAutomationRun(run) }()
			require.Eventually(t, func() bool { _, err := os.Stat(queried); return err == nil }, time.Second, 10*time.Millisecond)
			if mutation == "artifact" {
				id.ServerStartedAt++
				b, err := json.Marshal(id)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(automationRunDir(d.config(), run), "terminal.json"), b, 0o600))
			} else {
				_, err := d.automationStore.UpdateRun(def.ID, run.N, func(r *automation.Run) error { r.WindowID = "@2"; return nil })
				require.NoError(t, err)
			}
			require.NoError(t, os.WriteFile(release, nil, 0o600))
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("recovery did not finish")
			}
			assert.Len(t, d.sem, 1)
			current, err := d.automationStore.GetRun(def.ID, run.N)
			require.NoError(t, err)
			assert.Equal(t, "recovery_required", current.Phase)
			assert.NotNil(t, d.automationExecutions[def.ID])
		})
	}
}

func recoveryRealWindow(t *testing.T) tmux.WindowIdentity {
	t.Helper()
	if testing.Short() {
		t.Skip("tmux integration")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir, err := os.MkdirTemp("/tmp", "kar-")
	require.NoError(t, err)
	socket := filepath.Join(dir, "s")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "-N", "kill-server").Run(); _ = os.RemoveAll(dir) })
	out, err := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-s", "origin", "-P", "-F", "#{socket_path}\t#{pid}\t#{start_time}\t#{session_id}\t#{window_id}", "--", "/bin/sleep", "999").CombinedOutput()
	require.NoError(t, err, "%s", out)
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	require.Len(t, fields, 5)
	pid, err := strconv.Atoi(fields[1])
	require.NoError(t, err)
	started, err := strconv.ParseInt(fields[2], 10, 64)
	require.NoError(t, err)
	return tmux.WindowIdentity{SocketPath: fields[0], ServerPID: pid, ServerStartedAt: started, SessionID: fields[3], WindowID: fields[4]}
}

func TestAutomationRecoveryStopAcrossLinkedViewer(t *testing.T) {
	_, d, def, _ := automationFixture(t)
	id := recoveryRealWindow(t)
	run := recoveryArtifact(t, d, recoveryAllocate(t, d, def.ID, "interrupted", "recovery_required"), id)
	tmuxCmd := func(args ...string) {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-S", id.SocketPath}, args...)...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	tmuxCmd("new-session", "-d", "-s", "viewer", "--", "/bin/sleep", "999")
	tmuxCmd("link-window", "-k", "-s", id.WindowID, "-t", "=viewer:")
	tmuxCmd("kill-session", "-t", "=origin")
	// Reused session and window names must not make the replacement a recovery target.
	tmuxCmd("new-session", "-d", "-s", "origin", "-n", "run", "--", "/bin/sleep", "999")
	d.automationMu.Lock()
	d.reserveAutomationRecoveryLocked(run)
	d.automationMu.Unlock()
	stopping, err := d.StopAutomation(def.ID, run.N)
	require.NoError(t, err)
	assert.Equal(t, "stopping", stopping.Phase)
	d.background.Wait()
	current, err := d.automationStore.GetRun(def.ID, run.N)
	require.NoError(t, err)
	assert.Equal(t, "terminal", current.Phase)
	assert.Equal(t, "stopped by user", current.Result)
	require.NotNil(t, current.ExitCode)
	assert.Equal(t, 143, *current.ExitCode)
	assert.Equal(t, "tmux_window_gone", current.Termination)
	present, err := tmux.InspectWindowIdentity(id)
	require.NoError(t, err)
	assert.False(t, present)
	tmuxCmd("has-session", "-t", "=origin")
	assert.Empty(t, d.sem)
	assert.Empty(t, d.automationExecutions)
}
