package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/web"
)

const (
	transcriptCacheEntries = 32
	transcriptCacheBytes   = 32 << 20
)

type cachedTranscript struct {
	tape  logfmt.Tape
	bytes int
	used  uint64
}
type transcriptFlight struct {
	done    chan struct{}
	tape    logfmt.Tape
	version string
	err     error
}

// transcriptCache accounts retained strings, slices and structs, not process heap.
// Its lock is independent of d.mu and is never held during parsing or waits.
type transcriptCache struct {
	mu      sync.Mutex
	entries map[string]*cachedTranscript
	flights map[string]*transcriptFlight
	bytes   int
	clock   uint64
	parse   func(io.Reader, bool) (logfmt.Tape, error)
}

func parseTranscript(r io.Reader, isPi bool) (logfmt.Tape, error) {
	if isPi {
		return logfmt.EventsPi(r)
	}
	return logfmt.Events(r)
}

func (c *transcriptCache) read(path string, isPi bool) (logfmt.Tape, string, error) {
	return c.readConditional(path, isPi, nil)
}

// unchanged must include all response state. An accepted stable version returns no tape.
func (c *transcriptCache) readConditional(path string, isPi bool, unchanged func(string) bool) (logfmt.Tape, string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return logfmt.Tape{}, "", err
	}
	// Open even on a hit: an inaccessible current file must not validate old data.
	f, err := os.Open(path)
	if err != nil {
		return logfmt.Tape{}, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return logfmt.Tape{}, "", err
	}
	key := fmt.Sprintf("%s|pi=%t|schema=%d", session.FileVersion(path, st), isPi, logfmt.TapeVersion)
	validator := web.ContentETag([]byte(key), "")
	if unchanged != nil && unchanged(validator) && transcriptStable(path, f, st) {
		return logfmt.Tape{}, validator, nil
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*cachedTranscript)
		c.flights = make(map[string]*transcriptFlight)
	}
	if e := c.entries[key]; e != nil {
		c.clock++
		e.used = c.clock
		tape := e.tape
		c.mu.Unlock()
		if !transcriptStable(path, f, st) {
			validator = ""
		}
		return cloneTape(tape, false), validator, nil
	}
	if flight := c.flights[key]; flight != nil {
		c.mu.Unlock()
		<-flight.done
		if flight.err != nil {
			return logfmt.Tape{}, "", flight.err
		}
		validator = flight.version
		if !transcriptStable(path, f, st) {
			validator = ""
		}
		return cloneTape(flight.tape, false), validator, nil
	}
	flight := &transcriptFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()
	parse := c.parse
	if parse == nil {
		parse = parseTranscript
	}
	tape, err := parse(f, isPi)
	if err == nil {
		tape = cloneTape(tape, true)
	}
	// A live writer can append after a successful parse. Return that snapshot,
	// but do not cache it or validate it as the version observed before parsing.
	stable := err == nil && transcriptStable(path, f, st)
	if !stable {
		validator = ""
	}
	c.mu.Lock()
	if stable {
		cost := tapeBytes(tape) + len(key) + int(unsafe.Sizeof(cachedTranscript{}))
		if cost <= transcriptCacheBytes {
			for len(c.entries) >= transcriptCacheEntries || c.bytes+cost > transcriptCacheBytes {
				var oldest string
				age := ^uint64(0)
				for k, e := range c.entries {
					if e.used < age {
						oldest = k
						age = e.used
					}
				}
				c.bytes -= c.entries[oldest].bytes
				delete(c.entries, oldest)
			}
			c.clock++
			c.entries[key] = &cachedTranscript{tape: tape, bytes: cost, used: c.clock}
			c.bytes += cost
		}
	}
	flight.tape, flight.version, flight.err = tape, validator, err
	delete(c.flights, key)
	close(flight.done)
	c.mu.Unlock()
	if err != nil {
		return logfmt.Tape{}, "", err
	}
	return cloneTape(tape, false), validator, nil
}

func transcriptStable(path string, f *os.File, before os.FileInfo) bool {
	after, err := f.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(path)
	return err == nil && os.SameFile(before, after) && os.SameFile(before, current) && before.Size() == after.Size() && before.Size() == current.Size() && before.ModTime().Equal(after.ModTime()) && before.ModTime().Equal(current.ModTime())
}

// Copy strings on admission because parser substrings can retain larger records.
// Response copies share only immutable strings with the cached tape.
func cloneTape(t logfmt.Tape, copyStrings bool) logfmt.Tape {
	t.Events = slices.Clone(t.Events)
	t.Partial = slices.Clone(t.Partial)
	copyString := func(s string) string {
		if copyStrings {
			return strings.Clone(s)
		}
		return s
	}
	t.Agent = copyString(t.Agent)
	t.Model = copyString(t.Model)
	for i, s := range t.Partial {
		t.Partial[i] = copyString(s)
	}
	for i := range t.Events {
		e := &t.Events[i]
		if e.Time != nil {
			e.Time = new(*e.Time)
		}
		if e.Usage != nil {
			e.Usage = new(*e.Usage)
		}
		e.Kind = copyString(e.Kind)
		e.Model = copyString(e.Model)
		e.Text = copyString(e.Text)
		e.ID = copyString(e.ID)
		e.Tool = copyString(e.Tool)
		e.Arg = copyString(e.Arg)
		e.Summary = copyString(e.Summary)
		e.Result = copyString(e.Result)
	}
	return t
}

func tapeBytes(t logfmt.Tape) int {
	n := int(unsafe.Sizeof(t)) + len(t.Agent) + len(t.Model) + cap(t.Partial)*int(unsafe.Sizeof("")) + cap(t.Events)*int(unsafe.Sizeof(logfmt.Event{}))
	for _, s := range t.Partial {
		n += len(s)
	}
	for _, e := range t.Events {
		n += len(e.Kind) + len(e.Model) + len(e.Text) + len(e.ID) + len(e.Tool) + len(e.Arg) + len(e.Summary) + len(e.Result)
		if e.Time != nil {
			n += int(unsafe.Sizeof(*e.Time))
		}
		if e.Usage != nil {
			n += int(unsafe.Sizeof(*e.Usage))
		}
	}
	return n
}
