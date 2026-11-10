package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	s, err := Open(t.TempDir(), "test-scope")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	require.NoError(t, s.Reconcile(map[string]Spec{"nightly": testSpec()}, now))
	defs, err := s.List()
	require.NoError(t, err)
	require.Len(t, defs, 1)
	return s, defs[0].ID, now
}

func TestStoreReconcileAndTombstones(t *testing.T) {
	s, id, now := openTestStore(t)
	first, err := s.Get(id)
	require.NoError(t, err)
	require.NoError(t, s.Reconcile(map[string]Spec{"nightly": testSpec().Normalize()}, now.Add(time.Minute)))
	same, err := s.Get(id)
	require.NoError(t, err)
	require.Equal(t, first, same)
	r, found, err := s.Allocate(id, "manual", "request", time.Time{}, time.Time{}, now)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, uint64(1), r.N)
	spec := testSpec()
	spec.Name = "Renamed"
	spec.Schedule = "6h"
	require.NoError(t, s.Reconcile(map[string]Spec{"nightly": spec}, now.Add(time.Minute)))
	updated, err := s.Get(id)
	require.NoError(t, err)
	require.Equal(t, first.Revision+1, updated.Revision)
	require.Equal(t, first.CreatedAt, updated.CreatedAt)
	require.Equal(t, now.Add(time.Minute+6*time.Hour), updated.Next)
	frozen, err := s.GetRun(id, r.N)
	require.NoError(t, err)
	require.Equal(t, first.Spec, frozen.Spec)
	require.NoError(t, s.Reconcile(nil, now.Add(2*time.Minute)))
	defs, err := s.List()
	require.NoError(t, err)
	require.Empty(t, defs)
	_, err = s.Get(id)
	require.ErrorIs(t, err, ErrNotFound)
	unfinished, err := s.UnfinishedRuns()
	require.NoError(t, err)
	require.Equal(t, []Run{r}, unfinished)
	_, _, err = s.Allocate(id, "manual", "new", time.Time{}, time.Time{}, now)
	require.ErrorIs(t, err, ErrNotFound)
	replay, found, err := s.Allocate(id, "manual", "request", time.Time{}, time.Time{}, now)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, r, replay)
	require.NoError(t, s.Reconcile(map[string]Spec{"nightly": spec}, now.Add(3*time.Minute)))
	defs, err = s.List()
	require.NoError(t, err)
	require.Len(t, defs, 1)
	require.NotEqual(t, id, defs[0].ID)
	r2, _, err := s.Allocate(defs[0].ID, "manual", "request", time.Time{}, time.Time{}, now.Add(3*time.Minute))
	require.NoError(t, err)
	require.Equal(t, uint64(2), r2.N)
	oldRuns, err := s.Runs(id)
	require.NoError(t, err)
	require.Len(t, oldRuns, 1)
	defs, err = s.List()
	require.NoError(t, err)
	invalid := spec
	invalid.Overlap = "bad"
	require.ErrorIs(t, s.Reconcile(map[string]Spec{"nightly": invalid}, now), ErrValidation)
	after, err := s.List()
	require.NoError(t, err)
	require.Equal(t, defs, after)
}

func TestStoreReceiptsAndCursorTransaction(t *testing.T) {
	s, id, now := openTestStore(t)
	d, err := s.Get(id)
	require.NoError(t, err)
	next := d.Next.Add(24 * time.Hour)
	r, found, err := s.Allocate(id, "schedule", "slot", d.Next, next, now)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, "queued", r.Phase)
	require.Equal(t, uint64(1), r.Revision)
	require.NotEmpty(t, r.InvocationID)
	d2, err := s.Get(id)
	require.NoError(t, err)
	require.Equal(t, next, d2.Next)
	require.Equal(t, d.Revision, d2.Revision)
	replay, found, err := s.Allocate(id, "schedule", "slot", d.Next, next.Add(24*time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, r, replay)
	d2, err = s.Get(id)
	require.NoError(t, err)
	require.Equal(t, next, d2.Next)
	_, _, err = s.Allocate(id, "schedule", "slot", d.Next.Add(time.Minute), next, now)
	require.ErrorIs(t, err, ErrConflict)
	_, _, err = s.Allocate(id, "manual", "slot", time.Time{}, time.Time{}, now)
	require.ErrorIs(t, err, ErrConflict)
	_, _, err = s.Allocate(id, "manual", "manual", time.Time{}, next.Add(time.Minute), now)
	require.ErrorIs(t, err, ErrValidation)
	_, _, err = s.Allocate(id, "manual", "manual", time.Time{}, time.Time{}, now)
	require.NoError(t, err)
	require.NoError(t, s.SetNext(id, next.Add(24*time.Hour)))
	require.ErrorIs(t, s.SetNext(id, next), ErrConflict)
	d2, err = s.Get(id)
	require.NoError(t, err)
	require.Equal(t, d.Revision, d2.Revision)
	implicit, found, err := s.Allocate(id, "schedule", "", next, time.Time{}, now)
	require.NoError(t, err)
	require.False(t, found)
	implicit2, found, err := s.Allocate(id, "schedule", "", next, time.Time{}, now)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, implicit, implicit2)
	runs, err := s.Runs(id)
	require.NoError(t, err)
	require.Len(t, runs, 3)
	require.Greater(t, runs[0].N, runs[1].N)
}

func TestStoreCallbackAtomicityAndIsolation(t *testing.T) {
	s, id, now := openTestStore(t)
	r, _, err := s.Allocate(id, "manual", "request", time.Time{}, time.Time{}, now)
	require.NoError(t, err)
	var retained *Run
	updated, err := s.UpdateRun(id, r.N, func(r *Run) error {
		retained = r
		r.Phase = "running"
		r.StartedAt = now
		r.ExitCode = new(0)
		r.Cost = new("0.31")
		r.Result = "0"
		return nil
	})
	require.NoError(t, err)
	retained.Result = "changed"
	*retained.ExitCode = 99
	*retained.Cost = "99"
	*updated.ExitCode = 88
	*updated.Cost = "88"
	saved, err := s.GetRun(id, r.N)
	require.NoError(t, err)
	require.Equal(t, "0", saved.Result)
	require.Equal(t, 0, *saved.ExitCode)
	require.Equal(t, "0.31", *saved.Cost)
	sentinel := errors.New("abort")
	_, err = s.UpdateRun(id, r.N, func(r *Run) error { r.Result = "discard"; *r.Cost = "999"; return sentinel })
	require.ErrorIs(t, err, sentinel)
	saved2, err := s.GetRun(id, r.N)
	require.NoError(t, err)
	require.Equal(t, saved, saved2)
	_, err = s.UpdateRun(id, r.N, func(r *Run) error { r.Spec.Name = "not frozen"; return nil })
	require.ErrorIs(t, err, ErrConflict)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for range 40 {
		wg.Go(func() {
			_, err := s.UpdateRun(id, r.N, func(r *Run) error { n, err := strconv.Atoi(r.Result); r.Result = strconv.Itoa(n + 1); return err })
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	saved, err = s.GetRun(id, r.N)
	require.NoError(t, err)
	require.Equal(t, "40", saved.Result)
	require.Equal(t, uint64(42), saved.Revision)
	defs, err := s.List()
	require.NoError(t, err)
	require.True(t, defs[0].Running)
	*defs[0].CurrentRun.Cost = "777"
	saved, err = s.GetRun(id, r.N)
	require.NoError(t, err)
	require.Equal(t, "0.31", *saved.Cost)
}

func TestStoreConcurrentAllocationReceipts(t *testing.T) {
	s, id, now := openTestStore(t)
	var wg sync.WaitGroup
	results := make(chan Run, 30)
	errs := make(chan error, 30)
	for range 30 {
		wg.Go(func() {
			r, _, err := s.Allocate(id, "manual", "same-request", time.Time{}, time.Time{}, now)
			results <- r
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	for r := range results {
		require.Equal(t, uint64(1), r.N)
	}
	runs, err := s.Runs(id)
	require.NoError(t, err)
	require.Len(t, runs, 1)
}

func TestStoreAggregatesFullHistory(t *testing.T) {
	s, id, now := openTestStore(t)
	d, err := s.Get(id)
	require.NoError(t, err)
	require.Nil(t, d.SuccessRate)
	require.Nil(t, d.AvgWall)
	require.Nil(t, d.Spend30d)
	for i, outcome := range []string{"complete", "failed", "skipped", "complete"} {
		accepted := now
		if i == 3 {
			accepted = now.AddDate(0, 0, -40)
		}
		r, _, err := s.Allocate(id, "manual", fmt.Sprintf("r%d", i), time.Time{}, time.Time{}, accepted)
		require.NoError(t, err)
		_, err = s.UpdateRun(id, r.N, func(r *Run) error {
			r.Phase = "terminal"
			r.Outcome = outcome
			r.CompletedAt = accepted.Add(2 * time.Minute)
			if outcome != "skipped" {
				r.StartedAt = accepted
			}
			if i == 0 {
				r.Cost = new("0.31")
			}
			return nil
		})
		require.NoError(t, err)
	}
	d, err = s.Get(id)
	require.NoError(t, err)
	require.Equal(t, 4, d.RunsTotal)
	require.InDelta(t, 200.0/3, *d.SuccessRate, 0.0001)
	require.Equal(t, 120.0, *d.AvgWall)
	require.Equal(t, 1, d.PricedRuns)
	require.Equal(t, 2, d.TrackedRuns)
	require.Equal(t, "0.310000", *d.Spend30d)
	require.False(t, d.Running)
	require.Equal(t, uint64(4), d.LastRun.N)
}

func TestStoreDurabilityPermissionsAndLock(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, "scope")
	require.NoError(t, err)
	_, err = Open(root, "scope")
	require.ErrorIs(t, err, ErrUnavailable)
	other, err := Open(root, "other")
	require.NoError(t, err)
	require.NoError(t, other.Close())
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.Reconcile(map[string]Spec{"nightly": testSpec()}, now))
	defs, err := s.List()
	require.NoError(t, err)
	r, _, err := s.Allocate(defs[0].ID, "manual", "request", time.Time{}, time.Time{}, now)
	require.NoError(t, err)
	for _, path := range []string{root, s.dir, filepath.Join(s.dir, "state.json"), filepath.Join(s.dir, "store.lock")} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		want := os.FileMode(0600)
		if info.IsDir() {
			want = 0700
		}
		require.Equal(t, want, info.Mode().Perm())
	}
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	_, err = s.List()
	require.ErrorIs(t, err, ErrUnavailable)
	s, err = Open(root, "scope")
	require.NoError(t, err)
	defer s.Close()
	replay, found, err := s.Allocate(defs[0].ID, "manual", "request", time.Time{}, time.Time{}, now)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, r, replay)
	require.NoError(t, s.Reconcile(nil, now))
	require.NoError(t, s.Close())
	s, err = Open(root, "scope")
	require.NoError(t, err)
	defer s.Close()
	unfinished, err := s.UnfinishedRuns()
	require.NoError(t, err)
	require.Equal(t, []Run{r}, unfinished)
	require.NoError(t, s.Reconcile(map[string]Spec{"nightly": testSpec()}, now))
	defs, err = s.List()
	require.NoError(t, err)
	require.NotEqual(t, r.AutomationID, defs[0].ID)
	nextRun, _, err := s.Allocate(defs[0].ID, "manual", "request", time.Time{}, time.Time{}, now)
	require.NoError(t, err)
	require.Equal(t, uint64(2), nextRun.N)
}

func TestStoreFailedAllocationIsAtomic(t *testing.T) {
	for _, stage := range []string{"create", "write", "sync", "close", "rename", "sync_dir"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			s, err := Open(root, "scope")
			require.NoError(t, err)
			now := time.Now().UTC()
			require.NoError(t, s.Reconcile(map[string]Spec{"nightly": testSpec()}, now))
			defs, err := s.List()
			require.NoError(t, err)
			d := defs[0]
			next := d.Next.Add(24 * time.Hour)
			s.fault = func(current string) error {
				if current == stage {
					return errors.New("disk fault")
				}
				return nil
			}
			_, _, err = s.Allocate(d.ID, "schedule", "slot", d.Next, next, now)
			require.ErrorIs(t, err, ErrUnavailable)
			_, err = s.List()
			require.ErrorIs(t, err, ErrUnavailable)
			require.NoError(t, s.Close())
			s, err = Open(root, "scope")
			require.NoError(t, err)
			defer s.Close()
			persisted, err := s.Get(d.ID)
			require.NoError(t, err)
			runs, err := s.Runs(d.ID)
			require.NoError(t, err)
			if stage == "sync_dir" {
				require.Len(t, runs, 1)
				require.Equal(t, next, persisted.Next)
			} else {
				require.Empty(t, runs)
				require.Equal(t, d.Next, persisted.Next)
			}
			_, found, err := s.Allocate(d.ID, "schedule", "slot", d.Next, next, now)
			require.NoError(t, err)
			require.Equal(t, stage == "sync_dir", found)
		})
	}
}

func TestStoreRejectsExternalChangesAndCorruption(t *testing.T) {
	s, _, _ := openTestStore(t)
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, "state.json"), append(s.data, ' '), 0600))
	_, err := s.List()
	require.ErrorIs(t, err, ErrUnavailable)
	for _, change := range []string{"missing", "duplicate", "trailing", "unknown", "null"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			s, err := Open(root, "scope")
			require.NoError(t, err)
			path := filepath.Join(s.dir, "state.json")
			data := s.data
			require.NoError(t, s.Close())
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &fields))
			switch change {
			case "missing":
				delete(fields, "counters")
				data, err = json.Marshal(fields)
			case "duplicate":
				data = append([]byte(`{"version":2,`), data[1:]...)
			case "trailing":
				data = append(data, []byte(`{}`)...)
			case "unknown":
				fields["extra"] = json.RawMessage(`1`)
				data, err = json.Marshal(fields)
			case "null":
				fields["counters"] = json.RawMessage(`null`)
				data, err = json.Marshal(fields)
			}
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			_, err = Open(root, "scope")
			require.ErrorIs(t, err, ErrUnavailable)
		})
	}
}
