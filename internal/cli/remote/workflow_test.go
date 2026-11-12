package remote

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowClientActions(t *testing.T) {
	for _, action := range []string{"restart", "recover"} {
		t.Run(action, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/tickets/graph-1/workflow-"+action, r.URL.Path)
				assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
				if action == "recover" {
					assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
					var body struct {
						ExecutionID string `json:"execution_id"`
						Revision    uint64 `json:"revision"`
					}
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					assert.Equal(t, "execution", body.ExecutionID)
					assert.Equal(t, uint64(18446744073709551615), body.Revision)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			client := New(srv.URL, "secret")
			if action == "restart" {
				require.NoError(t, client.WorkflowRestart("graph-1"))
			} else {
				require.NoError(t, client.WorkflowRecover("graph-1", "execution", 18446744073709551615))
			}
		})
	}
}

func TestWorkflowClientPropagatesConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprint(w, `{"error":"workflow revision changed"}`)
	}))
	defer srv.Close()
	client := New(srv.URL, "")
	assert.EqualError(t, client.WorkflowRecover("graph-1", "execution", 12), "workflow revision changed")
	assert.EqualError(t, client.WorkflowRestart("graph-1"), "workflow revision changed")
}
