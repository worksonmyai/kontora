package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

type secondaryCoordination struct {
	source, destination             *testHarness
	sourceDaemon, destinationDaemon *Daemon
	sourceID                        string
	sourceStarts                    chan string
	destinationStarts               atomic.Int32
}

func newSecondaryCoordination(t *testing.T) *secondaryCoordination {
	t.Helper()
	p := &secondaryCoordination{source: newHarness(t), destination: newHarness(t), sourceStarts: make(chan string, 32)}
	for name, h := range map[string]*testHarness{"source": p.source, "destination": p.destination} {
		h.tmuxSession += "-" + name
		h.cfg.TmuxSession = h.tmuxSession
		h.cfg.InstanceName = "coordination-" + name
		h.writeTicket("coord-ready.md", h.taskMD("coord-ready", "open", "one-stage"))
	}
	p.destination.cfg.SecondaryTicketsDirs = []string{p.source.tasksDir}
	p.sourceDaemon = p.source.newDaemon(p.source.cfg, WithRunner(func(ctx context.Context, params RunnerParams) (process.Result, error) {
		p.sourceStarts <- params.TicketID
		if params.OnReady != nil {
			params.OnReady()
		}
		<-ctx.Done()
		return process.Result{}, ctx.Err()
	}))
	p.destinationDaemon = p.destination.newDaemon(p.destination.cfg, WithRunner(func(ctx context.Context, _ RunnerParams) (process.Result, error) {
		p.destinationStarts.Add(1)
		<-ctx.Done()
		return process.Result{}, ctx.Err()
	}))
	return p
}

func runSecondaryCoordinationDaemon(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("cooperating daemon did not stop")
		}
	})
}

func (p *secondaryCoordination) start(t *testing.T) {
	t.Helper()
	runSecondaryCoordinationDaemon(t, p.sourceDaemon)
	runSecondaryCoordinationDaemon(t, p.destinationDaemon)
	require.Eventually(t, func() bool {
		_, sourceErr := p.sourceDaemon.GetTicket("coord-ready")
		_, destinationErr := p.destinationDaemon.GetTicket("coord-ready")
		sources := p.destinationDaemon.ListTicketSources()
		return sourceErr == nil && destinationErr == nil && len(sources) == 1 && sources[0].Available
	}, 5*time.Second, 10*time.Millisecond)
	p.sourceID = p.destinationDaemon.ListTicketSources()[0].ID
}

func (p *secondaryCoordination) note(t *testing.T, id string) string {
	t.Helper()
	require.NoError(t, p.sourceDaemon.AddNote(id, web.AddNoteRequest{Text: "Keep this source note", Author: "source-author"}))
	info, err := p.sourceDaemon.GetTicket(id)
	require.NoError(t, err)
	require.Len(t, info.Notes, 1)
	return info.Notes[0].ID
}

func coordinationBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func TestSecondaryCoordinationClaimWhileSourceRuns(t *testing.T) {
	p := newSecondaryCoordination(t)
	p.source.writeTicket("busy.md", p.source.taskMD("busy", "todo", "one-stage"))
	idlePath := p.source.writeTicket("idle.md", p.source.schedTaskMD("idle", "open", ticket.FormatSchedule(time.Now().Add(24*time.Hour))))
	p.start(t)
	select {
	case id := <-p.sourceStarts:
		require.Equal(t, "busy", id)
	case <-time.After(5 * time.Second):
		t.Fatal("source did not launch its unrelated ticket")
	}
	p.source.waitForStatus("busy.md", ticket.StatusInProgress, 5*time.Second)
	require.Equal(t, 1, p.sourceDaemon.RunningAgents())

	noteID := p.note(t, "idle")
	require.NoError(t, p.sourceDaemon.SetReaction("idle", noteID, "👍", "source-reader", true))
	reactionsPath := filepath.Join(p.source.tasksDir, "idle.notes.json")
	reactions := coordinationBytes(t, reactionsPath)
	info, err := p.destinationDaemon.GetSecondaryTicket(p.sourceID, "idle")
	require.NoError(t, err)
	require.True(t, info.CanClaim, info.RefusalReason)
	claimed, err := p.destinationDaemon.ClaimSecondaryTicket(p.sourceID, "idle", info.Revision)
	require.NoError(t, err)
	assert.Equal(t, "idle", claimed.ID)
	assert.Empty(t, claimed.Source)
	assert.Equal(t, "open", claimed.Status)
	assert.Empty(t, claimed.ScheduledAt)
	require.Len(t, claimed.Notes, 1)
	assert.Equal(t, noteID, claimed.Notes[0].ID)
	assert.Equal(t, "Keep this source note", claimed.Notes[0].Text)
	assert.Equal(t, []web.ReactionInfo{{Emoji: "👍", Actors: []string{"source-reader"}}}, claimed.Notes[0].Reactions)
	assert.NoFileExists(t, idlePath)
	assert.NoFileExists(t, reactionsPath)
	assert.Equal(t, reactions, coordinationBytes(t, filepath.Join(p.destination.tasksDir, "idle.notes.json")))
	assert.Equal(t, ticket.StatusOpen, p.destination.readTask("idle.md").Status)
	require.Eventually(t, func() bool {
		_, err := p.sourceDaemon.GetTicket("idle")
		return errors.Is(err, web.ErrTicketNotFound)
	}, 5*time.Second, 10*time.Millisecond)
	_, err = p.destinationDaemon.GetSecondaryTicket(p.sourceID, "idle")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)
	assert.Equal(t, 1, p.sourceDaemon.RunningAgents())
	assert.Equal(t, ticket.StatusInProgress, p.source.readTask("busy.md").Status)
	assert.Empty(t, p.sourceStarts)
	assert.Zero(t, p.destinationStarts.Load())
	assert.Zero(t, p.destinationDaemon.RunningAgents())

	runningPath := filepath.Join(p.source.tasksDir, "busy.md")
	runningBefore := coordinationBytes(t, runningPath)
	running, err := p.destinationDaemon.GetSecondaryTicket(p.sourceID, "busy")
	require.NoError(t, err)
	assert.False(t, running.CanClaim)
	_, err = p.destinationDaemon.ClaimSecondaryTicket(p.sourceID, "busy", running.Revision)
	require.ErrorIs(t, err, web.ErrInvalidState)
	assert.Equal(t, runningBefore, coordinationBytes(t, runningPath))
	assert.NoFileExists(t, filepath.Join(p.destination.tasksDir, "busy.md"))
	assert.Equal(t, 1, p.sourceDaemon.RunningAgents())
}

func TestSecondaryCoordinationDelayedSourceWrites(t *testing.T) {
	p := newSecondaryCoordination(t)
	path := p.source.writeTicket("delayed.md", p.source.taskMD("delayed", "open", "one-stage"))
	p.start(t)
	stale := p.source.readTask("delayed.md")
	info, err := p.destinationDaemon.GetSecondaryTicket(p.sourceID, "delayed")
	require.NoError(t, err)
	_, err = p.destinationDaemon.ClaimSecondaryTicket(p.sourceID, "delayed", info.Revision)
	require.NoError(t, err)
	destinationPath := filepath.Join(p.destination.tasksDir, "delayed.md")
	before := coordinationBytes(t, destinationPath)

	for _, tc := range []struct {
		name  string
		write func() error
	}{
		{"UpdateTicket", func() error {
			return p.sourceDaemon.UpdateTicket("delayed", web.UpdateTicketRequest{Description: new("# Delayed source save")})
		}},
		{"cached daemon replacement", func() error {
			require.NoError(t, stale.SetField("summary", "delayed cached result"))
			return p.sourceDaemon.writeTicket(stale, path, notify.OriginDaemon)
		}},
		{"secondary editor", func() error {
			_, err := p.destinationDaemon.UpdateSecondaryTicket(p.sourceID, "delayed", web.UpdateTicketRequest{Description: new("# Delayed secondary save")})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, tc.write())
			assert.NoFileExists(t, path)
			assert.NoFileExists(t, filepath.Join(p.source.tasksDir, "delayed.notes.json"))
			assert.Equal(t, before, coordinationBytes(t, destinationPath))
		})
	}
	assert.Empty(t, p.sourceStarts)
	assert.Zero(t, p.destinationStarts.Load())
}

func TestSecondaryCoordinationConcurrentMutations(t *testing.T) {
	for _, tc := range []struct {
		name, route, mutation string
	}{
		{"source save", "source", "save"},
		{"source reaction", "source", "reaction"},
		{"secondary save", "secondary", "save"},
		{"secondary reaction", "secondary", "reaction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newSecondaryCoordination(t)
			const attempts = 8
			for i := range attempts {
				id := fmt.Sprintf("race-%d", i)
				p.source.writeTicket(id+".md", p.source.taskMD(id, "open", "one-stage"))
			}
			p.start(t)
			for i := range attempts {
				id := fmt.Sprintf("race-%d", i)
				noteID := p.note(t, id)
				require.NoError(t, p.sourceDaemon.SetReaction(id, noteID, "👍", "original", true))
				info, err := p.destinationDaemon.GetSecondaryTicket(p.sourceID, id)
				require.NoError(t, err)
				start := make(chan struct{})
				writerDone, claimDone := make(chan error, 1), make(chan error, 1)
				go func() {
					<-start
					if tc.mutation == "save" {
						req := web.UpdateTicketRequest{Description: new("# Concurrent editor save")}
						if tc.route == "source" {
							writerDone <- p.sourceDaemon.UpdateTicket(id, req)
						} else {
							_, err := p.destinationDaemon.UpdateSecondaryTicket(p.sourceID, id, req)
							writerDone <- err
						}
					} else if tc.route == "source" {
						writerDone <- p.sourceDaemon.SetReaction(id, noteID, "👍", "concurrent", true)
					} else {
						_, err := p.destinationDaemon.MutateSecondaryTicket(p.sourceID, id, web.SecondaryMutation{Action: web.SecondarySetReaction, NoteID: noteID, Emoji: "👍", Actor: "concurrent", On: true})
						writerDone <- err
					}
				}()
				go func() {
					<-start
					_, err := p.destinationDaemon.ClaimSecondaryTicket(p.sourceID, id, info.Revision)
					claimDone <- err
				}()
				close(start)
				writerErr := coordinationResult(t, writerDone)
				claimErr := coordinationResult(t, claimDone)
				if claimErr == nil {
					require.Error(t, writerErr, "%s: an old revision cannot include a successful concurrent mutation", id)
				} else {
					require.NoError(t, writerErr, "%s: claim failed: %v", id, claimErr)
					assert.NoFileExists(t, filepath.Join(p.destination.tasksDir, id+".md"))
					fresh, err := p.destinationDaemon.GetSecondaryTicket(p.sourceID, id)
					require.NoError(t, err)
					assert.NotEqual(t, info.Revision, fresh.Revision)
					_, err = p.destinationDaemon.ClaimSecondaryTicket(p.sourceID, id, fresh.Revision)
					require.NoError(t, err)
				}
				assert.NoFileExists(t, filepath.Join(p.source.tasksDir, id+".md"))
				assert.NoFileExists(t, filepath.Join(p.source.tasksDir, id+".notes.json"))
				claimed, err := p.destinationDaemon.GetTicket(id)
				require.NoError(t, err)
				assert.Equal(t, "open", claimed.Status)
				require.Len(t, claimed.Notes, 1)
				assert.Equal(t, noteID, claimed.Notes[0].ID)
				actors := []string{"original"}
				if writerErr == nil && tc.mutation == "reaction" {
					actors = []string{"concurrent", "original"}
				}
				require.Len(t, claimed.Notes[0].Reactions, 1)
				assert.ElementsMatch(t, actors, claimed.Notes[0].Reactions[0].Actors)
				if writerErr == nil && tc.mutation == "save" {
					assert.Equal(t, "Concurrent editor save", claimed.Title)
				} else {
					assert.Equal(t, "Test ticket "+id, claimed.Title)
				}
			}
			assert.Empty(t, p.sourceStarts)
			assert.Zero(t, p.destinationStarts.Load())
		})
	}
}

func coordinationResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("mutation and Claim did not finish")
		return nil
	}
}

func TestSecondaryCoordinationSchedulingAdmission(t *testing.T) {
	for _, holder := range []string{"source scheduler", "source transfer"} {
		t.Run(holder, func(t *testing.T) {
			p := newSecondaryCoordination(t)
			due := time.Now().Add(24 * time.Hour)
			path := p.source.writeTicket("scheduled.md", p.source.schedTaskMD("scheduled", "open", ticket.FormatSchedule(due)))
			p.start(t)
			info, err := p.destinationDaemon.GetSecondaryTicket(p.sourceID, "scheduled")
			require.NoError(t, err)
			before := coordinationBytes(t, path)
			var release func()
			if holder == "source scheduler" {
				release, err = ticket.LockOrdinaryRouting(p.source.tasksDir)
			} else {
				release, err = ticket.LockTransferSource(p.source.tasksDir)
			}
			require.NoError(t, err)
			defer func() {
				if release != nil {
					release()
				}
			}()
			done := make(chan error, 1)
			go func() {
				if holder == "source scheduler" {
					_, err := p.destinationDaemon.ClaimSecondaryTicket(p.sourceID, "scheduled", info.Revision)
					done <- err
				} else {
					p.sourceDaemon.promoteDueSchedules(due.Add(time.Minute))
					done <- nil
				}
			}()
			err = coordinationResult(t, done)
			if holder == "source scheduler" {
				require.ErrorIs(t, err, web.ErrInvalidState)
			} else {
				require.NoError(t, err)
				p.sourceDaemon.mu.Lock()
				_, retried := p.sourceDaemon.scheduleRetry["scheduled"]
				p.sourceDaemon.mu.Unlock()
				assert.True(t, retried, "promotion must retry rather than overwrite an exclusive transfer")
			}
			assert.Equal(t, before, coordinationBytes(t, path))
			assert.NoFileExists(t, filepath.Join(p.destination.tasksDir, "scheduled.md"))
			release()
			release = nil
			_, err = p.destinationDaemon.ClaimSecondaryTicket(p.sourceID, "scheduled", info.Revision)
			require.NoError(t, err)
			assert.NoFileExists(t, path)
			assert.Empty(t, p.destination.readTask("scheduled.md").ScheduledAt)
			assert.Empty(t, p.sourceStarts)
			assert.Zero(t, p.destinationStarts.Load())
		})
	}
}

func TestSecondaryCoordinationControlledSource(t *testing.T) {
	f := newManagedFixture(t)
	path := filepath.Join(f.dir, "tst-001.md")
	controlled, err := ticket.ParseFile(path)
	require.NoError(t, err)
	note, err := controlled.AddNote(ticket.AddNoteOptions{Text: "Controlled source note", Author: "author", At: time.Now()})
	require.NoError(t, err)
	data, err := controlled.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	relatedPath := filepath.Join(f.dir, "related.md")
	require.NoError(t, os.WriteFile(relatedPath, []byte("---\nid: related\nstatus: open\n---\n# Related\n"), 0o644))
	h := newHarness(t)
	h.cfg.SecondaryTicketsDirs = []string{f.dir}
	d := h.newDaemon(h.cfg)
	runSecondaryCoordinationDaemon(t, d)
	require.Eventually(t, func() bool {
		sources := d.ListTicketSources()
		return len(sources) == 1 && sources[0].Available
	}, 5*time.Second, 10*time.Millisecond)
	source := d.ListTicketSources()[0].ID
	info, err := d.GetSecondaryTicket(source, "tst-001")
	require.NoError(t, err)
	assert.False(t, info.CanEdit)
	assert.False(t, info.CanClaim)
	assert.NotEmpty(t, info.RefusalReason)
	before := coordinationBytes(t, path)
	relatedBefore := coordinationBytes(t, relatedPath)
	for _, action := range []string{"edit", "note", "reaction", "relation", "claim"} {
		t.Run(action, func(t *testing.T) {
			var err error
			switch action {
			case "edit":
				_, err = d.UpdateSecondaryTicket(source, "tst-001", web.UpdateTicketRequest{Description: new("# Forbidden")})
			case "note":
				_, err = d.MutateSecondaryTicket(source, "tst-001", web.SecondaryMutation{Action: web.SecondaryAddNote, Text: "Forbidden"})
			case "reaction":
				_, err = d.MutateSecondaryTicket(source, "tst-001", web.SecondaryMutation{Action: web.SecondarySetReaction, NoteID: note.ID, Emoji: "👍", Actor: "reader", On: true})
			case "relation":
				_, err = d.MutateSecondaryTicket(source, "tst-001", web.SecondaryMutation{Action: web.SecondaryLink, RelatedIDs: []string{"related"}})
			case "claim":
				_, err = d.ClaimSecondaryTicket(source, "tst-001", info.Revision)
			}
			require.ErrorContains(t, err, ticket.ErrManaged.Error())
			assert.Equal(t, before, coordinationBytes(t, path))
			assert.Equal(t, relatedBefore, coordinationBytes(t, relatedPath))
			assert.NoFileExists(t, filepath.Join(f.dir, "tst-001.notes.json"))
			assert.NoFileExists(t, filepath.Join(h.tasksDir, "tst-001.md"))
			assert.NoFileExists(t, filepath.Join(h.tasksDir, "tst-001.notes.json"))
		})
	}
}

func TestSecondaryCoordinationRelationNamesakes(t *testing.T) {
	for _, action := range []string{web.SecondaryAddDependency, web.SecondaryLink, web.SecondarySetParent} {
		t.Run(action, func(t *testing.T) {
			p := newSecondaryCoordination(t)
			for name, h := range map[string]*testHarness{"Source": p.source, "Primary": p.destination} {
				h.writeTicket("same.md", "---\nid: same\nstatus: open\n---\n# "+name+" subject\n")
				h.writeTicket("related.md", "---\nid: related\nkind: epic\nstatus: open\n---\n# "+name+" related\n")
			}
			p.destination.writeTicket("primary-only.md", "---\nid: primary-only\nkind: epic\nstatus: open\n---\n# Primary only\n")
			p.start(t)
			primarySubject := coordinationBytes(t, filepath.Join(p.destination.tasksDir, "same.md"))
			primaryRelated := coordinationBytes(t, filepath.Join(p.destination.tasksDir, "related.md"))
			op := web.SecondaryMutation{Action: action, RelatedIDs: []string{"related"}, Parent: "related"}
			_, err := p.destinationDaemon.MutateSecondaryTicket(p.sourceID, "same", op)
			require.NoError(t, err)
			info, err := p.destinationDaemon.GetSecondaryTicket(p.sourceID, "same")
			require.NoError(t, err)
			var refs []web.TicketRef
			switch action {
			case web.SecondaryAddDependency:
				refs = info.Deps
				assert.Equal(t, []string{"related"}, p.source.readTask("same.md").Deps)
			case web.SecondaryLink:
				refs = info.Links
				assert.Equal(t, []string{"related"}, p.source.readTask("same.md").Links)
				assert.Equal(t, []string{"same"}, p.source.readTask("related.md").Links)
			case web.SecondarySetParent:
				require.NotNil(t, info.Parent)
				refs = []web.TicketRef{*info.Parent}
				assert.Equal(t, "related", p.source.readTask("same.md").Parent)
				assert.True(t, info.EpicChild)
			}
			require.Len(t, refs, 1)
			assert.Equal(t, "related", refs[0].ID)
			assert.Equal(t, "Source related", refs[0].Title)
			assert.Equal(t, "open", refs[0].Status)
			assert.Contains(t, info.ClaimWarnings, "related will resolve to the primary folder's ticket")
			sourceSubject := coordinationBytes(t, filepath.Join(p.source.tasksDir, "same.md"))
			sourceRelated := coordinationBytes(t, filepath.Join(p.source.tasksDir, "related.md"))
			_, err = p.destinationDaemon.MutateSecondaryTicket(p.sourceID, "same", web.SecondaryMutation{Action: action, RelatedIDs: []string{"primary-only"}, Parent: "primary-only"})
			require.Error(t, err, "relations must not fall back to primary participants")
			assert.Equal(t, sourceSubject, coordinationBytes(t, filepath.Join(p.source.tasksDir, "same.md")))
			assert.Equal(t, sourceRelated, coordinationBytes(t, filepath.Join(p.source.tasksDir, "related.md")))
			assert.Equal(t, primarySubject, coordinationBytes(t, filepath.Join(p.destination.tasksDir, "same.md")))
			assert.Equal(t, primaryRelated, coordinationBytes(t, filepath.Join(p.destination.tasksDir, "related.md")))
			assert.Empty(t, p.sourceStarts)
			assert.Zero(t, p.destinationStarts.Load())
		})
	}
}
