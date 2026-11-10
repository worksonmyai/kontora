package automation

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefinitionTextValidation(t *testing.T) {
	for _, field := range []struct {
		name  string
		limit int
		set   func(*Definition, string)
	}{
		{"name", 200, func(d *Definition, value string) { d.Name = value }},
		{"path", 4096, func(d *Definition, value string) { d.Path = value }},
		{"agent", 200, func(d *Definition, value string) { d.Agent = value }},
		{"instructions", 65536, func(d *Definition, value string) { d.Instructions = value }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, tt := range []struct {
				name, value, message string
			}{
				{"limit", strings.Repeat("x", field.limit), ""},
				{"multibyte limit", strings.Repeat("é", field.limit/2), ""},
				{"too long", strings.Repeat("x", field.limit+1), field.name + " must be at most " + strconv.Itoa(field.limit) + " bytes"},
				{"multibyte too long", strings.Repeat("é", field.limit/2) + "x", field.name + " must be at most " + strconv.Itoa(field.limit) + " bytes"},
				{"blank", " \t ", "invalid " + field.name},
				{"invalid UTF-8", "\xff", "invalid " + field.name},
				{"control", "text\x00", "invalid " + field.name},
			} {
				t.Run(tt.name, func(t *testing.T) {
					d := definition()
					field.set(&d, tt.value)
					err := validateDefinition(d)
					if tt.message == "" {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, ErrValidation)
						require.EqualError(t, err, "invalid automation: "+tt.message)
					}
				})
			}
		})
	}
}

func TestOccurrenceRangesSurviveRestart(t *testing.T) {
	root, primary := t.TempDir(), t.TempDir()
	s := openStore(t, root, primary)
	input := definition()
	input.Schedule = Schedule{Frequency: "daily", Time: "02:30", Timezone: "America/New_York"}
	d, err := s.Create("range", input, instant("1900-01-01T00:00:00Z"))
	require.NoError(t, err)
	through, count, err := d.Schedule.LatestDue(d.Next, storeNow)
	require.NoError(t, err)
	o, err := s.SaveOccurrence(Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: d.Next, Through: through, Count: count, Phase: "skipped", Reason: "missed while unavailable"})
	require.NoError(t, err)
	require.Greater(t, o.Count, int64(45000))
	file, err := os.Stat(filepath.Join(s.dir, "state.json"))
	require.NoError(t, err)
	require.Less(t, file.Size(), int64(10000))
	_, err = s.SaveOccurrence(Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: through, Phase: "reserved"})
	require.ErrorIs(t, err, ErrConflict)
	next, err := d.Schedule.Next(through)
	require.NoError(t, err)
	require.NoError(t, s.SetNext(d.ID, next))
	require.NoError(t, s.Close())
	s = openStore(t, root, primary)
	records, err := s.Occurrences(d.ID)
	require.NoError(t, err)
	require.Equal(t, []Occurrence{o}, records)
	got, err := s.Get(d.ID)
	require.NoError(t, err)
	require.Equal(t, d.Revision, got.Revision)
	require.Equal(t, next, got.Next)
	_, err = s.SaveOccurrence(Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: next, Phase: "reserved"})
	require.NoError(t, err)
}

func TestOccurrenceInvalidRanges(t *testing.T) {
	for _, change := range []func(*Occurrence){
		func(o *Occurrence) { o.Count++ },
		func(o *Occurrence) { o.Through = o.Through.Add(time.Hour) },
		func(o *Occurrence) { o.Through = o.ScheduledFor.Add(-time.Hour) },
		func(o *Occurrence) { o.Phase = "reserved" },
		func(o *Occurrence) { o.Through = time.Time{} },
		func(o *Occurrence) { o.TicketID = "kon-range" },
	} {
		s := openStore(t, t.TempDir(), t.TempDir())
		d := createDefinition(t, s)
		through, err := d.Schedule.Next(d.Next)
		require.NoError(t, err)
		o := Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: d.Next, Through: through, Count: 2, Phase: "skipped"}
		change(&o)
		_, err = s.SaveOccurrence(o)
		require.ErrorIs(t, err, ErrValidation)
	}
}

func TestBlockedOccurrenceAcknowledgement(t *testing.T) {
	s := openStore(t, t.TempDir(), t.TempDir())
	d := createDefinition(t, s)
	o, err := s.SaveOccurrence(Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: d.Next, TicketID: "kon-reserved", Phase: "reserved"})
	require.NoError(t, err)
	o.Phase = "publishing"
	o, err = s.SaveOccurrence(o)
	require.NoError(t, err)
	o.Phase = "blocked"
	o.Reason = "publication uncertain"
	o, err = s.SaveOccurrence(o)
	require.NoError(t, err)
	retry := o
	retry.Phase = "publishing"
	_, err = s.SaveOccurrence(retry)
	require.ErrorIs(t, err, ErrConflict)
	o.Phase = "skipped"
	o.Reason = "acknowledged without replacement"
	o, err = s.SaveOccurrence(o)
	require.NoError(t, err)
	require.Equal(t, "kon-reserved", o.TicketID)
}

func TestOccurrenceAndCursorSaveUncertainty(t *testing.T) {
	for _, operation := range []string{"occurrence", "cursor", "update"} {
		t.Run(operation, func(t *testing.T) {
			root, primary := t.TempDir(), t.TempDir()
			s := openStore(t, root, primary)
			d := createDefinition(t, s)
			s.fault = func(stage string) error {
				if stage == "sync_dir" {
					return os.ErrPermission
				}
				return nil
			}
			switch operation {
			case "occurrence":
				_, err := s.SaveOccurrence(Occurrence{AutomationID: d.ID, ScheduleRevision: d.ScheduleRevision, ScheduledFor: d.Next, Phase: "reserved"})
				require.ErrorIs(t, err, ErrUnavailable)
			case "cursor":
				next, err := d.Schedule.Next(d.Next)
				require.NoError(t, err)
				require.ErrorIs(t, s.SetNext(d.ID, next), ErrUnavailable)
			case "update":
				d.Name = "Changed"
				_, err := s.Update(d, storeNow)
				require.ErrorIs(t, err, ErrUnavailable)
			}
			_, err := s.Get(d.ID)
			require.ErrorIs(t, err, ErrUnavailable)
			require.NoError(t, s.Close())
			s = openStore(t, root, primary)
			got, err := s.Get(d.ID)
			require.NoError(t, err)
			switch operation {
			case "occurrence":
				records, err := s.Occurrences(d.ID)
				require.NoError(t, err)
				require.Len(t, records, 1)
			case "cursor":
				require.True(t, got.Next.After(d.Next))
				require.Equal(t, d.Revision, got.Revision)
			case "update":
				require.Equal(t, "Changed", got.Name)
				require.Equal(t, d.Revision+1, got.Revision)
			}
		})
	}
}
