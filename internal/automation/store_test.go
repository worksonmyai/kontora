package automation

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var storeNow = instant("2026-03-02T08:00:00Z")

func definition() Definition {
	return Definition{Name: "Daily review", Path: "~/projects/kontora", Agent: "reviewer", Instructions: "Review changes.\nReport regressions.", Schedule: Schedule{Frequency: "weekly", Days: []int{1, 3, 5}, Time: "09:00", Timezone: "UTC"}}
}

func openStore(t *testing.T, root, primary string) *Store {
	t.Helper()
	s, err := Open(root, primary)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func createDefinition(t *testing.T, s *Store) Definition {
	t.Helper()
	d, err := s.Create("create", definition(), storeNow)
	require.NoError(t, err)
	return d
}

func TestStoreDefinitionsAndReceipts(t *testing.T) {
	root, primary := t.TempDir(), t.TempDir()
	s := openStore(t, root, primary)
	input := definition()
	input.Enabled = true
	input.Next = storeNow.Add(-time.Hour)
	d, err := s.Create("request-1", input, storeNow)
	require.NoError(t, err)
	require.True(t, validID(d.ID))
	require.Equal(t, uint64(1), d.Revision)
	require.Equal(t, uint64(1), d.ScheduleRevision)
	require.False(t, d.Enabled)
	require.False(t, d.SkipOverlap)
	require.Equal(t, instant("2026-03-02T09:00:00Z"), d.Next)
	original := cloneDefinition(d)
	d.Schedule.Days[0] = 0
	got, err := s.Get(d.ID)
	require.NoError(t, err)
	require.Equal(t, original, got)
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	list[0].Schedule.Days[0] = 0
	got, err = s.Get(d.ID)
	require.NoError(t, err)
	require.Equal(t, original, got)

	now := instant("2026-03-03T10:00:00Z")
	got.Enabled = true
	got.Next = storeNow
	updated, err := s.Update(got, now)
	require.NoError(t, err)
	require.Equal(t, uint64(2), updated.Revision)
	require.Equal(t, uint64(1), updated.ScheduleRevision)
	require.Equal(t, instant("2026-03-04T09:00:00Z"), updated.Next)
	_, err = s.Update(got, now)
	require.ErrorIs(t, err, ErrConflict)
	updated.Instructions = "Changed instructions"
	updated, err = s.Update(updated, now)
	require.NoError(t, err)
	require.Equal(t, uint64(1), updated.ScheduleRevision)
	updated.Schedule.Time = "10:00"
	updated, err = s.Update(updated, now)
	require.NoError(t, err)
	require.Equal(t, uint64(2), updated.ScheduleRevision)
	require.Equal(t, instant("2026-03-04T10:00:00Z"), updated.Next)
	require.NoError(t, s.SetNext(updated.ID, instant("2026-03-06T10:00:00Z")))
	current, err := s.Get(updated.ID)
	require.NoError(t, err)
	require.Equal(t, updated.Revision, current.Revision)
	require.Equal(t, updated.ScheduleRevision, current.ScheduleRevision)
	updated.Name = "Renamed"
	updated, err = s.Update(updated, now)
	require.NoError(t, err)
	require.Equal(t, current.Next, updated.Next)

	replay, err := s.Create("request-1", input, now)
	require.NoError(t, err)
	require.Equal(t, original, replay)
	input.Name = "Different"
	_, err = s.Create("request-1", input, now)
	require.ErrorIs(t, err, ErrConflict)
	require.NoError(t, s.Close())
	s = openStore(t, root, primary)
	input = definition()
	input.Enabled = true
	input.Next = storeNow.Add(-time.Hour)
	replay, err = s.Create("request-1", input, now)
	require.NoError(t, err)
	require.Equal(t, original, replay)
	got, err = s.Get(original.ID)
	require.NoError(t, err)
	require.Equal(t, updated, got)
	_, err = s.Get("missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStoreRescheduleAndEnable(t *testing.T) {
	s := openStore(t, t.TempDir(), t.TempDir())
	d := createDefinition(t, s)
	d.Enabled = true
	d, err := s.Update(d, instant("2026-03-02T09:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, instant("2026-03-04T09:00:00Z"), d.Next)
	d.Enabled = false
	d, err = s.Update(d, instant("2026-03-06T10:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, instant("2026-03-04T09:00:00Z"), d.Next)
	d.Enabled = true
	d, err = s.Update(d, instant("2026-03-06T10:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, instant("2026-03-09T09:00:00Z"), d.Next)
	d.Enabled = false
	d.Schedule.Time = "11:00"
	d, err = s.Update(d, instant("2026-03-06T10:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, instant("2026-03-06T11:00:00Z"), d.Next)
	before := d.ScheduleRevision
	d.Schedule.Days = []int{5, 3, 1}
	d, err = s.Update(d, storeNow)
	require.NoError(t, err)
	require.Equal(t, before, d.ScheduleRevision)
	require.ErrorIs(t, s.SetNext(d.ID, d.Next.Add(-time.Hour)), ErrConflict)
	require.ErrorIs(t, s.SetNext(d.ID, d.Next.Add(time.Hour)), ErrValidation)
	require.ErrorIs(t, s.SetNext(d.ID, time.Time{}), ErrValidation)
}

func TestStoreOccurrenceFreezeAndRevisions(t *testing.T) {
	root, primary := t.TempDir(), t.TempDir()
	s := openStore(t, root, primary)
	d := createDefinition(t, s)
	o, err := s.SaveOccurrence(Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: d.Next, Phase: "reserved"})
	require.NoError(t, err)
	require.True(t, validID(o.ID))
	require.Equal(t, uint64(1), o.Revision)
	require.Equal(t, d, o.Payload)
	_, err = s.SaveOccurrence(Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: d.Next, Phase: "reserved"})
	require.ErrorIs(t, err, ErrConflict)
	stale := o
	o.Phase = "publishing"
	o.TicketID = "kon-test"
	o, err = s.SaveOccurrence(o)
	require.NoError(t, err)
	require.Equal(t, uint64(2), o.Revision)
	_, err = s.SaveOccurrence(stale)
	require.ErrorIs(t, err, ErrConflict)
	for _, change := range []func(*Occurrence){
		func(o *Occurrence) { o.Payload.Instructions = "Replaced" },
		func(o *Occurrence) { o.Payload.Schedule.Days[0] = 0 },
		func(o *Occurrence) { o.TicketID = "kon-other" },
		func(o *Occurrence) { o.ScheduledFor = o.ScheduledFor.AddDate(0, 0, 2) },
		func(o *Occurrence) { o.ScheduleRevision++ },
		func(o *Occurrence) { o.Phase = "reserved" },
	} {
		changed := o
		changed.Payload = cloneDefinition(o.Payload)
		change(&changed)
		_, err = s.SaveOccurrence(changed)
		require.ErrorIs(t, err, ErrConflict)
	}
	d.Instructions = "New instructions"
	d.Schedule.Time = "10:00"
	d, err = s.Update(d, storeNow)
	require.NoError(t, err)
	o.Phase = "admitted"
	o, err = s.SaveOccurrence(o)
	require.NoError(t, err)
	require.Equal(t, uint64(3), o.Revision)
	require.NotEqual(t, d.Instructions, o.Payload.Instructions)
	require.NotEqual(t, d.ScheduleRevision, o.ScheduleRevision)
	frozen := o
	frozen.Payload = cloneDefinition(o.Payload)
	o.Payload.Schedule.Days[0] = 0
	occurrences, err := s.Occurrences(d.ID)
	require.NoError(t, err)
	require.Equal(t, []Occurrence{frozen}, occurrences)
	occurrences[0].Payload.Schedule.Days[0] = 0
	require.NoError(t, s.Close())
	s = openStore(t, root, primary)
	occurrences, err = s.Occurrences(d.ID)
	require.NoError(t, err)
	require.Equal(t, []Occurrence{frozen}, occurrences)
	frozen.Phase = "failed"
	_, err = s.SaveOccurrence(frozen)
	require.ErrorIs(t, err, ErrConflict)
	_, err = s.Occurrences("missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStoreValidation(t *testing.T) {
	s := openStore(t, t.TempDir(), t.TempDir())
	for _, tt := range []struct {
		name   string
		change func(*Definition)
	}{
		{"name", func(d *Definition) { d.Name = " " }},
		{"long name", func(d *Definition) { d.Name = strings.Repeat("x", 201) }},
		{"path", func(d *Definition) { d.Path = "" }},
		{"nul", func(d *Definition) { d.Path = "bad\x00path" }},
		{"agent", func(d *Definition) { d.Agent = "" }},
		{"instructions", func(d *Definition) { d.Instructions = "" }},
		{"long instructions", func(d *Definition) { d.Instructions = strings.Repeat("x", 65537) }},
		{"invalid utf8", func(d *Definition) { d.Name = string([]byte{0xff}) }},
		{"schedule", func(d *Definition) { d.Schedule.Timezone = "Local" }},
		{"identity", func(d *Definition) { d.ID = newID() }},
		{"revision", func(d *Definition) { d.Revision = 1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := definition()
			tt.change(&d)
			_, err := s.Create(tt.name, d, storeNow)
			require.ErrorIs(t, err, ErrValidation)
		})
	}
	_, err := s.Create("", definition(), storeNow)
	require.ErrorIs(t, err, ErrValidation)
	_, err = s.Create("request", definition(), time.Time{})
	require.ErrorIs(t, err, ErrValidation)
	d := createDefinition(t, s)
	for _, change := range []func(*Occurrence){
		func(o *Occurrence) { o.Phase = "unknown" },
		func(o *Occurrence) { o.ScheduledFor = o.ScheduledFor.Add(time.Hour) },
		func(o *Occurrence) { o.Phase = "admitted" },
		func(o *Occurrence) { o.ID = "not-random" },
		func(o *Occurrence) { o.Reason = strings.Repeat("x", 4097) },
	} {
		o := Occurrence{AutomationID: d.ID, ScheduleRevision: 1, ScheduledFor: d.Next, Phase: "reserved"}
		change(&o)
		_, err := s.SaveOccurrence(o)
		require.ErrorIs(t, err, ErrValidation)
	}
	for _, phase := range []string{"skipped", "blocked", "failed"} {
		next, err := d.Schedule.Next(d.Next)
		require.NoError(t, err)
		d.Next = next
		_, err = s.SaveOccurrence(Occurrence{AutomationID: d.ID, ScheduleRevision: 1, ScheduledFor: next, Phase: phase})
		require.NoError(t, err)
	}
}

func TestStoreSaveFailures(t *testing.T) {
	for _, stage := range []string{"create", "write", "sync", "close", "rename", "sync_dir"} {
		t.Run(stage, func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			s := openStore(t, root, primary)
			d := createDefinition(t, s)
			before, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
			require.NoError(t, err)
			injected := errors.New("injected failure")
			s.fault = func(at string) error {
				if at == stage {
					return injected
				}
				return nil
			}
			input := definition()
			input.Name = "Second"
			_, err = s.Create("second", input, storeNow)
			require.ErrorIs(t, err, ErrUnavailable)
			require.ErrorIs(t, err, injected)
			s.fault = nil
			_, err = s.List()
			require.ErrorIs(t, err, ErrUnavailable)
			_, err = s.Get(d.ID)
			require.ErrorIs(t, err, ErrUnavailable)
			_, err = s.Create("second", input, storeNow)
			require.ErrorIs(t, err, ErrUnavailable)
			result, found, err := s.CreateReceipt("create", definition())
			require.ErrorIs(t, err, ErrUnavailable)
			require.False(t, found)
			require.Empty(t, result)
			_, err = s.Update(d, storeNow)
			require.ErrorIs(t, err, ErrUnavailable)
			_, err = s.Occurrences(d.ID)
			require.ErrorIs(t, err, ErrUnavailable)
			_, err = s.SaveOccurrence(Occurrence{})
			require.ErrorIs(t, err, ErrUnavailable)
			require.ErrorIs(t, s.SetNext(d.ID, d.Next), ErrUnavailable)
			after, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
			require.NoError(t, err)
			if stage != "sync_dir" {
				require.Equal(t, before, after)
			}
			temps, err := filepath.Glob(filepath.Join(s.dir, ".state-*"))
			require.NoError(t, err)
			require.Empty(t, temps)
			require.NoError(t, s.Close())
			s = openStore(t, root, primary)
			second, err := s.Create("second", input, storeNow)
			require.NoError(t, err)
			replay, err := s.Create("second", input, storeNow)
			require.NoError(t, err)
			require.Equal(t, second, replay)
			list, err := s.List()
			require.NoError(t, err)
			require.Len(t, list, 2)
		})
	}
}

func TestStoreStrictSnapshots(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"empty", func(_ []byte) []byte { return []byte{} }},
		{"truncated", func(b []byte) []byte { return b[:len(b)/2] }},
		{"trailing", func(b []byte) []byte { return append(b, []byte("{}")...) }},
		{"null", func(_ []byte) []byte { return []byte("null") }},
		{"version", func(b []byte) []byte { return []byte(strings.Replace(string(b), `"version":1`, `"version":2`, 1)) }},
		{"duplicate", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"version":1`, `"version":1,"version":1`, 1))
		}},
		{"unknown", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"version":1`, `"version":1,"unknown":1`, 1))
		}},
		{"case insensitive", func(b []byte) []byte { return []byte(strings.Replace(string(b), `"version":1`, `"Version":1`, 1)) }},
		{"missing field", func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), `"enabled":false,`, "")) }},
		{"missing primary directory", func(b []byte) []byte {
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(b, &fields))
			delete(fields, "primary_dir")
			out, err := json.Marshal(fields)
			require.NoError(t, err)
			return out
		}},
		{"receipt input changed", func(b []byte) []byte {
			var st snapshot
			require.NoError(t, json.Unmarshal(b, &st))
			r := st.Receipts["create"]
			r.Input.Enabled = true
			st.Receipts["create"] = r
			out, err := json.Marshal(st)
			require.NoError(t, err)
			return out
		}},
		{"receipt result changed", func(b []byte) []byte {
			var st snapshot
			require.NoError(t, json.Unmarshal(b, &st))
			r := st.Receipts["create"]
			r.Result.Name = "Not the accepted name"
			st.Receipts["create"] = r
			out, err := json.Marshal(st)
			require.NoError(t, err)
			return out
		}},
		{"zero revision", func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), `"revision":1`, `"revision":0`)) }},
		{"null days", func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), `"days":[1,3,5]`, `"days":null`)) }},
		{"scope", func(b []byte) []byte {
			var st snapshot
			require.NoError(t, json.Unmarshal(b, &st))
			st.Scope = strings.Repeat("0", 64)
			out, err := json.Marshal(st)
			require.NoError(t, err)
			return out
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			s := openStore(t, root, primary)
			createDefinition(t, s)
			path := filepath.Join(s.dir, "state.json")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, s.Close())
			bad := tt.mutate(data)
			require.NoError(t, os.WriteFile(path, bad, 0600))
			other, err := Open(root, primary)
			require.ErrorIs(t, err, ErrUnavailable)
			require.Nil(t, other)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, bad, after)
		})
	}
}

func TestStoreOwnershipScopesAndPermissions(t *testing.T) {
	root, primary := t.TempDir(), t.TempDir()
	s := openStore(t, root, primary)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(primary, alias))
	for _, path := range []string{primary, alias, filepath.Join(primary, "unused", "..")} {
		other, err := Open(root, path)
		require.ErrorIs(t, err, ErrUnavailable)
		require.Nil(t, other)
	}
	other := openStore(t, root, t.TempDir())
	createDefinition(t, s)
	list, err := other.List()
	require.NoError(t, err)
	require.Empty(t, list)
	for path, mode := range map[string]os.FileMode{root: 0700, s.dir: 0700, filepath.Join(s.dir, "store.lock"): 0600, filepath.Join(s.dir, "state.json"): 0600} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm())
	}
	checkLockProcess(t, root, primary, true)
	before, err := os.Stat(filepath.Join(s.dir, "store.lock"))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	checkLockProcess(t, root, primary, false)
	after, err := os.Stat(filepath.Join(s.dir, "store.lock"))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	_, err = s.List()
	require.ErrorIs(t, err, ErrUnavailable)
}

func checkLockProcess(t *testing.T, root, primary string, busy bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAutomationLockProcess$")
	cmd.Env = append(os.Environ(), "AUTOMATION_LOCK_ROOT="+root, "AUTOMATION_LOCK_PRIMARY="+primary)
	if busy {
		cmd.Env = append(cmd.Env, "AUTOMATION_LOCK_BUSY=1")
	}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestAutomationLockProcess(t *testing.T) {
	root := os.Getenv("AUTOMATION_LOCK_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(root, os.Getenv("AUTOMATION_LOCK_PRIMARY"))
	if os.Getenv("AUTOMATION_LOCK_BUSY") == "1" {
		require.ErrorIs(t, err, ErrUnavailable)
		require.Nil(t, s)
	} else {
		require.NoError(t, err)
		require.NoError(t, s.Close())
	}
}

func TestStoreFilesystemProtection(t *testing.T) {
	for _, kind := range []string{"snapshot symlink", "snapshot directory", "lock symlink", "scope symlink", "external change", "deleted snapshot"} {
		t.Run(kind, func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			s := openStore(t, root, primary)
			createDefinition(t, s)
			path := filepath.Join(s.dir, "state.json")
			switch kind {
			case "external change":
				require.NoError(t, os.WriteFile(path, []byte("broken"), 0600))
				_, err := s.List()
				require.ErrorIs(t, err, ErrUnavailable)
				return
			case "deleted snapshot":
				require.NoError(t, os.Remove(path))
				_, err := s.List()
				require.ErrorIs(t, err, ErrUnavailable)
				return
			}
			require.NoError(t, s.Close())
			switch kind {
			case "snapshot symlink":
				target := filepath.Join(t.TempDir(), "target")
				require.NoError(t, os.WriteFile(target, []byte("preserve"), 0600))
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(target, path))
			case "snapshot directory":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0700))
			case "lock symlink":
				path = filepath.Join(s.dir, "store.lock")
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "target"), path))
			case "scope symlink":
				moved := s.dir + "-moved"
				require.NoError(t, os.Rename(s.dir, moved))
				require.NoError(t, os.Symlink(moved, s.dir))
			}
			other, err := Open(root, primary)
			require.ErrorIs(t, err, ErrUnavailable)
			require.Nil(t, other)
		})
	}
}

func TestStoreDefaultRootAndMissingPrimary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := openStore(t, "", filepath.Join(t.TempDir(), "not-created", "tickets"))
	expected, err := canonical(filepath.Join(home, ".kontora", "automations"))
	require.NoError(t, err)
	require.Equal(t, expected, filepath.Dir(s.dir))
	_, err = Open(t.TempDir(), "")
	require.ErrorIs(t, err, ErrValidation)
}

func TestStoreConcurrentRequests(t *testing.T) {
	s := openStore(t, t.TempDir(), t.TempDir())
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() { d, err := s.Create("one-request", definition(), storeNow); ids <- d.ID; errs <- err })
	}
	wg.Wait()
	close(ids)
	close(errs)
	var id string
	for got := range ids {
		if id == "" {
			id = got
		}
		require.Equal(t, id, got)
	}
	for err := range errs {
		require.NoError(t, err)
	}
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
}
