// Package notify delivers ticket status notifications to chat channels. It is
// a leaf package: it takes primitives, never internal/config or the daemon, and
// its Dispatcher does no I/O on the path the daemon calls.
package notify

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Origin says what caused the write a ticket was observed after. It is a
// required parameter on every daemon write so a new write site cannot compile
// without its author stating what caused it, and Origin(0) is invalid so a
// hand-built value fails loudly.
type Origin int

const (
	// OriginDaemon is a decision the daemon made on its own. Without the
	// all_changes switch, only this origin may send.
	OriginDaemon Origin = iota + 1
	// OriginRequest is a caller asking, through the API or the remote CLI.
	OriginRequest
	// OriginObserved is a ticket read off disk: the startup scan or a watcher event.
	OriginObserved
	// OriginDerived is a status the daemon computed from other tickets rather
	// than decided, as it does for an epic. It seeds lastSeen so a later real
	// change still diffs against the right status, and never sends: the change
	// the person cares about is the child's, which sent on its own.
	OriginDerived
)

// StatusWaiting is the pseudo-status a ticket names in notify: to be told when
// its agent blocks on a question. It is not a ticket status and never enters
// the remembered-status map.
const StatusWaiting = "waiting"

// Delivery outcomes reported to Options.OnResult.
const (
	ResultOK      = "ok"
	ResultFailed  = "failed"
	ResultDropped = "dropped"
)

// ChannelUnknown stands in for a channel name in a metric attribute when no
// configured channel answers to it. Ticket frontmatter is the source of those
// names, so reporting them as written would let a typo add a metric series.
const ChannelUnknown = "unknown"

// Fields is what a message renders from. It is a snapshot: the caller's ticket
// is mutated in place by the next pickup, so a reference would render whatever
// the ticket became by the time the worker got to it.
type Fields struct {
	Title, Stage, Branch, RepoPath, Project string
	Summary, LastError                      string
	Question                                string // StatusWaiting only
}

// Observation is one look at a ticket after a write or a file event.
type Observation struct {
	AllChanges bool
	Origin     Origin
	ID         string
	Status     string   // the status now on disk
	Want       []string // the ticket's notify: list, nil for a silent ticket
	Channels   []string // resolved by the daemon; empty sends nothing
	Fields     Fields
}

// Event is one notification worth sending.
type Event struct {
	TicketID string
	From     string // "" when the previous status was never seen
	To       string
	At       time.Time
	Fields   Fields
}

// Channel turns an Event into one HTTP request. Building the request per
// attempt rather than once keeps a retry from re-reading a consumed body.
type Channel interface {
	Name() string
	Request(ctx context.Context, e Event) (*http.Request, error)
}

// Options configures a Dispatcher. Every duration and count is expected to
// arrive already defaulted by the config layer; zero values fall back here so a
// hand-built Dispatcher in a test still behaves.
type Options struct {
	Channels []Channel
	Attempts int
	Backoff  time.Duration
	Timeout  time.Duration // per attempt
	Queue    int           // per channel
	Client   *http.Client
	Log      *slog.Logger
	// OnResult reports every finished delivery, for metrics. result is one of
	// ResultOK, ResultFailed or ResultDropped.
	OnResult func(channel, result string)
}

// Dispatcher remembers the last status it saw per ticket, decides whether a new
// observation is worth sending, and hands the send to a per-channel worker.
//
// Delivery is best effort. A send that fails every attempt, and a job that
// arrives when a channel's queue is full, are logged and dropped: there is no
// retry queue and nothing is persisted, so nobody should expect at-least-once.
type Dispatcher struct {
	mu       sync.Mutex
	lastSeen map[string]string

	// workers holds one queue per channel, each drained by its own worker. A
	// single shared worker would let one blackholed host, which costs attempts
	// times timeout plus backoff, hold up every other channel behind it.
	workers  map[string]*channelWorker
	runCtx   context.Context
	started  bool
	stopped  bool
	wg       sync.WaitGroup
	attempts int
	backoff  time.Duration
	timeout  time.Duration
	client   *http.Client
	log      *slog.Logger
	onResult func(channel, result string)

	// missing records the channel names already warned about, so an
	// unconfigured name in a ticket does not warn on every transition.
	missing map[string]bool
}

type channelWorker struct {
	name    string
	channel Channel
	queue   chan Event
	ctx     context.Context
	cancel  context.CancelCauseFunc
	// dropped holds what enqueue could not fit. The worker reports it: enqueue
	// runs under the daemon lock, where the log write and the metric record
	// belong to nobody's critical section. Guarded by Dispatcher.mu.
	dropped *dropRecord
}

var (
	errReload   = errors.New("reload")
	errShutdown = errors.New("shutdown")
)

type deliverySettings struct {
	attempts int
	backoff  time.Duration
	timeout  time.Duration
}

// maxDropDetail caps how many dropped notifications are remembered by ticket
// and status. Past it only the count is kept: the log line is the only trace a
// drop leaves, and the first few name what went missing without letting a
// wedged channel grow the list without limit.
const maxDropDetail = 32

type dropRecord struct {
	events []Event
	extra  int
}

const (
	defaultAttempts = 3
	defaultBackoff  = time.Second
	defaultTimeout  = 10 * time.Second
	defaultQueue    = 64
)

func New(opts Options) *Dispatcher {
	d := &Dispatcher{
		lastSeen: make(map[string]string),
		workers:  make(map[string]*channelWorker),
		missing:  make(map[string]bool),
		client:   opts.Client,
		log:      opts.Log,
		onResult: opts.OnResult,
	}
	if d.client == nil {
		d.client = &http.Client{}
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	d.Apply(opts)
	return d
}

// Apply replaces channels and delivery settings while keeping ticket statuses
// and queues for retained channel names. Existing deliveries keep their settings.
// Queue sets the capacity of added channels only. Client, Log and OnResult
// remain fixed by New.
func (d *Dispatcher) Apply(opts Options) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.attempts = cmp.Or(opts.Attempts, defaultAttempts)
	d.backoff = cmp.Or(opts.Backoff, defaultBackoff)
	d.timeout = cmp.Or(opts.Timeout, defaultTimeout)
	keep := make(map[string]bool, len(opts.Channels))
	for _, ch := range opts.Channels {
		name := ch.Name()
		keep[name] = true
		if w := d.workers[name]; w != nil {
			w.channel = ch
			continue
		}
		w := &channelWorker{name: name, channel: ch, queue: make(chan Event, cmp.Or(opts.Queue, defaultQueue))}
		d.workers[name] = w
		delete(d.missing, name)
		if d.started {
			d.startWorkerLocked(w)
		}
	}
	var removed []*channelWorker
	for name, w := range d.workers {
		if !keep[name] {
			delete(d.workers, name)
			removed = append(removed, w)
		}
	}
	d.mu.Unlock()
	// A worker drains under d.mu, so cancellation must happen outside it.
	for _, w := range removed {
		if w.cancel != nil {
			w.cancel(errReload)
		} else {
			d.drain(w, errReload)
		}
	}
}

func (d *Dispatcher) Live() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.stopped && len(d.workers) > 0
}

// Wants reports whether a ticket's notify: list names status.
func Wants(want []string, status string) bool {
	return slices.Contains(want, status)
}

// ShouldSend is the whole send rule: the origin is eligible, the ticket was
// seen at a different status before, it asked about this one, and it resolved
// to a channel. Request and observed changes are eligible only with AllChanges;
// derived changes never are. It is exported so a caller that records observations
// can ask the dispatcher's question instead of restating it.
func ShouldSend(obs Observation, prev string, seen bool) bool {
	switch obs.Origin {
	case OriginDaemon:
	case OriginRequest, OriginObserved:
		if !obs.AllChanges {
			return false
		}
	case OriginDerived:
		return false
	default:
		return false
	}
	// A ticket seen for the first time seeds and sends nothing, so a missed
	// seed costs one notification rather than a burst on startup.
	if !seen || prev == obs.Status {
		return false
	}
	return Wants(obs.Want, obs.Status) && len(obs.Channels) > 0
}

// Observe records the status a ticket now carries and sends a notification when
// ShouldSend says so. It does no I/O and takes no lock but its own, so a caller
// holding the daemon lock can call it: the lock order is caller's lock then
// this one, one direction only.
func (d *Dispatcher) Observe(obs Observation) {
	if d == nil {
		return
	}
	d.mu.Lock()
	prev, seen := d.lastSeen[obs.ID]
	d.lastSeen[obs.ID] = obs.Status
	d.mu.Unlock()

	switch obs.Origin {
	case OriginDaemon, OriginRequest, OriginObserved, OriginDerived:
	default:
		d.log.Warn("notification skipped: write site states no origin",
			"ticket", obs.ID, "status", obs.Status)
		return
	}
	if !ShouldSend(obs, prev, seen) {
		return
	}
	d.enqueue(Event{
		TicketID: obs.ID,
		// From is advisory. Concurrent unlocked writes to one ticket can
		// reorder it, so nothing may decide anything on it.
		From:   prev,
		To:     obs.Status,
		At:     time.Now(),
		Fields: obs.Fields,
	}, obs.Channels)
}

// Waiting sends the pseudo-status notification for an agent that just blocked
// on a question. It deliberately leaves lastSeen alone: recording "waiting"
// there would make the next real status write diff against a status the ticket
// never held.
func (d *Dispatcher) Waiting(id string, want, channels []string, f Fields) {
	if d == nil {
		return
	}
	if !Wants(want, StatusWaiting) || len(channels) == 0 {
		return
	}
	d.enqueue(Event{TicketID: id, To: StatusWaiting, At: time.Now(), Fields: f}, channels)
}

// Forget drops a deleted ticket's remembered status.
func (d *Dispatcher) Forget(id string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	delete(d.lastSeen, id)
	d.mu.Unlock()
}

// enqueue never blocks and writes nothing. Observe runs under the daemon lock
// on the editTicket, pause and annotation paths, and applyWaitMarker holds it
// for its whole body, so a blocking send or a log write here would wedge the
// scheduler, the web API and the metrics collect path.
//
// A name repeated in a channel list is sent to once: the list is concatenated
// from a ticket, its project and the default, and the same channel twice is a
// duplicate message, not two.
func (d *Dispatcher) enqueue(e Event, channels []string) {
	for i, name := range channels {
		if slices.Index(channels, name) != i {
			continue
		}
		d.mu.Lock()
		w := d.workers[name]
		stopped := d.stopped
		if w != nil && !stopped {
			select {
			case w.queue <- e:
			default:
				d.noteDroppedLocked(w, e)
			}
		}
		d.mu.Unlock()
		if stopped {
			d.result(name, ResultDropped)
		} else if w == nil {
			d.warnMissing(name, e.TicketID)
			d.result(ChannelUnknown, ResultDropped)
		}
	}
}

// Run delivers queued notifications until ctx ends, one worker per channel. It
// returns once every worker has stopped.
func (d *Dispatcher) Run(ctx context.Context) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.started || d.stopped {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.runCtx = ctx
	for _, w := range d.workers {
		d.startWorkerLocked(w)
	}
	d.mu.Unlock()
	<-ctx.Done()

	d.mu.Lock()
	d.stopped = true
	var workers []*channelWorker
	for _, w := range d.workers {
		workers = append(workers, w)
	}
	d.mu.Unlock()
	for _, w := range workers {
		w.cancel(errShutdown)
	}
	// stopped prevents Apply from adding workers while Wait is active.
	d.wg.Wait()
}

func (d *Dispatcher) startWorkerLocked(w *channelWorker) {
	w.ctx, w.cancel = context.WithCancelCause(d.runCtx)
	d.wg.Go(func() { d.serve(w) })
}

func (d *Dispatcher) serve(w *channelWorker) {
	for {
		if w.ctx.Err() != nil {
			d.drain(w, d.stopReason(w))
			return
		}
		select {
		case <-w.ctx.Done():
			d.drain(w, d.stopReason(w))
			return
		case e := <-w.queue:
			d.mu.Lock()
			ch := w.channel
			active := d.workers[ch.Name()] == w
			settings := deliverySettings{d.attempts, d.backoff, d.timeout}
			d.mu.Unlock()
			if !active || w.ctx.Err() != nil {
				d.drain(w, d.stopReason(w), e)
				return
			}
			d.deliver(w.ctx, ch, e, settings)
			d.reportDropped(w)
		}
	}
}

func (d *Dispatcher) stopReason(w *channelWorker) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.workers[w.name] != w {
		return errReload
	}
	return errShutdown
}

// Removed workers cannot receive new events, so draining needs no queue close.
// A worker includes an event it dequeued after removal in taken.
func (d *Dispatcher) drain(w *channelWorker, reason error, taken ...Event) {
	name := w.name
	left := len(taken)
	for {
		select {
		case <-w.queue:
			left++
		default:
			d.reportDropped(w)
			if left > 0 {
				d.log.Warn("notifications dropped at "+reason.Error(), "channel", name, "count", left, "reason", reason)
				for range left {
					d.result(name, ResultDropped)
				}
			}
			return
		}
	}
}

func (d *Dispatcher) noteDroppedLocked(w *channelWorker, e Event) {
	rec := w.dropped
	if rec == nil {
		rec = &dropRecord{}
		w.dropped = rec
	}
	if len(rec.events) < maxDropDetail {
		rec.events = append(rec.events, e)
		return
	}
	rec.extra++
}

// reportDropped logs and counts what enqueue could not fit since the last call.
func (d *Dispatcher) reportDropped(w *channelWorker) {
	d.mu.Lock()
	name := w.name
	rec := w.dropped
	w.dropped = nil
	d.mu.Unlock()
	if rec == nil {
		return
	}
	for _, e := range rec.events {
		d.log.Warn("notification dropped, queue full",
			"channel", name, "ticket", e.TicketID, "status", e.To)
		d.result(name, ResultDropped)
	}
	if rec.extra > 0 {
		d.log.Warn("notifications dropped, queue full", "channel", name, "count", rec.extra)
		for range rec.extra {
			d.result(name, ResultDropped)
		}
	}
}

func (d *Dispatcher) warnMissing(name, ticketID string) {
	d.mu.Lock()
	first := !d.missing[name]
	d.missing[name] = true
	d.mu.Unlock()
	if first {
		d.log.Warn("notification channel is not configured, skipping",
			"channel", name, "ticket", ticketID)
	}
}

func (d *Dispatcher) result(channel, result string) {
	if d.onResult != nil {
		d.onResult(channel, result)
	}
}
