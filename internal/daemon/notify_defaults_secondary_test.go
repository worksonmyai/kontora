package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestSecondaryNotifyIntentSurvivesEditingAndClaim(t *testing.T) {
	for _, choice := range []struct {
		name, field string
		want        []string
	}{
		{"unset", "", nil},
		{"Off", "notify: []\n", []string{}},
		{"saved", "notify: [done]\n", []string{"done"}},
	} {
		t.Run(choice.name, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			h.cfg.SecondaryTicketsDirs = []string{dir}
			h.cfg.Notifications.DefaultStatuses = []string{"paused"}
			h.cfg.Notifications.Channels = map[string]config.NotifyChannel{"ops": {Type: config.NotifyWebhook, URL: "https://example.invalid"}}
			path := filepath.Join(dir, "tst-notify.md")
			require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("---\nid: tst-notify\nstatus: open\npath: %q\n%s---\n# Secondary\n", h.repoDir, choice.field)), 0o644))
			d := h.newDaemon(h.cfg)
			stop, err := d.startSecondary(context.Background())
			require.NoError(t, err)
			defer stop()
			source := d.ListTicketSources()[0].ID
			for _, channels := range [][]string{{"ops"}, {}} {
				updated, err := d.UpdateSecondaryTicket(source, "tst-notify", web.UpdateTicketRequest{NotifyChannels: channels})
				require.NoError(t, err)
				assert.Equal(t, choice.want, updated.Notify)
				tk, err := ticket.ParseFile(path)
				require.NoError(t, err)
				assert.Equal(t, choice.want, tk.Notify.Statuses)
				if len(channels) == 0 {
					assert.Nil(t, tk.NotifyChannels)
				} else {
					assert.Equal(t, channels, tk.NotifyChannels)
				}
			}
			info, err := d.GetSecondaryTicket(source, "tst-notify")
			require.NoError(t, err)
			claimed, err := d.ClaimSecondaryTicket(source, "tst-notify", info.Revision)
			require.NoError(t, err)
			assert.Equal(t, choice.want, claimed.Notify)
			tk := h.readTask("tst-notify.md")
			assert.Equal(t, choice.want, tk.Notify.Statuses)
			assert.Empty(t, d.queue)
			assert.Empty(t, d.running)
		})
	}
}
