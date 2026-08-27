package metrics

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestRecorder returns a Recorder writing into a ManualReader, plus a
// collect function returning the current export keyed by metric name.
func newTestRecorder(t *testing.T) (*Recorder, func() map[string]metricdata.Metrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	rec, err := NewWithProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	require.NoError(t, err)

	return rec, func() map[string]metricdata.Metrics {
		t.Helper()
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		out := map[string]metricdata.Metrics{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				out[m.Name] = m
			}
		}
		return out
	}
}

// bucketOf returns the index in BucketCounts that a value falls into.
func bucketOf(bounds []float64, v float64) int {
	for i, b := range bounds {
		if v <= b {
			return i
		}
	}
	return len(bounds)
}

func attrMap(t *testing.T, m metricdata.Metrics) map[string]string {
	t.Helper()
	out := map[string]string{}
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		require.Len(t, data.DataPoints, 1)
		for _, kv := range data.DataPoints[0].Attributes.ToSlice() {
			out[string(kv.Key)] = kv.Value.String()
		}
	case metricdata.Histogram[float64]:
		require.Len(t, data.DataPoints, 1)
		for _, kv := range data.DataPoints[0].Attributes.ToSlice() {
			out[string(kv.Key)] = kv.Value.String()
		}
	default:
		t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
	}
	return out
}

func TestDisabledRecorderRecordsNothing(t *testing.T) {
	rec, shutdown, err := New(context.Background(), Options{Enabled: false, Endpoint: "localhost:4318"})
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.NotNil(t, shutdown)

	// Every call site must be safe without an enabled check.
	ctx := context.Background()
	rec.StageRun(ctx, StageAttrs{Stage: "implement", Outcome: OutcomeSuccess}, time.Minute)
	rec.Transition(ctx, "implement", "advance")
	rec.AgentError(ctx, "implement", "claude", ErrorKindSessionAPI)
	rec.Tokens(ctx, "implement", "claude", TokenUsage{Input: 100})
	rec.QueueWait(ctx, time.Second)
	require.NoError(t, rec.ObserveScheduler(
		func() int64 { return 1 },
		func() int64 { return 2 },
		func() int64 { return 3 },
	))

	assert.NoError(t, shutdown(ctx))
}

// TestNilRecorderRecordsNothing covers the daemon path where a Recorder could
// not be constructed at all: recording must be a no-op, not a panic.
func TestNilRecorderRecordsNothing(t *testing.T) {
	var rec *Recorder
	ctx := context.Background()

	assert.NotPanics(t, func() {
		rec.StageRun(ctx, StageAttrs{Stage: "implement"}, time.Minute)
		rec.Transition(ctx, "implement", "advance")
		rec.AgentError(ctx, "implement", "claude", ErrorKindFailurePattern)
		rec.Tokens(ctx, "implement", "claude", TokenUsage{Input: 1})
		rec.QueueWait(ctx, time.Second)
	})
	assert.NoError(t, rec.ObserveScheduler(nil, nil, nil))
}

func TestStageRunCountsOnceWithAgentScaleBuckets(t *testing.T) {
	rec, collect := newTestRecorder(t)

	// The spec's resumed-stage case: one run of 4860s (12:00 to 13:21) that
	// took two agent invocations.
	rec.StageRun(context.Background(), StageAttrs{
		Stage: "implement", Agent: "claude", Pipeline: "default",
		Outcome: OutcomeSuccess, ExitCode: 0,
	}, 4860*time.Second)

	got := collect()

	runs, ok := got["kontora.stage.runs"]
	require.True(t, ok, "kontora.stage.runs must be exported")
	assert.Equal(t, "{run}", runs.Unit)
	sum, ok := runs.Data.(metricdata.Sum[int64])
	require.True(t, ok, "stage.runs must be a counter, got %T", runs.Data)
	assert.True(t, sum.IsMonotonic)
	require.Len(t, sum.DataPoints, 1, "one stage run must produce exactly one data point")
	assert.Equal(t, int64(1), sum.DataPoints[0].Value)
	assert.Equal(t, map[string]string{
		"stage": "implement", "agent": "claude", "pipeline": "default",
		"outcome": "success", "exit_code": "0", "annotation": "false",
	}, attrMap(t, runs))

	dur, ok := got["kontora.stage.duration"]
	require.True(t, ok, "kontora.stage.duration must be exported")
	assert.Equal(t, "s", dur.Unit)
	hist, ok := dur.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "stage.duration must be a histogram, got %T", dur.Data)
	require.Len(t, hist.DataPoints, 1)
	dp := hist.DataPoints[0]
	assert.Equal(t, uint64(1), dp.Count, "one sample spans the whole run")
	assert.InDelta(t, 4860.0, dp.Sum, 0.001)
	assert.Equal(t, stageDurationBuckets, dp.Bounds)
	assert.Equal(t, map[string]string{
		"stage": "implement", "agent": "claude", "pipeline": "default",
		"outcome": "success", "annotation": "false",
	}, attrMap(t, dur), "exit_code is deliberately kept off the histogram")

	idx := bucketOf(stageDurationBuckets, 4860)
	assert.Less(t, idx, len(stageDurationBuckets), "an 81-minute run must not land in +Inf")
	assert.Equal(t, 7200.0, stageDurationBuckets[idx], "4860s belongs to the 7200s bucket")
	assert.Equal(t, uint64(1), dp.BucketCounts[idx])
	assert.Equal(t, uint64(0), dp.BucketCounts[bucketOf(stageDurationBuckets, 60)],
		"a 60s run would land in a different bucket, so percentiles stay useful")
}

func TestStageRunOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		outcome string
		exit    int
	}{
		{"success", OutcomeSuccess, 0},
		{"runner failure", OutcomeFailure, 1},
		{"cancelled", OutcomeCancelled, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, collect := newTestRecorder(t)
			rec.StageRun(context.Background(), StageAttrs{
				Stage: "rework", Agent: "claude", Pipeline: "default",
				Outcome: tt.outcome, ExitCode: tt.exit,
			}, 30*time.Second)

			got := collect()
			a := attrMap(t, got["kontora.stage.runs"])
			assert.Equal(t, tt.outcome, a["outcome"])
			assert.Equal(t, "rework", a["stage"])

			hist := got["kontora.stage.duration"].Data.(metricdata.Histogram[float64])
			require.Len(t, hist.DataPoints, 1)
			assert.Equal(t, uint64(1), hist.DataPoints[0].Count,
				"a failed or cancelled attempt still records one duration sample")
		})
	}
}

// TestStageRunSeparatesAnnotationRuns covers the attribute that keeps a ticket
// rewrite out of the stage's own numbers: both runs report the same stage, and
// only the attribute tells them apart.
func TestStageRunSeparatesAnnotationRuns(t *testing.T) {
	rec, collect := newTestRecorder(t)
	ctx := context.Background()

	attrs := StageAttrs{Stage: "code", Agent: "claude", Pipeline: "default", Outcome: OutcomeSuccess}
	rec.StageRun(ctx, attrs, time.Minute)
	attrs.Annotation = true
	rec.StageRun(ctx, attrs, 30*time.Second)

	got := collect()

	byAnnotation := map[string]int64{}
	for _, dp := range got["kontora.stage.runs"].Data.(metricdata.Sum[int64]).DataPoints {
		for _, kv := range dp.Attributes.ToSlice() {
			if kv.Key == "annotation" {
				byAnnotation[kv.Value.String()] = dp.Value
			}
		}
	}
	assert.Equal(t, map[string]int64{"false": 1, "true": 1}, byAnnotation,
		"an annotation run must not be counted as a run of the stage it borrows")

	hist := got["kontora.stage.duration"].Data.(metricdata.Histogram[float64])
	assert.Len(t, hist.DataPoints, 2, "the durations stay in separate series too")
}

// TestExecutionEventMetrics is the spec's combined scenario: one advance, both
// agent-error layers, and a complete token usage.
func TestExecutionEventMetrics(t *testing.T) {
	rec, collect := newTestRecorder(t)
	ctx := context.Background()

	rec.Transition(ctx, "implement", "advance")
	rec.AgentError(ctx, "implement", "claude", ErrorKindSessionAPI)
	rec.AgentError(ctx, "implement", "claude", ErrorKindFailurePattern)
	rec.Tokens(ctx, "implement", "claude", TokenUsage{
		Input: 100, Output: 20, CacheCreate: 5, CacheRead: 30,
	})

	got := collect()

	trans := got["kontora.stage.transitions"]
	assert.Equal(t, "{transition}", trans.Unit)
	transSum := trans.Data.(metricdata.Sum[int64])
	require.Len(t, transSum.DataPoints, 1)
	assert.Equal(t, int64(1), transSum.DataPoints[0].Value)
	assert.Equal(t, map[string]string{"stage": "implement", "action": "advance"}, attrMap(t, trans))

	errSum := got["kontora.agent.errors"].Data.(metricdata.Sum[int64])
	assert.Equal(t, "{error}", got["kontora.agent.errors"].Unit)
	kinds := map[string]int64{}
	for _, dp := range errSum.DataPoints {
		for _, kv := range dp.Attributes.ToSlice() {
			if kv.Key == "kind" {
				kinds[kv.Value.String()] = dp.Value
			}
		}
	}
	assert.Equal(t, map[string]int64{
		ErrorKindSessionAPI: 1, ErrorKindFailurePattern: 1,
	}, kinds, "each detection layer is counted under its own kind")

	tokSum := got["kontora.agent.tokens"].Data.(metricdata.Sum[int64])
	assert.Equal(t, "{token}", got["kontora.agent.tokens"].Unit)
	byKind := map[string]int64{}
	for _, dp := range tokSum.DataPoints {
		for _, kv := range dp.Attributes.ToSlice() {
			if kv.Key == "kind" {
				byKind[kv.Value.String()] = dp.Value
			}
		}
	}
	assert.Equal(t, map[string]int64{
		TokenKindInput: 100, TokenKindOutput: 20, TokenKindCacheCreate: 5, TokenKindCacheRead: 30,
	}, byKind)
}

func TestQueueWaitUsesItsOwnBuckets(t *testing.T) {
	rec, collect := newTestRecorder(t)
	rec.QueueWait(context.Background(), 5*time.Second)

	m := collect()["kontora.queue.wait"]
	assert.Equal(t, "s", m.Unit)
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "queue.wait must be a histogram, got %T", m.Data)
	require.Len(t, hist.DataPoints, 1)
	dp := hist.DataPoints[0]
	assert.Equal(t, uint64(1), dp.Count)
	assert.InDelta(t, 5.0, dp.Sum, 0.001)
	assert.Equal(t, queueWaitBuckets, dp.Bounds)
	assert.Empty(t, dp.Attributes.ToSlice(), "queue wait carries no attributes")

	idx := bucketOf(queueWaitBuckets, 5)
	assert.Equal(t, uint64(1), dp.BucketCounts[idx])
	assert.Equal(t, 5.0, queueWaitBuckets[idx])
}

func TestObserveSchedulerReadsCallbacksOnCollect(t *testing.T) {
	rec, collect := newTestRecorder(t)

	active, capacity, queued := int64(2), int64(4), int64(3)
	require.NoError(t, rec.ObserveScheduler(
		func() int64 { return active },
		func() int64 { return capacity },
		func() int64 { return queued },
	))

	assertGauge := func(t *testing.T, got map[string]metricdata.Metrics, name, unit string, want int64) {
		t.Helper()
		m, ok := got[name]
		require.True(t, ok, "%s must be exported", name)
		assert.Equal(t, unit, m.Unit)
		g, ok := m.Data.(metricdata.Gauge[int64])
		require.True(t, ok, "%s must be a gauge, got %T", name, m.Data)
		require.Len(t, g.DataPoints, 1)
		assert.Equal(t, want, g.DataPoints[0].Value)
	}

	got := collect()
	assertGauge(t, got, "kontora.scheduler.active", "{agent}", 2)
	assertGauge(t, got, "kontora.scheduler.capacity", "{agent}", 4)
	assertGauge(t, got, "kontora.queue.depth", "{ticket}", 3)

	// The callbacks are read at collect time, not at registration.
	active, queued = 0, 0
	got = collect()
	assertGauge(t, got, "kontora.scheduler.active", "{agent}", 0)
	assertGauge(t, got, "kontora.queue.depth", "{ticket}", 0)
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		insecure    bool
		wantHost    string
		wantPath    string
		wantPlain   bool
		explanation string
	}{
		{
			name: "empty leaves everything to the environment",
		},
		{
			name: "empty with insecure still leaves the transport to the environment",
			// Otherwise WithInsecure() would be passed with no endpoint of its
			// own and downgrade an https:// OTEL_EXPORTER_OTLP_ENDPOINT.
			endpoint: "", insecure: true,
		},
		{
			name: "bare host:port keeps the default path", endpoint: "collector:4318",
			wantHost: "collector:4318",
		},
		{
			name:     "bare host:port takes insecure from the config",
			endpoint: "collector:4318", insecure: true,
			wantHost: "collector:4318", wantPlain: true,
		},
		{
			name:     "an http url is plain and keeps the default path",
			endpoint: "http://collector:4318",
			wantHost: "collector:4318", wantPlain: true,
			explanation: "a pathless URL must not target /, or a collector serving /v1/metrics rejects it",
		},
		{
			name:     "an https url is TLS even when insecure is set",
			endpoint: "https://collector:4318", insecure: true,
			wantHost: "collector:4318",
		},
		{
			name:     "a trailing slash still means the default path",
			endpoint: "http://collector:4318/",
			wantHost: "collector:4318", wantPlain: true,
		},
		{
			name: "an explicit path is honored", endpoint: "https://collector.example/otlp/v1/metrics",
			wantHost: "collector.example", wantPath: "/otlp/v1/metrics",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, path, plain := parseEndpoint(tt.endpoint, tt.insecure)
			assert.Equal(t, tt.wantHost, host)
			assert.Equal(t, tt.wantPath, path, tt.explanation)
			assert.Equal(t, tt.wantPlain, plain)
		})
	}
}

func TestExporterOptionsEndpointForms(t *testing.T) {
	// The exporter's config is unexported, so this asserts only that every
	// endpoint form the config accepts builds an exporter without error.
	// Every address is a literal, so the shutdown flush cannot wait on DNS.
	tests := []struct {
		name     string
		endpoint string
		insecure bool
	}{
		{"empty endpoint defers to the environment", "", false},
		{"bare host:port with insecure", "127.0.0.1:4318", true},
		{"bare host:port without insecure", "127.0.0.1:4318", false},
		{"explicit http url", "http://127.0.0.1:4318", false},
		{"explicit https url with a path", "https://127.0.0.1:4318/otlp", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, shutdown, err := New(context.Background(), Options{
				Enabled:  true,
				Endpoint: tt.endpoint,
				Insecure: tt.insecure,
				Interval: time.Minute,
				Headers:  map[string]string{"authorization": "Bearer tok"},
				Version:  "test",
				Instance: "test-host",
			})
			require.NoError(t, err)
			require.NotNil(t, rec)
			// Nothing is exported: the periodic reader's first push is a minute
			// out and shutdown's flush fails silently against no collector.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = shutdown(ctx)
		})
	}
}

func TestResourceCarriesServiceIdentity(t *testing.T) {
	tests := []struct {
		name        string
		opts        Options
		wantService string
	}{
		{"defaults to kontora", Options{}, DefaultServiceName},
		{"honors an explicit name", Options{ServiceName: "kontora-staging"}, "kontora-staging"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Instance = "host-a"
			tt.opts.Version = "1.2.3"
			res, err := newResource(context.Background(), tt.opts)
			require.NoError(t, err)

			got := map[string]string{}
			for _, kv := range res.Attributes() {
				got[string(kv.Key)] = kv.Value.String()
			}
			assert.Equal(t, tt.wantService, got["service.name"])
			assert.Equal(t, "1.2.3", got["service.version"])
			assert.Equal(t, "host-a", got["service.instance.id"],
				"the instance name is what tells two daemons on one collector apart")
		})
	}
}

// TestExportReachesCollector drives the real OTLP/HTTP path against a local
// endpoint standing in for a collector: the exporter must POST the recorded
// metric families to /v1/metrics as protobuf.
//
// The body is searched for the metric names as raw bytes rather than decoded.
// Protobuf writes string fields literally, so this proves the families reached
// the wire without making the OTLP proto a direct dependency of this module.
func TestExportReachesCollector(t *testing.T) {
	type request struct {
		path     string
		encoding string
		partial  string
		body     []byte
	}
	got := make(chan request, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, zErr := gzip.NewReader(bytes.NewReader(body))
			if zErr == nil {
				if plain, rErr := io.ReadAll(zr); rErr == nil {
					body = plain
				}
				zr.Close()
			}
		}
		got <- request{
			path:     r.URL.Path,
			encoding: r.Header.Get("Content-Type"),
			partial:  r.Header.Get("Authorization"),
			body:     body,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec, shutdown, err := New(context.Background(), Options{
		Enabled:  true,
		Endpoint: srv.URL, // an http:// URL, so the transport is plain
		Interval: time.Hour,
		Headers:  map[string]string{"authorization": "Bearer tok"},
		Instance: "host-a",
	})
	require.NoError(t, err)

	ctx := context.Background()
	rec.StageRun(ctx, StageAttrs{
		Stage: "implement", Agent: "claude", Pipeline: "default",
		Outcome: OutcomeSuccess, ExitCode: 0,
	}, 90*time.Second)
	rec.Transition(ctx, "implement", "advance")
	rec.AgentError(ctx, "implement", "claude", ErrorKindSessionAPI)
	rec.Tokens(ctx, "implement", "claude", TokenUsage{Input: 100, Output: 20})
	rec.QueueWait(ctx, 5*time.Second)
	rec.Notification(ctx, "tg", "ok")
	require.NoError(t, rec.ObserveScheduler(
		func() int64 { return 2 },
		func() int64 { return 4 },
		func() int64 { return 3 },
	))

	// The interval is an hour out, so shutdown's final flush is what pushes.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, shutdown(shutdownCtx))

	select {
	case req := <-got:
		assert.Equal(t, "/v1/metrics", req.path)
		assert.Equal(t, "application/x-protobuf", req.encoding)
		assert.Equal(t, "Bearer tok", req.partial, "configured headers must reach the collector")
		for _, name := range []string{
			"kontora.stage.runs", "kontora.stage.duration", "kontora.stage.transitions",
			"kontora.agent.errors", "kontora.agent.tokens", "kontora.queue.wait",
			"kontora.notifications.sent",
			"kontora.scheduler.active", "kontora.scheduler.capacity", "kontora.queue.depth",
		} {
			assert.Contains(t, string(req.body), name, "%s must reach the collector", name)
		}
		assert.Contains(t, string(req.body), "host-a", "service.instance.id identifies the daemon")
		assert.Contains(t, string(req.body), DefaultServiceName)
	case <-time.After(10 * time.Second):
		t.Fatal("the exporter sent nothing")
	}
}

func TestNotification(t *testing.T) {
	rec, collect := newTestRecorder(t)
	ctx := context.Background()

	rec.Notification(ctx, "tg", "ok")
	rec.Notification(ctx, "tg", "ok")
	rec.Notification(ctx, "mm", "failed")

	m := collect()["kontora.notifications.sent"]
	assert.Equal(t, "{notification}", m.Unit)

	got := map[string]int64{}
	for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
		channel, _ := dp.Attributes.Value("channel")
		result, _ := dp.Attributes.Value("result")
		got[channel.AsString()+"/"+result.AsString()] = dp.Value
	}
	assert.Equal(t, map[string]int64{"tg/ok": 2, "mm/failed": 1}, got)
}
