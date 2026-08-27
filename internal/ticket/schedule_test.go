package ticket

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSchedule(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		want    string // FormatSchedule of the parsed instant
	}{
		{name: "utc", in: "2026-09-01T09:00:00Z", want: "2026-09-01T09:00:00Z"},
		{name: "positive offset", in: "2026-09-01T09:00:00+02:00", want: "2026-09-01T07:00:00Z"},
		{name: "negative offset", in: "2026-09-01T09:00:00-05:00", want: "2026-09-01T14:00:00Z"},
		{name: "fractional seconds truncate", in: "2026-09-01T09:00:00.750Z", want: "2026-09-01T09:00:00Z"},
		{name: "space instead of T", in: "2026-09-01 09:00:00Z", wantErr: true},
		{name: "no zone", in: "2026-09-01T09:00:00", wantErr: true},
		{name: "date only", in: "2026-09-01", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "prose", in: "tomorrow at nine", wantErr: true},
		{name: "impossible month", in: "2026-13-01T09:00:00Z", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, err := ParseSchedule(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "scheduled_at")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, FormatSchedule(at))
		})
	}
}

func TestTicketSchedule(t *testing.T) {
	const src = "---\nid: kon-1\nkontora: true\nstatus: open\nmine: keep\n---\n# Title\n"

	t.Run("absent", func(t *testing.T) {
		tk, err := ParseBytes([]byte(src))
		require.NoError(t, err)
		_, ok := tk.Schedule()
		assert.False(t, ok)
	})

	t.Run("set normalizes and preserves custom fields", func(t *testing.T) {
		tk, err := ParseBytes([]byte(src))
		require.NoError(t, err)
		require.NoError(t, tk.SetSchedule(time.Date(2026, 9, 1, 9, 0, 0, 0, time.FixedZone("CEST", 2*3600))))

		assert.Equal(t, "2026-09-01T07:00:00Z", tk.ScheduledAt)
		at, ok := tk.Schedule()
		require.True(t, ok)
		assert.True(t, at.Equal(time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)))

		out, err := tk.Marshal()
		require.NoError(t, err)
		assert.Contains(t, string(out), `scheduled_at: "2026-09-01T07:00:00Z"`)
		assert.Contains(t, string(out), "mine: keep")
	})

	t.Run("unquoted yaml timestamp reads as the stored string", func(t *testing.T) {
		tk, err := ParseBytes([]byte("---\nid: kon-1\nstatus: open\nscheduled_at: 2026-09-01T09:00:00Z\n---\n"))
		require.NoError(t, err)
		assert.Equal(t, "2026-09-01T09:00:00Z", tk.ScheduledAt)
		_, ok := tk.Schedule()
		assert.True(t, ok)
	})

	t.Run("malformed value is kept but reads as no schedule", func(t *testing.T) {
		tk, err := ParseBytes([]byte("---\nid: kon-1\nstatus: open\nscheduled_at: \"next tuesday\"\n---\n"))
		require.NoError(t, err)
		assert.Equal(t, "next tuesday", tk.ScheduledAt)
		_, ok := tk.Schedule()
		assert.False(t, ok)
	})

	t.Run("clear removes the key", func(t *testing.T) {
		tk, err := ParseBytes([]byte(src))
		require.NoError(t, err)
		require.NoError(t, tk.SetSchedule(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)))
		require.NoError(t, tk.ClearSchedule())

		assert.Empty(t, tk.ScheduledAt)
		out, err := tk.Marshal()
		require.NoError(t, err)
		assert.NotContains(t, string(out), "scheduled_at")
		assert.Contains(t, string(out), "mine: keep")
	})

	t.Run("clear on a ticket with no schedule is a no-op", func(t *testing.T) {
		tk, err := ParseBytes([]byte(src))
		require.NoError(t, err)
		require.NoError(t, tk.ClearSchedule())
		assert.Empty(t, tk.ScheduledAt)
	})
}

func TestParseScheduleFlex(t *testing.T) {
	// The local spellings are read in the runner's own zone, so the
	// expectations are built there too.
	local := func(y int, mo time.Month, d, h, mi int) string {
		return FormatSchedule(time.Date(y, mo, d, h, mi, 0, 0, time.Local))
	}

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "rfc 3339 utc", in: "2026-09-01T09:00:00Z", want: "2026-09-01T09:00:00Z"},
		{name: "rfc 3339 offset", in: "2026-09-01T09:00:00+02:00", want: "2026-09-01T07:00:00Z"},
		{name: "local space", in: "2026-09-01 09:00", want: local(2026, 9, 1, 9, 0)},
		{name: "local T", in: "2026-09-01T09:00", want: local(2026, 9, 1, 9, 0)},
		{name: "local with seconds", in: "2026-09-01 09:00:00", want: local(2026, 9, 1, 9, 0)},
		{name: "date only is refused", in: "2026-09-01", wantErr: true},
		{name: "time only", in: "09:00", wantErr: true},
		{name: "prose", in: "tomorrow at nine", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "impossible day", in: "2026-02-30 09:00", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScheduleFlex(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, FormatSchedule(got))
		})
	}
}

func TestParseScheduleDelay(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr string
	}{
		{name: "hours", in: "24h", want: 24 * time.Hour},
		{name: "minutes", in: "90m", want: 90 * time.Minute},
		{name: "seconds", in: "45s", want: 45 * time.Second},
		{name: "days", in: "3d", want: 72 * time.Hour},
		{name: "weeks", in: "2w", want: 336 * time.Hour},
		{name: "composite go units", in: "1h30m", want: 90 * time.Minute},
		{name: "composite with days", in: "1w2d3h", want: 219 * time.Hour},
		{name: "fractional days", in: "1.5d", want: 36 * time.Hour},
		{name: "zero", in: "0s", wantErr: "positive duration"},
		{name: "negative", in: "-1h", wantErr: "positive duration"},
		{name: "prose", in: "24 hours", wantErr: "such as"},
		{name: "months are not a unit", in: "2mo", wantErr: "such as"},
		{name: "empty", in: "", wantErr: "such as"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScheduleDelay(tt.in)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
