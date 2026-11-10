package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func automationFixture(t *testing.T) (*testHarness, *Daemon, automation.Definition, time.Time) {
	t.Helper()
	h := newHarness(t)
	h.cfg.AutoPickUp = new(false)
	h.cfg.DefaultPipeline = "one-stage"
	d := h.newDaemon(h.cfg)
	stop := d.startAutomations(h.tasksDir)
	t.Cleanup(stop)
	now := time.Date(2026, 8, 10, 9, 1, 0, 0, time.UTC)
	def, err := d.automationStore.Create("fixture", automation.Definition{Name: "Recurring task", Path: h.repoDir, Agent: "agent1", Instructions: "Do the task", Schedule: automation.Schedule{Frequency: "daily", Time: "09:00", Timezone: "UTC"}, SkipOverlap: true}, now.Add(-2*time.Minute))
	require.NoError(t, err)
	def.Enabled = true
	def, err = d.automationStore.Update(def, now.Add(-2*time.Minute))
	require.NoError(t, err)
	return h, d, def, now
}

func fixtureOccurrence(t *testing.T, d *Daemon, def automation.Definition, phase string) automation.Occurrence {
	t.Helper()
	id, err := cli.GenerateID(d.config(), def.Path)
	require.NoError(t, err)
	o, err := d.automationStore.SaveOccurrence(automation.Occurrence{ID: automationOccurrenceID(def, def.Next), AutomationID: def.ID, ScheduleRevision: def.ScheduleRevision, ScheduledFor: def.Next, TicketID: id, Payload: def, Phase: phase, Count: 1})
	require.NoError(t, err)
	return o
}

func publishFixtureOccurrence(t *testing.T, d *Daemon, o automation.Occurrence, status string) string {
	t.Helper()
	_, err := cli.New(d.config(), cli.NewOpts{ID: o.TicketID, Title: o.Payload.Name, Path: o.Payload.Path, Agent: o.Payload.Agent, Pipeline: config.NoneSentinel, Body: o.Payload.Instructions, Status: "open", NoEdit: true, AutomationID: o.AutomationID, OccurrenceID: o.ID, ScheduledFor: o.ScheduledFor.Format(time.RFC3339)})
	require.NoError(t, err)
	path := d.automationTicketPath(o)
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	require.NoError(t, tk.SetField("status", status))
	data, err := tk.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

func TestAutomationPublicationRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, phase, published, want string
		remove                       bool
		conflict                     bool
		paused                       bool
		rollback                     bool
	}{
		{name: "reserved publishes", phase: "reserved", want: "admitted"},
		{name: "publishing adopts todo", phase: "publishing", published: "todo", want: "admitted"},
		{name: "publishing does not reopen paused", phase: "publishing", published: "paused", want: "admitted"},
		{name: "publishing does not reopen archived", phase: "publishing", published: "archived", want: "admitted"},
		{name: "publishing does not reopen done", phase: "publishing", published: "done", want: "admitted"},
		{name: "reserved waits after clock rollback", phase: "reserved", rollback: true, want: "reserved"},
		{name: "publishing missing is ambiguous", phase: "publishing", want: "blocked"},
		{name: "publication then deletion is ambiguous", phase: "publishing", published: "todo", remove: true, want: "blocked"},
		{name: "different provenance blocks", phase: "publishing", published: "todo", conflict: true, want: "blocked"},
		{name: "paused reserved skips", phase: "reserved", paused: true, want: "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d, def, now := automationFixture(t)
			o := fixtureOccurrence(t, d, def, tc.phase)
			var before []byte
			if tc.published != "" {
				path := publishFixtureOccurrence(t, d, o, tc.published)
				if tc.conflict {
					tk, err := ticket.ParseFile(path)
					require.NoError(t, err)
					require.NoError(t, tk.DeleteField("occurrence_id"))
					data, err := tk.Marshal()
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(path, data, 0o644))
				}
				before, _ = os.ReadFile(path)
				if tc.remove {
					require.NoError(t, os.Remove(path))
				}
			}
			if tc.rollback {
				now = now.Add(-2 * time.Minute)
			}
			if tc.paused {
				def.Enabled = false
				var err error
				def, err = d.automationStore.Update(def, now)
				require.NoError(t, err)
			}
			require.NoError(t, d.reconcileAutomationsLocked(now))
			records, err := d.automationStore.Occurrences(def.ID)
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, tc.want, records[0].Phase)
			if tc.want == "blocked" {
				require.Equal(t, time.Minute, d.automationWaitLocked(now))
			}
			if before != nil && !tc.remove {
				after, err := os.ReadFile(d.automationTicketPath(o))
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			require.NoError(t, d.reconcileAutomationsLocked(now))
			again, err := d.automationStore.Occurrences(def.ID)
			require.NoError(t, err)
			require.Equal(t, records, again)
		})
	}
}

func TestAutomationDueDisposition(t *testing.T) {
	for _, tc := range []struct {
		name          string
		advance       time.Duration
		overlapStatus string
		invalid       string
		reserved      bool
		recover       bool
		permit        bool
		want          string
		count         int64
	}{
		{name: "due publishes standalone", want: "admitted", count: 1},
		{name: "late skips", advance: 5 * time.Minute, want: "skipped", count: 1},
		{name: "removed agent fails admission", invalid: "agent", want: "failed", count: 1},
		{name: "removed repository fails admission", invalid: "repository", want: "failed", count: 1},
		{name: "outage compresses missed range", advance: 7 * 24 * time.Hour, want: "admitted", count: 7},
		{name: "paused ticket blocks", overlapStatus: "paused", want: "skipped", count: 1},
		{name: "human review blocks", overlapStatus: "human_review", want: "skipped", count: 1},
		{name: "completed does not block", overlapStatus: "done", want: "admitted", count: 1},
		{name: "recovered completed publication does not block", overlapStatus: "done", recover: true, want: "admitted", count: 1},
		{name: "recovered paused publication blocks", overlapStatus: "paused", recover: true, want: "skipped", count: 1},
		{name: "recovered reserved completion blocks", overlapStatus: "done", recover: true, reserved: true, want: "skipped", count: 1},
		{name: "reserved completed blocks", overlapStatus: "done", reserved: true, want: "skipped", count: 1},
		{name: "overlap can be permitted", overlapStatus: "paused", permit: true, want: "admitted", count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d, def, now := automationFixture(t)
			if tc.permit {
				def.SkipOverlap = false
				var err error
				def, err = d.automationStore.Update(def, now)
				require.NoError(t, err)
			}
			if tc.invalid == "agent" {
				delete(d.config().Agents, "agent1")
			} else if tc.invalid == "repository" {
				require.NoError(t, os.RemoveAll(def.Path))
			}
			if tc.overlapStatus != "" {
				o := fixtureOccurrence(t, d, def, "publishing")
				path := publishFixtureOccurrence(t, d, o, "todo")
				tk, err := ticket.ParseFile(path)
				require.NoError(t, err)
				require.NoError(t, tk.SetField("status", tc.overlapStatus))
				data, err := tk.Marshal()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o644))
				if !tc.recover {
					o.Phase = "admitted"
					_, err = d.automationStore.SaveOccurrence(o)
					require.NoError(t, err)
				}
				if tc.reserved {
					d.running[o.TicketID] = func() {}
				}
				if !tc.recover {
					next, err := def.Schedule.Next(def.Next)
					require.NoError(t, err)
					require.NoError(t, d.automationStore.SetNext(def.ID, next))
					def.Next = next
				}
				now = now.AddDate(0, 0, 1)
			}
			require.NoError(t, d.reconcileAutomationsLocked(now.Add(tc.advance)))
			records, err := d.automationStore.Occurrences(def.ID)
			require.NoError(t, err)
			last := records[len(records)-1]
			require.Equal(t, tc.want, last.Phase)
			if tc.recover {
				require.Len(t, records, 2)
				require.Equal(t, "admitted", records[0].Phase)
			}
			if tc.advance > 24*time.Hour {
				require.Equal(t, tc.count, records[0].Count)
				require.False(t, records[0].Through.IsZero())
			}
			if tc.want == "admitted" {
				tk, err := ticket.ParseFile(d.automationTicketPath(last))
				require.NoError(t, err)
				require.Equal(t, "", tk.Pipeline)
				require.Equal(t, "agent1", tk.Agent)
				require.True(t, d.queued[tk.ID])
				require.Empty(t, d.running[tk.ID])
			}
			saved, err := d.automationStore.Get(def.ID)
			require.NoError(t, err)
			require.True(t, saved.Next.After(now.Add(tc.advance)))
		})
	}
}

func TestAutomationGenerateIDFailureIsLocal(t *testing.T) {
	h, d, _, now := automationFixture(t)
	input := automation.Definition{Name: "Healthy task", Path: h.repoDir, Agent: "agent1", Instructions: "Do the task", Schedule: automation.Schedule{Frequency: "daily", Time: "09:00", Timezone: "UTC"}, SkipOverlap: true}
	other, err := d.automationStore.Create("other", input, now.Add(-2*time.Minute))
	require.NoError(t, err)
	other.Enabled = true
	_, err = d.automationStore.Update(other, now.Add(-2*time.Minute))
	require.NoError(t, err)
	defs, err := d.automationStore.List()
	require.NoError(t, err)
	require.Len(t, defs, 2)
	bad, good := defs[0], defs[1]
	bad.Path = filepath.Join(h.repoDir, "项目")
	require.NoError(t, os.Mkdir(bad.Path, 0o700))
	bad, err = d.automationStore.Update(bad, now.Add(-2*time.Minute))
	require.NoError(t, err)
	require.NoError(t, d.validateAutomation(bad))
	_, idErr := cli.GenerateID(d.config(), bad.Path)
	require.ErrorContains(t, idErr, "cannot derive prefix")

	require.NoError(t, d.reconcileAutomationsLocked(now))
	failed, err := d.AutomationOccurrences(bad.ID)
	require.NoError(t, err)
	require.Len(t, failed, 1)
	require.Equal(t, "failed", failed[0].Phase)
	require.Contains(t, failed[0].Reason, idErr.Error())
	require.Empty(t, failed[0].TicketID)
	admitted, err := d.AutomationOccurrences(good.ID)
	require.NoError(t, err)
	require.Len(t, admitted, 1)
	require.Equal(t, "admitted", admitted[0].Phase)
	require.True(t, d.queued[admitted[0].TicketID])
	for _, def := range []automation.Definition{bad, good} {
		saved, err := d.GetAutomation(def.ID)
		require.NoError(t, err)
		require.Equal(t, def.Next.AddDate(0, 0, 1), saved.Next)
	}

	require.NoError(t, d.reconcileAutomationsLocked(now.Add(automationLateness)))
	again, err := d.AutomationOccurrences(bad.ID)
	require.NoError(t, err)
	require.Equal(t, failed, again)
	again, err = d.AutomationOccurrences(good.ID)
	require.NoError(t, err)
	require.Equal(t, admitted, again)
	files, err := filepath.Glob(filepath.Join(h.tasksDir, "*.md"))
	require.NoError(t, err)
	require.Equal(t, []string{d.automationTicketPath(admitted[0])}, files)
}

func TestAutomationReservedCursorRecovery(t *testing.T) {
	for _, tc := range []struct {
		name            string
		toggle          bool
		reconcilePaused bool
		edit            bool
		want            string
	}{
		{name: "unchanged reservation publishes", want: "admitted"},
		{name: "instruction edit preserves frozen reservation", edit: true, want: "admitted"},
		{name: "re-enable before paused reconciliation skips old reservation", toggle: true, want: "skipped"},
		{name: "paused reconciliation skips old reservation", toggle: true, reconcilePaused: true, want: "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d, def, _ := automationFixture(t)
			o := fixtureOccurrence(t, d, def, "reserved")
			require.NoError(t, d.automationStore.Close())
			var err error
			d.automationStore, err = automation.Open(d.automationRoot, h.tasksDir)
			require.NoError(t, err)
			rollback := o.ScheduledFor.Add(-time.Minute)
			require.NoError(t, d.reconcileAutomationsLocked(rollback))
			records, err := d.AutomationOccurrences(def.ID)
			require.NoError(t, err)
			require.Equal(t, []automation.Occurrence{o}, records)

			if tc.toggle {
				def.Enabled = false
				def, err = d.automationStore.Update(def, rollback)
				require.NoError(t, err)
				if tc.reconcilePaused {
					require.NoError(t, d.reconcileAutomationsLocked(rollback))
				}
				def.Enabled = true
			}
			if tc.toggle || tc.edit {
				def.Instructions = "New instructions for future occurrences"
				def, err = d.automationStore.Update(def, rollback)
				require.NoError(t, err)
			}
			require.Equal(t, o.ScheduleRevision, def.ScheduleRevision)
			if tc.toggle {
				require.Equal(t, o.ScheduledFor.AddDate(0, 0, 1), def.Next)
			} else {
				require.Equal(t, o.ScheduledFor, def.Next)
			}
			require.NoError(t, d.reconcileAutomationsLocked(rollback))
			records, err = d.AutomationOccurrences(def.ID)
			require.NoError(t, err)
			require.Len(t, records, 1)
			if tc.toggle {
				require.Equal(t, "skipped", records[0].Phase)
			} else {
				require.Equal(t, "reserved", records[0].Phase)
			}

			now := o.ScheduledFor.Add(time.Minute)
			require.NoError(t, d.reconcileAutomationsLocked(now))
			records, err = d.AutomationOccurrences(def.ID)
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, tc.want, records[0].Phase)
			require.Equal(t, o.Payload, records[0].Payload)
			require.Equal(t, tc.want == "admitted", d.queued[o.TicketID])
			if tc.want == "admitted" {
				tk, err := ticket.ParseFile(d.automationTicketPath(o))
				require.NoError(t, err)
				require.True(t, automationMatches(tk, o))
				require.Contains(t, tk.Body, o.Payload.Instructions)
			} else {
				_, err := os.Stat(d.automationTicketPath(o))
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			saved, err := d.GetAutomation(def.ID)
			require.NoError(t, err)
			require.Equal(t, o.ScheduledFor.AddDate(0, 0, 1), saved.Next)
			require.NoError(t, d.reconcileAutomationsLocked(now))
			again, err := d.AutomationOccurrences(def.ID)
			require.NoError(t, err)
			require.Equal(t, records, again)
		})
	}
}

func TestAutomationReceiptDeletionAndAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		receipt, existing, retained bool
	}{
		{name: "recorded receipt is not recreated", receipt: true},
		{name: "ambiguous publication needs acknowledgement"},
		{name: "acknowledged visible work still blocks overlap", existing: true},
		{name: "acknowledgement retains execution reservation", retained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d, def, now := automationFixture(t)
			if tc.receipt {
				require.NoError(t, d.reconcileAutomationsLocked(now))
				records, err := d.automationStore.Occurrences(def.ID)
				require.NoError(t, err)
				require.NoError(t, os.Remove(d.automationTicketPath(records[0])))
				require.NoError(t, d.reconcileAutomationsLocked(now))
				_, err = os.Stat(d.automationTicketPath(records[0]))
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				o := fixtureOccurrence(t, d, def, "publishing")
				if tc.existing {
					publishFixtureOccurrence(t, d, o, "todo")
					o.Phase, o.Reason = "blocked", "durability uncertain"
					_, err := d.automationStore.SaveOccurrence(o)
					require.NoError(t, err)
				} else {
					require.NoError(t, d.reconcileAutomationsLocked(now))
				}
				if tc.retained {
					d.running[o.TicketID] = func() {}
				}
				records, err := d.AutomationOccurrences(def.ID)
				require.NoError(t, err)
				o = records[0]
				_, err = d.SkipAutomationOccurrence(def.ID, o.ID, o.Revision-1)
				require.ErrorIs(t, err, automation.ErrConflict)
				saved, err := d.SkipAutomationOccurrence(def.ID, o.ID, o.Revision)
				require.NoError(t, err)
				require.Equal(t, "skipped", saved.Phase)
				if tc.existing {
					tk, err := ticket.ParseFile(d.automationTicketPath(o))
					require.NoError(t, err)
					require.Equal(t, ticket.StatusTodo, tk.Status)
				} else {
					_, err = os.Stat(d.automationTicketPath(o))
					require.ErrorIs(t, err, os.ErrNotExist)
				}
				records, err = d.AutomationOccurrences(def.ID)
				require.NoError(t, err)
				require.Equal(t, tc.existing || tc.retained, d.automationOverlap(records))
				if tc.retained {
					require.NotNil(t, d.running[o.TicketID])
				}
				def, err = d.automationStore.Get(def.ID)
				require.NoError(t, err)
				require.True(t, def.Next.After(time.Now()))
			}
		})
	}
}

func TestAutomationReadmission(t *testing.T) {
	for _, tc := range []struct {
		name, status            string
		dependency, reservation bool
		want                    bool
	}{
		{name: "explicit admission without auto pickup", status: "todo", want: true},
		{name: "dependency later releases", status: "todo", dependency: true, want: true},
		{name: "reservation later releases", status: "todo", reservation: true, want: true},
		{name: "paused never reopens", status: "paused"},
		{name: "review never reopens", status: "human_review"},
		{name: "done never reopens", status: "done"},
		{name: "archived never reopens", status: "archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d, def, now := automationFixture(t)
			o := fixtureOccurrence(t, d, def, "publishing")
			path := publishFixtureOccurrence(t, d, o, "todo")
			tk, err := ticket.ParseFile(path)
			require.NoError(t, err)
			require.NoError(t, tk.SetField("status", tc.status))
			if tc.dependency {
				require.NoError(t, tk.SetField("deps", []string{"prerequisite"}))
			}
			data, err := tk.Marshal()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o644))
			if tc.reservation {
				d.executions[o.TicketID] = &executionClaim{}
			}
			require.NoError(t, d.reconcileAutomationsLocked(now))
			if tc.dependency || tc.reservation {
				require.False(t, d.queued[o.TicketID])
			}
			if tc.dependency {
				dep, err := ticket.ParseBytes([]byte("---\nid: prerequisite\nstatus: done\n---\n# Prerequisite\n"))
				require.NoError(t, err)
				d.tickets[dep.ID] = newTicketState(dep, filepath.Join(h.tasksDir, "prerequisite.md"))
			}
			if tc.reservation {
				delete(d.executions, o.TicketID)
			}
			require.NoError(t, d.reconcileAutomationsLocked(now))
			require.Equal(t, tc.want, d.queued[o.TicketID])
		})
	}
}

func TestAutomationLifecycle(t *testing.T) {
	h := newHarness(t)
	h.cfg.AutoPickUp = new(false)
	root := t.TempDir()
	seed := h.newDaemon(h.cfg, WithAutomationRoot(root))
	stop := seed.startAutomations(h.tasksDir)
	now := time.Now().UTC()
	def, err := seed.automationStore.Create("lifecycle", automation.Definition{Name: "Run agent", Path: h.repoDir, Agent: "agent1", Instructions: "Do the task", Schedule: automation.Schedule{Frequency: "daily", Time: now.Format("15:04"), Timezone: "UTC"}, SkipOverlap: true}, now.Add(-24*time.Hour))
	require.NoError(t, err)
	def.Enabled = true
	def, err = seed.automationStore.Update(def, now.Add(-24*time.Hour))
	require.NoError(t, err)
	o := fixtureOccurrence(t, seed, def, "publishing")
	path := publishFixtureOccurrence(t, seed, o, "todo")
	tk, err := ticket.ParseFile(path)
	require.NoError(t, err)
	require.NoError(t, tk.SetField("deps", []string{"prerequisite"}))
	data, err := tk.Marshal()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	stop()
	d := h.newDaemon(h.cfg, WithAutomationRoot(root))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
	require.Eventually(t, func() bool {
		records, e := d.AutomationOccurrences(def.ID)
		return e == nil && len(records) > 0 && records[0].Phase == "admitted"
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, ticket.StatusTodo, h.readTask(o.TicketID+".md").Status)
	h.writeTicket("prerequisite.md", fmt.Sprintf("---\nid: prerequisite\nstatus: done\npath: %s\n---\n# Prerequisite\n", h.repoDir))
	h.waitForStatus(o.TicketID+".md", ticket.StatusDone, 10*time.Second)
	records, err := d.AutomationOccurrences(def.ID)
	require.NoError(t, err)
	require.Equal(t, o.TicketID, records[0].TicketID)
}

func TestAutomationManagementReplay(t *testing.T) {
	for _, change := range []string{"agent removed", "repository removed", "definition edited"} {
		t.Run(change, func(t *testing.T) {
			h, d, _, _ := automationFixture(t)
			input := automation.Definition{Name: "Management retry", Path: h.repoDir, Agent: "agent1", Instructions: "Keep this request", Schedule: automation.Schedule{Frequency: "daily", Time: "09:00", Timezone: "UTC"}, SkipOverlap: true}
			created, err := d.CreateAutomation("management-retry", input)
			require.NoError(t, err)
			switch change {
			case "agent removed":
				delete(h.cfg.Agents, "agent1")
			case "repository removed":
				require.NoError(t, os.RemoveAll(h.repoDir))
			case "definition edited":
				updated := created
				updated.Instructions = "Later instructions"
				_, err := d.UpdateAutomation(updated)
				require.NoError(t, err)
			}
			replayed, err := d.CreateAutomation("management-retry", input)
			require.NoError(t, err)
			require.Equal(t, created, replayed)
			input.Instructions = "Different request"
			_, err = d.CreateAutomation("management-retry", input)
			require.ErrorIs(t, err, automation.ErrConflict)
		})
	}
}

func TestAutomationScopeUnavailable(t *testing.T) {
	h, d, _, _ := automationFixture(t)
	other := h.newDaemon(h.cfg, WithAutomationRoot(d.automationRoot))
	stop := other.startAutomations(h.tasksDir)
	defer stop()
	_, err := other.ListAutomations()
	require.ErrorIs(t, err, automation.ErrUnavailable)
	_, err = other.CreateTicket(web.CreateTicketRequest{Path: h.repoDir, Title: "Ordinary draft", Status: "open"})
	require.NoError(t, err)
}
