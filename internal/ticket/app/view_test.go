package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// The review column orders by the finish time, so it has to hold still when the
// markdown is rewritten, and it has to be the same in both projections: the
// board builds its cards with detail=false and the SSE update with detail=true.
func TestBuildView_FinishedAt(t *testing.T) {
	at := func(mins int) *time.Time {
		v := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC).Add(time.Duration(mins) * time.Minute)
		return &v
	}

	cases := []struct {
		name string
		tk   ticket.Ticket
		want *time.Time
	}{
		{
			name: "the newest run that finished",
			tk: ticket.Ticket{History: []ticket.HistoryEntry{
				{Stage: "plan", CompletedAt: at(10)},
				{Stage: "code", CompletedAt: at(40)},
			}},
			want: at(40),
		},
		{
			name: "a run still open leaves the one before it",
			tk: ticket.Ticket{History: []ticket.HistoryEntry{
				{Stage: "plan", CompletedAt: at(10)},
				{Stage: "code", CompletedAt: at(40)},
				{Stage: "review", StartedAt: at(50)},
			}},
			want: at(40),
		},
		{
			name: "no run finished, so the frontmatter stamp",
			tk: ticket.Ticket{
				CompletedAt: at(30),
				History:     []ticket.HistoryEntry{{Stage: "code", StartedAt: at(20)}},
			},
			want: at(30),
		},
		{
			name: "no history at all, so the frontmatter stamp",
			tk:   ticket.Ticket{CompletedAt: at(30)},
			want: at(30),
		},
		{
			name: "nothing ever finished",
			tk:   ticket.Ticket{Created: at(0)},
			want: nil,
		},
	}

	cfg := &config.Config{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk := tc.tk
			list := BuildView(cfg, &tk, false)
			detail := BuildView(cfg, &tk, true)
			assert.Equal(t, tc.want, list.FinishedAt)
			assert.Equal(t, tc.want, detail.FinishedAt, "both projections report the same finish time")
			assert.Empty(t, list.History, "the board payload still carries no history")
		})
	}
}
