package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestNotifyDefaultsMutation(t *testing.T) {
	for _, boundary := range []struct {
		name, status string
		run          func(*Service) (Result, error)
	}{
		{"init", "open", func(s *Service) (Result, error) { return s.Init("tst-001", InitRequest{}) }},
		{"retry", "paused", func(s *Service) (Result, error) { return s.Retry("tst-001") }},
		{"status todo", "open", func(s *Service) (Result, error) { return s.SetStatus("tst-001", ticket.StatusTodo) }},
		{"skip", "paused", func(s *Service) (Result, error) { return s.Skip("tst-001") }},
	} {
		for _, mode := range []string{"fresh Off wins", "save failure"} {
			t.Run(boundary.name+"/"+mode, func(t *testing.T) {
				repo := newMemRepo()
				repo.add("tst-001", "---\nid: tst-001\nkontora: true\nstatus: "+boundary.status+"\npipeline: default\nstage: code\n---\n# Test\n")
				cfg := testCfg()
				cfg.Notifications.DefaultStatuses = []string{"paused"}
				rt := &spyRuntime{}
				if mode == "fresh Off wins" {
					repo.beforeMutation = func() { require.NoError(t, repo.tickets["tst-001"].Ticket.SetField("notify", []string{})) }
				} else {
					repo.saveErr = errors.New("save failed")
				}
				_, err := boundary.run(New(Static(cfg), repo, rt))
				got := repo.tickets["tst-001"].Ticket
				if mode == "fresh Off wins" {
					require.NoError(t, err)
					assert.Equal(t, []string{}, got.Notify.Statuses)
					assert.Equal(t, []string{"tst-001"}, repo.saved)
				} else {
					require.ErrorIs(t, err, repo.saveErr)
					assert.Nil(t, got.Notify.Statuses)
					assert.Equal(t, ticket.Status(boundary.status), got.Status)
					assert.Empty(t, repo.saved)
					assert.Empty(t, rt.enqueued)
					assert.Empty(t, rt.updated)
				}
			})
		}
	}
}
