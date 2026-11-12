package web

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func TestExecutionActiveProjection(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		active       bool
	}{
		{"legacy", "status: in_progress\n", true},
		{"active custom", "status: awaiting_data\nrunning_status: awaiting_data\n", true},
		{"parked custom", "status: awaiting_data\n", false},
		{"stale snapshot", "status: awaiting_data\nrunning_status: other\n", false},
		{"passive epic", "status: in_progress\nkind: epic\n", false},
	} {
		for _, detail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/detail=%t", tc.name, detail), func(t *testing.T) {
				tkt, err := ticket.ParseBytes([]byte("---\nid: tst-001\n" + tc.fields + "---\n# Work\n"))
				require.NoError(t, err)
				info := TicketInfoFromView(app.BuildView(&config.Config{}, tkt, detail))
				assert.Equal(t, string(tkt.Status), info.Status)
				assert.Equal(t, tc.active, info.ExecutionActive)
				assert.Equal(t, tc.active, info.IsRunning())
				for _, payload := range []any{info, TicketEvent{Type: "ticket_updated", Ticket: info}, TicketChild{ID: info.ID, Status: info.Status, ExecutionActive: info.ExecutionActive}} {
					encoded, err := json.Marshal(payload)
					require.NoError(t, err)
					assert.Contains(t, string(encoded), fmt.Sprintf(`"execution_active":%t`, tc.active))
					assert.Contains(t, string(encoded), fmt.Sprintf(`"status":%q`, tkt.Status))
					assert.NotContains(t, string(encoded), "running_status")
				}
			})
		}
	}
}
