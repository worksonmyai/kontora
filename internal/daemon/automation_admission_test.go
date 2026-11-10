package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
)

func TestAutoMissedSlotPreservesActiveRun(t *testing.T) {
	for _, policy := range []string{"skip", "queue", "replace"} {
		t.Run(policy, func(t *testing.T) {
			_, d, def, now := automationFixture(t)
			def.Overlap = policy
			def, err := d.UpdateAutomation(def.ID, def.Revision, def.Spec)
			require.NoError(t, err)
			first, err := d.RunAutomation(def.ID, "first")
			require.NoError(t, err)
			_, err = d.automationStore.UpdateRun(def.ID, first.N, func(r *automation.Run) error { r.Phase = "running"; r.StartedAt = time.Now(); return nil })
			require.NoError(t, err)
			d.automationMu.Lock()
			skipped, err := d.acceptAutomationLocked(def.ID, "schedule", "missed", now.Add(-5*time.Minute), def.Next.Add(time.Hour), now)
			execution := d.automationExecutions[def.ID]
			d.automationMu.Unlock()
			require.NoError(t, err)
			require.Equal(t, "skipped", skipped.Outcome)
			require.Contains(t, skipped.Result, "downtime")
			require.NoError(t, execution.ctx.Err())
			active, err := d.automationStore.GetRun(def.ID, first.N)
			require.NoError(t, err)
			require.Equal(t, "running", active.Phase)
			require.Empty(t, active.StopReason)
		})
	}
}

func TestAutoQueuedReplacementRecovery(t *testing.T) {
	h, d, def, _ := automationFixture(t)
	def.Overlap = "replace"
	def, err := d.UpdateAutomation(def.ID, def.Revision, def.Spec)
	require.NoError(t, err)
	first, err := d.RunAutomation(def.ID, "first")
	require.NoError(t, err)
	second, err := d.RunAutomation(def.ID, "second")
	require.NoError(t, err)
	saved, err := d.automationStore.GetRun(def.ID, first.N)
	require.NoError(t, err)
	require.Equal(t, "stopping", saved.Phase)
	automationClose(t, d)
	restarted := h.newDaemon(h.cfg, WithConfigPath(d.configPath))
	closeStore := restarted.startAutomations(h.tasksDir)
	defer closeStore()
	restarted.recoverAutomationRuns()
	saved, err = restarted.automationStore.GetRun(def.ID, first.N)
	require.NoError(t, err)
	require.Equal(t, "terminal", saved.Phase)
	require.Equal(t, "skipped", saved.Outcome)
	waiting, err := restarted.automationStore.GetRun(def.ID, second.N)
	require.NoError(t, err)
	require.Equal(t, "queued", waiting.Phase)
	require.NoError(t, restarted.automationTick(time.Now()))
	require.Len(t, restarted.automationJobs, 1)
	restarted.automationExecutions[def.ID].cancel()
	require.True(t, restarted.automationJobs[0].run(context.Background()))
}

func TestAutoRootMergeRefused(t *testing.T) {
	content := []byte("<<: {automations: {one: {name: first}, two: {name: second}}}\nagents: {}\n")
	for _, tc := range []struct {
		name, key string
		spec      *automation.Spec
	}{
		{"create", "third", &automation.Spec{Name: "new"}},
		{"update", "one", &automation.Spec{Name: "updated"}},
		{"delete", "one", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := automationYAML(content, tc.key, tc.spec)
			require.ErrorContains(t, err, "root YAML merge")
		})
	}
}
