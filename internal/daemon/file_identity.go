package daemon

import (
	"os"
	"time"
)

// fileIdentity is what a stat call says about a file: enough to tell that its
// bytes have not changed since an earlier read without reading them again.
type fileIdentity struct {
	dev, ino uint64
	mode     os.FileMode
	size     int64
	mtime    time.Time
	ctime    time.Time
}

// identityOf returns the identity of a stat result, or false when the
// platform does not report a change time.
func identityOf(info os.FileInfo) (fileIdentity, bool) {
	dev, ino, ctime, ok := statIdentity(info)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{dev: dev, ino: ino, mode: info.Mode(), size: info.Size(), mtime: info.ModTime(), ctime: ctime}, true
}

// settledBefore reports whether no write can land later without changing
// the identity. The kernel sets ctime on every write and user space cannot set
// it, so a file last changed well before a read shows any later write as a new
// ctime, even on a filesystem with coarse timestamps. A file changed just
// before the read could take a second write within the same timestamp.
func (id fileIdentity) settledBefore(readStart time.Time) bool {
	return id.ctime.Before(readStart.Add(-secondaryRacyWindow))
}

// secondaryRacyWindow covers the coarsest timestamp resolution in common use
// (FAT and some network filesystems round to two seconds).
const secondaryRacyWindow = 2 * time.Second
