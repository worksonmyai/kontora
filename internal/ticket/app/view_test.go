package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestBuildView_AutomationProvenance(t *testing.T) {
	for _, tc := range []struct {
		name string
		tk   ticket.Ticket
	}{
		{name: "legacy"},
		{name: "automation", tk: ticket.Ticket{
			AutomationID: "auto-1", OccurrenceID: "occ-1", ScheduledFor: "2026-09-01T09:00:00Z",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, detail := range []bool{false, true} {
				view := BuildView(&config.Config{}, &tc.tk, detail)
				assert.Equal(t, tc.tk.AutomationID, view.AutomationID)
				assert.Equal(t, tc.tk.OccurrenceID, view.OccurrenceID)
				assert.Equal(t, tc.tk.ScheduledFor, view.ScheduledFor)
				assert.Empty(t, view.ScheduledAt)
				assert.Empty(t, view.ClaimedBy)
				assert.Empty(t, view.InvocationID)
				assert.Nil(t, view.Managed)
				encoded, err := json.Marshal(view)
				require.NoError(t, err)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(encoded, &fields))
				for key, want := range map[string]string{
					"automation_id": tc.tk.AutomationID,
					"occurrence_id": tc.tk.OccurrenceID,
					"scheduled_for": tc.tk.ScheduledFor,
				} {
					if want == "" {
						assert.NotContains(t, fields, key)
					} else {
						assert.Equal(t, want, fields[key])
					}
				}
			}
		})
	}
}

func TestBuildView_ImportedHistory(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	end := start.Add(time.Minute)
	tk := &ticket.Ticket{ImportedHistoryCount: 2, History: []ticket.HistoryEntry{
		{Stage: "code", Summary: "imported stage", StartedAt: &start, CompletedAt: &end},
		{Stage: "code", Kind: ticket.KindAnnotation, Summary: "imported annotation", StartedAt: &start, CompletedAt: &end},
		{Stage: "code", Run: 2, Summary: "local"},
	}}
	view := BuildView(&config.Config{}, tk, true)
	assert.Len(t, view.History, 3)
	for i, row := range view.History {
		assert.Equal(t, i < 2, row.Imported)
		assert.Equal(t, tk.History[i].Summary, row.Summary)
		assert.Equal(t, tk.History[i].StartedAt, row.StartedAt)
		assert.Equal(t, tk.History[i].CompletedAt, row.CompletedAt)
		assert.Equal(t, tk.History[i].Run, row.Run)
	}
	assert.Empty(t, BuildView(&config.Config{}, tk, false).History)
	tk.ImportedHistoryCount = 0
	for _, row := range BuildView(&config.Config{}, tk, true).History {
		assert.False(t, row.Imported)
	}
}

func TestBuildView_RunOutcomes(t *testing.T) {
	tk := &ticket.Ticket{
		InvocationID: "active",
		History: []ticket.HistoryEntry{
			{Stage: "default", ExitCode: 0}, // legacy, not verified complete
			{Stage: "default", ExitCode: 0, RunOutcome: ticket.RunOutcomeBlocked, OutcomeReason: "needs input", InvocationID: "previous"},
		},
	}
	for _, detail := range []bool{false, true} {
		v := BuildView(&config.Config{}, tk, detail)
		assert.Equal(t, "active", v.InvocationID)
		if !detail {
			assert.Empty(t, v.History)
			continue
		}
		assert.Empty(t, v.History[0].RunOutcome)
		assert.Equal(t, "blocked", v.History[1].RunOutcome)
		assert.Equal(t, "needs input", v.History[1].OutcomeReason)
		assert.Equal(t, "previous", v.History[1].InvocationID)
		assert.Zero(t, v.History[1].ExitCode)
	}
}

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
