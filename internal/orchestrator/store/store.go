package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/worksonmyai/kontora/internal/orchestrator"
)

var (
	ErrIncompleteTail = errors.New("incomplete journal tail; preserve and reconcile before reopening")
	ErrRecovery       = errors.New("orchestrator recovery required")
	ErrClosed         = errors.New("orchestrator store is closed")
)

type RecoveryError struct {
	Path   string
	Offset int64
	Cause  error
}

func (e *RecoveryError) Error() string {
	return fmt.Sprintf("%s at byte %d: %v", e.Path, e.Offset, e.Cause)
}
func (e *RecoveryError) Unwrap() error        { return e.Cause }
func (e *RecoveryError) Is(target error) bool { return target == ErrRecovery }

type identityBody struct {
	Version int    `json:"version"`
	BoardID string `json:"board_id"`
}

type identityRecord struct {
	Body     identityBody `json:"body"`
	Checksum string       `json:"checksum"`
}

func readIdentity(dir, boardID string) error {
	path := filepath.Join(dir, "store.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return &RecoveryError{Path: path, Cause: err}
	}
	var r identityRecord
	if err = strictJSON(data, &r); err != nil {
		return &RecoveryError{Path: path, Cause: err}
	}
	body, _ := json.Marshal(r.Body)
	if r.Body.Version != 1 || r.Body.BoardID != boardID || r.Checksum != digest(body) {
		return &RecoveryError{Path: path, Cause: fmt.Errorf("store identity or checksum mismatch")}
	}
	info, err := os.Lstat(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		return &RecoveryError{Path: dir, Cause: err}
	}
	if !info.Mode().IsRegular() {
		return &RecoveryError{Path: dir, Cause: fmt.Errorf("journal is not a regular file")}
	}
	return nil
}

func prepareIdentity(dir, boardID string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, "store.json")); err == nil {
		return false, readIdentity(dir, boardID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(filepath.Join(dir, "snapshot.json")); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, &RecoveryError{Path: dir, Cause: fmt.Errorf("snapshot exists without store identity: %v", err)}
	}
	if info, err := os.Lstat(filepath.Join(dir, "journal.jsonl")); err == nil {
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return false, &RecoveryError{Path: dir, Cause: fmt.Errorf("journal exists without store identity")}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func writeIdentity(dir, boardID string) error {
	body := identityBody{Version: 1, BoardID: boardID}
	data, _ := json.Marshal(body)
	data, err := json.Marshal(identityRecord{Body: body, Checksum: digest(data)})
	if err != nil {
		return err
	}
	return replaceFile(filepath.Join(dir, "store.json"), append(data, '\n'), nil, "identity")
}

type recordBody struct {
	Version  int                `json:"version"`
	Sequence uint64             `json:"sequence"`
	Previous string             `json:"previous"`
	State    orchestrator.State `json:"state"`
}

type journalRecord struct {
	Body     recordBody `json:"body"`
	Checksum string     `json:"checksum"`
}

type snapshotBody struct {
	Version         int                `json:"version"`
	Sequence        uint64             `json:"sequence"`
	JournalChecksum string             `json:"journal_checksum"`
	State           orchestrator.State `json:"state"`
}

type snapshotRecord struct {
	Body     snapshotBody `json:"body"`
	Checksum string       `json:"checksum"`
}

type Store struct {
	mu       sync.Mutex
	dir      string
	state    orchestrator.State
	checksum string
	journal  *os.File
	lock     *os.File
	board    *BoardLock
	closed   bool
	poison   error
	fault    func(string) error
}

// OpenBoard establishes discoverable authority before callers can persist ownership.
func OpenBoard(ticketsDir string, authority Authority) (*Store, error) {
	board, err := AcquireBoard(ticketsDir, authority)
	if err != nil {
		return nil, err
	}
	s, err := Open(board.Authority.StateDir, board.Authority.BoardID)
	if err != nil {
		return nil, errors.Join(err, board.Close())
	}
	s.board = board
	return s, nil
}

// Open is the low-level state store. Board users must hold AcquireBoard or use OpenBoard.
func Open(stateDir, boardID string) (*Store, error) {
	if boardID == "" || stateDir == "" {
		return nil, fmt.Errorf("state directory and board ID are required")
	}
	if err := makeDurableDir(stateDir); err != nil {
		return nil, err
	}
	dir, err := canonicalDir(stateDir)
	if err != nil {
		return nil, err
	}
	if err = syncDir(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	lock, err := lockFile(filepath.Join(dir, "store.lock"))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, state: orchestrator.NewState(boardID), lock: lock}
	fail := func(err error) (*Store, error) { return nil, errors.Join(err, s.Close()) }
	if err = makeDurableDir(filepath.Join(dir, "artifacts")); err != nil {
		return fail(err)
	}
	if err = syncDir(dir); err != nil {
		return fail(err)
	}
	initialize, err := prepareIdentity(dir, boardID)
	if err != nil {
		return fail(err)
	}
	journalPath := filepath.Join(dir, "journal.jsonl")
	s.journal, err = os.OpenFile(journalPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return fail(err)
	}
	if err = s.journal.Sync(); err != nil {
		return fail(err)
	}
	if err = syncDir(dir); err != nil {
		return fail(err)
	}
	if initialize {
		if err = writeIdentity(dir, boardID); err != nil {
			return fail(err)
		}
	}
	if err = s.recover(); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *Store) recover() error {
	path := filepath.Join(s.dir, "snapshot.json")
	snap, err := s.readSnapshot(path)
	if err != nil {
		return err
	}
	if _, err := s.journal.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReader(s.journal)
	var offset int64
	matchedSnapshot := snap == nil
	if snap != nil && snap.Body.Sequence == 0 {
		data, _ := json.Marshal(s.state)
		snapshotData, _ := json.Marshal(snap.Body.State)
		matchedSnapshot = snap.Body.JournalChecksum == "" && bytes.Equal(data, snapshotData)
		if !matchedSnapshot {
			return &RecoveryError{Path: path, Cause: fmt.Errorf("snapshot does not match empty journal")}
		}
	}
	journalPath := filepath.Join(s.dir, "journal.jsonl")
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrIncompleteTail
			}
			return &RecoveryError{Path: journalPath, Offset: offset, Cause: err}
		}
		if err = s.replayRecord(line, journalPath, offset); err != nil {
			return err
		}
		if snap != nil && snap.Body.Sequence == s.state.Revision {
			stateData, _ := json.Marshal(s.state)
			snapshotData, _ := json.Marshal(snap.Body.State)
			if snap.Body.JournalChecksum != s.checksum || !bytes.Equal(stateData, snapshotData) {
				return &RecoveryError{Path: path, Cause: fmt.Errorf("snapshot disagrees with journal")}
			}
			matchedSnapshot = true
		}
		offset += int64(len(line))
	}
	if !matchedSnapshot {
		return &RecoveryError{Path: path, Cause: fmt.Errorf("snapshot references missing journal state")}
	}
	if err := s.checkArtifacts(s.state); err != nil {
		return &RecoveryError{Path: s.dir, Cause: err}
	}
	return nil
}

func (s *Store) readSnapshot(path string) (*snapshotRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &RecoveryError{Path: path, Cause: err}
	}
	var r snapshotRecord
	if err = strictJSON(data, &r); err != nil {
		return nil, &RecoveryError{Path: path, Cause: err}
	}
	body, err := json.Marshal(r.Body)
	if err != nil {
		return nil, err
	}
	if r.Body.Version != 1 || r.Body.Sequence != r.Body.State.Revision || r.Body.State.BoardID != s.state.BoardID || digest(body) != r.Checksum {
		return nil, &RecoveryError{Path: path, Cause: fmt.Errorf("invalid snapshot or checksum")}
	}
	if err = orchestrator.Validate(r.Body.State); err != nil {
		return nil, &RecoveryError{Path: path, Cause: err}
	}
	return &r, nil
}

func (s *Store) replayRecord(line []byte, path string, offset int64) error {
	var r journalRecord
	if err := strictJSON(line, &r); err != nil {
		return &RecoveryError{Path: path, Offset: offset, Cause: err}
	}
	body, err := json.Marshal(r.Body)
	if err != nil {
		return err
	}
	if r.Body.Version != 1 || r.Body.Sequence != s.state.Revision+1 || r.Body.Sequence != r.Body.State.Revision || r.Body.Previous != s.checksum || digest(body) != r.Checksum {
		return &RecoveryError{Path: path, Offset: offset, Cause: fmt.Errorf("journal sequence or checksum mismatch")}
	}
	if err = orchestrator.ValidateTransition(s.state, r.Body.State); err != nil {
		return &RecoveryError{Path: path, Offset: offset, Cause: err}
	}
	s.state, s.checksum = r.Body.State, r.Checksum
	return nil
}

func (s *Store) available() error {
	if s.closed {
		return ErrClosed
	}
	if s.poison != nil {
		return fmt.Errorf("%w: %v", ErrRecovery, s.poison)
	}
	return nil
}

func (s *Store) State() (orchestrator.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return orchestrator.State{}, err
	}
	return orchestrator.Clone(s.state)
}

func (s *Store) Apply(c orchestrator.Command) error {
	return s.Transact(c.ExpectedRevision, func(state orchestrator.State) (orchestrator.State, error) { return orchestrator.Apply(state, c) })
}

func (s *Store) ApplyBatch(expectedRevision uint64, commands []orchestrator.Command) error {
	return s.Transact(expectedRevision, func(state orchestrator.State) (orchestrator.State, error) {
		return orchestrator.ApplyBatch(state, expectedRevision, commands)
	})
}

// Transact serializes validation, append, synchronization, and publication. The callback must not call this store.
func (s *Store) Transact(expectedRevision uint64, change func(orchestrator.State) (orchestrator.State, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transact(expectedRevision, change)
}

func (s *Store) transact(expectedRevision uint64, change func(orchestrator.State) (orchestrator.State, error)) error {
	if err := s.available(); err != nil {
		return err
	}
	if s.state.Revision != expectedRevision {
		return orchestrator.ErrStale
	}
	if change == nil {
		return fmt.Errorf("transaction callback is required")
	}
	before, err := orchestrator.Clone(s.state)
	if err != nil {
		return err
	}
	next, err := change(before)
	if err != nil {
		return err
	}
	// Detach the committed state from memory retained by the callback.
	next, err = orchestrator.Clone(next)
	if err != nil {
		return err
	}
	if err = orchestrator.ValidateTransition(s.state, next); err != nil {
		return err
	}
	if err = s.checkArtifacts(next); err != nil {
		return err
	}
	if err = s.syncArtifacts(next); err != nil {
		return err
	}
	body := recordBody{Version: 1, Sequence: next.Revision, Previous: s.checksum, State: next}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	checksum := digest(data)
	line, err := json.Marshal(journalRecord{Body: body, Checksum: checksum})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if s.fault != nil {
		if err = s.fault("journal.before_write"); err != nil {
			return err
		}
	}
	n, err := s.journal.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err == nil && s.fault != nil {
		err = s.fault("journal.after_write")
	}
	if err == nil {
		err = s.journal.Sync()
	}
	if err == nil && s.fault != nil {
		err = s.fault("journal.after_sync")
	}
	if err != nil {
		s.poison = err
		return fmt.Errorf("%w: journal commit outcome is uncertain: %v", ErrRecovery, err)
	}
	s.state, s.checksum = next, checksum
	return nil
}

// Snapshots are replaceable checkpoints. The retained journal remains the recovery authority.
func (s *Store) Snapshot() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return err
	}
	if err := s.checkArtifacts(s.state); err != nil {
		return err
	}
	body := snapshotBody{Version: 1, Sequence: s.state.Revision, JournalChecksum: s.checksum, State: s.state}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	data, err = json.Marshal(snapshotRecord{Body: body, Checksum: digest(data)})
	if err != nil {
		return err
	}
	if err = replaceFile(filepath.Join(s.dir, "snapshot.json"), append(data, '\n'), s.fault, "snapshot"); err != nil {
		s.poison = err
		return fmt.Errorf("%w: snapshot replacement: %v", ErrRecovery, err)
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.journal != nil {
		err = s.journal.Close()
	}
	err = errors.Join(err, unlockFile(s.lock))
	if s.board != nil {
		err = errors.Join(err, s.board.Close())
	}
	return err
}
