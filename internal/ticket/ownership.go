package ticket

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var ErrManaged = errors.New("managed ticket requires its owning daemon")

// The lock file stays in place so every process locks the same inode.
func WithBoardMutation(dir string, fn func() error) error {
	if err := os.MkdirAll(filepath.Join(dir, ".kontora"), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".kontora", "mutation.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

func CheckBoardControl(dir string) error {
	entries, err := os.ReadDir(filepath.Join(dir, ".kontora"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "mutation.lock" && entry.Name() != "board.lock" {
			return fmt.Errorf("%w: board control state exists", ErrManaged)
		}
	}
	return nil
}

func CheckOrdinaryWrite(dir string) error {
	if err := CheckBoardControl(dir); err != nil {
		return err
	}
	paths, err := ListFiles(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, path := range paths {
		t, err := ParseFile(path)
		if err == nil && t.HasOwnershipMetadata() {
			return fmt.Errorf("%w: %s", ErrManaged, t.ID)
		}
	}
	return nil
}

func WithOfflineWrite(dir string, fn func() error) error {
	return WithBoardMutation(dir, func() error { return WithOrdinaryRouting(dir, fn) })
}

// WithOrdinaryRouting prevents authority acquisition during a local API mutation.
// The daemon may take the mutation lock while this caller waits for its HTTP response.
func WithOrdinaryRouting(dir string, fn func() error) error {
	release, err := LockOrdinaryRouting(dir)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// LockOrdinaryRouting excludes authority acquisition through the disk write and cache update.
// It must not wait for authority while a caller holds a daemon admission mutex.
func LockOrdinaryRouting(dir string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Join(dir, ".kontora"), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".kontora", "board.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: board authority is active", ErrManaged)
		}
		return nil, err
	}
	release = func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	if err := CheckOrdinaryWrite(dir); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// HasOwnershipMetadata also recognizes an empty or null imported marker.
func (t *Ticket) HasOwnershipMetadata() bool {
	if t.Managed != nil {
		return true
	}
	if t.rawNode != nil {
		for i := 0; i+1 < len(t.rawNode.Content); i += 2 {
			if t.rawNode.Content[i].Value == "managed" {
				return true
			}
		}
	}
	return false
}

func RejectImportedOwnership(t *Ticket) error {
	if t.HasOwnershipMetadata() {
		return fmt.Errorf("%w: imported ownership metadata", ErrManaged)
	}
	return nil
}

func WriteOffline(path string, data []byte) error {
	return WithOfflineWrite(filepath.Dir(path), func() error {
		t, err := ParseBytes(data)
		if err != nil {
			return err
		}
		if err := RejectImportedOwnership(t); err != nil {
			return err
		}
		if !IsSafeID(t.ID) || !IsCanonicalPath(path, t.ID) {
			return fmt.Errorf("ticket id does not match destination")
		}
		return os.WriteFile(path, data, 0o644)
	})
}
