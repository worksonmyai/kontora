package web

import "sync"

// SSEBroker manages Server-Sent Events subscriptions and broadcasting.
// Subscribers receive TicketEvents on a buffered channel. When a client's
// channel is full, individual events are dropped (non-blocking send).
type SSEBroker struct {
	mu      sync.Mutex
	clients map[chan TicketEvent]struct{}
}

func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		clients: make(map[chan TicketEvent]struct{}),
	}
}

// Subscribe returns a channel that receives broadcast events and an
// unsubscribe function. The caller must call unsubscribe when done.
func (b *SSEBroker) Subscribe() (<-chan TicketEvent, func()) {
	ch := make(chan TicketEvent, 64)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.clients[ch]; ok {
			delete(b.clients, ch)
			close(ch)
		}
	}
	return ch, unsubscribe
}

// Broadcast sends an event to all subscribed clients. If a client's
// channel is full, the event is dropped for that client (non-blocking).
func (b *SSEBroker) Broadcast(event TicketEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
		}
	}
}
