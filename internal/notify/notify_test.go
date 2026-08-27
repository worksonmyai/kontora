package notify

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureChannel records the events handed to it and answers every request from
// a test server the caller controls.
type captureChannel struct {
	name string
	url  string

	mu   sync.Mutex
	sent []Event
}

func (c *captureChannel) Name() string { return c.name }

func (c *captureChannel) Request(ctx context.Context, e Event) (*http.Request, error) {
	c.mu.Lock()
	c.sent = append(c.sent, e)
	c.mu.Unlock()
	return post(ctx, http.MethodPost, c.url, []byte(`{}`), nil)
}

func (c *captureChannel) events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.sent...)
}

// newTestDispatcher returns a running dispatcher whose only channel posts to a
// server that always succeeds, plus that channel.
func newTestDispatcher(t *testing.T, opts ...func(*Options)) (*Dispatcher, *captureChannel) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ch := &captureChannel{name: "tg", url: srv.URL}
	o := Options{Channels: []Channel{ch}, Backoff: time.Millisecond}
	for _, f := range opts {
		f(&o)
	}
	d := New(o)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return d, ch
}

// waitForEvents waits until ch has recorded at least n events, then returns
// them. A dispatcher hands its sends to a worker goroutine, so an assertion
// right after Observe would race the send it is asserting on.
func waitForEvents(t *testing.T, ch *captureChannel, n int) []Event {
	t.Helper()
	require.Eventually(t, func() bool { return len(ch.events()) >= n }, 2*time.Second, 5*time.Millisecond)
	return ch.events()
}

func TestObserveDecidesWhatToSend(t *testing.T) {
	notifyDone := []string{"human_review", "done"}

	tests := []struct {
		name string
		// seed is observed first; a nil seed leaves the ticket unseen.
		seed *Observation
		obs  Observation
		want bool
	}{
		{
			name: "first sight seeds and stays silent",
			obs:  Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: notifyDone, Channels: []string{"tg"}},
		},
		{
			name: "a listed status the daemon decided sends",
			seed: &Observation{Origin: OriginDaemon, ID: "kon-a", Status: "in_progress", Want: notifyDone, Channels: []string{"tg"}},
			obs:  Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: notifyDone, Channels: []string{"tg"}},
			want: true,
		},
		{
			name: "an unlisted status is silent",
			seed: &Observation{Origin: OriginDaemon, ID: "kon-a", Status: "in_progress", Want: notifyDone, Channels: []string{"tg"}},
			obs:  Observation{Origin: OriginDaemon, ID: "kon-a", Status: "paused", Want: notifyDone, Channels: []string{"tg"}},
		},
		{
			name: "a ticket with no notify list is silent",
			seed: &Observation{Origin: OriginDaemon, ID: "kon-a", Status: "in_progress", Channels: []string{"tg"}},
			obs:  Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Channels: []string{"tg"}},
		},
		{
			name: "the same status again is silent",
			seed: &Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: notifyDone, Channels: []string{"tg"}},
			obs:  Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: notifyDone, Channels: []string{"tg"}},
		},
		{
			name: "a requested change seeds and stays silent",
			seed: &Observation{Origin: OriginDaemon, ID: "kon-a", Status: "in_progress", Want: notifyDone, Channels: []string{"tg"}},
			obs:  Observation{Origin: OriginRequest, ID: "kon-a", Status: "done", Want: notifyDone, Channels: []string{"tg"}},
		},
		{
			name: "an observed change seeds and stays silent",
			seed: &Observation{Origin: OriginDaemon, ID: "kon-a", Status: "in_progress", Want: notifyDone, Channels: []string{"tg"}},
			obs:  Observation{Origin: OriginObserved, ID: "kon-a", Status: "done", Want: notifyDone, Channels: []string{"tg"}},
		},
		{
			name: "an unset origin sends nothing",
			seed: &Observation{Origin: OriginDaemon, ID: "kon-a", Status: "in_progress", Want: notifyDone, Channels: []string{"tg"}},
			obs:  Observation{ID: "kon-a", Status: "done", Want: notifyDone, Channels: []string{"tg"}},
		},
		{
			name: "no resolved channel sends nothing",
			seed: &Observation{Origin: OriginDaemon, ID: "kon-a", Status: "in_progress", Want: notifyDone},
			obs:  Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: notifyDone},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, ch := newTestDispatcher(t)
			if tt.seed != nil {
				d.Observe(*tt.seed)
			}
			d.Observe(tt.obs)

			if tt.want {
				events := waitForEvents(t, ch, 1)
				require.Len(t, events, 1)
				assert.Equal(t, tt.obs.Status, events[0].To)
				assert.Equal(t, tt.seed.Status, events[0].From)
				return
			}
			// Nothing should arrive. A send that is on its way would land
			// within this window; the queue-full test covers the blocking case.
			assert.Never(t, func() bool { return len(ch.events()) > 0 }, 100*time.Millisecond, 10*time.Millisecond)
		})
	}
}

func TestObserveSeedsEvenWhenItSendsNothing(t *testing.T) {
	d, ch := newTestDispatcher(t)
	want := []string{"done"}

	d.Observe(Observation{Origin: OriginObserved, ID: "kon-a", Status: "todo", Want: want, Channels: []string{"tg"}})
	// A person set it done through the API: silent, but remembered.
	d.Observe(Observation{Origin: OriginRequest, ID: "kon-a", Status: "done", Want: want, Channels: []string{"tg"}})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: []string{"tg"}})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: []string{"tg"}})

	events := waitForEvents(t, ch, 1)
	require.Len(t, events, 1)
	assert.Equal(t, "todo", events[0].From, "the requested change should have updated the remembered status")
}

func TestForgetDropsTheRememberedStatus(t *testing.T) {
	d, ch := newTestDispatcher(t)
	want := []string{"done"}

	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: []string{"tg"}})
	d.Forget("kon-a")
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: []string{"tg"}})

	assert.Never(t, func() bool { return len(ch.events()) > 0 }, 100*time.Millisecond, 10*time.Millisecond)
}

func TestWaitingFiresOnTheListedPseudoStatus(t *testing.T) {
	tests := []struct {
		name     string
		want     []string
		channels []string
		send     bool
	}{
		{name: "listed", want: []string{StatusWaiting}, channels: []string{"tg"}, send: true},
		{name: "not listed", want: []string{"done"}, channels: []string{"tg"}},
		{name: "no channels", want: []string{StatusWaiting}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, ch := newTestDispatcher(t)
			d.Waiting("kon-a", tt.want, tt.channels, Fields{Question: "which one?"})

			if !tt.send {
				assert.Never(t, func() bool { return len(ch.events()) > 0 }, 100*time.Millisecond, 10*time.Millisecond)
				return
			}
			events := waitForEvents(t, ch, 1)
			assert.Equal(t, StatusWaiting, events[0].To)
			assert.Equal(t, "which one?", events[0].Fields.Question)
		})
	}
}

func TestWaitingStaysOutOfTheRememberedStatus(t *testing.T) {
	d, ch := newTestDispatcher(t)
	want := []string{StatusWaiting, "done"}

	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "in_progress", Want: want, Channels: []string{"tg"}})
	d.Waiting("kon-a", want, []string{"tg"}, Fields{Question: "which one?"})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: []string{"tg"}})

	events := waitForEvents(t, ch, 2)
	require.Len(t, events, 2)
	assert.Equal(t, "in_progress", events[1].From, "the waiting event must not become the remembered status")
}

// blockingChannel holds every request until release is closed, so the queue can
// be filled deterministically.
type blockingChannel struct {
	url     string
	release chan struct{}
	calls   atomic.Int64
}

func (b *blockingChannel) Name() string { return "tg" }

func (b *blockingChannel) Request(ctx context.Context, _ Event) (*http.Request, error) {
	b.calls.Add(1)
	<-b.release
	return post(ctx, http.MethodPost, b.url, []byte(`{}`), nil)
}

func TestObserveDropsRatherThanBlockingOnAFullQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	blocked := &blockingChannel{url: srv.URL, release: make(chan struct{})}
	var mu sync.Mutex
	results := map[string]int{}
	logs := &syncBuffer{}
	d := New(Options{
		Channels: []Channel{blocked},
		Queue:    1,
		Log:      slog.New(slog.NewTextHandler(logs, nil)),
		OnResult: func(_, result string) {
			mu.Lock()
			defer mu.Unlock()
			results[result]++
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	defer func() { cancel(); <-done }()

	want := []string{"done", "paused", "human_review", "todo"}
	obs := func(status string) Observation {
		return Observation{Origin: OriginDaemon, ID: "kon-a", Status: status, Want: want, Channels: []string{"tg"}}
	}
	d.Observe(obs("in_progress"))
	// The worker takes the first job and blocks in the channel, the second
	// fills the one-slot queue, and everything after it must be dropped.
	d.Observe(obs("done"))
	require.Eventually(t, func() bool { return blocked.calls.Load() == 1 }, time.Second, time.Millisecond)
	d.Observe(obs("paused"))

	dropped := make(chan struct{})
	go func() {
		defer close(dropped)
		d.Observe(obs("human_review"))
		d.Observe(obs("todo"))
	}()
	select {
	case <-dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("Observe blocked on a full queue")
	}

	// The drop is accounted for by the worker, not by Observe: Observe runs
	// under the daemon lock, where neither the log write nor the metric belongs.
	close(blocked.release)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return results[ResultDropped] == 2
	}, 5*time.Second, time.Millisecond)

	// The log line is the only trace a dropped notification leaves, so it has
	// to say which ticket and which status went missing.
	assert.Contains(t, logs.String(), "queue full")
	assert.Contains(t, logs.String(), "ticket=kon-a")
	assert.Contains(t, logs.String(), "status=human_review")
	assert.Contains(t, logs.String(), "status=todo")
}

// syncBuffer is a log sink a test can read while the dispatcher's workers are
// still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestDeliverRetryPolicy(t *testing.T) {
	tests := []struct {
		name         string
		statuses     []int // one per attempt; the last repeats
		wantRequests int
		wantResult   string
	}{
		{name: "success on the first attempt", statuses: []int{200}, wantRequests: 1, wantResult: ResultOK},
		{name: "a 5xx is retried then dropped", statuses: []int{503}, wantRequests: 3, wantResult: ResultFailed},
		{name: "a 429 is retried", statuses: []int{429, 429, 200}, wantRequests: 3, wantResult: ResultOK},
		{name: "a transient failure recovers", statuses: []int{500, 200}, wantRequests: 2, wantResult: ResultOK},
		{name: "a 401 is not retried", statuses: []int{401}, wantRequests: 1, wantResult: ResultFailed},
		{name: "a 400 is not retried", statuses: []int{400}, wantRequests: 1, wantResult: ResultFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := int(requests.Add(1)) - 1
				if n >= len(tt.statuses) {
					n = len(tt.statuses) - 1
				}
				w.WriteHeader(tt.statuses[n])
				_, _ = w.Write([]byte(`{"description":"Unauthorized"}`))
			}))
			defer srv.Close()

			ch := &captureChannel{name: "tg", url: srv.URL}
			gotResult := make(chan string, 4)
			d := New(Options{
				Channels: []Channel{ch},
				Backoff:  time.Millisecond,
				OnResult: func(_, result string) { gotResult <- result },
			})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); d.Run(ctx) }()
			defer func() { cancel(); <-done }()

			want := []string{"done"}
			d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: []string{"tg"}})
			d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: []string{"tg"}})

			select {
			case result := <-gotResult:
				assert.Equal(t, tt.wantResult, result)
			case <-time.After(3 * time.Second):
				t.Fatal("no delivery result")
			}
			assert.Equal(t, int64(tt.wantRequests), requests.Load())
		})
	}
}

func TestRunSkipsAChannelTheDispatcherDoesNotHave(t *testing.T) {
	d, ch := newTestDispatcher(t)
	want := []string{"done"}

	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: []string{"gone", "tg"}})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: []string{"gone", "tg"}})

	events := waitForEvents(t, ch, 1)
	assert.Equal(t, "done", events[0].To)
}

func TestWants(t *testing.T) {
	tests := []struct {
		name   string
		want   []string
		status string
		ok     bool
	}{
		{name: "listed", want: []string{"human_review", "done"}, status: "done", ok: true},
		{name: "not listed", want: []string{"done"}, status: "human_review"},
		{name: "empty list", status: "done"},
		{name: "waiting pseudo-status", want: []string{StatusWaiting}, status: StatusWaiting, ok: true},
		{name: "custom status", want: []string{"needs_qa"}, status: "needs_qa", ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.ok, Wants(tt.want, tt.status))
		})
	}
}

func TestObserveIsSafeUnderConcurrentCallers(t *testing.T) {
	d, _ := newTestDispatcher(t)
	want := []string{"done"}

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for range 32 {
				d.Observe(Observation{
					Origin: OriginDaemon, ID: "kon-a", Status: "todo",
					Want: want, Channels: []string{"tg"},
				})
				d.Waiting("kon-a", want, []string{"tg"}, Fields{})
				if i%2 == 0 {
					d.Forget("kon-a")
				}
			}
		})
	}
	wg.Wait()
}

func TestNilDispatcherIsInert(t *testing.T) {
	var d *Dispatcher
	assert.NotPanics(t, func() {
		d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done"})
		d.Waiting("kon-a", []string{StatusWaiting}, []string{"tg"}, Fields{})
		d.Forget("kon-a")
		d.Run(t.Context())
	})
}

func TestDeliverLogsTheStatusAndBodyOfARejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	ch := &captureChannel{name: "tg", url: srv.URL}
	done := make(chan string, 1)
	d := New(Options{
		Channels: []Channel{ch},
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
		OnResult: func(_, result string) { done <- result },
	})
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.Run(ctx) }()
	defer func() { cancel(); <-stopped }()

	want := []string{"done"}
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: []string{"tg"}})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: []string{"tg"}})

	select {
	case result := <-done:
		require.Equal(t, ResultFailed, result)
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery result")
	}

	logs := buf.String()
	assert.Contains(t, logs, "level=WARN")
	// The status and the start of the body are what say the token is wrong.
	assert.Contains(t, logs, "http 401")
	assert.Contains(t, logs, "Unauthorized")
	assert.Contains(t, logs, "ticket=kon-a")
}

// TestDeliverLogsNoCredentialOnATransportFailure covers the leak the whole
// design is built around: the bot token is in the Telegram URL path and the
// Mattermost webhook URL is itself the credential, and *url.Error prints the
// URL it failed on.
func TestDeliverLogsNoCredentialOnATransportFailure(t *testing.T) {
	const secret = "123456:AAH-SUPER-SECRET"
	tests := []struct {
		name    string
		channel func(base string) Channel
	}{
		{
			name: "telegram",
			channel: func(base string) Channel {
				return &Telegram{ChannelName: "tg", Token: secret, ChatID: "1", BaseURL: base}
			},
		},
		{
			name:    "mattermost",
			channel: func(base string) Channel { return &Mattermost{ChannelName: "tg", URL: base + "/hooks/" + secret} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A port nothing listens on, so every attempt fails in transport.
			logs := &syncBuffer{}
			done := make(chan string, 1)
			d := New(Options{
				Channels: []Channel{tt.channel("http://127.0.0.1:1")},
				Attempts: 1,
				Timeout:  time.Second,
				Log:      slog.New(slog.NewTextHandler(logs, nil)),
				OnResult: func(_, result string) { done <- result },
			})
			ctx, cancel := context.WithCancel(t.Context())
			stopped := make(chan struct{})
			go func() { defer close(stopped); d.Run(ctx) }()
			defer func() { cancel(); <-stopped }()

			want := []string{"done"}
			d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: []string{"tg"}})
			d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: []string{"tg"}})

			select {
			case result := <-done:
				require.Equal(t, ResultFailed, result)
			case <-time.After(5 * time.Second):
				t.Fatal("no delivery result")
			}

			out := logs.String()
			assert.NotContains(t, out, secret, "a log line must not carry the credential")
			assert.NotContains(t, out, "/hooks/", "the path of the URL is where the credential sits")
			assert.NotContains(t, out, "sendMessage")
			// Still says what went wrong, or the redaction has cost the user
			// the only diagnosis they get.
			assert.Contains(t, out, "connection refused")
		})
	}
}

func TestEnqueueSendsOnceToARepeatedChannel(t *testing.T) {
	d, ch := newTestDispatcher(t)
	want := []string{"done"}

	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: []string{"tg", "tg"}})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: []string{"tg", "tg"}})

	events := waitForEvents(t, ch, 1)
	// Given a moment for a second delivery to arrive if one were coming.
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, events, 1)
	assert.Len(t, ch.events(), 1)
}

func TestUnknownChannelIsCountedUnderOneName(t *testing.T) {
	type result struct{ channel, outcome string }
	got := make(chan result, 4)
	d, _ := newTestDispatcher(t, func(o *Options) {
		o.OnResult = func(channel, outcome string) { got <- result{channel, outcome} }
	})

	want := []string{"done"}
	// The name comes out of ticket frontmatter, so reporting it as written
	// would let a typo add a metric series.
	chans := []string{"a-name-nobody-configured"}
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: chans})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: chans})

	select {
	case r := <-got:
		assert.Equal(t, result{ChannelUnknown, ResultDropped}, r)
	case <-time.After(3 * time.Second):
		t.Fatal("an unknown channel was discarded without a result")
	}
}

func TestOneStuckChannelDoesNotHoldUpAnother(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	stuck := &blockingChannel{url: srv.URL, release: make(chan struct{})}
	healthy := &captureChannel{name: "mm", url: srv.URL}
	d := New(Options{Channels: []Channel{stuck, healthy}, Backoff: time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	defer func() { close(stuck.release); cancel(); <-done }()

	want := []string{"done"}
	chans := []string{"tg", "mm"}
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "todo", Want: want, Channels: chans})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: want, Channels: chans})

	events := waitForEvents(t, healthy, 1)
	assert.Equal(t, "done", events[0].To)
	assert.Equal(t, int64(1), stuck.calls.Load(), "the other channel is still in its request")
}

func TestShutdownAccountsForWhatIsStillQueued(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	blocked := &blockingChannel{url: srv.URL, release: make(chan struct{})}
	logs := &syncBuffer{}
	var mu sync.Mutex
	results := map[string]int{}
	d := New(Options{
		Channels: []Channel{blocked},
		Queue:    4,
		Log:      slog.New(slog.NewTextHandler(logs, nil)),
		OnResult: func(_, result string) {
			mu.Lock()
			defer mu.Unlock()
			results[result]++
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()

	want := []string{"done", "paused", "todo"}
	obs := func(status string) Observation {
		return Observation{Origin: OriginDaemon, ID: "kon-a", Status: status, Want: want, Channels: []string{"tg"}}
	}
	d.Observe(obs("in_progress"))
	d.Observe(obs("done"))
	require.Eventually(t, func() bool { return blocked.calls.Load() == 1 }, time.Second, time.Millisecond)
	d.Observe(obs("paused"))
	d.Observe(obs("todo"))

	// Nothing is persisted, so a notification that goes with the daemon has to
	// leave a line and a count behind it.
	cancel()
	close(blocked.release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, results[ResultDropped])
	assert.Contains(t, logs.String(), "dropped at shutdown")
}
