package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/stats"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestStatsCacheResultsAreBounded(t *testing.T) {
	c := newStatsCache()
	now := time.Now()

	// project and pipeline reach the key straight from the query string, so a
	// page on another origin can ask for as many distinct ones as it likes.
	for i := range statsCacheMax + 20 {
		c.store(fmt.Sprintf("98\x00p%d\x00", i), now.Add(time.Duration(i)*time.Millisecond), web.StatsInfo{})
	}
	assert.LessOrEqual(t, len(c.results), statsCacheMax)

	_, ok := c.result(fmt.Sprintf("98\x00p%d\x00", statsCacheMax+19), now.Add(time.Second))
	assert.True(t, ok, "the newest entry survives; the oldest are the ones dropped")
}

func TestStatsCacheSidecar(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, in int) string {
		path := filepath.Join(dir, name)
		data, err := json.Marshal(logfmt.Tape{
			Version: logfmt.TapeVersion, Agent: "claude", Model: "sonnet-4.6",
			Totals: logfmt.Usage{Input: in},
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		return path
	}

	t.Run("a run in flight is asked again", func(t *testing.T) {
		c := newStatsCache()
		path := filepath.Join(dir, "live.0.events.json")

		_, _, ok := c.sidecar(path, false)
		require.False(t, ok, "nothing written yet")

		write("live.0.events.json", 42)
		_, usage, ok := c.sidecar(path, false)
		require.True(t, ok)
		require.NotNil(t, usage)
		assert.Equal(t, 42, usage.In)

		metadata, ok := c.sidecarMetadata(path, false)
		require.True(t, ok)
		require.NotNil(t, metadata.Usage)
		assert.Equal(t, int64(42), metadata.Usage.Input, "the cache retains raw disjoint usage")
	})

	t.Run("a run that has ended and wrote no tape is remembered", func(t *testing.T) {
		c := newStatsCache()
		path := filepath.Join(dir, "ended.0.events.json")

		_, _, ok := c.sidecar(path, true)
		require.False(t, ok)

		// The tape is written before the history row that names the run, so a
		// completed run with no sidecar never gains one. Remembering that is
		// what keeps an install whose agent writes no tapes from re-issuing the
		// same failing stat on every poll, for every run it ever made.
		write("ended.0.events.json", 7)
		_, _, ok = c.sidecar(path, true)
		assert.False(t, ok, "the absence is cached, not re-checked")
	})
}

func TestSimpleTicketRun(t *testing.T) {
	now := time.Now()
	oldStart, oldEnd := now.Add(-2*time.Hour), now.Add(-time.Hour)
	tests := []struct {
		name      string
		ticket    ticket.Ticket
		wantRun   bool
		wantIndex int
	}{
		{name: "unrecorded completed run", ticket: ticket.Ticket{ID: "kon-simple", StartedAt: &now, CompletedAt: &now}, wantRun: true},
		{name: "unrecorded paused run", ticket: ticket.Ticket{ID: "kon-paused", Status: ticket.StatusPaused, StartedAt: &now}, wantRun: true},
		{
			name:    "completed legacy ticket after annotation still needs synthetic stage run",
			wantRun: true, wantIndex: 1,
			ticket: ticket.Ticket{ID: "kon-annotated", StartedAt: &now, CompletedAt: &now, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, Kind: ticket.KindAnnotation, CompletedAt: &now},
			}},
		},
		{
			name:    "paused legacy ticket after annotation still needs synthetic stage run",
			wantRun: true, wantIndex: 1,
			ticket: ticket.Ticket{ID: "kon-paused-annotated", Status: ticket.StatusPaused, StartedAt: &now, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, Kind: ticket.KindAnnotation, CompletedAt: &now},
			}},
		},
		{
			name: "annotation without simple completion",
			ticket: ticket.Ticket{ID: "kon-only-annotation", StartedAt: &now, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, Kind: ticket.KindAnnotation, CompletedAt: &now},
			}},
		},
		{
			name: "failed annotation does not invent a simple run",
			ticket: ticket.Ticket{ID: "kon-failed-annotation", Status: ticket.StatusPaused, StartedAt: &now, AnnotationReturnStatus: ticket.StatusOpen, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, Kind: ticket.KindAnnotation, CompletedAt: &now},
			}},
		},
		{name: "active standalone retry excludes its old completed run", ticket: ticket.Ticket{ID: "kon-live", Status: ticket.StatusInProgress, StartedAt: &now, CompletedAt: &now}},
		{name: "pipeline ticket", ticket: ticket.Ticket{ID: "kon-pipe", Pipeline: "pipe", StartedAt: &now, CompletedAt: &now}},
		{
			name: "real blocked standalone run is not synthesized",
			ticket: ticket.Ticket{ID: "kon-blocked", Status: ticket.StatusPaused, StartedAt: &now, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, StartedAt: &now, CompletedAt: &now, RunOutcome: ticket.RunOutcomeBlocked},
			}},
		},
		{
			name: "real legacy standalone run is not synthesized",
			ticket: ticket.Ticket{ID: "kon-legacy", Status: ticket.StatusDone, StartedAt: &now, CompletedAt: &now, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, StartedAt: &now, CompletedAt: &now},
			}},
		},
		{
			name:    "removed pipeline history does not suppress a later standalone run",
			wantRun: true,
			ticket: ticket.Ticket{ID: "kon-retargeted", Status: ticket.StatusDone, StartedAt: &now, CompletedAt: &now, History: []ticket.HistoryEntry{
				{Stage: "code", StartedAt: &oldStart, CompletedAt: &oldEnd},
			}},
		},
		{
			name:    "unparented child history does not suppress a later paused run",
			wantRun: true, wantIndex: 1,
			ticket: ticket.Ticket{ID: "kon-unparented", Status: ticket.StatusPaused, StartedAt: &now, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, StartedAt: &oldStart, CompletedAt: &oldEnd, RunOutcome: ticket.RunOutcomeBlocked},
			}},
		},
		{
			name: "recorded current run can start after ticket pickup",
			ticket: ticket.Ticket{ID: "kon-delayed", Status: ticket.StatusDone, StartedAt: &oldStart, CompletedAt: &now, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, StartedAt: &oldEnd, CompletedAt: &now, RunOutcome: ticket.RunOutcomeComplete},
			}},
		},
		{
			name: "recorded current completion without a start is not synthesized",
			ticket: ticket.Ticket{ID: "kon-no-start", Status: ticket.StatusDone, StartedAt: &oldStart, CompletedAt: &now, History: []ticket.HistoryEntry{
				{Stage: simpleStageName, CompletedAt: &now},
			}},
		},
		{
			name: "supervisor uses recorded history only",
			ticket: ticket.Ticket{ID: "kon-epic", Kind: ticket.KindEpic, EpicExecution: ticket.EpicExecutionAgent,
				Status: ticket.StatusDone, StartedAt: &now, CompletedAt: &now,
				History: []ticket.HistoryEntry{{Stage: simpleStageName, StartedAt: &now, CompletedAt: &now}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run, path, ok := simpleTicketRun(&config.Config{DefaultAgent: "claude"}, "/logs", &tc.ticket)
			assert.Equal(t, tc.wantRun, ok)
			if !tc.wantRun {
				assert.Zero(t, run)
				assert.Empty(t, path)
				return
			}
			assert.Equal(t, simpleStageName, run.Stage)
			assert.Equal(t, statsSidecarPath("/logs", tc.ticket.ID, simpleStageName, tc.wantIndex), path)
			assert.Empty(t, run.RunOutcome, "synthesized legacy runs are not verified completions")
		})
	}
}

func TestStatsSnapshotStandaloneOutcomes(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Minute)
	cases := []struct {
		name      string
		outcome   ticket.RunOutcome
		exit      int
		firstPass float64
		complete  int
		blocked   int
		protocol  int
		failed    int
		unknown   int
	}{
		{name: "verified complete", outcome: ticket.RunOutcomeComplete, firstPass: 100, complete: 1},
		{name: "blocked exit zero", outcome: ticket.RunOutcomeBlocked, blocked: 1},
		{name: "protocol error exit zero", outcome: ticket.RunOutcomeProtocolError, protocol: 1},
		{name: "semantic failure exit zero", outcome: ticket.RunOutcomeFailed, failed: 1},
		{name: "legacy exit zero remains unverified", unknown: 1},
		{name: "legacy runner failure remains unverified", exit: 7, unknown: 1},
		{name: "complete with runner failure is not first pass", outcome: ticket.RunOutcomeComplete, exit: 7, complete: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk := &ticket.Ticket{
				ID: "kon-standalone", Status: ticket.StatusDone, StartedAt: &start, CompletedAt: &now,
				History: []ticket.HistoryEntry{{
					Stage: simpleStageName, Agent: "claude", StartedAt: &start, CompletedAt: &now,
					RunOutcome: tc.outcome, ExitCode: tc.exit,
				}},
			}
			cfg := &config.Config{LogsDir: t.TempDir(), DefaultAgent: "claude"}
			d := &Daemon{tickets: map[string]*ticketState{tk.ID: {ticket: tk}}}
			snapshot, _ := d.statsSnapshot(cfg, now)
			require.Len(t, snapshot, 1)
			projected := snapshot[0]
			require.Len(t, projected.ticket.History, 1, "a real standalone run must be counted exactly once")
			assert.Equal(t, string(tc.outcome), projected.ticket.History[0].RunOutcome)
			assert.Equal(t, tc.exit, projected.ticket.History[0].ExitCode)
			assert.Equal(t, []string{filepath.Join(cfg.LogsDir, tk.ID, "default.0.events.json")}, projected.paths)

			result := stats.Compute([]stats.Ticket{projected.ticket}, stats.Options{Now: now, Days: 7})
			assert.Equal(t, 1, result.Totals.Runs)
			assert.Equal(t, tc.firstPass, result.Totals.FirstPassPct)
			require.Len(t, result.Agents, 1)
			assert.Equal(t, tc.firstPass, result.Agents[0].FirstPassPct)
			require.Len(t, result.Stages, 1)
			stage := result.Stages[0]
			assert.Equal(t, tc.complete, stage.Complete)
			assert.Equal(t, tc.blocked, stage.Blocked)
			assert.Equal(t, tc.protocol, stage.ProtocolError)
			assert.Equal(t, tc.failed, stage.Failed)
			assert.Equal(t, tc.unknown, stage.Unknown)
			if tc.exit == 0 {
				assert.Zero(t, stage.RunnerFailed)
			} else {
				assert.Equal(t, 1, stage.RunnerFailed)
			}
		})
	}
}

func TestStatsSnapshotRetargetedStandaloneRun(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	oldStart, oldEnd := now.Add(-2*time.Hour), now.Add(-time.Hour)
	start := now.Add(-time.Minute)
	for _, stage := range []string{"code", simpleStageName} {
		t.Run(stage, func(t *testing.T) {
			tk := &ticket.Ticket{
				ID: "kon-retargeted", Status: ticket.StatusDone, Agent: "claude", StartedAt: &start, CompletedAt: &now,
				History: []ticket.HistoryEntry{{Stage: stage, Agent: "claude", StartedAt: &oldStart, CompletedAt: &oldEnd}},
			}
			cfg := &config.Config{LogsDir: t.TempDir()}
			index := stageRunIndex(tk, simpleStageName)
			path := statsSidecarPath(cfg.LogsDir, tk.ID, simpleStageName, index)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			data, err := json.Marshal(logfmt.Tape{Version: logfmt.TapeVersion, Totals: logfmt.Usage{Input: 123, Output: 45}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o644))
			d := &Daemon{tickets: map[string]*ticketState{tk.ID: {ticket: tk}}}
			snapshot, _ := d.statsSnapshot(cfg, now)
			require.Len(t, snapshot, 1)
			require.Len(t, snapshot[0].ticket.History, 2)
			assert.Equal(t, index, snapshot[0].ticket.History[1].Run)
			assert.Equal(t, path, snapshot[0].paths[1])
			newStatsCache().fillUsage(snapshot, stats.Options{Now: now, Days: 7})
			result := stats.Compute([]stats.Ticket{snapshot[0].ticket}, stats.Options{Now: now, Days: 7})
			assert.Equal(t, 2, result.Totals.Runs)
			assert.Equal(t, int64(123), result.Totals.TokensIn)
			assert.Equal(t, int64(45), result.Totals.TokensOut)
			for _, s := range result.Stages {
				if s.Name == simpleStageName {
					assert.Equal(t, int64(time.Minute/time.Millisecond), s.P50MS)
				}
			}
		})
	}
}

func TestStatsSidecarPathRejectsAPlantedStage(t *testing.T) {
	logs := "/var/logs"
	assert.Equal(t, filepath.Join(logs, "kon-1", "step1.2.events.json"),
		statsSidecarPath(logs, "kon-1", "step1", 2))

	// History is ticket frontmatter, which anyone can edit. filepath.Join would
	// clean these into a path outside the ticket's own log directory.
	for _, stage := range []string{"", ".", "..", "../../etc/passwd", "a/b", `a\b`} {
		assert.Empty(t, statsSidecarPath(logs, "kon-1", stage, 0), stage)
	}
}
