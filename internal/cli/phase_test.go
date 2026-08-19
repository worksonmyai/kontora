package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhaseComplete(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		sidecar    bool
		completed  string
		next       string
		wantErr    string
		wantOut    []string
		wantRecord bool
	}{
		{
			name:       "inside a checkpointing run",
			sidecar:    true,
			completed:  "Phase 2: x",
			next:       "Phase 3: y",
			wantOut:    []string{"Checkpoint recorded for Phase 2: x", "End your turn now"},
			wantRecord: true,
		},
		{
			name:      "outside a checkpointing run",
			completed: "Phase 2: x",
			next:      "Phase 3: y",
			wantOut:   []string{"not enabled for this run", "Phase 3: y"},
		},
		{
			name:    "missing completed phase",
			sidecar: true,
			next:    "Phase 3: y",
			wantErr: "--completed and --next are both required",
		},
		{
			name:      "missing next phase",
			sidecar:   true,
			completed: "Phase 2: x",
			wantErr:   "--completed and --next are both required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.sidecar {
				path = filepath.Join(t.TempDir(), "code.0.checkpoints.jsonl")
			}

			var out bytes.Buffer
			err := PhaseComplete(path, tt.completed, tt.next, now, &out)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			for _, want := range tt.wantOut {
				assert.Contains(t, out.String(), want)
			}

			if !tt.wantRecord {
				if path != "" {
					assert.NoFileExists(t, path)
				}
				return
			}

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			require.Len(t, lines, 1)

			var rec CheckpointRecord
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
			assert.Equal(t, CheckpointKindPhaseComplete, rec.Kind)
			assert.Equal(t, tt.completed, rec.CompletedPhase)
			assert.Equal(t, tt.next, rec.NextPhase)
			assert.True(t, now.Equal(rec.Time))
		})
	}
}

func TestAppendCheckpointRecordAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.0.checkpoints.jsonl")
	for _, kind := range []string{CheckpointKindPhaseComplete, CheckpointKindOutcome} {
		require.NoError(t, AppendCheckpointRecord(path, CheckpointRecord{Kind: kind}))
	}

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Len(t, strings.Split(strings.TrimSpace(string(data)), "\n"), 2)
}
