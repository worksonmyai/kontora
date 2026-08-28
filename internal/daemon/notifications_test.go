package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// notifyRecorder stands in for the dispatcher. The daemon calls it from its own
// goroutines, so every field is read back through the mutex.
type notifyRecorder struct {
	mu       sync.Mutex
	observed []notify.Observation
	waiting  []notify.Fields
	forgot   []string
}

func (r *notifyRecorder) Live() bool { return true }

func (r *notifyRecorder) Observe(obs notify.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, obs)
}

func (r *notifyRecorder) Waiting(_ string, want, channels []string, f notify.Fields) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !notify.Wants(want, notify.StatusWaiting) || len(channels) == 0 {
		return
	}
	r.waiting = append(r.waiting, f)
}

func (r *notifyRecorder) Forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgot = append(r.forgot, id)
}

func (r *notifyRecorder) all() []notify.Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.observed)
}

func (r *notifyRecorder) waitingFields() []notify.Fields {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.waiting)
}

func (r *notifyRecorder) forgotten() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.forgot)
}

// sends replays the observations through the dispatcher's own send rule and
// returns the events that would have been delivered. Asserting on those rather
// than on the raw observations is what makes these tests about the notification
// the user sees, not about how many times the daemon happened to write.
func (r *notifyRecorder) sends(t *testing.T) []notify.Event {
	t.Helper()
	var out []notify.Event
	seen := map[string]string{}
	for _, obs := range r.all() {
		prev, known := seen[obs.ID]
		seen[obs.ID] = obs.Status
		if notify.ShouldSend(obs, prev, known) {
			out = append(out, notify.Event{TicketID: obs.ID, From: prev, To: obs.Status, Fields: obs.Fields})
		}
	}
	return out
}

// lastFor returns the last observation recorded for one ticket.
func (r *notifyRecorder) lastFor(t *testing.T, id string) notify.Observation {
	t.Helper()
	var last *notify.Observation
	for _, obs := range r.all() {
		if obs.ID == id {
			last = &obs
		}
	}
	require.NotNil(t, last, "no observation for %s", id)
	return *last
}

// notifyTaskMD is taskMD with a notify: list and optional channel override.
func notifyTaskMD(h *testHarness, id, status, pipeline string, notifyList, channels []string) string {
	extra := ""
	if notifyList != nil {
		extra += fmt.Sprintf("notify: [%s]\n", strings.Join(notifyList, ", "))
	}
	if channels != nil {
		extra += fmt.Sprintf("notify_channels: [%s]\n", strings.Join(channels, ", "))
	}
	return fmt.Sprintf(`---
id: %s
kontora: true
status: %s
pipeline: %s
path: %s
created: 2026-01-01T00:00:00Z
%s---
# Test ticket %s
`, id, status, pipeline, h.repoDir, extra, id)
}

// notifyConfig is the harness config with one channel named tg as the default.
func notifyConfig(h *testHarness) *config.Config {
	cfg := h.defaultConfig("true", "true")
	cfg.Notifications = config.Notifications{
		Channels: map[string]config.NotifyChannel{
			"tg": {Type: config.NotifyWebhook, URL: "https://example.invalid/tg"},
			"mm": {Type: config.NotifyWebhook, URL: "https://example.invalid/mm"},
		},
		Default: []string{"tg"},
	}
	return cfg
}

func TestNotifyOnPipelineOutcome(t *testing.T) {
	tests := []struct {
		name     string
		notify   []string
		pipeline string
		agent    string
		want     ticket.Status
		wantSent []string
	}{
		{
			name:     "a completed pipeline reports done",
			notify:   []string{"done"},
			pipeline: "one-stage",
			agent:    "true",
			want:     ticket.StatusDone,
			wantSent: []string{"done"},
		},
		{
			name:     "an unlisted status is silent",
			notify:   []string{"done"},
			pipeline: "one-stage",
			agent:    "false",
			want:     ticket.StatusPaused,
		},
		{
			name:     "a failing stage under pause reports paused once",
			notify:   []string{"paused"},
			pipeline: "one-stage",
			agent:    "false",
			want:     ticket.StatusPaused,
			wantSent: []string{"paused"},
		},
		{
			name:     "a ticket with no notify list is silent",
			pipeline: "one-stage",
			agent:    "true",
			want:     ticket.StatusDone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := notifyConfig(h)
			cfg.Agents["agent1"] = config.Agent{Binary: tt.agent}
			rec := &notifyRecorder{}
			d := h.newDaemon(cfg, WithNotifier(rec))
			runDaemon(t, d)

			h.writeTicket("n-1.md", notifyTaskMD(h, "n-1", "todo", tt.pipeline, tt.notify, nil))
			h.waitForStatus("n-1.md", tt.want, 10*time.Second)

			var got []string
			for _, e := range rec.sends(t) {
				got = append(got, e.To)
			}
			assert.Equal(t, tt.wantSent, got)
		})
	}
}

func TestNotifyDoneCarriesTheRunSummary(t *testing.T) {
	h := newHarness(t)
	// The run's last assistant message becomes the ticket's summary, which is
	// the only summary that exists when the done notification fires.
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		require.NoError(t, os.MkdirAll(p.SessionDir, 0o755))
		line := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Implemented the fix."}]}}`
		require.NoError(t, os.WriteFile(filepath.Join(p.SessionDir, "session.jsonl"), []byte(line), 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := notifyConfig(h)
	cfg.Agents["agent1"] = config.Agent{Binary: "pi"}
	rec := &notifyRecorder{}
	d := h.newDaemon(cfg, WithNotifier(rec), WithRunner(runner))
	runDaemon(t, d)

	h.writeTicket("n-2.md", notifyTaskMD(h, "n-2", "todo", "one-stage", []string{"done"}, nil))
	h.waitForStatus("n-2.md", ticket.StatusDone, 10*time.Second)

	sends := rec.sends(t)
	require.Len(t, sends, 1)
	assert.Equal(t, "done", sends[0].To)
	// final_summary is written by a separate pass up to two minutes later, so a
	// done notification can only ever render from the per-run summary. Moving
	// the observation into storeFinalSummary to reach it would lose this.
	assert.Equal(t, "Implemented the fix.", sends[0].Fields.Summary)
}

func TestNotifyStaysSilentForRequestedChanges(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, d *Daemon, id string)
	}{
		{
			name: "the service bridge",
			change: func(t *testing.T, d *Daemon, id string) {
				_, err := d.svc.SetStatus(id, ticket.StatusDone)
				require.NoError(t, err)
			},
		},
		{
			name: "a dashboard pause",
			change: func(t *testing.T, d *Daemon, id string) {
				require.NoError(t, d.PauseTicket(id))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := notifyConfig(h)
			cfg.Agents["agent1"] = config.Agent{Binary: "sleep", Args: []string{"30"}}
			// Empty, or the rendered prompt is appended as a second argument
			// and sleep exits 1 before the ticket can be paused.
			cfg.Stages["step1"] = config.Stage{Prompt: ""}
			rec := &notifyRecorder{}
			d := h.newDaemon(cfg, WithNotifier(rec))
			runDaemon(t, d)

			h.writeTicket("n-3.md", notifyTaskMD(h, "n-3", "todo", "one-stage", []string{"done", "paused"}, nil))
			h.waitForStatus("n-3.md", ticket.StatusInProgress, 10*time.Second)

			tt.change(t, d, "n-3")

			require.Eventually(t, func() bool {
				return rec.lastFor(t, "n-3").Origin == notify.OriginRequest
			}, 5*time.Second, 20*time.Millisecond)
			assert.Empty(t, rec.sends(t), "a change a person asked for must not send")
		})
	}
}

func TestNotifyRecordsAnExternalEditWithoutSending(t *testing.T) {
	h := newHarness(t)
	cfg := notifyConfig(h)
	rec := &notifyRecorder{}
	d := h.newDaemon(cfg, WithNotifier(rec))
	runDaemon(t, d)

	// open: the daemon will not pick it up, so the only writes are the ones
	// this test makes.
	h.writeTicket("n-4.md", notifyTaskMD(h, "n-4", "open", "one-stage", []string{"done"}, nil))
	require.Eventually(t, func() bool {
		return len(rec.all()) > 0
	}, 5*time.Second, 20*time.Millisecond)

	// The local CLI writes ticket files itself, so `kontora human-review <id>`
	// arrives here as an external edit.
	h.writeTicket("n-4.md", notifyTaskMD(h, "n-4", "human_review", "one-stage", []string{"done"}, nil))
	require.Eventually(t, func() bool {
		return rec.lastFor(t, "n-4").Status == string(ticket.StatusHumanReview)
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, notify.OriginObserved, rec.lastFor(t, "n-4").Origin)
	assert.Empty(t, rec.sends(t))
}

func TestNotifySeedsOnStartupWithoutSending(t *testing.T) {
	h := newHarness(t)
	cfg := notifyConfig(h)
	rec := &notifyRecorder{}

	h.writeTicket("n-5.md", notifyTaskMD(h, "n-5", "human_review", "one-stage", []string{"human_review", "todo"}, nil))
	d := h.newDaemon(cfg, WithNotifier(rec))
	runDaemon(t, d)

	require.Eventually(t, func() bool { return len(rec.all()) > 0 }, 5*time.Second, 20*time.Millisecond)
	obs := rec.lastFor(t, "n-5")
	assert.Equal(t, notify.OriginObserved, obs.Origin)
	assert.Equal(t, []string{"human_review", "todo"}, obs.Want)
	assert.Empty(t, rec.sends(t), "a restart over a ticket already in a listed status must not send")
}

func TestNotifySeedsCrashRecoveryAsTodo(t *testing.T) {
	h := newHarness(t)
	cfg := notifyConfig(h)
	cfg.AutoPickUp = new(false)
	rec := &notifyRecorder{}

	md := notifyTaskMD(h, "n-6", "in_progress", "one-stage", []string{"todo"}, nil)
	h.writeTicket("n-6.md", md)
	d := h.newDaemon(cfg, WithNotifier(rec))
	runDaemon(t, d)

	require.Eventually(t, func() bool { return len(rec.all()) > 0 }, 5*time.Second, 20*time.Millisecond)
	obs := rec.lastFor(t, "n-6")
	assert.Equal(t, notify.OriginObserved, obs.Origin)
	assert.Equal(t, string(ticket.StatusTodo), obs.Status,
		"the seed must be the recovered status, not the stale in_progress")
	assert.Empty(t, rec.sends(t))
	assert.Equal(t, string(ticket.StatusTodo), string(h.readTask("n-6.md").Status),
		"crash recovery must still have written the file")
}

func TestNotifyForgetsARemovedTicket(t *testing.T) {
	h := newHarness(t)
	cfg := notifyConfig(h)
	cfg.AutoPickUp = new(false)
	rec := &notifyRecorder{}
	d := h.newDaemon(cfg, WithNotifier(rec))
	runDaemon(t, d)

	path := h.writeTicket("n-7.md", notifyTaskMD(h, "n-7", "open", "one-stage", []string{"done"}, nil))
	require.Eventually(t, func() bool { return len(rec.all()) > 0 }, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, os.Remove(path))
	require.Eventually(t, func() bool {
		return slices.Contains(rec.forgotten(), "n-7")
	}, 5*time.Second, 20*time.Millisecond)
}

func TestNotifyResolvesChannels(t *testing.T) {
	tests := []struct {
		name           string
		projectChans   []string
		ticketChannels []string
		want           []string
	}{
		{name: "the global default", want: []string{"tg"}},
		{name: "a project overrides the default", projectChans: []string{"mm"}, want: []string{"mm"}},
		{
			name:           "a ticket overrides its project",
			projectChans:   []string{"mm"},
			ticketChannels: []string{"tg"},
			want:           []string{"tg"},
		},
		{name: "none on the ticket silences it", ticketChannels: []string{config.NoneSentinel}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := notifyConfig(h)
			cfg.Projects = map[string]config.Project{
				h.repoName: {Path: h.repoDir, NotifyChannels: tt.projectChans},
			}
			rec := &notifyRecorder{}
			d := h.newDaemon(cfg, WithNotifier(rec))
			runDaemon(t, d)

			h.writeTicket("n-8.md", notifyTaskMD(h, "n-8", "todo", "one-stage", []string{"done"}, tt.ticketChannels))
			h.waitForStatus("n-8.md", ticket.StatusDone, 10*time.Second)

			sends := rec.sends(t)
			if tt.want == nil {
				assert.Empty(t, sends)
				return
			}
			require.Len(t, sends, 1)
			assert.Equal(t, tt.want, rec.lastFor(t, "n-8").Channels)
		})
	}
}

func TestNotifyWaitingFiresOncePerToolCall(t *testing.T) {
	h := newHarness(t)
	cfg := notifyConfig(h)
	rec := &notifyRecorder{}
	d := h.newDaemon(cfg, WithNotifier(rec), WithWaitPollInterval(fastWaitPoll))

	// applyWaitMarker reads the ticket out of d.tickets, so it has to be there.
	path := h.writeTicket("n-9.md", notifyTaskMD(h, "n-9", "open", "one-stage", []string{notify.StatusWaiting}, nil))
	tkt, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets["n-9"] = newTicketState(tkt, path)

	since := time.Now().UTC()
	q1 := &waitingState{Tool: "ask", ToolCallID: "A", Since: since, Question: "first?"}
	for range 4 {
		d.applyWaitMarker("n-9", q1)
	}
	d.applyWaitMarker("n-9", &waitingState{Tool: "ask", ToolCallID: "B", Since: since, Question: "second?"})
	d.applyWaitMarker("n-9", nil)

	got := rec.waitingFields()
	require.Len(t, got, 2, "one notification per tool call, not per poll")
	assert.Equal(t, "first?", got[0].Question)
	assert.Equal(t, "second?", got[1].Question)
	assert.Empty(t, rec.sends(t), "waiting must never enter the status stream")
}

func TestNotifyWaitingIgnoresARewordedQuestion(t *testing.T) {
	h := newHarness(t)
	rec := &notifyRecorder{}
	d := h.newDaemon(notifyConfig(h), WithNotifier(rec), WithWaitPollInterval(fastWaitPoll))

	path := h.writeTicket("n-10.md", notifyTaskMD(h, "n-10", "open", "one-stage", []string{notify.StatusWaiting}, nil))
	tkt, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets["n-10"] = newTicketState(tkt, path)

	since := time.Now().UTC()
	d.applyWaitMarker("n-10", &waitingState{Tool: "ask", ToolCallID: "A", Since: since, Question: "first?"})
	d.applyWaitMarker("n-10", &waitingState{Tool: "ask", ToolCallID: "A", Since: since, Question: "first, really?"})

	assert.Len(t, rec.waitingFields(), 1)
}

// --- wire tests -------------------------------------------------------------

// capturedRequest is what a wire test's server saw.
type capturedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// notifyWireServer answers with status and records every request.
func notifyWireServer(t *testing.T, status int, body string) (*httptest.Server, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, capturedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: raw})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// runDispatcher starts a real dispatcher over ch and drives one daemon-decided
// transition through it.
func runDispatcher(t *testing.T, ch notify.Channel, attempts int) {
	t.Helper()
	d := notify.New(notify.Options{
		Channels: []notify.Channel{ch},
		Attempts: attempts,
		Backoff:  time.Millisecond,
		Log:      testLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	obs := notify.Observation{
		Origin: notify.OriginDaemon, ID: "kon-w", Want: []string{"done"}, Channels: []string{ch.Name()},
		Fields: notify.Fields{Title: "Wire it up", Stage: "implement"},
	}
	obs.Status = "in_progress"
	d.Observe(obs)
	obs.Status = "done"
	d.Observe(obs)
}

func TestNotifyWireTelegram(t *testing.T) {
	srv, requests := notifyWireServer(t, http.StatusOK, `{"ok":true}`)
	runDispatcher(t, &notify.Telegram{ChannelName: "tg", Token: "123:secret", ChatID: "42", BaseURL: srv.URL}, 3)

	require.Eventually(t, func() bool { return len(requests()) == 1 }, 3*time.Second, 10*time.Millisecond)
	req := requests()[0]
	assert.Equal(t, http.MethodPost, req.method)
	assert.Equal(t, "/bot123:secret/sendMessage", req.path)
	assert.Equal(t, "application/json", req.header.Get("Content-Type"))

	var body map[string]string
	require.NoError(t, json.Unmarshal(req.body, &body))
	assert.Equal(t, "42", body["chat_id"])
	assert.Equal(t, "HTML", body["parse_mode"])
	assert.Contains(t, body["text"], "kon-w: done (was in_progress)")
	assert.NotContains(t, string(req.body), "123:secret", "the token belongs in the path and nowhere else")
}

func TestNotifyWireMattermost(t *testing.T) {
	srv, requests := notifyWireServer(t, http.StatusOK, "ok")
	runDispatcher(t, &notify.Mattermost{ChannelName: "mm", URL: srv.URL + "/hooks/abc", Channel: "town-square"}, 3)

	require.Eventually(t, func() bool { return len(requests()) == 1 }, 3*time.Second, 10*time.Millisecond)
	req := requests()[0]
	assert.Equal(t, http.MethodPost, req.method)
	assert.Equal(t, "/hooks/abc", req.path)

	var body map[string]string
	require.NoError(t, json.Unmarshal(req.body, &body))
	assert.Equal(t, "town-square", body["channel"])
	assert.Contains(t, body["text"], "kon-w: done")
}

func TestNotifyWireWebhook(t *testing.T) {
	srv, requests := notifyWireServer(t, http.StatusOK, "ok")
	runDispatcher(t, &notify.Webhook{
		ChannelName: "hook", URL: srv.URL + "/ingest",
		Headers: map[string]string{"X-Team": "kontora"}, Token: "shhh",
	}, 3)

	require.Eventually(t, func() bool { return len(requests()) == 1 }, 3*time.Second, 10*time.Millisecond)
	req := requests()[0]
	assert.Equal(t, http.MethodPost, req.method)
	assert.Equal(t, "/ingest", req.path)
	assert.Equal(t, "kontora", req.header.Get("X-Team"))
	assert.Equal(t, "Bearer shhh", req.header.Get("Authorization"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(req.body, &body))
	assert.Equal(t, "kon-w", body["ticket"])
	assert.Equal(t, "done", body["to"])
	assert.Equal(t, "implement", body["stage"])
}

func TestNotifyWireDoesNotRetryA401(t *testing.T) {
	srv, requests := notifyWireServer(t, http.StatusUnauthorized, `{"description":"Unauthorized"}`)
	runDispatcher(t, &notify.Webhook{ChannelName: "hook", URL: srv.URL + "/ingest"}, 3)

	// One attempt, and it stays one: a 4xx other than 429 is a configuration
	// mistake, and repeating it only repeats the same rejection.
	require.Eventually(t, func() bool { return len(requests()) == 1 }, 3*time.Second, 10*time.Millisecond)
	assert.Never(t, func() bool { return len(requests()) > 1 }, 300*time.Millisecond, 20*time.Millisecond)
}

// --- construction -----------------------------------------------------------

func TestBuildNotifyChannelResolvesSecrets(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "webhook-url")
	require.NoError(t, os.WriteFile(secretFile, []byte("https://mm.example.com/hooks/abc\n"), 0o600))
	t.Setenv("KONTORA_TEST_TG_TOKEN", "123:secret")

	tests := []struct {
		name    string
		channel config.NotifyChannel
		check   func(t *testing.T, ch notify.Channel)
		wantErr string
	}{
		{
			name:    "telegram from the environment",
			channel: config.NotifyChannel{Type: config.NotifyTelegram, SecretEnv: "KONTORA_TEST_TG_TOKEN", ChatID: "42"},
			check: func(t *testing.T, ch notify.Channel) {
				assert.Equal(t, "123:secret", ch.(*notify.Telegram).Token)
			},
		},
		{
			name:    "mattermost from a file, trimmed",
			channel: config.NotifyChannel{Type: config.NotifyMattermost, SecretFile: secretFile},
			check: func(t *testing.T, ch notify.Channel) {
				assert.Equal(t, "https://mm.example.com/hooks/abc", ch.(*notify.Mattermost).URL)
			},
		},
		{
			name:    "a webhook needs no credential",
			channel: config.NotifyChannel{Type: config.NotifyWebhook, URL: "https://example.com/ingest"},
			check: func(t *testing.T, ch notify.Channel) {
				w := ch.(*notify.Webhook)
				assert.Empty(t, w.Token)
				assert.Equal(t, http.MethodPost, w.Method)
			},
		},
		{
			name:    "an unset variable drops the channel",
			channel: config.NotifyChannel{Type: config.NotifyTelegram, SecretEnv: "KONTORA_TEST_UNSET", ChatID: "42"},
			wantErr: "KONTORA_TEST_UNSET",
		},
		{
			name:    "an unreadable file drops the channel",
			channel: config.NotifyChannel{Type: config.NotifyMattermost, SecretFile: filepath.Join(dir, "missing")},
			wantErr: "missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := buildNotifyChannel("c", tt.channel)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "c", ch.Name())
			tt.check(t, ch)
		})
	}
}

// TestNotificationsDeliverEndToEnd drives one real pipeline completion through a
// real dispatcher and a real HTTP endpoint. It covers the two things only an
// end-to-end run can show: that the counter reflects what happened, and that a
// failed delivery leaves the ticket exactly as the pipeline left it.
func TestNotificationsDeliverEndToEnd(t *testing.T) {
	const attempts = 3

	tests := []struct {
		name         string
		status       int
		wantRequests int
		wantResult   string
	}{
		{name: "delivered", status: http.StatusOK, wantRequests: 1, wantResult: "ok"},
		{name: "a 5xx is retried then dropped", status: http.StatusServiceUnavailable, wantRequests: attempts, wantResult: "failed"},
		{name: "a 401 is not retried", status: http.StatusUnauthorized, wantRequests: 1, wantResult: "failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			srv, requests := notifyWireServer(t, tt.status, `{"description":"nope"}`)

			cfg := h.defaultConfig("true", "true")
			cfg.Notifications = config.Notifications{
				Attempts: new(attempts),
				Timeout:  config.Duration{Duration: 2 * time.Second},
				Backoff:  config.Duration{Duration: time.Millisecond},
				Channels: map[string]config.NotifyChannel{
					"hook": {Type: config.NotifyWebhook, URL: srv.URL + "/ingest"},
				},
				Default: []string{"hook"},
			}

			d, collect := h.newMetricsDaemon(cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			h.writeTicket("n-11.md", notifyTaskMD(h, "n-11", "todo", "one-stage", []string{"done"}, nil))
			done := h.waitForStatus("n-11.md", ticket.StatusDone, 10*time.Second)

			require.Eventually(t, func() bool { return len(requests()) == tt.wantRequests },
				5*time.Second, 20*time.Millisecond)
			assert.Never(t, func() bool { return len(requests()) > tt.wantRequests },
				300*time.Millisecond, 50*time.Millisecond)

			cancel()
			require.NoError(t, <-errCh)

			// Delivery is best effort: whatever the endpoint answered, the
			// ticket is what the pipeline left behind.
			after := h.readTask("n-11.md")
			assert.Equal(t, ticket.StatusDone, after.Status)
			assert.Empty(t, after.LastError)
			assert.Equal(t, done.History, after.History)

			got := collect()["kontora.notifications.sent"]
			require.NotNil(t, got.Data, "kontora.notifications.sent must be exported")
			assert.Equal(t, map[string]int64{tt.wantResult: 1}, sumByAttr(t, got, "result"))
			assert.Equal(t, map[string]int64{"hook": 1}, sumByAttr(t, got, "channel"))
		})
	}
}

// TestNotificationsDeliverAfterARestart covers a ticket already on disk when the
// daemon starts. Its startup seed is what the first transition afterwards diffs
// against, so a dispatcher built after the initial scan would read that
// transition as a first sighting and send nothing.
func TestNotificationsDeliverAfterARestart(t *testing.T) {
	h := newHarness(t)
	srv, requests := notifyWireServer(t, http.StatusOK, "ok")

	cfg := h.defaultConfig("true", "true")
	cfg.Notifications = config.Notifications{
		Attempts: new(1),
		Timeout:  config.Duration{Duration: 2 * time.Second},
		Backoff:  config.Duration{Duration: time.Millisecond},
		Channels: map[string]config.NotifyChannel{
			"hook": {Type: config.NotifyWebhook, URL: srv.URL + "/ingest"},
		},
		Default: []string{"hook"},
	}

	// On disk before Run, so the initial scan is what seeds it.
	h.writeTicket("n-12.md", notifyTaskMD(h, "n-12", "todo", "one-stage", []string{"done"}, nil))

	d := h.newDaemon(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-errCh) }()

	h.waitForStatus("n-12.md", ticket.StatusDone, 10*time.Second)
	require.Eventually(t, func() bool { return len(requests()) == 1 },
		5*time.Second, 20*time.Millisecond, "the transition after the startup seed must be delivered")

	var body map[string]any
	require.NoError(t, json.Unmarshal(requests()[0].body, &body))
	assert.Equal(t, "n-12", body["ticket"])
	// in_progress, not the seeded todo: from is the last status observed, and
	// pickup wrote in_progress between the two.
	assert.Equal(t, "in_progress", body["from"])
	assert.Equal(t, "done", body["to"])
}

// TestNotifyWaitingIgnoresAReturningQuestion covers what the pi extension does
// when the newer of two open questions is answered: it rewrites the marker back
// to the older one, which the previous marker alone reads as a new question.
func TestNotifyWaitingIgnoresAReturningQuestion(t *testing.T) {
	h := newHarness(t)
	rec := &notifyRecorder{}
	d := h.newDaemon(notifyConfig(h), WithNotifier(rec), WithWaitPollInterval(fastWaitPoll))

	path := h.writeTicket("n-11.md", notifyTaskMD(h, "n-11", "open", "one-stage", []string{notify.StatusWaiting}, nil))
	tkt, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets["n-11"] = newTicketState(tkt, path)

	since := time.Now().UTC()
	mark := func(id, question string) {
		d.applyWaitMarker("n-11", &waitingState{Tool: "ask", ToolCallID: id, Since: since, Question: question})
	}
	mark("A", "first?")
	mark("B", "second?")
	mark("A", "first?")

	got := rec.waitingFields()
	require.Len(t, got, 2, "the user was already told about the returning question")
	assert.Equal(t, "first?", got[0].Question)
	assert.Equal(t, "second?", got[1].Question)

	// The run ends and the next one's tool call IDs are its own.
	d.applyWaitMarker("n-11", nil)
	mark("A", "a new run asks again")
	assert.Len(t, rec.waitingFields(), 3)
}

func TestNotifyForgetsATicketDeletedThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	cfg := notifyConfig(h)
	cfg.AutoPickUp = new(false)
	rec := &notifyRecorder{}
	d := h.newDaemon(cfg, WithNotifier(rec))
	runDaemon(t, d)

	h.writeTicket("n-12.md", notifyTaskMD(h, "n-12", "open", "one-stage", []string{"done"}, nil))
	require.Eventually(t, func() bool { return len(rec.all()) > 0 }, 5*time.Second, 20*time.Millisecond)

	// The API removes the entry itself, so the watcher event that follows finds
	// nothing at that path and never reaches Forget.
	require.NoError(t, d.DeleteTicket("n-12"))
	assert.Contains(t, rec.forgotten(), "n-12")
}

// TestWarnUnmatchedNotify covers the three ways a ticket asks for a
// notification and hears nothing, all of which are silent at delivery time.
func TestWarnUnmatchedNotify(t *testing.T) {
	tests := []struct {
		name     string
		notify   []string
		channels []string
		deflt    []string
		wantIn   []string
	}{
		{
			name:   "a status nothing reaches",
			notify: []string{"finished"},
			deflt:  []string{"tg"},
			wantIn: []string{"status nothing reaches", "finished"},
		},
		{
			name:     "a channel nothing answers to",
			notify:   []string{"done"},
			channels: []string{"tpyo"},
			deflt:    []string{"tg"},
			wantIn:   []string{"channel that is not configured", "tpyo"},
		},
		{
			// A configured channel, a ticket that asks, and no route between
			// them: the likeliest way to set this up and hear nothing.
			name:   "no channel at all",
			notify: []string{"done"},
			wantIn: []string{"resolves to no channel"},
		},
		{
			name:     "an explicit opt-out is not a mistake",
			notify:   []string{"done"},
			channels: []string{config.NoneSentinel},
		},
		{name: "a routed ticket", notify: []string{"done"}, deflt: []string{"tg"}},
		{name: "a silent ticket", deflt: []string{"tg"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := notifyConfig(h)
			cfg.Notifications.Default = tt.deflt
			logs := &strings.Builder{}
			d := h.newDaemon(cfg, WithNotifier(&notifyRecorder{}),
				WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))))

			path := h.writeTicket("n-13.md", notifyTaskMD(h, "n-13", "open", "one-stage", tt.notify, tt.channels))
			tkt, err := ticket.ParseFile(path)
			require.NoError(t, err)

			d.warnUnmatchedNotifyLocked(cfg, tkt)
			if tt.wantIn == nil {
				assert.Empty(t, logs.String())
				return
			}
			for _, want := range tt.wantIn {
				assert.Contains(t, logs.String(), want)
			}

			// A watcher event per keystroke would otherwise repeat the warning
			// for a ticket nobody changed.
			before := logs.Len()
			d.warnUnmatchedNotifyLocked(cfg, tkt)
			assert.Equal(t, before, logs.Len(), "the same warning must not repeat for an unchanged ticket")
		})
	}
}

// TestWarnUnmatchedNotifyOnALaterEdit covers the ticket written after the
// daemon started: it used to run to completion, send nothing and log nothing
// until the next restart.
func TestWarnUnmatchedNotifyOnALaterEdit(t *testing.T) {
	h := newHarness(t)
	cfg := notifyConfig(h)
	cfg.AutoPickUp = new(false)
	logs := &syncBuilder{}
	d := h.newDaemon(cfg, WithNotifier(&notifyRecorder{}),
		WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))))
	runDaemon(t, d)

	h.writeTicket("n-14.md", notifyTaskMD(h, "n-14", "open", "one-stage", []string{"finished"}, nil))
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "status nothing reaches")
	}, 5*time.Second, 20*time.Millisecond)
}

// syncBuilder is a log sink a test can read while the daemon is writing to it.
type syncBuilder struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuilder) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuilder) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSpawnFieldsAreNotWrittenOverALateStatusChange covers the window between
// pickup and the spawn write. The ticket in hand is the node read at pickup, so
// writing it back after a pause landed would put in_progress over the user's
// status and report a transition nobody made.
func TestSpawnFieldsAreNotWrittenOverALateStatusChange(t *testing.T) {
	h := newHarness(t)
	rec := &notifyRecorder{}
	d := h.newDaemon(notifyConfig(h), WithNotifier(rec))

	path := h.writeTicket("n-15.md", notifyTaskMD(h, "n-15", "in_progress", "one-stage", []string{"in_progress"}, nil))
	tkt, err := ticket.ParseFile(path)
	require.NoError(t, err)
	d.tickets["n-15"] = newTicketState(tkt, path)

	// What PauseTicket does while the run is still inside worktree.Create: it
	// writes the file from its own re-read and cancels the run.
	paused, err := ticket.ParseFile(path)
	require.NoError(t, err)
	require.NoError(t, paused.SetField("status", "paused"))
	require.NoError(t, d.writeTicket(paused, path, notify.OriginRequest))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, ok := d.prepareWorktreeForAgent(ctx, prepareWorktreeParams{
		cfg: d.config(), log: testLogger(t), t: tkt, filePath: path,
		ticketID: "n-15", stageName: "step1", agentName: "agent1",
		branch: "kontora/n-15", isPipeline: true,
	})
	assert.False(t, ok)

	assert.Equal(t, ticket.StatusPaused, h.readTask("n-15.md").Status, "the pause survives")
	assert.Empty(t, rec.sends(t), "no transition to report")
}

// The web API is the second way a notify: field is written, after a hand edit.
// One table over both endpoints, because they share validateNotifyFields and
// app.SetNotifyFields and only differ in which one the request reaches.
func TestDaemon_NotifyFieldsThroughTheAPI(t *testing.T) {
	tests := []struct {
		name string
		// Exactly one of the two runs.
		update   *web.UpdateTicketRequest
		init     *web.InitTicketRequest
		wantErr  error
		want     []string
		wantChan []string
	}{
		{
			name:     "update writes both fields",
			update:   &web.UpdateTicketRequest{Notify: []string{"paused", "human_review", "waiting"}, NotifyChannels: []string{"tg"}},
			want:     []string{"paused", "human_review", "waiting"},
			wantChan: []string{"tg"},
		},
		{
			name:   "an empty list removes the field",
			update: &web.UpdateTicketRequest{Notify: []string{}, NotifyChannels: []string{}},
		},
		{
			name:     "a request that says nothing leaves the fields alone",
			update:   &web.UpdateTicketRequest{Agent: new("agent2")},
			want:     []string{"done"},
			wantChan: []string{"tg"},
		},
		{
			name:    "a status nothing reaches is refused",
			update:  &web.UpdateTicketRequest{Notify: []string{"nearly_done"}},
			wantErr: web.ErrInvalidNotify,
		},
		{
			name:    "a channel nothing answers to is refused",
			update:  &web.UpdateTicketRequest{NotifyChannels: []string{"slack"}},
			wantErr: web.ErrInvalidNotify,
		},
		{
			name:    "none beside a channel is refused",
			update:  &web.UpdateTicketRequest{NotifyChannels: []string{"none", "tg"}},
			wantErr: web.ErrInvalidNotify,
		},
		{
			name:     "none alone silences the ticket",
			update:   &web.UpdateTicketRequest{NotifyChannels: []string{"none"}},
			want:     []string{"done"},
			wantChan: []string{"none"},
		},
		{
			name:     "the pseudo-status waiting is accepted",
			update:   &web.UpdateTicketRequest{Notify: []string{"waiting"}},
			want:     []string{"waiting"},
			wantChan: []string{"tg"},
		},
		{
			name:     "init writes what the modal picked",
			init:     &web.InitTicketRequest{Pipeline: "one-stage", Status: "open", Notify: []string{"human_review"}, NotifyChannels: []string{"tg"}},
			want:     []string{"human_review"},
			wantChan: []string{"tg"},
		},
		{
			name: "init clears what the ticket carried",
			init: &web.InitTicketRequest{Pipeline: "one-stage", Status: "open", Notify: []string{}, NotifyChannels: []string{}},
		},
		{
			name:    "init refuses an unconfigured channel too",
			init:    &web.InitTicketRequest{Pipeline: "one-stage", Status: "open", NotifyChannels: []string{"slack"}},
			wantErr: web.ErrInvalidNotify,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Notifications.Channels = map[string]config.NotifyChannel{"tg": {Type: config.NotifyTelegram}}
			d := h.newDaemon(h.cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			h.writeTicket("n-api.md", fmt.Sprintf(`---
id: n-api
kontora: true
status: open
pipeline: one-stage
path: %s
created: 2026-01-01T00:00:00Z
notify: [done]
notify_channels: [tg]
---
# Ticket with notify fields
`, h.repoDir))

			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			var err error
			if tc.update != nil {
				err = d.UpdateTicket("n-api", *tc.update)
			} else {
				req := *tc.init
				req.Path = h.repoDir
				err = d.InitTicket("n-api", req)
			}
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				got := h.readTask("n-api.md")
				assert.Equal(t, tc.want, got.Notify.Statuses)
				assert.Equal(t, tc.wantChan, got.NotifyChannels)
			}

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}
