package daemon

import (
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/stats"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// statsTTL is how long an aggregated payload is handed out again before the
// history is walked afresh. The page polls every 30s, and several open tabs
// must not each walk every ticket.
const statsTTL = 10 * time.Second

// statsCacheMax bounds the result cache. project and pipeline reach the key
// unvalidated, by design — the chips come from a config the client fetched
// earlier — so the key space belongs to the caller, and any page in the browser
// can mint entries with a no-cors fetch.
const statsCacheMax = 64

// statsCache holds the aggregated payloads and the sidecar token counts they
// were built from. Its own lock, not d.mu: aggregation reads files, and d.mu is
// held across tmux fork/exec and ticket writes.
type statsCache struct {
	mu       sync.Mutex
	results  map[string]statsResult
	sidecars map[string]statsSidecar
}

type statsResult struct {
	at     time.Time
	result web.StatsInfo
}

// statsSidecar is one parsed tape sidecar. A sidecar is written once when a run
// ends and never rewritten, so an unchanged modtime and size mean the cached
// counts still hold and only an os.Stat is needed.
//
// missing records the other answer: a run that has ended and has no sidecar
// never gains one, because the tape is written before the history row that
// names the run. Remembering that keeps an install whose agent writes no tapes
// from re-issuing a guaranteed-ENOENT stat per run on every poll.
//
// The map keeps one entry per run the logs dir holds and is never pruned. That
// is a few dozen bytes per run against a directory the daemon writes itself.
type statsSidecar struct {
	modTime time.Time
	size    int64
	model   string
	usage   *stats.Usage
	missing bool
}

// statsTicket pairs the aggregator's view of a ticket with the sidecar path of
// each of its runs. The paths are keyed by a different index from the run
// numbers the aggregator reads: a sidecar counts every history row of its
// stage, annotation rows included, while stats.Run.Run counts stage attempts
// alone.
type statsTicket struct {
	ticket stats.Ticket
	// paths has one entry per History row. It is empty for a row whose stage
	// name cannot key a file.
	paths []string
}

func newStatsCache() *statsCache {
	return &statsCache{
		results:  map[string]statsResult{},
		sidecars: map[string]statsSidecar{},
	}
}

// GetStats aggregates every tracked ticket, archived ones included, over the
// requested window.
func (d *Daemon) GetStats(q web.StatsQuery) (web.StatsInfo, error) {
	now := time.Now()
	key := fmt.Sprintf("%d\x00%s\x00%s", q.Days, q.Project, q.Pipeline)
	if cached, ok := d.stats.result(key, now); ok {
		return cached, nil
	}

	cfg := d.config()
	opts := stats.Options{
		Now:      now,
		Days:     q.Days,
		Project:  q.Project,
		Pipeline: q.Pipeline,
	}
	snapshot, live := d.statsSnapshot(cfg, now)
	d.stats.fillUsage(snapshot, opts)

	tickets := make([]stats.Ticket, len(snapshot))
	for i := range snapshot {
		tickets[i] = snapshot[i].ticket
	}
	result := stats.Compute(tickets, opts)
	result.Live = live
	d.stats.store(key, now, result)
	return result, nil
}

// statsSnapshot copies every ticket the daemon tracks into the aggregator's
// input, and reads the live capacity figures. It holds d.mu only for the copy:
// the sidecar reads that follow must not run under the scheduler's lock.
//
// Unlike ListTickets this keeps every status. Archived tickets are the bulk of
// any long window, and dropping them would report a fraction of the throughput.
func (d *Daemon) statsSnapshot(cfg *config.Config, now time.Time) ([]statsTicket, stats.Live) {
	out, live := d.statsTicketsLocked(cfg, now)

	// ProjectFor normalises every configured repo path on each call, so it runs
	// outside d.mu and once per distinct ticket path. That lock is the one the
	// scheduler takes to dispatch and the watcher takes on every file change.
	projects := make(map[string]string, 8)
	for i := range out {
		path := out[i].ticket.Path
		name, ok := projects[path]
		if !ok {
			name, _, _ = cfg.ProjectFor(path)
			projects[path] = name
		}
		out[i].ticket.Project = name
	}
	return out, live
}

func (d *Daemon) statsTicketsLocked(cfg *config.Config, now time.Time) ([]statsTicket, stats.Live) {
	d.mu.Lock()
	defer d.mu.Unlock()

	live := stats.Live{
		Running: len(d.running),
		Slots:   cfg.MaxConcurrentAgents,
		Queued:  len(d.queue),
	}
	for _, item := range d.queue {
		if wait := now.Sub(item.enqueuedAt).Milliseconds(); wait > live.OldestWaitMS {
			live.OldestWaitMS = wait
		}
	}

	logsDir := expandTilde(cfg.LogsDir)
	out := make([]statsTicket, 0, len(d.tickets))
	for _, ts := range d.tickets {
		t := ts.ticket
		st := statsTicket{
			ticket: stats.Ticket{
				ID:          t.ID,
				Status:      string(t.Status),
				Path:        t.Path,
				Pipeline:    t.Pipeline,
				Created:     cloneTime(t.Created),
				StartedAt:   cloneTime(t.StartedAt),
				CompletedAt: cloneTime(t.CompletedAt),
				History:     make([]stats.Run, 0, len(t.History)),
			},
			paths: make([]string, 0, len(t.History)),
		}
		// Both indices are counted from the rows before this one, the way the
		// daemon keys a run, rather than read off the row: the run field only
		// landed in 2026-08-08 and every older row carries a zero, which would
		// report the whole history as first-pass.
		files := map[string]int{}    // sidecar key: every row of the stage
		attempts := map[string]int{} // attempt index: stage runs alone
		for _, h := range t.History {
			file := files[h.Stage]
			files[h.Stage] = file + 1
			attempt := attempts[h.Stage]
			if h.Kind == "" {
				attempts[h.Stage] = attempt + 1
			}
			st.ticket.History = append(st.ticket.History, stats.Run{
				Stage:       h.Stage,
				Agent:       h.Agent,
				ExitCode:    h.ExitCode,
				Run:         attempt,
				StartedAt:   cloneTime(h.StartedAt),
				CompletedAt: cloneTime(h.CompletedAt),
				Kind:        h.Kind,
			})
			st.paths = append(st.paths, statsSidecarPath(logsDir, t.ID, h.Stage, file))
		}
		if run, path, ok := simpleTicketRun(cfg, logsDir, t); ok {
			st.ticket.History = append(st.ticket.History, run)
			st.paths = append(st.paths, path)
		}
		out = append(out, st)

		if t.Status == ticket.StatusHumanReview {
			live.InReview++
		}
		if _, running := d.running[t.ID]; running {
			// The agent of the run in flight, not the ticket's: a pipeline stage
			// names its own agent, and a ticket with no agent field of its own
			// would light no row at all.
			agent := d.liveRuns[t.ID].agent
			if agent == "" {
				agent = t.Agent
			}
			if agent != "" {
				live.Busy = append(live.Busy, agent)
			}
		}
	}
	slices.Sort(live.Busy)
	return out, live
}

// simpleTicketRun stands in for the history row a ticket with no pipeline never
// gets: that path never calls pipeline.Evaluate, so nothing appends one, and
// its runs, its duration and the tokens its sidecar holds would all read zero.
// The ticket's own fields describe the run, since the simple path runs an agent
// once per pickup under the "default" stage.
func simpleTicketRun(cfg *config.Config, logsDir string, t *ticket.Ticket) (stats.Run, string, bool) {
	if t.Pipeline != "" || len(t.History) > 0 || t.StartedAt == nil {
		return stats.Run{}, "", false
	}
	agent := t.Agent
	if agent == "" {
		agent = cfg.DefaultAgent
	}
	// The path pauses a ticket when the agent exits non-zero, and records no
	// exit code of its own.
	exit := 0
	if t.Status == ticket.StatusPaused {
		exit = 1
	}
	return stats.Run{
		Stage:       simpleStageName,
		Agent:       agent,
		ExitCode:    exit,
		StartedAt:   cloneTime(t.StartedAt),
		CompletedAt: cloneTime(t.CompletedAt),
	}, statsSidecarPath(logsDir, t.ID, simpleStageName, 0), true
}

// statsSidecarPath is stageEventsPath over an already expanded logs dir. It
// refuses a stage name a hand-edited history could use to reach outside the
// ticket's log directory, whose model string the response would then echo back.
func statsSidecarPath(logsDir, ticketID, stage string, run int) string {
	if !session.SafeStage(stage) {
		return ""
	}
	return session.EventsPath(logsDir, ticketID, stage, run)
}

// fillUsage attaches each run's model and token counts from its sidecar. A
// sidecar that is missing, unreadable or malformed before its events leaves the
// run with no usage and the page keeps rendering: the plaintext log is the
// contract, and one corrupt tape must not blank every figure.
//
// A ticket the filters exclude, and a run that started before the window the
// deltas compare against, are both skipped: no figure on the page reaches them,
// so reading their tapes would be a stat and a parse per archived run of every
// ticket the daemon has ever tracked.
func (c *statsCache) fillUsage(tickets []statsTicket, opts stats.Options) {
	since := stats.Since(opts)
	for ti := range tickets {
		if !stats.Selected(tickets[ti].ticket, opts) {
			continue
		}
		for ri := range tickets[ti].ticket.History {
			run := &tickets[ti].ticket.History[ri]
			if run.StartedAt == nil || run.StartedAt.Before(since) {
				continue
			}
			path := tickets[ti].paths[ri]
			if path == "" {
				continue
			}
			model, usage, ok := c.sidecar(path, run.CompletedAt != nil)
			if !ok {
				continue
			}
			run.Model, run.Usage = model, usage
		}
	}
}

// sidecar reads one tape's metadata, through the cache. ended reports whether
// the run is over, which is what makes an absent sidecar worth remembering.
func (c *statsCache) sidecar(path string, ended bool) (string, *stats.Usage, bool) {
	c.mu.Lock()
	cached, cachedOK := c.sidecars[path]
	c.mu.Unlock()
	if cachedOK && cached.missing {
		return "", nil, false
	}

	info, err := os.Stat(path)
	if err != nil {
		if ended {
			c.storeSidecar(path, statsSidecar{missing: true})
		}
		return "", nil, false
	}
	if cachedOK && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		return cached.model, cached.usage, true
	}

	model, usage, err := stats.SidecarTotals(path)
	if err != nil {
		// A tape that fails to parse fails the same way next time. Remember the
		// empty answer against its size and modtime, like any other read, so one
		// corrupt file is not re-parsed on every poll.
		model, usage = "", nil
	}
	c.storeSidecar(path, statsSidecar{modTime: info.ModTime(), size: info.Size(), model: model, usage: usage})
	return model, usage, true
}

func (c *statsCache) storeSidecar(path string, entry statsSidecar) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sidecars[path] = entry
}

func (c *statsCache) result(key string, now time.Time) (web.StatsInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.results[key]
	if !ok || now.Sub(entry.at) > statsTTL {
		return web.StatsInfo{}, false
	}
	return entry.result, true
}

func (c *statsCache) store(key string, now time.Time, result web.StatsInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, entry := range c.results {
		if now.Sub(entry.at) > statsTTL {
			delete(c.results, k)
		}
	}
	// Everything left is still live, so the oldest of them is the one whose next
	// read would have to recompute the least.
	for len(c.results) >= statsCacheMax {
		oldest, at := "", now
		for k, entry := range c.results {
			if oldest == "" || entry.at.Before(at) {
				oldest, at = k, entry.at
			}
		}
		delete(c.results, oldest)
	}
	c.results[key] = statsResult{at: now, result: result}
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
