package automation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const snapshotVersion = 2

type storedDefinition struct {
	Spec      Spec      `json:"spec"`
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Revision  uint64    `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	Next      time.Time `json:"next"`
	Deleted   bool      `json:"deleted"`
}

type receipt struct {
	AutomationID string    `json:"automation_id"`
	N            uint64    `json:"n"`
	Trigger      string    `json:"trigger"`
	ScheduledFor time.Time `json:"scheduled_for"`
}

type snapshot struct {
	Version     int                         `json:"version"`
	Scope       string                      `json:"scope"`
	Definitions map[string]storedDefinition `json:"definitions"`
	Runs        map[string]Run              `json:"runs"`
	Counters    map[string]uint64           `json:"counters"`
	Receipts    map[string]receipt          `json:"receipts"`
}

type Store struct {
	mu      sync.Mutex
	dir     string
	lock    *os.File
	blocked error
	state   snapshot
	data    []byte
	fault   func(string) error
}

func canonical(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		resolved, err = canonical(parent)
		if err == nil {
			resolved = filepath.Join(resolved, filepath.Base(path))
		}
	}
	return resolved, err
}

func Open(root, scope string) (*Store, error) {
	if scope == "" || !textValid(scope, 4096, false) {
		return nil, validation("scope is required")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		root = filepath.Join(home, ".kontora", "automations")
	}
	root, err := canonical(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	hash := sha256.Sum256([]byte(scope))
	dir := filepath.Join(root, hex.EncodeToString(hash[:]))
	if err := privateDirectory(root); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		return nil, fmt.Errorf("%w: scope is not a directory", ErrUnavailable)
	}
	if err := privateDirectory(dir); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "store.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: store locked: %v", ErrUnavailable, err)
	}
	s := &Store{dir: dir, lock: lock}
	fail := func(err error) (*Store, error) { _ = s.Close(); return nil, fmt.Errorf("%w: %v", ErrUnavailable, err) }
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return fail(errors.New("lock is not a regular file"))
	}
	if err := lock.Chmod(0600); err != nil {
		return fail(err)
	}
	data, err := readSnapshot(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		s.state = snapshot{Version: snapshotVersion, Scope: scope, Definitions: map[string]storedDefinition{}, Runs: map[string]Run{}, Counters: map[string]uint64{}, Receipts: map[string]receipt{}}
		if err := s.save(s.state); err != nil {
			return fail(err)
		}
	} else {
		if err != nil {
			return fail(err)
		}
		if err := decodeSnapshot(data, &s.state); err != nil {
			return fail(err)
		}
		if s.state.Scope != scope {
			return fail(errors.New("snapshot scope mismatch"))
		}
		if err := os.Chmod(filepath.Join(dir, "state.json"), 0600); err != nil {
			return fail(err)
		}
		s.data = data
	}
	return s, nil
}

func makeDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return errors.New("not a directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if err := makeDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncDirectory(parent)
}
func privateDirectory(path string) error {
	if err := makeDirectory(path); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}
func readSnapshot(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("snapshot is not a regular file")
	}
	return io.ReadAll(f)
}
func uniqueJSON(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON key")
			}
			seen[name] = true
			if err := uniqueJSON(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := uniqueJSON(dec); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = dec.Token()
	return err
}
func decodeSnapshot(data []byte, state *snapshot) error {
	if !utf8.Valid(data) {
		return errors.New("invalid snapshot UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueJSON(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing snapshot data")
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(state); err != nil {
		return err
	}
	if err := strictShape(data, reflect.TypeFor[snapshot]()); err != nil {
		return err
	}
	return validateSnapshot(*state)
}

func strictShape(data []byte, typ reflect.Type) error {
	if typ.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return nil
		}
		return strictShape(data, typ.Elem())
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null snapshot field")
	}
	if typ == reflect.TypeFor[time.Time]() {
		return nil
	}
	if typ.Kind() == reflect.Struct {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		if len(fields) != typ.NumField() {
			return errors.New("missing or unknown snapshot field")
		}
		for field := range typ.Fields() {
			value, ok := fields[field.Tag.Get("json")]
			if !ok {
				return errors.New("missing snapshot field")
			}
			if err := strictShape(value, field.Type); err != nil {
				return err
			}
		}
	} else if typ.Kind() == reflect.Map {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := strictShape(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func runKey(id string, n uint64) string { return fmt.Sprintf("%s/%d", id, n) }
func validateRun(r Run) error {
	if !validID(r.AutomationID) || ValidateKey(r.Key) != nil || r.N == 0 || r.Revision == 0 || r.InvocationID == "" || !validInstant(r.AcceptedAt) || r.Spec.Validate() != nil {
		return validation("invalid run identity or frozen spec")
	}
	if r.Trigger != "manual" && r.Trigger != "schedule" {
		return validation("invalid run trigger")
	}
	if r.Trigger == "schedule" && !validInstant(r.ScheduledFor) {
		return validation("scheduled run needs scheduled_for")
	}
	switch r.Phase {
	case "queued", "preparing", "running", "stopping", "recovery_required":
		if r.Outcome != "" || !r.CompletedAt.IsZero() {
			return validation("nonterminal run has a terminal result")
		}
	case "terminal":
		if r.Outcome != "complete" && r.Outcome != "failed" && r.Outcome != "skipped" {
			return validation("invalid run outcome")
		}
		if !validInstant(r.CompletedAt) {
			return validation("terminal run needs completed_at")
		}
	default:
		return validation("invalid run phase")
	}
	if !r.StartedAt.IsZero() && (!validInstant(r.StartedAt) || r.StartedAt.Before(r.AcceptedAt)) {
		return validation("invalid started_at")
	}
	if !r.CompletedAt.IsZero() && (r.CompletedAt.Before(r.AcceptedAt) || (!r.StartedAt.IsZero() && r.CompletedAt.Before(r.StartedAt))) {
		return validation("invalid completed_at")
	}
	if r.Cost != nil {
		if _, ok := parseCost(*r.Cost); !ok {
			return validation("cost must be a nonnegative decimal")
		}
	}
	return nil
}
func validStoredDefinition(d storedDefinition) bool {
	return validID(d.ID) && ValidateKey(d.Key) == nil && d.Revision > 0 && validInstant(d.CreatedAt) && d.Spec.Validate() == nil && d.Spec == d.Spec.Normalize() && validInstant(d.Next)
}

func validateSnapshot(st snapshot) error {
	if st.Version != snapshotVersion || st.Scope == "" || st.Definitions == nil || st.Runs == nil || st.Counters == nil || st.Receipts == nil {
		return errors.New("invalid snapshot header")
	}
	active := map[string]bool{}
	for id, d := range st.Definitions {
		if id != d.ID || !validStoredDefinition(d) {
			return errors.New("invalid definition")
		}
		if !d.Deleted {
			if active[d.Key] {
				return errors.New("duplicate active key")
			}
			active[d.Key] = true
		}
		if _, ok := st.Counters[d.Key]; !ok {
			return errors.New("missing key counter")
		}
	}
	numbers := map[string]bool{}
	for key, r := range st.Runs {
		d, ok := st.Definitions[r.AutomationID]
		number := runKey(r.Key, r.N)
		if !ok || key != runKey(r.AutomationID, r.N) || r.Key != d.Key || r.N > st.Counters[r.Key] || validateRun(r) != nil || numbers[number] {
			return errors.New("invalid run")
		}
		numbers[number] = true
	}
	for key := range st.Counters {
		if ValidateKey(key) != nil {
			return errors.New("invalid key counter")
		}
	}
	return validateReceipts(st)
}

func validateReceipts(st snapshot) error {
	seen := make(map[string]bool, len(st.Runs))
	for key, receipt := range st.Receipts {
		runID := runKey(receipt.AutomationID, receipt.N)
		r, ok := st.Runs[runID]
		if !ok || r.Trigger != receipt.Trigger || !r.ScheduledFor.Equal(receipt.ScheduledFor) || seen[runID] {
			return errors.New("invalid allocation receipt")
		}
		prefix := receipt.AutomationID + "/request/"
		if requestID, ok := strings.CutPrefix(key, prefix); ok {
			if requestID == "" || !textValid(requestID, 200, false) {
				return errors.New("invalid request receipt")
			}
		} else if receipt.Trigger != "schedule" || key != receipt.AutomationID+"/schedule/"+receipt.ScheduledFor.UTC().Format(time.RFC3339Nano) {
			return errors.New("invalid scheduled receipt")
		}
		seen[runID] = true
	}
	if len(seen) != len(st.Runs) {
		return errors.New("missing allocation receipt")
	}
	return nil
}

func (s *Store) available() error {
	if s.lock == nil {
		return fmt.Errorf("%w: store closed", ErrUnavailable)
	}
	if s.blocked != nil {
		return s.blocked
	}
	data, err := readSnapshot(filepath.Join(s.dir, "state.json"))
	if err != nil || !bytes.Equal(data, s.data) {
		s.blocked = fmt.Errorf("%w: snapshot changed or unreadable: %v", ErrUnavailable, err)
		return s.blocked
	}
	return nil
}
func (s *Store) copyState() snapshot {
	st := s.state
	st.Definitions = maps.Clone(s.state.Definitions)
	st.Runs = make(map[string]Run, len(s.state.Runs))
	for k, v := range s.state.Runs {
		st.Runs[k] = cloneRun(v)
	}
	st.Counters = maps.Clone(s.state.Counters)
	st.Receipts = maps.Clone(s.state.Receipts)
	return st
}

func (s *Store) Reconcile(specs map[string]Spec, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return err
	}
	if !validInstant(now) {
		return validation("now is out of range")
	}
	normalized := make(map[string]Spec, len(specs))
	for key, spec := range specs {
		if err := ValidateKey(key); err != nil {
			return err
		}
		spec = spec.Normalize()
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		normalized[key] = spec
	}
	st := s.copyState()
	live := map[string]string{}
	changed := false
	for id, d := range st.Definitions {
		if d.Deleted {
			continue
		}
		live[d.Key] = id
		if _, ok := normalized[d.Key]; !ok {
			if d.Revision == math.MaxUint64 {
				return ErrConflict
			}
			d.Deleted = true
			d.Revision++
			st.Definitions[id] = d
			changed = true
		}
	}
	keys := make([]string, 0, len(normalized))
	for key := range normalized {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		spec := normalized[key]
		id, exists := live[key]
		if !exists {
			id = "aut-" + newID()
			schedule, _ := Compile(spec.Schedule, spec.Timezone)
			st.Definitions[id] = storedDefinition{Spec: spec, ID: id, Key: key, Revision: 1, CreatedAt: now.UTC(), Next: schedule.Next(now).UTC()}
			if _, ok := st.Counters[key]; !ok {
				st.Counters[key] = 0
			}
			changed = true
			continue
		}
		d := st.Definitions[id]
		if d.Spec == spec {
			continue
		}
		if d.Revision == math.MaxUint64 {
			return ErrConflict
		}
		if d.Spec.Schedule != spec.Schedule || d.Spec.Timezone != spec.Timezone || (!d.Spec.Enabled && spec.Enabled) {
			schedule, _ := Compile(spec.Schedule, spec.Timezone)
			d.Next = schedule.Next(now).UTC()
		}
		d.Spec = spec
		d.Revision++
		st.Definitions[id] = d
		changed = true
	}
	if !changed {
		return nil
	}
	return s.save(st)
}

func (s *Store) List() ([]Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(s.state.Definitions))
	now := time.Now()
	for _, d := range s.state.Definitions {
		if !d.Deleted {
			out = append(out, s.definition(d, now))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (s *Store) Get(id string) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Definition{}, err
	}
	d, ok := s.state.Definitions[id]
	if !ok || d.Deleted {
		return Definition{}, ErrNotFound
	}
	return s.definition(d, time.Now()), nil
}
func (s *Store) runs(id string) []Run {
	out := []Run{}
	for _, r := range s.state.Runs {
		if r.AutomationID == id {
			out = append(out, cloneRun(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N })
	return out
}
func (s *Store) Runs(id string) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	if _, ok := s.state.Definitions[id]; !ok {
		return nil, ErrNotFound
	}
	return s.runs(id), nil
}

// UnfinishedRuns includes tombstoned automations so deletion cannot hide a process that still needs recovery.
func (s *Store) UnfinishedRuns() ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	out := []Run{}
	for _, run := range s.state.Runs {
		if run.Phase != "terminal" {
			out = append(out, cloneRun(run))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].AcceptedAt.Equal(out[j].AcceptedAt) {
			return out[i].AcceptedAt.After(out[j].AcceptedAt)
		}
		if out[i].AutomationID != out[j].AutomationID {
			return out[i].AutomationID < out[j].AutomationID
		}
		return out[i].N > out[j].N
	})
	return out, nil
}

func (s *Store) GetRun(id string, n uint64) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Run{}, err
	}
	r, ok := s.state.Runs[runKey(id, n)]
	if !ok {
		return Run{}, ErrNotFound
	}
	return cloneRun(r), nil
}

func (s *Store) Allocate(id, trigger, requestID string, scheduledFor, next, now time.Time, admissions ...Admission) (Run, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Run{}, false, err
	}
	if trigger != "manual" && trigger != "schedule" {
		return Run{}, false, validation("invalid trigger")
	}
	if !textValid(requestID, 200, false) || (trigger == "manual" && requestID == "") {
		return Run{}, false, validation("request ID is required (max 200 bytes)")
	}
	if trigger == "schedule" && !validInstant(scheduledFor) {
		return Run{}, false, validation("scheduled_for is required")
	}
	receiptKey := id + "/request/" + requestID
	if requestID == "" {
		receiptKey = id + "/schedule/" + scheduledFor.UTC().Format(time.RFC3339Nano)
	}
	if receipt, ok := s.state.Receipts[receiptKey]; ok {
		if receipt.Trigger != trigger || !receipt.ScheduledFor.Equal(scheduledFor) {
			return Run{}, false, ErrConflict
		}
		return cloneRun(s.state.Runs[runKey(id, receipt.N)]), true, nil
	}
	d, ok := s.state.Definitions[id]
	if !ok || d.Deleted {
		return Run{}, false, ErrNotFound
	}
	if !validInstant(now) {
		return Run{}, false, validation("now is out of range")
	}
	if !next.IsZero() && (!validInstant(next) || next.Before(d.Next) || trigger != "schedule") {
		return Run{}, false, validation("invalid next cursor")
	}
	if s.state.Counters[d.Key] == math.MaxUint64 {
		return Run{}, false, ErrConflict
	}
	if len(admissions) > 1 {
		return Run{}, false, validation("one admission decision is required")
	}
	decision := Admission{}
	if len(admissions) == 1 {
		decision = admissions[0]
	}
	r := Run{AutomationID: id, Key: d.Key, N: s.state.Counters[d.Key] + 1, Revision: 1, InvocationID: "inv-" + newID(), Phase: "queued", Trigger: trigger, ScheduledFor: scheduledFor.UTC(), AcceptedAt: now.UTC(), Spec: d.Spec}
	if decision.SkipReason != "" {
		r.Phase, r.Outcome, r.Result, r.CompletedAt = "terminal", "skipped", decision.SkipReason, now.UTC()
	}
	st := s.copyState()
	if err := applyAdmission(st, id, decision, now); err != nil {
		return Run{}, false, err
	}
	st.Counters[d.Key] = r.N
	st.Runs[runKey(id, r.N)] = r
	st.Receipts[receiptKey] = receipt{AutomationID: id, N: r.N, Trigger: trigger, ScheduledFor: r.ScheduledFor}
	if !next.IsZero() {
		d.Next = next.UTC()
		st.Definitions[id] = d
	}
	if err := s.save(st); err != nil {
		return Run{}, false, err
	}
	return cloneRun(r), false, nil
}

func applyAdmission(st snapshot, id string, decision Admission, now time.Time) error {
	for _, number := range decision.Supersede {
		key := runKey(id, number)
		previous, exists := st.Runs[key]
		if !exists || previous.Phase != "queued" || previous.Revision == math.MaxUint64 {
			return ErrConflict
		}
		previous.Phase, previous.Outcome, previous.Result, previous.CompletedAt = "terminal", "skipped", "superseded by a replacement", now.UTC()
		previous.Revision++
		st.Runs[key] = previous
	}
	for _, number := range decision.StopQueued {
		key := runKey(id, number)
		previous, exists := st.Runs[key]
		if !exists || previous.Phase != "queued" || previous.Revision == math.MaxUint64 {
			return ErrConflict
		}
		previous.Phase, previous.StopReason = "stopping", "replaced by a newer run"
		previous.Revision++
		st.Runs[key] = previous
	}
	return nil
}

func (s *Store) UpdateRun(id string, n uint64, fn func(*Run) error) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Run{}, err
	}
	old, ok := s.state.Runs[runKey(id, n)]
	if !ok {
		return Run{}, ErrNotFound
	}
	if fn == nil {
		return Run{}, validation("run callback is required")
	}
	if old.Revision == math.MaxUint64 {
		return Run{}, ErrConflict
	}
	r := cloneRun(old)
	if err := fn(&r); err != nil {
		return Run{}, err
	}
	if r.AutomationID != old.AutomationID || r.Key != old.Key || r.N != old.N || r.Revision != old.Revision || r.InvocationID != old.InvocationID || r.Spec != old.Spec || r.Trigger != old.Trigger || !r.AcceptedAt.Equal(old.AcceptedAt) || !r.ScheduledFor.Equal(old.ScheduledFor) {
		return Run{}, ErrConflict
	}
	if old.Phase == "terminal" && (r.Phase != "terminal" || r.Outcome != old.Outcome) {
		return Run{}, ErrConflict
	}
	r.Revision++
	if err := validateRun(r); err != nil {
		return Run{}, err
	}
	st := s.copyState()
	// The callback can retain its argument, so persisted pointers must not share that argument.
	st.Runs[runKey(id, n)] = cloneRun(r)
	if err := s.save(st); err != nil {
		return Run{}, err
	}
	return cloneRun(r), nil
}
func (s *Store) SetNext(id string, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return err
	}
	d, ok := s.state.Definitions[id]
	if !ok || d.Deleted {
		return ErrNotFound
	}
	if !validInstant(next) {
		return validation("invalid next cursor")
	}
	if next.Before(d.Next) {
		return ErrConflict
	}
	if next.Equal(d.Next) {
		return nil
	}
	d.Next = next.UTC()
	st := s.copyState()
	st.Definitions[id] = d
	return s.save(st)
}

func parseCost(value string) (*big.Rat, bool) {
	if value == "" || strings.ContainsAny(value, "/eE+- ") {
		return nil, false
	}
	dots := 0
	for _, r := range value {
		if r == '.' {
			dots++
		} else if r < '0' || r > '9' {
			return nil, false
		}
	}
	if dots > 1 {
		return nil, false
	}
	n, ok := new(big.Rat).SetString(value)
	return n, ok && n.Sign() >= 0
}
func (s *Store) definition(stored storedDefinition, now time.Time) Definition {
	d := Definition{Spec: stored.Spec, ID: stored.ID, Key: stored.Key, Revision: stored.Revision, CreatedAt: stored.CreatedAt, Next: stored.Next, NextRuns: []time.Time{}, ScheduleHuman: Human(stored.Spec.Schedule)}
	if schedule, err := Compile(d.Schedule, d.Timezone); err == nil {
		next := d.Next
		if !d.Enabled && next.Before(now) {
			next = schedule.Next(now)
		}
		for range 4 {
			if !validInstant(next) {
				break
			}
			d.NextRuns = append(d.NextRuns, next)
			next = schedule.Next(next)
		}
	}
	runs := s.runs(d.ID)
	d.RunsTotal = len(runs)
	complete, finished, walls := 0, 0, 0
	wall := 0.0
	spend := new(big.Rat)
	for _, r := range runs {
		if d.LastRun == nil && r.Phase == "terminal" {
			last := cloneRun(r)
			d.LastRun = &last
		}
		if r.Phase != "terminal" && (d.CurrentRun == nil || (d.CurrentRun.Phase == "queued" && r.Phase != "queued")) {
			current := cloneRun(r)
			d.CurrentRun = &current
			d.Running = r.Phase == "running" || (r.Phase == "stopping" && r.WindowID != "")
		}
		if r.Phase != "terminal" || r.Outcome == "skipped" {
			continue
		}
		finished++
		if r.Outcome == "complete" {
			complete++
		}
		if !r.StartedAt.IsZero() {
			wall += r.CompletedAt.Sub(r.StartedAt).Seconds()
			walls++
		}
		if !r.StartedAt.IsZero() && !r.StartedAt.Before(now.Add(-30*24*time.Hour)) && !r.StartedAt.After(now) {
			d.TrackedRuns++
			if r.Cost != nil {
				if cost, ok := parseCost(*r.Cost); ok {
					spend.Add(spend, cost)
					d.PricedRuns++
				}
			}
		}
	}
	if finished > 0 {
		d.SuccessRate = new(float64(complete) * 100 / float64(finished))
	}
	if walls > 0 {
		d.AvgWall = new(wall / float64(walls))
	}
	if d.PricedRuns > 0 {
		d.Spend30d = new(spend.FloatString(6))
	}
	return d
}

func (s *Store) fail(stage string) error {
	if s.fault != nil {
		return s.fault(stage)
	}
	return nil
}
func (s *Store) save(st snapshot) error {
	if err := validateSnapshot(st); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	data = append(data, '\n')
	if err := s.replace(data); err != nil {
		s.blocked = fmt.Errorf("%w: save snapshot: %w", ErrUnavailable, err)
		return s.blocked
	}
	s.state = st
	s.data = data
	return nil
}
func (s *Store) replace(data []byte) error {
	if err := s.fail("create"); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0600); err == nil {
		err = s.fail("write")
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = s.fail("sync")
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, s.fail("close"), f.Close())
	if err != nil {
		return err
	}
	if err := s.fail("rename"); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.dir, "state.json")); err != nil {
		return err
	}
	if err := s.fail("sync_dir"); err != nil {
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
	err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	err = errors.Join(err, s.lock.Close())
	s.lock = nil
	return err
}
