package companion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var (
	ErrStorageUnavailable = errors.New("companion storage is unavailable; read state and checkpoints cannot be saved")
	ErrLocked             = errors.New("companion store is locked")
	ErrCorrupt            = errors.New("corrupt companion snapshot")
	ErrUnsupportedVersion = errors.New("unsupported companion snapshot version")
)

type Store struct {
	mu      sync.Mutex
	dir     string
	lock    *os.File
	blocked error
	fault   func(string) error
}

func Scope(lockPath, ticketsDir string) string {
	normalize := func(path string) string {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return filepath.Clean(path)
		}
		return absolute
	}
	hash := sha256.Sum256([]byte(normalize(lockPath) + "\x00" + normalize(ticketsDir)))
	return hex.EncodeToString(hash[:])
}

func Open(dir string) (*Store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create companion directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "store.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open companion lock: %w", err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock companion store: %w", err)
	}
	if err = errors.Join(os.Chmod(dir, 0700), lock.Chmod(0600)); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("protect companion store: %w", err)
	}
	return &Store{dir: dir, lock: lock}, nil
}

func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, _, err := s.load()
	return state, err
}

func (s *Store) load() (State, []byte, error) {
	if s.lock == nil {
		return State{}, nil, os.ErrClosed
	}
	if s.blocked != nil {
		return State{}, nil, s.blocked
	}
	path := filepath.Join(s.dir, "state.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: snapshotVersion, Items: []Item{}}, nil, nil
	}
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%w: snapshot is not a regular file", ErrCorrupt)
	}
	var data []byte
	if err == nil {
		data, err = os.ReadFile(path)
	}
	var state State
	if err == nil {
		err = decodeSnapshot(data, &state)
	}
	if err != nil {
		s.blocked = fmt.Errorf("load companion snapshot %s: %w", path, err)
		return State{}, nil, s.blocked
	}
	return state, data, nil
}

func decodeSnapshot(data []byte, state *State) error {
	// Check the version before interpreting fields from a newer format.
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if header.Version != snapshotVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, header.Version)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(state); err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing snapshot data", ErrCorrupt)
	}
	return validateState(*state)
}

func validateState(state State) error {
	if state.Version != snapshotVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, state.Version)
	}
	if state.CaughtUp > state.Revision {
		return fmt.Errorf("%w: checkpoint exceeds snapshot revision", ErrCorrupt)
	}
	for identity, occurred := range state.SeenCompletions {
		if _, err := hex.DecodeString(identity); err != nil || len(identity) != sha256.Size*2 || occurred.IsZero() {
			return fmt.Errorf("%w: invalid completion identity", ErrCorrupt)
		}
	}
	ids := make(map[string]bool, len(state.Items))
	for _, item := range state.Items {
		if item.ID == "" || ids[item.ID] || item.Revision == 0 || item.Revision > state.Revision ||
			item.ChangeRevision == 0 || item.ChangeRevision > item.Revision || item.FirstSeen.IsZero() || item.ChangedAt.Before(item.FirstSeen) || (item.Active && item.InactiveReason != "") {
			return fmt.Errorf("%w: invalid item %q", ErrCorrupt, item.ID)
		}
		ids[item.ID] = true
	}
	return nil
}

func (s *Store) Save(state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, previous, err := s.load()
	if err != nil {
		return err
	}
	if err = validateState(state); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode companion snapshot: %w", err)
	}
	data = append(data, '\n')
	if bytes.Equal(data, previous) {
		return syncDirectory(s.dir)
	}
	if err = s.replace(data); err != nil {
		return fmt.Errorf("save companion snapshot: %w", err)
	}
	return nil
}

func (s *Store) fail(stage string) error {
	if s.fault != nil {
		return s.fault(stage)
	}
	return nil
}

func (s *Store) replace(data []byte) error {
	if err := s.fail("create"); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err = file.Chmod(0600); err == nil {
		err = s.fail("write")
	}
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = s.fail("sync")
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, s.fail("close"), file.Close())
	if err != nil {
		return err
	}
	if err = s.fail("rename"); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(s.dir, "state.json")); err != nil {
		return err
	}
	// A directory sync failure can leave the new snapshot visible but not durable.
	if err = s.fail("sync_dir"); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	// Unlinking the lock would let another process lock a different inode.
	err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	err = errors.Join(err, s.lock.Close())
	s.lock = nil
	return err
}
