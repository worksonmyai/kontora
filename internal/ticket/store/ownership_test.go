package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func TestOfflineWritesSerializeWithBoardAuthority(t *testing.T) {
	dir := t.TempDir()
	authority := controlstore.Authority{BoardID: "board", OwnerID: "owner", StateDir: t.TempDir()}
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(dir, alias))
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- ticket.WithOfflineWrite(dir, func() error { close(entered); <-release; return nil }) }()
	<-entered
	board, err := controlstore.AcquireBoard(alias, authority)
	require.ErrorIs(t, err, controlstore.ErrLocked)
	require.Nil(t, board)
	close(release)
	require.NoError(t, <-done)
	board, err = controlstore.AcquireBoard(alias, authority)
	require.NoError(t, err)
	require.ErrorIs(t, ticket.WithOfflineWrite(dir, func() error { t.Fatal("write ran under authority"); return nil }), ticket.ErrManaged)
	require.NoError(t, board.Close())
	require.ErrorIs(t, ticket.WithOfflineWrite(alias, func() error { t.Fatal("persisted authority was ignored"); return nil }), ticket.ErrManaged)
}

func TestDaemonRepoListDoesNotExposeCachePointers(t *testing.T) {
	parsed, err := ticket.ParseBytes([]byte("---\nid: tst-001\ndeps: [original]\ncustom: [preserved]\n---\n# Ticket\n"))
	require.NoError(t, err)
	var mu sync.Mutex
	repo := NewDaemonRepo(DaemonRepoCallbacks{ListTickets: func() []*app.StoredTicket {
		mu.Lock()
		defer mu.Unlock()
		return []*app.StoredTicket{{Ticket: parsed.Clone()}}
	}})
	stored, err := repo.List()
	require.NoError(t, err)
	require.NoError(t, stored[0].Ticket.SetField("deps", []string{"changed"}))
	require.Equal(t, []string{"original"}, parsed.Deps)

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			mu.Lock()
			err := parsed.SetField("summary", "concurrent update")
			mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 100 {
		_, err := repo.List()
		require.NoError(t, err)
	}
	wg.Wait()
}
