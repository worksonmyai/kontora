package daemon

import (
	"errors"
	"os"

	"github.com/worksonmyai/kontora/internal/watcher"
)

// Kqueue can miss a file created and deleted before its directory scan.
func (d *Daemon) reconcileRegisteredRemovals() {
	d.mu.Lock()
	paths := make([]string, 0, len(d.tickets))
	for _, ts := range d.tickets {
		paths = append(paths, ts.filePath)
	}
	d.mu.Unlock()
	for _, path := range paths {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			d.handleEvent(watcher.Event{Path: path, Op: watcher.OpRemoved})
		}
	}
}
