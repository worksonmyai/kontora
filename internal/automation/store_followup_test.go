package automation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStoreRetainedHistorySurvivesRestart(t *testing.T) {
	root, primary := t.TempDir(), t.TempDir()
	s := openStore(t, root, primary)
	input := definition()
	input.Instructions = strings.Repeat("x", 60000)
	input.Schedule = Schedule{Frequency: "daily", Time: "09:00", Timezone: "UTC"}
	d, err := s.Create("large-history", input, storeNow)
	require.NoError(t, err)
	st := s.copyState()
	for day := range 1120 {
		o := Occurrence{ID: newID(), Revision: 1, AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: d.Next.AddDate(0, 0, day), Count: 1, Phase: "skipped", Payload: d}
		st.Occurrences[o.ID] = o
	}
	require.NoError(t, s.save(st))
	info, err := os.Stat(filepath.Join(s.dir, "state.json"))
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(64<<20))
	require.NoError(t, s.Close())
	s = openStore(t, root, primary)
	got, err := s.Get(d.ID)
	require.NoError(t, err)
	require.Equal(t, d, got)
	occurrences, err := s.Occurrences(d.ID)
	require.NoError(t, err)
	require.Len(t, occurrences, len(st.Occurrences))
	for _, occurrence := range occurrences {
		require.Equal(t, st.Occurrences[occurrence.ID], occurrence)
	}
}

func TestStorePrimaryDirectoryIdentity(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "missing directory"} {
		t.Run(kind, func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			alias := filepath.Join(t.TempDir(), "alias")
			require.NoError(t, os.Symlink(primary, alias))
			input := primary
			switch kind {
			case "symlink":
				input = alias
			case "missing directory":
				input = filepath.Join(alias, "not-created", "tickets")
				primary = filepath.Join(primary, "not-created", "tickets")
			}
			expected, err := canonical(primary)
			require.NoError(t, err)
			s := openStore(t, root, input)
			data, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
			require.NoError(t, err)
			var st snapshot
			require.NoError(t, decodeSnapshot(data, &st))
			require.Equal(t, expected, st.PrimaryDir)
			hash := sha256.Sum256([]byte(expected))
			require.Equal(t, hex.EncodeToString(hash[:]), st.Scope)
			require.NoError(t, s.Close())
			s = openStore(t, root, primary)
			require.Equal(t, expected, s.state.PrimaryDir)
		})
	}
}

func TestStoreRejectsPrimaryDirectoryMismatch(t *testing.T) {
	for _, tt := range []struct {
		name       string
		mutate     func(*snapshot)
		validShape bool
	}{
		{"empty", func(st *snapshot) { st.PrimaryDir = "" }, false},
		{"relative", func(st *snapshot) { st.PrimaryDir = "tickets" }, false},
		{"unclean", func(st *snapshot) { st.PrimaryDir += "/unused/.." }, false},
		{"different path", func(st *snapshot) { st.PrimaryDir += "-other" }, false},
		{"different scope", func(st *snapshot) { st.Scope = strings.Repeat("0", 64) }, false},
		{"different identity", func(st *snapshot) {
			st.PrimaryDir += "-other"
			hash := sha256.Sum256([]byte(st.PrimaryDir))
			st.Scope = hex.EncodeToString(hash[:])
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			s := openStore(t, root, primary)
			st := s.copyState()
			path := filepath.Join(s.dir, "state.json")
			require.NoError(t, s.Close())
			tt.mutate(&st)
			err := validateSnapshot(st)
			if tt.validShape {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			data, err := json.Marshal(st)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			other, err := Open(root, primary)
			require.ErrorIs(t, err, ErrUnavailable)
			require.Nil(t, other)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, data, after)
		})
	}
}

func TestStoreEnableCursorFloor(t *testing.T) {
	for _, tt := range []struct {
		name    string
		now     string
		cursor  string
		phase   string
		through string
		change  string
		want    string
	}{
		{"rollback before cursor", "2026-02-27T08:00:00Z", "2026-03-06T09:00:00Z", "", "", "", "2026-03-06T09:00:00Z"},
		{"clock at cursor", "2026-03-06T09:00:00Z", "2026-03-06T09:00:00Z", "", "", "", "2026-03-09T09:00:00Z"},
		{"clock after cursor", "2026-03-10T08:00:00Z", "2026-03-06T09:00:00Z", "", "", "", "2026-03-11T09:00:00Z"},
		{"saved slot without cursor save", "2026-02-27T08:00:00Z", "", "failed", "", "", "2026-03-04T09:00:00Z"},
		{"reserved slot", "2026-02-27T08:00:00Z", "", "reserved", "", "", "2026-03-04T09:00:00Z"},
		{"blocked slot", "2026-02-27T08:00:00Z", "", "blocked", "", "", "2026-03-04T09:00:00Z"},
		{"saved range without cursor save", "2026-03-03T08:00:00Z", "", "skipped", "2026-03-06T09:00:00Z", "", "2026-03-09T09:00:00Z"},
		{"cursor beyond saved range", "2026-02-27T08:00:00Z", "2026-03-11T09:00:00Z", "skipped", "2026-03-06T09:00:00Z", "", "2026-03-11T09:00:00Z"},
		{"schedule change permits earlier cursor", "2026-02-27T08:00:00Z", "2026-03-11T09:00:00Z", "skipped", "2026-03-06T09:00:00Z", "time", "2026-02-27T10:00:00Z"},
		{"new revision can reuse recorded slot", "2026-03-02T08:00:00Z", "2026-03-11T09:00:00Z", "skipped", "2026-03-06T09:00:00Z", "days", "2026-03-02T09:00:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			s := openStore(t, root, primary)
			d := createDefinition(t, s)
			d.Enabled = true
			d, err := s.Update(d, storeNow)
			require.NoError(t, err)
			var recorded Occurrence
			if tt.phase != "" {
				o := Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: d.Next, Phase: tt.phase}
				if tt.through != "" {
					o.Through = instant(tt.through)
					_, count, err := d.Schedule.LatestDue(d.Next, o.Through)
					require.NoError(t, err)
					o.Count = count
				}
				var err error
				recorded, err = s.SaveOccurrence(o)
				require.NoError(t, err)
			}
			if tt.cursor != "" {
				require.NoError(t, s.SetNext(d.ID, instant(tt.cursor)))
			}
			d.Enabled = false
			_, err = s.Update(d, instant("2026-03-12T10:00:00Z"))
			require.NoError(t, err)
			require.NoError(t, s.Close())
			s = openStore(t, root, primary)
			d, err = s.Get(d.ID)
			require.NoError(t, err)
			old := cloneDefinition(d)
			d.Enabled = true
			d.Schedule.Days = []int{5, 3, 1}
			switch tt.change {
			case "time":
				d.Schedule.Time = "10:00"
			case "days":
				d.Schedule.Days = []int{1, 3}
			}
			d, err = s.Update(d, instant(tt.now))
			require.NoError(t, err)
			require.Equal(t, instant(tt.want), d.Next)
			require.Equal(t, old.Revision+1, d.Revision)
			if tt.change != "" {
				require.Equal(t, old.ScheduleRevision+1, d.ScheduleRevision)
			} else {
				require.Equal(t, old.ScheduleRevision, d.ScheduleRevision)
				require.False(t, d.Next.Before(old.Next))
				if tt.phase != "" {
					end := recorded.ScheduledFor
					if !recorded.Through.IsZero() {
						end = recorded.Through
					}
					require.True(t, d.Next.After(end))
				}
			}
			require.NoError(t, s.Close())
			s = openStore(t, root, primary)
			got, err := s.Get(d.ID)
			require.NoError(t, err)
			require.Equal(t, d, got)
			records, err := s.Occurrences(d.ID)
			require.NoError(t, err)
			if tt.phase != "" {
				require.Equal(t, []Occurrence{recorded}, records)
			}
		})
	}
}

func TestStoreCreateReplayAfterEdits(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "reopened"}[restart], func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			s := openStore(t, root, primary)
			input := definition()
			input.Path = filepath.Join(t.TempDir(), "removed-repository")
			require.NoError(t, os.Mkdir(input.Path, 0700))
			input.Schedule.Days = []int{5, 1, 3}
			input.Enabled = true
			input.Next = storeNow.Add(-time.Hour)
			original, err := s.Create("request", input, storeNow)
			require.NoError(t, err)
			edited := cloneDefinition(original)
			edited.Name, edited.Path, edited.Agent, edited.Instructions = "Renamed", "/another/repository", "another-agent", "New instructions"
			edited.Schedule.Time, edited.Enabled, edited.SkipOverlap = "10:00", true, true
			edited, err = s.Update(edited, storeNow.AddDate(0, 0, 7))
			require.NoError(t, err)
			require.NoError(t, os.RemoveAll(input.Path))
			if restart {
				require.NoError(t, s.Close())
				s = openStore(t, root, primary)
			}
			before, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
			require.NoError(t, err)
			s.fault = func(string) error { t.Fatal("receipt replay must not save"); return nil }
			for _, method := range []string{"receipt", "create"} {
				t.Run(method, func(t *testing.T) {
					var result Definition
					var err error
					if method == "receipt" {
						var found bool
						result, found, err = s.CreateReceipt("request", input)
						require.True(t, found)
					} else {
						result, err = s.Create("request", input, time.Time{})
					}
					require.NoError(t, err)
					require.Equal(t, original, result)
					result.Schedule.Days[0] = 0
				})
			}
			current, err := s.Get(original.ID)
			require.NoError(t, err)
			require.Equal(t, edited, current)
			after, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestStoreCreateReplayPayloadConflicts(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Definition)
	}{
		{"identity", func(d *Definition) { d.ID = newID() }},
		{"revision", func(d *Definition) { d.Revision = 1 }},
		{"schedule revision", func(d *Definition) { d.ScheduleRevision = 1 }},
		{"next", func(d *Definition) { d.Next = storeNow }},
		{"invalid next", func(d *Definition) { d.Next = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }},
		{"enabled", func(d *Definition) { d.Enabled = true }},
		{"name", func(d *Definition) { d.Name += " changed" }},
		{"path", func(d *Definition) { d.Path += "/other" }},
		{"agent", func(d *Definition) { d.Agent += "-other" }},
		{"instructions", func(d *Definition) { d.Instructions += "\nExtra step." }},
		{"overlap", func(d *Definition) { d.SkipOverlap = true }},
		{"frequency", func(d *Definition) { d.Schedule.Frequency = "daily" }},
		{"days", func(d *Definition) { d.Schedule.Days = []int{1, 3} }},
		{"day order", func(d *Definition) { d.Schedule.Days = []int{5, 3, 1} }},
		{"time", func(d *Definition) { d.Schedule.Time = "10:00" }},
		{"timezone", func(d *Definition) { d.Schedule.Timezone = "Europe/Paris" }},
		{"invalid payload", func(d *Definition) { d.Name = "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t, t.TempDir(), t.TempDir())
			createDefinition(t, s)
			input := definition()
			tt.change(&input)
			result, found, err := s.CreateReceipt("create", input)
			require.ErrorIs(t, err, ErrConflict)
			require.False(t, found)
			require.Empty(t, result)
			result, err = s.Create("create", input, storeNow)
			require.ErrorIs(t, err, ErrConflict)
			require.Empty(t, result)
		})
	}
}

func TestStoreCreateReceiptEmptyWeekdays(t *testing.T) {
	for _, days := range [][]int{nil, {}} {
		name := "empty"
		if days == nil {
			name = "nil"
		}
		t.Run(name, func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			s := openStore(t, root, primary)
			input := definition()
			input.Schedule.Frequency, input.Schedule.Days = "daily", days
			original, err := s.Create("request", input, storeNow)
			require.NoError(t, err)
			require.NoError(t, s.Close())
			s = openStore(t, root, primary)
			if days == nil {
				input.Schedule.Days = []int{}
			} else {
				input.Schedule.Days = nil
			}
			result, found, err := s.CreateReceipt("request", input)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, original, result)
		})
	}
}

func TestStoreCreateReceiptMissesAndErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		request string
		prepare func(*testing.T, *Store)
		wantErr error
	}{
		{"missing", "missing", func(*testing.T, *Store) {}, nil},
		{"invalid request", "", func(*testing.T, *Store) {}, ErrValidation},
		{"long request", strings.Repeat("x", 201), func(*testing.T, *Store) {}, ErrValidation},
		{"closed", "create", func(t *testing.T, s *Store) { require.NoError(t, s.Close()) }, ErrUnavailable},
		{"corrupt", "create", func(t *testing.T, s *Store) {
			require.NoError(t, os.WriteFile(filepath.Join(s.dir, "state.json"), []byte("broken"), 0600))
		}, ErrUnavailable},
		{"deleted", "create", func(t *testing.T, s *Store) {
			require.NoError(t, os.Remove(filepath.Join(s.dir, "state.json")))
		}, ErrUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t, t.TempDir(), t.TempDir())
			createDefinition(t, s)
			tt.prepare(t, s)
			input := definition()
			if tt.name == "missing" {
				input = Definition{}
			}
			result, found, err := s.CreateReceipt(tt.request, input)
			if tt.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.wantErr)
			}
			require.False(t, found)
			require.Empty(t, result)
		})
	}
}
