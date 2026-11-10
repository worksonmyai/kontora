package daemon

import (
	"os"
	"syscall"
	"time"
)

func statIdentity(info os.FileInfo) (dev, ino uint64, ctime time.Time, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, time.Time{}, false
	}
	return st.Dev, st.Ino, time.Unix(st.Ctim.Unix()), true
}
