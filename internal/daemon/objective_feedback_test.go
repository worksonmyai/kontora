package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestObjectiveFeedbackBlocksAcceptance(t *testing.T) {
	for _, planned := range []bool{false, true} {
		name := "outside plan"
		if planned {
			name = "planned but unresolved"
		}
		t.Run(name, func(t *testing.T) {
			store := newControlStore(t)
			checkedDeliveryFixture(t, store, initRepo(t), strings.Repeat("a", 40), strings.Repeat("b", 40))
			controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutPlan, Plan: orchestrator.Plan{ID: "plan", ObjectiveID: "objective", Revision: 1, WorkIDs: []string{"worker"}, RequiredResults: []string{"worker"}}})
			state, err := store.State()
			require.NoError(t, err)
			binding, err := orchestrator.CurrentAcceptanceBinding(state, "objective")
			require.NoError(t, err)
			evidence, err := store.PutArtifact([]byte("combined review"))
			require.NoError(t, err)
			controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordCombinedReview, OperatorID: "reviewer", At: time.Now(), Review: orchestrator.CombinedReview{ID: "combined", ObjectiveID: "objective", Binding: binding, Evidence: evidence}})
			state, err = store.State()
			require.NoError(t, err)
			require.NoError(t, orchestrator.AcceptanceReady(state, "objective", "combined"))
			service, err := NewObjectiveService(store)
			require.NoError(t, err)
			_, err = service.ControlObjective(context.Background(), "operator", web.ObjectiveControlRequest{BoardID: "board", RequestID: "correction", ExpectedRevision: state.Revision, Operation: "feedback", ObjectiveID: "objective", Text: "Fix the uncovered error path before acceptance"})
			require.NoError(t, err)
			if planned {
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutPlan, Plan: orchestrator.Plan{ID: "feedback-plan", ObjectiveID: "objective", Revision: 2, WorkIDs: []string{"worker", "feedback/correction"}, RequiredResults: []string{"worker"}}})
			}
			state, err = store.State()
			require.NoError(t, err)
			require.Empty(t, state.Work["feedback/correction"].LastAttemptID)
			require.NoError(t, orchestrator.Ready(state, "feedback/correction"))
			require.Error(t, orchestrator.AcceptanceReady(state, "objective", "combined"))
			_, err = orchestrator.Apply(state, orchestrator.Command{Kind: orchestrator.RecordAcceptance, ExpectedRevision: state.Revision, OperatorID: "operator", At: time.Now(), Acceptance: orchestrator.Acceptance{ID: "accept", ObjectiveID: "objective", Binding: &binding, ReviewID: "combined"}})
			require.Error(t, err)
			err = store.Transact(state.Revision, func(s orchestrator.State) (orchestrator.State, error) {
				return orchestrator.Apply(s, orchestrator.Command{Kind: orchestrator.RecordCombinedReview, ExpectedRevision: s.Revision, OperatorID: "reviewer", At: time.Now(), Review: orchestrator.CombinedReview{ID: "premature-review", ObjectiveID: "objective", Binding: binding, Evidence: evidence}})
			})
			require.Error(t, err)
		})
	}
}
