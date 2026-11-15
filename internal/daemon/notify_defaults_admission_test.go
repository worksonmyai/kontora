package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestRefusedExecutionDoesNotApplyNotifyDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		running      bool
		run          func(*Daemon, string) error
	}{
		{"run paused", "paused", false, (*Daemon).RunTicket},
		{"run claimed before status write", "todo", true, (*Daemon).RunTicket},
		{"retry todo", "todo", false, (*Daemon).RetryTicket},
		{"skip running", "in_progress", false, (*Daemon).SkipStage},
		{"init paused", "paused", false, func(d *Daemon, id string) error { return d.InitTicket(id, web.InitTicketRequest{}) }},
		{"annotation moved", "human_review", false, (*Daemon).parkForAnnotation},
		{"annotation claimed before status write", "open", true, (*Daemon).parkForAnnotation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Notifications.DefaultStatuses = []string{"paused"}
			d := h.newDaemon(h.cfg)
			id := "tst-refused"
			path := filepath.Join(h.tasksDir, id+".md")
			md := fmt.Sprintf("---\nid: %s\nkontora: true\npath: %q\nstatus: %s\npipeline: two-stage\nstage: step1\n---\n# Test\n", id, h.repoDir, tc.status)
			h.writeTicket(id+".md", md)
			d.tickets[id] = newTicketState(h.readTask(id+".md"), path)
			if tc.running {
				d.running[id] = func() {}
			}
			require.ErrorIs(t, tc.run(d, id), web.ErrInvalidState)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, md, string(got))
			assert.Nil(t, d.tickets[id].ticket.Notify.Statuses)
			assert.Empty(t, d.queue)
		})
	}
}
