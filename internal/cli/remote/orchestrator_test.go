package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestObjectiveClient(t *testing.T) {
	for _, tc := range []struct{ operation, id string }{
		{"list", "objective"}, {"show", "objective"}, {"pause", "objective"}, {"assign", "objective"}, {"stale", "objective"},
		{"show", "objective%2Fone"}, {"pause", "objective%2Fone"}, {"assign", "objective%2Fone"},
		{"show", "objective?one"}, {"pause", "objective#one"}, {"assign", "objective.one"},
	} {
		t.Run(tc.operation+"/"+tc.id, func(t *testing.T) {
			operation := tc.operation
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, "operator-secret", r.Header.Get(web.OperatorTokenHeader))
				require.Equal(t, "Bearer shared", r.Header.Get("Authorization"))
				switch operation {
				case "list":
					require.Equal(t, "/api/objectives", r.URL.Path)
					require.NoError(t, json.NewEncoder(w).Encode(web.ObjectiveList{BoardID: "board", Revision: 4}))
				case "show":
					require.Equal(t, "/api/objectives/"+url.PathEscape(tc.id), r.URL.EscapedPath())
					require.NoError(t, json.NewEncoder(w).Encode(web.ObjectiveSnapshot{BoardID: "board", Revision: 4, VerificationPolicies: []orchestrator.VerificationPolicy{{ID: "policy", ObjectiveID: tc.id}}, Reviews: []orchestrator.CombinedReview{{ID: "review", ObjectiveID: tc.id}}}))
				default:
					require.Equal(t, http.MethodPost, r.Method)
					if operation == "assign" {
						require.Equal(t, "/api/objectives", r.URL.Path)
					} else {
						require.Equal(t, "/api/objectives/"+url.PathEscape(tc.id)+"/controls", r.URL.EscapedPath())
					}
					var req web.ObjectiveControlRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					require.Equal(t, "request", req.RequestID)
					require.Equal(t, tc.id, req.ObjectiveID)
					require.EqualValues(t, 4, req.ExpectedRevision)
					if operation == "stale" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"error":"stale control state"}`))
						return
					}
					require.NoError(t, json.NewEncoder(w).Encode(web.ObjectiveControlResponse{BoardID: "board", Revision: 5}))
				}
			}))
			defer server.Close()
			original := New(server.URL, "shared")
			client := original.WithOperatorToken("operator-secret")
			require.Empty(t, original.operatorToken)
			switch operation {
			case "list":
				result, err := client.Objectives()
				require.NoError(t, err)
				require.EqualValues(t, 4, result.Revision)
			case "show":
				result, err := client.Objective(tc.id)
				require.NoError(t, err)
				require.EqualValues(t, 4, result.Revision)
				require.Equal(t, []orchestrator.VerificationPolicy{{ID: "policy", ObjectiveID: tc.id}}, result.VerificationPolicies)
				require.Equal(t, []orchestrator.CombinedReview{{ID: "review", ObjectiveID: tc.id}}, result.Reviews)
			default:
				_, err := client.ControlObjective(web.ObjectiveControlRequest{BoardID: "board", RequestID: "request", ObjectiveID: tc.id, ExpectedRevision: 4, Operation: operation})
				if operation == "stale" {
					require.ErrorContains(t, err, "stale control state")
				} else {
					require.NoError(t, err)
				}
			}
			require.Equal(t, 1, calls, "stale or failed controls must not be retried")
		})
	}
}

func TestObjectiveClientRejectsInvalidTargets(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client := New(server.URL, "").WithOperatorToken("operator")
	for _, id := range []string{"", ".", "..", "../other", "a/b", "a\\b", "a\n", "a\r", "a\t", "a b", "a\x00"} {
		t.Run(id, func(t *testing.T) {
			_, err := client.Objective(id)
			require.ErrorContains(t, err, "invalid objective id")
			for _, operation := range []string{"assign", "pause"} {
				_, err := client.ControlObjective(web.ObjectiveControlRequest{BoardID: "board", RequestID: "r", ObjectiveID: id, Operation: operation})
				require.ErrorContains(t, err, "objective identities are required")
			}
		})
	}
	require.Zero(t, calls)
	_, err := New("http://127.0.0.1:1", "shared").ControlObjective(web.ObjectiveControlRequest{BoardID: "board", RequestID: "r", ObjectiveID: "o"})
	require.ErrorContains(t, err, "operator credential")
}
