package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/logfmt"
)

func TestSidecarTotals(t *testing.T) {
	// A tape with 5000 events, so a reader that decoded the array would be
	// doing visible work the metadata read must avoid.
	bigTape := func() string {
		tape := logfmt.Tape{
			Version: logfmt.TapeVersion,
			Agent:   "claude",
			Model:   "sonnet-4.6",
			Totals:  logfmt.Usage{Input: 120, Output: 30, CacheCreate: 7, CacheRead: 400},
			Events:  make([]logfmt.Event, logfmt.MaxTapeEvents),
		}
		for i := range tape.Events {
			tape.Events[i] = logfmt.Event{Kind: "text", Text: strings.Repeat("x", 64)}
		}
		data, err := json.Marshal(tape)
		require.NoError(t, err)
		return string(data)
	}

	tests := []struct {
		name      string
		content   string
		wantModel string
		wantUsage *Usage
		wantErr   bool
	}{
		{
			name:      "large events array is not decoded",
			content:   bigTape(),
			wantModel: "sonnet-4.6",
			// In folds all three inbound categories; the two cache figures
			// name two of them again rather than adding to it.
			wantUsage: &Usage{In: 527, Out: 30, CacheCreate: 7, CacheRead: 400},
		},
		{
			name: "a tape whose session had no usage keys declares it partial",
			content: `{"version":1,"agent":"pi","model":"opus-5",` +
				`"totals":{"input":0,"output":0,"cache_create":0,"cache_read":0},` +
				`"partial":["time","usage","is_error"],"events":[]}`,
			wantModel: "opus-5",
			wantUsage: nil,
		},
		{
			name: "complete tape with genuine zero usage",
			content: `{"version":1,"agent":"claude","model":"sonnet-4.6",` +
				`"totals":{"input":0,"output":0,"cache_create":0,"cache_read":0},"events":[]}`,
			wantModel: "sonnet-4.6",
			wantUsage: &Usage{},
		},
		{
			name: "malformed after the first event still returns metadata",
			content: `{"version":1,"agent":"claude","model":"haiku-4.5",` +
				`"totals":{"input":5,"output":2,"cache_create":0,"cache_read":0},` +
				`"events":[{"kind":"text","text":"one"},{"kind":,,,`,
			wantModel: "haiku-4.5",
			wantUsage: &Usage{In: 5, Out: 2},
		},
		{
			// Nothing in logfmt pins the field order, and a zero read off a tape
			// whose totals were never reached is not a run that cost nothing.
			name: "events written before totals leaves usage unavailable",
			content: `{"version":1,"agent":"claude","model":"sonnet-4.6","events":[],` +
				`"totals":{"input":9,"output":3,"cache_create":0,"cache_read":0}}`,
			wantModel: "sonnet-4.6",
			wantUsage: nil,
		},
		{
			name:    "malformed before events",
			content: `{"version":1,"agent":"claude","model":`,
			wantErr: true,
		},
		{
			name:    "not an object",
			content: `[1,2,3]`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "step.0.events.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o644))

			model, usage, err := SidecarTotals(path)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantModel, model)
			require.Equal(t, tc.wantUsage, usage)
		})
	}

	t.Run("missing file", func(t *testing.T) {
		_, _, err := SidecarTotals(filepath.Join(t.TempDir(), "absent.json"))
		require.Error(t, err)
	})
}
