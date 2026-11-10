package notify

import (
	"context"
	"fmt"
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

func runApplyDispatcher(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.started
	}, time.Second, time.Millisecond)
}

func TestNotificationApplyAddsChannelsAndKeepsLastSeen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	d := New(Options{})
	runApplyDispatcher(t, d)
	assert.False(t, d.Live())
	d.Observe(Observation{Origin: OriginObserved, ID: "kon-a", Status: "todo"})
	ch := &captureChannel{name: "hook", url: srv.URL}
	d.Apply(Options{Channels: []Channel{ch}})
	assert.True(t, d.Live())
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: []string{"done"}, Channels: []string{"hook"}})
	events := waitForEvents(t, ch, 1)
	require.Len(t, events, 1)
	assert.Equal(t, "todo", events[0].From)

	d.Apply(Options{})
	assert.False(t, d.Live())
	d.Observe(Observation{Origin: OriginObserved, ID: "kon-a", Status: "paused"})
	d.Apply(Options{Channels: []Channel{ch}})
	d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: "done", Want: []string{"done"}, Channels: []string{"hook"}})
	events = waitForEvents(t, ch, 2)
	require.Len(t, events, 2)
	assert.Equal(t, "paused", events[1].From)
}

func TestNotificationApplyReplacesCredentialsWithoutReplacingQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	old := &blockingChannel{url: srv.URL, release: make(chan struct{})}
	defer close(old.release)
	d := New(Options{Channels: []Channel{old}, Queue: 2})
	runApplyDispatcher(t, d)
	d.Waiting("first", []string{StatusWaiting}, []string{"tg"}, Fields{})
	require.Eventually(t, func() bool { return old.calls.Load() == 1 }, time.Second, time.Millisecond)
	d.Waiting("queued", []string{StatusWaiting}, []string{"tg"}, Fields{})

	d.mu.Lock()
	queue := d.workers["tg"].queue
	d.mu.Unlock()
	replacement := &captureChannel{name: "tg", url: srv.URL}
	d.Apply(Options{Channels: []Channel{replacement}, Queue: 20})
	d.mu.Lock()
	assert.Equal(t, queue, d.workers["tg"].queue)
	assert.Equal(t, 2, cap(d.workers["tg"].queue))
	d.mu.Unlock()

	old.release <- struct{}{}
	events := waitForEvents(t, replacement, 1)
	require.Len(t, events, 1)
	assert.Equal(t, "queued", events[0].TicketID)
	assert.Equal(t, int64(1), old.calls.Load())
}

func TestNotificationApplyRemovesQueuedEventsWithReloadReason(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			defer srv.Close()
			old := &blockingChannel{url: srv.URL, release: make(chan struct{})}
			defer close(old.release)
			logs := &syncBuffer{}
			var drops atomic.Int64
			d := New(Options{
				Channels: []Channel{old}, Queue: 2,
				Log: slog.New(slog.NewTextHandler(logs, nil)),
				OnResult: func(_, result string) {
					if result == ResultDropped {
						drops.Add(1)
					}
				},
			})
			if running {
				runApplyDispatcher(t, d)
				d.Waiting("in-flight", []string{StatusWaiting}, []string{"tg"}, Fields{})
				require.Eventually(t, func() bool { return old.calls.Load() == 1 }, time.Second, time.Millisecond)
			}
			d.Waiting("queued-a", []string{StatusWaiting}, []string{"tg"}, Fields{})
			d.Waiting("queued-b", []string{StatusWaiting}, []string{"tg"}, Fields{})
			d.Apply(Options{})
			if running {
				old.release <- struct{}{}
			}
			require.Eventually(t, func() bool { return drops.Load() == 2 }, time.Second, time.Millisecond)
			assert.Contains(t, logs.String(), "reason=reload")
			assert.Contains(t, logs.String(), "count=2")

			ch := &captureChannel{name: "tg", url: srv.URL}
			d.Apply(Options{Channels: []Channel{ch}})
			if !running {
				runApplyDispatcher(t, d)
			}
			d.Waiting("new", []string{StatusWaiting}, []string{"tg"}, Fields{})
			events := waitForEvents(t, ch, 1)
			require.Len(t, events, 1)
			assert.Equal(t, "new", events[0].TicketID)
		})
	}
}

func TestNotificationApplyChangesAttemptsForNextDelivery(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ch := &captureChannel{name: "hook", url: srv.URL}
	results := make(chan string, 4)
	d := New(Options{
		Channels: []Channel{ch}, Attempts: 1, Backoff: time.Millisecond,
		OnResult: func(_, result string) { results <- result },
	})
	runApplyDispatcher(t, d)
	for _, attempts := range []int{1, 3} {
		d.Apply(Options{Channels: []Channel{ch}, Attempts: attempts, Backoff: time.Millisecond})
		before := requests.Load()
		d.Waiting("kon-a", []string{StatusWaiting}, []string{"hook"}, Fields{})
		select {
		case result := <-results:
			assert.Equal(t, ResultFailed, result)
		case <-time.After(2 * time.Second):
			t.Fatal("no delivery result")
		}
		assert.Equal(t, int64(attempts), requests.Load()-before)
	}
}

type applyChannel struct {
	name    string
	request func(context.Context, Event) (*http.Request, error)
}

func (c applyChannel) Name() string { return c.name }
func (c applyChannel) Request(ctx context.Context, e Event) (*http.Request, error) {
	return c.request(ctx, e)
}

func TestNotificationApplySettingsSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	type attemptRecord struct {
		id      string
		timeout time.Duration
		at      time.Time
	}
	records := make(chan attemptRecord, 4)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	ch := applyChannel{name: "hook", request: func(ctx context.Context, e Event) (*http.Request, error) {
		deadline, _ := ctx.Deadline()
		records <- attemptRecord{id: e.TicketID, timeout: time.Until(deadline), at: time.Now()}
		if e.TicketID == "old" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return post(ctx, http.MethodPost, srv.URL, nil, nil)
	}}
	results := make(chan string, 4)
	d := New(Options{
		Channels: []Channel{ch}, Attempts: 1, Backoff: time.Second, Timeout: 2 * time.Second,
		OnResult: func(_, result string) { results <- result },
	})
	runApplyDispatcher(t, d)
	d.Waiting("old", []string{StatusWaiting}, []string{"hook"}, Fields{})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first delivery did not start")
	}
	d.Waiting("new", []string{StatusWaiting}, []string{"hook"}, Fields{})
	d.Apply(Options{Channels: []Channel{ch}, Attempts: 2, Backoff: time.Millisecond, Timeout: 200 * time.Millisecond})
	release <- struct{}{}
	for range 2 {
		select {
		case result := <-results:
			assert.Equal(t, ResultFailed, result)
		case <-time.After(3 * time.Second):
			t.Fatal("no delivery result")
		}
	}
	require.Len(t, records, 3)
	old, first, second := <-records, <-records, <-records
	assert.Equal(t, "old", old.id)
	assert.Greater(t, old.timeout, time.Second)
	for _, record := range []attemptRecord{first, second} {
		assert.Equal(t, "new", record.id)
		assert.Positive(t, record.timeout)
		assert.LessOrEqual(t, record.timeout, 200*time.Millisecond)
	}
	assert.Less(t, second.at.Sub(first.at), time.Second, "the new delivery must not use the old backoff")
}

func TestNotificationApplyConcurrentObserveAndShutdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	for range 10 {
		d := New(Options{})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); d.Run(ctx) }()
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				for j := range 50 {
					if i%2 == 0 {
						channels := []Channel{&captureChannel{name: "hook", url: srv.URL}}
						if j%3 == 0 {
							channels = nil
						}
						d.Apply(Options{Channels: channels, Attempts: j%3 + 1, Backoff: time.Millisecond, Timeout: time.Second})
					} else {
						d.Observe(Observation{Origin: OriginDaemon, ID: "kon-a", Status: fmt.Sprint(j), Want: []string{fmt.Sprint(j)}, Channels: []string{"hook"}})
						d.Waiting("kon-a", []string{StatusWaiting}, []string{"hook"}, Fields{})
						d.Forget("kon-a")
					}
					if j == 25 {
						cancel()
					}
				}
			})
		}
		wg.Wait()
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("dispatcher did not stop")
		}
		d.Apply(Options{Channels: []Channel{&captureChannel{name: "hook", url: srv.URL}}})
		assert.False(t, d.Live())
	}
}
