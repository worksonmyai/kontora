package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestCustomActivityLifecycleGuards(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, operation := range []string{"retry", "skip", "set-stage", "pause", "cancel"} {
			t.Run(fmt.Sprintf("active=%t/%s", active, operation), func(t *testing.T) {
				repo := newMemRepo()
				marker := ""
				if active {
					marker = "running_status: awaiting_data\n"
				}
				repo.add("tst-001", "---\nid: tst-001\nkontora: true\nstatus: awaiting_data\n"+marker+"pipeline: default\nstage: code\nattempt: 2\n---\n# Work\n")
				before, err := repo.tickets["tst-001"].Ticket.Marshal()
				require.NoError(t, err)
				rt := &spyRuntime{}
				cfg := testCfg()
				cfg.Statuses = []string{"awaiting_data"}
				svc := New(Static(cfg), repo, rt)
				switch operation {
				case "retry":
					_, err = svc.Retry("tst-001")
				case "skip":
					_, err = svc.Skip("tst-001")
				case "set-stage":
					_, err = svc.SetStage("tst-001", "review")
				case "pause":
					_, err = svc.SetStatus("tst-001", ticket.StatusPaused)
				case "cancel":
					_, err = svc.SetStatus("tst-001", ticket.StatusCancelled)
				}
				if active && operation != "pause" && operation != "cancel" {
					require.ErrorIs(t, err, ErrInvalidState)
					after, marshalErr := repo.tickets["tst-001"].Ticket.Marshal()
					require.NoError(t, marshalErr)
					assert.Equal(t, before, after)
					assert.Empty(t, repo.saved)
					assert.Equal(t, &spyRuntime{}, rt)
					return
				}
				require.NoError(t, err)
				if operation == "pause" || operation == "cancel" {
					assert.False(t, repo.tickets["tst-001"].Ticket.IsRunning())
					assert.Empty(t, repo.tickets["tst-001"].Ticket.RunningStatus)
					assert.Equal(t, []string{"tst-001"}, rt.cancelled)
				}
			})
		}
	}
}

func TestBuildViewExecutionActive(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         bool
	}{
		{"legacy", "status: in_progress\n", true},
		{"active", "status: awaiting_data\nrunning_status: awaiting_data\n", true},
		{"inherited active", "status: awaiting_data\n<<: {running_status: awaiting_data}\n", true},
		{"parked", "status: awaiting_data\n", false},
		{"stale", "status: awaiting_data\nrunning_status: other\n", false},
		{"passive epic", "status: in_progress\nkind: epic\n", false},
		{"agent epic", "status: awaiting_data\nrunning_status: awaiting_data\nkind: epic\nepic_execution: agent\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tkt, err := ticket.ParseBytes([]byte("---\nid: tst-001\n" + tc.fields + "---\n# Work\n"))
			require.NoError(t, err)
			for _, detail := range []bool{false, true} {
				// No custom declaration: activity comes from the captured ticket state.
				view := BuildView(testCfg(), tkt, detail)
				assert.Equal(t, tc.want, view.ExecutionActive)
				assert.Equal(t, string(tkt.Status), view.Status)
			}
		})
	}
}
