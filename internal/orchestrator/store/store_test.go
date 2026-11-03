package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/worksonmyai/kontora/internal/orchestrator"
)

func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, "board")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func objectiveCommand(id string, revision uint64) orchestrator.Command {
	return orchestrator.Command{Kind: orchestrator.CreateObjective, ExpectedRevision: revision, Objective: orchestrator.Objective{ID: id, Assignment: "build", Criteria: []string{"checked"}, Budget: orchestrator.Budget{MaxAttempts: 10, MaxRuntime: time.Hour}}}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestJournalCommitFailures(t *testing.T) {
	for _, tc := range []struct {
		point     string
		recovered uint64
		poisoned  bool
	}{
		{"journal.before_write", 0, false},
		{"journal.after_write", 1, true},
		{"journal.after_sync", 1, true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			s.fault = func(point string) error {
				if point == tc.point {
					return errors.New("injected failure")
				}
				return nil
			}
			if err := s.Apply(objectiveCommand("o", 0)); err == nil {
				t.Fatal("fault was not returned")
			}
			state, err := s.State()
			if tc.poisoned {
				if !errors.Is(err, ErrRecovery) {
					t.Fatalf("uncertain commit remained usable: %v", err)
				}
				if err = s.Apply(objectiveCommand("retry", 0)); !errors.Is(err, ErrRecovery) {
					t.Fatalf("retry after uncertain commit: %v", err)
				}
			} else if err != nil || state.Revision != 0 {
				t.Fatalf("pre-write failure changed state: %+v %v", state, err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTestStore(t, dir)
			state, err = s.State()
			if err != nil || state.Revision != tc.recovered {
				t.Fatalf("recovered revision %d: %v", state.Revision, err)
			}
			if tc.recovered == 1 && state.Objectives["o"].ID != "o" {
				t.Fatal("committed intent lost")
			}
		})
	}
}

func TestIncompleteTailPreserved(t *testing.T) {
	base := t.TempDir()
	s := openTestStore(t, base)
	if err := s.Apply(objectiveCommand("o", 0)); err != nil {
		t.Fatal(err)
	}
	first := mustRead(t, filepath.Join(base, "journal.jsonl"))
	if err := s.Apply(objectiveCommand("second", 1)); err != nil {
		t.Fatal(err)
	}
	full := mustRead(t, filepath.Join(base, "journal.jsonl"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	second := full[len(first):]
	for _, cut := range []int{1, len(second) / 2, len(second) - 1} {
		t.Run(fmt.Sprintf("cut-%d", cut), func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, "store.json"), mustRead(t, filepath.Join(base, "store.json")))
			path := filepath.Join(dir, "journal.jsonl")
			data := append(append([]byte{}, first...), second[:cut]...)
			mustWrite(t, path, data)
			for range 2 {
				store, err := Open(dir, "board")
				if store != nil {
					store.Close()
					t.Fatal("incomplete tail returned writable store")
				}
				var recovery *RecoveryError
				if !errors.Is(err, ErrIncompleteTail) || !errors.Is(err, ErrRecovery) || !errors.As(err, &recovery) || recovery.Offset != int64(len(first)) {
					t.Fatalf("tail recovery: %v", err)
				}
				if !bytes.Equal(data, mustRead(t, path)) {
					t.Fatal("recovery changed incomplete journal bytes")
				}
			}
		})
	}
}

func TestJournalCorruption(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"bad-checksum", func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"assignment":"build"`), []byte(`"assignment":"other"`), 1)
		}},
		{"interior-json", func(data []byte) []byte { data[0] = '!'; return data }},
		{"duplicate-record", func(data []byte) []byte { return append(data, data...) }},
		{"sequence-gap", func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"sequence":1`), []byte(`"sequence":2`), 1)
		}},
		{"wrong-board", func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"board_id":"board"`), []byte(`"board_id":"other"`), 1)
		}},
		{"blank-line", func(data []byte) []byte { return append([]byte{'\n'}, data...) }},
		{"duplicate-json-key", func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			if err := s.Apply(objectiveCommand("o", 0)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "journal.jsonl")
			data := tc.mutate(mustRead(t, path))
			mustWrite(t, path, data)
			if recovered, err := Open(dir, "board"); err == nil {
				recovered.Close()
				t.Fatal("corruption accepted")
			} else if !errors.Is(err, ErrRecovery) {
				t.Fatalf("not a recovery error: %v", err)
			}
			if !bytes.Equal(data, mustRead(t, path)) {
				t.Fatal("corrupt journal was changed")
			}
		})
	}
}

func TestSnapshots(t *testing.T) {
	for _, point := range []string{"", "snapshot.before_rename", "snapshot.after_rename"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			if err := s.Snapshot(); err != nil {
				t.Fatal(err)
			}
			if err := s.Apply(objectiveCommand("o", 0)); err != nil {
				t.Fatal(err)
			}
			s.fault = func(p string) error {
				if p == point {
					return errors.New("snapshot fault")
				}
				return nil
			}
			err := s.Snapshot()
			if (err == nil) != (point == "") {
				t.Fatalf("snapshot result: %v", err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTestStore(t, dir)
			state, err := s.State()
			if err != nil || state.Revision != 1 || state.Objectives["o"].ID == "" {
				t.Fatalf("snapshot recovery: %+v %v", state, err)
			}
			if err = s.Apply(objectiveCommand("next", 1)); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTestStore(t, dir)
			state, _ = s.State()
			if state.Revision != 2 {
				t.Fatal("journal after snapshot was not replayed")
			}
		})
	}
}

func TestSnapshotCorruption(t *testing.T) {
	for _, kind := range []string{"checksum", "missing-journal", "mismatched-state", "unknown-field"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			if err := s.Apply(objectiveCommand("o", 0)); err != nil {
				t.Fatal(err)
			}
			if err := s.Snapshot(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "snapshot.json")
			data := mustRead(t, path)
			switch kind {
			case "checksum":
				data = bytes.Replace(data, []byte(`"assignment":"build"`), []byte(`"assignment":"other"`), 1)
			case "unknown-field":
				data = bytes.Replace(data, []byte(`"body":`), []byte(`"unknown":0,"body":`), 1)
			case "missing-journal":
				mustWrite(t, filepath.Join(dir, "journal.jsonl"), nil)
			case "mismatched-state":
				var r snapshotRecord
				if err := json.Unmarshal(data, &r); err != nil {
					t.Fatal(err)
				}
				o := r.Body.State.Objectives["o"]
				o.Assignment = "different"
				r.Body.State.Objectives[o.ID] = o
				body, _ := json.Marshal(r.Body)
				r.Checksum = digest(body)
				data, _ = json.Marshal(r)
			}
			mustWrite(t, path, data)
			if recovered, err := Open(dir, "board"); err == nil {
				recovered.Close()
				t.Fatal("corrupt snapshot accepted")
			} else if !errors.Is(err, ErrRecovery) {
				t.Fatalf("unexpected recovery error: %v", err)
			}
		})
	}
}

func TestSynchronizedTransactions(t *testing.T) {
	for _, mode := range []string{"same-revision", "retry-stale"} {
		t.Run(mode, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			var successes atomic.Int32
			var wg sync.WaitGroup
			for i := range 16 {
				wg.Go(func() {
					for {
						var revision uint64
						if mode == "retry-stale" {
							state, err := s.State()
							if err != nil {
								t.Error(err)
								return
							}
							revision = state.Revision
						}
						err := s.Apply(objectiveCommand(fmt.Sprintf("o-%d", i), revision))
						if err == nil {
							successes.Add(1)
							return
						}
						if !errors.Is(err, orchestrator.ErrStale) {
							t.Error(err)
							return
						}
						if mode == "same-revision" {
							return
						}
					}
				})
			}
			wg.Wait()
			want := int32(1)
			if mode == "retry-stale" {
				want = 16
			}
			state, err := s.State()
			if err != nil || successes.Load() != want || state.Revision != uint64(want) {
				t.Fatalf("transactions: successes %d state %d: %v", successes.Load(), state.Revision, err)
			}
		})
	}
}

func TestAtomicStateAndIntentPersistence(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	if err := s.Apply(objectiveCommand("o", 0)); err != nil {
		t.Fatal(err)
	}
	commands := []orchestrator.Command{
		{Kind: orchestrator.PutWork, ExpectedRevision: 1, Work: orchestrator.WorkItem{ID: "w", ObjectiveID: "o", Kind: orchestrator.WorkTicket, TicketID: "ticket", Specification: "managed"}},
		{Kind: orchestrator.RecordAction, ExpectedRevision: 1, Action: orchestrator.Action{ID: "project", ObjectiveID: "o", Kind: "projection", Preconditions: orchestrator.Preconditions{WorkID: "w", DocumentRevision: 1, SpecificationRevision: 1}}},
	}
	if err := s.ApplyBatch(1, commands); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, dir)
	state, err := s.State()
	if err != nil || state.Revision != 2 || state.Work["w"].TicketID != "ticket" || state.Actions["project"].Status != orchestrator.ActionPending {
		t.Fatalf("state and intent split across a transaction: %+v %v", state, err)
	}
	lines := bytes.Count(mustRead(t, filepath.Join(dir, "journal.jsonl")), []byte{'\n'})
	if lines != 2 {
		t.Fatalf("batch wrote %d journal records, want 2 total", lines)
	}
}

func TestTransactionIsolation(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	var retained orchestrator.State
	if err := s.Transact(0, func(state orchestrator.State) (orchestrator.State, error) {
		n, err := orchestrator.Apply(state, objectiveCommand("o", 0))
		retained = n
		return n, err
	}); err != nil {
		t.Fatal(err)
	}
	o := retained.Objectives["o"]
	o.Criteria[0] = "corrupt"
	retained.Objectives[o.ID] = o
	view, _ := s.State()
	delete(view.Objectives, "o")
	if err := s.Transact(1, func(state orchestrator.State) (orchestrator.State, error) {
		delete(state.Objectives, "o")
		return state, errors.New("abort")
	}); err == nil {
		t.Fatal("callback error lost")
	}
	state, _ := s.State()
	if state.Revision != 1 || state.Objectives["o"].Criteria[0] != "checked" {
		t.Fatal("transaction memory aliases committed state")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.State(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed state: %v", err)
	}
	if err := s.Apply(objectiveCommand("next", 1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed write: %v", err)
	}
}

func TestStoreIdentity(t *testing.T) {
	for _, kind := range []string{"wrong-board", "missing-journal", "corrupt-identity"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			boardID := "board"
			switch kind {
			case "wrong-board":
				boardID = "other"
			case "missing-journal":
				if err := os.Remove(filepath.Join(dir, "journal.jsonl")); err != nil {
					t.Fatal(err)
				}
			case "corrupt-identity":
				mustWrite(t, filepath.Join(dir, "store.json"), []byte("{}"))
			}
			if reopened, err := Open(dir, boardID); !errors.Is(err, ErrRecovery) {
				if reopened != nil {
					reopened.Close()
				}
				t.Fatalf("store identity ignored: %v", err)
			}
		})
	}
}

func TestIntentAndReservationRecovery(t *testing.T) {
	for _, kind := range []orchestrator.CommandKind{orchestrator.CancelAttempt, orchestrator.ParkAttempt, orchestrator.UncertainAttempt} {
		t.Run(string(kind), func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			commands := []orchestrator.Command{
				objectiveCommand("o", 0),
				{Kind: orchestrator.SetObjectiveStatus, ExpectedRevision: 1, ObjectiveID: "o", Status: orchestrator.ObjectiveActive},
				{Kind: orchestrator.PutWork, ExpectedRevision: 2, Work: orchestrator.WorkItem{ID: "w", ObjectiveID: "o", Kind: orchestrator.WorkJob, Specification: "run"}},
				{Kind: orchestrator.RecordAction, ExpectedRevision: 3, Action: orchestrator.Action{ID: "intent", ObjectiveID: "o", Kind: "projection"}},
				{Kind: orchestrator.AdmitAttempt, ExpectedRevision: 4, At: time.Now().UTC(), Capacity: 1, Attempt: orchestrator.Attempt{ID: "a", WorkID: "w", ObjectiveID: "o", SpecificationRevision: 1, Recipe: orchestrator.Recipe{Profile: "worker", ConfigurationRevision: "config", Timeout: time.Minute}, Reservation: orchestrator.Reservation{Slots: 1}}, Leases: []orchestrator.Lease{{Key: "workspace", WorkID: "w", Retain: true}}},
				{Kind: kind, ExpectedRevision: 5, WorkID: "w", AttemptID: "a", Reason: "stop exact process"},
			}
			for _, command := range commands {
				if err := s.Apply(command); err != nil {
					t.Fatalf("%s: %v", command.Kind, err)
				}
			}
			if err := s.Snapshot(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTestStore(t, dir)
			state, err := s.State()
			if err != nil {
				t.Fatal(err)
			}
			if state.Actions["intent"].Status != orchestrator.ActionPending || !state.Attempts["a"].Reservation.Held || state.Leases["workspace"].AttemptID != "a" || state.Objectives["o"].Usage.Attempts != 1 {
				t.Fatal("restart lost an ambiguous intent or live reservation")
			}
			if err = orchestrator.Ready(state, "w"); !errors.Is(err, orchestrator.ErrNotReady) {
				t.Fatalf("restart admitted unreconciled work: %v", err)
			}
		})
	}
}

func TestStoreSingleWriter(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	if second, err := Open(dir, "board"); !errors.Is(err, ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second writer: %v", err)
	}
	lockInfo, err := os.Stat(filepath.Join(dir, "store.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	openTestStore(t, dir)
	newInfo, err := os.Stat(filepath.Join(dir, "store.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(lockInfo, newInfo) {
		t.Fatal("store lock was replaced")
	}
}
