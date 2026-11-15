package daemon

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestPlannotatorReworkPreservesCompletedConcurrentUpdate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notify []string
		want   []string
	}{
		{"Off", []string{}, []string{}},
		{"saved statuses", []string{"done"}, []string{"done"}},
		{"other edits with unset notify", nil, []string{"paused"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS != "linux" {
				if _, err := exec.LookPath("lsof"); err != nil {
					t.Skip("lock descriptor observation requires /proc or lsof")
				}
			}
			h := newHarness(t)
			h.cfg.Notifications.DefaultStatuses = []string{"paused"}
			h.cfg.Notifications.Channels = map[string]config.NotifyChannel{
				"ops": {Type: config.NotifyWebhook, URL: "https://example.invalid"},
			}
			h.cfg.AutoPickUp = new(false)
			d := h.newDaemon(h.cfg)
			writer := h.newDaemon(h.cfg)
			id := "tst-rework-edit"
			path := h.writeTicket(id+".md", h.taskMD(id, "human_review", "two-stage"))
			for _, daemon := range []*Daemon{d, writer} {
				daemon.tickets[id] = newTicketState(h.readTask(id+".md"), path)
			}
			require.Nil(t, h.readTask(id+".md").Notify.Statuses)

			// Separate writers share the board lock, but have independent cache and self-write locks.
			writer.selfWritesMu.Lock()
			releaseWriter := sync.OnceFunc(writer.selfWritesMu.Unlock)
			defer releaseWriter()
			d.selfWritesMu.Lock()
			releaseRework := sync.OnceFunc(d.selfWritesMu.Unlock)
			defer releaseRework()

			dir, err := filepath.EvalSymlinks(h.tasksDir)
			require.NoError(t, err)
			opened := func(name string) int {
				count, err := mutationLockDescriptors(filepath.Join(dir, ".kontora", name))
				require.NoError(t, err)
				return count
			}
			updated := make(chan error, 1)
			go func() {
				updated <- writer.UpdateTicket(id, web.UpdateTicketRequest{
					Notify:         tc.notify,
					NotifyChannels: []string{"ops"},
					Description:    new("# Edited description\n\nKeep this edit."),
					BaseBranch:     new("operator-base"),
				})
			}()
			// UpdateTicket holds routing admission twice only after acquiring the board mutation lock.
			require.Eventually(t, func() bool { return opened("board.lock") == 2 }, 5*time.Second, 10*time.Millisecond)

			reworked := make(chan error, 1)
			go func() { reworked <- d.transitionToRework(id) }()
			// Rework either waits for the mutation lock or has read the old ticket and entered its write path.
			require.Eventually(t, func() bool {
				return opened("mutation.lock") == 2 || opened("board.lock") == 3
			}, 5*time.Second, 10*time.Millisecond)

			releaseWriter()
			select {
			case err := <-updated:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent update did not finish")
			}
			completed := h.readTask(id + ".md")
			require.Equal(t, tc.notify, completed.Notify.Statuses)
			require.Equal(t, []string{"ops"}, completed.NotifyChannels)
			require.Contains(t, completed.Body, "Keep this edit.")
			require.Equal(t, "operator-base", completed.BaseBranch)
			releaseRework()
			select {
			case err := <-reworked:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("rework did not finish after the concurrent update")
			}

			stored := h.readTask(id + ".md")
			for _, got := range []*ticket.Ticket{stored, d.tickets[id].ticket} {
				assert.Equal(t, tc.want, got.Notify.Statuses)
				assert.Equal(t, []string{"ops"}, got.NotifyChannels)
				assert.Contains(t, got.Body, "Keep this edit.")
				assert.Equal(t, "operator-base", got.BaseBranch)
				assert.Equal(t, ticket.StatusTodo, got.Status)
				assert.Equal(t, config.ReworkStageName, got.Stage)
			}
			require.Len(t, d.queue, 1)
			assert.True(t, d.queue[0].explicit, "rework must remain explicit with auto_pick_up disabled")
		})
	}
}
