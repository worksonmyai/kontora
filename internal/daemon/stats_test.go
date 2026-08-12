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

	"github.com/worksonmyai/kontora/internal/logfmt"
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
