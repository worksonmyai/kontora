package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/stats"
)

func TestPrintStatsOutcomes(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		outcome string
		exit    int
		counts  []string
	}{
		{name: "legacy runner failure", exit: 7, counts: []string{"1", "0", "0", "0", "0", "1", "1"}},
		{name: "legacy clean exit", counts: []string{"1", "0", "0", "0", "0", "1", "0"}},
		{name: "unrecognized outcome", outcome: "future", counts: []string{"1", "0", "0", "0", "0", "1", "0"}},
		{name: "failed outcome and runner", outcome: "failed", exit: 7, counts: []string{"1", "0", "0", "0", "1", "0", "1"}},
		{name: "complete", outcome: "complete", counts: []string{"1", "1", "0", "0", "0", "0", "0"}},
		{name: "blocked", outcome: "blocked", counts: []string{"1", "0", "1", "0", "0", "0", "0"}},
		{name: "protocol error", outcome: "protocol_error", counts: []string{"1", "0", "0", "1", "0", "0", "0"}},
		{name: "semantic failure", outcome: "failed", counts: []string{"1", "0", "0", "0", "1", "0", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := stats.Compute([]stats.Ticket{{ID: "kon-stats", History: []stats.Run{{
				Stage: "code", StartedAt: &now, CompletedAt: &now, RunOutcome: tc.outcome, ExitCode: tc.exit,
			}}}}, stats.Options{Now: now, Days: 7})
			var out bytes.Buffer
			require.NoError(t, PrintStats(info, &out))
			lines := strings.Split(out.String(), "\n")
			var header, row []string
			for _, line := range lines {
				fields := strings.Fields(line)
				if len(fields) > 0 && fields[0] == "STAGE" {
					header = fields
				}
				if len(fields) > 0 && fields[0] == "code" {
					row = fields
				}
			}
			require.Equal(t, []string{"STAGE", "RUNS", "COMPLETE", "BLOCKED", "PROTOCOL_ERROR", "FAILED", "UNVERIFIED", "RUNNER_FAILED", "P50", "P90", "TOKENS"}, header)
			require.Len(t, row, len(header))
			require.Equal(t, tc.counts, row[1:8])
		})
	}
}
