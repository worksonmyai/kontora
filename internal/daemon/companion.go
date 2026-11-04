package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/worksonmyai/kontora/internal/companion"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

type companionState struct {
	mu          sync.Mutex
	state       companion.State
	store       *companion.Store
	initialized bool
	err         error
	wake        chan struct{}
}

func newCompanionState() *companionState { return &companionState{wake: make(chan struct{}, 1)} }

func (d *Daemon) companionDir() string {
	cfg := d.config()
	return filepath.Join(expandTilde(cfg.LogsDir), "companion", companion.Scope(expandTilde(d.lockPath), expandTilde(cfg.TicketsDir)))
}

func (d *Daemon) wakeCompanion() {
	if d.companion == nil {
		return
	}
	select {
	case d.companion.wake <- struct{}{}:
	default:
	}
}

func (d *Daemon) startCompanion() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.reconcileCompanion(true)
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			case <-d.companion.wake:
			}
			d.reconcileCompanion(false)
		}
	}()
	return func() {
		cancel()
		<-done
		c := d.companion
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.store != nil {
			_ = c.store.Close()
			c.store = nil
		}
	}
}

type companionTicket struct {
	ID           string
	Identity     string
	Title        string
	Body         string
	Status       ticket.Status
	Stage        string
	StartedAt    string
	CompletedAt  string
	History      []companionRun
	LastError    string
	Summary      string
	FinalSummary string
	Waiting      waitingState
	Live         string
	Revision     string
}

type companionRun struct {
	Stage, StartedAt, CompletedAt, Summary string
	ExitCode                               int
	Run                                    int
}

func companionTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (d *Daemon) companionTickets() []companionTicket {
	d.mu.Lock()
	out := make([]companionTicket, 0, len(d.tickets))
	for _, ts := range d.tickets {
		t := ts.ticket
		if t.IsPassiveEpic() {
			continue
		}
		ct := companionTicket{ID: t.ID, Identity: t.ID + ":" + companionTime(t.Created), Title: t.Title(), Body: t.Body, Status: t.Status, Stage: t.Stage, StartedAt: companionTime(t.StartedAt), CompletedAt: companionTime(t.CompletedAt), LastError: t.LastError, Summary: t.Summary, FinalSummary: t.FinalSummary, Waiting: d.waiting[t.ID]}
		for _, h := range t.History {
			ct.History = append(ct.History, companionRun{Stage: h.Stage, StartedAt: companionTime(h.StartedAt), CompletedAt: companionTime(h.CompletedAt), Summary: h.Summary, ExitCode: h.ExitCode, Run: h.Run})
		}
		if lr, ok := d.liveRuns[t.ID]; ok {
			ct.Live = fmt.Sprintf("%s:%d:%s", lr.stage, lr.run, lr.startedAt.UTC().Format(time.RFC3339Nano))
		}
		out = append(out, ct)
	}
	d.mu.Unlock()
	for i := range out {
		data, _ := json.Marshal(out[i])
		hash := sha256.Sum256(data)
		out[i].Revision = hex.EncodeToString(hash[:])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func companionObservations(tickets []companionTicket) []companion.Observation {
	var out []companion.Observation
	for _, t := range tickets {
		base := companion.Observation{TicketID: t.ID, Revision: t.Revision, Title: t.Title}
		if t.Status == ticket.StatusPaused || t.Status == ticket.StatusHumanReview {
			o := base
			o.Condition = string(t.Status)
			o.Source = t.Identity + ":" + o.Condition + ":" + t.Stage + ":" + t.StartedAt
			o.Detail = t.LastError
			out = append(out, o)
		}
		if w := t.Waiting; w.Tool != "" {
			o := base
			o.Condition = "question"
			o.Detail = w.Question
			anchor := w.ToolCallID
			if anchor == "" {
				anchor = w.RawStartedAt
			}
			if anchor == "" {
				anchor = w.Tool + ":" + w.Question
				o.IdentityLimited = true
			}
			o.Source = t.Identity + ":question:" + w.Invocation + ":" + anchor
			out = append(out, o)
		}
		for i, h := range t.History {
			if h.CompletedAt == "" {
				continue
			}
			o := base
			o.Condition = "completion"
			o.OccurredAt, _ = time.Parse(time.RFC3339Nano, h.CompletedAt)
			o.Source = fmt.Sprintf("%s:run:%d:%s:%s", t.Identity, i, h.Stage, h.CompletedAt)
			o.Detail = fmt.Sprintf("%s exited %d. %s", h.Stage, h.ExitCode, h.Summary)
			data, _ := json.Marshal(h)
			hash := sha256.Sum256(data)
			o.Revision = hex.EncodeToString(hash[:])
			out = append(out, o)
		}
		if t.Status == ticket.StatusDone && t.CompletedAt != "" {
			o := base
			o.Condition = "completion"
			o.OccurredAt, _ = time.Parse(time.RFC3339Nano, t.CompletedAt)
			o.Source = t.Identity + ":done:" + t.CompletedAt
			o.Detail = t.FinalSummary
			if o.Detail == "" {
				o.Detail = t.Summary
			}
			hash := sha256.Sum256([]byte(o.Source + o.Detail))
			o.Revision = hex.EncodeToString(hash[:])
			out = append(out, o)
		}
	}
	return out
}

func (d *Daemon) reconcileCompanion(startup bool) {
	c := d.companion
	if !d.config().Assistant.Companion.Enabled {
		return
	}
	// Source locks never overlap the store lock or disk I/O.
	observations := companionObservations(d.companionTickets())
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized {
		c.initialized = true
		startup = true
		c.store, c.err = companion.Open(d.companionDir())
		if c.err == nil {
			c.state, c.err = c.store.Load()
		}
		if c.err != nil {
			d.log.Warn("companion storage unavailable", "err", c.err)
		}
	}
	observations = companionOrderHistory(observations)
	changed := c.state.Reconcile(observations, time.Now().UTC(), startup)
	if !changed || c.err != nil {
		return
	}
	if err := c.store.Save(c.state); err != nil {
		c.err = err
		d.log.Warn("saving companion attention failed", "err", err)
		return
	}
	d.broadcastCompanion()
}

func companionOrderHistory(observations []companion.Observation) []companion.Observation {
	out := make([]companion.Observation, 0, len(observations))
	var history []companion.Observation
	for _, o := range observations {
		if o.Condition == "completion" {
			history = append(history, o)
		} else {
			out = append(out, o)
		}
	}
	slices.SortFunc(history, func(a, b companion.Observation) int { return b.OccurredAt.Compare(a.OccurredAt) })
	return append(out, history...)
}

func (d *Daemon) broadcastCompanion() {
	if d.broker != nil {
		d.broker.Broadcast(web.TicketEvent{Type: "companion_updated"})
	}
}

func (d *Daemon) companionInfoLocked() web.CompanionInfo {
	c := d.companion
	cfg := d.config()
	name := cfg.Assistant.Companion.Name
	if name == "" {
		name = "Kotik"
	}
	info := web.CompanionInfo{Enabled: cfg.Assistant.Companion.Enabled, Name: name, Health: "ok", Revision: c.state.Revision, CaughtUp: c.state.CaughtUp, ObservedAt: c.state.ObservedAt, Items: slices.Clone(c.state.Items)}
	if info.Items == nil {
		info.Items = []companion.Item{}
	}
	ai := d.AssistantConfig()
	info.AIAvailable = ai.Enabled
	info.Hint = ai.Hint
	if c.err != nil {
		info.Health = "degraded"
		info.Error = c.err.Error()
	}
	return info
}

func (d *Daemon) CompanionSnapshot() web.CompanionInfo {
	c := d.companion
	c.mu.Lock()
	defer c.mu.Unlock()
	return d.companionInfoLocked()
}

func (d *Daemon) mutateCompanion(fn func(*companion.State) error) (web.CompanionInfo, error) {
	c := d.companion
	c.mu.Lock()
	defer c.mu.Unlock()
	if !d.config().Assistant.Companion.Enabled {
		return web.CompanionInfo{}, fmt.Errorf("%w: companion observation is disabled", web.ErrInvalidState)
	}
	if c.err != nil || c.store == nil {
		return web.CompanionInfo{}, companion.ErrStorageUnavailable
	}
	next := c.state
	next.Items = slices.Clone(next.Items)
	if err := fn(&next); err != nil {
		return web.CompanionInfo{}, fmt.Errorf("%w: %s", web.ErrInvalidState, err)
	}
	if next.Revision == c.state.Revision {
		return d.companionInfoLocked(), nil
	}
	if err := c.store.Save(next); err != nil {
		c.err = err
		d.log.Warn("saving companion attention failed", "err", err)
		return web.CompanionInfo{}, companion.ErrStorageUnavailable
	}
	c.state = next
	d.broadcastCompanion()
	return d.companionInfoLocked(), nil
}

func (d *Daemon) ReadCompanionItem(id string, revision uint64, read bool) (web.CompanionInfo, error) {
	return d.mutateCompanion(func(s *companion.State) error { return s.SetRead(id, revision, read) })
}
func (d *Daemon) CatchUpCompanion(revision uint64) (web.CompanionInfo, error) {
	return d.mutateCompanion(func(s *companion.State) error { return s.CatchUp(revision) })
}
