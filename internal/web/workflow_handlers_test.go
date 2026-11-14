package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

type releaseMockService struct {
	*mockService
	human      func(string, WorkflowReleaseRequest) (*ticket.WorkflowReceipt, error)
	deployment func(DeploymentReleaseRequest) (*ticket.WorkflowReceipt, error)
}

func (m *releaseMockService) WorkflowRelease(id string, req WorkflowReleaseRequest) (*ticket.WorkflowReceipt, error) {
	return m.human(id, req)
}
func (m *releaseMockService) DeploymentRelease(req DeploymentReleaseRequest) (*ticket.WorkflowReceipt, error) {
	return m.deployment(req)
}

func TestWorkflowReleaseStrictAPI(t *testing.T) {
	valid := `{"execution_id":"exec-1","wait_id":"wait-1","revision":0,"request_id":"request-1","reason":"Approved"}`
	for _, tc := range []struct {
		name, body string
		serviceErr error
		status     int
	}{
		{"accepted", valid, nil, 200},
		{"unicode reason", strings.Replace(valid, "Approved", `Approved \ud83d\ude80 \ufffd`, 1), nil, 200},
		{"stale", valid, ErrInvalidState, 409},
		{"not found", valid, ErrTicketNotFound, 404},
		{"empty", "", nil, 400},
		{"null", "null", nil, 400},
		{"array", "[]", nil, 400},
		{"unknown", strings.Replace(valid, `"reason"`, `"force":true,"reason"`, 1), nil, 400},
		{"duplicate", strings.Replace(valid, `"reason"`, `"wait_id":"other","reason"`, 1), nil, 400},
		{"case folding", strings.Replace(valid, "execution_id", "Execution_ID", 1), nil, 400},
		{"missing revision", strings.Replace(valid, `"revision":0,`, "", 1), nil, 400},
		{"null revision", strings.Replace(valid, `"revision":0`, `"revision":null`, 1), nil, 400},
		{"negative revision", strings.Replace(valid, `"revision":0`, `"revision":-1`, 1), nil, 400},
		{"overflow revision", strings.Replace(valid, `"revision":0`, `"revision":18446744073709551616`, 1), nil, 400},
		{"fraction revision", strings.Replace(valid, `"revision":0`, `"revision":0.1`, 1), nil, 400},
		{"empty reason", strings.Replace(valid, "Approved", "  ", 1), nil, 400},
		{"operator spoof", strings.Replace(valid, `"reason"`, `"operator":"alice","reason"`, 1), nil, 400},
		{"NUL", strings.Replace(valid, "Approved", `bad\u0000text`, 1), nil, 400},
		{"invalid unicode", strings.Replace(valid, "Approved", `bad\ud800text`, 1), nil, 400},
		{"trailing object", valid + " {}", nil, 400},
		{"oversize", strings.Replace(valid, "Approved", strings.Repeat("a", 64<<10), 1), nil, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			svc := &releaseMockService{mockService: &mockService{}, human: func(id string, req WorkflowReleaseRequest) (*ticket.WorkflowReceipt, error) {
				calls++
				assert.Equal(t, "t-001", id)
				assert.Equal(t, "exec-1", req.ExecutionID)
				assert.Equal(t, "wait-1", req.WaitID)
				assert.Empty(t, req.Operator)
				return &ticket.WorkflowReceipt{ID: "receipt-1", RequestID: req.RequestID, AcceptedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}, tc.serviceErr
			}}
			srv := startHandlerTestServer(t, svc)
			res := post(t, srv, "/api/tickets/t-001/workflow-release", tc.body)
			assert.Equal(t, tc.status, res.statusCode, res.body)
			if tc.status == 400 {
				assert.Zero(t, calls)
			} else {
				assert.Equal(t, 1, calls)
			}
			if tc.status == 200 {
				assert.JSONEq(t, `{"id":"receipt-1","request_id":"request-1","accepted_at":"2030-01-01T00:00:00Z"}`, res.body)
			}
		})
	}
}

func TestDeploymentReleaseStrictAPI(t *testing.T) {
	valid := `{"repository":"/repo","environment":"production","commit":"` + strings.Repeat("a", 40) + `","event_id":"deployment-1"}`
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"accepted", valid, 200},
		{"missing event", strings.Replace(valid, `,"event_id":"deployment-1"`, "", 1), 400},
		{"no ticket", strings.Replace(valid, `"repository"`, `"ticket_id":"t-1","repository"`, 1), 400},
		{"abbreviation", strings.Replace(valid, strings.Repeat("a", 40), "abcdef", 1), 400},
		{"ref", strings.Replace(valid, strings.Repeat("a", 40), "HEAD", 1), 400},
		{"uppercase", strings.Replace(valid, strings.Repeat("a", 40), strings.Repeat("A", 40), 1), 400},
		{"duplicate", strings.Replace(valid, `"environment"`, `"event_id":"other","environment"`, 1), 400},
		{"null field", strings.Replace(valid, `"production"`, `null`, 1), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			svc := &releaseMockService{mockService: &mockService{}, deployment: func(req DeploymentReleaseRequest) (*ticket.WorkflowReceipt, error) {
				calls++
				assert.Equal(t, "production", req.Environment)
				assert.Empty(t, req.Operator)
				return &ticket.WorkflowReceipt{ID: "deployment-1"}, nil
			}}
			res := post(t, startHandlerTestServer(t, svc), "/api/workflow-events/deployment", tc.body)
			assert.Equal(t, tc.status, res.statusCode, res.body)
			if tc.status == 400 {
				assert.Zero(t, calls)
			} else {
				assert.Equal(t, 1, calls)
			}
		})
	}
}

func TestWorkflowReleaseOperatorAttribution(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared token", true: "operator"}[named], func(t *testing.T) {
			got := "unset"
			svc := &releaseMockService{mockService: &mockService{}, human: func(_ string, req WorkflowReleaseRequest) (*ticket.WorkflowReceipt, error) {
				got = req.Operator
				return &ticket.WorkflowReceipt{ID: "receipt"}, nil
			}}
			s := &Server{svc: svc}
			var authority *AuthorityConfig
			if named {
				authority = &AuthorityConfig{Operators: []OperatorCredential{{ID: "alice", Token: "person-token"}}}
			}
			handler, err := AuthorityMiddleware("shared-token", authority, http.HandlerFunc(s.handleWorkflowRelease))
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/api/tickets/t-1/workflow-release", strings.NewReader(`{"execution_id":"exec","wait_id":"wait","revision":7,"request_id":"request","reason":"Approved"}`))
			req.Header.Set("Authorization", "Bearer shared-token")
			if named {
				req.Header.Set(OperatorTokenHeader, "person-token")
			}
			req.SetPathValue("id", "t-1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			assert.Equal(t, 200, response.Code, response.Body.String())
			if named {
				assert.Equal(t, "alice", got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}

func TestWorkflowReleaseRequestValidation(t *testing.T) {
	assert.NoError(t, (WorkflowReleaseRequest{ExecutionID: "e", WaitID: "w", RequestID: "r", Reason: "Approved"}).Validate())
	assert.Error(t, (WorkflowReleaseRequest{ExecutionID: "e", WaitID: "w", RequestID: "r"}).Validate())
	assert.NoError(t, (DeploymentReleaseRequest{Repository: "/repo", Environment: "prod", Commit: strings.Repeat("f", 64), EventID: "event"}).Validate())
	assert.Error(t, (DeploymentReleaseRequest{Repository: "/repo", Environment: "prod", Commit: strings.Repeat("f", 40), EventID: "\x00"}).Validate())
}

func TestWorkflowActions(t *testing.T) {
	for _, tc := range []struct {
		name, action, body string
		err                error
		status             int
	}{
		{name: "restart", action: "workflow-restart", status: http.StatusOK},
		{name: "recover", action: "workflow-recover", body: `{"execution_id":"workflow-1","revision":7}`, status: http.StatusOK},
		{name: "stale recovery", action: "workflow-recover", body: `{"execution_id":"workflow-1","revision":7}`, err: ErrInvalidState, status: http.StatusConflict},
		{name: "missing identity", action: "workflow-recover", body: `{"revision":7}`, status: http.StatusBadRequest},
		{name: "missing revision", action: "workflow-recover", body: `{"execution_id":"workflow-1"}`, status: http.StatusBadRequest},
		{name: "negative revision", action: "workflow-recover", body: `{"execution_id":"workflow-1","revision":-1}`, status: http.StatusBadRequest},
		{name: "unknown field", action: "workflow-recover", body: `{"execution_id":"workflow-1","revision":7,"force":true}`, status: http.StatusBadRequest},
		{name: "trailing object", action: "workflow-recover", body: `{"execution_id":"workflow-1","revision":7} {}`, status: http.StatusBadRequest},
		{name: "missing ticket", action: "workflow-restart", err: ErrTicketNotFound, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			svc := &mockService{tickets: []TicketInfo{{ID: "t-001", Status: "paused"}}, actionFn: func(id string) error {
				calls++
				assert.Equal(t, "t-001", id)
				return tc.err
			}}
			srv := startHandlerTestServer(t, svc)
			res := post(t, srv, "/api/tickets/t-001/"+tc.action, tc.body)
			assert.Equal(t, tc.status, res.statusCode)
			if tc.status == http.StatusBadRequest {
				assert.Zero(t, calls)
			} else {
				assert.Equal(t, 1, calls)
			}
		})
	}
}
