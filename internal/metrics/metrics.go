// Package metrics exports the daemon's stage, scheduler and agent measurements
// over OTLP. It is a leaf package: it takes primitives, never internal/config,
// and its Recorder wraps every instrument so no call site touches the OTel API.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// scopeName identifies this instrumentation scope in the exported data.
const scopeName = "github.com/worksonmyai/kontora/internal/metrics"

// DefaultServiceName is the service.name reported when Options leaves it empty.
const DefaultServiceName = "kontora"

// Outcome values for a finished stage run.
const (
	OutcomeSuccess   = "success"
	OutcomeFailure   = "failure"
	OutcomeCancelled = "cancelled"
)

// Agent-error kinds, one per detection layer.
const (
	ErrorKindSessionAPI     = "session_api_error"
	ErrorKindFailurePattern = "failure_pattern"
)

// Token kinds, matching the four categories a session record reports.
const (
	TokenKindInput       = "input"
	TokenKindOutput      = "output"
	TokenKindCacheCreate = "cache_create"
	TokenKindCacheRead   = "cache_read"
)

// stageDurationBuckets are the boundaries of kontora.stage.duration, in
// seconds. A stage run is an agent session: minutes to hours, not
// milliseconds. The SDK default tops out at 10s, which would leave every real
// run in the +Inf bucket and make percentiles meaningless.
var stageDurationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400}

// queueWaitBuckets are the boundaries of kontora.queue.wait, in seconds. A
// ticket leaves the queue as soon as a semaphore slot frees, so the useful
// range starts far below a stage run's.
var queueWaitBuckets = []float64{0.1, 1, 5, 30, 60, 300, 1800, 3600}

// Options are the primitives the daemon resolves from its config.
type Options struct {
	Enabled  bool
	Endpoint string
	Headers  map[string]string
	Interval time.Duration
	// Insecure sends over plain HTTP. An Endpoint that states its own scheme
	// overrides this, the same way it does inside the exporter's env handling.
	Insecure bool
	// ServiceName defaults to DefaultServiceName when empty.
	ServiceName string
	Version     string
	// Instance becomes service.instance.id. It is what tells two daemons
	// sharing one collector apart.
	Instance string
}

// StageAttrs describe one finished stage run.
type StageAttrs struct {
	Stage    string
	Agent    string
	Pipeline string
	Outcome  string
	ExitCode int
	// Annotation marks a run that borrowed the stage's name to answer review
	// annotations instead of doing the stage's work. Without it those runs
	// would be counted as runs of the stage and mixed into its durations.
	Annotation bool
}

// TokenUsage is one invocation's token spend, by category.
type TokenUsage struct {
	Input       int64
	Output      int64
	CacheCreate int64
	CacheRead   int64
}

// Recorder holds the instruments and exposes one typed method per measurement.
// Every method tolerates a nil receiver, so a caller that could not build one
// records nothing instead of panicking.
type Recorder struct {
	meter metric.Meter

	stageRuns        metric.Int64Counter
	stageDuration    metric.Float64Histogram
	stageTransitions metric.Int64Counter
	agentErrors      metric.Int64Counter
	agentTokens      metric.Int64Counter
	queueWait        metric.Float64Histogram
	notifications    metric.Int64Counter

	schedulerActive   metric.Int64ObservableGauge
	schedulerCapacity metric.Int64ObservableGauge
	queueDepth        metric.Int64ObservableGauge
}

// New builds a Recorder and the function that flushes and stops it. When
// opts.Enabled is false it returns a Recorder over noop.NewMeterProvider() and
// a shutdown that does nothing, so no call site needs an enabled check and no
// exporter, connection or real instrument is created.
//
// The provider stays local: otel.SetMeterProvider is deliberately not called,
// so nothing has to reset a process global between tests.
func New(ctx context.Context, opts Options) (*Recorder, func(context.Context) error, error) {
	noShutdown := func(context.Context) error { return nil }
	if !opts.Enabled {
		rec, err := NewWithProvider(noop.NewMeterProvider())
		return rec, noShutdown, err
	}

	exp, err := otlpmetrichttp.New(ctx, exporterOptions(opts)...)
	if err != nil {
		return nil, noShutdown, fmt.Errorf("creating OTLP metric exporter: %w", err)
	}

	res, err := newResource(ctx, opts)
	if err != nil {
		// No provider owns the exporter yet, so nothing else would ever stop it.
		return nil, noShutdown, errors.Join(err, exp.Shutdown(ctx))
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(opts.Interval))),
	)
	rec, err := NewWithProvider(mp)
	if err != nil {
		// The provider owns the exporter's connection, so it must be stopped
		// even though no Recorder came out of it.
		return nil, noShutdown, errors.Join(err, mp.Shutdown(ctx))
	}
	return rec, mp.Shutdown, nil
}

// NewWithProvider builds a Recorder over a caller-supplied provider. Tests use
// it with an sdkmetric.ManualReader.
func NewWithProvider(mp metric.MeterProvider) (*Recorder, error) {
	m := mp.Meter(scopeName)
	r := &Recorder{meter: m}

	var err error
	join := func(e error) {
		err = errors.Join(err, e)
	}

	r.stageRuns, err = m.Int64Counter("kontora.stage.runs",
		metric.WithDescription("Stage runs, counted once per run however many agent invocations it took"),
		metric.WithUnit("{run}"))
	if err != nil {
		return nil, fmt.Errorf("creating instruments: %w", err)
	}

	var e error
	r.stageDuration, e = m.Float64Histogram("kontora.stage.duration",
		metric.WithDescription("Wall time of a stage run"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(stageDurationBuckets...))
	join(e)

	r.stageTransitions, e = m.Int64Counter("kontora.stage.transitions",
		metric.WithDescription("Pipeline actions evaluated after an agent exit"),
		metric.WithUnit("{transition}"))
	join(e)

	r.agentErrors, e = m.Int64Counter("kontora.agent.errors",
		metric.WithDescription("Agent failures detected behind a clean exit code"),
		metric.WithUnit("{error}"))
	join(e)

	r.agentTokens, e = m.Int64Counter("kontora.agent.tokens",
		metric.WithDescription("Tokens an agent invocation spent, by category"),
		metric.WithUnit("{token}"))
	join(e)

	r.notifications, e = m.Int64Counter("kontora.notifications.sent",
		metric.WithDescription("Ticket status notifications, by channel and outcome"),
		metric.WithUnit("{notification}"))
	join(e)

	r.queueWait, e = m.Float64Histogram("kontora.queue.wait",
		metric.WithDescription("Time from a ticket being enqueued to its run starting"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(queueWaitBuckets...))
	join(e)

	r.schedulerActive, e = m.Int64ObservableGauge("kontora.scheduler.active",
		metric.WithDescription("Concurrency slots currently held"),
		metric.WithUnit("{agent}"))
	join(e)

	r.schedulerCapacity, e = m.Int64ObservableGauge("kontora.scheduler.capacity",
		metric.WithDescription("Concurrency slots configured"),
		metric.WithUnit("{agent}"))
	join(e)

	r.queueDepth, e = m.Int64ObservableGauge("kontora.queue.depth",
		metric.WithDescription("Tickets waiting in the scheduler queue"),
		metric.WithUnit("{ticket}"))
	join(e)

	if err != nil {
		return nil, fmt.Errorf("creating instruments: %w", err)
	}
	return r, nil
}

// StageRun records one finished stage run: one count and one duration sample,
// whatever the run took to get there.
func (r *Recorder) StageRun(ctx context.Context, a StageAttrs, d time.Duration) {
	if r == nil {
		return
	}
	base := []attribute.KeyValue{
		attribute.String("stage", a.Stage),
		attribute.String("agent", a.Agent),
		attribute.String("pipeline", a.Pipeline),
		attribute.String("outcome", a.Outcome),
		attribute.Bool("annotation", a.Annotation),
	}
	// exit_code rides on the count only. On the histogram it would multiply
	// every bucket set by the codes seen without telling anyone more than
	// outcome already does.
	runAttrs := append(append([]attribute.KeyValue{}, base...),
		attribute.String("exit_code", strconv.Itoa(a.ExitCode)))

	r.stageRuns.Add(ctx, 1, metric.WithAttributes(runAttrs...))
	r.stageDuration.Record(ctx, d.Seconds(), metric.WithAttributes(base...))
}

// Transition records one evaluated pipeline action.
func (r *Recorder) Transition(ctx context.Context, stage, action string) {
	if r == nil {
		return
	}
	r.stageTransitions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("stage", stage),
		attribute.String("action", action),
	))
}

// AgentError records a failure detected behind a clean exit code. kind names
// the layer that matched: ErrorKindSessionAPI or ErrorKindFailurePattern.
func (r *Recorder) AgentError(ctx context.Context, stage, agent, kind string) {
	if r == nil {
		return
	}
	r.agentErrors.Add(ctx, 1, metric.WithAttributes(
		attribute.String("stage", stage),
		attribute.String("agent", agent),
		attribute.String("kind", kind),
	))
}

// Tokens records one invocation's spend as four measurements, one per
// category. Callers must skip usage an agent reports as partial: a zero from a
// format that does not count tokens is indistinguishable from a run that spent
// none.
func (r *Recorder) Tokens(ctx context.Context, stage, agent string, u TokenUsage) {
	if r == nil {
		return
	}
	for _, kv := range []struct {
		kind  string
		value int64
	}{
		{TokenKindInput, u.Input},
		{TokenKindOutput, u.Output},
		{TokenKindCacheCreate, u.CacheCreate},
		{TokenKindCacheRead, u.CacheRead},
	} {
		r.agentTokens.Add(ctx, kv.value, metric.WithAttributes(
			attribute.String("stage", stage),
			attribute.String("agent", agent),
			attribute.String("kind", kv.kind),
		))
	}
}

// QueueWait records how long a ticket waited between being enqueued and its run
// starting, which includes the wait for a free concurrency slot.
func (r *Recorder) QueueWait(ctx context.Context, d time.Duration) {
	if r == nil {
		return
	}
	r.queueWait.Record(ctx, d.Seconds())
}

// Notification records one finished delivery attempt sequence. result names
// what happened to it: "ok", "failed" or "dropped".
func (r *Recorder) Notification(ctx context.Context, channel, result string) {
	if r == nil {
		return
	}
	r.notifications.Add(ctx, 1, metric.WithAttributes(
		attribute.String("channel", channel),
		attribute.String("result", result),
	))
}

// ObserveScheduler registers the three scheduler gauges against one callback.
//
// The callback runs on the exporter's collect path. Every function passed here
// must read lock-free state only: taking a lock the daemon holds across a
// subprocess or a file write would stall collection behind it.
func (r *Recorder) ObserveScheduler(active, capacity, queued func() int64) error {
	if r == nil {
		return nil
	}
	_, err := r.meter.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			o.ObserveInt64(r.schedulerActive, active())
			o.ObserveInt64(r.schedulerCapacity, capacity())
			o.ObserveInt64(r.queueDepth, queued())
			return nil
		},
		r.schedulerActive, r.schedulerCapacity, r.queueDepth,
	)
	if err != nil {
		return fmt.Errorf("registering scheduler gauges: %w", err)
	}
	return nil
}

// exporterOptions translates Options into exporter settings.
func exporterOptions(opts Options) []otlpmetrichttp.Option {
	var out []otlpmetrichttp.Option

	host, path, insecure := parseEndpoint(opts.Endpoint, opts.Insecure)
	if insecure {
		out = append(out, otlpmetrichttp.WithInsecure())
	}
	if host != "" {
		out = append(out, otlpmetrichttp.WithEndpoint(host))
	}
	if path != "" {
		out = append(out, otlpmetrichttp.WithURLPath(path))
	}
	if len(opts.Headers) > 0 {
		out = append(out, otlpmetrichttp.WithHeaders(opts.Headers))
	}
	return out
}

// parseEndpoint splits the configured endpoint into the host:port to dial, an
// explicit URL path ("" to keep the exporter's default /v1/metrics), and
// whether to talk plain HTTP. An empty endpoint leaves everything to the SDK's
// OTEL_EXPORTER_OTLP_* handling, including the transport: insecure describes
// the configured endpoint, and an explicit option here would win over the
// environment and downgrade an https:// endpoint set there to plaintext.
//
// A stated scheme decides the transport. A stated path is honored as-is, but a
// scheme-only URL keeps the default signal path: otlpmetrichttp.WithEndpointURL
// treats a pathless URL as targeting "/", so `http://collector:4318` would post
// to the root, which a collector serving only /v1/metrics rejects. Keeping the
// default there makes the URL form and the bare host:port form agree.
func parseEndpoint(endpoint string, insecure bool) (host, path string, plain bool) {
	if endpoint == "" {
		return "", "", false
	}
	if !strings.Contains(endpoint, "://") {
		return endpoint, "", insecure
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		// Validation rejects a malformed endpoint upstream; fall back to
		// treating it as an address rather than dropping it silently.
		return endpoint, "", insecure
	}
	path = u.Path
	if path == "/" {
		path = ""
	}
	return u.Host, path, u.Scheme != "https"
}

// newResource describes this daemon to the collector. resource.New reports
// ErrPartialResource and ErrSchemaURLConflict alongside a resource that is
// still usable, so only other errors are fatal.
func newResource(ctx context.Context, opts Options) (*resource.Resource, error) {
	name := opts.ServiceName
	if name == "" {
		name = DefaultServiceName
	}
	attrs := []attribute.KeyValue{semconv.ServiceName(name)}
	if opts.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(opts.Version))
	}
	if opts.Instance != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(opts.Instance))
	}

	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(attrs...),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) && !errors.Is(err, resource.ErrSchemaURLConflict) {
		return nil, fmt.Errorf("building resource: %w", err)
	}
	return res, nil
}
