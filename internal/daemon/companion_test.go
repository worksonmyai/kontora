package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/companion"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestCompanionObservation(t *testing.T) {
	cases := []struct {
		name, execution, condition string
		kind                       ticket.Kind
		status                     ticket.Status
	}{
		{name: "ordinary pause", status: ticket.StatusPaused, condition: "paused"},
		{name: "passive epic", kind: ticket.KindEpic, status: ticket.StatusPaused},
		{name: "supervisor pause", kind: ticket.KindEpic, execution: ticket.EpicExecutionAgent, status: ticket.StatusPaused, condition: "paused"},
		{name: "supervisor review", kind: ticket.KindEpic, execution: ticket.EpicExecutionAgent, status: ticket.StatusHumanReview, condition: "human_review"},
		{name: "supervisor question", kind: ticket.KindEpic, execution: ticket.EpicExecutionAgent, status: ticket.StatusInProgress, condition: "question"},
		{name: "supervisor completion", kind: ticket.KindEpic, execution: ticket.EpicExecutionAgent, status: ticket.StatusInProgress, condition: "completion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("true", "true")
			cfg.Assistant.Companion.Enabled = true
			var calls atomic.Int32
			d := h.newDaemon(cfg, WithTurnSpawner(func(context.Context, TurnParams) (string, error) { calls.Add(1); return "", nil }))
			now := time.Now().UTC()
			task := &ticket.Ticket{ID: "kon-test", Kind: tc.kind, EpicExecution: tc.execution, Status: tc.status, Created: &now, Body: "# Needs a person", LastError: "recorded error"}
			if tc.condition == "completion" {
				task.History = []ticket.HistoryEntry{{Stage: simpleStageName, CompletedAt: &now}}
			}
			if tc.condition == "question" {
				d.waiting[task.ID] = waitingState{Tool: "ask_user_question", ToolCallID: "question-1", Question: "Which branch?"}
			}
			d.mu.Lock()
			d.tickets[task.ID] = &ticketState{ticket: task}
			d.mu.Unlock()
			stop := d.startCompanion()
			defer stop()
			s := d.CompanionSnapshot()
			require.True(t, s.Enabled)
			require.False(t, s.AIAvailable)
			require.Equal(t, "ok", s.Health)
			if tc.condition == "" {
				require.Empty(t, s.Items)
				return
			}
			require.Len(t, s.Items, 1)
			require.Equal(t, tc.condition, s.Items[0].Condition)
			require.Equal(t, tc.condition != "completion", s.Items[0].Active)
			item := s.Items[0]
			s, err := d.ReadCompanionItem(item.ID, item.Revision, true)
			require.NoError(t, err)
			require.True(t, s.Items[0].Read)
			require.Equal(t, tc.condition != "completion", s.Items[0].Active)
			require.Equal(t, tc.status, task.Status)
			require.Zero(t, calls.Load())
			_, err = d.PostCompanionMessage(web.CompanionMessageRequest{Kind: "briefing", Text: "Explain"})
			require.ErrorIs(t, err, web.ErrAssistantDisabled)
			d.reconcileCompanion(false)
			require.Zero(t, calls.Load())
		})
	}
}

func TestCompanionDaemonNoBrowser(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.Assistant.Companion.Enabled = true
	cfg.AutoPickUp = new(false)
	var calls atomic.Int32
	d := h.newDaemon(cfg, WithTurnSpawner(func(context.Context, TurnParams) (string, error) { calls.Add(1); return "", nil }))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-errCh) }()
	h.writeTicket("tst-inbox.md", h.taskMD("tst-inbox", "paused", "one-stage"))
	require.Eventually(t, func() bool {
		for _, item := range d.CompanionSnapshot().Items {
			if item.TicketID == "tst-inbox" && item.Active {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	require.Zero(t, calls.Load())
}

func TestCompanionRestart(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.Assistant.Companion.Enabled = true
	now := time.Now().UTC()
	task := &ticket.Ticket{ID: "kon-test", Status: ticket.StatusHumanReview, Created: &now, Body: "# Review"}
	d := h.newDaemon(cfg)
	d.tickets[task.ID] = &ticketState{ticket: task}
	stop := d.startCompanion()
	s := d.CompanionSnapshot()
	require.Len(t, s.Items, 1)
	item := s.Items[0]
	_, err := d.ReadCompanionItem(item.ID, item.Revision, true)
	require.NoError(t, err)
	stop()
	next := h.newDaemon(cfg)
	next.tickets[task.ID] = &ticketState{ticket: task}
	stop = next.startCompanion()
	defer stop()
	s = next.CompanionSnapshot()
	require.Len(t, s.Items, 1)
	require.Equal(t, item.ID, s.Items[0].ID)
	require.True(t, s.Items[0].Read)
}

func TestCompanionCorruptStore(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.Assistant.Companion.Enabled = true
	d := h.newDaemon(cfg)
	dir := d.companionDir()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "state.json")
	require.NoError(t, os.WriteFile(path, []byte("broken"), 0o600))
	stop := d.startCompanion()
	defer stop()
	require.Equal(t, "degraded", d.CompanionSnapshot().Health)
	_, err := d.CatchUpCompanion(0)
	require.ErrorIs(t, err, companion.ErrStorageUnavailable)
	require.EqualError(t, err, "companion storage is unavailable; read state and checkpoints cannot be saved")
	require.NotErrorIs(t, err, web.ErrAssistantAtCapacity)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "broken", string(data))
}

func TestCompanionLateCompletion(t *testing.T) {
	for _, kind := range []string{"simple ticket", "pipeline history"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("true", "true")
			cfg.Assistant.Companion.Enabled = true
			d := h.newDaemon(cfg)
			task := &ticket.Ticket{ID: "kon-late", Status: ticket.StatusInProgress}
			d.tickets[task.ID] = &ticketState{ticket: task}
			d.reconcileCompanion(true)
			t.Cleanup(func() { require.NoError(t, d.companion.store.Close()) })
			observed := d.CompanionSnapshot().ObservedAt
			completed := observed.Truncate(time.Second)
			if kind == "simple ticket" {
				task.Status = ticket.StatusDone
				task.CompletedAt = &completed
			} else {
				completed = observed.Add(-time.Minute)
				task.History = []ticket.HistoryEntry{{Stage: "build", CompletedAt: &completed}}
			}
			d.reconcileCompanion(false)
			snapshot := d.CompanionSnapshot()
			require.Len(t, snapshot.Items, 1)
			require.Equal(t, "completion", snapshot.Items[0].Condition)
			require.False(t, snapshot.Items[0].Active)
			d.reconcileCompanion(false)
			require.Equal(t, snapshot.Items, d.CompanionSnapshot().Items)
		})
	}
}

func TestCompanionExpiredCompletionRestart(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.Assistant.Companion.Enabled = true
	d := h.newDaemon(cfg)
	then := time.Now().UTC().Add(-31 * 24 * time.Hour)
	task := &ticket.Ticket{ID: "kon-expired", Status: ticket.StatusDone, CompletedAt: &then}
	d.tickets[task.ID] = &ticketState{ticket: task}
	var saved companion.State
	saved.Reconcile(companionObservations(d.companionTickets()), then, true)
	require.Len(t, saved.Items, 1)
	require.NoError(t, saved.SetRead(saved.Items[0].ID, saved.Items[0].Revision, true))
	saved.SeenCompletions = nil
	store, err := companion.Open(d.companionDir())
	require.NoError(t, err)
	require.NoError(t, store.Save(saved))
	require.NoError(t, store.Close())
	d.reconcileCompanion(true)
	defer func() { require.NoError(t, d.companion.store.Close()) }()
	require.Empty(t, d.CompanionSnapshot().Items)
	d.reconcileCompanion(false)
	require.Empty(t, d.CompanionSnapshot().Items)
}

func TestCompanionCompletionCountRetention(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.Assistant.Companion.Enabled = true
	d := h.newDaemon(cfg)
	now := time.Now().UTC()
	task := &ticket.Ticket{ID: "kon-history", Status: ticket.StatusInProgress}
	for n := range 503 {
		completed := now.Add(-time.Duration(n+1) * time.Minute)
		task.History = append(task.History, ticket.HistoryEntry{Stage: fmt.Sprintf("stage-%d", n), CompletedAt: &completed})
	}
	d.tickets[task.ID] = &ticketState{ticket: task}
	d.reconcileCompanion(true)
	t.Cleanup(func() { require.NoError(t, d.companion.store.Close()) })
	snapshot := d.CompanionSnapshot()
	require.Len(t, snapshot.Items, 500)
	require.Contains(t, snapshot.Items[0].Detail, "stage-0 exited")
	require.Contains(t, snapshot.Items[499].Detail, "stage-499 exited")
	item := snapshot.Items[0]
	_, err := d.ReadCompanionItem(item.ID, item.Revision, true)
	require.NoError(t, err)
	d.reconcileCompanion(false)
	require.Len(t, d.CompanionSnapshot().Items, 500)

	completed := now.Add(-24 * time.Hour)
	task.History = append(task.History, ticket.HistoryEntry{Stage: "late", CompletedAt: &completed})
	d.reconcileCompanion(false)
	snapshot = d.CompanionSnapshot()
	require.Len(t, snapshot.Items, 500)
	var lateID string
	for _, item := range snapshot.Items {
		if item.Detail == "late exited 0. " {
			lateID = item.ID
		}
	}
	require.NotEmpty(t, lateID)
	d.reconcileCompanion(false)
	require.Equal(t, snapshot.Items, d.CompanionSnapshot().Items)
	require.NoError(t, d.companion.store.Close())

	next := h.newDaemon(cfg)
	next.tickets[task.ID] = &ticketState{ticket: task}
	next.reconcileCompanion(true)
	defer func() { require.NoError(t, next.companion.store.Close()) }()
	require.Equal(t, snapshot.Items, next.CompanionSnapshot().Items)
	require.Equal(t, snapshot.Revision, next.CompanionSnapshot().Revision)
}

func TestCompanionStorageMutationFailure(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.Assistant.Companion.Enabled = true
	d := h.newDaemon(cfg)
	d.tickets["kon-paused"] = &ticketState{ticket: &ticket.Ticket{ID: "kon-paused", Status: ticket.StatusPaused}}
	d.reconcileCompanion(true)
	defer func() { require.NoError(t, d.companion.store.Close()) }()
	snapshot := d.CompanionSnapshot()
	require.Len(t, snapshot.Items, 1)
	path := filepath.Join(d.companionDir(), "state.json")
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	item := snapshot.Items[0]
	_, err := d.ReadCompanionItem(item.ID, item.Revision, true)
	require.ErrorIs(t, err, companion.ErrStorageUnavailable)
	require.NotErrorIs(t, err, web.ErrAssistantAtCapacity)
	require.EqualError(t, err, "companion storage is unavailable; read state and checkpoints cannot be saved")
	require.Equal(t, "degraded", d.CompanionSnapshot().Health)
	require.Equal(t, snapshot.Items, d.CompanionSnapshot().Items)
	require.Equal(t, snapshot.Revision, d.CompanionSnapshot().Revision)
	_, err = d.CatchUpCompanion(snapshot.Revision)
	require.ErrorIs(t, err, companion.ErrStorageUnavailable)
	require.Zero(t, d.CompanionSnapshot().CaughtUp)
}

func TestCompanionStorageUnavailableHTTP(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.Assistant.Companion.Enabled = true
	d := h.newDaemon(cfg)
	dir := d.companionDir()
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte("broken"), 0600))
	d.reconcileCompanion(true)
	defer func() { require.NoError(t, d.companion.store.Close()) }()
	srv, err := web.New(d, d.broker, "127.0.0.1", 0, "", nil, d.tmuxSession, testLogger(t))
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	defer func() { require.NoError(t, srv.Shutdown(context.Background())) }()
	for _, tc := range []struct {
		path, body string
	}{
		{"/api/assistant/companion", `{"revision":0}`},
		{"/api/assistant/companion/items/item", `{"revision":1,"read":true}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPatch, "http://"+srv.Addr()+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			var body struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
			require.Equal(t, companion.ErrStorageUnavailable.Error(), body.Error)
		})
	}
}

func TestCompanionQuestionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limited bool
	}{
		{"tool call", false}, {"source timestamp", false}, {"missing identifiers", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.defaultConfig("true", "true")
			cfg.Assistant.Companion.Enabled = true
			d := h.newDaemon(cfg)
			d.tickets["kon-test"] = &ticketState{ticket: &ticket.Ticket{ID: "kon-test", Status: ticket.StatusInProgress}}
			d.reconcileCompanion(true)
			t.Cleanup(func() { require.NoError(t, d.companion.store.Close()) })
			for _, id := range []string{"a", "b", "a"} {
				st := &waitingState{Invocation: "run", Tool: "ask", Question: id}
				switch tc.name {
				case "tool call":
					st.ToolCallID = id
				case "source timestamp":
					st.RawStartedAt = "2026-11-03T10:00:00Z"
					if id == "b" {
						st.RawStartedAt = "2026-11-03T10:01:00Z"
					}
				}
				d.applyWaitMarker("kon-test", st)
				d.reconcileCompanion(false)
				if id == "a" {
					for _, item := range d.CompanionSnapshot().Items {
						if item.Active {
							_, err := d.ReadCompanionItem(item.ID, item.Revision, true)
							require.NoError(t, err)
						}
					}
				}
			}
			s := d.CompanionSnapshot()
			require.Len(t, s.Items, 2)
			active := 0
			for _, item := range s.Items {
				require.Equal(t, tc.limited, item.IdentityLimited)
				if item.Active {
					active++
					require.Equal(t, "a", item.Detail)
					require.True(t, item.Read)
				}
			}
			require.Equal(t, 1, active)
		})
	}
}

func TestCompanionHistoryReplay(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name     string
		observed time.Time
		occurred time.Time
		retained bool
		want     int
	}{
		{"initial recent", time.Time{}, now.Add(-time.Hour), false, 1},
		{"initial expired", time.Time{}, now.Add(-31 * 24 * time.Hour), false, 0},
		{"late unseen source", now, now.Add(-time.Hour), false, 1},
		{"new source", now.Add(-time.Hour), now, false, 1},
		{"retained source", now, now.Add(-time.Hour), true, 1},
		{"expired retained source", now.Add(-31 * 24 * time.Hour), now.Add(-31 * 24 * time.Hour), true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state companion.State
			observations := []companion.Observation{{Source: "source", Condition: "completion", OccurredAt: tc.occurred}}
			if tc.retained {
				state.Reconcile(observations, tc.occurred, true)
			}
			state.ObservedAt = tc.observed
			state.Reconcile(companionOrderHistory(observations), now, true)
			require.Len(t, state.Items, tc.want)
		})
	}
}

func TestCompanionEnablement(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	d := h.newDaemon(cfg)
	stop := d.startCompanion()
	defer stop()
	require.False(t, d.CompanionSnapshot().Enabled)
	enabled := *cfg
	enabled.Assistant.Companion = config.Companion{Enabled: true, Name: "Kotik"}
	d.cfg.Store(&enabled)
	d.wakeCompanion()
	require.Eventually(t, func() bool { return !d.CompanionSnapshot().ObservedAt.IsZero() }, time.Second, 10*time.Millisecond)
	disabled := enabled
	disabled.Assistant.Companion.Enabled = false
	d.cfg.Store(&disabled)
	require.False(t, d.CompanionSnapshot().Enabled)
	_, err := d.CatchUpCompanion(0)
	require.ErrorIs(t, err, web.ErrInvalidState)
}
