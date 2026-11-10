package automation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func instant(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

func TestScheduleValidation(t *testing.T) {
	for _, value := range []string{"* * * * *", "0 */6 * * *", "0 9 * * 1-5", "1,2,3 4 1-10/2 * 0", "1m", "30m", "6h", "1.5h", "60s"} {
		t.Run(value, func(t *testing.T) { _, err := Compile(value, "UTC"); require.NoError(t, err) })
	}
	for _, value := range []string{"", "@hourly", "@every 1m", "0 0 0 * * *", "0 9 * * MON", "0 9 ? * *", "TZ=UTC 0 9 * * *", "CRON_TZ=UTC 0 9 * * *", "60 * * * *", "0 24 * * *", "0 0 31 2 *", "0s", "-1m", "59s", "90s", "1.01m"} {
		t.Run(value, func(t *testing.T) { _, err := Compile(value, "UTC"); require.ErrorIs(t, err, ErrValidation) })
	}
	for _, zone := range []string{"Local", "localtime", "../UTC", "/etc/localtime", "Bad/Timezone", "UTC\x00"} {
		_, err := Compile("0 9 * * *", zone)
		require.ErrorIs(t, err, ErrValidation)
	}
	_, err := Compile("0 9 * * *", "")
	require.NoError(t, err)
}

func TestScheduleOccurrences(t *testing.T) {
	cases := []struct {
		name, schedule, zone, after string
		want                        []string
	}{
		{"exclusive", "0 * * * *", "UTC", "2024-01-01T09:00:00Z", []string{"2024-01-01T10:00:00Z", "2024-01-01T11:00:00Z"}},
		{"fixed interval", "30m", "Europe/Berlin", "2024-01-01T09:12:34Z", []string{"2024-01-01T09:42:34Z", "2024-01-01T10:12:34Z"}},
		{"spring gap", "30 2 * * *", "America/New_York", "2024-03-09T08:00:00Z", []string{"2024-03-11T06:30:00Z"}},
		{"autumn fold", "30 1 * * *", "America/New_York", "2024-11-03T05:00:00Z", []string{"2024-11-03T05:30:00Z", "2024-11-03T06:30:00Z"}},
		{"leap day", "0 9 29 2 *", "UTC", "2025-01-01T00:00:00Z", []string{"2028-02-29T09:00:00Z"}},
		{"weekdays", "0 9 * * 1-5", "UTC", "2024-01-05T09:00:00Z", []string{"2024-01-08T09:00:00Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Preview(tc.schedule, tc.zone, instant(tc.after), len(tc.want))
			require.NoError(t, err)
			for i, want := range tc.want {
				require.True(t, got[i].Equal(instant(want)), "got %s, want %s", got[i], want)
			}
		})
	}
	_, err := Preview("1m", "UTC", time.Now(), -1)
	require.ErrorIs(t, err, ErrValidation)
	_, err = Preview("1m", "UTC", time.Now(), 1001)
	require.ErrorIs(t, err, ErrValidation)
	_, err = Preview("1m", "UTC", time.Time{}, 1)
	require.ErrorIs(t, err, ErrValidation)
	got, err := Preview("1m", "UTC", time.Now(), 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestHuman(t *testing.T) {
	for value, want := range map[string]string{"1h": "every hour", "6h": "every 6h", "30m": "every 30m", "0 * * * *": "every hour, on the hour", "0 */6 * * *": "every 6h", "0 9 * * *": "daily at 09:00", "0 9 * * 1-5": "weekdays at 09:00", "0 10 * * 1": "Monday at 10:00", "5,10 9 * * *": "5,10 9 * * *"} {
		require.Equal(t, want, Human(value))
	}
}
