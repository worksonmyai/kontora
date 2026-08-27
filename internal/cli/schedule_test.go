package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestResolveSchedule(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		at      string
		after   string
		want    string
		wantErr string
	}{
		{name: "neither flag asks for nothing"},
		{name: "an instant is normalized", at: "2026-09-01T16:00:00+02:00", want: "2026-09-01T14:00:00Z"},
		{name: "an instant just behind now is still current", at: "2026-09-01T11:59:30Z", want: "2026-09-01T11:59:30Z"},
		{name: "a mistyped year is refused", at: "2025-09-01T14:00:00Z", wantErr: "--at is in the past"},
		{name: "a duration is measured from now", after: "24h", want: "2026-09-02T12:00:00Z"},
		{name: "sub-hour durations work", after: "90m", want: "2026-09-01T13:30:00Z"},
		{name: "both flags contradict", at: "2026-09-01T09:00:00Z", after: "24h", wantErr: "give one of them"},
		{name: "a malformed instant", at: "tomorrow", wantErr: "RFC 3339"},
		{name: "a malformed duration", after: "24 hours", wantErr: "Go duration"},
		{name: "a zero duration", after: "0s", wantErr: "positive duration"},
		{name: "a negative duration", after: "-1h", wantErr: "positive duration"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveSchedule(tc.at, tc.after, now)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNew_Scheduled(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		scheduledAt string
		// notARepo points the ticket at a plain directory instead of the test
		// repository.
		notARepo   bool
		wantErr    string
		wantStatus string
		wantStored string
	}{
		{
			name:        "a schedule creates the ticket open",
			scheduledAt: "2099-09-01T09:00:00Z",
			wantStatus:  "open",
			wantStored:  "2099-09-01T09:00:00Z",
		},
		{
			name:        "an offset is normalized before it is stored",
			scheduledAt: "2099-09-01T11:00:00+02:00",
			wantStatus:  "open",
			wantStored:  "2099-09-01T09:00:00Z",
		},
		{
			name:        "an explicit open status is accepted",
			status:      "open",
			scheduledAt: "2099-09-01T09:00:00Z",
			wantStatus:  "open",
			wantStored:  "2099-09-01T09:00:00Z",
		},
		{
			name:        "todo contradicts a schedule",
			status:      "todo",
			scheduledAt: "2099-09-01T09:00:00Z",
			wantErr:     "created open",
		},
		{
			name:        "a malformed instant is refused",
			scheduledAt: "next tuesday",
			wantErr:     "RFC 3339",
		},
		{
			name:        "an instant in the past is refused",
			scheduledAt: "2020-01-01T00:00:00Z",
			wantErr:     "is in the past",
		},
		{
			name:        "a scheduled ticket has its repository checked",
			scheduledAt: "2099-09-01T09:00:00Z",
			notARepo:    true,
			wantErr:     "not a git repository",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(dir)
			repoDir := initTestRepo(t)
			if tc.notARepo {
				repoDir = t.TempDir()
			}

			id, err := New(cfg, NewOpts{
				Path:        repoDir,
				Title:       "Scheduled",
				Status:      tc.status,
				ScheduledAt: tc.scheduledAt,
				NoEdit:      true,
			})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				// Nothing may be left behind by a rejected creation.
				entries, readErr := os.ReadDir(dir)
				require.NoError(t, readErr)
				assert.Empty(t, entries)
				return
			}
			require.NoError(t, err)

			// One write: the status and the schedule are in the file together,
			// so a watching daemon never sees an intermediate todo.
			tkt, err := ticket.ParseFile(filepath.Join(dir, id+".md"))
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, string(tkt.Status))
			assert.Equal(t, tc.wantStored, tkt.ScheduledAt)
			at, ok := tkt.Schedule()
			require.True(t, ok)
			assert.Equal(t, tc.wantStored, ticket.FormatSchedule(at))
		})
	}
}

func TestFormatSchedule(t *testing.T) {
	thisYear := time.Date(time.Now().Year(), 9, 1, 9, 0, 0, 0, time.UTC)
	assert.Equal(t, thisYear.Local().Format("Jan 02 15:04"), FormatSchedule(thisYear.Format(time.RFC3339)))

	// The year is printed whenever it is not the current one. A schedule is the
	// one field whose point is a long horizon, and a mistyped year is the
	// easiest way to get one wrong.
	other := time.Date(time.Now().Year()+3, 9, 1, 9, 0, 0, 0, time.UTC)
	assert.Equal(t, other.Local().Format("Jan 02 2006 15:04"), FormatSchedule(other.Format(time.RFC3339)))

	// A value the parser rejects is printed as it stands: showing a wrong time
	// would hide the typo instead of pointing at it.
	assert.Equal(t, "next tuesday", FormatSchedule("next tuesday"))
	assert.Empty(t, FormatSchedule(""))
}
