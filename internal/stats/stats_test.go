package stats

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A zone west of Greenwich, so a run late in the local evening would land on
// the next UTC day. Every bucket the aggregator cuts must use this zone.
var testLoc = time.FixedZone("test", -5*60*60)

// testNow is a Wednesday, so the Sunday week buckets do not line up with the
// window edges by accident.
var testNow = time.Date(2026, 8, 12, 18, 30, 0, 0, testLoc)

// ago returns the instant `days` before testNow's date, at hour `hour` local.
func ago(days, hour int) *time.Time {
	t := time.Date(2026, 8, 12, hour, 0, 0, 0, testLoc).AddDate(0, 0, -days)
	return &t
}

func mkRun(stage, agent string, idx int, start *time.Time, dur time.Duration) Run {
	end := start.Add(dur)
	return Run{Stage: stage, Agent: agent, Run: idx, StartedAt: start, CompletedAt: &end}
}

func TestCompute(t *testing.T) {
	tests := []struct {
		name    string
		tickets []Ticket
		opts    Options
		check   func(t *testing.T, r Result)
	}{
		{
			name: "35 day window excludes older tickets",
			tickets: []Ticket{
				{ID: "a", Status: "done", Project: "kontora", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
				{ID: "b", Status: "done", Project: "kontora", StartedAt: ago(41, 9), CompletedAt: ago(40, 9)},
				{ID: "c", Status: "done", Project: "kontora", StartedAt: ago(121, 9), CompletedAt: ago(120, 9)},
			},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 1, r.Totals.Shipped)
				require.Len(t, r.Days, 35)
				require.Equal(t, "2026-08-12", r.Window.To)
				require.Equal(t, "2026-07-09", r.Window.From)
				require.Equal(t, 35, r.Window.Days)
			},
		},
		{
			name: "98 day window",
			tickets: []Ticket{
				{ID: "a", Status: "done", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
				{ID: "b", Status: "done", StartedAt: ago(41, 9), CompletedAt: ago(40, 9)},
				{ID: "c", Status: "done", StartedAt: ago(121, 9), CompletedAt: ago(120, 9)},
			},
			opts: Options{Now: testNow, Days: 98},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 2, r.Totals.Shipped)
				require.Len(t, r.Days, 98)
			},
		},
		{
			name: "182 day window",
			tickets: []Ticket{
				{ID: "a", Status: "done", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
				{ID: "b", Status: "done", StartedAt: ago(41, 9), CompletedAt: ago(40, 9)},
				{ID: "c", Status: "done", StartedAt: ago(121, 9), CompletedAt: ago(120, 9)},
			},
			opts: Options{Now: testNow, Days: 182},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 3, r.Totals.Shipped)
				require.Len(t, r.Days, 182)
			},
		},
		{
			name: "shipped this week counts only the current Sunday week",
			tickets: []Ticket{
				// testNow is Wednesday 2026-08-12; the week opened Sunday the 9th.
				{ID: "a", Status: "done", StartedAt: ago(2, 9), CompletedAt: ago(1, 9)},
				{ID: "b", Status: "done", StartedAt: ago(9, 9), CompletedAt: ago(8, 9)},
			},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 2, r.Totals.Shipped)
				require.Equal(t, 1, r.Totals.ShippedThisWeek)
			},
		},
		{
			name: "day buckets straddle a month boundary in the local zone",
			tickets: []Ticket{{
				ID: "a", Status: "done",
				History: []Run{
					// 22:00 local on Jul 31 is 03:00 UTC on Aug 1. It belongs
					// to the day the work happened on.
					mkRun("plan", "claude", 0, ago(12, 22), time.Minute),
					mkRun("plan", "claude", 0, ago(11, 9), time.Minute),
					mkRun("plan", "claude", 0, ago(11, 14), time.Minute),
				},
			}},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				byDate := map[string]int{}
				for _, d := range r.Days {
					byDate[d.Date] = d.Runs
				}
				require.Equal(t, 1, byDate["2026-07-31"])
				require.Equal(t, 2, byDate["2026-08-01"])
				require.Equal(t, 3, r.Totals.Runs)
				require.Equal(t, "2026-08-01", r.Totals.BusiestDay)
				require.Equal(t, 2, r.Totals.BusiestDayRuns)
			},
		},
		{
			name: "archived counts as shipped, cancelled does not",
			tickets: []Ticket{
				{ID: "a", Status: "done", StartedAt: ago(3, 9), CompletedAt: ago(2, 9)},
				{ID: "b", Status: "archived", StartedAt: ago(3, 9), CompletedAt: ago(2, 9)},
				{ID: "c", Status: "cancelled", StartedAt: ago(3, 9), CompletedAt: ago(2, 9)},
			},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 2, r.Totals.Shipped)
				week := weekOf(t, r, "2026-08-09")
				require.Equal(t, 2, week.Done)
				require.Equal(t, 1, week.Cancelled)
			},
		},
		{
			name: "weeks cover the window even where nothing happened",
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				// 2026-07-09 through 2026-08-12: Sundays Jul 5 … Aug 9.
				require.Len(t, r.Weeks, 6)
				require.Equal(t, "2026-07-05", r.Weeks[0].Week)
				require.Equal(t, "2026-08-09", r.Weeks[5].Week)
				require.Equal(t, 6, r.Window.Weeks)
			},
		},
		{
			name: "stage and agent quality use measured runs",
			tickets: []Ticket{
				{ID: "a", Status: "done", History: []Run{
					mkRun("impl", "claude", 0, ago(5, 9), time.Minute),
					mkRun("impl", "claude", 1, ago(5, 11), 10*time.Minute),
				}},
				{ID: "b", Status: "done", History: []Run{
					mkRun("impl", "claude", 0, ago(4, 9), 2*time.Minute),
				}},
				{ID: "c", Status: "done", History: []Run{
					mkRun("impl", "claude", 0, ago(3, 9), 3*time.Minute),
				}},
			},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Len(t, r.Stages, 1)
				require.Equal(t, (2 * time.Minute).Milliseconds(), r.Stages[0].P50MS)
				require.Equal(t, (10 * time.Minute).Milliseconds(), r.Stages[0].P90MS)
				require.Equal(t, 4, r.Stages[0].Runs)
				require.InDelta(t, 25.0, r.Stages[0].RetryPct, 0.01)
				require.InDelta(t, 100.0, r.Stages[0].Share, 0.01)

				require.Len(t, r.Agents, 1)
				require.InDelta(t, 200.0/3, r.Agents[0].FirstPassPct, 0.01)
				require.InDelta(t, 1.0/3, r.Agents[0].RetriesPerTicket, 0.01)
				require.InDelta(t, 200.0/3, r.Totals.FirstPassPct, 0.01)
			},
		},
		{
			name: "an annotation run is not counted but its tokens are",
			tickets: []Ticket{{ID: "a", Status: "done", History: []Run{
				func() Run {
					r := mkRun("impl", "claude", 0, ago(5, 9), time.Minute)
					r.Usage = &Usage{In: 100, Out: 10}
					return r
				}(),
				func() Run {
					r := mkRun("rework", "claude", 0, ago(4, 9), 2*time.Minute)
					r.Kind = "annotation"
					r.Usage = &Usage{In: 300, Out: 30}
					return r
				}(),
			}}},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Len(t, r.Stages, 1)
				require.Equal(t, "impl", r.Stages[0].Name)
				require.Equal(t, 1, r.Totals.Runs)
				require.Equal(t, 1, r.Agents[0].Runs)
				require.Equal(t, int64(400), r.Totals.TokensIn)
				require.Equal(t, int64(40), r.Totals.TokensOut)
				require.Equal(t, int64(110), *r.Agents[0].TokensPerRun, "averaged over the agent's stage runs, the ones its row counts")
			},
		},
		{
			name: "failed runs are counted per stage",
			tickets: []Ticket{{ID: "a", Status: "done", History: []Run{
				mkRun("test", "claude", 0, ago(5, 9), time.Minute),
				func() Run {
					r := mkRun("test", "claude", 1, ago(5, 11), time.Minute)
					r.ExitCode = 1
					return r
				}(),
			}}},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 1, r.Stages[0].Failed)
				require.InDelta(t, 50.0, r.Stages[0].RetryPct, 0.01)
			},
		},
		{
			name: "unavailable usage leaves tokens per run null",
			tickets: []Ticket{
				{ID: "a", Status: "done", History: []Run{
					func() Run {
						r := mkRun("impl", "claude", 0, ago(5, 9), time.Minute)
						r.Model = "sonnet-4.6"
						r.Usage = &Usage{In: 1000, Out: 200}
						return r
					}(),
					func() Run {
						r := mkRun("impl", "claude", 0, ago(4, 9), time.Minute)
						r.Model = "sonnet-4.6"
						return r
					}(),
				}},
				{ID: "b", Status: "done", History: []Run{
					mkRun("impl", "pi", 0, ago(5, 9), time.Minute),
				}},
			},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				byName := map[string]Agent{}
				for _, a := range r.Agents {
					byName[a.Name] = a
				}
				require.NotNil(t, byName["claude"].TokensPerRun)
				require.Equal(t, int64(1200), *byName["claude"].TokensPerRun)
				require.Equal(t, "sonnet-4.6", byName["claude"].Model)
				require.Nil(t, byName["pi"].TokensPerRun)
				require.Equal(t, int64(1000), r.Totals.TokensIn)
				require.Equal(t, int64(200), r.Totals.TokensOut)
			},
		},
		{
			name: "project falls back to the path basename",
			tickets: []Ticket{
				{ID: "a", Status: "done", Project: "kontora", Path: "/repos/kontora", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
				{ID: "b", Status: "done", Path: "/elsewhere/acme-web", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
				{ID: "c", Status: "done", Path: "/elsewhere/acme-web", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
			},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Len(t, r.Projects, 2)
				require.Equal(t, "acme-web", r.Projects[0].Name)
				require.Equal(t, 2, r.Projects[0].Done)
				require.Equal(t, "kontora", r.Projects[1].Name)
				require.Equal(t, (24 * time.Hour).Milliseconds(), r.Projects[1].MedianCycleMS)
			},
		},
		{
			name: "project filter matches the resolved name",
			tickets: []Ticket{
				{ID: "a", Status: "done", Project: "kontora", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
				{ID: "b", Status: "done", Path: "/elsewhere/acme-web", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
			},
			opts: Options{Now: testNow, Days: 35, Project: "acme-web"},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 1, r.Totals.Shipped)
				require.Len(t, r.Projects, 1)
				require.Equal(t, "acme-web", r.Projects[0].Name)
			},
		},
		{
			name: "pipeline filter",
			tickets: []Ticket{
				{ID: "a", Status: "done", Pipeline: "default", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
				{ID: "b", Status: "done", Pipeline: "review", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
			},
			opts: Options{Now: testNow, Days: 35, Pipeline: "review"},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 1, r.Totals.Shipped)
			},
		},
		{
			name: "an unknown filter value matches nothing",
			tickets: []Ticket{
				{ID: "a", Status: "done", Project: "kontora", StartedAt: ago(4, 9), CompletedAt: ago(3, 9)},
			},
			opts: Options{Now: testNow, Days: 35, Project: "gone"},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 0, r.Totals.Shipped)
				require.Empty(t, r.Projects)
			},
		},
		{
			name: "deltas compare against the preceding window",
			tickets: []Ticket{
				{ID: "a", Status: "done", StartedAt: ago(3, 9), CompletedAt: ago(3, 11), History: []Run{
					func() Run {
						r := mkRun("impl", "claude", 0, ago(3, 9), 2*time.Hour)
						r.Usage = &Usage{In: 800, Out: 200}
						return r
					}(),
				}},
				{ID: "b", Status: "done", StartedAt: ago(10, 9), CompletedAt: ago(10, 12), History: []Run{
					func() Run {
						r := mkRun("impl", "claude", 0, ago(10, 9), 3*time.Hour)
						r.Usage = &Usage{In: 400, Out: 100}
						return r
					}(),
				}},
			},
			opts: Options{Now: testNow, Days: 7},
			check: func(t *testing.T, r Result) {
				require.Equal(t, 1, r.Totals.Shipped)
				require.Equal(t, (2 * time.Hour).Milliseconds(), r.Totals.MedianCycleMS)
				require.NotNil(t, r.Totals.MedianCycleDeltaMS)
				require.Equal(t, -(1 * time.Hour).Milliseconds(), *r.Totals.MedianCycleDeltaMS)
				require.NotNil(t, r.Totals.TokensDeltaPct)
				require.InDelta(t, 100.0, *r.Totals.TokensDeltaPct, 0.01)
			},
		},
		{
			name: "no preceding data leaves the deltas null",
			tickets: []Ticket{
				{ID: "a", Status: "done", StartedAt: ago(3, 9), CompletedAt: ago(3, 11)},
			},
			opts: Options{Now: testNow, Days: 7},
			check: func(t *testing.T, r Result) {
				require.Nil(t, r.Totals.MedianCycleDeltaMS)
				require.Nil(t, r.Totals.TokensDeltaPct)
			},
		},
		{
			name: "an agent that only ever annotated gets no row",
			tickets: []Ticket{{ID: "a", Status: "done", History: []Run{
				func() Run {
					r := mkRun("impl", "annotator", 0, ago(5, 9), time.Minute)
					r.Kind = "annotation"
					r.Usage = &Usage{In: 900, Out: 100}
					return r
				}(),
			}}},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Empty(t, r.Agents, "no stage run means no quality to report")
				require.Equal(t, 0, r.Totals.Runs)
				require.Equal(t, int64(900), r.Totals.TokensIn, "the spend is real and still counted")
			},
		},
		{
			name: "cycle time runs from the ticket being opened",
			tickets: []Ticket{
				// started_at is the last stage pickup: the daemon rewrites it on
				// every one, so a cycle measured from it would report 40 minutes.
				{ID: "a", Status: "done", Created: ago(4, 9), StartedAt: ago(1, 9), CompletedAt: ago(1, 10), History: []Run{
					mkRun("plan", "claude", 0, ago(4, 9), 2*time.Hour),
					mkRun("code", "claude", 0, ago(1, 9), time.Hour),
				}},
			},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Equal(t, (3*24*time.Hour + time.Hour).Milliseconds(), r.Totals.MedianCycleMS)
			},
		},
		{
			name: "a ticket with no created date falls back to its first run",
			tickets: []Ticket{
				{ID: "a", Status: "done", StartedAt: ago(1, 9), CompletedAt: ago(1, 10), History: []Run{
					mkRun("plan", "claude", 0, ago(2, 9), 2*time.Hour),
					mkRun("code", "claude", 0, ago(1, 9), time.Hour),
				}},
			},
			opts: Options{Now: testNow, Days: 35},
			check: func(t *testing.T, r Result) {
				require.Equal(t, (24*time.Hour + time.Hour).Milliseconds(), r.Totals.MedianCycleMS)
			},
		},
		{
			name: "empty input still spans the window",
			opts: Options{Now: testNow, Days: 98},
			check: func(t *testing.T, r Result) {
				require.Len(t, r.Days, 98)
				require.NotEmpty(t, r.Weeks)
				require.Empty(t, r.Stages)
				require.Empty(t, r.Agents)
				require.Empty(t, r.Projects)
				require.Equal(t, Totals{}, r.Totals)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, Compute(tc.tickets, tc.opts))
		})
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name   string
		values []int64
		p      int
		want   int64
	}{
		{name: "empty", values: nil, p: 50, want: 0},
		{name: "single", values: []int64{7}, p: 50, want: 7},
		{name: "single p90", values: []int64{7}, p: 90, want: 7},
		{name: "even count takes the lower of the two middles", values: []int64{1, 2, 3, 4}, p: 50, want: 2},
		{name: "odd count", values: []int64{3, 1, 2}, p: 50, want: 2},
		{name: "p90 is the ninth of ten", values: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, p: 90, want: 9},
		{name: "unsorted input", values: []int64{10, 1, 3, 2}, p: 90, want: 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, percentile(tc.values, tc.p))
		})
	}
}

func weekOf(t *testing.T, r Result, week string) Week {
	t.Helper()
	for _, w := range r.Weeks {
		if w.Week == week {
			return w
		}
	}
	t.Fatalf("no week %s in %v", week, r.Weeks)
	return Week{}
}

// TestComputeMidnightDST pins the buckets in a zone that moves its clock at
// midnight. Chile's 2024 change was at 24:00 on 7 September, so 2024-09-08 has
// no 00:00 and a naive time.Date normalises backward into the day before,
// which would shift every key the aggregator emits by a day.
func TestComputeMidnightDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skip("zone database unavailable")
	}
	at := func(day, hour int) *time.Time {
		v := time.Date(2024, 9, day, hour, 0, 0, 0, loc)
		return &v
	}
	now := time.Date(2024, 9, 20, 12, 0, 0, 0, loc)

	r := Compute([]Ticket{
		{ID: "a", Status: "done", Created: at(15, 9), CompletedAt: at(16, 9), History: []Run{
			{Stage: "impl", Agent: "claude", StartedAt: at(20, 9), CompletedAt: at(20, 10)},
		}},
		{ID: "b", Status: "done", Created: at(16, 9), CompletedAt: at(17, 9)},
	}, Options{Now: now, Days: 20})

	require.Equal(t, 2, r.Totals.Shipped)
	require.Len(t, r.Days, 20)
	require.Equal(t, "2024-09-01", r.Days[0].Date)
	require.Equal(t, "2024-09-20", r.Days[19].Date)
	seen := map[string]bool{}
	for _, d := range r.Days {
		require.False(t, seen[d.Date], "duplicate day %s", d.Date)
		seen[d.Date] = true
	}

	byWeek := map[string]Week{}
	for _, w := range r.Weeks {
		byWeek[w.Week] = w
	}
	require.Equal(t, 2, byWeek["2024-09-15"].Done, "the completions land in a week the series emits")
	require.Equal(t, "2024-09-20", r.Totals.BusiestDay, "today is reachable")
	require.Equal(t, 1, r.Totals.BusiestDayRuns)
}

func TestSince(t *testing.T) {
	// The comparison window opens 2 x 35 - 1 days before the last day of the
	// requested one.
	since := Since(Options{Now: testNow, Days: 35})
	require.Equal(t, "2026-06-04", since.Format(dayFormat))
	require.Equal(t, testLoc, since.Location())
}
