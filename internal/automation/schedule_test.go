package automation

import (
	"archive/zip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func instant(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestScheduleValidation(t *testing.T) {
	base := Schedule{Frequency: "daily", Time: "09:30", Timezone: "UTC"}
	for _, tt := range []struct {
		name   string
		change func(*Schedule)
	}{
		{"frequency", func(s *Schedule) { s.Frequency = "monthly" }},
		{"no weekly days", func(s *Schedule) { s.Frequency = "weekly" }},
		{"daily days", func(s *Schedule) { s.Days = []int{1} }},
		{"duplicate", func(s *Schedule) { s.Frequency = "weekly"; s.Days = []int{1, 1} }},
		{"negative", func(s *Schedule) { s.Frequency = "weekly"; s.Days = []int{-1} }},
		{"seven", func(s *Schedule) { s.Frequency = "weekly"; s.Days = []int{7} }},
		{"hour", func(s *Schedule) { s.Time = "24:00" }},
		{"minute", func(s *Schedule) { s.Time = "09:60" }},
		{"padding", func(s *Schedule) { s.Time = "9:30" }},
		{"seconds", func(s *Schedule) { s.Time = "09:30:00" }},
		{"no timezone", func(s *Schedule) { s.Timezone = "" }},
		{"local", func(s *Schedule) { s.Timezone = "Local" }},
		{"host alias", func(s *Schedule) { s.Timezone = "localtime" }},
		{"offset", func(s *Schedule) { s.Timezone = "+03:00" }},
		{"unknown timezone", func(s *Schedule) { s.Timezone = "Mars/Olympus" }},
		{"traversal", func(s *Schedule) { s.Timezone = "../UTC" }},
	} {
		t.Run(tt.name, func(t *testing.T) { s := base; tt.change(&s); require.ErrorIs(t, s.Validate(), ErrValidation) })
	}
	require.NoError(t, base.Validate())
	for _, zone := range []string{"UTC", "America/New_York", "Asia/Kathmandu", "Australia/Lord_Howe"} {
		s := base
		s.Timezone = zone
		require.NoError(t, s.Validate())
	}
}

func TestScheduleRejectsHostTimezoneAlias(t *testing.T) {
	if os.Getenv("KONTORA_TEST_HOST_TIMEZONE_ALIAS") == "1" {
		_, err := time.LoadLocation("localtime")
		require.NoError(t, err)
		s := Schedule{Frequency: "daily", Time: "09:00", Timezone: "localtime"}
		require.ErrorIs(t, s.Validate(), ErrValidation)
		return
	}
	zones, err := zip.OpenReader(filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip"))
	require.NoError(t, err)
	defer func() { require.NoError(t, zones.Close()) }()
	zone, err := zones.Open("America/New_York")
	require.NoError(t, err)
	data, err := io.ReadAll(zone)
	require.NoError(t, err)
	require.NoError(t, zone.Close())
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "localtime"), data, 0600))
	cmd := exec.Command(os.Args[0], "-test.run=^TestScheduleRejectsHostTimezoneAlias$")
	cmd.Env = append(os.Environ(), "ZONEINFO="+root, "KONTORA_TEST_HOST_TIMEZONE_ALIAS=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestScheduleNextAndPreview(t *testing.T) {
	for _, tt := range []struct {
		name        string
		schedule    Schedule
		after, want string
	}{
		{"daily", Schedule{Frequency: "daily", Time: "09:00", Timezone: "UTC"}, "2026-03-02T08:59:59Z", "2026-03-02T09:00:00Z"},
		{"strictly future", Schedule{Frequency: "daily", Time: "09:00", Timezone: "UTC"}, "2026-03-02T09:00:00Z", "2026-03-03T09:00:00Z"},
		{"weekend", Schedule{Frequency: "weekdays", Time: "09:00", Timezone: "UTC"}, "2026-03-06T10:00:00Z", "2026-03-09T09:00:00Z"},
		{"sunday zero", Schedule{Frequency: "weekly", Days: []int{0}, Time: "09:00", Timezone: "UTC"}, "2026-03-06T10:00:00Z", "2026-03-08T09:00:00Z"},
		{"gap", Schedule{Frequency: "daily", Time: "02:30", Timezone: "America/New_York"}, "2026-03-07T08:00:00Z", "2026-03-09T06:30:00Z"},
		{"fold first", Schedule{Frequency: "daily", Time: "01:30", Timezone: "America/New_York"}, "2026-11-01T05:00:00Z", "2026-11-01T05:30:00Z"},
		{"fold no replay", Schedule{Frequency: "daily", Time: "01:30", Timezone: "America/New_York"}, "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"},
		{"half hour gap", Schedule{Frequency: "daily", Time: "02:15", Timezone: "Australia/Lord_Howe"}, "2026-10-03T00:00:00Z", "2026-10-04T15:15:00Z"},
		{"half hour fold", Schedule{Frequency: "daily", Time: "01:45", Timezone: "Australia/Lord_Howe"}, "2026-04-04T14:00:00Z", "2026-04-04T14:45:00Z"},
		{"date skipped", Schedule{Frequency: "daily", Time: "09:00", Timezone: "Pacific/Apia"}, "2011-12-29T20:00:00Z", "2011-12-30T19:00:00Z"},
		{"fractional offset", Schedule{Frequency: "daily", Time: "09:00", Timezone: "Asia/Kathmandu"}, "2026-03-02T00:00:00Z", "2026-03-02T03:15:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			next, err := tt.schedule.Next(instant(tt.after))
			require.NoError(t, err)
			require.Equal(t, instant(tt.want), next)
			preview, err := tt.schedule.Preview(instant(tt.after), 3)
			require.NoError(t, err)
			require.Len(t, preview, 3)
			require.Equal(t, next, preview[0])
			for i := 1; i < len(preview); i++ {
				require.True(t, preview[i].After(preview[i-1]))
			}
		})
	}
	s := Schedule{Frequency: "daily", Time: "09:00", Timezone: "UTC"}
	_, err := s.Preview(time.Now(), -1)
	require.ErrorIs(t, err, ErrValidation)
	_, err = s.Preview(time.Now(), 1001)
	require.ErrorIs(t, err, ErrValidation)
	x, err := s.Preview(time.Now(), 0)
	require.NoError(t, err)
	require.Empty(t, x)
	_, err = s.Next(instant("9999-12-31T23:00:00Z"))
	require.ErrorIs(t, err, ErrValidation)
}

func TestLatestDueBoundaries(t *testing.T) {
	s := Schedule{Frequency: "daily", Time: "09:00", Timezone: "UTC"}
	for _, tt := range []struct {
		first, now, latest string
		count              int64
	}{
		{"2026-03-01T09:00:00Z", "2026-03-03T09:00:00Z", "2026-03-03T09:00:00Z", 3},
		{"2026-03-01T09:00:00.000000001Z", "2026-03-03T08:59:59Z", "2026-03-02T09:00:00Z", 1},
		{"2026-03-01T09:00:00Z", "2026-03-01T09:04:00Z", "2026-03-01T09:00:00Z", 1},
		{"2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z", "", 0},
		{"2026-03-02T09:00:00Z", "2026-03-01T09:00:00Z", "", 0},
	} {
		latest, count, err := s.LatestDue(instant(tt.first), instant(tt.now))
		require.NoError(t, err)
		require.Equal(t, tt.count, count)
		if tt.latest == "" {
			require.True(t, latest.IsZero())
		} else {
			require.Equal(t, instant(tt.latest), latest)
		}
	}
}

func TestLatestDueMatchesEnumeration(t *testing.T) {
	for _, zone := range []string{"UTC", "America/New_York", "Europe/Berlin", "Australia/Lord_Howe", "Pacific/Apia", "Africa/Casablanca"} {
		for _, clock := range []string{"00:00", "01:30", "02:15", "09:00", "23:59"} {
			t.Run(zone+"/"+clock, func(t *testing.T) {
				s := Schedule{Frequency: "weekly", Days: []int{0, 1, 4}, Time: clock, Timezone: zone}
				first := instant("2010-01-01T00:00:00Z")
				now := instant("2013-01-01T00:00:00Z")
				latest, count, err := s.LatestDue(first, now)
				require.NoError(t, err)
				var wantLatest time.Time
				var wantCount int64
				for cursor := first.Add(-time.Nanosecond); ; {
					next, err := s.Next(cursor)
					require.NoError(t, err)
					if next.After(now) {
						break
					}
					wantCount++
					wantLatest = next
					cursor = next
				}
				require.Equal(t, wantCount, count)
				require.Equal(t, wantLatest, latest)
			})
		}
	}
}

func TestLatestDueLongRange(t *testing.T) {
	for _, zone := range []string{"UTC", "America/New_York"} {
		s := Schedule{Frequency: "daily", Time: "12:00", Timezone: zone}
		start := time.Now()
		latest, count, err := s.LatestDue(instant("0001-01-01T12:00:00Z"), instant("9999-12-31T23:59:59Z"))
		require.NoError(t, err)
		require.Greater(t, count, int64(3650000))
		require.Equal(t, 9999, latest.Year())
		require.Less(t, time.Since(start), 3*time.Second)
		if zone == "UTC" {
			require.Equal(t, int64(3652059), count)
		}
	}
}

func TestLatestDueDST(t *testing.T) {
	for _, tt := range []struct {
		clock, first, now, latest string
		count                     int64
	}{
		{"02:30", "2026-03-07T00:00:00Z", "2026-03-09T23:59:59Z", "2026-03-09T06:30:00Z", 2},
		{"01:30", "2026-11-01T00:00:00Z", "2026-11-01T08:00:00Z", "2026-11-01T05:30:00Z", 1},
		{"01:30", "2026-11-01T05:30:00.000000001Z", "2026-11-01T08:00:00Z", "", 0},
		{"09:00", "2400-12-30T00:00:00Z", "2401-01-02T23:59:59Z", "2401-01-02T14:00:00Z", 4},
	} {
		s := Schedule{Frequency: "daily", Time: tt.clock, Timezone: "America/New_York"}
		latest, count, err := s.LatestDue(instant(tt.first), instant(tt.now))
		require.NoError(t, err)
		require.Equal(t, tt.count, count)
		if tt.latest == "" {
			require.True(t, latest.IsZero())
		} else {
			require.Equal(t, instant(tt.latest), latest)
		}
	}
}

func BenchmarkLatestDue(b *testing.B) {
	s := Schedule{Frequency: "daily", Time: "02:30", Timezone: "America/New_York"}
	for b.Loop() {
		_, _, err := s.LatestDue(instant("1900-01-01T00:00:00Z"), instant("9999-12-31T23:59:59Z"))
		if err != nil {
			b.Fatal(err)
		}
	}
}
