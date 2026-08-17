package compaction

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fileReader implements SessionReader for a testdata file.
type fileReader struct {
	path string
	f    *os.File
}

func (r *fileReader) Path() string             { return r.path }
func (r *fileReader) Open() (io.Reader, error) { f, err := os.Open(r.path); r.f = f; return f, err }
func (r *fileReader) Close() error {
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

type errorReader struct {
	path string
	err  error
}

func (r *errorReader) Path() string             { return r.path }
func (r *errorReader) Open() (io.Reader, error) { return nil, r.err }
func (r *errorReader) Close() error             { return nil }

func testdataPath(name string) string {
	return filepath.Join("testdata", name)
}

func openTestSession(t *testing.T, name string) *SessionTrace {
	t.Helper()
	f, err := os.Open(testdataPath(name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	trace, err := ReadSession(f, name)
	if err != nil {
		t.Fatalf("ReadSession %s: %v", name, err)
	}
	return trace
}

// --- Active path reconstruction tests ---------------------------------------

func TestActivePathLinear(t *testing.T) {
	trace := openTestSession(t, "linear.jsonl")

	if trace.SessionID != "sess-linear" {
		t.Errorf("session ID = %q, want sess-linear", trace.SessionID)
	}
	if trace.Exclusion != "" {
		t.Errorf("unexpected exclusion: %q", trace.Exclusion)
	}
	if len(trace.AssistantCalls) == 0 {
		t.Fatal("expected assistant calls, got none")
	}

	// Verify prompt context grows monotonically (linear session).
	for i := 1; i < len(trace.AssistantCalls); i++ {
		if trace.AssistantCalls[i].PromptContext <= trace.AssistantCalls[i-1].PromptContext {
			t.Errorf("call %d context %d <= call %d context %d",
				i, trace.AssistantCalls[i].PromptContext,
				i-1, trace.AssistantCalls[i-1].PromptContext)
		}
	}
}

func TestActivePathBranched(t *testing.T) {
	trace := openTestSession(t, "branched.jsonl")

	if trace.SessionID != "sess-branched" {
		t.Errorf("session ID = %q, want sess-branched", trace.SessionID)
	}

	// The active path should follow the last leaf (branch B).
	// Only one assistant call should be on the active branch (a-2b),
	// plus a-1 which is on the common prefix.
	if trace.Exclusion != "" {
		t.Errorf("unexpected exclusion: %q", trace.Exclusion)
	}

	// With branching, the active path should include a-1 and a-2b (not a-2a).
	found := false
	for _, ac := range trace.AssistantCalls {
		if ac.ID == "a-2b" {
			found = true
		}
		if ac.ID == "a-2a" {
			t.Error("active path includes branch A entry a-2a, expected only branch B")
		}
	}
	if !found {
		t.Error("active path missing branch B entry a-2b")
	}
}

// --- Exclusion tests --------------------------------------------------------

func TestExclusionNoUsage(t *testing.T) {
	trace := openTestSession(t, "partial_usage.jsonl")

	if trace.Exclusion != ExcludeNoUsage {
		t.Errorf("exclusion = %q, want %q", trace.Exclusion, ExcludeNoUsage)
	}
}

func TestUsageValues(t *testing.T) {
	ptr := func(v int) *int { return &v }
	tests := []struct {
		name  string
		usage *rawUsage
		want  usageState
	}{
		{name: "missing", usage: nil, want: usageMalformed},
		{name: "incomplete", usage: &rawUsage{Input: ptr(1)}, want: usageMalformed},
		{name: "negative", usage: &rawUsage{Input: ptr(-1), Output: ptr(0), CacheRead: ptr(0), CacheWrite: ptr(0)}, want: usageMalformed},
		{name: "all zero", usage: &rawUsage{Input: ptr(0), Output: ptr(0), CacheRead: ptr(0), CacheWrite: ptr(0)}, want: usageEmpty},
		{name: "valid", usage: &rawUsage{Input: ptr(1), Output: ptr(2), CacheRead: ptr(3), CacheWrite: ptr(4)}, want: usageValid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, _, got := tt.usage.values()
			if got != tt.want {
				t.Errorf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAbortedTurnDoesNotExcludeSession(t *testing.T) {
	jsonl := `{"type":"session","version":3,"id":"sess-aborted"}
{"type":"message","id":"u1","parentId":null,"message":{"role":"user","content":[]}}
{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[],"usage":{"input":10,"output":2,"cacheRead":5,"cacheWrite":5}}}
{"type":"message","id":"a2","parentId":"a1","message":{"role":"assistant","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"stopReason":"error","errorMessage":"Request timed out."}}
{"type":"message","id":"a3","parentId":"a2","message":{"role":"assistant","content":[],"usage":{"input":20,"output":3,"cacheRead":10,"cacheWrite":10}}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "aborted.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Exclusion != "" {
		t.Errorf("exclusion = %q, want eligible", trace.Exclusion)
	}
	if len(trace.AssistantCalls) != 2 {
		t.Fatalf("assistant calls = %d, want 2", len(trace.AssistantCalls))
	}
	if trace.TotalPrompt != 60 {
		t.Errorf("total prompt = %d, want 60", trace.TotalPrompt)
	}
}

func TestAllZeroUsageSessionHasNoUsage(t *testing.T) {
	jsonl := `{"type":"session","version":3,"id":"sess-empty-usage"}
{"type":"message","id":"u1","parentId":null,"message":{"role":"user","content":[]}}
{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "empty-usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Exclusion != ExcludeNoUsage {
		t.Errorf("exclusion = %q, want %q", trace.Exclusion, ExcludeNoUsage)
	}
}

func TestExclusionBrokenChain(t *testing.T) {
	trace := openTestSession(t, "broken_chain.jsonl")

	if trace.Exclusion != ExcludeBrokenChain {
		t.Errorf("exclusion = %q, want %q", trace.Exclusion, ExcludeBrokenChain)
	}
}

func TestExclusionHasCompaction(t *testing.T) {
	trace := openTestSession(t, "compacted.jsonl")

	if trace.Exclusion != ExcludeHasCompaction {
		t.Errorf("exclusion = %q, want %q", trace.Exclusion, ExcludeHasCompaction)
	}

	// Should still have compaction records for calibration.
	if len(trace.Compactions) == 0 {
		t.Error("expected compaction records for calibration, got none")
	}
}

func TestExclusionBranchSummary(t *testing.T) {
	// Create a session with a branch_summary entry inline.
	jsonl := `{"type":"session","version":3,"id":"sess-bs","timestamp":"2026-08-01T10:00:00.000Z","cwd":"/w"}
{"type":"model_change","id":"mc-1","parentId":null,"timestamp":"2026-08-01T10:00:00.100Z","provider":"anthropic","modelId":"claude-opus-5"}
{"type":"branch_summary","id":"bs-1","parentId":"mc-1","timestamp":"2026-08-01T10:00:01.000Z","summary":"branch done"}
{"type":"message","id":"u-1","parentId":"bs-1","timestamp":"2026-08-01T10:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
{"type":"message","id":"a-1","parentId":"u-1","timestamp":"2026-08-01T10:00:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input":100,"output":50,"cacheRead":500,"cacheWrite":1000,"totalTokens":1650},"stopReason":"end_turn"}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "inline-bs")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if trace.Exclusion != ExcludeBranchSummary {
		t.Errorf("exclusion = %q, want %q", trace.Exclusion, ExcludeBranchSummary)
	}
}

// --- Checkpoint detection tests ---------------------------------------------

func TestCheckpointFromTodos(t *testing.T) {
	trace := openTestSession(t, "linear.jsonl")

	// linear.jsonl has three phases. Phase 1 completes (L103 equivalent),
	// then Phase 2 completes (L111 equivalent). Phase 3 completion is the
	// final phase so it should NOT generate a checkpoint.
	if len(trace.Checkpoints) < 2 {
		t.Fatalf("expected at least 2 checkpoints from todos, got %d", len(trace.Checkpoints))
	}

	cp1 := trace.Checkpoints[0]
	if cp1.Source != "todo" {
		t.Errorf("checkpoint 0 source = %q, want todo", cp1.Source)
	}
	if cp1.CompletedPhase != "Phase 1" {
		t.Errorf("checkpoint 0 completed = %q, want Phase 1", cp1.CompletedPhase)
	}
	if cp1.NextPhase != "Phase 2" {
		t.Errorf("checkpoint 0 next = %q, want Phase 2", cp1.NextPhase)
	}

	cp2 := trace.Checkpoints[1]
	if cp2.CompletedPhase != "Phase 2" {
		t.Errorf("checkpoint 1 completed = %q, want Phase 2", cp2.CompletedPhase)
	}
}

func TestCheckpointExplicit(t *testing.T) {
	trace := openTestSession(t, "checkpoint.jsonl")

	if len(trace.Checkpoints) < 2 {
		t.Fatalf("expected at least 2 explicit checkpoints, got %d", len(trace.Checkpoints))
	}

	for _, cp := range trace.Checkpoints {
		if cp.Source != "explicit" {
			t.Errorf("checkpoint %q source = %q, want explicit", cp.CompletedPhase, cp.Source)
		}
	}

	if trace.Checkpoints[0].CompletedPhase != "Phase 1: setup" {
		t.Errorf("first checkpoint completed = %q", trace.Checkpoints[0].CompletedPhase)
	}
	if trace.Checkpoints[1].CompletedPhase != "Phase 2: impl" {
		t.Errorf("second checkpoint completed = %q", trace.Checkpoints[1].CompletedPhase)
	}
}

func TestCheckpointFailedStillDetected(t *testing.T) {
	trace := openTestSession(t, "checkpoint_failed.jsonl")

	if len(trace.Checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint (failed), got %d", len(trace.Checkpoints))
	}
	if trace.Checkpoints[0].CompletedPhase != "Phase 1" {
		t.Errorf("checkpoint completed = %q, want Phase 1", trace.Checkpoints[0].CompletedPhase)
	}
	if trace.Checkpoints[0].Outcome != "failed" {
		t.Errorf("checkpoint outcome = %q, want failed", trace.Checkpoints[0].Outcome)
	}
}

func TestEstimateFailedCheckpointCountsOnce(t *testing.T) {
	report, err := Estimate([]SessionReader{
		&fileReader{path: testdataPath("checkpoint_failed.jsonl")},
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Checkpoints != 1 {
		t.Errorf("checkpoints = %d, want 1", report.Checkpoints)
	}
}

func TestNoPhaseCheckpoints(t *testing.T) {
	trace := openTestSession(t, "no_phases.jsonl")

	if len(trace.Checkpoints) != 0 {
		t.Errorf("expected 0 checkpoints for non-phase todos, got %d", len(trace.Checkpoints))
	}
}

// --- Calibration tests ------------------------------------------------------

func TestCalibrationFromCompacted(t *testing.T) {
	trace := openTestSession(t, "compacted.jsonl")

	cal := BuildCalibration([]*SessionTrace{trace})

	if cal.SampleCount != 1 {
		t.Fatalf("sample count = %d, want 1", cal.SampleCount)
	}
	if len(cal.SummaryRatios) != 1 {
		t.Fatalf("summary ratios count = %d, want 1", len(cal.SummaryRatios))
	}

	// tokensBefore=180000 and summaryInput=90000 give a ratio of 0.5.
	expectedRatio := 90000.0 / 180000.0
	if cal.SummaryRatios[0] != expectedRatio {
		t.Errorf("summary ratio = %f, want %f", cal.SummaryRatios[0], expectedRatio)
	}
	if cal.SummaryOutputs[0] != 800 {
		t.Errorf("summary output = %d, want 800", cal.SummaryOutputs[0])
	}

	// Post-compaction context: input=8000 + cacheRead=2000 + cacheWrite=25000 = 35000
	if len(cal.PostCompactionCtx) != 1 {
		t.Fatalf("post-compaction ctx count = %d, want 1", len(cal.PostCompactionCtx))
	}
	if cal.PostCompactionCtx[0] != 35000 {
		t.Errorf("post-compaction ctx = %d, want 35000", cal.PostCompactionCtx[0])
	}
}

func TestCalibrationEmpty(t *testing.T) {
	trace := openTestSession(t, "linear.jsonl")
	cal := BuildCalibration([]*SessionTrace{trace})

	if cal.SampleCount != 0 {
		t.Errorf("sample count = %d, want 0", cal.SampleCount)
	}
}

func TestDeriveScenariosSingle(t *testing.T) {
	cal := Calibration{
		SampleCount:       1,
		SummaryRatios:     []float64{0.5},
		SummaryOutputs:    []int{800},
		PostCompactionCtx: []int{35000},
	}

	scenarios := DeriveScenarios(cal)
	if len(scenarios) != 3 {
		t.Fatalf("expected 3 scenarios, got %d", len(scenarios))
	}

	// With a single sample, all percentiles should be the same.
	for _, s := range scenarios {
		if s.SummaryRatio != 0.5 {
			t.Errorf("scenario %q ratio = %f, want 0.5", s.Label, s.SummaryRatio)
		}
		if s.SummaryOutput != 800 {
			t.Errorf("scenario %q output = %d, want 800", s.Label, s.SummaryOutput)
		}
		if s.PostCompactionCtx != 35000 {
			t.Errorf("scenario %q post-ctx = %d, want 35000", s.Label, s.PostCompactionCtx)
		}
	}
}

func TestDeriveScenariosNone(t *testing.T) {
	cal := Calibration{}
	if scenarios := DeriveScenarios(cal); scenarios != nil {
		t.Errorf("expected nil scenarios for empty calibration, got %d", len(scenarios))
	}
}

// --- Threshold replay tests -------------------------------------------------

func TestReplaySessionNoCheckpoints(t *testing.T) {
	trace := &SessionTrace{
		AssistantCalls: []AssistantCall{
			{ID: "a1", PromptContext: 10000, Output: 100},
			{ID: "a2", PromptContext: 20000, Output: 200},
			{ID: "a3", PromptContext: 30000, Output: 300},
		},
	}

	scenario := CalibrationScenario{
		Label:             "median",
		SummaryRatio:      0.5,
		SummaryOutput:     500,
		PostCompactionCtx: 15000,
	}

	res := replaySession(trace, 25000, scenario)

	// Without checkpoints, projected usage equals the baseline.
	if res.CompactionCount != 0 {
		t.Errorf("compaction count = %d, want 0", res.CompactionCount)
	}
	if res.ProjectedPrompt != res.BaselinePrompt {
		t.Errorf("projected %d != baseline %d", res.ProjectedPrompt, res.BaselinePrompt)
	}
}

func TestReplaySessionWithCheckpoint(t *testing.T) {
	trace := &SessionTrace{
		AssistantCalls: []AssistantCall{
			{ID: "a1", PromptContext: 10000, Output: 100},
			{ID: "a2", PromptContext: 50000, Output: 200}, // checkpoint after this
			{ID: "a3", PromptContext: 60000, Output: 300},
			{ID: "a4", PromptContext: 70000, Output: 400},
		},
		Checkpoints: []Checkpoint{
			{AfterCallIndex: 1, CompletedPhase: "Phase 1", NextPhase: "Phase 2", Source: "todo"},
		},
	}

	scenario := CalibrationScenario{
		Label:             "median",
		SummaryRatio:      0.5,
		SummaryOutput:     500,
		PostCompactionCtx: 20000,
	}

	res := replaySession(trace, 40000, scenario)

	// The first checkpoint exceeds the threshold.
	if res.CompactionCount != 1 {
		t.Errorf("compaction count = %d, want 1", res.CompactionCount)
	}
	if res.ProjectedSummary <= 0 {
		t.Error("expected positive summary tokens")
	}

	// After compaction, calls a3 and a4 should have reduced context.
	// baseOffset = PostCompactionCtx - nextObserved = 20000 - 60000 = -40000
	// a3 projected = 60000 + (-40000) = 20000
	// a4 projected = 70000 + (-40000) = 30000
	// Baseline: 10000 + 50000 + 60000 + 70000 = 190000
	// Projected: 10000 + 50000 + 20000 + 30000 = 110000
	if res.BaselinePrompt != 190000 {
		t.Errorf("baseline prompt = %d, want 190000", res.BaselinePrompt)
	}
	if res.ProjectedPrompt != 110000 {
		t.Errorf("projected prompt = %d, want 110000", res.ProjectedPrompt)
	}

	// Summary: summaryInput = 50000 * 0.5 = 25000, summaryOutput = 500
	// Total summary = 25500
	if res.ProjectedSummary != 25500 {
		t.Errorf("projected summary = %d, want 25500", res.ProjectedSummary)
	}

	// PromptReduction = 190000 - 110000 = 80000
	if res.PromptReduction != 80000 {
		t.Errorf("prompt reduction = %d, want 80000", res.PromptReduction)
	}

	// RawTokenReduction = 80000 - 25500 = 54500
	if res.RawTokenReduction != 54500 {
		t.Errorf("raw token reduction = %d, want 54500", res.RawTokenReduction)
	}
}

func TestReplaySessionBelowThreshold(t *testing.T) {
	trace := &SessionTrace{
		AssistantCalls: []AssistantCall{
			{ID: "a1", PromptContext: 10000, Output: 100},
			{ID: "a2", PromptContext: 20000, Output: 200},
			{ID: "a3", PromptContext: 30000, Output: 300},
		},
		Checkpoints: []Checkpoint{
			{AfterCallIndex: 1, CompletedPhase: "Phase 1", NextPhase: "Phase 2", Source: "todo"},
		},
	}

	scenario := CalibrationScenario{
		Label:             "median",
		SummaryRatio:      0.5,
		SummaryOutput:     500,
		PostCompactionCtx: 10000,
	}

	res := replaySession(trace, 50000, scenario)

	// The checkpoint context is below the threshold.
	if res.CompactionCount != 0 {
		t.Errorf("compaction count = %d, want 0", res.CompactionCount)
	}
	if res.ProjectedPrompt != res.BaselinePrompt {
		t.Errorf("projected %d != baseline %d", res.ProjectedPrompt, res.BaselinePrompt)
	}
}

// --- Estimate integration test ----------------------------------------------

func TestEstimateLinear(t *testing.T) {
	sessions := []SessionReader{
		&fileReader{path: testdataPath("linear.jsonl")},
	}
	report, err := Estimate(sessions, []int{40000, 60000}, 0)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	if report.FilesScanned != 1 {
		t.Errorf("files scanned = %d, want 1", report.FilesScanned)
	}
	if report.Eligible != 1 {
		t.Errorf("eligible = %d, want 1", report.Eligible)
	}
	if report.Checkpoints < 2 {
		t.Errorf("checkpoints = %d, want >= 2", report.Checkpoints)
	}
	// Threshold projections require calibration data.
	if len(report.Thresholds) != 0 {
		t.Errorf("thresholds = %d, want 0 (no calibration)", len(report.Thresholds))
	}
	if report.Calibration.SampleCount != 0 {
		t.Errorf("calibration samples = %d, want 0", report.Calibration.SampleCount)
	}
}

func TestEstimateCompacted(t *testing.T) {
	sessions := []SessionReader{
		&fileReader{path: testdataPath("compacted.jsonl")},
	}
	report, err := Estimate(sessions, []int{100000}, 0)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	if report.FilesScanned != 1 {
		t.Errorf("files scanned = %d, want 1", report.FilesScanned)
	}
	// Compacted session is excluded from eligible.
	if report.Eligible != 0 {
		t.Errorf("eligible = %d, want 0", report.Eligible)
	}
	if report.Excluded[ExcludeHasCompaction] != 1 {
		t.Errorf("excluded[has_compaction] = %d, want 1", report.Excluded[ExcludeHasCompaction])
	}
	if report.Calibration.SampleCount != 1 {
		t.Errorf("calibration samples = %d, want 1", report.Calibration.SampleCount)
	}
}

func TestEstimateMixed(t *testing.T) {
	// Mix a compacted session (for calibration) with a linear session (for replay).
	sessions := []SessionReader{
		&fileReader{path: testdataPath("compacted.jsonl")},
		&fileReader{path: testdataPath("linear.jsonl")},
	}
	report, err := Estimate(sessions, []int{30000, 50000}, 10)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	if report.FilesScanned != 2 {
		t.Errorf("files scanned = %d, want 2", report.FilesScanned)
	}
	if report.Eligible != 1 {
		t.Errorf("eligible = %d, want 1", report.Eligible)
	}
	if report.Calibration.SampleCount != 1 {
		t.Errorf("calibration samples = %d, want 1", report.Calibration.SampleCount)
	}
	if len(report.Thresholds) != 2 {
		t.Fatalf("threshold count = %d, want 2", len(report.Thresholds))
	}

	// At lower threshold, more compactions should occur.
	lowTh := report.Thresholds[0]
	highTh := report.Thresholds[1]
	if lowTh.Threshold != 30000 {
		t.Errorf("first threshold = %d, want 30000", lowTh.Threshold)
	}
	if highTh.Threshold != 50000 {
		t.Errorf("second threshold = %d, want 50000", highTh.Threshold)
	}
}

func TestEstimateAllExcluded(t *testing.T) {
	sessions := []SessionReader{
		&fileReader{path: testdataPath("partial_usage.jsonl")},
		&fileReader{path: testdataPath("broken_chain.jsonl")},
	}
	report, err := Estimate(sessions, []int{100000}, 0)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	if report.Eligible != 0 {
		t.Errorf("eligible = %d, want 0", report.Eligible)
	}
	if report.FilesScanned != 2 {
		t.Errorf("files scanned = %d, want 2", report.FilesScanned)
	}
}

// --- Empty and error edge cases ---------------------------------------------

func TestReadSessionEmptyFile(t *testing.T) {
	_, err := ReadSession(strings.NewReader(""), "empty.jsonl")
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestEstimateCountsEmptySessionAsUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Estimate([]SessionReader{&fileReader{path: path}}, []int{100000}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Excluded[ExcludeUnreadable] != 1 {
		t.Errorf("unreadable exclusions = %d, want 1", report.Excluded[ExcludeUnreadable])
	}
	if report.Excluded[ExcludeNoUsage] != 0 {
		t.Errorf("no_usage exclusions = %d, want 0", report.Excluded[ExcludeNoUsage])
	}
}

func TestEstimateCountsOpenFailureAsUnreadable(t *testing.T) {
	report, err := Estimate([]SessionReader{
		&errorReader{path: "denied.jsonl", err: errors.New("permission denied")},
		&fileReader{path: testdataPath("linear.jsonl")},
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesScanned != 2 {
		t.Errorf("files scanned = %d, want 2", report.FilesScanned)
	}
	if report.Eligible != 1 {
		t.Errorf("eligible = %d, want 1", report.Eligible)
	}
	if report.Excluded[ExcludeUnreadable] != 1 {
		t.Errorf("unreadable exclusions = %d, want 1", report.Excluded[ExcludeUnreadable])
	}
}

func TestReadSessionMalformedJSON(t *testing.T) {
	jsonl := `{"type":"session","version":3,"id":"s1","timestamp":"2026-08-01T10:00:00.000Z","cwd":"/w"}
not valid json
{"type":"model_change","id":"mc-1","parentId":null,"timestamp":"2026-08-01T10:00:00.100Z","provider":"anthropic","modelId":"claude-opus-5"}
{"type":"message","id":"u-1","parentId":"mc-1","timestamp":"2026-08-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
{"type":"message","id":"a-1","parentId":"u-1","timestamp":"2026-08-01T10:00:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input":100,"output":50,"cacheRead":500,"cacheWrite":1000,"totalTokens":1650},"stopReason":"end_turn"}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "malformed.jsonl")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	// Should skip malformed line and still parse the rest.
	if trace.Exclusion != "" {
		t.Errorf("unexpected exclusion: %q", trace.Exclusion)
	}
	if len(trace.AssistantCalls) != 1 {
		t.Errorf("assistant calls = %d, want 1", len(trace.AssistantCalls))
	}
}

// --- Percentile tests -------------------------------------------------------

func TestPercentileFloat64(t *testing.T) {
	tests := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []float64{0.5}, 0.5, 0.5},
		{"two_low", []float64{0.2, 0.8}, 0.25, 0.35},
		{"two_mid", []float64{0.2, 0.8}, 0.5, 0.5},
		{"three_mid", []float64{0.1, 0.5, 0.9}, 0.5, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PercentileFloat64(tt.sorted, tt.p)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("percentileFloat64(%v, %f) = %f, want %f", tt.sorted, tt.p, got, tt.want)
			}
		})
	}
}

func TestPercentileInt(t *testing.T) {
	tests := []struct {
		name   string
		sorted []int
		p      float64
		want   int
	}{
		{"empty", nil, 0.5, 0},
		{"single", []int{100}, 0.5, 100},
		{"two_mid", []int{100, 200}, 0.5, 150},
		{"three_mid", []int{100, 200, 300}, 0.5, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PercentileInt(tt.sorted, tt.p)
			if got != tt.want {
				t.Errorf("percentileInt(%v, %f) = %d, want %d", tt.sorted, tt.p, got, tt.want)
			}
		})
	}
}

// --- Prompt context calculation test ----------------------------------------

func TestPromptContextCalculation(t *testing.T) {
	// Verify that prompt context = input + cacheRead + cacheWrite.
	jsonl := `{"type":"session","version":3,"id":"s1","timestamp":"2026-08-01T10:00:00.000Z","cwd":"/w"}
{"type":"model_change","id":"mc-1","parentId":null,"timestamp":"2026-08-01T10:00:00.100Z","provider":"anthropic","modelId":"claude-opus-5"}
{"type":"message","id":"u-1","parentId":"mc-1","timestamp":"2026-08-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
{"type":"message","id":"a-1","parentId":"u-1","timestamp":"2026-08-01T10:00:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input":100,"output":50,"cacheRead":500,"cacheWrite":1000,"totalTokens":1650},"stopReason":"end_turn"}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "ctx.jsonl")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}

	if len(trace.AssistantCalls) != 1 {
		t.Fatalf("assistant calls = %d, want 1", len(trace.AssistantCalls))
	}

	ac := trace.AssistantCalls[0]
	// input(100) + cacheRead(500) + cacheWrite(1000) = 1600
	if ac.PromptContext != 1600 {
		t.Errorf("prompt context = %d, want 1600 (100+500+1000)", ac.PromptContext)
	}
	if ac.Output != 50 {
		t.Errorf("output = %d, want 50", ac.Output)
	}
}

// --- Compaction record extraction test --------------------------------------

func TestCompactionRecordExtraction(t *testing.T) {
	trace := openTestSession(t, "compacted.jsonl")

	if len(trace.Compactions) != 1 {
		t.Fatalf("compactions = %d, want 1", len(trace.Compactions))
	}

	rec := trace.Compactions[0]
	if rec.TokensBefore != 180000 {
		t.Errorf("tokensBefore = %d, want 180000", rec.TokensBefore)
	}
	if rec.SummaryInput != 90000 {
		t.Errorf("summaryInput = %d, want 90000", rec.SummaryInput)
	}
	if rec.SummaryOutput != 800 {
		t.Errorf("summaryOutput = %d, want 800", rec.SummaryOutput)
	}
	if !rec.HasPostCompaction {
		t.Error("expected HasPostCompaction=true")
	}
	// Post-compaction: input=8000 + cacheRead=2000 + cacheWrite=25000 = 35000
	if rec.PostCompactionCtx != 35000 {
		t.Errorf("postCompactionCtx = %d, want 35000", rec.PostCompactionCtx)
	}
}

// --- Checkpoint session with compaction calibration -------------------------

func TestCheckpointSessionCalibration(t *testing.T) {
	trace := openTestSession(t, "checkpoint.jsonl")

	// checkpoint.jsonl has a compaction, so it's excluded from projection but
	// provides calibration data.
	if trace.Exclusion != ExcludeHasCompaction {
		t.Errorf("exclusion = %q, want %q", trace.Exclusion, ExcludeHasCompaction)
	}

	if len(trace.Compactions) != 1 {
		t.Fatalf("compactions = %d, want 1", len(trace.Compactions))
	}

	rec := trace.Compactions[0]
	if rec.TokensBefore != 50002 {
		t.Errorf("tokensBefore = %d, want 50002", rec.TokensBefore)
	}
	if rec.SummaryInput != 25000 {
		t.Errorf("summaryInput = %d, want 25000", rec.SummaryInput)
	}
}

// --- Large line test (bufio.Reader vs Scanner) ------------------------------

func TestLargeLineHandled(t *testing.T) {
	// Create a session where one line exceeds the default scanner buffer.
	bigText := strings.Repeat("x", 2*1024*1024) // 2 MiB
	jsonl := `{"type":"session","version":3,"id":"s-big","timestamp":"2026-08-01T10:00:00.000Z","cwd":"/w"}
{"type":"model_change","id":"mc-1","parentId":null,"timestamp":"2026-08-01T10:00:00.100Z","provider":"anthropic","modelId":"claude-opus-5"}
{"type":"message","id":"u-1","parentId":"mc-1","timestamp":"2026-08-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"` + bigText + `"}]}}
{"type":"message","id":"a-1","parentId":"u-1","timestamp":"2026-08-01T10:00:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input":100,"output":50,"cacheRead":500,"cacheWrite":1000,"totalTokens":1650},"stopReason":"end_turn"}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "big.jsonl")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if trace.Exclusion != "" {
		t.Errorf("unexpected exclusion: %q", trace.Exclusion)
	}
	if len(trace.AssistantCalls) != 1 {
		t.Errorf("assistant calls = %d, want 1", len(trace.AssistantCalls))
	}
}

// --- Snake-case checkpoint parsing ------------------------------------------

func TestCheckpointSnakeCase(t *testing.T) {
	trace := openTestSession(t, "checkpoint_snake.jsonl")

	if len(trace.Checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint from snake_case, got %d", len(trace.Checkpoints))
	}
	cp := trace.Checkpoints[0]
	if cp.Source != "explicit" {
		t.Errorf("source = %q, want explicit", cp.Source)
	}
	if cp.CompletedPhase != "Phase 1: setup" {
		t.Errorf("completed = %q, want Phase 1: setup", cp.CompletedPhase)
	}
	if cp.NextPhase != "Phase 2: impl" {
		t.Errorf("next = %q, want Phase 2: impl", cp.NextPhase)
	}
}

// --- DiscoverSessions filesystem tests --------------------------------------

func TestDiscoverSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	for _, ticket := range []string{"t-aaa", "t-bbb"} {
		stageDir := filepath.Join(root, ticket, "pi-sessions", "implement")
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(stageDir, "session.jsonl")
		data := []byte("{\"type\":\"session\",\"version\":3,\"id\":\"s1\"}\n")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		mtime := now.Add(-time.Hour)
		if ticket == "t-bbb" {
			mtime = now.Add(-20 * time.Minute)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	marker := []byte(`{"started_at":"2026-08-01T11:40:00Z"}`)
	if err := os.WriteFile(filepath.Join(root, "t-bbb", "implement.session"), marker, 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, err := discoverSessions(root, "implement", now)
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	var liveCount int
	for _, sr := range sessions {
		info, ok := sr.(SessionInfo)
		if !ok {
			t.Fatal("session reader does not implement SessionInfo")
		}
		if info.IsLive() {
			liveCount++
		}
		if info.Stage() != "implement" {
			t.Errorf("stage = %q, want implement", info.Stage())
		}
	}
	if liveCount != 1 {
		t.Errorf("live count = %d, want 1", liveCount)
	}
}

func TestDiscoverSessionsMarksFreshFileLiveWithoutMarker(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	stageDir := filepath.Join(root, "t-fresh", "pi-sessions", "implement")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stageDir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := now.Add(-time.Minute)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	sessions, err := discoverSessions(root, "implement", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !sessions[0].(SessionInfo).IsLive() {
		t.Fatal("fresh session without a marker should be live")
	}
}

func TestDiscoverSessionsCopiedFileUsesRecordedActivity(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	stageDir := filepath.Join(root, "t-copied", "pi-sessions", "implement")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stageDir, "session.jsonl")
	data := []byte("{\"type\":\"session\",\"timestamp\":\"2026-08-01T10:00:00Z\"}\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}

	sessions, err := discoverSessions(root, "implement", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].(SessionInfo).IsLive() {
		t.Fatal("a copied session with old recorded activity should not be live")
	}
}

func TestDiscoverSessionsMarkerOnlyClaimsCurrentAttempt(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	stageDir := filepath.Join(root, "t-retry", "pi-sessions", "implement")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, mtime := range map[string]time.Time{
		"old.jsonl": now.Add(-2 * time.Hour),
		"new.jsonl": now.Add(-30 * time.Minute),
	} {
		path := filepath.Join(stageDir, name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	marker := []byte(`{"started_at":"2026-08-01T11:30:00Z"}`)
	if err := os.WriteFile(filepath.Join(root, "t-retry", "implement.session"), marker, 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, err := discoverSessions(root, "implement", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
	for _, session := range sessions {
		wantLive := filepath.Base(session.Path()) == "new.jsonl"
		if session.(SessionInfo).IsLive() != wantLive {
			t.Errorf("%s live = %t, want %t", session.Path(), session.(SessionInfo).IsLive(), wantLive)
		}
	}
}

func TestDiscoverSessionsInvalidMarkerIsConservative(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	stageDir := filepath.Join(root, "t-invalid", "pi-sessions", "implement")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stageDir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := now.Add(-time.Hour)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "t-invalid", "implement.session"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, err := discoverSessions(root, "implement", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !sessions[0].(SessionInfo).IsLive() {
		t.Fatal("invalid marker should conservatively mark the session live")
	}
}

func TestDiscoverSessions_NoTickets(t *testing.T) {
	root := t.TempDir()
	sessions, err := DiscoverSessions(root, "implement")
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestDiscoverSessions_NoStageDir(t *testing.T) {
	root := t.TempDir()
	// Ticket exists but no pi-sessions/implement.
	if err := os.MkdirAll(filepath.Join(root, "t-aaa"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessions, err := DiscoverSessions(root, "implement")
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

// --- Live resume exclusion via Estimate -------------------------------------

func TestEstimateLiveExclusion(t *testing.T) {
	root := t.TempDir()

	ticket := "t-live"
	stageDir := filepath.Join(root, ticket, "pi-sessions", "implement")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Copy linear.jsonl into the temp directory.
	src, err := os.ReadFile(testdataPath("linear.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "session.jsonl"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	// Create the live session marker.
	if err := os.WriteFile(filepath.Join(root, ticket, "implement.session"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, err := DiscoverSessions(root, "implement")
	if err != nil {
		t.Fatal(err)
	}

	report, err := Estimate(sessions, []int{30000}, 10)
	if err != nil {
		t.Fatal(err)
	}

	if report.Eligible != 0 {
		t.Errorf("eligible = %d, want 0 (live session should be excluded)", report.Eligible)
	}
	if report.Excluded[ExcludeLiveResume] != 1 {
		t.Errorf("excluded[live_resume] = %d, want 1", report.Excluded[ExcludeLiveResume])
	}
}

// --- Replay with multiple checkpoints (cascading compactions) ---------------

func TestReplayMultipleCheckpoints(t *testing.T) {
	trace := &SessionTrace{
		AssistantCalls: []AssistantCall{
			{ID: "a1", PromptContext: 10000, Output: 100},
			{ID: "a2", PromptContext: 60000, Output: 200}, // checkpoint 1 after this
			{ID: "a3", PromptContext: 70000, Output: 300},
			{ID: "a4", PromptContext: 80000, Output: 400},
			{ID: "a5", PromptContext: 120000, Output: 500}, // checkpoint 2 after this
			{ID: "a6", PromptContext: 130000, Output: 600},
			{ID: "a7", PromptContext: 140000, Output: 700},
		},
		Checkpoints: []Checkpoint{
			{AfterCallIndex: 1, CompletedPhase: "Phase 1", NextPhase: "Phase 2", Source: "todo"},
			{AfterCallIndex: 4, CompletedPhase: "Phase 2", NextPhase: "Phase 3", Source: "todo"},
		},
	}

	scenario := CalibrationScenario{
		Label:             "median",
		SummaryRatio:      0.5,
		SummaryOutput:     500,
		PostCompactionCtx: 20000,
	}

	res := replaySession(trace, 50000, scenario)

	// First compaction at call 1: ctx=60000 > 50000.
	// baseOffset = 20000 - 70000 = -50000
	// a3 projected = 70000 - 50000 = 20000
	// a4 projected = 80000 - 50000 = 30000
	// The second checkpoint has 70000 projected tokens, above the 50000 threshold.
	// Second compaction at call 4: projected ctx = 70000.
	// baseOffset = 20000 - 130000 = -110000
	// a6 projected = 130000 - 110000 = 20000
	// a7 projected = 140000 - 110000 = 30000
	if res.CompactionCount != 2 {
		t.Errorf("compaction count = %d, want 2", res.CompactionCount)
	}

	expectedBaseline := 10000 + 60000 + 70000 + 80000 + 120000 + 130000 + 140000
	if res.BaselinePrompt != expectedBaseline {
		t.Errorf("baseline = %d, want %d", res.BaselinePrompt, expectedBaseline)
	}

	// Projected: 10000 + 60000 + 20000 + 30000 + 70000 + 20000 + 30000 = 240000
	expectedProjected := 10000 + 60000 + 20000 + 30000 + 70000 + 20000 + 30000
	if res.ProjectedPrompt != expectedProjected {
		t.Errorf("projected = %d, want %d", res.ProjectedPrompt, expectedProjected)
	}

	// Summary overhead: compaction 1: 60000*0.5 + 500 = 30500, compaction 2: 70000*0.5 + 500 = 35500
	expectedSummary := 30500 + 35500
	if res.ProjectedSummary != expectedSummary {
		t.Errorf("summary = %d, want %d", res.ProjectedSummary, expectedSummary)
	}
}

// --- Multiple calibration samples -------------------------------------------

func TestCalibrationMultipleSamples(t *testing.T) {
	// Build three synthetic traces with compaction records.
	traces := []*SessionTrace{
		{
			Compactions: []Record{
				{TokensBefore: 100000, SummaryInput: 10000, SummaryOutput: 500, PostCompactionCtx: 15000, HasPostCompaction: true},
			},
		},
		{
			Compactions: []Record{
				{TokensBefore: 200000, SummaryInput: 40000, SummaryOutput: 800, PostCompactionCtx: 25000, HasPostCompaction: true},
			},
		},
		{
			Compactions: []Record{
				{TokensBefore: 150000, SummaryInput: 30000, SummaryOutput: 600, PostCompactionCtx: 20000, HasPostCompaction: true},
			},
		},
	}

	cal := BuildCalibration(traces)

	if cal.SampleCount != 3 {
		t.Fatalf("sample count = %d, want 3", cal.SampleCount)
	}

	// The sorted ratios are [0.1, 0.2, 0.2].
	if len(cal.SummaryRatios) != 3 {
		t.Fatalf("ratios count = %d, want 3", len(cal.SummaryRatios))
	}
	if cal.SummaryRatios[0] != 0.1 {
		t.Errorf("ratio[0] = %f, want 0.1", cal.SummaryRatios[0])
	}

	scenarios := DeriveScenarios(cal)
	if len(scenarios) != 3 {
		t.Fatalf("scenarios = %d, want 3", len(scenarios))
	}
	// Median (p50) of [0.1, 0.2, 0.2] = 0.2
	if scenarios[1].SummaryRatio != 0.2 {
		t.Errorf("median ratio = %f, want 0.2", scenarios[1].SummaryRatio)
	}
}

// --- Top sessions ranking ---------------------------------------------------

func TestTopSessionsRanking(t *testing.T) {
	// Use synthetic sessions to test that top sessions are ranked by
	// median raw-token reduction.
	sessions := []SessionReader{
		&fileReader{path: testdataPath("compacted.jsonl")},
		&fileReader{path: testdataPath("linear.jsonl")},
	}

	report, err := Estimate(sessions, []int{30000}, 10)
	if err != nil {
		t.Fatal(err)
	}

	if report.Calibration.SampleCount == 0 {
		t.Fatal("expected calibration samples from compacted.jsonl")
	}

	// There should be top sessions (linear.jsonl is eligible with checkpoints).
	// Whether reduction > 0 depends on threshold vs context.
	// At threshold 30000, linear.jsonl has contexts growing from ~25k to ~62k,
	// so at least one checkpoint should trigger compaction.
	if len(report.Thresholds) != 1 {
		t.Fatalf("thresholds = %d, want 1", len(report.Thresholds))
	}

	// Top sessions should include median reduction.
	for _, ts := range report.TopSessions {
		if ts.MedianReduction <= 0 {
			t.Errorf("top session %q has median reduction %d, want > 0", ts.File, ts.MedianReduction)
		}
	}
}

// --- Compaction without post-compaction call ---------------------------------

func TestPostCompactionSkipsAbortedTurn(t *testing.T) {
	jsonl := `{"type":"session","version":3,"id":"sess-post-abort"}
{"type":"message","id":"u1","parentId":null,"message":{"role":"user","content":[]}}
{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[],"usage":{"input":10,"output":2,"cacheRead":5,"cacheWrite":5}}}
{"type":"compaction","id":"c1","parentId":"a1","tokensBefore":100,"usage":{"input":50,"output":5,"cacheRead":0,"cacheWrite":0}}
{"type":"message","id":"a2","parentId":"c1","message":{"role":"assistant","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"stopReason":"error"}}
{"type":"message","id":"a3","parentId":"a2","message":{"role":"assistant","content":[],"usage":{"input":20,"output":3,"cacheRead":10,"cacheWrite":10}}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "post-abort.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Compactions) != 1 {
		t.Fatalf("compactions = %d, want 1", len(trace.Compactions))
	}
	rec := trace.Compactions[0]
	if !rec.HasPostCompaction || rec.PostCompactionCtx != 40 {
		t.Errorf("post-compaction context = %d, present %t; want 40, true", rec.PostCompactionCtx, rec.HasPostCompaction)
	}
}

func TestCompactionNoPostCompaction(t *testing.T) {
	jsonl := `{"type":"session","version":3,"id":"s1","timestamp":"2026-08-01T10:00:00.000Z","cwd":"/w"}
{"type":"model_change","id":"mc-1","parentId":null,"timestamp":"2026-08-01T10:00:00.100Z","provider":"anthropic","modelId":"claude-opus-5"}
{"type":"message","id":"u-1","parentId":"mc-1","timestamp":"2026-08-01T10:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"start"}]}}
{"type":"message","id":"a-1","parentId":"u-1","timestamp":"2026-08-01T10:00:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input":100,"output":50,"cacheRead":500,"cacheWrite":1000,"totalTokens":1650},"stopReason":"end_turn"}}
{"type":"compaction","id":"comp-1","parentId":"a-1","timestamp":"2026-08-01T10:01:00.000Z","summary":"summary","tokensBefore":5000,"usage":{"input":2500,"output":400,"cacheRead":0,"cacheWrite":0}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "no-post.jsonl")
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}

	if len(trace.Compactions) != 1 {
		t.Fatalf("compactions = %d, want 1", len(trace.Compactions))
	}
	rec := trace.Compactions[0]
	if rec.HasPostCompaction {
		t.Error("expected HasPostCompaction=false when no assistant call follows")
	}
	if rec.PostCompactionCtx != 0 {
		t.Errorf("post ctx = %d, want 0", rec.PostCompactionCtx)
	}

	cal := BuildCalibration([]*SessionTrace{trace})
	if cal.SampleCount != 0 {
		t.Errorf("sample count = %d, want 0", cal.SampleCount)
	}
	if scenarios := DeriveScenarios(cal); scenarios != nil {
		t.Errorf("expected nil scenarios, got %d", len(scenarios))
	}
}

// --- Table-driven exclusion summary test ------------------------------------

func TestExclusionReasons(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		exclusion ExclusionReason
	}{
		{"no_usage", "partial_usage.jsonl", ExcludeNoUsage},
		{"broken_chain", "broken_chain.jsonl", ExcludeBrokenChain},
		{"has_compaction", "compacted.jsonl", ExcludeHasCompaction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trace := openTestSession(t, tt.fixture)
			if trace.Exclusion != tt.exclusion {
				t.Errorf("exclusion = %q, want %q", trace.Exclusion, tt.exclusion)
			}
		})
	}
}

// --- ProjectThreshold aggregate test ----------------------------------------

func TestProjectThresholdAggregate(t *testing.T) {
	eligible := &SessionTrace{
		AssistantCalls: []AssistantCall{
			{ID: "a1", PromptContext: 10000, Output: 100},
			{ID: "a2", PromptContext: 60000, Output: 200},
			{ID: "a3", PromptContext: 80000, Output: 300},
		},
		Checkpoints: []Checkpoint{
			{AfterCallIndex: 1, CompletedPhase: "Phase 1", NextPhase: "Phase 2", Source: "todo"},
		},
	}
	excluded := &SessionTrace{
		Exclusion: ExcludeNoUsage,
		AssistantCalls: []AssistantCall{
			{ID: "x1", PromptContext: 99999, Output: 999},
		},
	}

	scenario := CalibrationScenario{
		Label:             "median",
		SummaryRatio:      0.5,
		SummaryOutput:     500,
		PostCompactionCtx: 20000,
	}

	res := ProjectThreshold([]*SessionTrace{eligible, excluded}, 50000, scenario)

	// Only the eligible trace should contribute.
	if res.BaselinePrompt != 150000 { // 10000+60000+80000
		t.Errorf("baseline prompt = %d, want 150000", res.BaselinePrompt)
	}
	if res.CompactionCount != 1 {
		t.Errorf("compaction count = %d, want 1", res.CompactionCount)
	}
	// Excluded trace should not contribute.
	if res.BaselineOutput != 600 { // 100+200+300
		t.Errorf("baseline output = %d, want 600", res.BaselineOutput)
	}
}

func TestMalformedAssistantUsageExcluded(t *testing.T) {
	jsonl := `{"type":"session","version":3,"id":"sess-malformed"}
{"type":"message","id":"u1","parentId":null,"message":{"role":"user","content":[]}}
{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[],"usage":{"input":10,"output":2,"cacheRead":5}}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "malformed.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Exclusion != ExcludeNoUsage {
		t.Errorf("exclusion = %q, want %q", trace.Exclusion, ExcludeNoUsage)
	}
}

func TestDuplicateEntryIDExcluded(t *testing.T) {
	jsonl := `{"type":"session","version":3,"id":"sess-duplicate"}
{"type":"message","id":"u1","parentId":null,"message":{"role":"user","content":[]}}
{"type":"message","id":"same","parentId":"u1","message":{"role":"assistant","content":[],"usage":{"input":10,"output":2,"cacheRead":5,"cacheWrite":5}}}
{"type":"message","id":"same","parentId":"same","message":{"role":"assistant","content":[],"usage":{"input":20,"output":2,"cacheRead":5,"cacheWrite":5}}}
`
	trace, err := ReadSession(strings.NewReader(jsonl), "duplicate.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Exclusion != ExcludeBrokenChain {
		t.Errorf("exclusion = %q, want %q", trace.Exclusion, ExcludeBrokenChain)
	}
}

func TestExtractPhasesRequiresEveryTodoInGroup(t *testing.T) {
	phases := extractPhases([]rawTodo{
		{ID: 1, Title: "Phase 1: implementation", Status: "completed"},
		{ID: 2, Title: "Phase 1: tests", Status: "in-progress"},
		{ID: 3, Title: "Phase 2: docs", Status: "not-started"},
	})
	if phases[1] != "in-progress" {
		t.Errorf("phase 1 status = %q, want in-progress", phases[1])
	}
	if phases[2] != "not-started" {
		t.Errorf("phase 2 status = %q, want not-started", phases[2])
	}
}
