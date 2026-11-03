package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestFinishAgentExitConcurrentMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *Daemon, *ticket.Ticket)
		queued bool
	}{
		{name: "note", queued: true, change: func(t *testing.T, d *Daemon, tk *ticket.Ticket) {
			require.NoError(t, d.AddNote(tk.ID, web.AddNoteRequest{Text: "retain this note"}))
		}},
		{name: "pause", change: func(t *testing.T, d *Daemon, tk *ticket.Ticket) {
			require.NoError(t, tk.SetField("status", "paused"))
			data, err := tk.Marshal()
			require.NoError(t, err)
			h := d.tickets[tk.ID]
			require.NoError(t, ticket.WriteOffline(h.filePath, data))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			path := h.writeTicket("tst-exit.md", h.taskMD("tst-exit", "todo", "one-stage"))
			tk, err := ticket.ParseFile(path)
			require.NoError(t, err)
			d.tickets[tk.ID] = newTicketState(tk, path)
			d.running[tk.ID] = func() {}
			written := tk.Clone()
			tc.change(t, d, tk)
			d.finishAgentExit(context.Background(), written, handleExitParams{log: testLogger(t), ticketID: tk.ID, filePath: path}, pipeline.ActionAdvance, false)
			require.Equal(t, tc.queued, d.queued[tk.ID])
			if tc.queued {
				require.Contains(t, d.tickets[tk.ID].ticket.Body, "retain this note")
			}
		})
	}
}
