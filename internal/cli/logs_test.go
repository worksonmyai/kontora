package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStageActivityImportedHistory(t *testing.T) {
	ticketsDir := t.TempDir()
	logsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ticketsDir, "act-imported.md"), []byte(`---
id: act-imported
status: done
imported_history_count: 3
history:
  - {stage: review}
  - {stage: plan}
  - {stage: review, kind: annotation}
  - {stage: review, run: 2}
---
# Imported activity
`), 0o644))
	logDir := filepath.Join(logsDir, "act-imported")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "review.log"), []byte("local plaintext"), 0o644))
	for _, run := range []string{"0", "2"} {
		require.NoError(t, os.WriteFile(filepath.Join(logDir, "review."+run+".events.json"),
			[]byte(`{"version":1,"events":[{"kind":"text","text":"local tape"}]}`), 0o644))
	}
	for _, tc := range []struct {
		name, stage string
		run         int
	}{
		{name: "existing sidecar", stage: "review"},
		{name: "plaintext fallback", stage: "review", run: 1},
		{name: "interleaved stage", stage: "plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tape, content, err := StageActivity(ticketsDir, logsDir, "act-imp", tc.stage, tc.run)
			require.ErrorIs(t, err, os.ErrNotExist)
			assert.Contains(t, err.Error(), "imported run")
			assert.Nil(t, tape)
			assert.Empty(t, content)
		})
	}
	tape, content, err := StageActivity(ticketsDir, logsDir, "act-imp", "review", 2)
	require.NoError(t, err)
	require.NotNil(t, tape)
	assert.Empty(t, content)
	assert.Equal(t, "local tape", tape.Events[0].Text)
	require.NoError(t, os.Remove(filepath.Join(logDir, "review.2.events.json")))
	tape, content, err = StageActivity(ticketsDir, logsDir, "act-imp", "review", 2)
	require.NoError(t, err)
	assert.Nil(t, tape)
	assert.Equal(t, "local plaintext", content)
}

func TestStageActivity(t *testing.T) {
	ticketsDir := t.TempDir()
	logsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ticketsDir, "act-001.md"),
		[]byte("---\nid: act-001\nstatus: done\n---\n# Activity\n"), 0o644))

	logDir := filepath.Join(logsDir, "act-001")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "review.log"), []byte("plaintext review log"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "review.1.events.json"),
		[]byte(`{"version":1,"agent":"claude","events":[{"kind":"text","text":"hi"}]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "broken.0.events.json"), []byte("{not json"), 0o644))

	t.Run("an existing sidecar is returned as a tape", func(t *testing.T) {
		tape, content, err := StageActivity(ticketsDir, logsDir, "act-001", "review", 1)
		require.NoError(t, err)
		require.NotNil(t, tape)
		assert.Empty(t, content)
		assert.Equal(t, "claude", tape.Agent)
		require.Len(t, tape.Events, 1)
		assert.Equal(t, "hi", tape.Events[0].Text)
	})

	t.Run("a missing sidecar falls back to the plaintext log", func(t *testing.T) {
		tape, content, err := StageActivity(ticketsDir, logsDir, "act-001", "review", 0)
		require.NoError(t, err)
		assert.Nil(t, tape)
		assert.Equal(t, "plaintext review log", content)
	})

	t.Run("a malformed sidecar is an error rather than a silent fallback", func(t *testing.T) {
		_, _, err := StageActivity(ticketsDir, logsDir, "act-001", "broken", 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parsing activity sidecar")
	})

	t.Run("a named stage with no log does not borrow another stage's", func(t *testing.T) {
		_, _, err := StageActivity(ticketsDir, logsDir, "act-001", "ship", 0)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	// The daemon turns this into an empty live payload rather than a 404, so a
	// poll that arrives before the first bytes are written shows no error.
	t.Run("a sidecar for another run of the stage is not borrowed", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(logDir, "plan.2.events.json"),
			[]byte(`{"version":1,"agent":"claude","events":[]}`), 0o644))
		_, _, err := StageActivity(ticketsDir, logsDir, "act-001", "plan", 0)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("an unnamed stage keeps the newest-log fallback", func(t *testing.T) {
		tape, content, err := StageActivity(ticketsDir, logsDir, "act-001", "", 0)
		require.NoError(t, err)
		assert.Nil(t, tape)
		assert.Equal(t, "plaintext review log", content)
	})
}
