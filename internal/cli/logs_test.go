package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	t.Run("an unnamed stage keeps the newest-log fallback", func(t *testing.T) {
		tape, content, err := StageActivity(ticketsDir, logsDir, "act-001", "", 0)
		require.NoError(t, err)
		assert.Nil(t, tape)
		assert.Equal(t, "plaintext review log", content)
	})
}
