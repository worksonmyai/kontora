package assistant

import (
	"sync"
	"time"
)

// PartialMax bounds the text one thread accumulates. Past it the tail is
// dropped: the settled message is about to replace it anyway.
const PartialMax = 64 << 10

// partialTTL is how long a finished thread's text is kept. Start prunes, so
// the map is bounded without a goroutine.
const partialTTL = time.Hour

// Partial is one thread's live text as a reader sees it.
type Partial struct {
	// Gen is bumped when a new text block starts, so a reader replaces rather
	// than appends. Zero means no block has started.
	Gen  int
	Text string
	Tool string
	// Sealed stops the caret, and lets the daemon match the text loosely
	// against the tape.
	Sealed bool
	// Truncated is set when the text hit PartialMax.
	Truncated bool
	Running   bool
	// Present is false for a thread with no buffer at all.
	Present bool
}

type livePartial struct {
	gen       int
	text      []byte
	tool      string
	sealed    bool
	truncated bool
	running   bool
	// suppressed is the generation the session file already records. A snapshot
	// at it reports nothing, so the words are never rendered twice.
	suppressed int
	touched    time.Time
}

// LiveText holds what each thread's agent is typing. Daemon memory only: a
// logfmt.Tape is the parsed form of a file, and no file carries this yet.
type LiveText struct {
	mu      sync.Mutex
	threads map[string]*livePartial
	now     func() time.Time
}

func NewLiveText() *LiveText {
	return &LiveText{threads: make(map[string]*livePartial), now: time.Now}
}

// Start opens a buffer and drops the last turn's. It is the only method that
// creates an entry, so a delta after a delete cannot bring the thread back.
func (l *LiveText) Start(threadID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for id, e := range l.threads {
		if !e.running && now.Sub(e.touched) > partialTTL {
			delete(l.threads, id)
		}
	}
	l.threads[threadID] = &livePartial{running: true, touched: now}
}

// Block starts a new text block, dropping the text it replaces.
func (l *LiveText) Block(threadID string) {
	l.with(threadID, func(e *livePartial) {
		e.gen++
		e.text = e.text[:0]
		e.tool = ""
		e.sealed = false
		e.truncated = false
	})
}

// Append adds one delta, capped at PartialMax.
func (l *LiveText) Append(threadID, delta string) {
	if delta == "" {
		return
	}
	l.with(threadID, func(e *livePartial) {
		if e.gen == 0 {
			// A delta with no content_block_start before it is still a block.
			e.gen = 1
		}
		e.sealed = false
		room := PartialMax - len(e.text)
		if room <= 0 {
			e.truncated = true
			return
		}
		if len(delta) > room {
			delta = delta[:room]
			e.truncated = true
		}
		e.text = append(e.text, delta...)
	})
}

// Tool names the call whose arguments are being generated.
func (l *LiveText) Tool(threadID, name string) {
	l.with(threadID, func(e *livePartial) { e.tool = name })
}

func (l *LiveText) Seal(threadID string) {
	l.with(threadID, func(e *livePartial) { e.sealed = true })
}

// End keeps the text: a turn that died before writing its session file leaves
// this as the only record of it. The pending tool goes, since it never lands.
func (l *LiveText) End(threadID string) {
	l.with(threadID, func(e *livePartial) {
		e.running = false
		e.sealed = true
		e.tool = ""
	})
}

// Suppress stops reporting a generation the session file now carries. A stale
// one is ignored: the agent has moved on to a block the tape does not have.
func (l *LiveText) Suppress(threadID string, gen int) {
	l.with(threadID, func(e *livePartial) {
		if gen != 0 && e.gen == gen {
			e.suppressed = gen
		}
	})
}

func (l *LiveText) Clear(threadID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.threads, threadID)
}

// Snapshot copies out what the thread holds. The generation survives
// suppression, so the poll and the ETag agree on which block it was.
func (l *LiveText) Snapshot(threadID string) Partial {
	if l == nil {
		return Partial{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.threads[threadID]
	if !ok {
		return Partial{}
	}
	p := Partial{
		Gen:       e.gen,
		Sealed:    e.sealed,
		Truncated: e.truncated,
		Running:   e.running,
		Present:   true,
	}
	if e.suppressed != 0 && e.suppressed == e.gen {
		return p
	}
	p.Text = string(e.text)
	p.Tool = e.tool
	return p
}

// with runs fn against an existing entry. A thread with no buffer is a no-op.
func (l *LiveText) with(threadID string, fn func(*livePartial)) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.threads[threadID]
	if !ok {
		return
	}
	fn(e)
	e.touched = l.now()
}
