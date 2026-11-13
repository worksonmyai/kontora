package web

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
