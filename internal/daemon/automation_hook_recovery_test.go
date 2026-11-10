package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
)

func TestAutoHookRecoveryEvidence(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "confirmed hook"
		if pending {
			name = "unconfirmed hook"
		}
		t.Run(name, func(t *testing.T) {
			_, d, definition, _ := automationFixture(t)
			run := recoveryAllocate(t, d, definition.ID, "interrupted", "recovery_required")
			run = recoveryArtifact(t, d, run, recoveryAbsentTmux(t))
			if pending {
				var err error
				run, err = d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error { r.Termination = automationHookPending; return nil })
				require.NoError(t, err)
			}
			d.recoverAutomationRun(run)
			saved, err := d.automationStore.GetRun(run.AutomationID, run.N)
			require.NoError(t, err)
			if pending {
				require.Equal(t, "recovery_required", saved.Phase)
				require.Contains(t, saved.Result, "hook termination is unconfirmed")
				require.Equal(t, 1, d.sem.used)
			} else {
				require.Equal(t, "terminal", saved.Phase)
				require.Zero(t, d.sem.used)
			}
		})
	}
}
