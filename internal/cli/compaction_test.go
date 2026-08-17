package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/compaction"
)

func TestParseThresholds(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []int
		wantErr string
	}{
		{
			name:  "single value",
			input: "150000",
			want:  []int{150000},
		},
		{
			name:  "multiple values",
			input: "100000,125000,150000",
			want:  []int{100000, 125000, 150000},
		},
		{
			name:  "spaces around values",
			input: " 100000 , 200000 ",
			want:  []int{100000, 200000},
		},
		{
			name:  "trailing comma ignored",
			input: "100000,200000,",
			want:  []int{100000, 200000},
		},
		{
			name:    "negative value",
			input:   "-1",
			wantErr: "must be positive",
		},
		{
			name:    "zero",
			input:   "0",
			wantErr: "must be positive",
		},
		{
			name:    "non-numeric",
			input:   "abc",
			wantErr: "invalid threshold",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: "at least one threshold",
		},
		{
			name:    "only commas",
			input:   ",,",
			wantErr: "at least one threshold",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseThresholds(tc.input)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPrintCompactionEstimate_WithCalibration(t *testing.T) {
	report := &CompactionReport{
		LogsDir:          "~/.kontora/logs",
		Stage:            "implement",
		SessionsScanned:  89,
		SessionsEligible: 38,
		Checkpoints:      137,
		Exclusions: []ExclusionCount{
			{Reason: "branch_summary", Count: 12},
			{Reason: "no_usage", Count: 8},
			{Reason: "broken_parent", Count: 3},
			{Reason: "has_compaction", Count: 9},
			{Reason: "live_resume", Count: 2},
		},
		Calibration: &CalibrationData{
			Samples:              3,
			SummaryRatioSamples:  3,
			SummaryOutputSamples: 3,
			PostContextSamples:   3,
			SummaryRatio:         Percentiles{Low: 0.12, Median: 0.15, High: 0.18},
			SummaryOutput:        Percentiles{Low: 2400, Median: 3100, High: 4200},
			PostContext:          Percentiles{Low: 15000, Median: 22000, High: 28000},
		},
		Thresholds: []ThresholdRow{
			{Threshold: 100000, Compactions: 45, BaselineTokens: 2100000, LowReduction: 420000, MedianReduction: 380000, HighReduction: 350000},
			{Threshold: 150000, Compactions: 30, BaselineTokens: 2100000, LowReduction: 300000, MedianReduction: 270000, HighReduction: 250000},
		},
		TopSessions: []TopSession{
			{Ticket: "kon-abc1", Stage: "implement", File: "/logs/kon-abc1/pi-sessions/implement/first.jsonl", BaselineTokens: 180000, Checkpoints: 4, MedianReduction: 52000},
			{Ticket: "kon-abc1", Stage: "implement", File: "/logs/kon-abc1/pi-sessions/implement/second.jsonl", BaselineTokens: 150000, Checkpoints: 3, MedianReduction: 38000},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, PrintCompactionEstimate(report, &buf))
	out := buf.String()

	// Header.
	assert.Contains(t, out, "implement")
	assert.Contains(t, out, "~/.kontora/logs")

	// Coverage.
	assert.Contains(t, out, "sessions scanned:    89")
	assert.Contains(t, out, "sessions eligible:   38")
	assert.Contains(t, out, "checkpoints:         137")

	// Exclusions.
	assert.Contains(t, out, "branch_summary")
	assert.Contains(t, out, "12")
	assert.Contains(t, out, "no_usage")
	assert.Contains(t, out, "broken_parent")
	assert.Contains(t, out, "has_compaction")
	assert.Contains(t, out, "live_resume")

	// Calibration.
	assert.Contains(t, out, "summary ratio (3)")
	assert.Contains(t, out, "summary output (3)")
	assert.Contains(t, out, "post context (3)")
	assert.Contains(t, out, "0.1200")
	assert.Contains(t, out, "0.1500")
	assert.Contains(t, out, "0.1800")
	assert.Contains(t, out, "2400")
	assert.Contains(t, out, "3100")
	assert.Contains(t, out, "15000")

	// Scenarios.
	assert.Contains(t, out, "100,000")
	assert.Contains(t, out, "150,000")
	assert.Contains(t, out, "45")
	assert.Contains(t, out, "30")
	assert.Contains(t, out, "2,100,000")
	assert.Contains(t, out, "-420,000 (20%)")
	assert.Contains(t, out, "-380,000 (18%)")
	assert.Contains(t, out, "CAL P25")
	assert.Contains(t, out, "CAL P50")
	assert.Contains(t, out, "CAL P75")
	assert.NotContains(t, out, " LOW ")

	// Top sessions.
	assert.Contains(t, out, "kon-abc1")
	assert.Contains(t, out, "SESSION")
	assert.Contains(t, out, "first.jsonl")
	assert.Contains(t, out, "second.jsonl")
	assert.Contains(t, out, "180,000")
	assert.Contains(t, out, "52,000")
	assert.Contains(t, out, "REDUCTION")

	// Limits.
	assert.Contains(t, out, "Limits")
	assert.Contains(t, out, "compaction sample")
	assert.Contains(t, out, "3 complete compaction sample(s)")
}

func TestPrintCompactionEstimate_NoCalibration(t *testing.T) {
	report := &CompactionReport{
		LogsDir:          "/tmp/logs",
		Stage:            "implement",
		SessionsScanned:  10,
		SessionsEligible: 5,
		Checkpoints:      20,
		Calibration:      nil,
	}

	var buf bytes.Buffer
	require.NoError(t, PrintCompactionEstimate(report, &buf))
	out := buf.String()

	// Coverage is still printed.
	assert.Contains(t, out, "sessions scanned:    10")
	assert.Contains(t, out, "sessions eligible:   5")
	assert.Contains(t, out, "checkpoints:         20")

	// No scenarios or token-reduction figures.
	assert.Contains(t, out, "No usable compaction samples")
	assert.NotContains(t, out, "Scenarios")
	assert.NotContains(t, out, "Top Sessions")
	assert.Contains(t, out, "Limits")
}

func TestPrintCompactionEstimate_NoExclusions(t *testing.T) {
	report := &CompactionReport{
		LogsDir:          "/tmp/logs",
		Stage:            "implement",
		SessionsScanned:  5,
		SessionsEligible: 5,
		Checkpoints:      10,
		Calibration: &CalibrationData{
			Samples:              1,
			SummaryRatioSamples:  1,
			SummaryOutputSamples: 1,
			PostContextSamples:   1,
			SummaryRatio:         Percentiles{Low: 0.15, Median: 0.15, High: 0.15},
			SummaryOutput:        Percentiles{Low: 3000, Median: 3000, High: 3000},
			PostContext:          Percentiles{Low: 20000, Median: 20000, High: 20000},
		},
		Thresholds: []ThresholdRow{
			{Threshold: 100000, Compactions: 5, BaselineTokens: 500000, LowReduction: 50000, MedianReduction: 50000, HighReduction: 50000},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, PrintCompactionEstimate(report, &buf))
	out := buf.String()

	// The Exclusions sub-heading must not appear.
	assert.NotContains(t, out, "Exclusions")
	// Scenarios are shown.
	assert.Contains(t, out, "Scenarios")
	assert.Contains(t, out, "100,000")
}

func TestPrintCompactionEstimate_NoTopSessions(t *testing.T) {
	report := &CompactionReport{
		LogsDir:          "/tmp/logs",
		Stage:            "implement",
		SessionsScanned:  1,
		SessionsEligible: 1,
		Checkpoints:      2,
		Calibration: &CalibrationData{
			Samples:              1,
			SummaryRatioSamples:  1,
			SummaryOutputSamples: 1,
			PostContextSamples:   1,
			SummaryRatio:         Percentiles{Low: 0.1, Median: 0.1, High: 0.1},
			SummaryOutput:        Percentiles{Low: 1000, Median: 1000, High: 1000},
			PostContext:          Percentiles{Low: 10000, Median: 10000, High: 10000},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, PrintCompactionEstimate(report, &buf))
	out := buf.String()

	// Top Sessions heading must not appear when the list is empty.
	assert.NotContains(t, out, "Top Sessions")
	// Limits are still printed when calibration exists.
	assert.Contains(t, out, "Limits")
}

func TestPrintCompactionEstimate_NilReport(t *testing.T) {
	var buf bytes.Buffer
	err := PrintCompactionEstimate(nil, &buf)
	require.ErrorContains(t, err, "nil report")
}

func TestPrintCompactionEstimate_ZeroReductionRow(t *testing.T) {
	report := &CompactionReport{
		LogsDir:          "/tmp/logs",
		Stage:            "implement",
		SessionsScanned:  1,
		SessionsEligible: 1,
		Checkpoints:      0,
		Calibration: &CalibrationData{
			Samples:              1,
			SummaryRatioSamples:  1,
			SummaryOutputSamples: 1,
			PostContextSamples:   1,
			SummaryRatio:         Percentiles{Low: 0.1, Median: 0.1, High: 0.1},
			SummaryOutput:        Percentiles{Low: 1000, Median: 1000, High: 1000},
			PostContext:          Percentiles{Low: 10000, Median: 10000, High: 10000},
		},
		Thresholds: []ThresholdRow{
			{Threshold: 200000, Compactions: 0, BaselineTokens: 100000, LowReduction: 0, MedianReduction: 0, HighReduction: 0},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, PrintCompactionEstimate(report, &buf))
	out := buf.String()

	// Zero reduction is shown as "0", not "-0 (0%)".
	_, row := scenarioLines(t, out)
	assert.Equal(t, []string{"200,000", "0", "0", "0", "0", "100,000"}, strings.Fields(row))
	assert.NotContains(t, out, "-0 (0%)")
}

func TestPrintCompactionEstimateWideReductionsStayAligned(t *testing.T) {
	report := &CompactionReport{
		LogsDir: "/tmp/logs",
		Stage:   "implement",
		Calibration: &CalibrationData{
			Samples:              3,
			SummaryRatioSamples:  3,
			SummaryOutputSamples: 3,
			PostContextSamples:   3,
		},
		Thresholds: []ThresholdRow{{
			Threshold:       150000,
			Compactions:     25,
			BaselineTokens:  1418618049,
			LowReduction:    444551439,
			MedianReduction: 444050032,
			HighReduction:   439326520,
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, PrintCompactionEstimate(report, &buf))
	header, row := scenarioLines(t, buf.String())

	assert.Equal(t, strings.Index(header, "CAL P25"), strings.Index(row, "-444,551,439"))
	assert.Equal(t, strings.Index(header, "CAL P50"), strings.Index(row, "-444,050,032"))
	assert.Equal(t, strings.Index(header, "CAL P75"), strings.Index(row, "-439,326,520"))
	assert.Equal(t, strings.Index(header, "BASELINE"), strings.Index(row, "1,418,618,049"))
}

func scenarioLines(t *testing.T, output string) (string, string) {
	t.Helper()
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if strings.Contains(line, "THRESHOLD") {
			require.Greater(t, len(lines), i+1)
			return line, lines[i+1]
		}
	}
	t.Fatal("scenario table not found")
	return "", ""
}

func TestFormatTokenCount(t *testing.T) {
	cases := []struct {
		input int
		want  string
	}{
		{0, "0"},
		{1, "1"},
		{100, "100"},
		{999, "999"},
		{1000, "1,000"},
		{10000, "10,000"},
		{100000, "100,000"},
		{1000000, "1,000,000"},
		{2100000, "2,100,000"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, formatTokenCount(tc.input))
		})
	}
}

func TestFormatReduction(t *testing.T) {
	cases := []struct {
		name      string
		reduction int
		baseline  int
		want      string
	}{
		{"zero", 0, 100000, "0"},
		{"increase", -200, 1000, "+200 (-20%)"},
		{"20 percent", 420000, 2100000, "-420,000 (20%)"},
		{"small", 500, 10000, "-500 (5%)"},
		{"100 percent", 100000, 100000, "-100,000 (100%)"},
		{"zero baseline", 50000, 0, "-50,000 (0%)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatReduction(tc.reduction, tc.baseline))
		})
	}
}

func TestEstimateCompaction_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()
	report, err := EstimateCompaction(tmpDir, "implement", []int{100000}, 10)
	require.NoError(t, err)
	assert.NotNil(t, report)
	assert.Equal(t, 0, report.SessionsScanned)
	assert.Equal(t, 0, report.SessionsEligible)
	assert.Equal(t, tmpDir, report.LogsDir)
	assert.Equal(t, "implement", report.Stage)
}

func TestEstimateCompactionDoesNotModifySessions(t *testing.T) {
	logsDir := t.TempDir()
	sessionDir := filepath.Join(logsDir, "kon-test", "pi-sessions", "implement")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	data, err := os.ReadFile(filepath.Join("..", "compaction", "testdata", "linear.jsonl"))
	require.NoError(t, err)
	path := filepath.Join(sessionDir, "session.jsonl")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	mtime := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(path, mtime, mtime))

	_, err = EstimateCompaction(logsDir, "implement", []int{100000}, 10)
	require.NoError(t, err)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, after)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, mtime, info.ModTime())
}

func TestEstimateCompactionRejectsNegativeTop(t *testing.T) {
	_, err := EstimateCompaction(t.TempDir(), "implement", []int{100000}, -1)
	require.ErrorContains(t, err, "top must not be negative")
}

func TestEstimateCompaction_MissingDir(t *testing.T) {
	_, err := EstimateCompaction("/nonexistent/path/to/logs", "implement", []int{100000}, 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover sessions")
}

func TestConvertReport_FullMapping(t *testing.T) {
	// Verify the conversion from compaction.Report to CompactionReport.
	srcReport := &compaction.Report{
		FilesScanned: 50,
		Eligible:     30,
		Excluded: map[compaction.ExclusionReason]int{
			compaction.ExcludeNoUsage:       5,
			compaction.ExcludeBranchSummary: 3,
			compaction.ExcludeHasCompaction: 10,
			compaction.ExcludeBrokenChain:   2,
			compaction.ExcludeUnreadable:    1,
		},
		Checkpoints: 100,
		Calibration: compaction.Calibration{
			SampleCount:       3,
			SummaryRatios:     []float64{0.1, 0.2, 0.3},
			SummaryOutputs:    []int{500, 800, 1200},
			PostCompactionCtx: []int{10000, 20000, 30000},
		},
		Thresholds: []compaction.ThresholdResult{
			{
				Threshold: 100000,
				Scenarios: [3]compaction.ScenarioResult{
					{Label: "low", CompactionCount: 10, BaselinePrompt: 500000, BaselineOutput: 10000, RawTokenReduction: 80000},
					{Label: "median", CompactionCount: 8, BaselinePrompt: 500000, BaselineOutput: 10000, RawTokenReduction: 60000},
					{Label: "high", CompactionCount: 6, BaselinePrompt: 500000, BaselineOutput: 10000, RawTokenReduction: 40000},
				},
			},
		},
		TopSessions: []compaction.TopSessionResult{
			{Ticket: "t-1", Stage: "implement", File: "/logs/t-1/pi-sessions/implement/s.jsonl", BaselineTokens: 200000, Checkpoints: 3, MedianReduction: 50000},
		},
	}

	cr := convertReport(srcReport, "/logs", "implement")

	assert.Equal(t, 50, cr.SessionsScanned)
	assert.Equal(t, 30, cr.SessionsEligible)
	assert.Equal(t, 100, cr.Checkpoints)
	assert.Equal(t, "/logs", cr.LogsDir)
	assert.Equal(t, "implement", cr.Stage)

	// Exclusions must be sorted alphabetically.
	require.Len(t, cr.Exclusions, 5)
	assert.Equal(t, "branch_summary", cr.Exclusions[0].Reason)
	assert.Equal(t, 3, cr.Exclusions[0].Count)
	assert.Equal(t, "broken_chain", cr.Exclusions[1].Reason)
	assert.Equal(t, "has_compaction", cr.Exclusions[2].Reason)
	assert.Equal(t, "no_usage", cr.Exclusions[3].Reason)
	assert.Equal(t, "unreadable", cr.Exclusions[4].Reason)

	// Calibration.
	require.NotNil(t, cr.Calibration)
	assert.Equal(t, 3, cr.Calibration.Samples)
	assert.Equal(t, 3, cr.Calibration.SummaryRatioSamples)
	assert.Equal(t, 3, cr.Calibration.SummaryOutputSamples)
	assert.Equal(t, 3, cr.Calibration.PostContextSamples)

	// Thresholds.
	require.Len(t, cr.Thresholds, 1)
	th := cr.Thresholds[0]
	assert.Equal(t, 100000, th.Threshold)
	assert.Equal(t, 8, th.Compactions)
	assert.Equal(t, 510000, th.BaselineTokens)
	assert.Equal(t, 80000, th.LowReduction)
	assert.Equal(t, 60000, th.MedianReduction)
	assert.Equal(t, 40000, th.HighReduction)

	// Top sessions.
	require.Len(t, cr.TopSessions, 1)
	assert.Equal(t, "t-1", cr.TopSessions[0].Ticket)
	assert.Equal(t, "/logs/t-1/pi-sessions/implement/s.jsonl", cr.TopSessions[0].File)
	assert.Equal(t, 200000, cr.TopSessions[0].BaselineTokens)
	assert.Equal(t, 50000, cr.TopSessions[0].MedianReduction)
}

func TestConvertReport_NoCalibration(t *testing.T) {
	r := &compaction.Report{
		FilesScanned: 5,
		Eligible:     3,
		Excluded:     map[compaction.ExclusionReason]int{},
		Calibration: compaction.Calibration{
			SampleCount: 0,
		},
	}

	cr := convertReport(r, "/logs", "implement")
	assert.Nil(t, cr.Calibration)
	assert.Empty(t, cr.Thresholds)
	assert.Empty(t, cr.TopSessions)
}
