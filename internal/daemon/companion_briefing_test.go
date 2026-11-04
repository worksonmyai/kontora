package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestCompanionEvidence(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("pi")
	cfg.Assistant.Companion.Enabled = true
	d := h.newDaemon(cfg)
	now := time.Now().UTC()
	task := &ticket.Ticket{ID: "kon-test", Status: ticket.StatusPaused, Created: &now, Body: "# Task\n" + strings.Repeat("details ", 2000), Summary: "recorded summary"}
	d.tickets[task.ID] = &ticketState{ticket: task}
	stop := d.startCompanion()
	defer stop()
	packet, err := d.collectCompanionEvidence(web.CompanionMessageRequest{Kind: "investigation", Text: "Explain", TicketIDs: []string{task.ID}})
	require.NoError(t, err)
	require.NotZero(t, packet.Metadata.CapturedAt)
	require.NotEmpty(t, packet.Metadata.Sources[task.ID])
	require.NotEmpty(t, packet.Metadata.Omissions)
	require.Len(t, packet.Tickets, 1)
	data, err := json.Marshal(packet)
	require.NoError(t, err)
	require.LessOrEqual(t, len(data), companionEvidenceMax)
	thread, err := d.createCompanionThread()
	require.NoError(t, err)
	require.NoError(t, saveCompanionEvidence(d.assistantThreadStore(cfg, thread.ID), thread.ID, packet))
	meta := d.companionEvidenceInfo(thread.ID)
	require.Len(t, meta, 1)
	require.False(t, meta[0].Outdated)
	snapshot := d.CompanionSnapshot()
	_, err = d.ReadCompanionItem(snapshot.Items[0].ID, snapshot.Items[0].Revision, true)
	require.NoError(t, err)
	require.False(t, d.companionEvidenceInfo(thread.ID)[0].Outdated, "read state is not a source change")
	d.mu.Lock()
	task.Summary = "new summary"
	d.mu.Unlock()
	meta = d.companionEvidenceInfo(thread.ID)
	require.True(t, meta[0].Outdated)
	changed, err := d.collectCompanionEvidence(web.CompanionMessageRequest{Kind: "investigation", Text: "Explain", TicketIDs: []string{task.ID}})
	require.NoError(t, err)
	require.Empty(t, changed.Attention, "stale active attention is omitted from current evidence")
	require.Contains(t, strings.Join(changed.Metadata.Omissions, " "), "changed since observation")
	dir, err := d.assistantThreadStore(cfg, thread.ID).Dir(thread.ID)
	require.NoError(t, err)
	saved, err := os.ReadFile(filepath.Join(dir, "evidence-"+packet.Metadata.ID+".json"))
	require.NoError(t, err)
	require.JSONEq(t, string(data), string(saved))
}

func TestCompanionRequestedTurn(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("pi")
	cfg.Assistant.Companion.Enabled = true
	spawned := make(chan string, 1)
	var d *Daemon
	d = h.newDaemon(cfg, WithTurnSpawner(func(_ context.Context, p TurnParams) (string, error) {
		files, _ := filepath.Glob(filepath.Join(d.assistantThreadStore(cfg, p.ThreadID).Root(), p.ThreadID, "evidence-*.json"))
		if len(files) == 0 {
			return "", fmt.Errorf("evidence not saved before spawn")
		}
		var packet companionEvidence
		data, err := os.ReadFile(files[0])
		if err != nil {
			return "", err
		}
		if err = json.Unmarshal(data, &packet); err != nil {
			return "", err
		}
		spawned <- p.ThreadID
		if slices.Contains(p.Args, "Fail this turn") {
			return "", fmt.Errorf("injected model failure")
		}
		return "", nil
	}))
	stop := d.startCompanion()
	defer stop()
	info, err := d.PostCompanionMessage(web.CompanionMessageRequest{Kind: "briefing", Text: "What needs me?"})
	require.NoError(t, err)
	require.Equal(t, "companion", info.Purpose)
	select {
	case id := <-spawned:
		require.Equal(t, info.ID, id)
	case <-time.After(5 * time.Second):
		t.Fatal("no requested turn")
	}
	waitIdle(t, d, info.ID)
	require.Len(t, d.companionEvidenceInfo(info.ID), 1)
	_, err = d.PostCompanionMessage(web.CompanionMessageRequest{ThreadID: info.ID, Kind: "followup", Text: "Explain more"})
	require.NoError(t, err)
	<-spawned
	waitIdle(t, d, info.ID)
	require.Len(t, d.companionEvidenceInfo(info.ID), 2)
	_, err = d.PostCompanionMessage(web.CompanionMessageRequest{ThreadID: info.ID, Kind: "followup", Text: "Fail this turn"})
	require.NoError(t, err)
	<-spawned
	waitIdle(t, d, info.ID)
	reopened, err := d.GetAssistantThread(info.ID)
	require.NoError(t, err)
	require.Len(t, reopened.Evidence, 3)
	activity, err := d.AssistantActivity(web.AssistantActivityQuery{ID: info.ID})
	require.NoError(t, err)
	require.Len(t, activity.Evidence, 3)
	history, err := d.ListAssistantThreads()
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Empty(t, history[0].Evidence, "history summaries do not consume evidence")
	require.Len(t, reopened.Messages, 3)
	require.Contains(t, reopened.Messages[2].Error, "injected model failure")
	stored, err := d.assistantThreadStore(cfg, info.ID).Load(info.ID)
	require.NoError(t, err)
	require.Equal(t, assistant.PurposeCompanion, stored.Purpose)
	err = d.PostAssistantMessage(info.ID, web.AssistantMessageRequest{Text: "bypass", Context: "client evidence"})
	require.ErrorIs(t, err, web.ErrAssistantGateDenied)
	d.assistant.stopAll()
	d.background.Wait()
}

func TestCompanionPlaintextEvidenceOutdated(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		changed             bool
	}{
		{name: "unchanged", before: "original transcript", after: "original transcript"},
		{name: "rewritten", before: "original transcript", after: "modified transcript", changed: true},
		{name: "changed beyond clipping", before: strings.Repeat("x", 7000) + "a", after: strings.Repeat("x", 7000) + "b", changed: true},
		{name: "removed", before: "original transcript", changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.assistantConfig("pi")
			cfg.Assistant.Companion.Enabled = true
			d := h.newDaemon(cfg)
			task := &ticket.Ticket{ID: "kon-plain", Status: ticket.StatusDone, Stage: "work"}
			h.writeTicket(task.ID+".md", h.taskMD(task.ID, "done", ""))
			d.tickets[task.ID] = &ticketState{ticket: task}
			path := stageLogPath(cfg, task.ID, task.Stage)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(tc.before), 0o600))
			stat, err := os.Stat(path)
			require.NoError(t, err)
			stop := d.startCompanion()
			defer stop()
			packet, err := d.collectCompanionEvidence(web.CompanionMessageRequest{Kind: "investigation", TicketIDs: []string{task.ID}})
			require.NoError(t, err)
			require.NotEmpty(t, packet.Tickets[0].Activity)
			require.NotEmpty(t, packet.Activities[task.ID].Revision)
			thread, err := d.createCompanionThread()
			require.NoError(t, err)
			store := d.assistantThreadStore(cfg, thread.ID)
			require.NoError(t, saveCompanionEvidence(store, thread.ID, packet))
			require.False(t, d.companionEvidenceInfo(thread.ID)[0].Outdated)
			if tc.after == "" {
				require.NoError(t, os.Remove(path))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(tc.after), 0o600))
				require.NoError(t, os.Chtimes(path, stat.ModTime(), stat.ModTime()))
			}
			require.Equal(t, tc.changed, d.companionEvidenceInfo(thread.ID)[0].Outdated)
			// Exercise the collection boundary after changing only the log, before
			// an evidence packet would be persisted and sent to the model.
			require.NoError(t, d.revalidateCompanionEvidence(&packet))
			if tc.changed {
				require.Empty(t, packet.Tickets[0].Activity)
				require.Empty(t, packet.Activities)
				require.Contains(t, strings.Join(packet.Metadata.Omissions, " "), "activity changed during capture")
			} else {
				require.NotEmpty(t, packet.Tickets[0].Activity)
				require.Len(t, packet.Activities, 1)
			}
		})
	}
}

func TestCompanionPipelineLessLiveEvidence(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("pi")
	d := h.newDaemon(cfg)
	task := &ticket.Ticket{ID: "kon-live", Status: ticket.StatusInProgress}
	h.writeTicket(task.ID+".md", h.taskMD(task.ID, "in_progress", ""))
	d.tickets[task.ID] = &ticketState{ticket: task}
	sessionDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "s.jsonl"), []byte(piSessionJSONL), 0o600))
	d.setLiveRun(task.ID, liveRun{stage: "default", run: 0, params: RunnerParams{SessionDir: sessionDir}})
	live, err := d.GetActivity(web.ActivityQuery{ID: task.ID, Stage: "default", Run: 0})
	require.NoError(t, err)
	require.NotNil(t, live.Tape)
	packet, err := d.collectCompanionEvidence(web.CompanionMessageRequest{Kind: "investigation", TicketIDs: []string{task.ID}})
	require.NoError(t, err)
	require.Contains(t, packet.Tickets[0].Activity, "Two tickets are running.")
	require.Equal(t, web.ActivityQuery{ID: task.ID, Stage: "default", Run: 0}, packet.Activities[task.ID].Query)
}

func TestCompanionEvidenceLimitIncludesNotice(t *testing.T) {
	for _, field := range []string{"details", "activity"} {
		t.Run(field, func(t *testing.T) {
			p := companionEvidence{Tickets: make([]companionEvidenceTicket, 5), Activities: map[string]companionEvidenceActivity{}}
			p.Metadata.Sources = map[string]string{}
			for i := range p.Tickets {
				id := fmt.Sprintf("kon-%d", i)
				p.Metadata.Sources[id] = strings.Repeat("r", 64)
				p.Tickets[i] = companionEvidenceTicket{ID: id, Title: strings.Repeat("t", 256), Summary: strings.Repeat("s", 2048), LastError: strings.Repeat("e", 1024)}
				if field == "activity" && i > 0 {
					p.Tickets[i].Activity = "x"
				}
			}
			data, err := json.Marshal(p)
			require.NoError(t, err)
			remaining := companionEvidenceMax - len(data) - 1
			for i := range p.Tickets {
				if field == "details" {
					n := min(3072, remaining)
					p.Tickets[i].Details = strings.Repeat("d", n)
					remaining -= n
				} else if i > 0 {
					n := min(6143, remaining)
					p.Tickets[i].Activity += strings.Repeat("a", n)
					remaining -= n
				}
			}
			require.Zero(t, remaining, "packet must fit within capture's per-field limits")
			data, err = json.Marshal(p)
			require.NoError(t, err)
			require.Len(t, data, companionEvidenceMax-1)
			p.Tickets[0].Activity = "overflow"
			require.NoError(t, limitCompanionEvidence(&p))
			data, err = json.Marshal(p)
			require.NoError(t, err)
			require.LessOrEqual(t, len(data), companionEvidenceMax)
			require.Contains(t, strings.Join(p.Metadata.Omissions, " "), "32 KiB evidence limit")
			require.Empty(t, p.Tickets[0].Activity)
			if field == "details" {
				require.Empty(t, p.Tickets[0].Details)
			} else {
				require.Empty(t, p.Tickets[1].Activity)
			}
		})
	}
}

func TestCompanionImplicitChatCapacityFailure(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("pi")
	cfg.Assistant.Companion.Enabled = true
	d := h.newDaemon(cfg)
	for range assistantMaxConcurrent {
		id := assistant.NewID()
		require.NoError(t, d.assistant.claim(id, "nonce", func() {}))
		defer d.assistant.release(id)
	}
	for range 3 {
		_, err := d.PostCompanionMessage(web.CompanionMessageRequest{Kind: "briefing", Text: "Explain"})
		require.ErrorIs(t, err, web.ErrAssistantAtCapacity)
	}
	threads, err := d.ListAssistantThreads()
	require.NoError(t, err)
	require.Empty(t, threads)
	entries, err := os.ReadDir(d.assistantThreadStore(cfg, "k").Root())
	require.NoError(t, err)
	require.Empty(t, entries, "rollback removes the implicit thread directory too")
}

func TestCompanionMessageValidation(t *testing.T) {
	h := newHarness(t)
	cfg := h.assistantConfig("pi")
	cfg.Assistant.Companion.Enabled = true
	d := h.newDaemon(cfg)
	for _, tc := range []struct {
		name string
		req  web.CompanionMessageRequest
	}{
		{"blank", web.CompanionMessageRequest{Kind: "briefing"}},
		{"kind", web.CompanionMessageRequest{Kind: "run", Text: "run"}},
		{"no selected ticket", web.CompanionMessageRequest{Kind: "investigation", Text: "explain"}},
		{"too many", web.CompanionMessageRequest{Kind: "investigation", Text: "explain", TicketIDs: []string{"1", "2", "3", "4", "5", "6"}}},
		{"oversized text", web.CompanionMessageRequest{Kind: "briefing", Text: strings.Repeat("x", companionEvidenceMax+1)}},
		{"general thread", web.CompanionMessageRequest{ThreadID: assistant.NewID(), Kind: "briefing", Text: "explain"}},
		{"missing ticket", web.CompanionMessageRequest{Kind: "investigation", Text: "explain", TicketIDs: []string{"missing"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.PostCompanionMessage(tc.req)
			require.Error(t, err)
			threads, err := d.ListAssistantThreads()
			require.NoError(t, err)
			require.Empty(t, threads, "rejected requests must not leave implicit chats")
		})
	}
	t.Run("existing thread survives preparation failure", func(t *testing.T) {
		thread, err := d.createCompanionThread()
		require.NoError(t, err)
		_, err = d.PostCompanionMessage(web.CompanionMessageRequest{ThreadID: thread.ID, Kind: "investigation", Text: "explain", TicketIDs: []string{"missing"}})
		require.ErrorIs(t, err, web.ErrTicketNotFound)
		stored, err := d.GetAssistantThread(thread.ID)
		require.NoError(t, err)
		require.Zero(t, stored.Turns)
		require.Empty(t, stored.Evidence)
		require.False(t, stored.Running)
	})
}
