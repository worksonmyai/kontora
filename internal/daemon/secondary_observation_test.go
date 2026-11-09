package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestSecondaryDetailWaitsForCompleteRevision(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.cfg.SecondaryTicketsDirs = []string{dir}
	path := filepath.Join(dir, "review.md")
	require.NoError(t, os.WriteFile(path, []byte("---\nid: review\nstatus: open\n---\n# Before\n"), 0o644))
	d := h.newDaemon(h.cfg)
	stop, err := d.startSecondary(context.Background())
	require.NoError(t, err)
	stop()
	source := d.ListTicketSources()[0].ID

	written := make(chan struct{})
	finish := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- ticket.WithBoardMutation(dir, func() error {
			t, err := ticket.ParseFile(path)
			if err != nil {
				return err
			}
			if err := t.SetDescription("# Reviewed\n"); err != nil {
				return err
			}
			noteID, err := t.AddNote(ticket.AddNoteOptions{Text: "Reviewed note", Author: "reader", At: time.Now()})
			if err != nil {
				return err
			}
			data, err := t.Marshal()
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return err
			}
			close(written)
			<-finish
			return os.WriteFile(filepath.Join(dir, "review.notes.json"), []byte(fmt.Sprintf(`{"version":1,"reactions":{%q:[{"emoji":"ok","actors":["reader"]}]}}`, noteID.ID)), 0o644)
		})
	}()
	t.Cleanup(func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	})
	select {
	case <-written:
	case err := <-writeDone:
		t.Fatalf("writer failed before snapshot: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not acquire mutation lock")
	}
	type result struct {
		info web.TicketInfo
		err  error
	}
	readDone := make(chan result, 1)
	go func() {
		info, err := d.GetSecondaryTicket(source, "review")
		readDone <- result{info, err}
	}()
	select {
	case got := <-readDone:
		t.Fatalf("detail escaped an unfinished cooperating write: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(finish)
	require.NoError(t, <-writeDone)
	var got result
	select {
	case got = <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("detail did not resume after mutation")
	}
	require.NoError(t, got.err)
	assert.Equal(t, "Reviewed", got.info.Title)
	require.Len(t, got.info.Notes, 1)
	assert.Equal(t, []web.ReactionInfo{{Emoji: "ok", Actors: []string{"reader"}}}, got.info.Notes[0].Reactions)
	revision, err := store.Revision(dir, "review")
	require.NoError(t, err)
	assert.Equal(t, revision, got.info.Revision)
	_, err = d.UpdateSecondaryTicket(source, "review", web.UpdateTicketRequest{Description: new("# Unseen\n")})
	require.NoError(t, err)
	_, err = d.ClaimSecondaryTicket(source, "review", got.info.Revision)
	require.ErrorContains(t, err, "revision changed")
	assert.FileExists(t, path)
	assert.NoFileExists(t, filepath.Join(h.tasksDir, "review.md"))
}

func TestSecondarySnapshotHashesObservedBytes(t *testing.T) {
	for _, sidecar := range []bool{false, true} {
		t.Run(fmt.Sprintf("sidecar=%t", sidecar), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "review.md")
			require.NoError(t, os.WriteFile(path, []byte("---\nid: review\nstatus: open\n---\n# Reviewed\n"), 0o644))
			if sidecar {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "review.notes.json"), []byte(`{"version":1}`), 0o644))
			}
			reviewed, err := store.Revision(dir, "review")
			require.NoError(t, err)
			snapshot, err := observeSecondary(dir, func(p string) ([]byte, os.FileInfo, error) {
				data, stat, err := readSecondaryFile(p)
				if p == path && err == nil {
					// Model a scheduled writer that shares routing but not the mutation lock.
					require.NoError(t, os.WriteFile(path, []byte("---\nid: review\nstatus: todo\n---\n# Unseen\n"), 0o644))
				}
				return data, stat, err
			})
			require.NoError(t, err)
			assert.Equal(t, "Reviewed", snapshot.tickets["review"].stored.Ticket.Title())
			assert.Equal(t, reviewed, snapshot.tickets["review"].revision)
			current, err := store.Revision(dir, "review")
			require.NoError(t, err)
			assert.NotEqual(t, current, snapshot.tickets["review"].revision)
		})
	}
}

func TestSecondaryNonregularEntriesDoNotBlockStartup(t *testing.T) {
	for _, entry := range []string{"fifo.md", "safe.notes.json", "symlink.md"} {
		t.Run(entry, func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			h.cfg.SecondaryTicketsDirs = []string{dir}
			h.writeTicket("primary.md", "---\nid: primary\nstatus: open\n---\n# Primary\n")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "safe.md"), []byte("---\nid: safe\nstatus: open\n---\n# Safe\n"), 0o644))
			unsafe := filepath.Join(dir, entry)
			if entry == "symlink.md" {
				require.NoError(t, os.Symlink(filepath.Join(dir, "safe.md"), unsafe))
			} else {
				require.NoError(t, syscall.Mkfifo(unsafe, 0o600))
			}
			d := h.newDaemon(h.cfg)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- d.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				// Release a blocked legacy reader as well, so a failed regression can stop.
				if f, err := os.OpenFile(unsafe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = f.Close()
				}
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Error("daemon did not stop")
				}
			})
			require.Eventually(t, func() bool {
				_, err := d.GetTicket("primary")
				return err == nil && len(d.ListTicketSources()) == 1 && d.ListTicketSources()[0].Available
			}, 5*time.Second, 10*time.Millisecond)
			result := make(chan error, 1)
			go func() {
				listed, err := d.ListSecondaryTickets(d.ListTicketSources()[0].ID)
				if err == nil && (len(listed) != 1 || listed[0].ID != "safe" || listed[0].CanClaim || listed[0].CanEdit) {
					err = fmt.Errorf("unexpected unsafe-source projection: %+v", listed)
				}
				result <- err
			}()
			select {
			case err := <-result:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("secondary list blocked on nonregular entry")
			}
		})
	}
}

func TestSecondaryWatcherReattachesAfterReplacement(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.Mkdir(dir, 0o755))
	h.cfg.SecondaryTicketsDirs = []string{dir}
	write := func(id, title string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), []byte(fmt.Sprintf("---\nid: %s\nstatus: archived\n---\n# %s\n", id, title)), 0o644))
	}
	write("old", "Old")
	d := h.newDaemon(h.cfg)
	stop, err := d.startSecondary(context.Background())
	require.NoError(t, err)
	defer stop()
	source := d.ListTicketSources()[0].ID
	require.NoError(t, os.Rename(dir, dir+"-old"))
	require.NoError(t, os.Mkdir(dir, 0o755))
	write("new", "Replacement")
	cached := func(id, title string) bool {
		d.secondaryMu.Lock()
		defer d.secondaryMu.Unlock()
		catalog := d.secondary[source]
		st := catalog.tickets[id]
		return catalog.source.Available && catalog.tickets["old"] == nil && st != nil && st.ticket.Title() == title
	}
	require.Eventually(t, func() bool { return cached("new", "Replacement") }, 5*time.Second, 10*time.Millisecond)
	write("new", "Watched again")
	require.Eventually(t, func() bool { return cached("new", "Watched again") }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, os.Remove(filepath.Join(dir, "new.md")))
	require.Eventually(t, func() bool {
		d.secondaryMu.Lock()
		defer d.secondaryMu.Unlock()
		return len(d.secondary[source].tickets) == 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestSecondarySnapshotReadCounts(t *testing.T) {
	for _, count := range []int{10, 100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			h := newHarness(t)
			dir := t.TempDir()
			for i := range count {
				data := fmt.Sprintf("---\nid: t-%04d\nstatus: archived\ndeps: [t-%04d]\n---\n# Ticket %d\n", i, (i+1)%count, i)
				require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("t-%04d.md", i)), []byte(data), 0o644))
			}
			reads := make(map[string]int)
			snapshot, err := observeSecondary(dir, func(path string) ([]byte, os.FileInfo, error) {
				reads[path]++
				return readSecondaryFile(path)
			})
			require.NoError(t, err)
			require.Len(t, snapshot.tickets, count)
			require.Len(t, reads, 2*count)
			for path, calls := range reads {
				assert.Equal(t, 1, calls, path)
			}
			d := h.newDaemon(h.cfg)
			cfg := *h.cfg
			cfg.TicketsDir = dir
			// Projection must use the snapshot even if the source paths disappear.
			require.NoError(t, os.Rename(dir, dir+"-captured"))
			t.Cleanup(func() { _ = os.RemoveAll(dir + "-captured") })
			for _, observed := range snapshot.tickets {
				info := d.secondaryTicketInfo(&cfg, "source", observed, snapshot)
				require.NotEmpty(t, info.Revision)
				require.Len(t, info.Deps, 1)
				assert.NotEmpty(t, info.Deps[0].Title)
				require.Len(t, info.Blocks, 1)
			}
		})
	}
}

func TestSecondaryDanglingRelationshipWarning(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	warnings := d.claimRelationshipWarnings(&ticket.Ticket{ID: "claim", Deps: []string{"missing"}}, nil)
	assert.Equal(t, []string{"missing is missing in primary. Claim does not move related tickets."}, warnings)
}
