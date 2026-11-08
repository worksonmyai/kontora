package ticket

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCloneDetachedYAMLAndTypedFields(t *testing.T) {
	original, err := ParseBytes([]byte("---\nid: tst-001\ncustom: &custom\n  nested: [one, two] # keep me\ncopy: *custom\ndeps: [before]\nhistory:\n  - stage: code\n    started_at: 2026-01-01T00:00:00Z\nnotify: [done]\nmanaged:\n  board_id: board\n  generation: 1\n---\n# Original\n"))
	require.NoError(t, err)
	before, err := original.Marshal()
	require.NoError(t, err)
	cloned := original.Clone()
	cloned.Deps[0] = "after"
	cloned.Notify.Statuses[0] = "paused"
	*cloned.History[0].StartedAt = time.Time{}
	cloned.Managed.Generation = 2
	require.NoError(t, cloned.SetField("custom", map[string]any{"nested": []string{"changed"}}))
	cloned.SetBody("# Changed\n")
	after, err := original.Marshal()
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, "before", original.Deps[0])
	require.False(t, original.History[0].StartedAt.IsZero())
	require.Equal(t, uint64(1), original.Managed.Generation)
	require.Equal(t, "done", original.Notify.Statuses[0])
	require.Contains(t, string(after), "# keep me")
	require.Contains(t, string(after), "copy: *custom")
}

func TestOfflineOwnershipRefusal(t *testing.T) {
	for _, marker := range []string{"managed: {}\n", "managed: null\n", "managed:\n  board_id: foreign\n"} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tst-001.md")
			data := []byte("---\nid: tst-001\n" + marker + "---\n# Ticket\n")
			require.NoError(t, os.WriteFile(path, data, 0o644))
			require.ErrorIs(t, WriteOffline(path, []byte("---\nid: tst-001\n---\n# Replacement\n")), ErrManaged)
			require.ErrorIs(t, SetNoteReaction(dir, "tst-001", "note", "ok", "author", true), ErrManaged)
			require.ErrorIs(t, RemoveNoteSidecar(dir, "tst-001"), ErrManaged)
			actual, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, data, actual)
		})
	}
}

func TestTransferRoutingAdmission(t *testing.T) {
	for _, heldExclusive := range []bool{false, true} {
		t.Run(fmt.Sprint(heldExclusive), func(t *testing.T) {
			dir := t.TempDir()
			lock := LockOrdinaryRouting
			if heldExclusive {
				lock = LockTransferSource
			}
			release, err := lock(dir)
			require.NoError(t, err)
			defer release()
			_, err = LockTransferSource(dir)
			require.ErrorIs(t, err, ErrManaged)
			if heldExclusive {
				_, err = LockOrdinaryRouting(dir)
				require.ErrorIs(t, err, ErrManaged)
			}
		})
	}
}

func TestCreateOfflineDoesNotOverwrite(t *testing.T) {
	for _, collision := range []string{"markdown", "markdown symlink", "sidecar", "sidecar symlink"} {
		t.Run(collision, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tst-new.md")
			existing := path
			if strings.HasPrefix(collision, "sidecar") {
				existing = filepath.Join(dir, "tst-new.notes.json")
			}
			if strings.HasSuffix(collision, "symlink") {
				require.NoError(t, os.Symlink("missing", existing))
			} else {
				require.NoError(t, os.WriteFile(existing, []byte("original"), 0o644))
			}
			require.Error(t, CreateOffline(path, []byte("---\nid: tst-new\n---\n# New\n")))
			if strings.HasSuffix(collision, "symlink") {
				target, err := os.Readlink(existing)
				require.NoError(t, err)
				require.Equal(t, "missing", target)
			} else {
				data, err := os.ReadFile(existing)
				require.NoError(t, err)
				require.Equal(t, "original", string(data))
			}
		})
	}
}

func TestCreateOfflineConcurrentPublication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tst-new.md")
	const writers = 12
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			results <- CreateOffline(path, []byte("---\nid: tst-new\n---\n# New\n"))
		})
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	require.Equal(t, 1, winners)
	got, err := ParseFile(path)
	require.NoError(t, err)
	require.Equal(t, "New", got.Title())
}

func TestRejectImportedOwnershipBeforeCreation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tst-001.md")
	require.ErrorIs(t, WriteOffline(path, []byte("---\nid: tst-001\nmanaged: {}\n---\n# Forged\n")), ErrManaged)
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
