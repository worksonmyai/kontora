package web

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSEBroker_SubscribeReceives(t *testing.T) {
	b := NewSSEBroker()
	ch, unsub := b.Subscribe()
	defer unsub()

	ev := TicketEvent{Type: "ticket_updated", Ticket: TicketInfo{ID: "t-001", Status: "in_progress"}}
	b.Broadcast(ev)

	select {
	case got := <-ch:
		assert.Equal(t, ev, got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSSEBroker_MultipleClients(t *testing.T) {
	b := NewSSEBroker()
	ch1, unsub1 := b.Subscribe()
	defer unsub1()
	ch2, unsub2 := b.Subscribe()
	defer unsub2()

	ev := TicketEvent{Type: "ticket_updated", Ticket: TicketInfo{ID: "t-002"}}
	b.Broadcast(ev)

	for _, ch := range []<-chan TicketEvent{ch1, ch2} {
		select {
		case got := <-ch:
			assert.Equal(t, ev, got)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event")
		}
	}
}

func TestSSEBroker_Unsubscribe(_ *testing.T) {
	b := NewSSEBroker()
	_, unsub := b.Subscribe()
	unsub()

	// Double unsubscribe should not panic.
	unsub()

	// Broadcast after all clients unsubscribed should not panic.
	b.Broadcast(TicketEvent{Type: "ticket_updated"})
}

func TestSSEBroker_SlowClient(t *testing.T) {
	b := NewSSEBroker()
	ch, unsub := b.Subscribe()
	defer unsub()

	// Fill the channel buffer (cap 64).
	for i := range 65 {
		b.Broadcast(TicketEvent{Type: "ticket_updated", Ticket: TicketInfo{ID: "t-slow", Attempt: i}})
	}

	// Should have 64 events buffered, 65th dropped.
	assert.Len(t, ch, 64)
}

func TestSSEBroker_ConcurrentAccess(t *testing.T) {
	b := NewSSEBroker()
	var wg sync.WaitGroup

	// Concurrent subscribers.
	for range 10 {
		wg.Go(func() {
			_, unsub := b.Subscribe()
			// Read a few events then unsubscribe.
			time.Sleep(5 * time.Millisecond)
			unsub()
		})
	}

	// Concurrent broadcasters.
	for i := range 20 {
		wg.Go(func() {
			b.Broadcast(TicketEvent{Type: "ticket_updated", Ticket: TicketInfo{ID: "t-conc", Attempt: i}})
		})
	}

	wg.Wait()

	// If we got here without a race condition or panic, the test passes.
	b.mu.Lock()
	require.Empty(t, b.clients)
	b.mu.Unlock()
}
