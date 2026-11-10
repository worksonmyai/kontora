package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSSEConnectionNotificationsPrecedeEvents(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := opens.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: ticket_updated\ndata: {\"id\":\"subject\",\"attempt\":%d}\n\n", n)
		w.(http.Flusher).Flush()
		if n > 1 {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := New(srv.URL, "").Subscribe(ctx)
	for _, want := range []string{SSEConnected, "ticket_updated", SSEConnected, "ticket_updated"} {
		select {
		case event := <-events:
			require.Equal(t, want, event.Type)
			if want == SSEConnected {
				require.Empty(t, event.Ticket.ID)
			} else {
				require.Equal(t, "subject", event.Ticket.ID)
				require.Equal(t, int(opens.Load()), event.Ticket.Attempt)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing stream notification or event")
		}
	}
}

func TestSSEBackpressureCancelsWithoutDroppingBufferedEvents(t *testing.T) {
	sent := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 256 {
			_, _ = fmt.Fprintf(w, "event: ticket_updated\ndata: {\"id\":\"subject\",\"attempt\":%d}\n\n", i)
		}
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := New(srv.URL, "").Subscribe(ctx)
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("server did not send burst")
	}
	require.Eventually(t, func() bool { return len(events) == cap(events) }, time.Second, time.Millisecond)
	select {
	case event := <-events:
		require.Equal(t, SSEConnected, event.Type)
	case <-time.After(time.Second):
		t.Fatal("missing connection notification")
	}
	for i := range 128 {
		select {
		case event := <-events:
			require.Equal(t, i, event.Ticket.Attempt)
		case <-time.After(time.Second):
			t.Fatal("client dropped a ticket event")
		}
	}
	require.Eventually(t, func() bool { return len(events) == cap(events) }, time.Second, time.Millisecond)
	cancel()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-time.After(time.Second):
			t.Fatal("blocked stream delivery ignored cancellation")
		}
	}
}
