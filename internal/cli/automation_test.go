package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/cli/remote"
)

func TestNewAutomation(t *testing.T) {
	for _, tc := range []struct {
		name, schedule string
		wantRequest    bool
	}{
		{"valid cron", "0 9 * * 1-5", true},
		{"valid interval", "30m", true},
		{"invalid schedule", "bad", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, "POST", r.Method)
				require.Equal(t, "/api/automations", r.URL.Path)
				var body struct {
					RequestID  string          `json:"request_id"`
					Key        string          `json:"key"`
					Definition automation.Spec `json:"definition"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Len(t, body.RequestID, 32)
				require.Equal(t, "review", body.Key)
				require.Equal(t, tc.schedule, body.Definition.Schedule)
				require.Equal(t, "UTC", body.Definition.Timezone)
				require.False(t, body.Definition.Enabled)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				require.NoError(t, json.NewEncoder(w).Encode(automation.Definition{ID: "aut-created", Key: body.Key, Spec: body.Definition}))
			}))
			defer server.Close()
			row, err := NewAutomation(remote.New(server.URL, ""), "review", automation.Spec{Name: "Review", Prompt: "Review the code", Path: "/repo", Agent: "agent", Schedule: tc.schedule})
			if tc.wantRequest {
				require.NoError(t, err)
				require.Equal(t, "aut-created", row.ID)
				require.Equal(t, 1, calls)
			} else {
				require.Error(t, err)
				require.Zero(t, calls)
			}
		})
	}
}
