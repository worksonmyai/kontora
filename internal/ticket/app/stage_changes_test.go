package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestStageChangesRejectInvalidState(t *testing.T) {
	for _, operation := range []string{"skip", "set-stage"} {
		for _, tc := range []struct {
			name, fields string
		}{
			{"running", "status: in_progress\n"},
			{"custom active", "status: awaiting_data\nrunning_status: awaiting_data\n"},
			{"inherited active", "status: awaiting_data\n<<: {running_status: awaiting_data}\n"},
			{"archived", "status: archived\n"},
			{"epic", "status: paused\nkind: epic\n"},
			{"uninitialized", "status: paused\nkontora: false\n"},
			{"uninitialized running", "status: in_progress\nkontora: false\n"},
		} {
			if operation == "set-stage" && tc.name == "uninitialized" {
				continue
			}
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				repo := newMemRepo()
				initialized := "kontora: true\n"
				if tc.name == "uninitialized" || tc.name == "uninitialized running" {
					initialized = ""
				}
				repo.add("tst-001", "---\nid: tst-001\n"+initialized+tc.fields+"pipeline: default\nstage: code\nattempt: 3\n---\n# Test\n")
				before, err := repo.tickets["tst-001"].Ticket.Marshal()
				require.NoError(t, err)
				rt := &spyRuntime{}
				svc := New(Static(testCfg()), repo, rt)
				if operation == "skip" {
					_, err = svc.Skip("tst-001")
				} else {
					_, err = svc.SetStage("tst-001", "review")
				}
				require.ErrorIs(t, err, ErrInvalidState)
				if tc.name == "running" {
					assert.ErrorContains(t, err, "pause first")
				}
				after, err := repo.tickets["tst-001"].Ticket.Marshal()
				require.NoError(t, err)
				assert.Equal(t, before, after)
				assert.Empty(t, repo.saved)
				assert.Equal(t, &spyRuntime{}, rt)
			})
		}
	}
}

func TestStageChangesValidateFreshStateInsideTransaction(t *testing.T) {
	for _, operation := range []string{"skip", "set-stage"} {
		t.Run(operation, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("tst-001", "---\nid: tst-001\nkontora: true\nstatus: paused\npipeline: default\nstage: code\n---\n")
			repo.beforeMutation = func() {
				require.NoError(t, repo.tickets["tst-001"].Ticket.SetField("status", "in_progress"))
			}
			rt := &spyRuntime{}
			svc := New(Static(testCfg()), repo, rt)
			var err error
			if operation == "skip" {
				_, err = svc.Skip("tst-001")
			} else {
				_, err = svc.SetStage("tst-001", "review")
			}
			require.ErrorIs(t, err, ErrInvalidState)
			assert.Equal(t, ticket.StatusInProgress, repo.tickets["tst-001"].Ticket.Status)
			assert.Equal(t, "code", repo.tickets["tst-001"].Ticket.Stage)
			assert.Empty(t, repo.saved)
			assert.Equal(t, &spyRuntime{}, rt)
		})
	}
}

func TestSetStagePreservesStoppedState(t *testing.T) {
	for _, tc := range []struct {
		status  string
		managed bool
	}{
		{"open", true}, {"todo", true}, {"paused", true}, {"human_review", true}, {"done", true}, {"cancelled", true},
		{"open", false},
	} {
		t.Run(fmt.Sprintf("%s/managed=%t", tc.status, tc.managed), func(t *testing.T) {
			status := tc.status
			repo := newMemRepo()
			repo.add("tst-001", fmt.Sprintf("---\nid: tst-001\nkontora: %t\nstatus: %s\npipeline: default\nstage: code\nattempt: 3\nlast_error: keep me\nsummary: keep summary\n---\n# Body\n", tc.managed, status))
			rt := &spyRuntime{}
			svc := New(Static(testCfg()), repo, rt)
			result, err := svc.SetStage("tst", "review")
			require.NoError(t, err)
			assert.Equal(t, status, result.Status)
			stored := repo.tickets["tst-001"].Ticket
			assert.Equal(t, "review", stored.Stage)
			assert.Equal(t, tc.managed, stored.Kontora)
			assert.Equal(t, ticket.Status(status), stored.Status)
			assert.Equal(t, 3, stored.Attempt)
			assert.Equal(t, "keep me", stored.LastError)
			assert.Equal(t, "keep summary", stored.Summary)
			assert.Empty(t, rt.enqueued)
			assert.Empty(t, rt.cancelled)
			assert.Equal(t, []string{"tst-001"}, rt.updated)
		})
	}
}

func TestSetStageRejectsUnknownStageAndPipeline(t *testing.T) {
	for _, tc := range []struct{ pipeline, stage string }{
		{"default", "missing"}, {"missing", "review"}, {"", "review"},
	} {
		t.Run(tc.pipeline+"/"+tc.stage, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("tst-001", fmt.Sprintf("---\nid: tst-001\nkontora: true\nstatus: paused\npipeline: %s\nstage: code\n---\n", tc.pipeline))
			rt := &spyRuntime{}
			_, err := New(Static(testCfg()), repo, rt).SetStage("tst-001", tc.stage)
			require.ErrorIs(t, err, ErrInvalidState)
			assert.Empty(t, repo.saved)
			assert.Equal(t, &spyRuntime{}, rt)
		})
	}
}
