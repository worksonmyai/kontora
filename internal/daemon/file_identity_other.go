//go:build !darwin && !linux

package daemon

import (
	"os"
	"time"
)

// Without a change time an unchanged-looking file cannot be trusted, so every
// observation reads it again.
func statIdentity(os.FileInfo) (dev, ino uint64, ctime time.Time, ok bool) {
	return 0, 0, time.Time{}, false
}
