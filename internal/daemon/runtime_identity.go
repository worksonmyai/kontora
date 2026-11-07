package daemon

import (
	"context"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/tmux"
)

// freezeStoreIdentity keeps the scanned store's identity even if its symlink changes.
// A failed startup lookup stays empty; a later directory cannot identify the loaded store.
func (d *Daemon) freezeStoreIdentity(dir string) {
	d.storeIdentityOnce.Do(func() {
		identity, _ := config.DirectoryIdentity(dir)
		d.storeIdentity.Store(&identity)
	})
}

// discoverTmuxSocket runs outside request handlers and stops with the daemon.
// Repeated discovery includes sessions created after startup and clears sockets for sessions that disappear.
func (d *Daemon) discoverTmuxSocket(ctx context.Context, interval, timeout time.Duration) {
	for ctx.Err() == nil {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		socket, _ := tmux.SocketPath(probeCtx, d.tmuxSession)
		cancel()
		d.tmuxSocket.Store(&socket)

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
