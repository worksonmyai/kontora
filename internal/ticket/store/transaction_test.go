package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func assertBoardLocks(t *testing.T, dir string, held bool) {
	t.Helper()
	for _, name := range []string{"mutation.lock", "board.lock"} {
		f, err := os.OpenFile(filepath.Join(dir, ".kontora", name), os.O_RDWR, 0o600)
		require.NoError(t, err)
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if held {
			require.ErrorIs(t, err, syscall.EWOULDBLOCK, name)
		} else {
			require.NoError(t, err, name)
			require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
		}
		require.NoError(t, f.Close())
	}
}

func TestDiskRepoWithMutationFreshReadsAndNonRelockingSaves(t *testing.T) {
	dir := t.TempDir()
	path := writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Before\n")
	repo := NewDiskRepo(dir)
	before, err := repo.Get("tst-001")
	require.NoError(t, err)
	require.NoError(t, repo.WithMutation(func(scoped app.MutationRepository) error {
		assertBoardLocks(t, dir, true)
		// An earlier snapshot must not satisfy any scoped read. Direct disk
		// changes here stand in for changes committed before lock acquisition.
		require.NoError(t, os.WriteFile(path, []byte("---\nid: tst-001\nstatus: open\n---\n# Fresh\n"), 0o644))
		writeTestTicket(t, dir, "tst-002", "---\nid: tst-002\nstatus: todo\n---\n# New\n")
		id, err := scoped.Resolve("tst-002")
		require.NoError(t, err)
		require.Equal(t, "tst-002", id)
		_, err = scoped.Resolve("tst")
		require.ErrorContains(t, err, "ambiguous")
		st, err := scoped.Get("tst-001")
		require.NoError(t, err)
		require.Equal(t, ticket.StatusOpen, st.Ticket.Status)
		require.Equal(t, ticket.StatusTodo, before.Ticket.Status)
		listed, err := scoped.List()
		require.NoError(t, err)
		require.Len(t, listed, 2)
		require.Equal(t, ticket.StatusOpen, listed[0].Ticket.Status)
		for _, status := range []string{"paused", "done"} {
			require.NoError(t, st.Ticket.SetField("status", status))
			require.NoError(t, scoped.Save(st))
			fresh, err := scoped.Get("tst-001")
			require.NoError(t, err)
			require.Equal(t, ticket.Status(status), fresh.Ticket.Status)
			listed, err = scoped.List()
			require.NoError(t, err)
			require.Equal(t, ticket.Status(status), listed[0].Ticket.Status)
		}
		return nil
	}))
	assertBoardLocks(t, dir, false)
}

func TestDiskRepoWithMutationReleasesLocks(t *testing.T) {
	failure := errors.New("callback failed")
	for _, outcome := range []string{"success", "error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			repo := NewDiskRepo(dir)
			run := func() error {
				return repo.WithMutation(func(app.MutationRepository) error {
					assertBoardLocks(t, dir, true)
					if outcome == "panic" {
						panic(failure)
					}
					if outcome == "error" {
						return failure
					}
					return nil
				})
			}
			switch outcome {
			case "panic":
				require.Panics(t, func() { _ = run() })
			case "error":
				require.ErrorIs(t, run(), failure)
			default:
				require.NoError(t, run())
			}
			assertBoardLocks(t, dir, false)
			require.NoError(t, repo.WithMutation(func(app.MutationRepository) error { return nil }))
		})
	}
}

func TestDiskRepoWithMutationRejectsInvalidScopedSave(t *testing.T) {
	for _, name := range []string{"incoming ownership", "current ownership", "null current ownership", "board control", "outside destination", "conflict destination", "unsafe id", "current id mismatch", "malformed current", "missing current", "nil ticket"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Test\n")
			repo := NewDiskRepo(dir)
			st, err := repo.Get("tst-001")
			require.NoError(t, err)
			require.NoError(t, st.Ticket.SetField("status", "done"))
			err = repo.WithMutation(func(scoped app.MutationRepository) error {
				managed := false
				switch name {
				case "incoming ownership":
					require.NoError(t, st.Ticket.SetField("managed", map[string]any{}))
					managed = true
				case "current ownership", "null current ownership":
					marker := "{}"
					if name == "null current ownership" {
						marker = "null"
					}
					require.NoError(t, os.WriteFile(path, []byte("---\nid: tst-001\nmanaged: "+marker+"\n---\n# Managed\n"), 0o644))
					managed = true
				case "board control":
					require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora", "authority.json"), []byte("{}"), 0o600))
					managed = true
				case "outside destination":
					st.FilePath = filepath.Join(t.TempDir(), "tst-001.md")
				case "conflict destination":
					st.FilePath = filepath.Join(dir, "tst-001 2.md")
				case "unsafe id":
					st.Ticket.ID = "../escape"
				case "current id mismatch":
					require.NoError(t, os.WriteFile(path, []byte("---\nid: another\n---\n# Wrong\n"), 0o644))
				case "malformed current":
					require.NoError(t, os.WriteFile(path, []byte("---\ninvalid: [\n---\n"), 0o644))
				case "missing current":
					require.NoError(t, os.Remove(path))
				case "nil ticket":
					st.Ticket = nil
				}
				before, readErr := os.ReadFile(path)
				saveErr := scoped.Save(st)
				require.Error(t, saveErr)
				if managed {
					require.ErrorIs(t, saveErr, ticket.ErrManaged)
				}
				after, afterErr := os.ReadFile(path)
				if readErr != nil {
					require.ErrorIs(t, afterErr, os.ErrNotExist)
				} else {
					require.NoError(t, afterErr)
					require.Equal(t, before, after)
				}
				if name == "outside destination" || name == "conflict destination" {
					_, err := os.Stat(st.FilePath)
					require.ErrorIs(t, err, os.ErrNotExist)
				}
				return saveErr
			})
			require.Error(t, err)
			assertBoardLocks(t, dir, false)
		})
	}
}

func TestDiskRepoWithMutationAdmissionFailureReleasesLocks(t *testing.T) {
	for _, marker := range []string{"managed ticket", "board control"} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			if marker == "managed ticket" {
				writeTestTicket(t, dir, "other", "---\nid: other\nmanaged: {}\n---\n# Managed\n")
			} else {
				require.NoError(t, os.Mkdir(filepath.Join(dir, ".kontora"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora", "authority.json"), []byte("{}"), 0o600))
			}
			err := NewDiskRepo(dir).WithMutation(func(app.MutationRepository) error {
				t.Fatal("callback ran despite managed board")
				return nil
			})
			require.ErrorIs(t, err, ticket.ErrManaged)
			assertBoardLocks(t, dir, false)
		})
	}
}

func TestDiskRepoWithMutationDoesNotPromiseRollback(t *testing.T) {
	dir := t.TempDir()
	writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Test\n")
	repo := NewDiskRepo(dir)
	failure := errors.New("later operation failed")
	err := repo.WithMutation(func(scoped app.MutationRepository) error {
		st, err := scoped.Get("tst-001")
		require.NoError(t, err)
		require.NoError(t, st.Ticket.SetField("status", "done"))
		require.NoError(t, scoped.Save(st))
		return failure
	})
	require.ErrorIs(t, err, failure)
	st, err := repo.Get("tst-001")
	require.NoError(t, err)
	require.Equal(t, ticket.StatusDone, st.Ticket.Status)
	assertBoardLocks(t, dir, false)
}

func TestDaemonRepoWithMutationRequiresCallback(t *testing.T) {
	repo := NewDaemonRepo(DaemonRepoCallbacks{
		PathLookup:  func(string) (string, error) { t.Fatal("unsafe fallback lookup"); return "", nil },
		WriteTicket: func(*ticket.Ticket, string) error { t.Fatal("unsafe fallback write"); return nil },
	})
	err := repo.WithMutation(func(app.MutationRepository) error {
		t.Fatal("callback ran without daemon coordination")
		return nil
	})
	require.ErrorContains(t, err, "transaction callback is not configured")
}

func TestDaemonRepoWithMutationAndSaveDelegateToScope(t *testing.T) {
	dir := t.TempDir()
	writeTestTicket(t, dir, "tst-001", "---\nid: tst-001\nstatus: open\n---\n# Fresh\n")
	disk := NewDiskRepo(dir)
	calls := 0
	repo := NewDaemonRepo(DaemonRepoCallbacks{
		WithMutation: func(fn func(app.MutationRepository) error) error {
			calls++
			return disk.WithMutation(fn)
		},
		PathLookup:  func(string) (string, error) { t.Fatal("outer lookup used"); return "", nil },
		WriteTicket: func(*ticket.Ticket, string) error { t.Fatal("legacy write used"); return nil },
		AfterSave:   func(string, *app.StoredTicket) { t.Fatal("legacy cache update used") },
		ListTickets: func() []*app.StoredTicket {
			return []*app.StoredTicket{{Ticket: &ticket.Ticket{ID: "stale"}}}
		},
	})
	var st *app.StoredTicket
	require.NoError(t, repo.WithMutation(func(scoped app.MutationRepository) error {
		id, err := scoped.Resolve("tst")
		require.NoError(t, err)
		st, err = scoped.Get(id)
		require.NoError(t, err)
		require.Equal(t, ticket.StatusOpen, st.Ticket.Status)
		listed, err := scoped.List()
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.Equal(t, "tst-001", listed[0].Ticket.ID)
		return nil
	}))
	require.NoError(t, st.Ticket.SetField("status", "done"))
	require.NoError(t, repo.Save(st))
	require.Equal(t, 2, calls)
	fresh, err := disk.Get("tst-001")
	require.NoError(t, err)
	require.Equal(t, ticket.StatusDone, fresh.Ticket.Status)
}

func TestDaemonRepoTransactionFailureHasNoLegacyFallback(t *testing.T) {
	failure := errors.New("admission refused")
	repo := NewDaemonRepo(DaemonRepoCallbacks{
		WithMutation: func(func(app.MutationRepository) error) error { return failure },
		PathLookup:   func(string) (string, error) { t.Fatal("unsafe fallback lookup"); return "", nil },
	})
	require.ErrorIs(t, repo.WithMutation(func(app.MutationRepository) error {
		t.Fatal("callback ran despite admission refusal")
		return nil
	}), failure)
	require.ErrorIs(t, repo.Save(&app.StoredTicket{Ticket: &ticket.Ticket{ID: "test"}}), failure)

	repo = NewDaemonRepo(DaemonRepoCallbacks{
		WithMutation: func(fn func(app.MutationRepository) error) error { return fn(nil) },
	})
	require.ErrorIs(t, repo.WithMutation(func(app.MutationRepository) error { return failure }), failure)
}
