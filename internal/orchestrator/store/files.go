package store

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
	"strings"
	"sync"
	"syscall"

	"github.com/worksonmyai/kontora/internal/orchestrator"
)

var ErrLocked = errors.New("orchestrator authority is locked")

type Authority struct {
	Version  int    `json:"version"`
	BoardID  string `json:"board_id"`
	OwnerID  string `json:"owner_id"`
	StateDir string `json:"state_dir"`
}

type authorityRecord struct {
	Authority Authority `json:"authority"`
	Checksum  string    `json:"checksum"`
}

type BoardLock struct {
	mu        sync.Mutex
	file      *os.File
	Authority Authority
}

func digest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func strictJSON(data []byte, value any) error {
	keys := json.NewDecoder(bytes.NewReader(data))
	keys.UseNumber()
	if err := uniqueKeys(keys); err != nil {
		return err
	}
	if _, err := keys.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

func uniqueKeys(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON key %v", key)
			}
			seen[name] = true
			if err = uniqueKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueKeys(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func makeDurableDir(path string) error {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("not a directory: %s", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err = makeDurableDir(parent); err != nil {
		return err
	}
	if err = os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err = syncDir(parent); err != nil {
		return err
	}
	return syncDir(path)
}

func canonicalDestination(path string) (string, error) {
	resolved, err := canonicalDir(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", fmt.Errorf("cannot resolve path %s", path)
	}
	resolved, err = canonicalDestination(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(abs)), nil
}

func canonicalDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func DefaultStateDir(boardID string) (string, error) {
	if boardID == "" || boardID == "." || boardID == ".." || strings.ContainsAny(boardID, "/\\\x00") {
		return "", fmt.Errorf("invalid board ID")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kontora", "orchestrator", boardID), nil
}

// Discover reads board authority independently of the caller's configured state directory.
func Discover(ticketsDir string) (Authority, error) {
	dir, err := canonicalDir(ticketsDir)
	if err != nil {
		return Authority{}, err
	}
	path := filepath.Join(dir, ".kontora", "authority.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Authority{}, err
		}
		return Authority{}, &RecoveryError{Path: path, Cause: err}
	}
	var r authorityRecord
	if err = strictJSON(data, &r); err != nil {
		return Authority{}, &RecoveryError{Path: path, Cause: err}
	}
	body, err := json.Marshal(r.Authority)
	if err != nil {
		return Authority{}, err
	}
	if digest(body) != r.Checksum {
		return Authority{}, &RecoveryError{Path: path, Cause: fmt.Errorf("authority registry checksum mismatch")}
	}
	a := r.Authority
	if a.Version != 1 || a.BoardID == "" || a.OwnerID == "" || !filepath.IsAbs(a.StateDir) || filepath.Clean(a.StateDir) != a.StateDir {
		return Authority{}, &RecoveryError{Path: path, Cause: fmt.Errorf("invalid authority registry")}
	}
	return a, nil
}

func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, err
	}
	if err = f.Sync(); err == nil {
		err = syncDir(filepath.Dir(path))
	}
	if err != nil {
		return nil, errors.Join(err, unlockFile(f))
	}
	return f, nil
}

func unlockFile(f *os.File) error {
	if f == nil {
		return nil
	}
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return errors.Join(err, f.Close())
}

// AcquireBoard never replaces an existing authority or removes the lock file.
func AcquireBoard(ticketsDir string, proposed Authority) (*BoardLock, error) {
	return acquireBoard(ticketsDir, proposed, nil)
}

func acquireBoard(ticketsDir string, proposed Authority, fault func(string) error) (*BoardLock, error) {
	dir, err := canonicalDir(ticketsDir)
	if err != nil {
		return nil, err
	}
	control := filepath.Join(dir, ".kontora")
	if err = makeDurableDir(control); err != nil {
		return nil, err
	}
	if err = syncDir(dir); err != nil {
		return nil, err
	}
	f, err := lockFile(filepath.Join(control, "board.lock"))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*BoardLock, error) { return nil, errors.Join(err, unlockFile(f)) }
	if proposed.Version == 0 {
		proposed.Version = 1
	}
	if proposed.Version != 1 || proposed.BoardID == "" || proposed.OwnerID == "" || proposed.StateDir == "" {
		return fail(fmt.Errorf("invalid proposed authority"))
	}
	if !filepath.IsAbs(proposed.StateDir) {
		return fail(fmt.Errorf("authority state directory must be absolute"))
	}
	proposed.StateDir, err = canonicalDestination(proposed.StateDir)
	if err != nil {
		return fail(err)
	}
	existing, err := Discover(dir)
	if err == nil {
		if existing != proposed {
			return fail(fmt.Errorf("%w: board authority belongs to %s at %s", orchestrator.ErrConflict, existing.OwnerID, existing.StateDir))
		}
		if err = readIdentity(existing.StateDir, existing.BoardID); err != nil {
			return fail(err)
		}
		return &BoardLock{file: f, Authority: existing}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	initialized, err := Open(proposed.StateDir, proposed.BoardID)
	if err != nil {
		return fail(err)
	}
	if err = initialized.Close(); err != nil {
		return fail(err)
	}
	body, err := json.Marshal(proposed)
	if err != nil {
		return fail(err)
	}
	data, err := json.Marshal(authorityRecord{Authority: proposed, Checksum: digest(body)})
	if err != nil {
		return fail(err)
	}
	if err = replaceFile(filepath.Join(control, "authority.json"), append(data, '\n'), fault, "authority"); err != nil {
		return fail(err)
	}
	return &BoardLock{file: f, Authority: proposed}, nil
}

func (l *BoardLock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return unlockFile(f)
}

// A failed rename or directory sync can leave the new file visible. Callers must reconcile before retrying effects.
func replaceFile(path string, data []byte, fault func(string) error, prefix string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".orchestrator-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if fault != nil {
		if err = fault(prefix + ".before_rename"); err != nil {
			return err
		}
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if fault != nil {
		if err = fault(prefix + ".after_rename"); err != nil {
			return err
		}
	}
	return syncDir(filepath.Dir(path))
}

func validArtifact(ref orchestrator.ArtifactRef) bool {
	if len(ref.Hash) != 64 || ref.Size < 0 {
		return false
	}
	decoded, err := hex.DecodeString(ref.Hash)
	return err == nil && hex.EncodeToString(decoded) == ref.Hash
}

func (s *Store) artifactPath(ref orchestrator.ArtifactRef) (string, error) {
	if !validArtifact(ref) {
		return "", fmt.Errorf("invalid artifact reference")
	}
	return filepath.Join(s.dir, "artifacts", ref.Hash), nil
}

func (s *Store) readArtifact(ref orchestrator.ArtifactRef) ([]byte, error) {
	path, err := s.artifactPath(ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("referenced artifact %s: %w", ref.Hash, err)
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Size {
		return nil, fmt.Errorf("artifact size or file type mismatch: %s", ref.Hash)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if digest(data) != ref.Hash {
		return nil, fmt.Errorf("artifact checksum mismatch: %s", ref.Hash)
	}
	return data, nil
}

func (s *Store) PutArtifact(data []byte) (orchestrator.ArtifactRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return orchestrator.ArtifactRef{}, err
	}
	ref := orchestrator.ArtifactRef{Hash: digest(data), Size: int64(len(data))}
	path, _ := s.artifactPath(ref)
	if _, err := os.Lstat(path); err == nil {
		_, err = s.readArtifact(ref)
		if err == nil {
			err = syncFile(path)
		}
		if err == nil {
			err = syncDir(filepath.Dir(path))
		}
		return ref, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return orchestrator.ArtifactRef{}, err
	}
	if err := replaceFile(path, data, s.fault, "artifact"); err != nil {
		return orchestrator.ArtifactRef{}, err
	}
	return ref, nil
}

func (s *Store) ReadArtifact(ref orchestrator.ArtifactRef) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	return s.readArtifact(ref)
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (s *Store) syncArtifacts(state orchestrator.State) error {
	refs := orchestrator.ReferencedArtifacts(state)
	for _, ref := range refs {
		path, err := s.artifactPath(ref)
		if err != nil {
			return err
		}
		if err = syncFile(path); err != nil {
			return err
		}
	}
	if len(refs) != 0 {
		return syncDir(filepath.Join(s.dir, "artifacts"))
	}
	return nil
}

func (s *Store) checkArtifacts(state orchestrator.State) error {
	for _, ref := range orchestrator.ReferencedArtifacts(state) {
		if _, err := s.readArtifact(ref); err != nil {
			return err
		}
	}
	return nil
}
