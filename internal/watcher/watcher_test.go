package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWatcher(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 50*time.Millisecond, MarkdownFilter)
	require.NoError(t, err)
	defer w.Close()

	t.Run("create md file", func(t *testing.T) {
		path := filepath.Join(dir, "ticket.md")
		require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))
		ev := waitEvent(t, w, 2*time.Second)
		require.Equal(t, OpChanged, ev.Op)
		require.Equal(t, path, ev.Path)
	})

	t.Run("modify md file", func(t *testing.T) {
		path := filepath.Join(dir, "ticket.md")
		require.NoError(t, os.WriteFile(path, []byte("updated"), 0o644))
		ev := waitEvent(t, w, 2*time.Second)
		require.Equal(t, OpChanged, ev.Op)
	})

	t.Run("remove md file", func(t *testing.T) {
		path := filepath.Join(dir, "ticket.md")
		require.NoError(t, os.Remove(path))
		ev := waitEvent(t, w, 2*time.Second)
		require.Equal(t, OpRemoved, ev.Op)
	})

	t.Run("ignore non-md files", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("skip"), 0o644))
		assertNoEvent(t, w, 200*time.Millisecond)
	})

	t.Run("debounce collapses rapid writes", func(t *testing.T) {
		path := filepath.Join(dir, "rapid.md")
		for i := range 5 {
			require.NoError(t, os.WriteFile(path, []byte{byte(i)}, 0o644))
			time.Sleep(10 * time.Millisecond)
		}
		ev := waitEvent(t, w, 2*time.Second)
		require.Equal(t, OpChanged, ev.Op)
		// Should not get additional events for the debounced writes.
		assertNoEvent(t, w, 200*time.Millisecond)
	})
}

func TestWatcherPathSet(t *testing.T) {
	dir := t.TempDir()
	watched := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(watched, []byte("initial"), 0o644))

	w, err := New(dir, 50*time.Millisecond, PathSet(watched))
	require.NoError(t, err)
	defer w.Close()

	t.Run("reports the watched path", func(t *testing.T) {
		require.NoError(t, os.WriteFile(watched, []byte("changed"), 0o644))
		ev := waitEvent(t, w, 2*time.Second)
		require.Equal(t, watched, ev.Path)
	})

	t.Run("ignores other files in the same directory", func(t *testing.T) {
		// The daemon lock file and the atomic-write temp files share the config
		// directory; neither may trigger a reload.
		require.NoError(t, os.WriteFile(filepath.Join(dir, "lock"), []byte("1"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kontora-config-123.tmp"), []byte("x"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "other.yaml"), []byte("x"), 0o644))
		assertNoEvent(t, w, 300*time.Millisecond)
	})

	t.Run("reports an atomic rename over the watched path", func(t *testing.T) {
		tmp := filepath.Join(dir, "staged.tmp")
		require.NoError(t, os.WriteFile(tmp, []byte("renamed"), 0o644))
		require.NoError(t, os.Rename(tmp, watched))
		ev := waitEvent(t, w, 2*time.Second)
		require.Equal(t, watched, ev.Path)
	})
}

func TestWatcherNilFilterReportsEverything(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, 50*time.Millisecond, nil)
	require.NoError(t, err)
	defer w.Close()

	path := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))
	ev := waitEvent(t, w, 2*time.Second)
	require.Equal(t, path, ev.Path)
}

func waitEvent(t *testing.T, w *Watcher, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev := <-w.Events():
		return ev
	case err := <-w.Errors():
		require.NoError(t, err, "watcher error")
		return Event{}
	case <-time.After(timeout):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func assertNoEvent(t *testing.T, w *Watcher, wait time.Duration) {
	t.Helper()
	select {
	case ev := <-w.Events():
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(wait):
	}
}
