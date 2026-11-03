package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	ticketstore "github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/worktree"
)

type managedFixture struct {
	m         *ManagedTickets
	dir       string
	authority controlstore.Authority
	cached    *app.StoredTicket
}

func newManagedFixture(t *testing.T) *managedFixture {
	t.Helper()
	f := &managedFixture{dir: t.TempDir(), authority: controlstore.Authority{Version: 1, BoardID: "board", OwnerID: "owner", StateDir: t.TempDir()}}
	data := []byte("---\nid: tst-001\nkind: epic\nstatus: open\nkontora: true\ncustom: [keep, these] # preserved\n---\n# Ticket\n\nOriginal specification.\n")
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "tst-001.md"), data, 0o644))
	m, err := OpenManagedTickets(f.dir, f.authority, ManagedTicketHooks{Reserve: func(_ string, commit func() error) error { return commit() }, AfterSave: func(st *app.StoredTicket) { f.cached = st }})
	require.NoError(t, err)
	f.m = m
	require.NoError(t, m.Control.Apply(orchestrator.Command{Kind: orchestrator.CreateObjective, Objective: orchestrator.Objective{ID: "objective", Assignment: "test ownership", Criteria: []string{"keep ownership"}, Budget: orchestrator.Budget{MaxAttempts: 10, MaxRuntime: time.Hour}}}))
	t.Cleanup(func() { require.NoError(t, f.m.Close()) })
	return f
}

func (f *managedFixture) enroll(t *testing.T) *app.StoredTicket {
	t.Helper()
	state, err := f.m.Control.State()
	require.NoError(t, err)
	st, err := f.m.Enroll("tst-001", "work", "objective", state.Revision)
	require.NoError(t, err)
	return st
}

func TestManagedTicketConditionalMutations(t *testing.T) {
	f := newManagedFixture(t)
	initial := f.enroll(t)
	repo := ticketstore.NewDaemonRepo(ticketstore.DaemonRepoCallbacks{ManagedMutation: f.m.MutateManaged})
	svc := app.New(app.Static(&config.Config{}), repo, app.NoopRuntime{})
	st, err := svc.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: *initial.Ticket.Managed, Kind: "document", Fields: map[string]any{"summary": "document only"}})
	require.NoError(t, err)
	require.Equal(t, uint64(2), st.Ticket.Managed.DocumentRevision)
	require.Equal(t, uint64(1), st.Ticket.Managed.SpecificationRevision)
	before, err := os.ReadFile(st.FilePath)
	require.NoError(t, err)
	cache := f.cached
	for _, field := range []string{"board", "owner", "objective", "generation", "document", "specification", "attempt"} {
		t.Run(field, func(t *testing.T) {
			expected := *st.Ticket.Managed
			switch field {
			case "board":
				expected.BoardID = "other"
			case "owner":
				expected.OwnerID = "other"
			case "objective":
				expected.ObjectiveID = "other"
			case "generation":
				expected.Generation++
			case "document":
				expected.DocumentRevision--
			case "specification":
				expected.SpecificationRevision++
			case "attempt":
				expected.AttemptID = "superseded"
			}
			_, err := svc.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: expected, Kind: "document", Fields: map[string]any{"summary": "stale"}})
			require.ErrorIs(t, err, orchestrator.ErrStale)
			actual, err := os.ReadFile(st.FilePath)
			require.NoError(t, err)
			require.Equal(t, before, actual)
			require.Same(t, cache, f.cached)
		})
	}
	body := "# Revised\n\nNew specification.\n"
	revised, err := svc.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: *st.Ticket.Managed, Kind: "specification", Body: &body})
	require.NoError(t, err)
	require.Equal(t, uint64(3), revised.Ticket.Managed.DocumentRevision)
	require.Equal(t, uint64(2), revised.Ticket.Managed.SpecificationRevision)
	require.Contains(t, string(mustManagedBytes(t, revised.Ticket)), "# preserved")
	revised.Ticket.Managed.Generation = 99
	require.Equal(t, uint64(1), f.cached.Ticket.Managed.Generation)
}

func mustManagedBytes(t *testing.T, ticket *ticket.Ticket) []byte {
	t.Helper()
	out, err := ticket.Marshal()
	require.NoError(t, err)
	return out
}

func TestManagedAttemptIdentityProjection(t *testing.T) {
	f := newManagedFixture(t)
	st := f.enroll(t)
	state, err := f.m.Control.State()
	require.NoError(t, err)
	require.NoError(t, f.m.Control.Apply(orchestrator.Command{Kind: orchestrator.SetObjectiveStatus, ExpectedRevision: state.Revision, ObjectiveID: "objective", Status: orchestrator.ObjectiveActive}))
	for _, attemptID := range []string{"first", "replacement"} {
		state, err = f.m.Control.State()
		require.NoError(t, err)
		w := state.Work["work"]
		require.NoError(t, f.m.Control.Apply(orchestrator.Command{Kind: orchestrator.AdmitAttempt, ExpectedRevision: state.Revision, Attempt: orchestrator.Attempt{ID: attemptID, WorkID: w.ID, ObjectiveID: w.ObjectiveID, SpecificationRevision: w.SpecificationRevision, Inputs: w.Inputs, Recipe: orchestrator.Recipe{Profile: "worker", ConfigurationRevision: "frozen", Timeout: time.Minute}, Reservation: orchestrator.Reservation{Slots: 1}}, Capacity: 1, At: time.Now()}))
		f.restart(t)
		require.NoError(t, f.m.ReconcileProjections())
		st, err = f.m.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: *st.Ticket.Managed, Kind: "ownership"})
		require.NoError(t, err)
		require.Equal(t, attemptID, st.Ticket.Managed.AttemptID)
		cache := f.cached
		old := *st.Ticket.Managed
		old.AttemptID = "superseded"
		_, err := f.m.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: old, Kind: "attempt", Fields: map[string]any{"summary": "late completion"}})
		require.ErrorIs(t, err, orchestrator.ErrStale)
		require.Same(t, cache, f.cached)
		st, err = f.m.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: *st.Ticket.Managed, Kind: "attempt", Fields: map[string]any{"summary": "exact completion"}})
		require.NoError(t, err)
		state, err = f.m.Control.State()
		require.NoError(t, err)
		require.NoError(t, f.m.Control.Apply(orchestrator.Command{Kind: orchestrator.StopAttempt, ExpectedRevision: state.Revision, WorkID: "work", AttemptID: attemptID, AttemptStatus: orchestrator.AttemptSucceeded, Receipt: "test termination evidence", At: time.Now()}))
		f.restart(t)
		require.NoError(t, f.m.ReconcileProjections())
		st, err = f.m.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: *st.Ticket.Managed, Kind: "ownership"})
		require.NoError(t, err)
		require.Empty(t, st.Ticket.Managed.AttemptID)
	}
}

func TestManagedEnrollmentRaceAndRestartReservation(t *testing.T) {
	f := newManagedFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() { _, err := f.m.Enroll("tst-001", fmt.Sprintf("work-%d", i), "objective", 1); results <- err })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	state, err := f.m.Control.State()
	require.NoError(t, err)
	require.Len(t, state.Work, 1)
	require.NoError(t, f.m.Close())
	f.m, err = OpenManagedTickets(f.dir, f.authority, ManagedTicketHooks{})
	require.NoError(t, err)
	reserved, err := f.m.Reserved("tst-001")
	require.NoError(t, err)
	require.True(t, reserved)
	require.NoError(t, f.m.ReconcileProjections())
}

func TestManagedReservationBeforeProjectionAndOfflineWriters(t *testing.T) {
	f := newManagedFixture(t)
	f.m.hooks.Reserve = func(_ string, commit func() error) error {
		require.NoError(t, commit())
		return fmt.Errorf("crash before projection")
	}
	_, err := f.m.Enroll("tst-001", "work", "objective", 1)
	require.ErrorContains(t, err, "crash before projection")
	original, err := ticket.ParseFile(filepath.Join(f.dir, "tst-001.md"))
	require.NoError(t, err)
	require.Nil(t, original.Managed)
	require.NoError(t, f.m.Close())
	cfg := &config.Config{TicketsDir: f.dir}
	for name, mutate := range map[string]func() error{
		"note":    func() error { return cli.Note(f.dir, "tst-001", "bypass", cli.NoteOptions{}) },
		"summary": func() error { return cli.Summary(f.dir, "tst-001", "bypass") },
		"delete":  func() error { return cli.Delete(f.dir, "tst-001") },
		"editor":  func() error { return cli.Edit(f.dir, "false", "tst-001") },
		"status":  func() error { return cli.SetStatus(cfg, "tst-001", "done") },
		"creation": func() error {
			_, err := cli.New(cfg, cli.NewOpts{ID: "new-001", Path: "/unused", Title: "new", Status: "open", NoEdit: true})
			return err
		},
		"sidecar": func() error { return ticket.SetNoteReaction(f.dir, "tst-001", "note", "ok", "operator", true) },
	} {
		t.Run(name, func(t *testing.T) { require.ErrorIs(t, mutate(), ticket.ErrManaged) })
	}
	f.m, err = OpenManagedTickets(f.dir, f.authority, ManagedTicketHooks{})
	require.NoError(t, err)
	require.NoError(t, f.m.ReconcileProjections())
	projected, err := ticket.ParseFile(original.FilePath)
	require.NoError(t, err)
	require.NotNil(t, projected.Managed)
}

func TestManagedExternalEditAndDeletion(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			f := newManagedFixture(t)
			st := f.enroll(t)
			cache := f.cached
			external := []byte("external edit without frontmatter\n")
			if deleted {
				require.NoError(t, os.Remove(st.FilePath))
			} else {
				require.NoError(t, os.WriteFile(st.FilePath, external, 0o644))
			}
			require.ErrorContains(t, f.m.Observe("tst-001"), "diverged")
			require.Error(t, f.m.ReconcileProjections())
			require.Same(t, cache, f.cached)
			state, err := f.m.Control.State()
			require.NoError(t, err)
			require.NotEmpty(t, state.Work["work"].Divergence)
			if !deleted {
				actual, err := os.ReadFile(st.FilePath)
				require.NoError(t, err)
				require.Equal(t, external, actual)
			}
		})
	}
}

func TestManagedProjectionRecoveryAcknowledgesAppliedBytes(t *testing.T) {
	f := newManagedFixture(t)
	f.m.hooks.Reserve = func(_ string, commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		return fmt.Errorf("crash before projection")
	}
	_, err := f.m.Enroll("tst-001", "work", "objective", 1)
	require.Error(t, err)
	state, err := f.m.Control.State()
	require.NoError(t, err)
	action, ok := f.m.latestProjection(state, "work")
	require.True(t, ok)
	projection, err := f.m.projection(action)
	require.NoError(t, err)
	path := filepath.Join(f.dir, "tst-001.md")
	require.NoError(t, writeManagedProjection(path, projection.After))
	before, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, f.m.Close())
	f.m, err = OpenManagedTickets(f.dir, f.authority, ManagedTicketHooks{})
	require.NoError(t, err)
	require.NoError(t, f.m.ReconcileProjections())
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "recovery must acknowledge an already applied projection")
	state, err = f.m.Control.State()
	require.NoError(t, err)
	require.Equal(t, orchestrator.ActionCompleted, state.Actions[action.ID].Status)
}

func (f *managedFixture) restart(t *testing.T) {
	t.Helper()
	hooks := f.m.hooks
	require.NoError(t, f.m.Close())
	var err error
	f.m, err = OpenManagedTickets(f.dir, f.authority, hooks)
	require.NoError(t, err)
}

func TestManagedEnrollmentRequiresQuiescence(t *testing.T) {
	f := newManagedFixture(t)
	f.m.hooks.Reserve = func(_ string, _ func() error) error { return fmt.Errorf("summary still running") }
	_, err := f.m.Enroll("tst-001", "work", "objective", 1)
	require.ErrorContains(t, err, "summary still running")
	reserved, err := f.m.Reserved("tst-001")
	require.NoError(t, err)
	require.False(t, reserved)
	require.Nil(t, f.cached)
}

func TestOrdinaryMutationExcludesAuthorityAcquisition(t *testing.T) {
	for _, operation := range []string{"summary", "delete"} {
		t.Run(operation, func(t *testing.T) {
			h := newHarness(t)
			path := h.writeTicket("tst-001.md", "---\nid: tst-001\nstatus: open\n---\n# Ticket\n")
			d := h.newDaemon(h.cfg)
			tk, err := ticket.ParseFile(path)
			require.NoError(t, err)
			d.tickets[tk.ID] = newTicketState(tk, path)
			release, err := ticket.LockOrdinaryRouting(h.tasksDir)
			require.NoError(t, err)
			release()
			probe, err := os.OpenFile(filepath.Join(h.tasksDir, ".kontora", "board.lock"), os.O_RDWR, 0o600)
			require.NoError(t, err)
			defer probe.Close()
			d.mu.Lock()
			locked := true
			finished := false
			done := make(chan error, 1)
			go func() {
				if operation == "summary" {
					done <- d.SetSummary(tk.ID, "ordinary update")
				} else {
					done <- d.DeleteTicket(tk.ID)
				}
			}()
			t.Cleanup(func() {
				if locked {
					d.mu.Unlock()
				}
				if !finished {
					select {
					case err := <-done:
						t.Logf("mutation cleanup result: %v", err)
					case <-time.After(5 * time.Second):
						t.Error("ordinary mutation did not stop during cleanup")
					}
				}
			})
			require.Eventually(t, func() bool {
				err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				if err == nil {
					_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
				}
				return err == syscall.EWOULDBLOCK
			}, 5*time.Second, time.Millisecond)
			authority := controlstore.Authority{BoardID: "board", OwnerID: "owner", StateDir: t.TempDir()}
			board, err := controlstore.AcquireBoard(h.tasksDir, authority)
			require.ErrorIs(t, err, controlstore.ErrLocked)
			require.Nil(t, board)
			d.mu.Unlock()
			locked = false
			select {
			case err := <-done:
				finished = true
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("ordinary mutation did not finish")
			}
			board, err = controlstore.AcquireBoard(h.tasksDir, authority)
			require.NoError(t, err)
			defer board.Close()
			require.ErrorIs(t, d.SetSummary(tk.ID, "late update"), ticket.ErrManaged)
			require.ErrorIs(t, d.DeleteTicket(tk.ID), ticket.ErrManaged)
			if operation == "summary" {
				current, err := ticket.ParseFile(path)
				require.NoError(t, err)
				require.Equal(t, "ordinary update", current.Summary)
			} else {
				require.NoFileExists(t, path)
			}
		})
	}
}

func TestManagedInvalidProjectionDoesNotCommit(t *testing.T) {
	f := newManagedFixture(t)
	path := filepath.Join(f.dir, "tst-001.md")
	require.NoError(t, os.WriteFile(path, []byte("---\nid: tst-001\nkind: epic\nstatus: open\nsummary: &custom original\ncustom: *custom\n---\n# Ticket\n"), 0o644))
	st := f.enroll(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	state, err := f.m.Control.State()
	require.NoError(t, err)
	cache := f.cached
	_, err = f.m.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: *st.Ticket.Managed, Kind: "document", Fields: map[string]any{"summary": "replacement"}})
	require.ErrorContains(t, err, "invalid ticket projection")
	after, err := f.m.Control.State()
	require.NoError(t, err)
	require.Equal(t, state, after)
	require.Same(t, cache, f.cached)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, actual)
	f.restart(t)
	require.NoError(t, f.m.ReconcileProjections())
	_, err = f.m.MutateManaged(app.ManagedMutation{ID: "tst-001", Expected: *st.Ticket.Managed, Kind: "document", Fields: map[string]any{"final_summary": "valid change"}})
	require.NoError(t, err)
}

func managedTestRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out, err := exec.Command("git", "init", "-q", dir).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return dir
}

func (f *managedFixture) repositoryTicket(t *testing.T, repository, scope string) uint64 {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "tst-001.md"), fmt.Appendf(nil, "---\nid: tst-001\nstatus: open\npath: %q\nbranch: managed-test\n---\n# Ticket\n", repository), 0o644))
	state, err := f.m.Control.State()
	require.NoError(t, err)
	require.NoError(t, f.m.Control.Apply(orchestrator.Command{Kind: orchestrator.CreateObjective, ExpectedRevision: state.Revision, Objective: orchestrator.Objective{ID: "scoped", Assignment: "scoped work", Criteria: []string{"checked"}, Policy: orchestrator.Policy{Repositories: []string{scope}}, Budget: orchestrator.Budget{MaxAttempts: 10, MaxRuntime: time.Hour}}}))
	state, err = f.m.Control.State()
	require.NoError(t, err)
	return state.Revision
}

func TestManagedEnrollmentRepositoryPolicy(t *testing.T) {
	f := newManagedFixture(t)
	repository := managedTestRepository(t)
	revision := f.repositoryTicket(t, repository, managedTestRepository(t))
	before, err := f.m.Control.State()
	require.NoError(t, err)
	_, err = f.m.Enroll("tst-001", "work", "scoped", revision)
	require.ErrorContains(t, err, "outside objective policy")
	after, err := f.m.Control.State()
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoFileExists(t, filepath.Join(repository, ".git", "kontora", "branches.json"))
	tk, err := ticket.ParseFile(filepath.Join(f.dir, "tst-001.md"))
	require.NoError(t, err)
	require.Nil(t, tk.Managed)
	require.Nil(t, f.cached)
}

func TestManagedBranchEnrollmentRecovery(t *testing.T) {
	for _, phase := range []string{"stale revision", "before lease", "after lease", "conflicting lease"} {
		t.Run(phase, func(t *testing.T) {
			f := newManagedFixture(t)
			repository := managedTestRepository(t)
			revision := f.repositoryTicket(t, repository, repository)
			f.m.hooks.Reserve = func(_ string, commit func() error) error {
				if phase == "stale revision" {
					require.NoError(t, f.m.Control.Apply(orchestrator.Command{Kind: orchestrator.SetObjectiveStatus, ExpectedRevision: revision, ObjectiveID: "scoped", Status: orchestrator.ObjectiveActive}))
					return commit()
				}
				if err := commit(); err != nil {
					return err
				}
				if phase == "after lease" {
					state, err := f.m.Control.State()
					require.NoError(t, err)
					action, ok := f.m.latestProjection(state, "work")
					require.True(t, ok)
					p, err := f.m.projection(action)
					require.NoError(t, err)
					require.NotNil(t, p.Branch)
					require.NoError(t, p.Branch.reserve())
				}
				return fmt.Errorf("crash before ticket projection")
			}
			_, err := f.m.Enroll("tst-001", "work", "scoped", revision)
			require.Error(t, err)
			if phase == "stale revision" {
				require.ErrorIs(t, err, orchestrator.ErrStale)
				require.NoFileExists(t, filepath.Join(repository, ".git", "kontora", "branches.json"))
			}
			state, err := f.m.Control.State()
			require.NoError(t, err)
			if phase == "stale revision" {
				require.Empty(t, state.Work)
				require.Empty(t, state.Actions)
			} else {
				require.Contains(t, state.Work, "work")
				action, ok := f.m.latestProjection(state, "work")
				require.True(t, ok)
				require.Equal(t, orchestrator.ActionPending, action.Status)
			}
			if phase == "conflicting lease" {
				require.NoError(t, worktree.ReserveBranch(repository, "managed-test", worktree.BranchOwnership{BoardID: "other", ObjectiveID: "other", Generation: 1}))
			}
			f.m.hooks.Reserve = func(_ string, commit func() error) error { return commit() }
			f.restart(t)
			if phase == "stale revision" {
				state, err = f.m.Control.State()
				require.NoError(t, err)
				_, err = f.m.Enroll("tst-001", "work", "scoped", state.Revision)
				require.NoError(t, err)
			}
			err = f.m.ReconcileProjections()
			if phase == "conflicting lease" {
				require.ErrorIs(t, err, worktree.ErrOwnedBranch)
				require.Nil(t, f.cached)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, f.cached.Ticket.Managed)
			require.ErrorIs(t, worktree.ReserveBranch(repository, "managed-test", worktree.BranchOwnership{BoardID: "ordinary", ObjectiveID: "ordinary", Generation: 1}), worktree.ErrOwnedBranch)
		})
	}
}
