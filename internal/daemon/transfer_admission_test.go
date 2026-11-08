package daemon

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestPendingTransferRejectsPrimaryAdmission(t *testing.T) {
	for _, name := range []string{"recovery exclusion", "new durable record", "recoverable transfer"} {
		t.Run(name, func(t *testing.T) {
			persisted := name != "recovery exclusion"
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			id := "tst-pending"
			path := h.writeTicket(id+".md", h.taskMD(id, "todo", "one-stage"))
			require.NoError(t, d.initialScan(h.tasksDir))
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			tk := h.readTask(id + ".md")
			d.mu.Lock()
			d.removeQueuedLocked(id)
			if !persisted {
				d.transferBlocked = map[string]bool{id: true}
			}
			d.mu.Unlock()
			switch name {
			case "new durable record":
				txn := filepath.Join(h.tasksDir, ".kontora-transfers", "transfer-"+hex.EncodeToString([]byte(id))+"-interrupted")
				require.NoError(t, os.MkdirAll(txn, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(txn, "record.json"), []byte("interrupted record"), 0o600))
			case "recoverable transfer":
				source := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(source, id+".md"), before, 0o644))
				revision, err := store.Revision(source, id)
				require.NoError(t, err)
				require.NoError(t, os.Remove(path))
				_, err = store.Transfer(store.TransferRequest{SourceDir: source, DestinationDir: h.tasksDir, ID: id, Revision: revision,
					Validate: func(*ticket.Ticket) error { return os.WriteFile(path, before, 0o644) },
				})
				require.ErrorContains(t, err, "requires recovery")
				pending, err := store.HasPendingTransfer(h.tasksDir, id)
				require.NoError(t, err)
				require.True(t, pending)
			}
			for _, action := range []struct {
				name string
				run  func() error
			}{
				{"run", func() error { return d.RunTicket(id) }},
				{"note", func() error { return d.AddNote(id, web.AddNoteRequest{Text: "must not save"}) }},
				{"summary", func() error { return d.SetSummary(id, "must not save") }},
				{"reaction", func() error { return d.SetReaction(id, "note", ":)", "actor", true) }},
				{"update", func() error { return d.UpdateTicket(id, web.UpdateTicketRequest{}) }},
				{"delete", func() error { return d.DeleteTicket(id) }},
				{"move", func() error { return d.MoveTicket(id, "open") }},
			} {
				t.Run(action.name, func(t *testing.T) {
					require.ErrorIs(t, action.run(), web.ErrInvalidState)
					got, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, before, got)
				})
			}
			err = d.withTicketMutation(func(repo app.MutationRepository) error {
				_, err := repo.Get(id)
				require.ErrorIs(t, err, app.ErrInvalidState)
				listed, err := repo.List()
				require.NoError(t, err)
				assert.Empty(t, listed)
				return err
			})
			require.NoError(t, err)
			d.mu.Lock()
			d.enqueue(tk)
			_, claimable := d.claimableLocked(id, d.ticketLog(id))
			assert.False(t, claimable)
			assert.False(t, d.queued[id])
			d.mu.Unlock()
			_, _, _, _, _, err = d.claimExecution(context.Background(), h.cfg, id, d.ticketLog(id))
			require.ErrorIs(t, err, errExecutionChanged)
			if persisted {
				require.ErrorIs(t, d.writeTicket(tk, path, notify.OriginDaemon), app.ErrInvalidState)
			}
			_, err = os.Lstat(filepath.Join(h.tasksDir, id+".notes.json"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestNewTicketPublicationWaitsForTransferMutationLock(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	id := "tst-upload"
	path := filepath.Join(h.tasksDir, id+".md")
	tk, err := ticket.ParseBytes([]byte(h.taskMD(id, "open", "one-stage")))
	require.NoError(t, err)
	started := make(chan struct{})
	finished := make(chan error, 1)
	require.NoError(t, ticket.WithBoardMutation(h.tasksDir, func() error {
		go func() {
			close(started)
			finished <- d.createTicketFile(tk, path, notify.OriginRequest)
		}()
		<-started
		select {
		case err := <-finished:
			t.Fatalf("publication bypassed transfer lock: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		return os.WriteFile(path, []byte("claimed contents"), 0o644)
	}))
	select {
	case err := <-finished:
		require.ErrorIs(t, err, os.ErrExist)
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not resume after mutation lock release")
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "claimed contents", string(got))
}
