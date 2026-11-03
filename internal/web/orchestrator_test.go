package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
)

type objectiveTestService struct {
	operator string
	calls    int
	err      error
}

func (s *objectiveTestService) ListObjectives(context.Context) (ObjectiveList, error) {
	return ObjectiveList{BoardID: "board", Revision: 3, DisabledReason: ErrOrchestratorUnavailable.Error()}, s.err
}
func (s *objectiveTestService) GetObjective(_ context.Context, id string) (ObjectiveSnapshot, error) {
	return ObjectiveSnapshot{BoardID: "board", Revision: 3, Objective: orchestrator.Objective{ID: id}}, s.err
}
func (s *objectiveTestService) ControlObjective(_ context.Context, operator string, req ObjectiveControlRequest) (ObjectiveControlResponse, error) {
	s.operator, s.calls = operator, s.calls+1
	return ObjectiveControlResponse{BoardID: "board", Revision: 4, ObjectiveID: req.ObjectiveID, Operation: req.Operation}, s.err
}

func TestObjectiveRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, credential, body string
		want                   int
		serviceErr             error
		wantError              string
	}{
		{name: "operator control", credential: "operator", body: `{"request_id":"r","board_id":"board","expected_revision":3,"operation":"pause","objective_id":"o","text":"review"}`, want: 200},
		{name: "worker refused", credential: "worker", body: `{}`, want: 403},
		{name: "shared token not operator", credential: "shared", body: `{}`, want: 401},
		{name: "anonymous", body: `{}`, want: 401},
		{name: "body cannot declare authority", credential: "operator", body: `{"operator_id":"admin"}`, want: 400, wantError: `invalid objective request: json: unknown field "operator_id"`},
		{name: "wrong field type", credential: "operator", body: `{"expected_revision":"three"}`, want: 400, wantError: "invalid objective request: json: cannot unmarshal string into Go struct field ObjectiveControlRequest.expected_revision of type uint64"},
		{name: "duplicate field", credential: "operator", body: `{"objective_id":"o","objective_id":"o"}`, want: 400},
		{name: "mixed field casing", credential: "operator", body: `{"Objective_id":"o"}`, want: 400},
		{name: "multiple objects", credential: "operator", body: `{} {}`, want: 400},
		{name: "wrong path", credential: "operator", body: `{"objective_id":"other"}`, want: 400},
		{name: "stale request", credential: "operator", body: `{"objective_id":"o"}`, want: 409, serviceErr: orchestrator.ErrStale},
		{name: "unavailable service", credential: "operator", body: `{"objective_id":"o"}`, want: 503, serviceErr: ErrOrchestratorUnavailable},
		{name: "conflict", credential: "operator", body: `{"objective_id":"o"}`, want: 409, serviceErr: orchestrator.ErrConflict},
		{name: "not ready", credential: "operator", body: `{"objective_id":"o"}`, want: 409, serviceErr: orchestrator.ErrNotReady},
		{name: "budget", credential: "operator", body: `{"objective_id":"o"}`, want: 400, serviceErr: orchestrator.ErrBudget},
		{name: "missing objective", credential: "operator", body: `{"objective_id":"o"}`, want: 404, serviceErr: ErrTicketNotFound},
		{name: "invalid client input", credential: "operator", body: `{"objective_id":"o"}`, want: 400, serviceErr: &ObjectiveRequestError{Err: errors.New("control reason is required")}},
		{name: "wrapped domain error", credential: "operator", body: `{"objective_id":"o"}`, want: 409, serviceErr: &ObjectiveRequestError{Err: orchestrator.ErrConflict}},
		{name: "explicit service status", credential: "operator", body: `{"objective_id":"o"}`, want: 409, serviceErr: ErrInvalidState},
		{name: "closed store", credential: "operator", body: `{"objective_id":"o"}`, want: 500, serviceErr: controlstore.ErrClosed},
		{name: "recovery required", credential: "operator", body: `{"objective_id":"o"}`, want: 500, serviceErr: fmt.Errorf("journal: %w", controlstore.ErrRecovery)},
		{name: "unexpected server failure", credential: "operator", body: `{"objective_id":"o"}`, want: 500, serviceErr: errors.New("disk failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &objectiveTestService{err: tc.serviceErr}
			authority := AuthorityConfig{Operators: []OperatorCredential{{ID: "person", Token: "operator"}}, Attempts: []AttemptCapability{{Token: "worker", Scope: AttemptScope{BoardID: "board", WorkID: "work", AttemptID: "attempt"}, Access: AttemptPropose, ExpiresAt: time.Now().Add(time.Hour)}}, AttemptActive: func(context.Context, AttemptScope) bool { return true }}
			server, err := New(&mockService{}, NewSSEBroker(), "127.0.0.1", 0, "shared", nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)), WithAuthority(authority), WithOrchestratorService(svc))
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/objectives/o/controls", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.credential == "operator" {
				req.Header.Set(OperatorTokenHeader, tc.credential)
			} else if tc.credential != "" {
				req.Header.Set("Authorization", "Bearer "+tc.credential)
			}
			response := httptest.NewRecorder()
			server.httpSrv.Handler.ServeHTTP(response, req)
			require.Equal(t, tc.want, response.Code, response.Body.String())
			if tc.wantError != "" {
				var body map[string]string
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				require.Equal(t, tc.wantError, body["error"])
			}
			if tc.serviceErr != nil {
				require.Equal(t, 1, svc.calls)
				return
			}
			switch tc.want {
			case http.StatusOK:
				require.Equal(t, "person", svc.operator)
			case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
				require.Zero(t, svc.calls)
			}
		})
	}
}

func TestObjectiveServiceOptional(t *testing.T) {
	server, err := New(&mockService{}, NewSSEBroker(), "127.0.0.1", 0, "", nil, "", slog.Default())
	require.NoError(t, err)
	for _, path := range []string{"/api/objectives", "/api/objectives/o"} {
		response := httptest.NewRecorder()
		server.httpSrv.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil))
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
	}
	_, err = New(&mockService{}, NewSSEBroker(), "127.0.0.1", 0, "", nil, "", slog.Default(), WithOrchestratorService(&objectiveTestService{}))
	require.ErrorContains(t, err, "operator authority")
}

func TestObjectiveJSON(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{body: `{}`, want: true},
		{body: `{"objective":{"criteria":["checked"],"policy":{"revision":1}}}`, want: true},
		{body: `{"objective":{"id":"a","id":"b"}}`},
		{body: `{"text":"x","Text":"y"}`},
		{body: `null`},
		{body: `[]`},
		{body: `{`},
		{body: `{}` + strings.Repeat(" ", 8), want: true},
		{body: `{}` + `{}`},
		{body: `{"x":` + strings.Repeat("[", 66) + `0` + strings.Repeat("]", 66) + `}`},
	} {
		require.Equal(t, tc.want, validObjectiveJSON([]byte(tc.body)), tc.body)
	}
}
