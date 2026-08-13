// Package stats aggregates ticket history into the figures the Stats page
// shows: throughput over time, per-stage and per-agent quality, and per-project
// output.
//
// Compute is pure. It reads no clock, no filesystem and no config, so the
// daemon resolves projects and reads sidecar token counts before calling it and
// tests build their inputs literally.
package stats

import (
	"cmp"
	"path/filepath"
	"slices"
	"time"
)

// Statuses the aggregator treats specially. They arrive as plain strings so
// this package stays a leaf; the daemon passes through whatever the ticket
// carries.
const (
	statusDone      = "done"
	statusArchived  = "archived"
	statusCancelled = "cancelled"
)

const (
	dayFormat  = "2006-01-02"
	hourFormat = "2006-01-02T15:04"
)

// Ticket is one ticket as the aggregator needs it. Project is the name the
// daemon resolved from the config; an empty one falls back to the basename of
// Path.
type Ticket struct {
	ID       string
	Status   string
	Path     string
	Project  string
	Pipeline string
	Created  *time.Time
	// StartedAt is the last stage pickup, not the ticket's start: the daemon
	// rewrites it every time a stage is picked up. Cycle time therefore reads it
	// last, after Created and the first run.
	StartedAt   *time.Time
	CompletedAt *time.Time
	History     []Run
}

// Run is one history entry, carrying the token counts of its sidecar. Usage is
// nil when the run has no sidecar, or when its session file left a record's
// counts unfilled, which is not the same as a run that really cost nothing.
type Run struct {
	Stage    string
	Agent    string
	Model    string
	ExitCode int
	// Run is the zero-based attempt index of this run among the stage runs of
	// the same stage, counted by the caller from the rows before it. It is not
	// the run index that keys the sidecar file, which counts annotation rows too.
	Run         int
	StartedAt   *time.Time
	CompletedAt *time.Time
	// Kind is empty for a pipeline stage run and names the exception
	// otherwise ("annotation"). Only stage runs are counted; every run's
	// tokens are.
	Kind  string
	Usage *Usage
}

// Usage is one run's token count. In is every token the model was fed, so
// CacheCreate and CacheRead are subsets of it rather than additions to it:
// In + Out is the run's total.
type Usage struct{ In, Out, CacheCreate, CacheRead int }

// Options bounds one query. Now carries the zone every day and week boundary is
// cut on, so Compute needs no clock of its own.
type Options struct {
	Now      time.Time
	Days     int
	Project  string // "" = all
	Pipeline string // "" = all
}

type Day struct {
	Date string `json:"date"`
	Runs int    `json:"runs"`
}

// TokenCounts is the four-category token figure a window or a week reports,
// embedded in both so the field names, the wire names and the accumulation are
// declared once.
//
// TokensIn is every token fed to the model, so the two cache figures are
// subsets of it. Summing all four double-counts: the total is
// TokensIn + TokensOut.
type TokenCounts struct {
	TokensIn          int64 `json:"tokens_in"`
	TokensOut         int64 `json:"tokens_out"`
	TokensCacheCreate int64 `json:"tokens_cache_create"`
	TokensCacheRead   int64 `json:"tokens_cache_read"`
}

// add folds one run's usage in. It is the only place Usage's field names are
// mapped onto the wire's.
func (c *TokenCounts) add(u Usage) {
	c.TokensIn += int64(u.In)
	c.TokensOut += int64(u.Out)
	c.TokensCacheCreate += int64(u.CacheCreate)
	c.TokensCacheRead += int64(u.CacheRead)
}

// Throughput bucket units. The series granularity follows the window length, so
// a one-day window is drawn as a day of hours rather than as a single bar.
const (
	UnitHour = "hour"
	UnitDay  = "day"
	UnitWeek = "week"
)

// bucketUnit is the granularity a window of the given length is bucketed at.
func bucketUnit(days int) string {
	switch {
	case days <= 1:
		return UnitHour
	case days <= 7:
		return UnitDay
	default:
		return UnitWeek
	}
}

// Bucket is one bucket of the throughput series, at the granularity Window.Unit
// names. Done and Cancelled count tickets by their completion time; the token
// counts come from the runs started in the bucket.
//
// Start is the bucket's local start — "2006-01-02" for a day or a week,
// "2006-01-02T15:04" for an hour — and is a label rather than an identity: on a
// fall-back DST day two hour buckets carry the same local time. A reader that
// has to tell two buckets apart uses their position in the series.
type Bucket struct {
	Start     string `json:"start"`
	Done      int    `json:"done"`
	Cancelled int    `json:"cancelled"`
	TokenCounts
}

// Stage is one pipeline stage's quality. Share is the stage's percentage of all
// measured stage time, so the shares of a window sum to 100, and TokenShare is
// the same figure over tokens.
//
// TokenRuns is zero when none of the stage's runs recorded usable token counts.
// That is not a stage that cost nothing: an agent whose session format carries
// no counts leaves every run of the stage unmeasured. Annotation runs are left
// out of every token field here, exactly as they are out of the time figures,
// so a window's stage tokens sum to at most its total.
type Stage struct {
	Name       string  `json:"name"`
	P50MS      int64   `json:"p50_ms"`
	P90MS      int64   `json:"p90_ms"`
	Share      float64 `json:"share"`
	Runs       int     `json:"runs"`
	Failed     int     `json:"failed"`
	RetryPct   float64 `json:"retry_pct"`
	Tokens     int64   `json:"tokens"`
	TokensP90  int64   `json:"tokens_p90"`
	TokenShare float64 `json:"token_share"`
	TokenRuns  int     `json:"token_runs"`
}

// Agent is one agent's quality. Every figure counts stage runs alone, token
// counts included, so the tooltip's runs and tokens-per-run multiply out.
// TokensPerRun is null when none of the agent's runs recorded usable token
// counts, so an agent whose sessions all left a count unfilled is not reported
// as free.
type Agent struct {
	Name             string  `json:"name"`
	Model            string  `json:"model,omitempty"`
	Runs             int     `json:"runs"`
	FirstPassPct     float64 `json:"first_pass_pct"`
	MedianMS         int64   `json:"median_ms"`
	TokensPerRun     *int64  `json:"tokens_per_run"`
	RetriesPerTicket float64 `json:"retries_per_ticket"`
}

type Project struct {
	Name          string `json:"name"`
	Done          int    `json:"done"`
	MedianCycleMS int64  `json:"median_cycle_ms"`
}

// Live is the daemon's current capacity. It is not window-dependent and is
// filled by the daemon, not by Compute.
type Live struct {
	Running      int   `json:"running"`
	Slots        int   `json:"slots"`
	Queued       int   `json:"queued"`
	OldestWaitMS int64 `json:"oldest_wait_ms"`
	InReview     int   `json:"in_review"`
	// Busy names the agent in each occupied slot, in no particular order.
	Busy []string `json:"busy,omitempty"`
}

// Totals holds the headline figures. The two delta fields are null when the
// preceding window has nothing to compare against.
type Totals struct {
	Shipped            int     `json:"shipped"`
	ShippedThisWeek    int     `json:"shipped_this_week"`
	Runs               int     `json:"runs"`
	MedianCycleMS      int64   `json:"median_cycle_ms"`
	MedianCycleDeltaMS *int64  `json:"median_cycle_delta_ms"`
	FirstPassPct       float64 `json:"first_pass_pct"`
	TokenCounts
	TokensDeltaPct *float64 `json:"tokens_delta_pct"`
	BusiestDay     string   `json:"busiest_day,omitempty"`
	BusiestDayRuns int      `json:"busiest_day_runs"`
}

// Window reports the slice the figures were taken over. Unit names the
// granularity of the throughput series and Buckets counts it, so a page can
// label the series without re-deriving the rule from Days.
type Window struct {
	Days    int    `json:"days"`
	Unit    string `json:"unit"`
	Buckets int    `json:"buckets"`
	From    string `json:"from"`
	To      string `json:"to"`
}

type Result struct {
	Days     []Day     `json:"days"`
	Buckets  []Bucket  `json:"buckets"`
	Stages   []Stage   `json:"stages"`
	Agents   []Agent   `json:"agents"`
	Projects []Project `json:"projects"`
	Live     Live      `json:"live"`
	Totals   Totals    `json:"totals"`
	Window   Window    `json:"window"`
}

// Compute aggregates tickets over the last opts.Days days ending on the day of
// opts.Now, and compares the headline cycle time and token count against the
// immediately preceding window of the same length.
func Compute(tickets []Ticket, opts Options) Result {
	loc := opts.Now.Location()
	w := windowOf(opts)

	selected := selectTickets(tickets, opts)
	cur := aggregate(selected, w, bucketUnit(max(opts.Days, 1)), loc)
	prevMedian, prevHasCycle, prevTokens := compare(selected, w.prevStart, w.prevEndExcl)

	// Clamped to the window so the figure stays a subset of Shipped when the
	// window starts mid-week.
	thisWeek := startOfWeek(w.end, loc)
	if thisWeek.Before(w.start) {
		thisWeek = w.start
	}
	for _, t := range selected {
		if isShipped(t.Status) && inRange(t.CompletedAt, thisWeek, w.endExcl) {
			cur.totals.ShippedThisWeek++
		}
	}

	if cur.hasCycle && prevHasCycle {
		delta := cur.totals.MedianCycleMS - prevMedian
		cur.totals.MedianCycleDeltaMS = &delta
	}
	if prevTokens > 0 {
		curTokens := cur.totals.TokensIn + cur.totals.TokensOut
		pct := (float64(curTokens-prevTokens) / float64(prevTokens)) * 100
		cur.totals.TokensDeltaPct = &pct
	}

	return Result{
		Days:     cur.days,
		Buckets:  cur.buckets,
		Stages:   cur.stages,
		Agents:   cur.agents,
		Projects: cur.projects,
		Totals:   cur.totals,
		Window: Window{
			Days:    max(opts.Days, 1),
			Unit:    cur.unit,
			Buckets: len(cur.buckets),
			From:    w.start.Format(dayFormat),
			To:      w.end.Format(dayFormat),
		},
	}
}

// window is the pair of day ranges one query reads: the requested one and the
// equal-length one before it, which the headline deltas compare against. Every
// bound is a local midnight; the exclusive ends are carried so no reader has to
// add a day of its own in a zone where that is not always 24 hours.
type window struct {
	start, end, endExcl time.Time
	// prevEndExcl is the start of the requested window: the comparison window
	// ends the day before it.
	prevStart, prevEndExcl time.Time
	// now is carried because the hourly series stops at the current hour rather
	// than drawing the rest of the day as empty.
	now time.Time
}

func windowOf(opts Options) window {
	loc := opts.Now.Location()
	days := max(opts.Days, 1)

	var w window
	w.now = opts.Now
	w.end = startOfDay(opts.Now, loc)
	w.endExcl = addDays(w.end, 1, loc)
	w.start = addDays(w.end, -(days - 1), loc)
	w.prevEndExcl = w.start
	w.prevStart = addDays(addDays(w.start, -1, loc), -(days - 1), loc)
	return w
}

// Since is the earliest instant a query with these options reads, which is the
// start of the comparison window. A run older than this contributes to nothing,
// so a caller that resolves token counts per run can skip it.
func Since(opts Options) time.Time {
	return windowOf(opts).prevStart
}

// Selected reports whether a ticket passes the query's filters, resolving its
// project name the way Compute does. It is exported for callers that resolve
// per-run data before calling Compute: the tickets this drops cost nothing to
// leave unresolved.
func Selected(t Ticket, opts Options) bool {
	if opts.Project != "" && projectName(t) != opts.Project {
		return false
	}
	return opts.Pipeline == "" || t.Pipeline == opts.Pipeline
}

// selectTickets resolves each ticket's project and drops the ones the filters
// exclude. An unknown project or pipeline matches nothing.
func selectTickets(tickets []Ticket, opts Options) []Ticket {
	out := make([]Ticket, 0, len(tickets))
	for _, t := range tickets {
		if !Selected(t, opts) {
			continue
		}
		t.Project = projectName(t)
		out = append(out, t)
	}
	return out
}

func projectName(t Ticket) string {
	if t.Project != "" {
		return t.Project
	}
	if t.Path == "" {
		return ""
	}
	return filepath.Base(t.Path)
}

// windowAgg is one window's worth of aggregation. hasCycle separates "the
// median is zero" from "nothing shipped", which the delta needs.
type windowAgg struct {
	days     []Day
	buckets  []Bucket
	unit     string
	stages   []Stage
	agents   []Agent
	projects []Project
	totals   Totals
	hasCycle bool
}

// compare aggregates the two figures the headline deltas read off the
// preceding window. That window is never drawn, so walking it through the
// per-stage, per-agent and per-project accumulators would be work nothing reads.
func compare(tickets []Ticket, start, endExcl time.Time) (medianCycleMS int64, hasCycle bool, tokens int64) {
	var cycles []int64
	for _, t := range tickets {
		if isShipped(t.Status) && inRange(t.CompletedAt, start, endExcl) {
			cycles = append(cycles, cycleOf(t))
		}
		for _, r := range t.History {
			if r.Usage != nil && inRange(r.StartedAt, start, endExcl) {
				tokens += int64(r.Usage.In) + int64(r.Usage.Out)
			}
		}
	}
	if len(cycles) == 0 {
		return 0, false, tokens
	}
	return percentile(cycles, 50), true, tokens
}

func aggregate(tickets []Ticket, w window, unit string, loc *time.Location) windowAgg {
	agg := windowAgg{unit: unit}
	start, end, endExcl := w.start, w.end, w.endExcl

	dayRuns := map[string]int{}
	buckets := newBucketAcc(w, unit, loc)
	stages := map[string]*stageAcc{}
	agents := map[string]*agentAcc{}
	projects := map[string]*projectAcc{}
	var cycles []int64
	firstPassPairs := map[string]int{} // ticket\x00stage → highest run index

	for _, t := range tickets {
		if inRange(t.CompletedAt, start, endExcl) {
			switch {
			case isShipped(t.Status):
				buckets.done(*t.CompletedAt)
				agg.totals.Shipped++
				cycle := cycleOf(t)
				cycles = append(cycles, cycle)
				proj := projectAccFor(projects, t.Project)
				proj.done++
				proj.cycles = append(proj.cycles, cycle)
			case t.Status == statusCancelled:
				buckets.cancelled(*t.CompletedAt)
			}
		}

		for _, r := range t.History {
			if !inRange(r.StartedAt, start, endExcl) {
				continue
			}
			// The window and bucket token totals count every run. An annotation
			// run is real spend even though it is not a stage, and a page that
			// hid it would report less than the machine cost.
			if r.Usage != nil {
				buckets.tokens(*r.StartedAt, *r.Usage)
				agg.totals.add(*r.Usage)
			}

			// Everything below counts stage runs alone: the heat map, the KPI
			// and both quality tables are labelled that way, and an annotation
			// run rewrites the ticket rather than attempting a stage. The agent
			// table is included, so an agent that only ever rewrote tickets gets
			// no row of zeroes and no agent's tokens-per-run averages over runs
			// its row does not count.
			if r.Kind != "" {
				continue
			}

			agg.totals.Runs++
			dayRuns[r.StartedAt.In(loc).Format(dayFormat)]++

			duration, measured := runDuration(r)
			if r.Agent != "" {
				a := agentAccFor(agents, r.Agent)
				a.runs++
				if r.Model != "" {
					a.models[r.Model]++
				}
				if measured {
					a.durations = append(a.durations, duration)
				}
				if r.Run > 0 {
					a.retries++
				}
				if r.Usage != nil {
					a.tokens += int64(r.Usage.In) + int64(r.Usage.Out)
					a.tokenRuns++
				}
				a.tickets[t.ID] = struct{}{}
				a.pairs[t.ID+"\x00"+r.Stage] = max(a.pairs[t.ID+"\x00"+r.Stage], r.Run)
			}

			s := stageAccFor(stages, r.Stage)
			s.runs++
			if r.ExitCode != 0 {
				s.failed++
			}
			if r.Run > 0 {
				s.retries++
			}
			if measured {
				s.durations = append(s.durations, duration)
				s.totalMS += duration
			}
			if r.Usage != nil {
				n := int64(r.Usage.In) + int64(r.Usage.Out)
				s.tokens += n
				s.tokenPerRun = append(s.tokenPerRun, n)
				s.tokenRuns++
			}
			key := t.ID + "\x00" + r.Stage
			firstPassPairs[key] = max(firstPassPairs[key], r.Run)
		}
	}

	agg.days = daySeries(dayRuns, start, end, loc)
	agg.buckets = buckets.list
	agg.stages = stageSeries(stages)
	agg.agents = agentSeries(agents)
	agg.projects = projectSeries(projects)

	for _, d := range agg.days {
		if d.Runs > agg.totals.BusiestDayRuns {
			agg.totals.BusiestDayRuns = d.Runs
			agg.totals.BusiestDay = d.Date
		}
	}
	if len(cycles) > 0 {
		agg.hasCycle = true
		agg.totals.MedianCycleMS = percentile(cycles, 50)
	}
	if len(firstPassPairs) > 0 {
		clean := 0
		for _, maxRun := range firstPassPairs {
			if maxRun == 0 {
				clean++
			}
		}
		agg.totals.FirstPassPct = pct(clean, len(firstPassPairs))
	}
	return agg
}

type stageAcc struct {
	durations   []int64
	totalMS     int64
	tokens      int64
	tokenPerRun []int64
	tokenRuns   int
	runs        int
	failed      int
	retries     int
}

type agentAcc struct {
	models    map[string]int
	durations []int64
	tokens    int64
	tokenRuns int
	runs      int
	retries   int
	tickets   map[string]struct{}
	pairs     map[string]int
}

type projectAcc struct {
	done   int
	cycles []int64
}

func stageAccFor(m map[string]*stageAcc, name string) *stageAcc {
	if a, ok := m[name]; ok {
		return a
	}
	a := &stageAcc{}
	m[name] = a
	return a
}

func agentAccFor(m map[string]*agentAcc, name string) *agentAcc {
	if a, ok := m[name]; ok {
		return a
	}
	a := &agentAcc{
		models:  map[string]int{},
		tickets: map[string]struct{}{},
		pairs:   map[string]int{},
	}
	m[name] = a
	return a
}

func projectAccFor(m map[string]*projectAcc, name string) *projectAcc {
	if a, ok := m[name]; ok {
		return a
	}
	a := &projectAcc{}
	m[name] = a
	return a
}

// bucketAcc lays the throughput series out up front and hands out the bucket a
// time falls in, so a bucket nothing landed in still draws its empty column.
//
// A bucket is addressed by its offset from the series origin rather than by a
// formatted key. On a day that moves the clock back, two hours of the day share
// a local time, and a key made of that label would fold them into one.
type bucketAcc struct {
	unit   string
	origin time.Time
	loc    *time.Location
	list   []Bucket
}

func newBucketAcc(w window, unit string, loc *time.Location) *bucketAcc {
	b := &bucketAcc{unit: unit, loc: loc, origin: w.start}
	// The hourly series stops at the hour in progress rather than drawing the
	// rest of the day as empty. The weekly one opens on the Sunday before the
	// window, so a window starting mid-week keeps a whole first column.
	var last int
	switch unit {
	case UnitHour:
		last = b.index(w.now)
	case UnitWeek:
		b.origin = startOfWeek(w.start, loc)
		last = b.index(w.end)
	default:
		last = b.index(w.end)
	}
	b.list = make([]Bucket, last+1)
	for i := range b.list {
		b.list[i].Start = b.startOf(i)
	}
	return b
}

// index is the position of the bucket holding t. It is negative before the
// origin and past the end of the series after it, which is what bounds the
// three accessors below.
func (b *bucketAcc) index(t time.Time) int {
	switch b.unit {
	case UnitHour:
		// Elapsed hours, not the local hour of day: a DST change makes those
		// two different counts, and only this one advances once per bucket.
		return int(t.Sub(b.origin) / time.Hour)
	case UnitDay:
		return dayNumber(t, b.loc) - dayNumber(b.origin, b.loc)
	default:
		return (dayNumber(t, b.loc) - dayNumber(b.origin, b.loc)) / 7
	}
}

// startOf writes the local start of bucket i the way its unit reads.
func (b *bucketAcc) startOf(i int) string {
	switch b.unit {
	case UnitHour:
		return b.origin.Add(time.Duration(i) * time.Hour).In(b.loc).Format(hourFormat)
	case UnitDay:
		return addDays(b.origin, i, b.loc).Format(dayFormat)
	default:
		return addDays(b.origin, i*7, b.loc).Format(dayFormat)
	}
}

// at is the bucket t falls in, or nil for a time outside the series. Only a
// timestamp from the future can miss: the window bounds every caller, and the
// hourly series stops at the hour in progress.
func (b *bucketAcc) at(t time.Time) *Bucket {
	i := b.index(t)
	if i < 0 || i >= len(b.list) {
		return nil
	}
	return &b.list[i]
}

func (b *bucketAcc) done(at time.Time) {
	if x := b.at(at); x != nil {
		x.Done++
	}
}

func (b *bucketAcc) cancelled(at time.Time) {
	if x := b.at(at); x != nil {
		x.Cancelled++
	}
}

func (b *bucketAcc) tokens(at time.Time, u Usage) {
	if x := b.at(at); x != nil {
		x.add(u)
	}
}

func daySeries(runs map[string]int, start, end time.Time, loc *time.Location) []Day {
	out := []Day{}
	for d := start; !d.After(end); d = addDays(d, 1, loc) {
		key := d.In(loc).Format(dayFormat)
		out = append(out, Day{Date: key, Runs: runs[key]})
	}
	return out
}

// stageSeries orders the stages by time share alone. The page sorts its own
// rows when it draws the token figures, so this single order is what pins each
// stage to a colour in either mode.
func stageSeries(m map[string]*stageAcc) []Stage {
	var total, totalTokens int64
	for _, a := range m {
		total += a.totalMS
		totalTokens += a.tokens
	}
	out := make([]Stage, 0, len(m))
	for name, a := range m {
		s := Stage{
			Name:      name,
			P50MS:     percentile(a.durations, 50),
			P90MS:     percentile(a.durations, 90),
			Runs:      a.runs,
			Failed:    a.failed,
			RetryPct:  pct(a.retries, a.runs),
			Tokens:    a.tokens,
			TokensP90: percentile(a.tokenPerRun, 90),
			TokenRuns: a.tokenRuns,
		}
		if total > 0 {
			s.Share = float64(a.totalMS) / float64(total) * 100
		}
		if totalTokens > 0 {
			s.TokenShare = float64(a.tokens) / float64(totalTokens) * 100
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Stage) int {
		if c := cmp.Compare(b.Share, a.Share); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}

func agentSeries(m map[string]*agentAcc) []Agent {
	out := make([]Agent, 0, len(m))
	for name, a := range m {
		clean := 0
		for _, maxRun := range a.pairs {
			if maxRun == 0 {
				clean++
			}
		}
		ag := Agent{
			Name:         name,
			Model:        topKey(a.models),
			Runs:         a.runs,
			FirstPassPct: pct(clean, len(a.pairs)),
			MedianMS:     percentile(a.durations, 50),
		}
		if a.tokenRuns > 0 {
			perRun := a.tokens / int64(a.tokenRuns)
			ag.TokensPerRun = &perRun
		}
		if n := len(a.tickets); n > 0 {
			ag.RetriesPerTicket = float64(a.retries) / float64(n)
		}
		out = append(out, ag)
	}
	slices.SortFunc(out, func(a, b Agent) int {
		if c := cmp.Compare(b.Runs, a.Runs); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}

func projectSeries(m map[string]*projectAcc) []Project {
	out := make([]Project, 0, len(m))
	for name, a := range m {
		out = append(out, Project{
			Name:          name,
			Done:          a.done,
			MedianCycleMS: percentile(a.cycles, 50),
		})
	}
	slices.SortFunc(out, func(a, b Project) int {
		if c := cmp.Compare(b.Done, a.Done); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}

// topKey returns the most frequent key, ties broken by name so the result does
// not depend on map order.
func topKey(m map[string]int) string {
	best, bestN := "", 0
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

// percentile is the nearest-rank value at p, with no interpolation. An empty
// input is zero.
func percentile(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	return sorted[min(max(rank, 1), len(sorted))-1]
}

func pct(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

// cycleOf is the time from the ticket being opened to it completing.
func cycleOf(t Ticket) int64 {
	from := openedAt(t)
	if from == nil || t.CompletedAt == nil {
		return 0
	}
	return max(t.CompletedAt.Sub(*from).Milliseconds(), 0)
}

// openedAt is when the ticket's clock started. StartedAt is the last resort:
// the daemon rewrites it on every stage pickup, so on a ticket that ran more
// than one stage it marks the last stage rather than the ticket, and a cycle
// measured from it would leave out every queue and every review it waited
// through.
func openedAt(t Ticket) *time.Time {
	if t.Created != nil {
		return t.Created
	}
	var first *time.Time
	for _, r := range t.History {
		if r.StartedAt != nil && (first == nil || r.StartedAt.Before(*first)) {
			first = r.StartedAt
		}
	}
	if first != nil {
		return first
	}
	return t.StartedAt
}

func runDuration(r Run) (int64, bool) {
	if r.StartedAt == nil || r.CompletedAt == nil {
		return 0, false
	}
	d := r.CompletedAt.Sub(*r.StartedAt).Milliseconds()
	if d < 0 {
		return 0, false
	}
	return d, true
}

func isShipped(status string) bool {
	return status == statusDone || status == statusArchived
}

// inRange reports whether at falls from start up to, but not including,
// endExcl. Both are local midnights.
func inRange(at *time.Time, start, endExcl time.Time) bool {
	return at != nil && !at.Before(start) && at.Before(endExcl)
}

// startOfDay is the first instant of t's local day. It re-anchors when that day
// has no midnight: a zone that moves its clock forward at 00:00 (America/
// Santiago, Africa/Cairo, Asia/Beirut) makes time.Date normalise backward into
// 23:00 of the day before, and every bucket key downstream would be a day out.
func startOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	if start.Day() != d {
		start = time.Date(y, m, d, 1, 0, 0, 0, loc)
	}
	return start
}

// addDays is the first instant of the local day n days from t. The arithmetic
// runs from noon so that a day whose length is not 24 hours cannot carry it
// into a neighbouring day: adding to a midnight would.
func addDays(t time.Time, n int, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	return startOfDay(time.Date(y, m, d+n, 12, 0, 0, 0, loc), loc)
}

// dayNumber counts local days from the epoch. The date is carried through UTC,
// where every day is 24 hours, so the difference between two of these is an
// exact number of days in a zone where subtracting the instants is not.
func dayNumber(t time.Time, loc *time.Location) int {
	y, m, d := t.In(loc).Date()
	return int(time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Unix() / 86400)
}

// startOfWeek is the Sunday on or before t, matching the heat map's columns.
func startOfWeek(t time.Time, loc *time.Location) time.Time {
	d := startOfDay(t, loc)
	return addDays(d, -int(d.Weekday()), loc)
}
