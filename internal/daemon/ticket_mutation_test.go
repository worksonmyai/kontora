package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/watcher"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestTicketMutationPublicNotes(t *testing.T) {
	for _, summary := range []bool{false, true} {
		t.Run(map[bool]string{false: "notes", true: "notes_and_summary"}[summary], func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			path := h.writeTicket("tst-notes.md", h.taskMD("tst-notes", "open", "one-stage"))
			d.tickets["tst-notes"] = newTicketState(h.readTask("tst-notes.md"), path)
			if runtime.GOOS != "linux" {
				if _, err := exec.LookPath("lsof"); err != nil {
					t.Skip("lock descriptor observation requires /proc or lsof")
				}
			}
			const count = 2
			errs := make(chan error, count)
			require.NoError(t, ticket.WithBoardMutation(h.tasksDir, func() error {
				go func() { errs <- d.AddNote("tst-notes", web.AddNoteRequest{Text: "first note"}) }()
				go func() {
					if summary {
						errs <- d.SetSummary("tst-notes", "current summary")
					} else {
						errs <- d.AddNote("tst-notes", web.AddNoteRequest{Text: "second note"})
					}
				}()
				lockPath, err := filepath.EvalSymlinks(filepath.Join(h.tasksDir, ".kontora", "mutation.lock"))
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					opened, err := mutationLockDescriptors(lockPath)
					if err != nil {
						return err
					}
					if opened == count+1 {
						break
					}
					select {
					case err := <-errs:
						return fmt.Errorf("public mutation returned before acquiring the board lock: %v", err)
					case <-ctx.Done():
						return ctx.Err()
					case <-ticker.C:
					}
				}
				fresh := h.readTask("tst-notes.md")
				if err := fresh.SetField("branch", "intervening-branch"); err != nil {
					return err
				}
				data, err := fresh.Marshal()
				if err != nil {
					return err
				}
				return os.WriteFile(path, data, 0o644)
			}))
			for range count {
				require.NoError(t, <-errs)
			}
			stored := h.readTask("tst-notes.md")
			expectedNotes := count
			if summary {
				expectedNotes--
			}
			assert.Len(t, ticket.ParseNotes(stored.Body), expectedNotes)
			assert.Equal(t, "intervening-branch", stored.Branch)
			if summary {
				assert.Equal(t, "current summary", stored.Summary)
			}
			assert.Equal(t, stored.Body, d.tickets["tst-notes"].ticket.Body)
		})
	}
}

// Open lock descriptors mark callers at the mutation-lock boundary, before their scoped reads.
func mutationLockDescriptors(path string) (int, error) {
	if runtime.GOOS == "linux" {
		fds, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return 0, err
		}
		count := 0
		for _, fd := range fds {
			target, err := os.Readlink("/proc/self/fd/" + fd.Name())
			if err == nil && target == path {
				count++
			}
		}
		return count, nil
	}
	out, err := exec.Command("lsof", "-a", "-p", fmt.Sprint(os.Getpid()), "-Fn").Output()
	if err != nil {
		return 0, err
	}
	count := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "n"+path {
			count++
		}
	}
	return count, nil
}

func TestRunTicketUsesFreshState(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cachedStatus string
		freshStatus  string
		blocked      bool
		wasQueued    bool
	}{
		{name: "new dependency", cachedStatus: "todo", freshStatus: "todo", blocked: true},
		{name: "new dependency removes queued ticket", cachedStatus: "todo", freshStatus: "todo", blocked: true, wasQueued: true},
		{name: "manual run after unseen move", cachedStatus: "open", freshStatus: "todo"},
		{name: "open ticket is saved", cachedStatus: "open", freshStatus: "open"},
	} {
		for _, autoPickUp := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/auto_pick_up=%v", tc.name, autoPickUp), func(t *testing.T) {
				h := newHarness(t)
				h.cfg.AutoPickUp = new(autoPickUp)
				d := h.newDaemon(h.cfg)
				path := h.writeTicket("tst-run.md", h.taskMD("tst-run", tc.cachedStatus, "one-stage"))
				d.tickets["tst-run"] = newTicketState(h.readTask("tst-run.md"), path)
				depPath := h.writeTicket("tst-dep.md", h.taskMD("tst-dep", "open", "one-stage"))
				d.tickets["tst-dep"] = newTicketState(h.readTask("tst-dep.md"), depPath)
				if tc.wasQueued {
					d.enqueue(d.tickets["tst-run"].ticket)
				}
				fresh := h.readTask("tst-run.md")
				require.NoError(t, fresh.SetField("status", tc.freshStatus))
				if tc.blocked {
					require.NoError(t, fresh.SetField("deps", []string{"tst-dep"}))
				}
				data, err := fresh.Marshal()
				require.NoError(t, err)
				h.writeTicket("tst-run.md", string(data))
				events, unsubscribe := d.Subscribe()
				defer unsubscribe()

				require.NoError(t, d.RunTicket("tst-run"))
				assert.Equal(t, !tc.blocked, d.queued["tst-run"])
				assert.Equal(t, ticket.StatusTodo, d.tickets["tst-run"].ticket.Status)
				assert.Equal(t, fresh.Deps, d.tickets["tst-run"].ticket.Deps)
				assert.Equal(t, fresh.Deps, h.readTask("tst-run.md").Deps)
				select {
				case event := <-events:
					require.NotNil(t, event.Ticket)
					assert.Equal(t, "todo", event.Ticket.Status)
				default:
					t.Fatal("run did not publish current ticket state")
				}
			})
		}
	}
}

func TestTicketMutationScope(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	path := h.writeTicket("tst-scope.md", h.taskMD("tst-scope", "open", "one-stage"))
	d.tickets["tst-scope"] = newTicketState(h.readTask("tst-scope.md"), path)
	var saved *app.StoredTicket
	require.NoError(t, d.withTicketMutation(func(repo app.MutationRepository) error {
		st, err := repo.Get("tst-scope")
		require.NoError(t, err)
		st.Ticket.SetBody("changed only in detached value")
		fresh, err := repo.Get("tst-scope")
		require.NoError(t, err)
		assert.NotEqual(t, st.Ticket.Body, fresh.Ticket.Body)
		listed, err := repo.List()
		require.NoError(t, err)
		require.Len(t, listed, 1)
		listed[0].Ticket.SetBody("changed only in listing")
		assert.NotEqual(t, listed[0].Ticket.Body, d.tickets["tst-scope"].ticket.Body)
		require.NoError(t, st.Ticket.SetField("summary", "committed"))
		require.NoError(t, repo.Save(st))
		saved = st
		lock, err := os.OpenFile(h.tasksDir+"/.kontora/mutation.lock", os.O_RDWR, 0)
		require.NoError(t, err)
		defer lock.Close()
		assert.ErrorIs(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
		return nil
	}))
	saved.Ticket.SetBody("changed after commit")
	assert.NotEqual(t, saved.Ticket.Body, d.tickets["tst-scope"].ticket.Body)
	assert.Equal(t, "committed", d.tickets["tst-scope"].ticket.Summary)
	require.NoError(t, d.AddNote("tst-scope", web.AddNoteRequest{Text: "after locks released"}))
}

func TestTicketMutationFailureDoesNotPublish(t *testing.T) {
	for _, failure := range []string{"callback", "persistence", "ownership", "destination", "serialized id"} {
		t.Run(failure, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			path := h.writeTicket("tst-fail.md", h.taskMD("tst-fail", "open", "one-stage"))
			d.tickets["tst-fail"] = newTicketState(h.readTask("tst-fail.md"), path)
			events, unsubscribe := d.Subscribe()
			defer unsubscribe()
			before := d.tickets["tst-fail"]
			err := d.withTicketMutation(func(repo app.MutationRepository) error {
				st, err := repo.Get("tst-fail")
				require.NoError(t, err)
				require.NoError(t, st.Ticket.SetField("summary", "must not publish"))
				switch failure {
				case "callback":
					return errors.New("rejected")
				case "persistence":
					require.NoError(t, os.Chmod(path, 0o444))
					defer func() { require.NoError(t, os.Chmod(path, 0o644)) }()
				case "ownership":
					require.NoError(t, st.Ticket.SetField("managed", nil))
				case "destination":
					st.FilePath = filepath.Join(t.TempDir(), "tst-fail.md")
				case "serialized id":
					require.NoError(t, st.Ticket.SetField("id", "other"))
					st.Ticket.ID = "tst-fail"
				}
				return repo.Save(st)
			})
			require.Error(t, err)
			assert.Same(t, before, d.tickets["tst-fail"])
			assert.Empty(t, h.readTask("tst-fail.md").Summary)
			select {
			case event := <-events:
				t.Fatalf("failed mutation published event: %+v", event)
			default:
			}
			assert.Empty(t, d.selfWrites)
			require.NoError(t, d.SetSummary("tst-fail", "after failure"))
		})
	}
}

func TestPartialRelationMutationPublishesCommittedTickets(t *testing.T) {
	for _, unlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("unlink=%v", unlink), func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			for _, id := range []string{"tst-a", "tst-b"} {
				path := h.writeTicket(id+".md", h.taskMD(id, "open", "one-stage"))
				d.tickets[id] = newTicketState(h.readTask(id+".md"), path)
			}
			if unlink {
				require.NoError(t, d.LinkTickets("tst-a", []string{"tst-b"}))
			}
			events, unsubscribe := d.Subscribe()
			defer unsubscribe()
			drain := func() []string {
				var ids []string
				for {
					select {
					case event := <-events:
						ids = append(ids, event.Ticket.ID)
					default:
						return ids
					}
				}
			}
			call := d.LinkTickets
			if unlink {
				call = d.UnlinkTickets
			}
			path := h.taskPath("tst-b.md")
			require.NoError(t, os.Chmod(path, 0o444))
			defer func() { require.NoError(t, os.Chmod(path, 0o644)) }()
			err := call("tst-a", []string{"tst-b"})
			require.ErrorIs(t, err, os.ErrPermission)
			require.ErrorContains(t, err, "already changed: tst-a")
			assert.Equal(t, []string{"tst-a"}, drain())
			for _, id := range []string{"tst-a", "tst-b"} {
				stored := h.readTask(id + ".md")
				assert.Equal(t, stored.Links, d.tickets[id].ticket.Links)
				if (id == "tst-a") == unlink {
					assert.Empty(t, stored.Links)
				} else {
					assert.Len(t, stored.Links, 1)
				}
			}
			d.handleEvent(watcher.Event{Op: watcher.OpChanged, Path: h.taskPath("tst-a.md")})
			assert.Empty(t, drain(), "self-write suppression must not be needed to publish the committed side")

			require.NoError(t, os.Chmod(path, 0o644))
			require.NoError(t, call("tst-a", []string{"tst-b"}))
			assert.Equal(t, []string{"tst-b"}, drain(), "repair publishes only the missing side")
		})
	}
}

func TestTicketMutationCancelCanMutate(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	path := h.writeTicket("tst-pause.md", h.taskMD("tst-pause", "in_progress", "one-stage"))
	d.tickets["tst-pause"] = newTicketState(h.readTask("tst-pause.md"), path)
	callback := make(chan error, 1)
	d.running["tst-pause"] = context.CancelFunc(func() {
		callback <- d.AddNote("tst-pause", web.AddNoteRequest{Text: "cancel callback"})
	})
	done := make(chan error, 1)
	go func() { done <- d.PauseTicket("tst-pause") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("pause retained mutation locks during cancellation")
	}
	require.NoError(t, <-callback)
	stored := h.readTask("tst-pause.md")
	assert.Equal(t, ticket.StatusPaused, stored.Status)
	assert.Len(t, ticket.ParseNotes(stored.Body), 1)
}
