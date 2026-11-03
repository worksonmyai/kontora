package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestObjectiveServiceControls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation string
		text      string
		revision  uint64
		board     string
		want      error
		wantText  string
	}{
		{name: "feedback remains tracked", operation: "feedback", text: "Add error coverage", revision: 1, board: "board"},
		{name: "pause", operation: "pause", text: "review policy", revision: 1, board: "board"},
		{name: "cancel", operation: "cancel", text: "retain work", revision: 1, board: "board"},
		{name: "pause needs a reason", operation: "pause", revision: 1, board: "board", wantText: "control reason is required"},
		{name: "cancel needs a reason", operation: "cancel", revision: 1, board: "board", wantText: "control reason is required"},
		{name: "feedback needs text", operation: "feedback", revision: 1, board: "board", wantText: "feedback is required"},
		{name: "unknown operation", operation: "unknown", revision: 1, board: "board", wantText: `unknown objective operation "unknown"`},
		{name: "stale mutation", operation: "pause", text: "pause", board: "board", want: orchestrator.ErrStale},
		{name: "another board", operation: "pause", text: "pause", revision: 1, board: "other", want: orchestrator.ErrConflict},
		{name: "resume stays disabled", operation: "resume", revision: 1, board: "board", want: web.ErrOrchestratorUnavailable},
		{name: "release stays disabled", operation: "release", revision: 1, board: "board", want: web.ErrOrchestratorUnavailable},
		{name: "accept stays disabled", operation: "accept", revision: 1, board: "board", want: web.ErrOrchestratorUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			service, err := NewObjectiveService(store)
			require.NoError(t, err)
			req := web.ObjectiveControlRequest{BoardID: "board", RequestID: "assign", Operation: "assign", ObjectiveID: "objective", Objective: &orchestrator.Objective{ID: "objective", Assignment: "deliver", Criteria: []string{"checked"}, Policy: orchestrator.Policy{RequireCommitApproval: true, RequireAcceptance: true}, Budget: orchestrator.Budget{MaxAttempts: 1, MaxRuntime: time.Minute}}}
			assigned, err := service.ControlObjective(context.Background(), "operator", req)
			require.NoError(t, err)
			require.EqualValues(t, 1, assigned.Revision)
			command := web.ObjectiveControlRequest{BoardID: tc.board, RequestID: "control", ExpectedRevision: tc.revision, Operation: tc.operation, ObjectiveID: "objective", Text: tc.text}
			response, err := service.ControlObjective(context.Background(), "operator", command)
			if tc.wantText != "" {
				require.EqualError(t, err, tc.wantText)
				var invalid *web.ObjectiveRequestError
				require.ErrorAs(t, err, &invalid)
			} else {
				require.ErrorIs(t, err, tc.want)
				if tc.want == nil {
					require.EqualValues(t, 2, response.Revision)
				}
			}
			again, err := service.ControlObjective(context.Background(), "operator", req)
			require.NoError(t, err)
			require.Equal(t, assigned, again)
			snapshot, err := service.GetObjective(context.Background(), "objective")
			require.NoError(t, err)
			require.False(t, snapshot.ExecutionEnabled)
			if tc.operation == "feedback" && tc.wantText == "" {
				require.Len(t, snapshot.Work, 1)
				require.Equal(t, "Add error coverage", snapshot.Work[0].Specification)
				require.True(t, snapshot.Work[0].AcceptanceRequired)
				require.Equal(t, orchestrator.ObjectiveDraft, snapshot.Objective.Status)
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.SetObjectiveStatus, ObjectiveID: "objective", Status: orchestrator.ObjectiveActive})
				admitControl(t, store, snapshot.Work[0].ID, "feedback-attempt")
				scope := web.AttemptScope{BoardID: "board", WorkID: snapshot.Work[0].ID, AttemptID: "feedback-attempt"}
				handler, err := web.AuthorityMiddleware("", &web.AuthorityConfig{
					Attempts: []web.AttemptCapability{{Token: "worker", Scope: scope, Access: web.AttemptPropose, ExpiresAt: time.Now().Add(time.Hour)}},
					AttemptActive: func(_ context.Context, got web.AttemptScope) bool {
						state, err := store.State()
						require.NoError(t, err)
						a := state.Attempts[got.AttemptID]
						return got == scope && a.WorkID == got.WorkID && a.Reservation.Held
					},
				}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got, ok := web.AttemptIdentity(r.Context())
					require.True(t, ok)
					require.Equal(t, scope, got)
					w.WriteHeader(http.StatusNoContent)
				}))
				require.NoError(t, err)
				r := httptest.NewRequest(http.MethodPost, "/api/boards/board/attempts/feedback-attempt/proposals", nil)
				r.Header.Set("Authorization", "Bearer worker")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				require.Equal(t, http.StatusNoContent, w.Code)
			}
			snapshot.Objective.Criteria[0] = "modified detached value"
			list, err := service.ListObjectives(context.Background())
			require.NoError(t, err)
			require.Equal(t, []string{"checked"}, list.Objectives[0].Criteria)
		})
	}
}

func TestObjectiveServiceApproval(t *testing.T) {
	store := newControlStore(t)
	assignedControl(t, store)
	controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordAction, Action: orchestrator.Action{ID: "commit", ObjectiveID: "objective", Kind: "commit_candidate", RequiresApproval: true}})
	service, err := NewObjectiveService(store)
	require.NoError(t, err)
	state, err := store.State()
	require.NoError(t, err)
	req := web.ObjectiveControlRequest{BoardID: "board", ExpectedRevision: state.Revision, RequestID: "approval", Operation: "approve", ObjectiveID: "objective", ActionID: "commit", Binding: "stale candidate"}
	_, err = service.ControlObjective(context.Background(), "operator", req)
	require.ErrorIs(t, err, orchestrator.ErrStale)
	req.Binding = orchestrator.ActionBinding(state.Actions["commit"])
	_, err = service.ControlObjective(context.Background(), "operator", req)
	require.NoError(t, err)
	state, err = store.State()
	require.NoError(t, err)
	require.Equal(t, "operator", state.Actions["commit"].Approval.OperatorID)
	require.Equal(t, orchestrator.ActionPending, state.Actions["commit"].Status)
}

func TestObjectiveServiceRejectsInvalidAndCancelledRequests(t *testing.T) {
	store := newControlStore(t)
	service, err := NewObjectiveService(store)
	require.NoError(t, err)
	_, err = service.ControlObjective(context.Background(), "", web.ObjectiveControlRequest{})
	require.Error(t, err)
	for _, tc := range []struct {
		name, requestID, text, want string
	}{
		{name: "long request identity", requestID: strings.Repeat("r", 257), want: "request identity exceeds 256 bytes"},
		{name: "large request", requestID: "request", text: strings.Repeat("x", 1<<20), want: "objective request exceeds limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.ControlObjective(context.Background(), "operator", web.ObjectiveControlRequest{BoardID: "board", RequestID: tc.requestID, Operation: "assign", Text: tc.text})
			require.EqualError(t, err, tc.want)
			var invalid *web.ObjectiveRequestError
			require.ErrorAs(t, err, &invalid)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.ListObjectives(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = service.ControlObjective(ctx, "operator", web.ObjectiveControlRequest{})
	require.ErrorIs(t, err, context.Canceled)
	state, err := store.State()
	require.NoError(t, err)
	require.Zero(t, state.Revision)
}

func TestObjectiveServiceClosedStore(t *testing.T) {
	store := newControlStore(t)
	service, err := NewObjectiveService(store)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	_, err = service.ListObjectives(context.Background())
	require.Equal(t, controlstore.ErrClosed, err)
	_, err = service.GetObjective(context.Background(), "objective")
	require.Equal(t, controlstore.ErrClosed, err)
	_, err = service.ControlObjective(context.Background(), "operator", web.ObjectiveControlRequest{BoardID: "board", RequestID: "request", ObjectiveID: "objective", Operation: "pause", Text: "review"})
	require.Equal(t, controlstore.ErrClosed, err)
}

func TestObjectiveServiceAssignment(t *testing.T) {
	const diagnostic = "Draft assignment requires an objective whose nonempty id matches objective_id. Criteria, commit approval, and human acceptance are required."
	for _, tc := range []struct {
		name   string
		id     string
		mutate func(*web.ObjectiveControlRequest)
		want   string
	}{
		{name: "repository scope is optional", id: "objective"},
		{name: "escaped identity", id: "objective%2Fone"},
		{name: "missing objective", id: "objective", mutate: func(r *web.ObjectiveControlRequest) { r.Objective = nil }, want: diagnostic},
		{name: "missing id", want: diagnostic},
		{name: "missing objective id", id: "objective", mutate: func(r *web.ObjectiveControlRequest) { r.Objective.ID = "" }, want: diagnostic},
		{name: "mismatched id", id: "objective", mutate: func(r *web.ObjectiveControlRequest) { r.Objective.ID = "other" }, want: diagnostic},
		{name: "missing criteria", id: "objective", mutate: func(r *web.ObjectiveControlRequest) { r.Objective.Criteria = nil }, want: diagnostic},
		{name: "missing commit approval", id: "objective", mutate: func(r *web.ObjectiveControlRequest) { r.Objective.Policy.RequireCommitApproval = false }, want: diagnostic},
		{name: "missing human acceptance", id: "objective", mutate: func(r *web.ObjectiveControlRequest) { r.Objective.Policy.RequireAcceptance = false }, want: diagnostic},
		{name: "dot", id: ".", want: "invalid objective id"},
		{name: "double dot", id: "..", want: "invalid objective id"},
		{name: "slash", id: "a/b", want: "invalid objective id"},
		{name: "backslash", id: `a\b`, want: "invalid objective id"},
		{name: "whitespace", id: "a b", want: "invalid objective id"},
		{name: "invalid domain state", id: "objective", mutate: func(r *web.ObjectiveControlRequest) { r.Objective.Status = orchestrator.ObjectiveActive }, want: "new objectives must be drafts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newControlStore(t)
			service, err := NewObjectiveService(store)
			require.NoError(t, err)
			req := web.ObjectiveControlRequest{BoardID: "board", RequestID: "assign", Operation: "assign", ObjectiveID: tc.id, Objective: &orchestrator.Objective{ID: tc.id, Assignment: "deliver", Criteria: []string{"checked"}, Policy: orchestrator.Policy{RequireCommitApproval: true, RequireAcceptance: true}}}
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			_, err = service.ControlObjective(context.Background(), "operator", req)
			if tc.want == "" {
				require.NoError(t, err)
				snapshot, err := service.GetObjective(context.Background(), tc.id)
				require.NoError(t, err)
				require.Equal(t, tc.id, snapshot.Objective.ID)
			} else {
				require.EqualError(t, err, tc.want)
				var invalid *web.ObjectiveRequestError
				require.ErrorAs(t, err, &invalid)
				state, err := store.State()
				require.NoError(t, err)
				require.Zero(t, state.Revision)
			}
		})
	}
}

func TestObjectiveServiceSnapshotEvidence(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "historical and current evidence"}[populated], func(t *testing.T) {
			store := newControlStore(t)
			if populated {
				repo := initRepo(t)
				checkedDeliveryFixture(t, store, repo, strings.Repeat("a", 40), strings.Repeat("b", 40))
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.PutPlan, Plan: orchestrator.Plan{ID: "plan", ObjectiveID: "objective", Revision: 1, WorkIDs: []string{"worker"}, RequiredResults: []string{"worker"}}})
				state, err := store.State()
				require.NoError(t, err)
				binding, err := orchestrator.CurrentAcceptanceBinding(state, "objective")
				require.NoError(t, err)
				evidence, err := store.PutArtifact([]byte("combined review findings"))
				require.NoError(t, err)
				for _, id := range []string{"z-review", "a-review"} {
					controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordCombinedReview, OperatorID: "reviewer", At: time.Now(), Review: orchestrator.CombinedReview{ID: id, ObjectiveID: "objective", Binding: binding, Evidence: evidence, Findings: []orchestrator.ReviewFinding{{Message: "add coverage", RepairWorkID: "worker"}}}})
				}
				definition := state.VerificationPolicies["policy"].Definitions[0]
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordVerificationPolicy, OperatorID: "operator", At: time.Now(), VerificationPolicy: orchestrator.VerificationPolicy{ID: "a-policy", ObjectiveID: "objective", Revision: "v2", Definitions: []orchestrator.CheckDefinition{definition}}})
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.CreateObjective, Objective: orchestrator.Objective{ID: "other", Assignment: "other assignment", Criteria: []string{"other criteria"}}})
				controlApply(t, store, orchestrator.Command{Kind: orchestrator.RecordVerificationPolicy, OperatorID: "operator", At: time.Now(), VerificationPolicy: orchestrator.VerificationPolicy{ID: "other-policy", ObjectiveID: "other", Revision: "v1", Definitions: []orchestrator.CheckDefinition{definition}}})
			} else {
				assignedControl(t, store)
			}
			service, err := NewObjectiveService(store)
			require.NoError(t, err)
			snapshot, err := service.GetObjective(context.Background(), "objective")
			require.NoError(t, err)
			data, err := json.Marshal(snapshot)
			require.NoError(t, err)
			var decoded web.ObjectiveSnapshot
			require.NoError(t, json.Unmarshal(data, &decoded))
			if !populated {
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(data, &fields))
				require.JSONEq(t, `[]`, string(fields["verification_policies"]))
				require.JSONEq(t, `[]`, string(fields["reviews"]))
				return
			}
			other, err := service.GetObjective(context.Background(), "other")
			require.NoError(t, err)
			require.Empty(t, other.Reviews)
			require.Len(t, other.VerificationPolicies, 1)
			require.Equal(t, "other-policy", other.VerificationPolicies[0].ID)
			state, err := store.State()
			require.NoError(t, err)
			require.Equal(t, []orchestrator.VerificationPolicy{state.VerificationPolicies["a-policy"], state.VerificationPolicies["policy"]}, decoded.VerificationPolicies)
			require.Equal(t, []orchestrator.CombinedReview{state.Reviews["a-review"], state.Reviews["z-review"]}, decoded.Reviews)
			snapshot.VerificationPolicies[0].Definitions[0].Command[0] = "changed"
			snapshot.Reviews[0].Findings[0].Message = "changed"
			again, err := service.GetObjective(context.Background(), "objective")
			require.NoError(t, err)
			require.Equal(t, decoded.VerificationPolicies, again.VerificationPolicies)
			require.Equal(t, decoded.Reviews, again.Reviews)
		})
	}
}
