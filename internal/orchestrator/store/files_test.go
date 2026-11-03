package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/worksonmyai/kontora/internal/orchestrator"
)

func TestBoardAuthorityDiscovery(t *testing.T) {
	for _, id := range []string{"board", "", "..", "../escape", "nested/board"} {
		t.Run("default-state/"+id, func(t *testing.T) {
			path, err := DefaultStateDir(id)
			if id != "board" {
				if err == nil {
					t.Fatal("unsafe board ID accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			if path != filepath.Join(home, ".kontora", "orchestrator", id) {
				t.Fatalf("unexpected default state directory: %s", path)
			}
		})
	}
	tickets := t.TempDir()
	a := Authority{Version: 1, BoardID: "board", OwnerID: "owner", StateDir: filepath.Join(t.TempDir(), "state")}
	a.StateDir, _ = canonicalDestination(a.StateDir)
	if _, err := Discover(tickets); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent registry: %v", err)
	}
	lock, err := AcquireBoard(tickets, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	discovered, err := Discover(tickets)
	if err != nil || discovered != a {
		t.Fatalf("authority discovery: %+v %v", discovered, err)
	}
	lockPath := filepath.Join(tickets, ".kontora", "board.lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "tickets-alias")
	if err = os.Symlink(tickets, alias); err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireBoard(alias, a); !errors.Is(err, ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("aliased board writer: %v", err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Authority)
	}{
		{"board", func(p *Authority) { p.BoardID = "different" }},
		{"owner", func(p *Authority) { p.OwnerID = "different" }},
		{"configuration", func(p *Authority) { p.StateDir = filepath.Join(t.TempDir(), "other-state") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proposed := a
			tc.change(&proposed)
			if other, err := AcquireBoard(alias, proposed); !errors.Is(err, orchestrator.ErrConflict) {
				if other != nil {
					other.Close()
				}
				t.Fatalf("conflicting authority: %v", err)
			}
			if got, err := Discover(alias); err != nil || got != a {
				t.Fatalf("conflicting configuration replaced authority: %+v %v", got, err)
			}
		})
	}
	lock, err = AcquireBoard(alias, a)
	if err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, newInfo) {
		t.Fatal("board lock was replaced on release")
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(lockPath); err != nil {
		t.Fatal("board lock was removed")
	}
}

func TestAuthorityFaultRecovery(t *testing.T) {
	for _, tc := range []struct {
		point     string
		persisted bool
	}{
		{"authority.before_rename", false},
		{"authority.after_rename", true},
	} {
		t.Run(tc.point, func(t *testing.T) {
			tickets := t.TempDir()
			a := Authority{Version: 1, BoardID: "board", OwnerID: "owner", StateDir: t.TempDir()}
			a.StateDir, _ = canonicalDir(a.StateDir)
			lock, err := acquireBoard(tickets, a, func(point string) error {
				if point == tc.point {
					return errors.New("authority fault")
				}
				return nil
			})
			if err == nil || lock != nil {
				if lock != nil {
					lock.Close()
				}
				t.Fatal("fault did not fail acquisition")
			}
			got, err := Discover(tickets)
			if tc.persisted {
				if err != nil || got != a {
					t.Fatalf("persisted authority lost: %+v %v", got, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("uncommitted authority visible: %v", err)
			}
			lock, err = AcquireBoard(tickets, a)
			if err != nil {
				t.Fatal(err)
			}
			if err = lock.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCorruptAuthorityFailsClosed(t *testing.T) {
	for _, data := range [][]byte{[]byte("{"), []byte(`{"authority":{},"checksum":"wrong"}`), []byte("null"), []byte(`{}\n{}`)} {
		t.Run(string(data), func(t *testing.T) {
			tickets := t.TempDir()
			if err := os.Mkdir(filepath.Join(tickets, ".kontora"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(tickets, ".kontora", "authority.json")
			mustWrite(t, path, data)
			a := Authority{Version: 1, BoardID: "board", OwnerID: "owner", StateDir: t.TempDir()}
			if _, err := Discover(tickets); err == nil {
				t.Fatal("corrupt authority discovered")
			}
			if lock, err := AcquireBoard(tickets, a); err == nil {
				lock.Close()
				t.Fatal("corrupt authority overwritten")
			}
			if !bytes.Equal(data, mustRead(t, path)) {
				t.Fatal("corrupt registry was changed")
			}
		})
	}
}

func TestOpenBoardReservesBeforeProjection(t *testing.T) {
	tickets := t.TempDir()
	a := Authority{BoardID: "board", OwnerID: "owner", StateDir: t.TempDir()}
	s, err := OpenBoard(tickets, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Apply(objectiveCommand("o", 0)); err != nil {
		t.Fatal(err)
	}
	if err = s.Apply(orchestrator.Command{Kind: orchestrator.PutWork, ExpectedRevision: 1, Work: orchestrator.WorkItem{ID: "w", ObjectiveID: "o", Kind: orchestrator.WorkTicket, TicketID: "ticket", Specification: "owned"}}); err != nil {
		t.Fatal(err)
	}
	// No ticket projection exists. The registry still leads another daemon to the ownership reservation.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(tickets)
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenBoard(tickets, got)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State()
	if err != nil || state.Work["w"].TicketID != "ticket" {
		t.Fatalf("enrollment reservation lost: %+v %v", state, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(tickets, ".kontora", "board.lock")); err != nil {
		t.Fatal(err)
	}
}

func TestRegisteredStateLossFailsClosed(t *testing.T) {
	for _, missing := range []string{"store.json", "journal.jsonl", "state-directory"} {
		t.Run(missing, func(t *testing.T) {
			tickets := t.TempDir()
			a := Authority{BoardID: "board", OwnerID: "owner", StateDir: filepath.Join(t.TempDir(), "state")}
			s, err := OpenBoard(tickets, a)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Apply(objectiveCommand("o", 0)); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			a, err = Discover(tickets)
			if err != nil {
				t.Fatal(err)
			}
			registryPath := filepath.Join(tickets, ".kontora", "authority.json")
			registry := mustRead(t, registryPath)
			if missing == "state-directory" {
				err = os.RemoveAll(a.StateDir)
			} else {
				err = os.Remove(filepath.Join(a.StateDir, missing))
			}
			if err != nil {
				t.Fatal(err)
			}
			if reopened, err := OpenBoard(tickets, a); !errors.Is(err, ErrRecovery) {
				if reopened != nil {
					reopened.Close()
				}
				t.Fatalf("missing registered state became a fresh board: %v", err)
			}
			if !bytes.Equal(registry, mustRead(t, registryPath)) {
				t.Fatal("state loss replaced authority")
			}
			if missing == "state-directory" {
				if _, err := os.Stat(a.StateDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("lost state directory was recreated")
				}
			}
		})
	}
}

func TestArtifacts(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	data := []byte("retained research evidence")
	ref, err := s.PutArtifact(data)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash != digest(data) || ref.Size != int64(len(data)) {
		t.Fatal("artifact was not content addressed")
	}
	if again, err := s.PutArtifact(data); err != nil || again != ref {
		t.Fatalf("idempotent artifact: %+v %v", again, err)
	}
	got, err := s.ReadArtifact(ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("artifact round trip: %q %v", got, err)
	}
	for _, bad := range []orchestrator.ArtifactRef{{Hash: "../escape"}, {Hash: strings.ToUpper(ref.Hash), Size: ref.Size}, {Hash: ref.Hash, Size: ref.Size + 1}, {Hash: ref.Hash, Size: -1}} {
		if _, err = s.ReadArtifact(bad); err == nil {
			t.Fatalf("bad reference accepted: %+v", bad)
		}
	}
}

func TestArtifactWriteFaults(t *testing.T) {
	for _, point := range []string{"artifact.before_rename", "artifact.after_rename"} {
		t.Run(point, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			s.fault = func(p string) error {
				if p == point {
					return errors.New("artifact fault")
				}
				return nil
			}
			if _, err := s.PutArtifact([]byte("output")); err == nil {
				t.Fatal("artifact fault ignored")
			}
			s.fault = nil
			ref, err := s.PutArtifact([]byte("output"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ReadArtifact(ref); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func commitArtifact(t *testing.T, s *Store, ref orchestrator.ArtifactRef) {
	t.Helper()
	if err := s.Apply(objectiveCommand("o", 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(orchestrator.Command{Kind: orchestrator.RecordAction, ExpectedRevision: 1, Action: orchestrator.Action{ID: "intent", ObjectiveID: "o", Kind: "projection", Payload: ref}}); err != nil {
		t.Fatal(err)
	}
}

func TestReferencedArtifactRecovery(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt", "symlink", "wrong-size"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			ref, err := s.PutArtifact([]byte("evidence"))
			if err != nil {
				t.Fatal(err)
			}
			commitArtifact(t, s, ref)
			if err = s.Snapshot(); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "artifacts", ref.Hash)
			switch kind {
			case "missing":
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				mustWrite(t, path, []byte("changed!"))
			case "wrong-size":
				mustWrite(t, path, []byte("short"))
			case "symlink":
				target := filepath.Join(t.TempDir(), "artifact")
				mustWrite(t, target, []byte("evidence"))
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if recovered, err := Open(dir, "board"); err == nil {
				recovered.Close()
				t.Fatal("broken artifact admitted")
			} else if !errors.Is(err, ErrRecovery) {
				t.Fatalf("artifact recovery: %v", err)
			}
		})
	}
}

func TestMissingArtifactCannotCommit(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	if err := s.Apply(objectiveCommand("o", 0)); err != nil {
		t.Fatal(err)
	}
	ref := orchestrator.ArtifactRef{Hash: digest([]byte("not stored")), Size: 10}
	if err := s.Apply(orchestrator.Command{Kind: orchestrator.RecordAction, ExpectedRevision: 1, Action: orchestrator.Action{ID: "a", ObjectiveID: "o", Kind: "projection", Payload: ref}}); err == nil {
		t.Fatal("missing artifact committed")
	}
	state, err := s.State()
	if err != nil || state.Revision != 1 || len(state.Actions) != 0 {
		t.Fatal("rejected artifact reference changed state")
	}
}
