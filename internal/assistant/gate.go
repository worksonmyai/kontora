package assistant

import (
	"sync"
	"time"
)

// Call is one tool call the gate intercepted.
type Call struct {
	ThreadID string   `json:"thread_id"`
	Tool     string   `json:"tool"`
	Arg      string   `json:"arg,omitempty"`
	Kind     Decision `json:"kind"`
}

// Pending is a parked call as the UI reads it.
type Pending struct {
	ID string `json:"id"`
	Call
	AskedAt time.Time `json:"asked_at"`
}

// parked is one call waiting on a person. done carries the answer exactly once
// and is closed after, so a second Resolve, or a Resolve racing the timeout,
// cannot write to a channel nobody is reading.
type parked struct {
	pending Pending
	done    chan bool
	once    sync.Once
}

// Gate holds the write-classified tool calls waiting on a person. The daemon
// keeps one; each parked call blocks the agent's tool boundary until the UI
// answers or the wait times out.
type Gate struct {
	mu      sync.Mutex
	pending map[string]*parked
	timeout time.Duration
	nextID  func() string
}

// NewGate returns a gate whose parked calls are denied after timeout. A
// non-positive timeout leaves them parked until the turn ends.
func NewGate(timeout time.Duration) *Gate {
	return &Gate{
		pending: make(map[string]*parked),
		timeout: timeout,
		nextID:  NewID,
	}
}

// Park holds a call and returns its id and a channel carrying the answer: true
// to run the call, false to refuse it. The channel always yields, so the caller
// blocked on it is never stranded.
func (g *Gate) Park(c Call) (string, <-chan bool) {
	p := &parked{
		pending: Pending{ID: g.nextID(), Call: c, AskedAt: time.Now()},
		done:    make(chan bool, 1),
	}
	g.mu.Lock()
	g.pending[p.pending.ID] = p
	g.mu.Unlock()

	if g.timeout > 0 {
		// A person who never answers is a refusal, not an open subprocess. The
		// timer is not held: answering removes the entry, so a timer that fires
		// after an answer names nothing and does nothing.
		time.AfterFunc(g.timeout, func() { g.answer(p.pending.ID, false) })
	}
	return p.pending.ID, p.done
}

// Resolve answers a parked call. It reports false when the id names nothing,
// which is what a second click on an already-answered card sends.
func (g *Gate) Resolve(id string, approve bool) bool {
	return g.answer(id, approve)
}

// Pending reports the call this thread is waiting on. Only one call is parked
// per thread at a time, because the agent is blocked while it waits.
func (g *Gate) Pending(threadID string) (Pending, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.pending {
		if p.pending.ThreadID == threadID {
			return p.pending, true
		}
	}
	return Pending{}, false
}

// Clear refuses every call still parked for a thread. The daemon calls it when
// a turn ends, so a card left on screen cannot resolve into an agent that is
// no longer running.
func (g *Gate) Clear(threadID string) {
	g.mu.Lock()
	var ids []string
	for id, p := range g.pending {
		if p.pending.ThreadID == threadID {
			ids = append(ids, id)
		}
	}
	g.mu.Unlock()
	for _, id := range ids {
		g.answer(id, false)
	}
}

func (g *Gate) answer(id string, approve bool) bool {
	g.mu.Lock()
	p, ok := g.pending[id]
	if ok {
		delete(g.pending, id)
	}
	g.mu.Unlock()
	if !ok {
		return false
	}
	p.once.Do(func() {
		p.done <- approve
		close(p.done)
	})
	return true
}
