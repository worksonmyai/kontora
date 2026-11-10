package automation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const snapshotVersion = 1

type receipt struct {
	Fingerprint string     `json:"fingerprint"`
	Input       Definition `json:"input"`
	Result      Definition `json:"result"`
}

type snapshot struct {
	Version     int                   `json:"version"`
	Scope       string                `json:"scope"`
	PrimaryDir  string                `json:"primary_dir"`
	Definitions map[string]Definition `json:"definitions"`
	Occurrences map[string]Occurrence `json:"occurrences"`
	Receipts    map[string]receipt    `json:"receipts"`
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

func Open(root, primaryDir string) (*Store, error) {
	if primaryDir == "" {
		return nil, validation("primary ticket directory is required")
	}
	primary, err := canonical(primaryDir)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical ticket directory: %v", ErrUnavailable, err)
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		root = filepath.Join(home, ".kontora", "automations")
	}
	root, err = canonical(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	hash := sha256.Sum256([]byte(primary))
	scope := hex.EncodeToString(hash[:])
	dir := filepath.Join(root, scope)
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
		s.state = snapshot{Version: snapshotVersion, Scope: scope, PrimaryDir: primary, Definitions: map[string]Definition{}, Occurrences: map[string]Occurrence{}, Receipts: map[string]receipt{}}
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
		if s.state.Scope != scope || s.state.PrimaryDir != primary {
			return fail(errors.New("snapshot scope or primary directory mismatch"))
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
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null snapshot field")
	}
	if typ == reflect.TypeFor[time.Time]() {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		if len(fields) != typ.NumField() {
			return errors.New("missing or unknown snapshot field")
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			value, ok := fields[field.Tag.Get("json")]
			if !ok {
				return errors.New("missing snapshot field")
			}
			if err := strictShape(value, field.Type); err != nil {
				return err
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := strictShape(value, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
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

func validPersistedDefinition(d Definition) bool {
	if !validID(d.ID) || d.Revision == 0 || d.ScheduleRevision == 0 || d.ScheduleRevision > d.Revision || validateDefinition(d) != nil || !validInstant(d.Next) {
		return false
	}
	next, err := d.Schedule.Next(d.Next.Add(-time.Nanosecond))
	return err == nil && next.Equal(d.Next)
}

func validateOccurrence(o Occurrence) error {
	if !validID(o.ID) || o.Revision == 0 || !validID(o.AutomationID) || o.ScheduleRevision == 0 || !validInstant(o.ScheduledFor) || !textValid(o.TicketID, 200, false) || !textValid(o.Reason, 4096, true) {
		return validation("invalid occurrence identity or fields")
	}
	switch o.Phase {
	case "reserved", "publishing", "admitted", "skipped", "blocked", "failed":
	default:
		return validation("invalid occurrence phase")
	}
	if o.Count < 1 {
		return validation("occurrence count must be positive")
	}
	if !o.Through.IsZero() {
		if !validInstant(o.Through) || o.Through.Before(o.ScheduledFor) {
			return validation("invalid occurrence range")
		}
		latest, count, err := o.Payload.Schedule.LatestDue(o.ScheduledFor, o.Through)
		if err != nil || !latest.Equal(o.Through) || count != o.Count {
			return validation("invalid occurrence range count")
		}
	} else if o.Count != 1 {
		return validation("range end is required for multiple slots")
	}
	if o.Count > 1 && (o.Phase != "skipped" || o.TicketID != "") {
		return validation("only skipped occurrences may cover multiple slots")
	}
	if o.Phase == "admitted" && o.TicketID == "" {
		return validation("admitted occurrence requires ticket ID")
	}
	if !validPersistedDefinition(o.Payload) || o.Payload.ID != o.AutomationID || o.Payload.ScheduleRevision != o.ScheduleRevision {
		return validation("invalid frozen payload")
	}
	next, err := o.Payload.Schedule.Next(o.ScheduledFor.Add(-time.Nanosecond))
	if err != nil || !next.Equal(o.ScheduledFor) {
		return validation("occurrence is not a scheduled slot")
	}
	return nil
}

func validateSnapshot(st snapshot) error {
	if st.Version != snapshotVersion || len(st.Scope) != 64 || st.Definitions == nil || st.Occurrences == nil || st.Receipts == nil {
		return errors.New("invalid snapshot header")
	}
	if !filepath.IsAbs(st.PrimaryDir) || filepath.Clean(st.PrimaryDir) != st.PrimaryDir {
		return errors.New("invalid primary ticket directory")
	}
	hash := sha256.Sum256([]byte(st.PrimaryDir))
	if st.Scope != hex.EncodeToString(hash[:]) {
		return errors.New("snapshot scope does not match primary directory")
	}
	for id, d := range st.Definitions {
		if id != d.ID || !validPersistedDefinition(d) {
			return errors.New("invalid definition")
		}
	}
	slots := map[string]bool{}
	ordered := make([]Occurrence, 0, len(st.Occurrences))
	for id, o := range st.Occurrences {
		d, ok := st.Definitions[o.AutomationID]
		key := slotKey(o)
		if id != o.ID || !ok || o.Payload.Revision > d.Revision || o.ScheduleRevision > d.ScheduleRevision || validateOccurrence(o) != nil || slots[key] {
			return errors.New("invalid occurrence")
		}
		slots[key] = true
		ordered = append(ordered, o)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.AutomationID != b.AutomationID {
			return a.AutomationID < b.AutomationID
		}
		if a.ScheduleRevision != b.ScheduleRevision {
			return a.ScheduleRevision < b.ScheduleRevision
		}
		return a.ScheduledFor.Before(b.ScheduledFor)
	})
	for i := 1; i < len(ordered); i++ {
		if rangesOverlap(ordered[i-1], ordered[i]) {
			return errors.New("overlapping occurrence ranges")
		}
	}
	created := map[string]bool{}
	for id, r := range st.Receipts {
		d, ok := st.Definitions[r.Result.ID]
		if !validRequestID(id) || !ok || len(r.Fingerprint) != 64 || !validPersistedDefinition(r.Result) || r.Result.Revision != 1 || r.Result.ScheduleRevision != 1 || r.Result.Enabled || r.Result.Revision > d.Revision {
			return errors.New("invalid receipt")
		}
		if _, err := hex.DecodeString(r.Fingerprint); err != nil {
			return err
		}
		fp, err := fingerprint(r.Input)
		if err != nil || fp != r.Fingerprint || created[r.Result.ID] {
			return errors.New("receipt fingerprint mismatch or duplicate receipt")
		}
		if validateDefinition(r.Input) != nil || r.Input.ID != "" || r.Input.Revision != 0 || r.Input.ScheduleRevision != 0 {
			return errors.New("invalid receipt input")
		}
		expected := createInput(r.Input)
		expected.ID, expected.Revision, expected.ScheduleRevision, expected.Next = r.Result.ID, 1, 1, r.Result.Next
		if !reflect.DeepEqual(expected, r.Result) {
			return errors.New("receipt result does not match input")
		}
		created[r.Result.ID] = true
	}
	if len(created) != len(st.Definitions) {
		return errors.New("missing create receipt")
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
	st.Definitions = make(map[string]Definition, len(s.state.Definitions))
	for k, v := range s.state.Definitions {
		st.Definitions[k] = v
	}
	st.Occurrences = make(map[string]Occurrence, len(s.state.Occurrences))
	for k, v := range s.state.Occurrences {
		st.Occurrences[k] = v
	}
	st.Receipts = make(map[string]receipt, len(s.state.Receipts))
	for k, v := range s.state.Receipts {
		st.Receipts[k] = v
	}
	return st
}

func (s *Store) List() ([]Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(s.state.Definitions))
	for _, d := range s.state.Definitions {
		out = append(out, cloneDefinition(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) Get(id string) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Definition{}, err
	}
	d, ok := s.state.Definitions[id]
	if !ok {
		return Definition{}, ErrNotFound
	}
	return cloneDefinition(d), nil
}

func validRequestID(id string) bool { return id != "" && textValid(id, 200, false) }

func createInput(d Definition) Definition {
	d = cloneDefinition(d)
	d.ID = ""
	d.Revision = 0
	d.ScheduleRevision = 0
	d.Next = time.Time{}
	d.Enabled = false
	sort.Ints(d.Schedule.Days)
	return d
}

// Retries must match the decoded payload, including weekday order; nil and empty weekdays are equivalent.
func fingerprint(d Definition) (string, error) {
	data, err := json.Marshal(cloneDefinition(d))
	if err != nil {
		return "", validation("invalid create payload: %v", err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

// CreateReceipt skips live resource validation so retries survive agent or repository removal.
func (s *Store) CreateReceipt(requestID string, input Definition) (Definition, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Definition{}, false, err
	}
	return s.createReceipt(requestID, input)
}

func (s *Store) createReceipt(requestID string, input Definition) (Definition, bool, error) {
	if !validRequestID(requestID) {
		return Definition{}, false, validation("request ID is required (max 200 bytes)")
	}
	r, ok := s.state.Receipts[requestID]
	if !ok {
		return Definition{}, false, nil
	}
	fp, err := fingerprint(input)
	if err != nil || r.Fingerprint != fp {
		return Definition{}, false, ErrConflict
	}
	return cloneDefinition(r.Result), true, nil
}

func (s *Store) Create(requestID string, input Definition, now time.Time) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Definition{}, err
	}
	if result, found, err := s.createReceipt(requestID, input); found || err != nil {
		return result, err
	}
	if err := validateDefinition(input); err != nil {
		return Definition{}, err
	}
	if input.ID != "" || input.Revision != 0 || input.ScheduleRevision != 0 {
		return Definition{}, validation("new definition must not have an identity or revision")
	}
	fp, err := fingerprint(input)
	if err != nil {
		return Definition{}, err
	}
	if !validInstant(now) {
		return Definition{}, validation("now is out of range")
	}
	d := createInput(input)
	for {
		d.ID = newID()
		if _, exists := s.state.Definitions[d.ID]; !exists {
			break
		}
	}
	d.Revision = 1
	d.ScheduleRevision = 1
	next, err := d.Schedule.Next(now)
	if err != nil {
		return Definition{}, err
	}
	d.Next = next
	st := s.copyState()
	st.Definitions[d.ID] = d
	st.Receipts[requestID] = receipt{Fingerprint: fp, Input: cloneDefinition(input), Result: d}
	if err := s.save(st); err != nil {
		return Definition{}, err
	}
	return cloneDefinition(d), nil
}

func sameSchedule(a, b Schedule) bool {
	a.Days = append([]int{}, a.Days...)
	b.Days = append([]int{}, b.Days...)
	sort.Ints(a.Days)
	sort.Ints(b.Days)
	return reflect.DeepEqual(a, b)
}

func (s *Store) Update(input Definition, now time.Time) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Definition{}, err
	}
	old, ok := s.state.Definitions[input.ID]
	if !ok {
		return Definition{}, ErrNotFound
	}
	if input.Revision != old.Revision || input.ScheduleRevision != old.ScheduleRevision {
		return Definition{}, ErrConflict
	}
	if err := validateDefinition(input); err != nil {
		return Definition{}, err
	}
	if !validInstant(now) {
		return Definition{}, validation("now is out of range")
	}
	if old.Revision == math.MaxUint64 {
		return Definition{}, ErrConflict
	}
	d := cloneDefinition(input)
	d.Revision++
	d.Next = old.Next
	sort.Ints(d.Schedule.Days)
	changed := !sameSchedule(d.Schedule, old.Schedule)
	if changed {
		d.ScheduleRevision++
	}
	if changed || (d.Enabled && !old.Enabled) {
		after := now
		if !changed {
			// A saved occurrence can precede its cursor save. Do not select that slot again.
			for _, o := range s.state.Occurrences {
				if o.AutomationID != d.ID || o.ScheduleRevision != d.ScheduleRevision {
					continue
				}
				end := o.Through
				if end.IsZero() {
					end = o.ScheduledFor
				}
				if end.After(after) {
					after = end
				}
			}
		}
		next, err := d.Schedule.Next(after)
		if err != nil {
			return Definition{}, err
		}
		if changed || next.After(old.Next) {
			d.Next = next
		}
	}
	st := s.copyState()
	st.Definitions[d.ID] = d
	if err := s.save(st); err != nil {
		return Definition{}, err
	}
	return cloneDefinition(d), nil
}

func (s *Store) Occurrences(id string) ([]Occurrence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	if _, ok := s.state.Definitions[id]; !ok {
		return nil, ErrNotFound
	}
	out := []Occurrence{}
	for _, o := range s.state.Occurrences {
		if o.AutomationID == id {
			o.Payload = cloneDefinition(o.Payload)
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ScheduledFor.Equal(out[j].ScheduledFor) {
			return out[i].ID < out[j].ID
		}
		return out[i].ScheduledFor.Before(out[j].ScheduledFor)
	})
	return out, nil
}

func slotKey(o Occurrence) string {
	return fmt.Sprintf("%s/%d/%s", o.AutomationID, o.ScheduleRevision, o.ScheduledFor.UTC().Format(time.RFC3339Nano))
}

func rangesOverlap(a, b Occurrence) bool {
	if a.AutomationID != b.AutomationID || a.ScheduleRevision != b.ScheduleRevision {
		return false
	}
	aEnd, bEnd := a.Through, b.Through
	if aEnd.IsZero() {
		aEnd = a.ScheduledFor
	}
	if bEnd.IsZero() {
		bEnd = b.ScheduledFor
	}
	return !a.ScheduledFor.After(bEnd) && !b.ScheduledFor.After(aEnd)
}

func terminal(phase string) bool { return phase != "reserved" && phase != "publishing" }

func (s *Store) SaveOccurrence(o Occurrence) (Occurrence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Occurrence{}, err
	}
	d, ok := s.state.Definitions[o.AutomationID]
	if !ok {
		return Occurrence{}, ErrNotFound
	}
	old, exists := s.state.Occurrences[o.ID]
	if exists {
		if o.Revision != old.Revision || old.Revision == math.MaxUint64 {
			return Occurrence{}, ErrConflict
		}
		if o.AutomationID != old.AutomationID || o.ScheduleRevision != old.ScheduleRevision || !o.ScheduledFor.Equal(old.ScheduledFor) || !o.Through.Equal(old.Through) || o.Count != old.Count || !reflect.DeepEqual(o.Payload, old.Payload) || (old.TicketID != "" && o.TicketID != old.TicketID) || (terminal(old.Phase) && o.Phase != old.Phase && !(old.Phase == "blocked" && o.Phase == "skipped")) || (old.Phase == "publishing" && o.Phase == "reserved") {
			return Occurrence{}, ErrConflict
		}
	} else {
		if o.Revision != 0 {
			return Occurrence{}, ErrConflict
		}
		if o.ID == "" {
			for {
				o.ID = newID()
				if _, exists := s.state.Occurrences[o.ID]; !exists {
					break
				}
			}
		}
		if o.ScheduleRevision != d.ScheduleRevision {
			return Occurrence{}, ErrConflict
		}
		if o.Payload.ID == "" {
			o.Payload = cloneDefinition(d)
		} else {
			payload, current := cloneDefinition(o.Payload), cloneDefinition(d)
			payload.Next = current.Next
			if !reflect.DeepEqual(payload, current) {
				return Occurrence{}, ErrConflict
			}
		}
		if o.Count == 0 {
			o.Count = 1
		}
		for _, other := range s.state.Occurrences {
			if rangesOverlap(other, o) {
				return Occurrence{}, ErrConflict
			}
		}
	}
	o.Revision++
	o.ScheduledFor = o.ScheduledFor.UTC()
	o.Payload = cloneDefinition(o.Payload)
	if err := validateOccurrence(o); err != nil {
		return Occurrence{}, err
	}
	st := s.copyState()
	st.Occurrences[o.ID] = o
	if err := s.save(st); err != nil {
		return Occurrence{}, err
	}
	o.Payload = cloneDefinition(o.Payload)
	return o, nil
}

func (s *Store) SetNext(id string, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return err
	}
	d, ok := s.state.Definitions[id]
	if !ok {
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
	if !validPersistedDefinition(d) {
		return validation("next cursor must be a scheduled slot")
	}
	st := s.copyState()
	st.Definitions[id] = d
	return s.save(st)
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
