package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/metrics"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// histogramPoint returns the single data point of a histogram metric.
func histogramPoint(t *testing.T, m metricdata.Metrics) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s must be a histogram, got %T", m.Name, m.Data)
	require.Len(t, hist.DataPoints, 1)
	return hist.DataPoints[0]
}

// gaugeValue returns the single data point value of an int64 gauge.
func gaugeValue(t *testing.T, got map[string]metricdata.Metrics, name string) int64 {
	t.Helper()
	m, ok := got[name]
	require.True(t, ok, "%s must be exported", name)
	g, ok := m.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "%s must be a gauge, got %T", name, m.Data)
	require.Len(t, g.DataPoints, 1)
	return g.DataPoints[0].Value
}

// TestMetricsPipelineRun drives real tickets through the daemon and asserts on
// what the run recorded. Collection happens after the daemon has stopped:
// reading the reader while it runs races the goroutines still recording.
func TestMetricsPipelineRun(t *testing.T) {
	tests := []struct {
		name         string
		agent1Binary string
		pipeline     string
		wantStatus   ticket.Status
		wantOutcomes map[string]int64
		wantActions  map[string]int64
	}{
		{
			name:         "a two-stage pipeline records one success per stage",
			agent1Binary: "true",
			pipeline:     "two-stage",
			wantStatus:   ticket.StatusDone,
			wantOutcomes: map[string]int64{"success": 2},
			wantActions:  map[string]int64{"advance": 1, "complete": 1},
		},
		{
			name:         "a stage the agent fails records one failure",
			agent1Binary: "false",
			pipeline:     "one-stage",
			wantStatus:   ticket.StatusPaused,
			wantOutcomes: map[string]int64{"failure": 1},
			wantActions:  map[string]int64{"pause": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			d, collect := h.newMetricsDaemon(h.defaultConfig(tt.agent1Binary, "true"))

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)

			h.writeTicket("tst-met.md", h.taskMD("tst-met", "todo", tt.pipeline))
			h.waitForStatus("tst-met.md", tt.wantStatus, 10*time.Second)

			cancel()
			require.NoError(t, <-errCh)

			got := collect()

			runs, ok := got["kontora.stage.runs"]
			require.True(t, ok, "kontora.stage.runs must be exported")
			assert.Equal(t, tt.wantOutcomes, sumByAttr(t, runs, "outcome"))
			assert.Equal(t, map[string]int64{tt.pipeline: total(tt.wantOutcomes)},
				sumByAttr(t, runs, "pipeline"))

			trans, ok := got["kontora.stage.transitions"]
			require.True(t, ok, "kontora.stage.transitions must be exported")
			assert.Equal(t, tt.wantActions, sumByAttr(t, trans, "action"))

			dur, ok := got["kontora.stage.duration"]
			require.True(t, ok, "kontora.stage.duration must be exported")
			hist, ok := dur.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			var samples uint64
			for _, dp := range hist.DataPoints {
				samples += dp.Count
				assert.Equal(t, uint64(0), dp.BucketCounts[len(dp.BucketCounts)-1],
					"a stage run must land in a finite bucket, not +Inf")
				assert.Equal(t, dp.Count, dp.BucketCounts[0],
					"a sub-second test run belongs to the first bucket")
			}
			assert.Equal(t, total(tt.wantOutcomes), int64(samples),
				"one duration sample per stage run")
		})
	}
}

func total(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// TestMetricsSchedulerGaugesAndQueueWait covers the lock-free scheduler state:
// the gauges report the semaphore's own numbers, queue depth returns to zero
// once the queue drains, and every ticket contributes one queue-wait sample.
func TestMetricsSchedulerGaugesAndQueueWait(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.MaxConcurrentAgents = 4
	d, collect := h.newMetricsDaemon(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	const tickets = 3
	for i := range tickets {
		id := "tst-q" + string(rune('a'+i))
		h.writeTicket(id+".md", h.taskMD(id, "todo", "one-stage"))
	}
	for i := range tickets {
		id := "tst-q" + string(rune('a'+i))
		h.waitForStatus(id+".md", ticket.StatusDone, 15*time.Second)
	}

	cancel()
	require.NoError(t, <-errCh)

	got := collect()
	assert.Equal(t, int64(4), gaugeValue(t, got, "kontora.scheduler.capacity"),
		"capacity is cap(d.sem), the configured concurrency")
	assert.Equal(t, int64(0), gaugeValue(t, got, "kontora.scheduler.active"),
		"every slot is released once the runs finish")
	assert.Equal(t, int64(0), gaugeValue(t, got, "kontora.queue.depth"),
		"the queue drains back to empty")

	wait, ok := got["kontora.queue.wait"]
	require.True(t, ok, "kontora.queue.wait must be exported")
	dp := histogramPoint(t, wait)
	assert.GreaterOrEqual(t, dp.Count, uint64(tickets),
		"every ticket the scheduler took contributes one wait sample")
	assert.Empty(t, dp.Attributes.ToSlice(), "queue wait carries no attributes")
}

// TestStartMetricsExportsToTheConfiguredCollector drives the production path
// every other test skips by injecting a provider: the config's own settings
// build the exporter, replace the no-op recorder, and register the gauges.
func TestStartMetricsExportsToTheConfiguredCollector(t *testing.T) {
	bodies := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if r.Header.Get("Content-Encoding") == "gzip" {
			if zr, zErr := gzip.NewReader(bytes.NewReader(body)); zErr == nil {
				if plain, rErr := io.ReadAll(zr); rErr == nil {
					body = plain
				}
				zr.Close()
			}
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.Metrics = config.Metrics{
		Enabled:  new(true),
		Endpoint: srv.URL, // an http:// URL, so the transport is plain
		Interval: config.Duration{Duration: time.Hour},
	}
	d := h.newDaemon(cfg)

	stop := d.startMetrics(context.Background(), cfg)
	d.queueDepth.Store(2)
	d.metrics.StageRun(context.Background(), metrics.StageAttrs{
		Stage: "step1", Agent: "agent1", Pipeline: "one-stage", Outcome: metrics.OutcomeSuccess,
	}, time.Minute)
	// The interval is an hour out, so the flush stop() runs is what pushes.
	stop()

	select {
	case body := <-bodies:
		assert.Contains(t, string(body), "kontora.stage.runs", "the swapped-in recorder must be the exporting one")
		assert.Contains(t, string(body), "kontora.queue.depth", "the gauges must be registered against it")
		assert.Contains(t, string(body), "test-instance", "service.instance.id comes from instance_name")
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon exported nothing")
	}
}

// TestMetricsQueueWaitIncludesTheSlotWait pins where the sample is taken. The
// scheduler pops with the ticket already out of the heap and then parks on the
// semaphore, so a sample taken at the pop would leave out the part of the wait
// an operator cares about most: the queue standing still under saturation.
func TestMetricsQueueWaitIncludesTheSlotWait(t *testing.T) {
	const runTime = 400 * time.Millisecond

	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	// One slot, so the second ticket cannot start until the first is done.
	cfg.MaxConcurrentAgents = 1
	runner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		started := time.Now()
		time.Sleep(runTime)
		return process.Result{ExitCode: 0, StartedAt: started, ExitedAt: time.Now()}, nil
	}
	d, collect := h.newMetricsDaemon(cfg, WithRunner(runner))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	for _, id := range []string{"tst-qwa", "tst-qwb"} {
		h.writeTicket(id+".md", h.taskMD(id, "todo", "one-stage"))
	}
	for _, id := range []string{"tst-qwa", "tst-qwb"} {
		h.waitForStatus(id+".md", ticket.StatusDone, 20*time.Second)
	}

	cancel()
	require.NoError(t, <-errCh)

	dp := histogramPoint(t, collect()["kontora.queue.wait"])
	longest, ok := dp.Max.Value()
	require.True(t, ok, "the histogram must carry a maximum")
	assert.GreaterOrEqual(t, longest, 0.75*runTime.Seconds(),
		"the ticket that waited for the slot must report that wait")
}

// TestMetricsGaugeCallbacksDoNotTakeDaemonLock is the deadlock guard: d.mu is
// held across tmux fork/exec and ticket writes, so a gauge callback that took
// it would stall every export behind them.
func TestMetricsGaugeCallbacksDoNotTakeDaemonLock(t *testing.T) {
	h := newHarness(t)
	cfg := h.defaultConfig("true", "true")
	cfg.MaxConcurrentAgents = 4
	d, collect := h.newMetricsDaemon(cfg)
	// Run is what normally registers the gauges; this test drives the daemon's
	// state directly instead of starting it.
	defer d.startMetrics(context.Background(), cfg)()

	d.sem <- struct{}{}
	d.sem <- struct{}{}
	d.queueDepth.Store(3)

	// Held for the whole collect, the way handleFileChanged holds it across a
	// tmux call.
	d.mu.Lock()
	defer d.mu.Unlock()

	done := make(chan map[string]metricdata.Metrics, 1)
	go func() { done <- collect() }()

	select {
	case got := <-done:
		assert.Equal(t, int64(2), gaugeValue(t, got, "kontora.scheduler.active"))
		assert.Equal(t, int64(4), gaugeValue(t, got, "kontora.scheduler.capacity"))
		assert.Equal(t, int64(3), gaugeValue(t, got, "kontora.queue.depth"))
	case <-time.After(5 * time.Second):
		t.Fatal("collection blocked on d.mu")
	}
}

// TestMaterializeAgentLogsUsage covers the token source: a Claude tape reports
// complete usage, and a pi tape declares its usage partial so the caller
// records nothing rather than a run that looks like it spent zero tokens.
func TestMaterializeAgentLogsUsage(t *testing.T) {
	claudeLine := `{"type":"assistant","message":{"model":"m1","usage":{"input_tokens":100,"output_tokens":20,` +
		`"cache_creation_input_tokens":5,"cache_read_input_tokens":30},"content":[{"type":"text","text":"hi"}]}}`
	piLine := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`

	tests := []struct {
		name         string
		line         string
		pi           bool
		wantUsage    logfmt.Usage
		wantComplete bool
	}{
		{
			name:         "a claude tape reports complete usage",
			line:         claudeLine,
			wantUsage:    logfmt.Usage{Input: 100, Output: 20, CacheCreate: 5, CacheRead: 30},
			wantComplete: true,
		},
		{
			name:         "a pi tape declares its usage partial",
			line:         piLine,
			pi:           true,
			wantComplete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			dir := t.TempDir()

			params := RunnerParams{LogFile: filepath.Join(dir, "stage.log")}
			if tt.pi {
				params.SessionDir = filepath.Join(dir, "sessions")
				require.NoError(t, os.MkdirAll(params.SessionDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(params.SessionDir, "s.jsonl"), []byte(tt.line), 0o644))
			} else {
				projects := filepath.Join(dir, "claude", "projects", "p")
				require.NoError(t, os.MkdirAll(projects, 0o755))
				params.SessionID = "sess-1"
				params.Env = map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(dir, "claude")}
				require.NoError(t, os.WriteFile(filepath.Join(projects, "sess-1.jsonl"), []byte(tt.line), 0o644))
			}

			usage, complete := d.materializeAgentLogs(testLogger(t), params, filepath.Join(dir, "events.json"))
			assert.Equal(t, tt.wantComplete, complete)
			if tt.wantComplete {
				assert.Equal(t, tt.wantUsage, usage)
			}
		})
	}
}

// claudeAssistantLine is one assistant record of a Claude session JSONL,
// carrying the usage the tape totals sum.
func claudeAssistantLine(input, output int) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"model":"m1","usage":{"input_tokens":%d,`+
		`"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},`+
		`"content":[{"type":"text","text":"hi"}]}}`+"\n", input, output)
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString(content)
	require.NoError(t, err)
}

// TestMetricsAnnotationRun covers what an annotation run reports. It borrows a
// stage's name and continues that stage's finished session, so it must carry
// its agent and pipeline, must be marked as an annotation rather than counted
// as a run of the stage, and must record only the tokens it spent itself:
// --resume appends to the session JSONL the stage already reported.
func TestMetricsAnnotationRun(t *testing.T) {
	const (
		id        = "tst-anm1"
		sessionID = "c3d4e5f6-0000-4000-8000-000000000009"
	)
	h := newPlannotatorHarness(t)
	claudeDir := t.TempDir()
	// reworkAgent falls back to the default agent, so that is the one the
	// annotation run resumes.
	h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
	h.cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeDir}

	h.seedReviewWorktree(id)
	wtPath := filepath.Join(h.wtDir, h.repoName, id)

	// claudeSessionFiles globs by the worktree path, so the session file has to
	// sit under the directory derived from it.
	sessionPath := filepath.Join(claudeDir, "projects",
		strings.ReplaceAll(strings.ReplaceAll(wtPath, "/", "-"), ".", "-"), sessionID+".jsonl")
	// What the stage spent and already reported.
	appendFile(t, sessionPath, claudeAssistantLine(100, 20))

	rec := resumeRecord{
		SessionID: sessionID,
		Stage:     "step2",
		Agent:     agentKindClaude,
		Worktree:  wtPath,
		Instance:  "test-instance",
		StartedAt: time.Now().Add(-time.Minute),
	}
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	recPath := completedRecordPath(h.cfg, id, "step2")
	require.NoError(t, os.MkdirAll(filepath.Dir(recPath), 0o755))
	require.NoError(t, os.WriteFile(recPath, data, 0o644))

	mp, collect := h.manualMetrics()
	// The resumed agent appends to the same file, which is what makes the tape
	// totals cover both runs.
	runner := func(_ context.Context, _ RunnerParams) (process.Result, error) {
		appendFile(t, sessionPath, claudeAssistantLine(40, 7))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}
	d := h.newAnnotationDaemon(runner, WithMeterProvider(mp))

	_, stop := startAnnotationDaemon(t, h, d, id,
		h.annotationTicketMD(id, "human_review", "branch: kontora/"+id+"\n"))

	require.NoError(t, d.StartPlannotatorAnnotate(id))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)
	h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

	got := h.waitForAnnotationRuns(id, 1, ticket.StatusHumanReview)
	require.Len(t, got.History, 1)
	require.True(t, got.History[0].SessionReused, "the run must have resumed the stage's session")
	stop()

	m := collect()

	tokens, ok := m["kontora.agent.tokens"]
	require.True(t, ok, "kontora.agent.tokens must be exported")
	assert.Equal(t, map[string]int64{"input": 40, "output": 7, "cache_create": 0, "cache_read": 0},
		sumByAttr(t, tokens, "kind"),
		"only what this invocation added to the session, not the session's totals")

	runs, ok := m["kontora.stage.runs"]
	require.True(t, ok, "kontora.stage.runs must be exported")
	assert.Equal(t, map[string]int64{"agent1": 1}, sumByAttr(t, runs, "agent"))
	assert.Equal(t, map[string]int64{"two-stage": 1}, sumByAttr(t, runs, "pipeline"))
	assert.Equal(t, map[string]int64{"true": 1}, sumByAttr(t, runs, "annotation"),
		"the stage itself never ran")
}

// TestMetricsCrashRecoveryResumeReportsTheWholeSession is the other half of the
// resume rule. An annotation run subtracts what the session already held, but
// the invocation a crash-recovery record points at died before anything could
// report its tokens, so the run that continues it reports the file's totals.
func TestMetricsCrashRecoveryResumeReportsTheWholeSession(t *testing.T) {
	reader := sdkmetric.NewManualReader()

	var claudeDir string
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		appendFile(t, filepath.Join(claudeDir, "projects", "-worktree-"+resumeTicketID,
			p.SessionID+".jsonl"), claudeAssistantLine(40, 7))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}
	rd := newResumeDaemon(t, "claude",
		func(ctx context.Context, _ int, p RunnerParams) (process.Result, error) { return runner(ctx, p) },
		WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	claudeDir = rd.claudeDir

	rd.plantRecord(t, agentKindClaude, nil)
	// What the interrupted invocation spent before the daemon died.
	sessionPath := filepath.Join(claudeDir, "projects", "-worktree-"+resumeTicketID,
		resumeTestSessionID+".jsonl")
	require.NoError(t, os.WriteFile(sessionPath, []byte(claudeAssistantLine(100, 20)), 0o644))

	rd.run(t, ticket.StatusDone)

	tokens, ok := collectMetrics(t, reader)["kontora.agent.tokens"]
	require.True(t, ok, "kontora.agent.tokens must be exported")
	assert.Equal(t, map[string]int64{"input": 140, "output": 27, "cache_create": 0, "cache_read": 0},
		sumByAttr(t, tokens, "kind"),
		"the interrupted invocation's spend was never reported, so this run reports it")
}

// TestMetricsAgentErrorsRecordTheDetectionLayer covers a failure hidden behind
// a clean exit code: the error is counted under the layer that caught it, and
// the run is a failure.
func TestMetricsAgentErrorsRecordTheDetectionLayer(t *testing.T) {
	h := newHarness(t)
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		require.NoError(t, os.MkdirAll(filepath.Dir(p.LogFile), 0o755))
		require.NoError(t, os.WriteFile(p.LogFile, []byte("working...\nError: quota exceeded for today\n"), 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	cfg := h.defaultConfig("some-agent", "some-agent")
	cfg.Agents["agent1"] = config.Agent{Binary: "some-agent", FailurePatterns: []string{"(?i)quota exceeded"}}
	d, collect := h.newMetricsDaemon(cfg, WithRunner(runner))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	h.writeTicket("tst-aerr.md", h.taskMD("tst-aerr", "todo", "one-stage"))
	h.waitForStatus("tst-aerr.md", ticket.StatusPaused, 10*time.Second)

	cancel()
	require.NoError(t, <-errCh)

	got := collect()
	agentErrs, ok := got["kontora.agent.errors"]
	require.True(t, ok, "kontora.agent.errors must be exported")
	assert.Equal(t, map[string]int64{metrics.ErrorKindFailurePattern: 1}, sumByAttr(t, agentErrs, "kind"))
	assert.Equal(t, map[string]int64{"agent1": 1}, sumByAttr(t, agentErrs, "agent"))
	assert.Equal(t, map[string]int64{"step1": 1}, sumByAttr(t, agentErrs, "stage"))

	assert.Equal(t, map[string]int64{"failure": 1}, sumByAttr(t, got["kontora.stage.runs"], "outcome"),
		"a run paused by a detected error is a failed run")
}

// TestMetricsReworkOutcomes covers the built-in rework stage's own record,
// which recordStageRun never sees. A run only counts as a success once it has
// reached the agent and come back clean.
func TestMetricsReworkOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		runnerErr error
		exitCode  int
		cancel    bool
		want      string
	}{
		{name: "a clean exit is a success", want: metrics.OutcomeSuccess},
		{name: "a non-zero exit is a failure", exitCode: 1, want: metrics.OutcomeFailure},
		{name: "a runner error is a failure", runnerErr: errors.New("tmux died"), want: metrics.OutcomeFailure},
		{name: "a cancelled ticket is neither", cancel: true, want: metrics.OutcomeCancelled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			d, collect := h.newMetricsDaemon(h.cfg)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			d.recordReworkRun(ctx, "agent1", "two-stage",
				process.Result{ExitCode: tt.exitCode}, tt.runnerErr, 30*time.Second)

			got := collect()
			runs, ok := got["kontora.stage.runs"]
			require.True(t, ok, "kontora.stage.runs must be exported")
			assert.Equal(t, map[string]int64{tt.want: 1}, sumByAttr(t, runs, "outcome"))
			assert.Equal(t, map[string]int64{config.ReworkStageName: 1}, sumByAttr(t, runs, "stage"))
			assert.Equal(t, map[string]int64{"false": 1}, sumByAttr(t, runs, "annotation"))
			assert.Equal(t, uint64(1), histogramPoint(t, got["kontora.stage.duration"]).Count)
		})
	}
}

// TestMetricsReworkBinaryLookupFailure covers the rework stage broken at the
// config level: spawnAgentRun records a failure for the same case, so the
// built-in stage must not stay silent about it.
func TestMetricsReworkBinaryLookupFailure(t *testing.T) {
	const id = "tst-rwm1"
	h := newPlannotatorHarness(t)
	mp, collect := h.manualMetrics()
	d := h.newAnnotationDaemon(DirectRunner, WithMeterProvider(mp),
		WithAgentLookup(func(string) (string, error) {
			return "", errors.New("executable file not found in $PATH")
		}))

	filePath := h.writeTicket(id+".md", h.reviewTaskMD(id, "human_review", "kontora/"+id))
	tk, err := ticket.ParseFile(filePath)
	require.NoError(t, err)

	ctx := context.Background()
	d.runReworkStage(ctx, ctx, h.cfg, testLogger(t), id, tk, filePath)

	paused, err := ticket.ParseFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, ticket.StatusPaused, paused.Status)

	got := collect()
	runs, ok := got["kontora.stage.runs"]
	require.True(t, ok, "a rework stage that never reached its agent must still be counted")
	assert.Equal(t, map[string]int64{config.ReworkStageName: 1}, sumByAttr(t, runs, "stage"))
	assert.Equal(t, map[string]int64{"failure": 1}, sumByAttr(t, runs, "outcome"))
}

// TestUsageSince covers the arithmetic behind the resumed-session delta,
// including the clamp that keeps a monotonic counter from going backwards.
func TestUsageSince(t *testing.T) {
	tests := []struct {
		name  string
		prior logfmt.Usage
		total logfmt.Usage
		want  logfmt.Usage
	}{
		{
			name:  "a fresh session reports its totals",
			total: logfmt.Usage{Input: 100, Output: 20, CacheCreate: 5, CacheRead: 30},
			want:  logfmt.Usage{Input: 100, Output: 20, CacheCreate: 5, CacheRead: 30},
		},
		{
			name:  "a resumed session reports what it added",
			prior: logfmt.Usage{Input: 100, Output: 20, CacheCreate: 5, CacheRead: 30},
			total: logfmt.Usage{Input: 140, Output: 27, CacheCreate: 5, CacheRead: 80},
			want:  logfmt.Usage{Input: 40, Output: 7, CacheRead: 50},
		},
		{
			name:  "a shrunken session reports nothing rather than a negative",
			prior: logfmt.Usage{Input: 100, Output: 20},
			total: logfmt.Usage{Input: 10, Output: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, usageSince(tt.prior, tt.total))
		})
	}
}

// TestMetricsTokensSkipPartialUsage checks the recording gate itself: only a
// tape whose usage is complete produces token measurements.
func TestMetricsTokensSkipPartialUsage(t *testing.T) {
	h := newHarness(t)
	d, collect := h.newMetricsDaemon(h.cfg)
	ctx := context.Background()

	d.recordTokens(ctx, "implement", "claude", logfmt.Usage{Input: 100, Output: 20, CacheCreate: 5, CacheRead: 30}, true)
	d.recordTokens(ctx, "implement", "pi", logfmt.Usage{}, false)

	got := collect()
	tokens, ok := got["kontora.agent.tokens"]
	require.True(t, ok, "kontora.agent.tokens must be exported")
	assert.Equal(t, map[string]int64{
		"input": 100, "output": 20, "cache_create": 5, "cache_read": 30,
	}, sumByAttr(t, tokens, "kind"))
	assert.Equal(t, map[string]int64{"claude": 155}, sumByAttr(t, tokens, "agent"),
		"the partial pi tape contributes no measurement")
}
