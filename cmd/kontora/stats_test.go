package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/stats"
	"github.com/worksonmyai/kontora/internal/web"
)

type cliStatsService struct {
	web.TicketService
	getStats func(web.StatsQuery) (web.StatsInfo, error)
}

func (s cliStatsService) GetStats(q web.StatsQuery) (web.StatsInfo, error) { return s.getStats(q) }

func TestCLIStats(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   []string
		query   web.StatsQuery
		wantErr string
	}{
		{name: "default window", query: web.StatsQuery{Days: 98}},
		{
			name:  "range, project, and pipeline",
			flags: []string{"--range", "1w", "--project", "Research & Design", "--pipeline", "review+ship"},
			query: web.StatsQuery{Days: 7, Project: "Research & Design", Pipeline: "review+ship"},
		},
		{name: "invalid range", flags: []string{"--range", "2w"}, wantErr: "range must be one of 1d, 1w, 30d, 90d, all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startCLIHTTPServer(t, cliStatsService{getStats: func(q web.StatsQuery) (web.StatsInfo, error) {
				assert.Empty(t, tc.wantErr, "the HTTP handler must reject invalid ranges before calling the service")
				assert.Equal(t, tc.query, q)
				return web.StatsInfo{
					Window: stats.Window{From: "2026-11-01", To: "2026-11-07", Days: q.Days},
					Totals: stats.Totals{Shipped: 2, Runs: 5},
					Live:   stats.Live{Running: 1, Slots: 3, Queued: 2, InReview: 1},
				}, nil
			}})
			out, stderr, err := runCLISeparated(t, env, nil, append([]string{"stats"}, tc.flags...)...)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, stderr, tc.wantErr)
				assert.Empty(t, out)
				return
			}
			require.NoError(t, err, stderr)
			assert.Contains(t, out, "2026-11-01 → 2026-11-07")
			assert.Contains(t, out, "1 running of 3 slots, 2 queued, 1 in review")
			assert.Contains(t, strings.Join(strings.Fields(out), " "), "Totals shipped 2 shipped this week 0 runs 5")
		})
	}
}
