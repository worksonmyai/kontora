package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

type eventFlushWriter struct {
	*httptest.ResponseRecorder
	onFlush func()
}

func (w eventFlushWriter) Flush() {
	w.ResponseRecorder.Flush()
	w.onFlush()
}

func TestHandleSSE_SubscribeBeforeHeaders(t *testing.T) {
	broker := NewSSEBroker()
	server := &Server{broker: broker}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	flushes := 0
	writer := eventFlushWriter{ResponseRecorder: httptest.NewRecorder(), onFlush: func() {
		flushes++
		if flushes == 1 {
			broker.mu.Lock()
			subscribers := len(broker.clients)
			broker.mu.Unlock()
			require.Equal(t, 1, subscribers)
			broker.Broadcast(TicketEvent{Type: "ticket_deleted", Ticket: TicketInfo{ID: "gone"}})
		} else {
			cancel()
		}
	}}
	server.handleSSE(writer, httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx))
	require.Equal(t, 2, flushes)
	require.Contains(t, writer.Body.String(), "event: ticket_deleted")
	require.Contains(t, writer.Body.String(), `"id":"gone"`)
}
