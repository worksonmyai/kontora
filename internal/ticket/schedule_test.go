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
