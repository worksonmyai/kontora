package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/watcher"
)

func TestAtomicTicketWatcherEvents(t *testing.T) {
	cases := []struct {
		name      string
		op        watcher.Op
		remove    bool
		reconcile bool
	}{
		{name: "replacement reported as removal", op: watcher.OpRemoved},
		{name: "replacement reported as change", op: watcher.OpChanged},
		{name: "removed after creation event", op: watcher.OpChanged, remove: true},
		{name: "removed after replacement", op: watcher.OpRemoved, remove: true},
		{name: "replacement retained without watcher event", reconcile: true},
		{name: "removed without watcher event", reconcile: true, remove: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			path := h.writeTicket("tst-atomic.md", h.taskMD("tst-atomic", "open", "one-stage"))
			require.NoError(t, d.initialScan(h.tasksDir))
			tk := h.readTask("tst-atomic.md")
			require.NoError(t, os.Chmod(path, 0o600))
			require.NoError(t, tk.SetField("summary", "replacement"))
			require.NoError(t, d.writeTicket(tk, path, notify.OriginDaemon))
			st, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
			if tc.remove {
				require.NoError(t, os.Remove(path))
			}
			if tc.reconcile {
				d.reconcileRegisteredRemovals()
			} else {
				d.handleEvent(watcher.Event{Path: path, Op: tc.op})
			}
			d.mu.Lock()
			_, known := d.tickets["tst-atomic"]
			d.mu.Unlock()
			assert.Equal(t, !tc.remove, known)
			if !tc.remove {
				got, err := ticket.ParseFile(path)
				require.NoError(t, err)
				assert.Equal(t, "replacement", got.Summary)
			}
		})
	}
}
