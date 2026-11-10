package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/web"
)

func secondaryRefreshFixture(t *testing.T, documents map[string]string) (*Daemon, string, string) {
	t.Helper()
	h := newHarness(t)
	dir := t.TempDir()
	h.cfg.SecondaryTicketsDirs = []string{dir}
	for id, document := range documents {
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".md"), []byte(document), 0o644))
	}
	d := h.newDaemon(h.cfg)
	stop, err := d.startSecondary(context.Background())
	require.NoError(t, err)
	stop()
	return d, d.ListTicketSources()[0].ID, dir
}

func obstructSecondaryMutationLock(t *testing.T, dir string) func() {
	t.Helper()
	path := filepath.Join(dir, ".kontora", "mutation.lock")
	saved := filepath.Join(t.TempDir(), "mutation.lock")
	require.NoError(t, os.Rename(path, saved))
	// A directory forces a lock-open error regardless of the test process's permissions.
	require.NoError(t, os.Mkdir(path, 0o755))
	return func() {
		t.Helper()
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Rename(saved, path))
	}
}

func readSecondaryForRecovery(d *Daemon, source, method string) ([]web.TicketInfo, error) {
	if method == "list" {
		return d.ListSecondaryTickets(source)
	}
	info, err := d.GetSecondaryTicket(source, "subject")
	if err != nil {
		return nil, err
	}
	return []web.TicketInfo{info}, nil
}

func TestSecondaryReadsRetryAfterMutationLockRepair(t *testing.T) {
	for _, failedRead := range []string{"list", "detail"} {
		for _, retryRead := range []string{"list", "detail"} {
			t.Run(failedRead+" then "+retryRead, func(t *testing.T) {
				d, source, dir := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Before", "")})
				reviewed, err := d.GetSecondaryTicket(source, "subject")
				require.NoError(t, err)
				identity, err := os.Stat(dir)
				require.NoError(t, err)
				repair := obstructSecondaryMutationLock(t, dir)
				for range 2 {
					infos, err := readSecondaryForRecovery(d, source, failedRead)
					require.ErrorContains(t, err, filepath.Join(".kontora", "mutation.lock"))
					assert.Empty(t, infos)
					assert.False(t, d.ListTicketSources()[0].Available)
				}
				_, err = d.UpdateSecondaryTicket(source, "subject", web.UpdateTicketRequest{Description: new("# Forbidden")})
				require.ErrorIs(t, err, web.ErrInvalidState)
				_, err = d.ClaimSecondaryTicket(source, "subject", reviewed.Revision)
				require.ErrorIs(t, err, web.ErrInvalidState)

				repair()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "subject.md"), []byte(secondaryDocument("subject", "Repaired", "")), 0o644))
				current, err := os.Stat(dir)
				require.NoError(t, err)
				require.True(t, os.SameFile(identity, current))
				infos, err := readSecondaryForRecovery(d, source, retryRead)
				require.NoError(t, err)
				require.Len(t, infos, 1)
				assert.Equal(t, "Repaired", infos[0].Title)
				assert.NotEqual(t, reviewed.Revision, infos[0].Revision)
				assert.True(t, infos[0].CanEdit)
				assert.True(t, infos[0].CanClaim, infos[0].RefusalReason)
				assert.True(t, d.ListTicketSources()[0].Available)
				assert.Empty(t, d.ListTicketSources()[0].Error)
				_, err = d.ClaimSecondaryTicket(source, "subject", reviewed.Revision)
				require.ErrorContains(t, err, "revision changed")
				updated, err := d.UpdateSecondaryTicket(source, "subject", web.UpdateTicketRequest{Description: new("# Edited after repair")})
				require.NoError(t, err)
				assert.Equal(t, "Edited after repair", updated.Title)
				claimed, err := d.ClaimSecondaryTicket(source, "subject", updated.Revision)
				require.NoError(t, err)
				assert.Equal(t, "Edited after repair", claimed.Title)
				assert.Empty(t, claimed.Source)
				assert.NoFileExists(t, filepath.Join(dir, "subject.md"))
			})
		}
	}
}

func TestSecondaryReadRecoveryPreservesSourceAdmission(t *testing.T) {
	for _, method := range []string{"list", "detail"} {
		for _, control := range []string{"board control", "ownership"} {
			t.Run(method+"/"+control, func(t *testing.T) {
				d, source, dir := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Subject", "")})
				reviewed, err := d.GetSecondaryTicket(source, "subject")
				require.NoError(t, err)
				repair := obstructSecondaryMutationLock(t, dir)
				_, err = readSecondaryForRecovery(d, source, method)
				require.Error(t, err)
				repair()
				controlPath := filepath.Join(dir, ".kontora", "board.yaml")
				data := "controlled"
				if control == "ownership" {
					controlPath = filepath.Join(dir, "foreign.md")
					data = secondaryDocument("foreign", "Foreign", "managed: {}\n")
				}
				require.NoError(t, os.WriteFile(controlPath, []byte(data), 0o644))
				infos, err := readSecondaryForRecovery(d, source, method)
				require.NoError(t, err)
				require.NotEmpty(t, infos)
				for _, info := range infos {
					assert.False(t, info.CanEdit)
					assert.False(t, info.CanClaim)
					assert.Contains(t, info.RefusalReason, ticket.ErrManaged.Error())
				}
				assert.True(t, d.ListTicketSources()[0].Available)
				path := filepath.Join(dir, "subject.md")
				before := coordinationBytes(t, path)
				_, err = d.UpdateSecondaryTicket(source, "subject", web.UpdateTicketRequest{Description: new("# Forbidden")})
				assert.ErrorIs(t, err, ticket.ErrManaged)
				_, err = d.MutateSecondaryTicket(source, "subject", web.SecondaryMutation{Action: web.SecondaryAddNote, Text: "Forbidden"})
				assert.ErrorIs(t, err, ticket.ErrManaged)
				_, err = d.ClaimSecondaryTicket(source, "subject", reviewed.Revision)
				assert.ErrorContains(t, err, ticket.ErrManaged.Error())
				assert.Equal(t, before, coordinationBytes(t, path))
				assert.NoFileExists(t, filepath.Join(d.config().TicketsDir, "subject.md"))
				require.NoError(t, os.Remove(controlPath))
				infos, err = readSecondaryForRecovery(d, source, method)
				require.NoError(t, err)
				require.Len(t, infos, 1)
				assert.True(t, infos[0].CanEdit)
				assert.True(t, infos[0].CanClaim, infos[0].RefusalReason)
			})
		}
	}
}

func TestSecondaryReadsDoNotRecreateMissingSource(t *testing.T) {
	for _, method := range []string{"list", "detail"} {
		t.Run(method, func(t *testing.T) {
			d, source, dir := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Subject", "")})
			reviewed, err := d.GetSecondaryTicket(source, "subject")
			require.NoError(t, err)
			require.NoError(t, os.RemoveAll(dir))
			for range 2 {
				infos, err := readSecondaryForRecovery(d, source, method)
				require.ErrorIs(t, err, os.ErrNotExist)
				assert.Empty(t, infos)
				assert.False(t, d.ListTicketSources()[0].Available)
				assert.NoDirExists(t, dir)
			}
			_, err = d.UpdateSecondaryTicket(source, "subject", web.UpdateTicketRequest{Description: new("# Forbidden")})
			assert.ErrorIs(t, err, web.ErrInvalidState)
			_, err = d.MutateSecondaryTicket(source, "subject", web.SecondaryMutation{Action: web.SecondaryAddNote, Text: "Forbidden"})
			assert.ErrorIs(t, err, web.ErrInvalidState)
			_, err = d.ClaimSecondaryTicket(source, "subject", reviewed.Revision)
			assert.ErrorIs(t, err, web.ErrInvalidState)
			assert.NoDirExists(t, dir)
			assert.NoFileExists(t, filepath.Join(d.config().TicketsDir, "subject.md"))
			_, err = readSecondaryForRecovery(d, "unknown-source", method)
			assert.ErrorIs(t, err, web.ErrTicketNotFound)
		})
	}
}

func secondaryDocument(id, title, extra string) string {
	return fmt.Sprintf("---\nid: %s\nstatus: open\n%s---\n# %s\n\nDescription.\n", id, extra, title)
}

func waitSecondaryPending(t *testing.T, o *secondaryObserver) *secondaryGeneration {
	t.Helper()
	var pending *secondaryGeneration
	require.Eventually(t, func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		pending = o.pending
		return pending != nil
	}, 5*time.Second, time.Millisecond)
	return pending
}

func TestSecondaryObservationCoalescesTrailingGenerations(t *testing.T) {
	for _, read := range []string{"list", "detail"} {
		t.Run(read, func(t *testing.T) {
			d, source, dir := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Before", "")})
			o := &d.secondary[source].observer
			observe := o.observe
			started := make(chan int32, 3)
			var calls, active atomic.Int32
			release := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
			once := make([]sync.Once, len(release))
			unblock := func(i int) { once[i].Do(func() { close(release[i]) }) }
			t.Cleanup(func() {
				for i := range release {
					unblock(i)
				}
				o.wait()
			})
			o.observe = func() (map[string]web.TicketInfo, error) {
				assert.Equal(t, int32(1), active.Add(1), "source scans must not overlap")
				defer active.Add(-1)
				n := calls.Add(1)
				infos, err := observe()
				started <- n
				if n <= int32(len(release)) {
					<-release[n-1]
				}
				return infos, err
			}
			first := d.requestSecondaryObservation(source)
			require.Equal(t, int32(1), <-started)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "subject.md"), []byte(secondaryDocument("subject", "Trailing", "")), 0o644))
			type result struct {
				info web.TicketInfo
				err  error
			}
			readDone := make(chan result, 1)
			go func() {
				if read == "list" {
					infos, err := d.ListSecondaryTickets(source)
					if err != nil || len(infos) != 1 {
						readDone <- result{err: fmt.Errorf("list: %v, tickets: %d", err, len(infos))}
						return
					}
					readDone <- result{info: infos[0]}
				} else {
					info, err := d.GetSecondaryTicket(source, "subject")
					readDone <- result{info, err}
				}
			}()
			second := waitSecondaryPending(t, o)
			for range 20 {
				require.Same(t, second, d.requestSecondaryObservation(source), "path notifications join one trailing generation")
			}
			unblock(0)
			require.Equal(t, int32(2), <-started)
			before, err := first.result()
			require.NoError(t, err)
			assert.Equal(t, "Before", before["subject"].Title)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "subject.md"), []byte(secondaryDocument("subject", "Uncovered", "")), 0o644))
			third := d.requestSecondaryObservation(source)
			require.NotSame(t, second, third)
			unblock(1)
			got := <-readDone
			require.NoError(t, got.err)
			assert.Equal(t, "Trailing", got.info.Title)
			if read == "list" {
				assert.Empty(t, got.info.Body)
				assert.Nil(t, got.info.Description)
				assert.Nil(t, got.info.DescriptionTail)
			} else {
				assert.Contains(t, got.info.Body, "Trailing")
				require.NotNil(t, got.info.Description)
			}
			require.Equal(t, int32(3), <-started)
			unblock(2)
			after, err := third.result()
			require.NoError(t, err)
			assert.Equal(t, "Uncovered", after["subject"].Title)
			o.wait()
			assert.Equal(t, int32(3), calls.Load())
		})
	}
}

func TestSecondaryMutationJoinsTrailingObservation(t *testing.T) {
	d, source, _ := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Before", "")})
	o := &d.secondary[source].observer
	observe := o.observe
	var calls atomic.Int32
	captured, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); o.wait() })
	o.observe = func() (map[string]web.TicketInfo, error) {
		n := calls.Add(1)
		infos, err := observe()
		if n == 1 {
			close(captured)
			<-release
		}
		return infos, err
	}
	first := d.requestSecondaryObservation(source)
	<-captured
	type result struct {
		info web.TicketInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		info, err := d.UpdateSecondaryTicket(source, "subject", web.UpdateTicketRequest{Description: new("# Written during observation\n")})
		done <- result{info, err}
	}()
	trailing := waitSecondaryPending(t, o)
	for range 20 {
		require.Same(t, trailing, d.requestSecondaryObservation(source))
	}
	unblock()
	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, "Written during observation", got.info.Title)
	before, err := first.result()
	require.NoError(t, err)
	assert.Equal(t, "Before", before["subject"].Title)
	o.wait()
	assert.Equal(t, int32(2), calls.Load())
}

func TestSecondaryMutationUsesPostWriteGeneration(t *testing.T) {
	for _, mutation := range []string{"update", "note"} {
		t.Run(mutation, func(t *testing.T) {
			d, source, dir := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Before", "")})
			o := &d.secondary[source].observer
			observe := o.observe
			var calls atomic.Int32
			captured := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(func() { unblock(); o.wait() })
			o.observe = func() (map[string]web.TicketInfo, error) {
				calls.Add(1)
				infos, err := observe()
				close(captured)
				<-release
				return infos, err
			}
			type result struct {
				info web.TicketInfo
				err  error
			}
			done := make(chan result, 1)
			go func() {
				var info web.TicketInfo
				var err error
				if mutation == "update" {
					info, err = d.UpdateSecondaryTicket(source, "subject", web.UpdateTicketRequest{Description: new("# Written\n")})
				} else {
					info, err = d.MutateSecondaryTicket(source, "subject", web.SecondaryMutation{Action: web.SecondaryAddNote, Text: "Written note"})
				}
				done <- result{info, err}
			}()
			<-captured
			revision, err := store.Revision(dir, "subject")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "subject.md"), []byte(secondaryDocument("subject", "Later external write", "")), 0o644))
			unblock()
			got := <-done
			require.NoError(t, got.err)
			assert.Equal(t, revision, got.info.Revision)
			assert.NotContains(t, got.info.Body, "Later external write")
			assert.NotNil(t, got.info.Description)
			assert.NotNil(t, got.info.DescriptionTail)
			if mutation == "update" {
				assert.Equal(t, "Written", got.info.Title)
			} else {
				require.Len(t, got.info.Notes, 1)
				assert.Equal(t, "Written note", got.info.Notes[0].Text)
			}
			o.wait()
			assert.Equal(t, int32(1), calls.Load(), "mutation must observe only once after writing")
		})
	}
}

func secondaryEvents(ch <-chan web.TicketEvent) []web.TicketEvent {
	var events []web.TicketEvent
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, event)
		default:
			return events
		}
	}
}

func secondaryUpdateIDs(t *testing.T, events []web.TicketEvent) []string {
	t.Helper()
	ids := make([]string, 0, len(events))
	for _, event := range events {
		require.Equal(t, "secondary_ticket_updated", event.Type)
		info := event.Ticket
		ids = append(ids, info.ID)
		data, err := json.Marshal(info)
		require.NoError(t, err)
		for _, field := range []string{"body", "description", "description_tail"} {
			assert.NotContains(t, string(data), `"`+field+`":`)
		}
		assert.NotEmpty(t, info.Revision)
	}
	return ids
}

func TestSecondaryPublishesChangedDerivedProjections(t *testing.T) {
	d, source, dir := secondaryRefreshFixture(t, map[string]string{
		"subject":   secondaryDocument("subject", "Subject", ""),
		"base":      secondaryDocument("base", "Dependency", ""),
		"epic":      secondaryDocument("epic", "Epic", "kind: epic\n"),
		"link":      secondaryDocument("link", "Linked", ""),
		"unrelated": secondaryDocument("unrelated", "Unrelated", ""),
	})
	ch, unsubscribe := d.broker.Subscribe()
	defer unsubscribe()
	d.refreshSecondary(source)
	require.Empty(t, secondaryEvents(ch), "unchanged watcher echo")

	relations := "deps: [base]\nparent: epic\nlinks: [link]\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "subject.md"), []byte(secondaryDocument("subject", "Subject", relations)), 0o644))
	d.refreshSecondary(source)
	events := secondaryEvents(ch)
	assert.ElementsMatch(t, []string{"subject", "base", "epic", "link"}, secondaryUpdateIDs(t, events))
	for _, event := range events {
		info := event.Ticket
		switch info.ID {
		case "base":
			assert.Equal(t, []web.TicketRef{{ID: "subject", Title: "Subject", Status: "open"}}, info.Blocks)
		case "epic":
			require.Len(t, info.Children, 1)
			assert.Equal(t, "Subject", info.Children[0].Title)
		case "link":
			assert.NotEmpty(t, info.ClaimWarnings)
		}
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "subject.md"), []byte(secondaryDocument("subject", "Renamed", relations)), 0o644))
	d.refreshSecondary(source)
	assert.ElementsMatch(t, []string{"subject", "base", "epic"}, secondaryUpdateIDs(t, secondaryEvents(ch)))
	d.refreshSecondary(source)
	assert.Empty(t, secondaryEvents(ch))

	require.NoError(t, os.Remove(filepath.Join(dir, "subject.md")))
	d.refreshSecondary(source)
	events = secondaryEvents(ch)
	var updates []web.TicketEvent
	var deleted []string
	for _, event := range events {
		if event.Type == "secondary_ticket_deleted" {
			info := event.Ticket
			assert.Equal(t, source, info.Source)
			deleted = append(deleted, info.ID)
		} else {
			updates = append(updates, event)
		}
	}
	assert.Equal(t, []string{"subject"}, deleted)
	assert.ElementsMatch(t, []string{"base", "epic", "link"}, secondaryUpdateIDs(t, updates))
	d.refreshSecondary(source)
	assert.Empty(t, secondaryEvents(ch), "deletion echoes must not republish")
}

func TestSecondaryPublishesReactionChangesWithoutMarkdownEdits(t *testing.T) {
	d, source, dir := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Subject", "")})
	noted, err := d.MutateSecondaryTicket(source, "subject", web.SecondaryMutation{Action: web.SecondaryAddNote, Text: "React here", Author: "reader"})
	require.NoError(t, err)
	require.Len(t, noted.Notes, 1)
	markdown := coordinationBytes(t, filepath.Join(dir, "subject.md"))
	ch, unsubscribe := d.broker.Subscribe()
	defer unsubscribe()
	require.NoError(t, ticket.SetNoteReaction(dir, "subject", noted.Notes[0].ID, "👍", "reader", true))
	d.refreshSecondary(source)
	events := secondaryEvents(ch)
	require.Equal(t, []string{"subject"}, secondaryUpdateIDs(t, events))
	info := events[0].Ticket
	assert.NotEqual(t, noted.Revision, info.Revision)
	require.Len(t, info.Notes, 1)
	assert.Equal(t, []web.ReactionInfo{{Emoji: "👍", Actors: []string{"reader"}}}, info.Notes[0].Reactions)
	assert.Equal(t, markdown, coordinationBytes(t, filepath.Join(dir, "subject.md")))
	d.refreshSecondary(source)
	assert.Empty(t, secondaryEvents(ch))
}

func TestSecondaryPublicationComparesBodyBeforeOmission(t *testing.T) {
	d, source, dir := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Subject", "")})
	ch, unsubscribe := d.broker.Subscribe()
	defer unsubscribe()
	path := filepath.Join(dir, "subject.md")
	stat, err := os.Stat(path)
	require.NoError(t, err)
	before := d.secondary[source].projections["subject"]
	require.NoError(t, os.WriteFile(path, []byte(secondaryDocument("subject", "Subject", "")+"\nOnly description changed.\n"), 0o644))
	require.NoError(t, os.Chtimes(path, stat.ModTime(), stat.ModTime()))
	d.refreshSecondary(source)
	assert.Equal(t, []string{"subject"}, secondaryUpdateIDs(t, secondaryEvents(ch)))
	assert.NotEqual(t, before, d.secondary[source].projections["subject"])
	d.refreshSecondary(source)
	assert.Empty(t, secondaryEvents(ch))
}

func TestSecondaryObservationRevalidatesCapabilitiesAndAdmission(t *testing.T) {
	for _, change := range []string{"reservation", "destination", "control", "ownership"} {
		t.Run(change, func(t *testing.T) {
			d, source, dir := secondaryRefreshFixture(t, map[string]string{"subject": secondaryDocument("subject", "Subject", "")})
			reviewed, err := d.GetSecondaryTicket(source, "subject")
			require.NoError(t, err)
			require.True(t, reviewed.CanEdit)
			require.True(t, reviewed.CanClaim, reviewed.RefusalReason)
			ch, unsubscribe := d.broker.Subscribe()
			defer unsubscribe()
			o := &d.secondary[source].observer
			observe := o.observe
			captured, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(func() { unblock(); o.wait() })
			o.observe = func() (map[string]web.TicketInfo, error) {
				n := calls.Add(1)
				infos, err := observe()
				if n == 1 {
					close(captured)
					<-release
				}
				return infos, err
			}
			d.requestSecondaryObservation(source)
			<-captured
			switch change {
			case "reservation":
				d.mu.Lock()
				d.queued["subject"] = true
				d.mu.Unlock()
			case "destination":
				require.NoError(t, os.WriteFile(filepath.Join(d.config().TicketsDir, "subject.notes.json"), []byte(`{"version":1}`), 0o644))
			case "control":
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".kontora"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora", "board.yaml"), []byte("changed"), 0o644))
			case "ownership":
				require.NoError(t, os.WriteFile(filepath.Join(dir, "foreign.md"), []byte(secondaryDocument("foreign", "Foreign", "managed: {}\n")), 0o644))
			}
			type result struct {
				infos []web.TicketInfo
				err   error
			}
			listedDone := make(chan result, 1)
			go func() {
				infos, err := d.ListSecondaryTickets(source)
				listedDone <- result{infos, err}
			}()
			waitSecondaryPending(t, o)
			unblock()
			listed := <-listedDone
			require.NoError(t, listed.err)
			var current web.TicketInfo
			for _, info := range listed.infos {
				if info.ID == "subject" {
					current = info
				}
			}
			require.Equal(t, "subject", current.ID)
			assert.False(t, current.CanClaim)
			assert.NotEmpty(t, current.RefusalReason)
			ids := secondaryUpdateIDs(t, secondaryEvents(ch))
			assert.Contains(t, ids, "subject", "capability changes without source document edits must publish")
			if change == "control" || change == "ownership" {
				assert.False(t, current.CanEdit)
				_, err := d.UpdateSecondaryTicket(source, "subject", web.UpdateTicketRequest{Description: new("# Forbidden")})
				assert.ErrorIs(t, err, ticket.ErrManaged)
				_, err = d.MutateSecondaryTicket(source, "subject", web.SecondaryMutation{Action: web.SecondaryAddNote, Text: "Forbidden"})
				assert.ErrorIs(t, err, ticket.ErrManaged)
			}
			_, err = d.ClaimSecondaryTicket(source, "subject", reviewed.Revision)
			require.Error(t, err, "Claim must revalidate even a previously eligible reviewed revision")
			assert.FileExists(t, filepath.Join(dir, "subject.md"))
		})
	}
}
