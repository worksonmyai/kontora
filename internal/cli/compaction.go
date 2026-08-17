package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/worksonmyai/kontora/internal/compaction"
)

// CompactionReport holds the values rendered by the estimator command.
type CompactionReport struct {
	LogsDir string
	Stage   string

	SessionsScanned  int
	SessionsEligible int
	Exclusions       []ExclusionCount
	Checkpoints      int

	// Calibration is nil when no usable compaction samples exist.
	Calibration *CalibrationData

	// Thresholds is empty when Calibration is nil.
	Thresholds []ThresholdRow

	// TopSessions ranks projected median reduction at the lowest threshold.
	TopSessions []TopSession
}

// ExclusionCount is one reason sessions were excluded and how many.
type ExclusionCount struct {
	Reason string
	Count  int
}

// CalibrationData holds statistics derived from observed compaction records.
type CalibrationData struct {
	Samples              int
	SummaryRatioSamples  int
	SummaryOutputSamples int
	PostContextSamples   int
	SummaryRatio         Percentiles // summary input / tokensBefore
	SummaryOutput        Percentiles // summary output tokens
	PostContext          Percentiles // first assistant prompt context after compaction
}

// Percentiles holds the low, median, and high values of a distribution.
// These are labelled as scenarios, not confidence intervals.
type Percentiles struct {
	Low    float64
	Median float64
	High   float64
}

// ThresholdRow is one row of the scenarios table.
type ThresholdRow struct {
	Threshold       int
	Compactions     int // total projected compactions across all sessions
	BaselineTokens  int // total baseline tokens across all sessions
	LowReduction    int // tokens saved in the low calibration scenario
	MedianReduction int // tokens saved in the median calibration scenario
	HighReduction   int // tokens saved in the high calibration scenario
}

// TopSession is one entry in the top sessions list.
type TopSession struct {
	Ticket          string
	Stage           string
	File            string
	BaselineTokens  int
	Checkpoints     int
	MedianReduction int
}

// ParseThresholds parses a comma-separated list of positive integers.
func ParseThresholds(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	result := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid threshold %q: %w", p, err)
		}
		if v <= 0 {
			return nil, fmt.Errorf("threshold must be positive, got %d", v)
		}
		result = append(result, v)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("at least one threshold is required")
	}
	return result, nil
}

// EstimateCompaction produces a compaction report by scanning session files.
// It delegates to internal/compaction for discovery and estimation.
func EstimateCompaction(logsDir, stage string, thresholds []int, top int) (*CompactionReport, error) {
	if top < 0 {
		return nil, fmt.Errorf("top must not be negative, got %d", top)
	}
	sessions, err := compaction.DiscoverSessions(logsDir, stage)
	if err != nil {
		return nil, fmt.Errorf("discover sessions: %w", err)
	}

	report, err := compaction.Estimate(sessions, thresholds, top)
	if err != nil {
		return nil, fmt.Errorf("estimate: %w", err)
	}

	return convertReport(report, logsDir, stage), nil
}

// convertReport maps a compaction.Report to the CLI CompactionReport.
func convertReport(r *compaction.Report, logsDir, stage string) *CompactionReport {
	cr := &CompactionReport{
		LogsDir:          logsDir,
		Stage:            stage,
		SessionsScanned:  r.FilesScanned,
		SessionsEligible: r.Eligible,
		Checkpoints:      r.Checkpoints,
	}

	// Convert exclusions sorted by reason name.
	if len(r.Excluded) > 0 {
		reasons := make([]string, 0, len(r.Excluded))
		for reason := range r.Excluded {
			reasons = append(reasons, string(reason))
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			cr.Exclusions = append(cr.Exclusions, ExclusionCount{
				Reason: reason,
				Count:  r.Excluded[compaction.ExclusionReason(reason)],
			})
		}
	}

	// Convert calibration.
	cal := r.Calibration
	if cal.SampleCount > 0 && len(cal.PostCompactionCtx) > 0 {
		cr.Calibration = &CalibrationData{
			Samples:              cal.SampleCount,
			SummaryRatioSamples:  len(cal.SummaryRatios),
			SummaryOutputSamples: len(cal.SummaryOutputs),
			PostContextSamples:   len(cal.PostCompactionCtx),
			SummaryRatio: Percentiles{
				Low:    compaction.PercentileFloat64(cal.SummaryRatios, 0.25),
				Median: compaction.PercentileFloat64(cal.SummaryRatios, 0.5),
				High:   compaction.PercentileFloat64(cal.SummaryRatios, 0.75),
			},
			SummaryOutput: Percentiles{
				Low:    float64(compaction.PercentileInt(cal.SummaryOutputs, 0.25)),
				Median: float64(compaction.PercentileInt(cal.SummaryOutputs, 0.5)),
				High:   float64(compaction.PercentileInt(cal.SummaryOutputs, 0.75)),
			},
			PostContext: Percentiles{
				Low:    float64(compaction.PercentileInt(cal.PostCompactionCtx, 0.25)),
				Median: float64(compaction.PercentileInt(cal.PostCompactionCtx, 0.5)),
				High:   float64(compaction.PercentileInt(cal.PostCompactionCtx, 0.75)),
			},
		}
	}

	// Convert thresholds.
	for _, th := range r.Thresholds {
		row := ThresholdRow{
			Threshold: th.Threshold,
		}
		// Scenarios: [0]=low, [1]=median, [2]=high.
		row.Compactions = th.Scenarios[1].CompactionCount
		row.BaselineTokens = th.Scenarios[1].BaselinePrompt + th.Scenarios[1].BaselineOutput
		row.LowReduction = th.Scenarios[0].RawTokenReduction
		row.MedianReduction = th.Scenarios[1].RawTokenReduction
		row.HighReduction = th.Scenarios[2].RawTokenReduction
		cr.Thresholds = append(cr.Thresholds, row)
	}

	// Convert top sessions.
	for _, ts := range r.TopSessions {
		cr.TopSessions = append(cr.TopSessions, TopSession{
			Ticket:          ts.Ticket,
			Stage:           ts.Stage,
			File:            ts.File,
			BaselineTokens:  ts.BaselineTokens,
			Checkpoints:     ts.Checkpoints,
			MedianReduction: ts.MedianReduction,
		})
	}

	return cr
}

// PrintCompactionEstimate renders the estimator report to w.
func PrintCompactionEstimate(r *CompactionReport, w io.Writer) error {
	if r == nil {
		return fmt.Errorf("nil report")
	}

	// Header.
	fmt.Fprintf(w, "%s\n", styleBold.Render("Compaction Estimate"))
	fmt.Fprintf(w, "  stage:  %s\n", r.Stage)
	fmt.Fprintf(w, "  logs:   %s\n", r.LogsDir)
	fmt.Fprintln(w)

	// Coverage.
	fmt.Fprintf(w, "%s\n", styleBold.Render("Coverage"))
	fmt.Fprintf(w, "  sessions scanned:    %d\n", r.SessionsScanned)
	fmt.Fprintf(w, "  sessions eligible:   %d\n", r.SessionsEligible)
	fmt.Fprintf(w, "  checkpoints:         %d\n", r.Checkpoints)
	if len(r.Exclusions) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s\n", styleFaint.Render("Exclusions:"))
		for _, e := range r.Exclusions {
			fmt.Fprintf(w, "    %-22s %d\n", e.Reason, e.Count)
		}
	}
	fmt.Fprintln(w)

	// Calibration.
	if r.Calibration == nil {
		fmt.Fprintf(w, "%s\n", styleBold.Render("Calibration"))
		fmt.Fprintf(w, "  %s\n", styleWarn.Render("No usable compaction samples."))
		fmt.Fprintf(w, "  %s\n", styleWarn.Render("Token-reduction scenarios cannot be projected without calibration data."))
		fmt.Fprintln(w)
		printCompactionLimits(w, 0)
		return nil
	}

	c := r.Calibration
	fmt.Fprintf(w, "%s\n", styleBold.Render("Calibration"))
	printPercentiles(w, "summary ratio", c.SummaryRatioSamples, c.SummaryRatio, false)
	printPercentiles(w, "summary output", c.SummaryOutputSamples, c.SummaryOutput, true)
	printPercentiles(w, "post context", c.PostContextSamples, c.PostContext, true)
	fmt.Fprintln(w)

	// Scenarios.
	if len(r.Thresholds) > 0 {
		fmt.Fprintf(w, "%s\n", styleBold.Render("Scenarios"))
		fmt.Fprintf(w, "  %s\n", styleFaint.Render(
			"Calibration percentiles, not reduction bounds: CAL P25 uses smaller values; CAL P75 uses larger values."))
		header := fmt.Sprintf("  %-12s %-13s %-20s %-20s %-20s %s",
			"THRESHOLD", "COMPACTIONS", "CAL P25", "CAL P50", "CAL P75", "BASELINE")
		fmt.Fprintln(w, styleFaint.Render(header))
		for _, row := range r.Thresholds {
			fmt.Fprintf(w, "  %-12s %-13d %-20s %-20s %-20s %s\n",
				formatTokenCount(row.Threshold),
				row.Compactions,
				formatReduction(row.LowReduction, row.BaselineTokens),
				formatReduction(row.MedianReduction, row.BaselineTokens),
				formatReduction(row.HighReduction, row.BaselineTokens),
				formatTokenCount(row.BaselineTokens))
		}
		fmt.Fprintln(w)
	}

	// Top sessions.
	if len(r.TopSessions) > 0 {
		fmt.Fprintf(w, "%s\n", styleBold.Render("Top Sessions"))
		topHeader := fmt.Sprintf("  %-14s %-14s %-14s %-14s %-13s %s",
			"TICKET", "STAGE", "BASELINE", "REDUCTION", "CHECKPOINTS", "SESSION")
		fmt.Fprintln(w, styleFaint.Render(topHeader))
		for _, s := range r.TopSessions {
			fmt.Fprintf(w, "  %-14s %-14s %-14s %-14s %-13d %s\n",
				s.Ticket, s.Stage, formatTokenCount(s.BaselineTokens),
				formatTokenCount(s.MedianReduction), s.Checkpoints, filepath.Base(s.File))
		}
		fmt.Fprintln(w)
	}

	// Limits.
	printCompactionLimits(w, c.Samples)
	return nil
}

func printPercentiles(w io.Writer, label string, samples int, p Percentiles, intFmt bool) {
	label = fmt.Sprintf("%s (%d):", label, samples)
	if intFmt {
		fmt.Fprintf(w, "  %-22s low %-10.0f median %-10.0f high %.0f\n",
			label, p.Low, p.Median, p.High)
	} else {
		fmt.Fprintf(w, "  %-22s low %-10.4f median %-10.4f high %.4f\n",
			label, p.Low, p.Median, p.High)
	}
}

func printCompactionLimits(w io.Writer, samples int) {
	fmt.Fprintf(w, "%s\n", styleBold.Render("Limits"))
	for _, l := range []string{
		"Token projection replays the recorded call sequence; a compacted model may make different tool calls.",
		"Cache identity and retention change after compaction; projected context does not account for cache misses.",
		"Dollar cost is not projected: model pricing and cache discounts vary.",
		fmt.Sprintf("Calibration uses %d complete compaction sample(s).", samples),
	} {
		fmt.Fprintf(w, "  - %s\n", l)
	}
}

// formatTokenCount formats a token count with comma separators.
func formatTokenCount(n int) string {
	if n < 0 {
		return "-" + insertCommas(strconv.Itoa(-n))
	}
	return insertCommas(strconv.Itoa(n))
}

// formatReduction formats a token reduction as a negative delta with a
// percentage of the baseline.
func formatReduction(reduction, baseline int) string {
	if reduction == 0 {
		return "0"
	}
	pct := 0
	if baseline > 0 {
		pct = reduction * 100 / baseline
	}
	if reduction < 0 {
		return fmt.Sprintf("+%s (%d%%)", insertCommas(strconv.Itoa(-reduction)), pct)
	}
	return fmt.Sprintf("-%s (%d%%)", insertCommas(strconv.Itoa(reduction)), pct)
}

// insertCommas adds thousand-separator commas to a non-negative numeric string.
func insertCommas(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	first := n % 3
	if first == 0 {
		first = 3
	}
	b.WriteString(s[:first])
	for i := first; i < n; i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
