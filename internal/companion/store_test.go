package companion

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func TestStoreRestart(t *testing.T) {
	for _, condition := range []string{"paused", "human_review", "question", "completed", "completion"} {
		t.Run(condition, func(t *testing.T) {
			dir := t.TempDir()
			store := openStore(t, dir)
			state, err := store.Load()
			require.NoError(t, err)
			require.Equal(t, State{Version: snapshotVersion, Items: []Item{}}, state)
			o := observation("ticket-created/run-1/tool-1", condition)
			state.Reconcile([]Observation{o}, observedTime, true)
			require.NoError(t, state.SetRead(state.Items[0].ID, state.Items[0].Revision, true))
			require.NoError(t, state.CatchUp(state.Revision))
			require.NoError(t, store.Save(state))
			require.NoError(t, store.Close())

			reopened := openStore(t, dir)
			loaded, err := reopened.Load()
			require.NoError(t, err)
			require.Equal(t, state, loaded)
			changed := loaded.Reconcile([]Observation{o}, observedTime.Add(time.Minute), true)
			if condition == "question" {
				require.True(t, changed)
				require.Len(t, loaded.Items, 2)
				require.Equal(t, state.Items[0].ID, loaded.Items[0].ID)
				require.Equal(t, "interrupted", loaded.Items[0].InactiveReason)
				require.False(t, loaded.Items[0].Active)
				require.True(t, loaded.Items[0].Read)
				require.NotEqual(t, loaded.Items[0].ID, loaded.Items[1].ID)
				require.False(t, loaded.Items[1].Read)
			} else {
				require.False(t, changed)
				require.Equal(t, state.Items, loaded.Items)
			}
			require.Equal(t, state.CaughtUp, loaded.CaughtUp)
			require.NoError(t, reopened.Save(loaded))
		})
	}
}

func TestStorePreservesInvalidSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
		err  error
	}{
		{"truncated", `{"version":1,"items":[`, ErrCorrupt},
		{"empty", ``, ErrCorrupt},
		{"invalid JSON", `not json`, ErrCorrupt},
		{"future format", `{"version":2,"new_field":true}`, ErrUnsupportedVersion},
		{"unversioned", `{"items":[]}`, ErrUnsupportedVersion},
		{"null", `null`, ErrUnsupportedVersion},
		{"unknown field", `{"version":1,"unknown":true}`, ErrCorrupt},
		{"trailing object", `{"version":1} {}`, ErrCorrupt},
		{"future checkpoint", `{"version":1,"revision":1,"caught_up":2}`, ErrCorrupt},
		{"invalid item", `{"version":1,"items":[{"id":"broken"}]}`, ErrCorrupt},
	} {
		for _, loadFirst := range []bool{false, true} {
			t.Run(tt.name+"/load-first="+strconv.FormatBool(loadFirst), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "state.json")
				require.NoError(t, os.WriteFile(path, []byte(tt.data), 0600))
				store := openStore(t, dir)
				if loadFirst {
					_, err := store.Load()
					require.ErrorIs(t, err, tt.err)
				}
				var state State
				state.Reconcile([]Observation{observation("status", "paused")}, observedTime, false)
				require.ErrorIs(t, store.Save(state), tt.err)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, tt.data, string(data))
				require.NoError(t, os.Remove(path))
				require.ErrorIs(t, store.Save(state), tt.err)
				_, err = os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
			})
		}
	}
}

func TestStoreFailures(t *testing.T) {
	for _, stage := range []string{"create", "write", "sync", "close", "rename", "sync_dir"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			store := openStore(t, dir)
			var original State
			original.Reconcile([]Observation{observation("status", "paused")}, observedTime, false)
			require.NoError(t, store.Save(original))
			before, err := os.ReadFile(filepath.Join(dir, "state.json"))
			require.NoError(t, err)
			updated := cloneState(t, original)
			require.NoError(t, updated.SetRead(updated.Items[0].ID, updated.Items[0].Revision, true))
			failure := errors.New("injected " + stage + " failure")
			store.fault = func(at string) error {
				if at == stage {
					return failure
				}
				return nil
			}
			require.ErrorIs(t, store.Save(updated), failure)
			data, err := os.ReadFile(filepath.Join(dir, "state.json"))
			require.NoError(t, err)
			want := original
			if stage == "sync_dir" {
				want = updated
			} else {
				require.Equal(t, before, data)
			}
			loaded, err := store.Load()
			require.NoError(t, err)
			require.Equal(t, want, loaded)
			temps, err := filepath.Glob(filepath.Join(dir, ".state-*"))
			require.NoError(t, err)
			require.Empty(t, temps)
			store.fault = nil
			require.NoError(t, store.Save(updated))
			require.NoError(t, store.Close())
			reopened := openStore(t, dir)
			loaded, err = reopened.Load()
			require.NoError(t, err)
			require.Equal(t, updated, loaded)
		})
	}
}

func TestStorePermissionsAndNoOp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "companion", "scope")
	store := openStore(t, dir)
	var state State
	state.Reconcile([]Observation{observation("status", "paused")}, observedTime, false)
	require.NoError(t, store.Save(state))
	for path, permissions := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "store.lock"): 0600, filepath.Join(dir, "state.json"): 0600} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, permissions, info.Mode().Perm(), path)
	}
	before, err := os.Stat(filepath.Join(dir, "state.json"))
	require.NoError(t, err)
	store.fault = func(stage string) error { return errors.New("unexpected write at " + stage) }
	require.NoError(t, store.Save(state))
	after, err := os.Stat(filepath.Join(dir, "state.json"))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, before.ModTime(), after.ModTime())
}

func TestStoreOwnership(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	lockPath := filepath.Join(dir, "store.lock")
	before, err := os.Stat(lockPath)
	require.NoError(t, err)
	other, err := Open(dir)
	require.ErrorIs(t, err, ErrLocked)
	require.Nil(t, other)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(dir, alias))
	other, err = Open(alias)
	require.ErrorIs(t, err, ErrLocked)
	require.Nil(t, other)
	checkProcessOwnership(t, dir, true)
	require.NoError(t, store.Close())
	require.NoError(t, store.Close())
	after, err := os.Stat(lockPath)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	checkProcessOwnership(t, dir, false)
	reopened := openStore(t, dir)
	after, err = os.Stat(lockPath)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	_, err = reopened.Load()
	require.NoError(t, err)
	_, err = store.Load()
	require.ErrorIs(t, err, os.ErrClosed)
	require.ErrorIs(t, store.Save(State{Version: snapshotVersion}), os.ErrClosed)
}

func checkProcessOwnership(t *testing.T, dir string, locked bool) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestStoreLockProcess$")
	command.Env = append(os.Environ(), "COMPANION_LOCK_TEST_DIR="+dir)
	if locked {
		command.Env = append(command.Env, "COMPANION_LOCK_TEST_BUSY=1")
	}
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestStoreLockProcess(t *testing.T) {
	dir := os.Getenv("COMPANION_LOCK_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	store, err := Open(dir)
	if os.Getenv("COMPANION_LOCK_TEST_BUSY") == "1" {
		require.ErrorIs(t, err, ErrLocked)
		require.Nil(t, store)
	} else {
		require.NoError(t, err)
		require.NoError(t, store.Close())
	}
}

func TestStoreFilesystemErrors(t *testing.T) {
	for _, name := range []string{"directory snapshot", "symlink snapshot", "symlink lock", "non-directory store", "rename failure", "corrupted after load"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			var state State
			state.Reconcile([]Observation{observation("status", "paused")}, observedTime, false)
			switch name {
			case "directory snapshot":
				require.NoError(t, os.Mkdir(path, 0700))
				store := openStore(t, dir)
				_, err := store.Load()
				require.ErrorIs(t, err, ErrCorrupt)
				require.ErrorIs(t, store.Save(state), ErrCorrupt)
			case "symlink snapshot":
				target := filepath.Join(t.TempDir(), "target")
				require.NoError(t, os.WriteFile(target, []byte("preserve me"), 0600))
				require.NoError(t, os.Symlink(target, path))
				store := openStore(t, dir)
				require.ErrorIs(t, store.Save(state), ErrCorrupt)
				data, err := os.ReadFile(target)
				require.NoError(t, err)
				require.Equal(t, "preserve me", string(data))
				info, err := os.Lstat(path)
				require.NoError(t, err)
				require.NotZero(t, info.Mode()&os.ModeSymlink)
			case "symlink lock":
				require.NoError(t, os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "store.lock")))
				store, err := Open(dir)
				require.Error(t, err)
				require.Nil(t, store)
			case "non-directory store":
				require.NoError(t, os.WriteFile(path, nil, 0600))
				store, err := Open(path)
				require.Error(t, err)
				require.Nil(t, store)
			case "rename failure":
				store := openStore(t, dir)
				store.fault = func(stage string) error {
					if stage == "rename" {
						return os.Mkdir(path, 0700)
					}
					return nil
				}
				require.Error(t, store.Save(state))
				temps, err := filepath.Glob(filepath.Join(dir, ".state-*"))
				require.NoError(t, err)
				require.Empty(t, temps)
			case "corrupted after load":
				store := openStore(t, dir)
				_, err := store.Load()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte("broken"), 0600))
				require.ErrorIs(t, store.Save(state), ErrCorrupt)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "broken", string(data))
			}
		})
	}
}

func TestStoreRejectsInvalidSave(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*State)
		err    error
	}{
		{"unsupported", func(s *State) { s.Version++ }, ErrUnsupportedVersion},
		{"checkpoint", func(s *State) { s.CaughtUp = s.Revision + 1 }, ErrCorrupt},
		{"duplicate ID", func(s *State) { s.Items = append(s.Items, s.Items[0]) }, ErrCorrupt},
		{"item revision", func(s *State) { s.Items[0].Revision++ }, ErrCorrupt},
		{"missing first seen", func(s *State) { s.Items[0].FirstSeen = time.Time{} }, ErrCorrupt},
		{"backwards timestamp", func(s *State) { s.Items[0].ChangedAt = observedTime.Add(-time.Second) }, ErrCorrupt},
		{"active with reason", func(s *State) { s.Items[0].InactiveReason = "interrupted" }, ErrCorrupt},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t, t.TempDir())
			var state State
			state.Reconcile([]Observation{observation("status", "paused")}, observedTime, false)
			require.NoError(t, store.Save(state))
			original := cloneState(t, state)
			tt.change(&state)
			require.ErrorIs(t, store.Save(state), tt.err)
			loaded, err := store.Load()
			require.NoError(t, err)
			require.Equal(t, original, loaded)
		})
	}
}

func TestStoreChangeRevision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change uint64
	}{
		{"zero", 0},
		{"exceeds item revision", 3},
		{"exceeds state revision", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := openStore(t, dir)
			var state State
			state.Reconcile([]Observation{observation("done", "completion")}, observedTime, false)
			require.NoError(t, state.SetRead(state.Items[0].ID, state.Items[0].Revision, true))
			require.NoError(t, state.CatchUp(state.Revision))
			require.NoError(t, store.Save(state))
			path := filepath.Join(dir, "state.json")
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			state.Items[0].ChangeRevision = tc.change
			require.ErrorIs(t, store.Save(state), ErrCorrupt)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, original, data)
			require.NoError(t, store.Close())

			corrupt, err := json.Marshal(state)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, corrupt, 0600))
			reopened := openStore(t, dir)
			_, err = reopened.Load()
			require.ErrorIs(t, err, ErrCorrupt)
			require.ErrorIs(t, reopened.Save(State{Version: snapshotVersion}), ErrCorrupt)
			data, err = os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, corrupt, data)
		})
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	store := openStore(t, t.TempDir())
	var state State
	state.Reconcile([]Observation{observation("status", "paused")}, observedTime, false)
	require.NoError(t, store.Save(state))
	var group sync.WaitGroup
	errors := make(chan error, 20)
	for range 10 {
		group.Go(func() { errors <- store.Save(state) })
		group.Go(func() { _, err := store.Load(); errors <- err })
	}
	group.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
}

func TestScope(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	baseline := Scope("daemon.lock", "tickets")
	for _, tt := range []struct {
		name, lock, tickets string
		same                bool
	}{
		{"absolute", filepath.Join(cwd, "daemon.lock"), filepath.Join(cwd, "tickets"), true},
		{"dot segments", "./daemon.lock", "./unused/../tickets/", true},
		{"different lock", "other.lock", "tickets", false},
		{"different tickets", "daemon.lock", "other-tickets", false},
		{"swapped paths", "tickets", "daemon.lock", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := Scope(tt.lock, tt.tickets)
			require.Len(t, scope, 64)
			if tt.same {
				require.Equal(t, baseline, scope)
			} else {
				require.NotEqual(t, baseline, scope)
			}
		})
	}
	logs := t.TempDir()
	first := openStore(t, filepath.Join(logs, "companion", baseline))
	second := openStore(t, filepath.Join(logs, "companion", Scope("other.lock", "tickets")))
	var state State
	state.Reconcile([]Observation{observation("first", "paused")}, observedTime, false)
	require.NoError(t, first.Save(state))
	other, err := second.Load()
	require.NoError(t, err)
	require.Empty(t, other.Items)
}
