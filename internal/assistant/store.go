// Package assistant holds the pieces the dashboard's assistant pane is built
// from: the chat threads and their on-disk store, the write classifier that
// gates an agent's tool calls, the registry of the calls waiting on a person,
// and the argument list one headless turn runs with.
//
// It takes primitives and config.Agent rather than the whole config, the way
// process, worktree and prompt do; the daemon wires it.
package assistant

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Thread is one chat, which is one agent session. Every field but the counters
// is fixed when the thread is created: the agent, the model and the working
// directory cannot move between turns without breaking session resume.
type Thread struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Agent     string    `json:"agent"`
	Kind      string    `json:"kind"`
	Model     string    `json:"model,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	Cwd       string    `json:"cwd"`
	Autonomy  string    `json:"autonomy"`
	// SessionID is the reference the agent resumes on: a UUID for claude, the
	// project session id for pi. It is minted with the first turn.
	SessionID string `json:"session_id,omitempty"`
	Turns     int    `json:"turns"`
	Writes    int    `json:"writes"`
}

// Turn is one user message and what running it did.
type Turn struct {
	N    int    `json:"n"`
	Text string `json:"text"`
	// Context is the page the user was on, as the brief was told it. Recorded
	// so a turn says what the agent knew; the pane does not render it.
	Context   string       `json:"context,omitempty"`
	StartedAt time.Time    `json:"started_at"`
	EndedAt   time.Time    `json:"ended_at,omitzero"`
	ExitCode  int          `json:"exit_code"`
	Error     string       `json:"error,omitempty"`
	Gates     []GateRecord `json:"gates,omitempty"`
}

// GateRecord is one tool call the gate saw and what it answered.
type GateRecord struct {
	Tool     string   `json:"tool"`
	Arg      string   `json:"arg,omitempty"`
	Kind     Decision `json:"kind"`
	Verdict  Verdict  `json:"verdict"`
	Approved bool     `json:"approved,omitempty"`
}

// ErrThreadNotFound is returned for an id no thread directory answers to.
var ErrThreadNotFound = errors.New("assistant thread not found")

// threadIDRe is what a directory name under the store root may look like. Ids
// are minted here, but they also arrive from a URL path, so they are checked
// before they reach filepath.Join.
var threadIDRe = regexp.MustCompile(`^[a-z0-9]{4,32}$`)

// ValidID reports whether an id can key a directory.
func ValidID(id string) bool { return threadIDRe.MatchString(id) }

// NewID mints a thread id.
func NewID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b[:])
}

// TitleMax is where a thread title taken from the first message is cut.
const TitleMax = 48

// Title derives a thread title from the first user message.
func Title(text string) string {
	line := strings.TrimSpace(text)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return "New chat"
	}
	if len(line) <= TitleMax {
		return line
	}
	return strings.TrimSpace(line[:TitleMax-1]) + "…"
}

// Store keeps threads under one root directory, one directory per thread:
//
//	<root>/<thread-id>/thread.json   the metadata above
//	<root>/<thread-id>/turns.jsonl   one record per user turn
//	<root>/<thread-id>/turn.<n>.log  that turn's captured stdout
//	<root>/<thread-id>/pi-sessions/  a pi thread's transcript
//
// A claude thread's transcript stays in claude's own config directory, which is
// where claude puts it and where SessionPath looks.
type Store struct{ root string }

// NewStore returns a store rooted at dir. The directory is created lazily.
func NewStore(dir string) *Store { return &Store{root: dir} }

// Root reports the directory the store writes under.
func (s *Store) Root() string { return s.root }

// Dir is the directory holding one thread.
func (s *Store) Dir(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrThreadNotFound
	}
	return filepath.Join(s.root, id), nil
}

// PiSessionDir is where a pi thread's session JSONL is written.
func (s *Store) PiSessionDir(id string) (string, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pi-sessions"), nil
}

// TurnLogPath is where one turn's captured stdout goes.
func (s *Store) TurnLogPath(id string, n int) (string, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("turn.%d.log", n)), nil
}

// Save writes the thread metadata, replacing what was there.
func (s *Store) Save(t Thread) error {
	dir, err := s.Dir(t.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "thread.json"), append(data, '\n'))
}

// Load reads one thread.
func (s *Store) Load(id string) (Thread, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return Thread{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "thread.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Thread{}, ErrThreadNotFound
	}
	if err != nil {
		return Thread{}, err
	}
	var t Thread
	if err := json.Unmarshal(data, &t); err != nil {
		return Thread{}, fmt.Errorf("thread %s: %w", id, err)
	}
	return t, nil
}

// List returns every readable thread, most recently updated first. A directory
// whose thread.json is missing or unparseable is skipped rather than failing
// the listing: one bad thread must not hide the history.
func (s *Store) List() ([]Thread, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return []Thread{}, nil
	}
	if err != nil {
		return nil, err
	}
	threads := make([]Thread, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := s.Load(e.Name())
		if err != nil {
			continue
		}
		threads = append(threads, t)
	}
	sort.Slice(threads, func(i, j int) bool {
		if threads[i].UpdatedAt.Equal(threads[j].UpdatedAt) {
			return threads[i].ID < threads[j].ID
		}
		return threads[i].UpdatedAt.After(threads[j].UpdatedAt)
	})
	return threads, nil
}

// Delete removes a thread and everything under it.
func (s *Store) Delete(id string) error {
	dir, err := s.Dir(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return ErrThreadNotFound
	}
	return os.RemoveAll(dir)
}

// AppendTurn adds one record to the thread's turn log.
func (s *Store) AppendTurn(id string, turn Turn) error {
	dir, err := s.Dir(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "turns.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// Turns reads the thread's turn log, oldest first. A line that does not parse
// is skipped: a crash mid-write must not make the whole history unreadable.
func (s *Store) Turns(id string) ([]Turn, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "turns.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return []Turn{}, nil
	}
	if err != nil {
		return nil, err
	}
	turns := []Turn{}
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var t Turn
		if json.Unmarshal([]byte(line), &t) != nil {
			continue
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// writeFileAtomic writes through a temporary file in the same directory, so a
// reader never sees a half-written thread.json.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
