package daemon

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
)

func TestPlannerEvidenceArtifactLimits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		data        []byte
		generations int
		binary      bool
		truncated   bool
	}{
		{name: "binary", data: []byte{0xff, 0x00}, generations: 1, binary: true},
		{name: "oversized", data: bytes.Repeat([]byte("x"), 300<<10), generations: 1, truncated: true},
		{name: "retained history", data: bytes.Repeat([]byte("x"), 220<<10), generations: 6, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			assignedControl(t, store)
			artifact, err := store.PutArtifact(tc.data)
			require.NoError(t, err)
			for i := 1; i <= tc.generations; i++ {
				id := fmt.Sprintf("worker-%d", i)
				admitControl(t, store, "worker", id)
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.StopAttempt, WorkID: "worker", AttemptID: id, AttemptStatus: orchestrator.AttemptSucceeded, Receipt: "worker stopped", At: time.Now()})
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordResult, Result: orchestrator.Result{ID: id, WorkID: "worker", AttemptID: id, Generation: uint64(i), SpecificationRevision: 1, Artifacts: []orchestrator.ArtifactRef{artifact}, Valid: true}})
			}
			admitControl(t, store, "planner", "turn")
			controller := objectiveController{store: store}
			snapshot, err := controller.snapshot("objective", "turn")
			require.NoError(t, err)
			require.Len(t, snapshot.Evidence.Results, tc.generations)
			for _, result := range snapshot.Evidence.Results {
				if !result.Valid {
					require.Empty(t, result.Information)
					continue
				}
				require.Len(t, result.Information, 1)
				info := result.Information[0]
				require.Equal(t, tc.binary, info.Binary)
				require.Equal(t, tc.truncated, info.Truncated)
				require.LessOrEqual(t, len(info.Text), 8<<10)
			}
		})
	}
}
