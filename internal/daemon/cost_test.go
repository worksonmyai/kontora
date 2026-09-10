package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/pricing"
	"github.com/worksonmyai/kontora/internal/stats"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func costTestDaemon(t *testing.T, logs string, tk *ticket.Ticket) *Daemon {
	t.Helper()
	h := newHarness(t)
	cfg := *h.cfg
	cfg.LogsDir = logs
	d := h.newDaemon(&cfg)
	d.tickets = map[string]*ticketState{tk.ID: {ticket: tk}}
	return d
}

func writeCostSidecar(t *testing.T, logs, id, stage string, run int, body string) {
	t.Helper()
	dir := filepath.Join(logs, id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.%d.events.json", stage, run)), []byte(body), 0o644))
}

func completeCostSidecar(model string, input, output, write, read int64) string {
	return fmt.Sprintf(`{"version":1,"model":%q,"totals":{"input":%d,"output":%d,"cache_create":%d,"cache_read":%d},"events":[]}`,
		model, input, output, write, read)
}

func TestGetTicketCostMapsHistoryPositionally(t *testing.T) {
	logs := t.TempDir()
	now := time.Now()
	tk := &ticket.Ticket{ID: "kon-cost", Pipeline: "pipe", CompletedAt: &now, History: []ticket.HistoryEntry{
		{Stage: "code", Run: 0, Model: "claude-haiku-4-5", CompletedAt: &now},
		{Stage: "code", Run: 0, Kind: ticket.KindAnnotation, Model: "anthropic/claude-sonnet-4.6", CompletedAt: &now},
		{Stage: "code", Run: 0, Model: "claude-haiku-4-5", CompletedAt: &now}, // legacy zero
		{Stage: "code", Run: 99, Model: "claude-haiku-4-5", CompletedAt: &now},
		{Stage: "../../planted", Model: "claude-haiku-4-5", CompletedAt: &now},
	}}
	writeCostSidecar(t, logs, tk.ID, "code", 0, completeCostSidecar("", 1, 0, 0, 0))
	writeCostSidecar(t, logs, tk.ID, "code", 1, completeCostSidecar("claude-haiku-4-5", 0, 1, 0, 0))
	writeCostSidecar(t, logs, tk.ID, "code", 2, completeCostSidecar("claude-haiku-4-5", 0, 0, 4, 10))

	got, err := costTestDaemon(t, logs, tk).GetTicketCost(tk.ID)
	require.NoError(t, err)
	require.Len(t, got.Runs, 5)
	assert.Equal(t, []int{0, 1, 2, 3, 0}, []int{got.Runs[0].Run, got.Runs[1].Run, got.Runs[2].Run, got.Runs[3].Run, got.Runs[4].Run})
	for i := range got.Runs {
		require.NotNil(t, got.Runs[i].HistoryIndex)
		assert.Equal(t, i, *got.Runs[i].HistoryIndex)
	}
	assert.Equal(t, "history", got.Runs[0].ModelSource)
	assert.Equal(t, "claude-haiku-4-5", got.Runs[0].Model)
	assert.Equal(t, "sidecar", got.Runs[1].ModelSource, "the tape model wins over the history model")
	assert.Equal(t, "anthropic/claude-haiku-4.5", got.Runs[1].ResolvedModel)
	assert.Equal(t, "0.000001", *got.Runs[0].CostUSD)
	assert.Equal(t, "0.000005", *got.Runs[1].CostUSD)
	assert.Equal(t, "0.000006", *got.Runs[2].CostUSD)
	assert.Nil(t, got.Runs[3].CostUSD, "a missing retry sidecar is unpriced")
	assert.Nil(t, got.Runs[4].CostUSD, "an unsafe stage is never turned into a path")
	assert.Equal(t, 3, got.PricedRuns)
	assert.Equal(t, 5, got.TrackedRuns)
	require.NotNil(t, got.CostUSD)
	assert.Equal(t, "0.000012", *got.CostUSD, "partial coverage returns the known subtotal")
	require.Len(t, got.Stages, 2)
	assert.Equal(t, "code", got.Stages[0].Name)
	assert.Equal(t, 3, got.Stages[0].PricedRuns)
	assert.Equal(t, 4, got.Stages[0].TrackedRuns)
	require.NotNil(t, got.Stages[0].CostUSD)
	assert.Equal(t, "0.000012", *got.Stages[0].CostUSD)
	assert.Equal(t, web.TicketCostStageInfo{Name: "../../planted", PricedRuns: 0, TrackedRuns: 1}, got.Stages[1])
}

func TestGetTicketCostUnpricedAndZeroRuns(t *testing.T) {
	tests := []struct {
		name    string
		stage   string
		model   string
		sidecar string
		priced  bool
		cost    string
	}{
		{name: "missing sidecar", stage: "code", model: "claude-haiku-4-5"},
		{name: "partial usage", stage: "code", sidecar: `{"version":1,"model":"claude-haiku-4-5","totals":{"input":1,"output":0,"cache_create":0,"cache_read":0},"partial":["usage"],"events":[]}`},
		{name: "malformed sidecar", stage: "code", model: "claude-haiku-4-5", sidecar: `{"model":`},
		{name: "malformed version", stage: "code", sidecar: `{"version":"one","model":"claude-haiku-4-5","totals":{"input":1,"output":0,"cache_create":0,"cache_read":0},"events":[]}`},
		{name: "unsupported version", stage: "code", sidecar: `{"version":2,"model":"claude-haiku-4-5","totals":{"input":1,"output":0,"cache_create":0,"cache_read":0},"events":[]}`},
		{name: "unresolved model", stage: "code", sidecar: completeCostSidecar("future/model", 1, 0, 0, 0)},
		{name: "negative input", stage: "code", sidecar: completeCostSidecar("claude-haiku-4-5", -1, 0, 0, 0)},
		{name: "negative output", stage: "code", sidecar: completeCostSidecar("claude-haiku-4-5", 0, -1, 0, 0)},
		{name: "negative cache write", stage: "code", sidecar: completeCostSidecar("claude-haiku-4-5", 0, 0, -1, 0)},
		{name: "negative cache read", stage: "code", sidecar: completeCostSidecar("claude-haiku-4-5", 0, 0, 0, -1)},
		{name: "unsafe stage", stage: "../code", sidecar: completeCostSidecar("claude-haiku-4-5", 1, 0, 0, 0)},
		{name: "measured zero", stage: "code", sidecar: completeCostSidecar("claude-haiku-4-5", 0, 0, 0, 0), priced: true, cost: "0.000000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := t.TempDir()
			now := time.Now()
			tk := &ticket.Ticket{ID: "kon-one", Pipeline: "pipe", History: []ticket.HistoryEntry{{Stage: tc.stage, Model: tc.model, CompletedAt: &now}}}
			if tc.sidecar != "" {
				if tc.stage == "code" {
					writeCostSidecar(t, logs, tk.ID, tc.stage, 0, tc.sidecar)
				} else if tc.name == "unsafe stage" {
					require.NoError(t, os.WriteFile(filepath.Join(logs, "code.0.events.json"), []byte(tc.sidecar), 0o644))
				}
			}
			got, err := costTestDaemon(t, logs, tk).GetTicketCost(tk.ID)
			require.NoError(t, err)
			require.Len(t, got.Runs, 1)
			assert.Equal(t, tc.priced, got.Runs[0].CostUSD != nil)
			assert.Equal(t, tc.priced, got.CostUSD != nil)
			if tc.priced {
				assert.Equal(t, tc.cost, *got.Runs[0].CostUSD)
				assert.Equal(t, tc.cost, *got.CostUSD)
				assert.Equal(t, 1, got.PricedRuns, "$0 is still priced")
			} else {
				assert.Zero(t, got.PricedRuns)
			}
			assert.Equal(t, 1, got.TrackedRuns)
		})
	}
}

func TestGetTicketCostStandaloneAndRunning(t *testing.T) {
	now := time.Now()
	t.Run("completed standalone gets synthetic default run", func(t *testing.T) {
		logs := t.TempDir()
		tk := &ticket.Ticket{ID: "kon-simple", StartedAt: &now, CompletedAt: &now}
		writeCostSidecar(t, logs, tk.ID, simpleStageName, 0, completeCostSidecar("claude-haiku-4-5", 1, 0, 0, 0))
		got, err := costTestDaemon(t, logs, tk).GetTicketCost(tk.ID)
		require.NoError(t, err)
		require.Len(t, got.Runs, 1)
		assert.Nil(t, got.Runs[0].HistoryIndex)
		assert.Equal(t, simpleStageName, got.Runs[0].Stage)
		assert.Equal(t, 0, got.Runs[0].Run)
		assert.Equal(t, "0.000001", *got.CostUSD)
	})

	t.Run("completed standalone after an annotation gets both runs", func(t *testing.T) {
		logs := t.TempDir()
		tk := &ticket.Ticket{
			ID: "kon-annotated", StartedAt: &now, CompletedAt: &now,
			History: []ticket.HistoryEntry{{
				Stage: simpleStageName, Kind: ticket.KindAnnotation, CompletedAt: &now,
			}},
		}
		writeCostSidecar(t, logs, tk.ID, simpleStageName, 0, completeCostSidecar("claude-haiku-4-5", 0, 1, 0, 0))
		writeCostSidecar(t, logs, tk.ID, simpleStageName, 1, completeCostSidecar("claude-haiku-4-5", 1, 0, 0, 0))

		got, err := costTestDaemon(t, logs, tk).GetTicketCost(tk.ID)
		require.NoError(t, err)
		require.Len(t, got.Runs, 2)
		require.NotNil(t, got.Runs[0].HistoryIndex)
		assert.Equal(t, 0, *got.Runs[0].HistoryIndex)
		assert.Equal(t, 0, got.Runs[0].Run)
		assert.Nil(t, got.Runs[1].HistoryIndex)
		assert.Equal(t, 1, got.Runs[1].Run)
		assert.Equal(t, 2, got.PricedRuns)
		assert.Equal(t, 2, got.TrackedRuns)
		assert.Equal(t, "0.000006", *got.CostUSD)
	})

	t.Run("paused standalone gets its ended run", func(t *testing.T) {
		logs := t.TempDir()
		tk := &ticket.Ticket{ID: "kon-paused", Status: ticket.StatusPaused, StartedAt: &now}
		writeCostSidecar(t, logs, tk.ID, simpleStageName, 0, completeCostSidecar("claude-haiku-4-5", 1, 0, 0, 0))

		got, err := costTestDaemon(t, logs, tk).GetTicketCost(tk.ID)
		require.NoError(t, err)
		require.Len(t, got.Runs, 1)
		assert.Nil(t, got.Runs[0].HistoryIndex)
		assert.Equal(t, "0.000001", *got.CostUSD)
	})

	t.Run("paused standalone rechecks a missing sidecar", func(t *testing.T) {
		logs := t.TempDir()
		tk := &ticket.Ticket{ID: "kon-paused-late", Status: ticket.StatusPaused, StartedAt: &now}
		d := costTestDaemon(t, logs, tk)

		first, err := d.GetTicketCost(tk.ID)
		require.NoError(t, err)
		assert.Zero(t, first.PricedRuns)

		writeCostSidecar(t, logs, tk.ID, simpleStageName, 0, completeCostSidecar("claude-haiku-4-5", 1, 0, 0, 0))
		second, err := d.GetTicketCost(tk.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, second.PricedRuns)
		assert.Equal(t, "0.000001", *second.CostUSD)
	})

	t.Run("failed annotation does not invent a standalone run", func(t *testing.T) {
		logs := t.TempDir()
		tk := &ticket.Ticket{
			ID: "kon-failed-annotation", Status: ticket.StatusPaused, StartedAt: &now,
			AnnotationReturnStatus: ticket.StatusOpen,
			History: []ticket.HistoryEntry{{
				Stage: simpleStageName, Kind: ticket.KindAnnotation, CompletedAt: &now,
			}},
		}
		writeCostSidecar(t, logs, tk.ID, simpleStageName, 0, completeCostSidecar("claude-haiku-4-5", 1, 0, 0, 0))

		got, err := costTestDaemon(t, logs, tk).GetTicketCost(tk.ID)
		require.NoError(t, err)
		require.Len(t, got.Runs, 1)
		require.NotNil(t, got.Runs[0].HistoryIndex)
	})

	t.Run("completed ticket that never started has no synthetic run", func(t *testing.T) {
		tk := &ticket.Ticket{ID: "kon-manual", CompletedAt: &now}
		got, err := costTestDaemon(t, t.TempDir(), tk).GetTicketCost(tk.ID)
		require.NoError(t, err)
		assert.Empty(t, got.Runs)
		assert.Zero(t, got.TrackedRuns)
	})

	t.Run("running stages without history are excluded", func(t *testing.T) {
		tests := []ticket.Ticket{
			{ID: "kon-live-pipeline", Pipeline: "pipe", Status: ticket.StatusInProgress, Stage: "code"},
			{ID: "kon-live-standalone", Status: ticket.StatusInProgress, StartedAt: &now, CompletedAt: &now},
		}
		for i := range tests {
			t.Run(tests[i].ID, func(t *testing.T) {
				tk := &tests[i]
				got, err := costTestDaemon(t, t.TempDir(), tk).GetTicketCost(tk.ID)
				require.NoError(t, err)
				assert.Empty(t, got.Runs)
				assert.Empty(t, got.Stages)
				assert.Zero(t, got.TrackedRuns)
				assert.Nil(t, got.CostUSD)
			})
		}
	})
}

func TestGetTicketCostAggregateExceedsAmount(t *testing.T) {
	logs := t.TempDir()
	now := time.Now()
	tk := &ticket.Ticket{ID: "kon-large", Pipeline: "pipe", History: []ticket.HistoryEntry{
		{Stage: "code", CompletedAt: &now},
		{Stage: "code", CompletedAt: &now},
	}}
	for run := range 2 {
		writeCostSidecar(t, logs, tk.ID, "code", run, completeCostSidecar("claude-haiku-4-5", 0, 1_000_000_000_000_000_000, 0, 0))
	}

	got, err := costTestDaemon(t, logs, tk).GetTicketCost(tk.ID)
	require.NoError(t, err)
	require.Len(t, got.Runs, 2)
	assert.Equal(t, "5000000000000.000000", *got.Runs[0].CostUSD)
	assert.Equal(t, "5000000000000.000000", *got.Runs[1].CostUSD)
	assert.Equal(t, "10000000000000.000000", *got.CostUSD)
	assert.Equal(t, 2, got.PricedRuns)
	assert.Equal(t, 2, got.TrackedRuns)
}

func TestCalculateRunCostMissingRate(t *testing.T) {
	usage := &stats.SidecarUsage{Input: 1}
	resolved := func(string) (pricing.Model, bool) {
		return pricing.Model{ID: "provider/model", Pricing: pricing.Rates{}}, true
	}
	model, _, ok := calculateRunCost("model-as-recorded", usage, resolved)
	assert.Equal(t, "provider/model", model.ID)
	assert.False(t, ok, "a missing rate for a nonzero category is unpriced")
}

func TestGetTicketCostNotFound(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	_, err := d.GetTicketCost("missing")
	assert.ErrorIs(t, err, web.ErrTicketNotFound)
}
