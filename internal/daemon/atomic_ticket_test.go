package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/watcher"
)

func TestAtomicTicketWriterCannotRecreateTransferredSource(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	path := h.writeTicket("tst-transfer.md", h.taskMD("tst-transfer", "open", "one-stage"))
	require.NoError(t, d.initialScan(h.tasksDir))
	stale := h.readTask("tst-transfer.md")
	revision, err := store.Revision(h.tasksDir, stale.ID)
	require.NoError(t, err)
	destination := t.TempDir()
	_, err = store.Transfer(store.TransferRequest{SourceDir: h.tasksDir, DestinationDir: destination, ID: stale.ID, Revision: revision})
	require.NoError(t, err)
	require.NoError(t, stale.SetField("status", string(ticket.StatusTodo)))
	err = d.writeTicket(stale, path, notify.OriginDaemon)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Lstat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
	got, err := ticket.ParseFile(filepath.Join(destination, stale.ID+".md"))
	require.NoError(t, err)
	assert.Equal(t, ticket.StatusOpen, got.Status)
}

func TestAtomicTicketWriterRefusesNonregularFile(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	path := h.writeTicket("tst-unsafe.md", h.taskMD("tst-unsafe", "open", "one-stage"))
	tk := h.readTask("tst-unsafe.md")
	require.NoError(t, os.Remove(path))
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("external"), 0o644))
	require.NoError(t, os.Symlink(target, path))
	require.Error(t, d.writeTicket(tk, path, notify.OriginDaemon))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "external", string(got))
	_, err = os.Readlink(path)
	require.NoError(t, err)
}

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
