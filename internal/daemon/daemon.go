package daemon

import (
	"container/heap"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	charmlog "github.com/charmbracelet/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/hook"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/metrics"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/prompt"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/watcher"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

const defaultPromptTemplate = "Work on this ticket: {{ .Ticket.ID }} — {{ .Ticket.Title }}\n\n{{ .Ticket.Description }}"

// defaultAnnotationPrompt is sent to the run a submitted set of Plannotator
// annotations schedules. A stage carries no tool policy, so the restriction to
// the ticket file can only be stated here.
const defaultAnnotationPrompt = `A reviewer annotated ticket {{ .Ticket.ID }} — {{ .Ticket.Title }} and submitted these notes:

{{ plannotatorAnnotations }}

Change the ticket at {{ .Ticket.FilePath }} so that it satisfies every note. Do not add replies to the notes in the ticket text. Leave the parts the reviewer did not comment on as they are.

Rules for this run:

- Edit only {{ .Ticket.FilePath }}.
- Change the markdown body only. Leave the YAML frontmatter between the --- lines exactly as it is; the daemon owns those fields.
- Do not write code, do not run tests, and do not commit or push anything.
- Do not do the work the ticket describes. This run changes what the ticket asks for, nothing else.`

// defaultResumePrompt is sent instead of the stage prompt when a restart
// interrupted a stage and the daemon reattaches the agent to its own session.
// The stage prompt would tell the agent to begin the stage, which is the one
// thing a resumed run must not do.
const defaultResumePrompt = "Your previous run on ticket {{ .Ticket.ID }} — {{ .Ticket.Title }} was interrupted when the daemon restarted. " +
	"This is the same conversation, and the worktree still holds every change you made.\n\n" +
	"Re-read the ticket and check the worktree to see how far you got, then finish the stage you were working on. " +
	"Do not start it over and do not undo work that is already there."

func (d *Daemon) renderTicketPrompt(cfg *config.Config, tmpl string, t *ticket.Ticket, filePath, wtPath string) (string, error) {
	opts := prompt.Options{
		ReviewsDir: expandTilde(cfg.Plannotator.ReviewsDir),
		Logger:     d.log,
	}
	rendered, err := prompt.RenderWithOptions(tmpl, prompt.Data{
		Ticket: prompt.TicketData{
			ID:          t.ID,
			Title:       t.Title(),
			Description: t.Body,
			FilePath:    filePath,
		},
	}, wtPath, opts)
	if err != nil {
		return "", err
	}
	if rendered == "" {
		return "", nil
	}

	rendered += "\n\n---"
	rendered += fmt.Sprintf("\nTask ID: %s", t.ID)
	rendered += fmt.Sprintf("\nTicket: %s", filePath)
	rendered += fmt.Sprintf("\nWorkspace: %s", wtPath)

	rendered += fmt.Sprintf("\n\nIMPORTANT: When you finish your work, write your results as a note on the ticket. Include all relevant details so they are preserved. Use:\n  kontora note %s \"your results here\"", t.ID)

	return rendered, nil
}

// RunnerFunc runs a command and returns its result. The daemon calls the
// configured runner for every agent spawn. Two implementations are provided:
// DirectRunner (wraps process.Run) and the default tmux-based runner.
type RunnerFunc func(ctx context.Context, p RunnerParams) (process.Result, error)

// RunnerParams contains the parameters passed to a RunnerFunc.
type RunnerParams struct {
	Binary      string
	Args        []string
	Dir         string
	Timeout     time.Duration
	TicketID    string            // ticket ID used as tmux window name
	SessionName string            // tmux session; ignored by DirectRunner
	LogFile     string            // path for agent output log (PTY capture or materialized session log)
	Interactive bool              // use interactive tmux wait-for flow (for Claude agents)
	SessionID   string            // Claude session ID; used for session JSONL materialization after agent exit
	SessionDir  string            // pi session directory; used for session JSONL materialization after agent exit
	Env         map[string]string // environment variables to set for the agent process
	OnReady     func()            // called after the agent process is running (e.g. tmux window created)
	// OnIdle is asked what to do each time an interactive agent signals it is
	// idle. Nil, which is every run but a checkpointing Claude one, finishes the
	// run on the first signal. DirectRunner ignores it: without a terminal there
	// is nothing to type into.
	OnIdle func(context.Context, tmux.IdleEvent) tmux.IdleDecision
	// CompactChannel is the tmux wait-for channel this run's PostCompact hook
	// signals. Empty for every run that does not checkpoint.
	CompactChannel string
}

// DirectRunner wraps process.Run for use without tmux (useful in tests).
func DirectRunner(ctx context.Context, p RunnerParams) (process.Result, error) {
	var logFile *os.File
	if p.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(p.LogFile), 0o755); err == nil {
			f, err := os.OpenFile(p.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				logFile = f
				defer logFile.Close()
			}
		}
	}
	return process.Run(ctx, process.RunParams{
		Binary:  p.Binary,
		Args:    p.Args,
		Dir:     p.Dir,
		Timeout: p.Timeout,
		Stdout:  logFile,
		Stderr:  logFile,
		Env:     envPairs(p.Env),
	})
}

// envPairs renders an environment map as the KEY=VALUE slice a subprocess
// takes.
func envPairs(env map[string]string) []string {
	pairs := make([]string, 0, len(env))
	for k, v := range env {
		pairs = append(pairs, k+"="+v)
	}
	return pairs
}

// tmuxRunner wraps tmux.Run for use as a RunnerFunc.
func tmuxRunner(ctx context.Context, p RunnerParams) (process.Result, error) {
	return tmux.Run(ctx, tmux.RunParams{
		Binary:         p.Binary,
		Args:           p.Args,
		Dir:            p.Dir,
		Timeout:        p.Timeout,
		TicketID:       p.TicketID,
		SessionName:    p.SessionName,
		LogFile:        p.LogFile,
		Interactive:    p.Interactive,
		SessionID:      p.SessionID,
		Env:            p.Env,
		OnReady:        p.OnReady,
		OnIdle:         p.OnIdle,
		CompactChannel: p.CompactChannel,
	})
}

type Option func(*Daemon)

func WithLogger(l *slog.Logger) Option {
	return func(d *Daemon) { d.log = l }
}

func WithDebounce(dur time.Duration) Option {
	return func(d *Daemon) { d.debounce = dur }
}

func WithLockPath(path string) Option {
	return func(d *Daemon) { d.lockPath = path }
}

// WithConfigPath records the path the config was loaded from, so the daemon can
// serve and rewrite the on-disk config via the raw-config API.
func WithConfigPath(path string) Option {
	return func(d *Daemon) { d.configPath = path }
}

// WithConfigOverride registers the caller's command-line overrides. New applies
// fn to the starting config and every reload applies it again, so the running
// config and a freshly loaded one never disagree about a flag.
//
// `kontora start --address` and `--port` set fields that are never written to
// the config file. Without this, a reload would drop them: the daemon would
// keep serving on the flag's address, because the web listener is pinned, while
// warning on every reload that the file says otherwise.
//
// fn runs on each load, so it must be idempotent.
func WithConfigOverride(fn func(*config.Config)) Option {
	return func(d *Daemon) { d.configOverride = fn }
}

// WithRunner overrides the default runner (tmux-based). Use DirectRunner for
// tests that don't need tmux.
func WithRunner(fn RunnerFunc) Option {
	return func(d *Daemon) { d.runner = fn }
}

// WithSkipOrphanCleanup disables the startup cleanup of tmux windows left
// behind by a previous run. Tests use it to avoid forking `tmux list-windows`
// on every daemon start; windows they don't own are already safe, because
// cleanup only kills windows named after a ticket in this daemon's tickets_dir.
func WithSkipOrphanCleanup() Option {
	return func(d *Daemon) { d.skipOrphanCleanup = true }
}

// WithMeterProvider records metrics through mp instead of building an exporter
// from the config. Tests pass a provider over an sdkmetric.ManualReader.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(d *Daemon) { d.meterProvider = mp }
}

// WithVersion sets the build version reported as service.version.
func WithVersion(v string) Option {
	return func(d *Daemon) { d.version = v }
}

// PlannotatorSpawner runs the `plannotator review` subprocess and returns its
// captured stdout. Kept as a separate seam from the generic RunnerFunc because
// the rest of the codebase conflates "runner for an agent" with tmux lifecycle
// hooks that don't apply here.
type PlannotatorSpawner func(ctx context.Context, params PlannotatorParams) (stdout string, err error)

// PlannotatorParams carries inputs for a single plannotator invocation.
type PlannotatorParams struct {
	Binary string
	// Args is the full argument list, starting with the plannotator subcommand.
	// Code review passes "review"; a ticket annotation passes "annotate" and the
	// ticket file.
	Args    []string
	Dir     string
	Env     map[string]string
	Timeout time.Duration
}

// PlannotatorLookup resolves the plannotator binary to an absolute path.
// Returns the resolved path on success (usable directly as argv[0]) so the
// daemon can invoke the binary even when it lives outside the restricted
// PATH the daemon inherits. Tests inject this to skip real filesystem
// lookups when pairing with a fake spawner.
type PlannotatorLookup func(binary string) (string, error)

// AgentLookup resolves an agent binary to an absolute path. The daemon calls
// this before spawning so a missing binary fails with a clear, early error
// (instead of surfacing later as "agent exited too quickly" when the tmux
// shell wrapper can't find the binary on its stripped PATH).
type AgentLookup func(binary string) (string, error)

// WithPlannotatorSpawner overrides the default plannotator subprocess runner.
// Tests use this to return canned stdout without forking a real process.
func WithPlannotatorSpawner(fn PlannotatorSpawner) Option {
	return func(d *Daemon) { d.plannotatorSpawner = fn }
}

// WithPlannotatorLookup overrides the binary-available check. Tests use this
// to bypass exec.LookPath when pairing with a fake spawner.
func WithPlannotatorLookup(fn PlannotatorLookup) Option {
	return func(d *Daemon) { d.plannotatorLookup = fn }
}

// WithAgentLookup overrides the agent binary resolver. Tests inject a
// passthrough so they can use stand-in binary names (e.g. "agent1") without
// touching the filesystem.
func WithAgentLookup(fn AgentLookup) Option {
	return func(d *Daemon) { d.agentLookup = fn }
}

func defaultAgentLookup(binary string) (string, error) {
	return process.LookupBinary(binary)
}

// windowOps is the tmux surface startup cleanup needs. Tests substitute it to
// run cleanup without a tmux server.
type windowOps struct {
	list func(session string) ([]string, error)
	kill func(session, window string) error
}

var defaultWindowOps = windowOps{list: tmux.ListWindows, kill: tmux.KillWindow}

type Daemon struct {
	cfg                 atomic.Pointer[config.Config]
	worktrees           *worktree.Manager
	runner              RunnerFunc
	plannotatorSpawner  PlannotatorSpawner
	plannotatorLookup   PlannotatorLookup
	finalSummarySpawner FinalSummarySpawner
	agentLookup         AgentLookup
	skipOrphanCleanup   bool
	windows             windowOps
	broker              *web.SSEBroker
	svc                 *app.Service

	debounce     time.Duration
	lockPath     string
	configPath   string
	instanceName string
	tmuxSession  string
	version      string
	log          *slog.Logger

	// metrics is never nil: New installs a no-op-backed recorder so no call
	// site needs an enabled check. Run replaces it when the config turns
	// export on. meterProvider, when set by WithMeterProvider, takes
	// precedence over the config and keeps Run from building an exporter.
	metrics       *metrics.Recorder
	meterProvider metric.MeterProvider

	// queueDepth mirrors len(d.queue). It exists so the queue-depth gauge can
	// be read from the exporter's collect path, which must never take d.mu:
	// that lock is held across tmux fork/exec and ticket file writes.
	queueDepth atomic.Int64

	// configOverride re-applies the caller's command-line overrides to every
	// config a reload loads. Set once at construction, read without a lock.
	configOverride func(*config.Config)

	// reloadMu serializes config reloads from the different triggers (SIGHUP,
	// the config file watcher, and the raw-config endpoint).
	reloadMu sync.Mutex

	mu              sync.Mutex
	tickets         map[string]*ticketState
	running         map[string]context.CancelFunc
	liveRuns        map[string]liveRun // ticket ID → the agent invocation in flight
	queued          map[string]bool    // dedupe: prevents same ticket being enqueued twice
	runningBranches map[string]string  // repoPath\x00branch → ticketID holding the branch
	sem             chan struct{}
	plannotator     map[string]context.CancelFunc // in-flight plannotator subprocesses
	// plannotatorDeferred holds the tickets whose pickup runTicket dropped
	// because a Plannotator session was open. releasePlannotator offers each of
	// them again when the session closes.
	plannotatorDeferred map[string]struct{}

	// selfWrites remembers, per ticket path, the bytes the daemon last wrote
	// there; a watcher event whose file content matches is the daemon's own.
	// One run can write a ticket twice inside one debounce interval, so
	// matching is by content.
	selfWrites   map[string]selfWrite
	selfWritesMu sync.Mutex

	// background tracks post-processing that outlives the ticket run that
	// started it, so Run does not return while it is still writing tickets.
	background sync.WaitGroup

	queue     priorityQueue
	queueCond *sync.Cond

	// stats caches the Stats page payload and the sidecars it reads. It keeps
	// its own lock so aggregation never holds d.mu across file I/O.
	stats *statsCache
}

type ticketState struct {
	ticket   *ticket.Ticket
	filePath string
	modTime  time.Time
}

// liveRun addresses the agent invocation a ticket is running right now, so a
// reader can follow the session JSONL the agent is still appending to. params
// is read for its session locators only (SessionID, SessionDir, Env);
// startedAt tells a pi retry's session file apart from the previous attempt's
// in the directory they share.
//
// agent is the agent actually running, which on a pipeline ticket is the stage's
// and not the ticket's.
//
// It is deliberately not a ticketState field: an agent rewrites its own ticket
// mid-run, and every write replaces that struct.
type liveRun struct {
	stage     string
	agent     string
	run       int
	params    RunnerParams
	startedAt time.Time
}

func (d *Daemon) setLiveRun(ticketID string, lr liveRun) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.liveRuns[ticketID] = lr
}

func (d *Daemon) clearLiveRun(ticketID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.liveRuns, ticketID)
}

// newTicketState builds a ticketState and stats filePath once to cache its
// modtime, so buildTicketInfo can set UpdatedAt without a stat per read.
// Construction sites all run after the file has been written/parsed, so the
// cached modtime is fresh.
func newTicketState(t *ticket.Ticket, filePath string) *ticketState {
	ts := &ticketState{ticket: t, filePath: filePath}
	if st, err := os.Stat(filePath); err == nil {
		ts.modTime = st.ModTime()
	}
	return ts
}

func New(cfg *config.Config, opts ...Option) *Daemon {
	d := &Daemon{
		worktrees:           worktree.New(expandTilde(cfg.WorktreesDir)),
		runner:              tmuxRunner,
		plannotatorSpawner:  defaultPlannotatorSpawner,
		plannotatorLookup:   defaultPlannotatorLookup,
		finalSummarySpawner: defaultFinalSummarySpawner,
		agentLookup:         defaultAgentLookup,
		windows:             defaultWindowOps,
		broker:              web.NewSSEBroker(),
		debounce:            time.Second,
		lockPath:            defaultLockPath(),
		instanceName:        cfg.InstanceName,
		tmuxSession:         cfg.TmuxSessionName(),
		log: slog.New(charmlog.NewWithOptions(os.Stderr, charmlog.Options{
			ReportTimestamp: true,
		})),
		tickets:         make(map[string]*ticketState),
		running:         make(map[string]context.CancelFunc),
		liveRuns:        make(map[string]liveRun),
		queued:          make(map[string]bool),
		runningBranches: make(map[string]string),
		sem:             make(chan struct{}, cfg.MaxConcurrentAgents),
		plannotator:     make(map[string]context.CancelFunc),

		plannotatorDeferred: make(map[string]struct{}),
		selfWrites:          make(map[string]selfWrite),
		stats:               newStatsCache(),
	}
	d.cfg.Store(cfg)
	for _, opt := range opts {
		opt(d)
	}
	// Same treatment the starting config gets and every reloaded one gets, in
	// the same order, so pinRestartOnly never compares a flag-set field against
	// the file's own value.
	if d.configOverride != nil {
		d.configOverride(cfg)
	}
	d.queueCond = sync.NewCond(&d.mu)
	// An injected provider wins over the config: a test that passes one must
	// see its own reader, not an exporter Run would build. Everything else
	// starts on the no-op provider, so recording is free and safe before Run
	// decides whether to export.
	mp := metric.MeterProvider(noop.NewMeterProvider())
	if d.meterProvider != nil {
		mp = d.meterProvider
	}
	rec, err := metrics.NewWithProvider(mp)
	if err != nil {
		// Recorder methods tolerate a nil receiver, so the daemon runs on
		// without metrics rather than failing construction over them.
		d.log.Warn("building metric instruments failed, continuing without metrics", "err", err)
	}
	d.metrics = rec
	d.svc = d.buildService()
	return d
}

// config returns the config currently in effect. A reload replaces the whole
// pointer, so a function that reads several fields must take one snapshot and
// use it throughout. Otherwise a concurrent reload can leave it with half the
// old values and half the new ones.
func (d *Daemon) config() *config.Config { return d.cfg.Load() }

func (d *Daemon) buildService() *app.Service {
	repo := store.NewDaemonRepo(store.DaemonRepoCallbacks{
		PathLookup: func(id string) (string, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			ts, ok := d.tickets[id]
			if !ok {
				return "", app.ErrNotFound
			}
			return ts.filePath, nil
		},
		WriteTicket: func(t *ticket.Ticket, path string) error {
			return d.writeTicket(t, path)
		},
		AfterSave: func(id string, st *app.StoredTicket) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.tickets[id] = newTicketState(st.Ticket, st.FilePath)
		},
		ListTickets: func() []*app.StoredTicket {
			d.mu.Lock()
			defer d.mu.Unlock()
			result := make([]*app.StoredTicket, 0, len(d.tickets))
			for _, ts := range d.tickets {
				result = append(result, &app.StoredTicket{Ticket: ts.ticket, FilePath: ts.filePath})
			}
			return result
		},
	})
	rt := &daemonRuntime{d: d}
	return app.New(d.config, repo, rt)
}

// ticketLog returns a logger with the ticket ID pre-set.
func (d *Daemon) ticketLog(ticketID string) *slog.Logger {
	return d.log.With("ticket", ticketID)
}

func (d *Daemon) Run(ctx context.Context) error {
	// Claim SIGHUP first. A signal delivered before signal.Notify takes its
	// default disposition and terminates the process, so every statement above
	// this one would be a window where a reload request kills the daemon. The
	// buffered channel holds one signal until the consumer goroutine starts.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	lockFile, err := d.acquireLock()
	if err != nil {
		return fmt.Errorf("acquiring lock: %w", err)
	}
	defer d.releaseLock(lockFile)

	// Startup snapshot. The values read here are frozen for the daemon's
	// lifetime, which is why reloadConfig pins them (see pinRestartOnly).
	cfg := d.config()

	tasksDir := expandTilde(cfg.TicketsDir)
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		return fmt.Errorf("creating tickets dir: %w", err)
	}
	logsDir := expandTilde(cfg.LogsDir)
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("creating logs dir: %w", err)
	}

	// Watch before scanning. A ticket written after the scan reads the
	// directory but before the watch is in place produces no event and no scan
	// entry, so it sits at todo until someone touches the file again. Events
	// that arrive during the rest of startup wait in the watcher's buffer until
	// the event loop below drains them; a ticket the scan already enqueued is
	// not enqueued twice, because handleFileChanged only acts on a status that
	// changed.
	w, err := watcher.New(tasksDir, d.debounce, watcher.MarkdownFilter)
	if err != nil {
		return fmt.Errorf("starting watcher: %w", err)
	}
	defer w.Close()

	if err := d.initialScan(tasksDir); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}

	// Cleanup sits between the scan and the scheduler: it needs the ticket set
	// the scan populates to tell its own windows from another daemon's, and it
	// must finish before the scheduler starts creating windows of its own.
	if !d.skipOrphanCleanup {
		d.cleanOrphanedWindows()
	}

	d.log.Info("daemon started", "dir", tasksDir, "tasks", len(d.tickets), "queued", d.queue.Len(), "tmux_session", d.tmuxSession)

	if cfg.Web.Enabled != nil && *cfg.Web.Enabled {
		srv := web.New(d, d.broker, cfg.Web.Host, cfg.Web.Port, cfg.Web.Token, d.tmuxSession, d.log)
		if err := srv.Start(); err != nil {
			d.log.Warn("web server failed to start, continuing without it", "err", err)
		} else {
			defer func() {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer shutdownCancel()
				_ = srv.Shutdown(shutdownCtx)
			}()
			d.log.Info("web server started", "addr", srv.Addr())
		}
	}

	// Registered before d.background.Wait() below, so LIFO runs it last: the
	// final flush then carries the measurements of work that outlived the
	// event loop.
	stopMetrics := d.startMetrics(ctx, cfg)
	defer stopMetrics()

	// Registered before the cancel below so it runs after it: background work
	// stops on a cancelled context, and waiting first would hang.
	defer d.background.Wait()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	// Scheduler goroutine.
	wg.Go(func() {
		d.scheduler(ctx, &wg)
	})

	stopReloadTriggers := d.startReloadTriggers(ctx, &wg, hup)
	defer stopReloadTriggers()

	// Event loop.
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				cancel()
				wg.Wait()
				return nil
			}
			d.handleEvent(ev)

		case err, ok := <-w.Errors():
			if !ok {
				cancel()
				wg.Wait()
				return nil
			}
			d.log.Error("watcher error", "err", err)

		case <-ctx.Done():
			d.log.Info("shutting down")
			d.killAll()
			wg.Wait()
			return nil
		}
	}
}

// startMetrics builds the configured exporter, registers the scheduler gauges,
// and returns the function that flushes and stops export.
//
// Failing to export must never keep tickets from running, so every error here
// warns and leaves the no-op recorder in place. An injected provider skips
// exporter construction entirely.
func (d *Daemon) startMetrics(ctx context.Context, cfg *config.Config) func() {
	stop := func() {}

	if d.meterProvider == nil && cfg.Metrics.Enabled != nil && *cfg.Metrics.Enabled {
		insecure, conflict := cfg.Metrics.ResolveInsecure()
		if conflict {
			d.log.Warn("metrics.endpoint states its own scheme, which overrides metrics.insecure",
				"endpoint", cfg.Metrics.Endpoint, "insecure", cfg.Metrics.Insecure)
		}
		rec, shutdown, err := metrics.New(ctx, metrics.Options{
			Enabled:     true,
			Endpoint:    cfg.Metrics.Endpoint,
			Headers:     cfg.Metrics.Headers,
			Interval:    cfg.Metrics.Interval.Duration,
			Insecure:    insecure,
			ServiceName: metrics.DefaultServiceName,
			Version:     d.version,
			Instance:    d.instanceName,
		})
		if err != nil {
			d.log.Warn("metrics exporter failed to start, continuing without it", "err", err)
		} else {
			d.metrics = rec
			stop = func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := shutdown(shutdownCtx); err != nil {
					d.log.Warn("metrics shutdown failed", "err", err)
				}
			}
			d.log.Info("metrics export started",
				"endpoint", cfg.Metrics.Endpoint, "interval", cfg.Metrics.Interval, "insecure", insecure)
		}
	}

	// Registered against whichever recorder is in place; on the no-op one this
	// costs nothing. The callbacks run on the collect path and so read only
	// lock-free state: taking d.mu here would stall every export behind a
	// wedged tmux call or a ticket write.
	if err := d.metrics.ObserveScheduler(
		func() int64 { return int64(len(d.sem)) },
		func() int64 { return int64(cap(d.sem)) },
		func() int64 { return d.queueDepth.Load() },
	); err != nil {
		d.log.Warn("registering scheduler gauges failed", "err", err)
	}

	return stop
}

func (d *Daemon) acquireLock() (*os.File, error) {
	path := expandTilde(d.lockPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock held by another process: %w", err)
	}
	return f, nil
}

func (d *Daemon) releaseLock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
	os.Remove(expandTilde(d.lockPath))
}

// initialScan registers every canonical ticket in the directory and queues the
// ones that are ready to run. It is two passes: readiness is decided against the
// whole store, which a single pass would still be building when it reached the
// first todo ticket.
func (d *Daemon) initialScan(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	cfg := d.config()
	var scanned []*ticket.Ticket
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		t, err := ticket.ParseFile(path)
		if err != nil {
			d.log.Warn("skipping file", "file", entry.Name(), "err", err)
			continue
		}
		if t.ID == "" {
			continue
		}
		if !ticket.IsCanonicalPath(path, t.ID) {
			d.log.Warn("skipping non-canonical ticket file", "file", entry.Name(), "id", t.ID)
			continue
		}

		d.mu.Lock()

		// Crash recovery: reset running → todo (kontora tickets only). Only
		// recover tickets this instance owns: an empty claim (single-machine
		// installs, pre-upgrade tickets) or our own name. A ticket claimed by
		// another instance sharing this tickets_dir is left in_progress and not
		// enqueued, so starting a second daemon can't steal or kill its work.
		if t.Kontora && t.Status == ticket.StatusInProgress {
			if t.ClaimedBy == "" || t.ClaimedBy == d.instanceName {
				d.ticketLog(t.ID).Warn("crash recovery: resetting to todo")
				_ = t.SetField("status", string(ticket.StatusTodo))
				data, merr := t.Marshal()
				if merr == nil {
					d.recordSelfWrite(path, data)
					_ = os.WriteFile(path, data, 0o644)
				}
			} else {
				d.ticketLog(t.ID).Info("in_progress on another instance, leaving alone", "claimed_by", t.ClaimedBy)
			}
		}

		d.tickets[t.ID] = newTicketState(t, path)
		d.mu.Unlock()
		scanned = append(scanned, t)
	}

	if !*cfg.AutoPickUp {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range scanned {
		if !t.Kontora || t.Status != ticket.StatusTodo {
			continue
		}
		if len(d.blockersLocked(t)) == 0 {
			d.ticketLog(t.ID).Info("enqueuing", "pipeline", t.Pipeline, "stage", t.Stage)
		}
		// enqueue applies the readiness guard and logs the dependencies that
		// hold the ticket back.
		d.enqueue(t)
	}
	return nil
}

func (d *Daemon) handleEvent(ev watcher.Event) {
	switch ev.Op {
	case watcher.OpChanged:
		// A removal is never the daemon's own write, so the suppression check
		// belongs here rather than above the switch: consulting it for a
		// removed file would drop the ticket's own deletion.
		if d.isSelfWrite(ev.Path) {
			return
		}
		d.handleFileChanged(ev.Path)
	case watcher.OpRemoved:
		d.handleFileRemoved(ev.Path)
	}
}

func (d *Daemon) handleFileChanged(path string) {
	t, err := ticket.ParseFile(path)
	if err != nil {
		d.log.Error("parse failed", "path", path, "err", err)
		return
	}
	if t.ID == "" {
		return
	}
	if !ticket.IsCanonicalPath(path, t.ID) {
		d.log.Warn("ignoring non-canonical ticket file", "path", path, "id", t.ID)
		return
	}

	log := d.ticketLog(t.ID)
	cfg := d.config()

	d.mu.Lock()
	defer d.mu.Unlock()

	prev, known := d.tickets[t.ID]
	d.tickets[t.ID] = newTicketState(t, path)
	d.broadcastTicketUpdate(t.ID)

	// An external edit can change this ticket's own deps or a status that
	// releases the tickets waiting on it, so the queue is reconciled whether or
	// not the status switch below has anything to do.
	defer d.reconcileDependenciesLocked(t.ID)

	if !t.Kontora {
		return
	}

	switch t.Status { //nolint:exhaustive
	case ticket.StatusTodo:
		if !known || prev.ticket.Status != ticket.StatusTodo {
			if cancel, ok := d.running[t.ID]; ok {
				log.Info("killing agent", "reason", "status changed to todo")
				cancel()
			}
			// Kill any leftover tmux window (e.g. from a paused ticket being retried).
			d.killTaskWindow(t.ID)

			if !*cfg.AutoPickUp {
				log.Info("skipping auto pick-up", "pipeline", t.Pipeline)
			} else if !known {
				log.Info("new ticket", "pipeline", t.Pipeline)
				d.enqueue(t)
			} else {
				log.Info("enqueuing", "previous_status", string(prev.ticket.Status), "pipeline", t.Pipeline, "stage", t.Stage)
				d.enqueue(t)
			}
		}
	case ticket.StatusInProgress:
		if d.claimedElsewhere(t) {
			// Another instance claimed this ticket and its claim reached us
			// through sync. If we're running it, yield: cancel our agent and
			// leave the file alone. The cancelled-context path in the exit
			// handlers writes nothing, so the foreign claim and worktree survive.
			if cancel, ok := d.running[t.ID]; ok {
				log.Info("yielding: ticket claimed by another instance", "claimed_by", t.ClaimedBy)
				cancel()
			}
			return
		}
		// Claimed by us but no local agent is running it: a stale self-claim that
		// came back through sync, or the both-sides-yielded state. Reset it to
		// todo and re-enqueue, matching live crash recovery.
		if _, running := d.running[t.ID]; !running && t.ClaimedBy == d.instanceName {
			log.Info("recovering stale self-claim", "claimed_by", t.ClaimedBy)
			_ = t.SetField("status", string(ticket.StatusTodo))
			if data, merr := t.Marshal(); merr == nil {
				d.recordSelfWrite(path, data)
				if werr := os.WriteFile(path, data, 0o644); werr != nil {
					log.Error("recover stale self-claim: write failed", "err", werr)
					return
				}
			}
			d.tickets[t.ID] = newTicketState(t, path)
			d.enqueue(t)
			d.broadcastTicketUpdate(t.ID)
		}
	case ticket.StatusPaused, ticket.StatusHumanReview, ticket.StatusCancelled, ticket.StatusOpen:
		if cancel, ok := d.running[t.ID]; ok {
			log.Info("killing agent", "reason", "user set "+string(t.Status))
			cancel()
		}
		if t.Status == ticket.StatusCancelled {
			go d.cleanupWorktree(log, t)
		}
	case ticket.StatusDone, ticket.StatusArchived:
		if cancel, ok := d.running[t.ID]; ok {
			log.Info("killing agent", "reason", "user set "+string(t.Status))
			cancel()
		}
		go d.cleanupWorktree(log, t)
	default:
		if cfg.IsCustomStatus(string(t.Status)) {
			if cancel, ok := d.running[t.ID]; ok {
				log.Info("killing agent", "reason", "user set "+string(t.Status))
				cancel()
			}
		}
	}
}

func (d *Daemon) handleFileRemoved(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.forgetSelfWrite(path)
	for id, ts := range d.tickets {
		if ts.filePath == path {
			if cancel, ok := d.running[id]; ok {
				d.ticketLog(id).Info("killing agent", "reason", "file removed")
				cancel()
			}
			d.removeQueuedLocked(id)
			d.broadcastTicketDeleted(ts)
			delete(d.tickets, id)
			// Whoever depended on this ticket now names an id nothing answers,
			// which blocks them.
			d.reconcileDependenciesLocked(id)
			return
		}
	}
}

// ticketBranch returns the branch name for a ticket, using the ticket's
// existing branch if set, otherwise generating one from the config prefix.
// It takes the caller's config snapshot because branch_prefix reloads live: a
// caller that reads it twice can reserve one branch name and check out another.
func ticketBranch(cfg *config.Config, t *ticket.Ticket) string {
	if b := strings.TrimSpace(t.Branch); b != "" {
		return b
	}
	return worktree.BranchName(cfg.BranchPrefixFor(t.Path), t.ID)
}

// autoTicketBranch returns the branch a ticket that carries none would run on:
// the slug-derived name when its project has branch naming on and the title
// yields a slug, the ID-derived name otherwise. Pickup produces the same two
// names, one through persistGeneratedBranchLocked and one through ticketBranch,
// so the web UI can show this before a run starts.
func autoTicketBranch(cfg *config.Config, t *ticket.Ticket) string {
	if cfg.BranchNamingFor(t.Path).Mode == config.BranchNamingModeSlug {
		if branch, ok := generatedTicketBranch(cfg, t); ok {
			return branch
		}
	}
	return worktree.BranchName(cfg.BranchPrefixFor(t.Path), t.ID)
}

func generatedTicketBranch(cfg *config.Config, t *ticket.Ticket) (string, bool) {
	slug := worktree.Slug(t.Title())
	if slug == "" {
		return "", false
	}
	return worktree.BranchName(cfg.BranchPrefixFor(t.Path), slug+"-"+t.ID), true
}

// ticketBase returns the branch a ticket's worktree is cut from. Empty means
// the repository's default branch. Unlike ticketBranch it takes no config
// snapshot, because no config value feeds it.
func ticketBase(t *ticket.Ticket) string {
	return strings.TrimSpace(t.BaseBranch)
}

// removeWorktreeAt removes the git worktree at wtPath. Logs but does not
// propagate errors — a failed cleanup should not block ticket completion.
// Dirty worktrees are preserved (branch and directory kept intact).
func (d *Daemon) removeWorktreeAt(log *slog.Logger, repoPath, wtPath string) {
	d.logRemoveResult(log, d.worktrees.RemoveAt(repoPath, wtPath))
}

// removeWorktreeByBranch discovers the worktree via git and removes it. Used
// when the caller doesn't have the worktree path in hand (e.g. status-change
// goroutine). Logs but does not propagate errors.
func (d *Daemon) removeWorktreeByBranch(log *slog.Logger, repoPath, branch string) {
	d.logRemoveResult(log, d.worktrees.Remove(repoPath, branch))
}

func (d *Daemon) logRemoveResult(log *slog.Logger, err error) {
	switch {
	case errors.Is(err, worktree.ErrDirtyWorktree):
		log.Warn("worktree has uncommitted changes, keeping it")
	case err != nil:
		log.Warn("worktree cleanup failed", "err", err)
	default:
		log.Info("worktree removed")
	}
}

// claimedElsewhere reports whether the ticket carries a non-empty claim owned
// by a different instance. It guards every place the daemon would otherwise
// overwrite a ticket that another machine on the shared tickets_dir is running.
// Callers consult it only for in_progress tickets, per the claim model.
func (d *Daemon) claimedElsewhere(t *ticket.Ticket) bool {
	return t.ClaimedBy != "" && t.ClaimedBy != d.instanceName
}

// isUserOverride returns true if the status represents a user-initiated
// override that should prevent the exit handler from changing the status.
func (d *Daemon) isUserOverride(s ticket.Status) bool {
	return s == ticket.StatusPaused || s == ticket.StatusHumanReview ||
		s == ticket.StatusCancelled || s == ticket.StatusOpen ||
		s == ticket.StatusDone || s == ticket.StatusArchived ||
		d.config().IsCustomStatus(string(s))
}

// isTerminalOverride returns true if the status is a terminal user override
// that requires worktree cleanup and task window teardown.
func isTerminalOverride(s ticket.Status) bool {
	return s == ticket.StatusCancelled || s == ticket.StatusDone || s == ticket.StatusArchived
}

// cleanupWorktree resolves repo info from a ticket and removes its worktree.
// Safe to call from a goroutine (does not hold d.mu).
func (d *Daemon) cleanupWorktree(log *slog.Logger, t *ticket.Ticket) {
	_, repoPath, err := d.resolvePath(t)
	if err != nil {
		log.Warn("worktree cleanup: resolve path failed", "err", err)
		return
	}
	d.removeWorktreeByBranch(log, repoPath, ticketBranch(d.config(), t))
}

// blockersLocked returns the dependency ids that hold a ticket back: the ones
// naming a ticket that is not closed, and the ones naming no ticket at all.
// Must be called with d.mu held.
func (d *Daemon) blockersLocked(t *ticket.Ticket) []string {
	var blockers []string
	for _, dep := range t.Deps {
		if dep == "" {
			continue
		}
		if ts, ok := d.tickets[dep]; ok && ticket.IsDependencyResolved(ts.ticket.Status) {
			continue
		}
		blockers = append(blockers, dep)
	}
	return blockers
}

// dependentsLocked returns the ids of the tracked tickets whose deps name id.
// Must be called with d.mu held.
func (d *Daemon) dependentsLocked(id string) []string {
	var ids []string
	for other, ts := range d.tickets {
		if slices.Contains(ts.ticket.Deps, id) {
			ids = append(ids, other)
		}
	}
	return ids
}

// reconcileDependenciesLocked re-checks a ticket and every ticket that depends
// on it against the dependency graph: one that became ready is queued, one that
// became blocked is dropped from the queue. A running ticket is left alone, so
// an edge added mid-run does not interrupt it. Must be called with d.mu held.
func (d *Daemon) reconcileDependenciesLocked(id string) {
	autoPickUp := *d.config().AutoPickUp

	for _, candidate := range append([]string{id}, d.dependentsLocked(id)...) {
		ts, ok := d.tickets[candidate]
		if !ok {
			continue
		}
		t := ts.ticket
		if !t.Kontora || t.Status != ticket.StatusTodo {
			continue
		}
		if _, running := d.running[candidate]; running {
			continue
		}
		if len(d.blockersLocked(t)) == 0 {
			if autoPickUp {
				d.enqueue(t)
			}
			continue
		}
		d.removeQueuedLocked(candidate)
	}
}

// enqueue adds a ticket to the queue. Must be called with d.mu held.
// Skips enqueue if the ticket is already queued or blocked by a dependency.
func (d *Daemon) enqueue(t *ticket.Ticket) {
	if t == nil || !t.Kontora {
		return
	}
	if d.queued[t.ID] {
		return
	}
	if blockers := d.blockersLocked(t); len(blockers) > 0 {
		d.ticketLog(t.ID).Info("not queued: blocked by dependencies", "blockers", strings.Join(blockers, ", "))
		return
	}
	d.queued[t.ID] = true
	heap.Push(&d.queue, &queueItem{
		ticketID:   t.ID,
		created:    derefTime(t.Created),
		enqueuedAt: time.Now(),
	})
	d.syncQueueDepthLocked()
	d.queueCond.Signal()
}

// syncQueueDepthLocked republishes the queue length for the depth gauge. Must
// be called with d.mu held, at every point that changes the heap. Storing the
// length rather than stepping a counter keeps the two from drifting when a
// removal finds nothing to remove.
func (d *Daemon) syncQueueDepthLocked() {
	d.queueDepth.Store(int64(d.queue.Len()))
}

func (d *Daemon) scheduler(ctx context.Context, wg *sync.WaitGroup) {
	go func() {
		<-ctx.Done()
		d.mu.Lock()
		d.queueCond.Signal()
		d.mu.Unlock()
	}()

	for {
		// The slot is taken before the queue head, not after. Popping first would
		// hide the ticket from the queue for as long as the daemon stays busy:
		// it would not be counted as queued, and a ticket enqueued in the
		// meantime could not overtake it however much older it is.
		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		d.mu.Lock()
		for d.queue.Len() == 0 {
			if ctx.Err() != nil {
				d.mu.Unlock()
				<-d.sem
				return
			}
			d.queueCond.Wait()
		}

		item := heap.Pop(&d.queue).(*queueItem)
		delete(d.queued, item.ticketID)
		d.syncQueueDepthLocked()
		d.mu.Unlock()

		if ctx.Err() != nil {
			<-d.sem
			return
		}

		// The slot is already held by the time an item is popped, so the wait ends
		// here. Recorded after the unlock, because the exporter's collect path
		// must never end up waiting behind d.mu.
		d.metrics.QueueWait(ctx, time.Since(item.enqueuedAt))

		wg.Add(1)
		go func(ticketID string) {
			defer wg.Done()
			defer func() { <-d.sem }()
			d.runTicket(ctx, ticketID)
		}(item.ticketID)
	}
}

// claimableLocked reports whether a popped ticket may still be started, and
// returns its state when it may. Everything here can have changed between the
// enqueue and this claim. Must be called with d.mu held.
func (d *Daemon) claimableLocked(ticketID string, log *slog.Logger) (*ticketState, bool) {
	ts, ok := d.tickets[ticketID]
	if !ok {
		return nil, false
	}

	// The ticket must still be managed by Kontora and in a state to process.
	if !ts.ticket.Kontora || ts.ticket.Status != ticket.StatusTodo {
		return nil, false
	}

	// A dependency can be added, or a resolved one reopened, after the enqueue.
	// The queue guard cannot see that, so readiness is checked again here, where
	// the agent is about to be spawned.
	if blockers := d.blockersLocked(ts.ticket); len(blockers) > 0 {
		log.Info("pickup skipped: blocked by dependencies", "blockers", strings.Join(blockers, ", "))
		return nil, false
	}

	// A Plannotator session open on this ticket owns it. A stage run would edit
	// the file under the reviewer, and the annotations they then submit would be
	// refused because the ticket is running, leaving the notes unapplied.
	// releasePlannotator offers the ticket again once the session closes.
	if _, annotating := d.plannotator[ticketID]; annotating {
		d.plannotatorDeferred[ticketID] = struct{}{}
		log.Info("pickup deferred: a plannotator session is open for this ticket")
		return nil, false
	}
	delete(d.plannotatorDeferred, ticketID)

	return ts, true
}

func (d *Daemon) runTicket(ctx context.Context, ticketID string) {
	log := d.ticketLog(ticketID)
	cfg := d.config()

	d.mu.Lock()
	if _, isRunning := d.running[ticketID]; isRunning {
		// Ticket is still running (e.g. shutting down after pause/skip).
		// Re-enqueue it to be picked up later and release the semaphore slot
		// immediately so other tickets can proceed.
		if ts, ok := d.tickets[ticketID]; ok {
			d.enqueue(ts.ticket)
		}
		d.mu.Unlock()
		return
	}

	ts, ok := d.claimableLocked(ticketID, log)
	if !ok {
		d.mu.Unlock()
		return
	}
	t := ts.ticket
	filePath := ts.filePath

	// Register cancel func before mutating status so concurrent ops
	// can properly cancel the ticket while it's setting up worktrees.
	taskCtx, taskCancel := context.WithCancel(ctx)
	d.running[ticketID] = taskCancel

	// A pending annotation must not stamp a branch. The run edits the ticket file,
	// and a ticket annotated before its first stage has no work to put on a branch
	// yet.
	if t.AnnotationReturnStatus == "" {
		d.persistGeneratedBranchLocked(cfg, log, t, filePath)
	}

	// Reserve the branch atomically so two tickets targeting the same
	// repository and branch can't both pass this guard and reuse each
	// other's worktree by surprise. Keyed by (repoPath, branch) because
	// identical branch names in different repos don't collide.
	//
	// An annotation run reserves nothing: it builds no worktree and stamps no
	// branch, and two tickets that name neither a repository nor a branch would
	// share one key and pause each other.
	branch := ticketBranch(cfg, t)
	var branchKey string
	if t.AnnotationReturnStatus == "" {
		branchKey = expandTilde(t.Path) + "\x00" + branch
		if holder, ok := d.runningBranches[branchKey]; ok && holder != ticketID {
			taskCancel()
			delete(d.running, ticketID)
			d.mu.Unlock()
			d.pauseTicket(t, filePath, fmt.Sprintf("branch %s in use by ticket %s", branch, holder))
			return
		}
		d.runningBranches[branchKey] = ticketID
	}

	defer func() {
		taskCancel()
		d.mu.Lock()
		delete(d.running, ticketID)
		if branchKey != "" && d.runningBranches[branchKey] == ticketID {
			delete(d.runningBranches, branchKey)
		}
		d.mu.Unlock()
	}()

	// A pending annotation owns the pickup, whether or not the ticket has a
	// pipeline: the run rewrites the ticket and hands the status back, so no stage
	// runs and no pipeline action is evaluated.
	if t.AnnotationReturnStatus != "" {
		d.mu.Unlock()
		d.runAnnotationRun(ctx, taskCtx, cfg, log, ticketID, t, filePath)
		return
	}

	if t.Pipeline == "" {
		d.mu.Unlock()
		d.runSimpleTicket(ctx, taskCtx, cfg, log, ticketID, t, filePath)
		return
	}

	pipelineCfg, ok := cfg.Pipelines[t.Pipeline]
	if !ok {
		// A config reload can drop a pipeline out from under a todo ticket.
		// Pause it with a reason so it does not sit in todo unexplained.
		log.Error("unknown pipeline", "pipeline", t.Pipeline)
		d.mu.Unlock()
		d.pauseTicket(t, filePath, fmt.Sprintf("unknown pipeline %q", t.Pipeline))
		return
	}

	// If stage is empty, set to first stage.
	if t.Stage == "" {
		_ = t.SetField("stage", pipelineCfg[0].Stage)
	}
	d.mu.Unlock()

	// Out-of-band rework stage: when built-in rework handling is enabled and
	// the ticket is parked in the rework stage (set by StartPlannotatorReview)
	// and the user's pipeline doesn't declare it as a step, run it via the
	// dedicated path so we can route back to status=review after the agent
	// exits. A user-defined rework stage is left alone.
	if cfg.ReworkIsBuiltin &&
		t.Stage == config.ReworkStageName &&
		!stageInPipeline(pipelineCfg, config.ReworkStageName) {
		d.runReworkStage(ctx, taskCtx, cfg, log, ticketID, t, filePath)
		return
	}

	// Evaluate pickup.
	action, err := pipeline.Evaluate(t, pipelineCfg, pipeline.Event{
		Kind:      pipeline.EventPickedUp,
		Timestamp: time.Now(),
	})
	if err != nil {
		// A reload that removes the ticket's stage reaches here. Pause with the
		// reason rather than leaving the ticket stuck in todo.
		log.Error("evaluate pickup failed", "err", err)
		d.pauseTicket(t, filePath, "evaluate pickup failed: "+err.Error())
		return
	}

	// Apply fields (status=in_progress, started_at).
	if err := d.applyAction(t, action); err != nil {
		log.Error("apply action failed", "phase", "pickup", "err", err)
		return
	}
	// Claim the ticket for this instance in the same write that flips it to
	// in_progress, so a daemon on another machine sharing the tickets_dir sees
	// the owner before it acts.
	_ = t.SetField("claimed_by", d.instanceName)
	if err := d.writeTicket(t, filePath); err != nil {
		log.Error("write failed", "phase", "pickup", "err", err)
		return
	}
	log.Info("picked up", "pipeline", t.Pipeline, "stage", t.Stage)
	d.broadcastTicketUpdateLocking(ticketID)

	// Check if we were paused/cancelled between pickup and now.
	if err := taskCtx.Err(); err != nil {
		log.Info("cancelled before worktree creation")
		return
	}

	stageName := action.Spawn.Stage
	agentName := action.Spawn.Agent
	if t.Agent != "" {
		agentName = t.Agent
	}

	agentCfg, agentOK := cfg.Agents[agentName]
	if !agentOK {
		log.Error("unknown agent", "agent", agentName)
		d.pauseTicket(t, filePath, fmt.Sprintf("unknown agent %q", agentName))
		return
	}
	stageCfg := cfg.Stages[stageName]

	repoPath, wtPath, prepOK := d.prepareWorktreeForAgent(taskCtx, prepareWorktreeParams{
		cfg:        cfg,
		log:        log,
		t:          t,
		filePath:   filePath,
		ticketID:   ticketID,
		stageName:  stageName,
		agentName:  agentName,
		branch:     branch,
		isPipeline: true,
	})
	if !prepOK {
		return
	}

	rendered, err := d.renderTicketPrompt(cfg, stageCfg.Prompt, t, filePath, wtPath)
	if err != nil {
		log.Error("render prompt failed", "stage", stageName, "err", err)
		d.pauseTicket(t, filePath, "render prompt failed: "+err.Error())
		return
	}
	if rendered != "" {
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, true, checkpointKind(agentCfg))
	}

	log.Info("spawning agent", "agent", agentName, "stage", stageName, "binary", agentCfg.Binary)

	runIndex := stageRunIndex(t, stageName)
	run, spawnOK := d.spawnAgentRun(taskCtx, t, spawnAgentParams{
		cfg:          cfg,
		ctx:          ctx,
		log:          log,
		ticketID:     ticketID,
		filePath:     filePath,
		agentName:    agentName,
		pipelineName: t.Pipeline,
		stageName:    stageName,
		run:          runIndex,
		wtPath:       wtPath,
		repoPath:     repoPath,
		branch:       branch,
		rendered:     rendered,
		agentCfg:     agentCfg,
		stageCfg:     stageCfg,
		isPipeline:   true,
	})
	if !spawnOK {
		return
	}

	d.handleAgentExit(ctx, taskCtx, handleExitParams{
		cfg:          cfg,
		agentName:    agentName,
		log:          log,
		ticketID:     ticketID,
		filePath:     filePath,
		stageName:    stageName,
		run:          runIndex,
		result:       run.Result,
		finalMessage: run.FinalMessage,
		model:        run.Model,
		effort:       run.Effort,
		sessionKind:  run.SessionKind,
		sessionRef:   run.SessionRef,
		pipelineCfg:  pipelineCfg,
		repoPath:     repoPath,
		wtPath:       wtPath,
		branch:       branch,
	})
}

// persistGeneratedBranchLocked writes a generated branch while d.mu excludes
// watcher updates. If the write fails, worktree creation and cleanup both use
// ticketBranch's derived fallback.
func (d *Daemon) persistGeneratedBranchLocked(cfg *config.Config, log *slog.Logger, t *ticket.Ticket, filePath string) {
	if cfg.BranchNamingFor(t.Path).Mode != config.BranchNamingModeSlug || strings.TrimSpace(t.Branch) != "" {
		return
	}

	branch, ok := generatedTicketBranch(cfg, t)
	if !ok {
		return
	}
	oldBranch := t.Branch
	if err := t.SetField("branch", branch); err != nil {
		log.Error("set generated branch failed", "branch", branch, "err", err)
		return
	}
	if err := d.writeTicketLocked(t, filePath); err != nil {
		if resetErr := t.SetField("branch", oldBranch); resetErr != nil {
			log.Error("reset generated branch failed", "branch", branch, "err", resetErr)
		}
		log.Error("persist generated branch failed", "branch", branch, "err", err)
		return
	}
	log.Info("generated branch", "branch", branch)
}

// simpleStageName keys the log, the session and the history rows of a ticket
// that runs without a pipeline. It is not a configured stage, so nothing reads
// a prompt or a timeout under this name.
const simpleStageName = "default"

func (d *Daemon) runSimpleTicket(ctx, taskCtx context.Context, cfg *config.Config, log *slog.Logger, ticketID string, t *ticket.Ticket, filePath string) {
	agentName := cfg.DefaultAgent
	if t.Agent != "" {
		agentName = t.Agent
	}
	agentCfg, ok := cfg.Agents[agentName]
	if !ok {
		log.Error("unknown agent", "agent", agentName)
		d.pauseTicket(t, filePath, fmt.Sprintf("unknown agent %q", agentName))
		return
	}

	// Set status=in_progress, started_at, and claim for this instance.
	now := time.Now()
	_ = t.SetField("status", string(ticket.StatusInProgress))
	_ = t.SetField("started_at", now.Format(time.RFC3339))
	_ = t.SetField("claimed_by", d.instanceName)
	if err := d.writeTicket(t, filePath); err != nil {
		log.Error("write failed", "phase", "pickup", "err", err)
		return
	}
	log.Info("picked up (simple)", "agent", agentName)

	// Check if we were paused/cancelled between pickup and now.
	if err := taskCtx.Err(); err != nil {
		log.Info("cancelled before worktree creation")
		return
	}

	branch := ticketBranch(cfg, t)
	repoPath, wtPath, prepOK := d.prepareWorktreeForAgent(taskCtx, prepareWorktreeParams{
		cfg:       cfg,
		log:       log,
		t:         t,
		filePath:  filePath,
		ticketID:  ticketID,
		stageName: simpleStageName,
		agentName: agentName,
		branch:    branch,
	})
	if !prepOK {
		return
	}

	rendered, err := d.renderTicketPrompt(cfg, defaultPromptTemplate, t, filePath, wtPath)
	if err != nil {
		log.Error("render prompt failed", "err", err)
		d.pauseTicket(t, filePath, "render prompt failed: "+err.Error())
		return
	}
	if rendered != "" {
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, false, checkpointKind(agentCfg))
	}

	log.Info("spawning agent", "agent", agentName, "binary", agentCfg.Binary)

	run, spawnOK := d.spawnAgentRun(taskCtx, t, spawnAgentParams{
		cfg:        cfg,
		ctx:        ctx,
		log:        log,
		ticketID:   ticketID,
		filePath:   filePath,
		agentName:  agentName,
		stageName:  simpleStageName,
		run:        stageRunIndex(t, simpleStageName),
		wtPath:     wtPath,
		repoPath:   repoPath,
		branch:     branch,
		rendered:   rendered,
		agentCfg:   agentCfg,
		stageCfg:   config.Stage{},
		isPipeline: false,
	})
	if !spawnOK {
		return
	}
	result := run.Result

	// Handle context cancellation.
	if taskCtx.Err() != nil {
		if ctx.Err() != nil {
			log.Warn("interrupted by shutdown")
			return
		}
		if t2, err := ticket.ParseFile(filePath); err == nil {
			if isTerminalOverride(t2.Status) {
				d.removeWorktreeAt(log, repoPath, wtPath)
				d.killTaskWindow(ticketID)
			}
		}
		log.Info("interrupted by user")
		return
	}

	// Checked before the hooks for what it rules out, not for the ticket it
	// returns: hooks that write the worktree must not run for an exit this
	// instance may not act on.
	guard := exitGuardParams{log: log, ticketID: ticketID, filePath: filePath, repoPath: repoPath, wtPath: wtPath}
	if _, ok := d.readTicketForExit(guard); !ok {
		return
	}

	// A ticket without a pipeline runs one stage under simpleStageName, so it
	// fires the post-run event too. spawnAgentRun already fired the pre-run one.
	hookErr := d.runHooks(taskCtx, cfg, log, hook.Context{
		Event:      config.HookStageEnd,
		TicketID:   ticketID,
		TicketFile: filePath,
		Worktree:   wtPath,
		RepoPath:   repoPath,
		Branch:     branch,
		Stage:      simpleStageName,
		Agent:      agentName,
		ExitCode:   &result.ExitCode,
	})
	if taskCtx.Err() != nil {
		log.Info("interrupted during stage_end hooks")
		return
	}

	// The hooks can take as long as their timeout allows, and one of them can
	// write the ticket itself through KONTORA_TICKET_FILE, so the read the write
	// below builds on is taken after them.
	t2, guardOK := d.readTicketForExit(guard)
	if !guardOK {
		return
	}

	// Simple exit handling: 0 → done, non-0 → paused.
	switch {
	case result.ExitCode != 0:
		_ = t2.SetField("status", string(ticket.StatusPaused))
		_ = t2.SetField("last_error", fmt.Sprintf("agent exited with code %d", result.ExitCode))
		log.Warn("paused", "exit_code", result.ExitCode)
	case hookErr != nil:
		// The agent succeeded, so the hook decides the outcome alone. Its run is
		// still recorded: a paused ticket with no summary hides what the agent did.
		_ = t2.SetField("status", string(ticket.StatusPaused))
		_ = t2.SetField("last_error", hookErr.Error())
		_ = t2.SetField("summary", runSummary(t2.Summary, run.FinalMessage))
		t2.AppendNote(hookErr.Error(), time.Now())
	default:
		_ = t2.SetField("status", string(ticket.StatusDone))
		_ = t2.SetField("last_error", "")
		_ = t2.SetField("summary", runSummary(t2.Summary, run.FinalMessage))
		completedAt := result.ExitedAt
		if completedAt.IsZero() {
			completedAt = time.Now()
		}
		_ = t2.SetField("completed_at", completedAt.Format(time.RFC3339))
		log.Info("completed", "branch", t2.Branch)
		d.killTaskWindow(ticketID)
	}

	if err := d.writeTicket(t2, filePath); err != nil {
		log.Error("write failed", "phase", "exit", "err", err)
		return
	}

	if result.ExitCode == 0 && hookErr == nil {
		d.removeWorktreeAt(log, repoPath, wtPath)
	}

	d.mu.Lock()
	d.setTicketState(ticketID, t2, filePath)
	d.broadcastTicketUpdate(ticketID)
	d.mu.Unlock()
}

// exitGuardParams is what readTicketForExit needs to decide an exit is still
// the daemon's to act on, and to clean up when it is not.
type exitGuardParams struct {
	log      *slog.Logger
	ticketID string
	filePath string
	repoPath string
	wtPath   string
}

// readTicketForExit re-reads the ticket after an agent has exited and reports
// whether this instance may still act on that exit. It is called again after
// the stage_end hooks, because those run for as long as their timeout allows:
// a user or another instance can move in that window, and a hook holding
// KONTORA_TICKET_FILE can rewrite the file the daemon is about to write back.
func (d *Daemon) readTicketForExit(p exitGuardParams) (*ticket.Ticket, bool) {
	t2, err := ticket.ParseFile(p.filePath)
	if err != nil {
		p.log.Error("re-read failed after agent exit", "err", err)
		return nil, false
	}

	// If user changed status while running, respect that.
	if d.isUserOverride(t2.Status) {
		p.log.Info("user override during execution", "status", t2.Status)
		if isTerminalOverride(t2.Status) {
			d.removeWorktreeAt(p.log, p.repoPath, p.wtPath)
			d.killTaskWindow(p.ticketID)
		}
		d.mu.Lock()
		d.tickets[p.ticketID] = newTicketState(t2, p.filePath)
		d.mu.Unlock()
		return nil, false
	}

	// If another instance claimed the ticket while we ran (its claim arrived
	// through sync), discard our result: write nothing and keep the worktree,
	// which may hold unpushed commits.
	if d.claimedElsewhere(t2) {
		p.log.Info("discarding exit result: claimed by another instance", "claimed_by", t2.ClaimedBy)
		d.killTaskWindow(p.ticketID)
		d.mu.Lock()
		d.tickets[p.ticketID] = newTicketState(t2, p.filePath)
		d.mu.Unlock()
		return nil, false
	}
	return t2, true
}

type handleExitParams struct {
	// cfg is the caller's config snapshot and agentName the agent it resolved
	// from it, both handed on to the final summary pass.
	cfg          *config.Config
	agentName    string
	log          *slog.Logger
	ticketID     string
	filePath     string
	stageName    string
	run          int
	result       process.Result
	finalMessage string
	// model and effort are what the run passed the agent, and sessionKind and
	// sessionRef locate the session JSONL it wrote. All four are recorded on
	// its history entry.
	model       string
	effort      string
	sessionKind string
	sessionRef  string
	pipelineCfg config.Pipeline
	repoPath    string
	wtPath      string
	branch      string
}

func (d *Daemon) handleAgentExit(ctx, taskCtx context.Context, p handleExitParams) {
	// If context was cancelled, don't evaluate exit as a pipeline failure.
	// Distinguish daemon shutdown (ctx cancelled) from user cancel (only taskCtx).
	if taskCtx.Err() != nil {
		if ctx.Err() != nil {
			p.log.Warn("interrupted by shutdown", "stage", p.stageName)
			return
		}
		// User changed status (e.g. cancelled, done, paused, or open) while running. Clean up worktree if in a terminal override.
		if t2, err := ticket.ParseFile(p.filePath); err == nil {
			if isTerminalOverride(t2.Status) {
				d.removeWorktreeAt(p.log, p.repoPath, p.wtPath)
				d.killTaskWindow(p.ticketID)
			}
		}
		p.log.Info("interrupted by user", "stage", p.stageName)
		return
	}

	// Checked before the hooks for what it rules out, not for the ticket it
	// returns: an exit the guards discard is not this stage's outcome to act on,
	// and hooks that write the worktree must not run for it.
	guard := exitGuardParams{
		log: p.log, ticketID: p.ticketID, filePath: p.filePath, repoPath: p.repoPath, wtPath: p.wtPath,
	}
	if _, ok := d.readTicketForExit(guard); !ok {
		return
	}

	// stage_end runs before the exit is evaluated, so a hook still sees the
	// worktree as the agent left it. It runs after the guards above, because an
	// exit those discard is not this stage's outcome to act on. Its failure
	// cannot change what the pipeline decides; it is applied after the action.
	hookErr := d.runHooks(taskCtx, p.cfg, p.log, hook.Context{
		Event:      config.HookStageEnd,
		TicketID:   p.ticketID,
		TicketFile: p.filePath,
		Worktree:   p.wtPath,
		RepoPath:   p.repoPath,
		Branch:     p.branch,
		Stage:      p.stageName,
		Agent:      p.agentName,
		ExitCode:   &p.result.ExitCode,
	})
	if taskCtx.Err() != nil {
		p.log.Info("interrupted during stage_end hooks", "stage", p.stageName)
		return
	}

	// The hooks can take as long as their timeout allows, and one of them can
	// write the ticket itself through KONTORA_TICKET_FILE, so the read the
	// pipeline evaluates is taken after them.
	t2, guardOK := d.readTicketForExit(guard)
	if !guardOK {
		return
	}

	// Use the fresh ticket from disk (preserves user edits during execution)
	// but restore status to running for engine's precondition.
	_ = t2.SetField("status", string(ticket.StatusInProgress))

	// Evaluate exit.
	exitAction, err := pipeline.Evaluate(t2, p.pipelineCfg, pipeline.Event{
		Kind:      pipeline.EventAgentExited,
		ExitCode:  p.result.ExitCode,
		Timestamp: p.result.ExitedAt,
	})
	if err != nil {
		p.log.Error("evaluate exit failed", "stage", p.stageName, "err", err)
		d.pauseTicket(t2, p.filePath, "evaluate exit failed: "+err.Error())
		return
	}

	summary := runSummary(t2.Summary, p.finalMessage)
	if exitAction.History != nil {
		// The agent that actually ran, together with the settings resolved for
		// it. The engine names the pipeline step's agent, which the ticket's own
		// agent field overrides at spawn. Reading the name back off the ticket
		// here instead would let a stage_end hook that rewrites that field leave
		// a row naming one agent beside another's model and effort.
		if p.agentName != "" {
			exitAction.History.Agent = p.agentName
		}
		exitAction.History.Summary = summary
		exitAction.History.Run = p.run
		exitAction.History.Model = p.model
		exitAction.History.Effort = p.effort
		exitAction.History.SessionKind = p.sessionKind
		exitAction.History.SessionRef = p.sessionRef
	}

	nextStage := fieldValue(exitAction.Fields, "stage")
	switch exitAction.Kind {
	case pipeline.ActionAdvance:
		p.log.Info("advancing", "from", p.stageName, "to", nextStage)
	case pipeline.ActionComplete:
		p.log.Info("completed", "branch", t2.Branch)
	case pipeline.ActionRetry:
		attempt := fieldValue(exitAction.Fields, "attempt")
		p.log.Info("retrying", "stage", p.stageName, "attempt", attempt)
	case pipeline.ActionBack:
		p.log.Info("going back", "from", p.stageName, "to", nextStage)
	case pipeline.ActionPause:
		p.log.Warn("paused", "stage", p.stageName, "exit_code", p.result.ExitCode)
	case pipeline.ActionPark:
		status := fieldValue(exitAction.Fields, "status")
		p.log.Info("parked", "stage", p.stageName, "status", status, "exit_code", p.result.ExitCode)
	case pipeline.ActionSpawn:
		p.log.Warn("unexpected spawn action after exit", "stage", p.stageName)
	}

	// Recorded after the switch, not per case, so a kind added later cannot be
	// forgotten here.
	d.metrics.Transition(ctx, p.stageName, exitAction.Kind.String())

	if err := d.applyAction(t2, exitAction); err != nil {
		p.log.Error("apply action failed", "phase", "exit", "err", err)
		d.pauseTicket(t2, p.filePath, "apply action failed: "+err.Error())
		return
	}

	switch {
	case exitAction.Kind == pipeline.ActionPause:
		_ = t2.SetField("last_error", fmt.Sprintf("agent exited with code %d (stage: %s)", p.result.ExitCode, p.stageName))
	case exitAction.Kind == pipeline.ActionPark && p.result.ExitCode != 0:
		_ = t2.SetField("last_error", fmt.Sprintf("agent exited with code %d (stage: %s)", p.result.ExitCode, p.stageName))
	default:
		_ = t2.SetField("last_error", "")
	}
	_ = t2.SetField("summary", summary)

	hookPaused := hookErr != nil
	if hookPaused {
		recordStageEndHookFailure(t2, hookErr)
	}

	if err := d.writeTicket(t2, p.filePath); err != nil {
		p.log.Error("write failed", "phase", "exit", "err", err)
		return
	}

	d.finishAgentExit(ctx, t2, p, exitAction.Kind, hookPaused)
}

// recordStageEndHookFailure stops a ticket whose stage_end hook failed under a
// pause policy. What the pipeline decided is left alone: the action's fields
// are already applied, and last_error keeps the pipeline's own reason, taking
// the hook's message only when the pipeline recorded none.
//
// completed_at is the exception. A completing action stamps it, and a paused
// ticket carrying the time it finished is a state no other path can produce —
// the run is not over, and no final summary was written for it either.
func recordStageEndHookFailure(t *ticket.Ticket, hookErr error) {
	_ = t.SetField("status", string(ticket.StatusPaused))
	if t.CompletedAt != nil {
		_ = t.SetField("completed_at", nil)
	}
	t.AppendNote(hookErr.Error(), time.Now())
	if t.LastError == "" {
		_ = t.SetField("last_error", hookErr.Error())
	}
}

// finishAgentExit runs everything that follows the exit write: worktree
// cleanup, the tmux window, the cached state, re-queueing, and the final
// summary pass. hookPaused suppresses the steps that would carry onwards a
// ticket a stage_end hook stopped — a re-enqueued advance would undo the pause
// on the next tick.
func (d *Daemon) finishAgentExit(ctx context.Context, t *ticket.Ticket, p handleExitParams, kind pipeline.ActionKind, hookPaused bool) {
	// Clean up worktree on terminal states.
	if kind == pipeline.ActionComplete && !hookPaused {
		d.removeWorktreeAt(p.log, p.repoPath, p.wtPath)
	}

	// Kill tmux window unless paused or parked-with-failure — keep it alive
	// so the user can attach and inspect/fix the failure in the worktree.
	keepWindow := hookPaused ||
		kind == pipeline.ActionPause ||
		(kind == pipeline.ActionPark && p.result.ExitCode != 0)
	if !keepWindow {
		d.killTaskWindow(p.ticketID)
	}

	d.mu.Lock()
	d.setTicketState(p.ticketID, t, p.filePath)

	// Re-enqueue if advance/retry/back.
	if !hookPaused {
		switch kind { //nolint:exhaustive
		case pipeline.ActionAdvance, pipeline.ActionRetry, pipeline.ActionBack:
			d.enqueue(t)
		}
	}
	d.broadcastTicketUpdate(p.ticketID)
	d.mu.Unlock()

	if !hookPaused {
		d.startFinalSummary(ctx, t, p, kind)
	}
}

// startFinalSummary generates the ticket's final summary when the exit ended
// the pipeline with the stage's work accepted. A pipeline can finish either
// way: on_success: done completes the ticket, and any other on_success value
// parks it in that status.
//
// The pass runs on the daemon's context and off the ticket's goroutine,
// because the ticket is finished: it must hold neither a concurrency slot nor
// its running entry while an agent writes prose about it. Only daemon shutdown
// stops it.
func (d *Daemon) startFinalSummary(ctx context.Context, t *ticket.Ticket, p handleExitParams, kind pipeline.ActionKind) {
	terminalSuccess := kind == pipeline.ActionComplete ||
		(kind == pipeline.ActionPark && p.result.ExitCode == 0)
	if !terminalSuccess {
		return
	}
	params := finalSummaryParams{
		log:       p.log,
		cfg:       p.cfg,
		ticketID:  p.ticketID,
		filePath:  p.filePath,
		agentName: p.agentName,
		dir:       p.repoPath,
		tkt:       finalSummaryTicket{Title: t.Title(), Body: t.Body},
		runs:      eligibleFinalSummaryRuns(t),
		status:    t.Status,
	}
	d.background.Go(func() { d.runFinalSummary(ctx, params) })
}

type prepareWorktreeParams struct {
	// cfg is the caller's config snapshot, so the worktree_created hooks a
	// pickup runs come from the same config version as its stage and agent.
	cfg       *config.Config
	log       *slog.Logger
	t         *ticket.Ticket
	filePath  string
	ticketID  string
	stageName string
	// agentName is the agent the caller resolved for this stage. It only
	// reaches the hook environment; the worktree itself does not depend on it.
	agentName string
	// branch is passed in rather than recomputed: runTicket already reserved it
	// in runningBranches, and branch_prefix reloads live, so a second
	// ticketBranch call could return a different name from the one under guard.
	branch     string
	isPipeline bool
}

// prepareWorktreeForAgent resolves the ticket's repo path, creates (or reuses)
// a worktree for the given branch, runs the worktree_created hooks when it
// created one, and writes the branch/last_log fields. On failure the ticket is
// paused and ok=false is returned so the caller can return immediately.
func (d *Daemon) prepareWorktreeForAgent(taskCtx context.Context, p prepareWorktreeParams) (repoPath, wtPath string, ok bool) {
	repoName, repoPath, err := d.resolvePath(p.t)
	if err != nil {
		p.log.Error("resolve path failed", "err", err)
		d.pauseTicket(p.t, p.filePath, "resolve path failed: "+err.Error())
		return "", "", false
	}

	wtPath, created, err := d.worktrees.Create(worktree.CreateOpts{
		RepoPath: repoPath, RepoName: repoName, TaskID: p.ticketID,
		Branch: p.branch, Base: ticketBase(p.t),
	})
	if err != nil {
		p.log.Error("create worktree failed", "path", repoPath, "err", err)
		d.pauseTicket(p.t, p.filePath, "create worktree failed: "+err.Error())
		return "", "", false
	}
	if !created {
		p.log.Info("reusing existing worktree for branch", "path", wtPath, "branch", p.branch)
	} else {
		p.log.Info("worktree created", "path", wtPath, "branch", p.branch)
	}
	if !d.runWorktreeCreatedHooks(taskCtx, p.cfg, p.log, p.t, p.filePath, hook.Context{
		TicketID:   p.ticketID,
		TicketFile: p.filePath,
		Worktree:   wtPath,
		RepoPath:   repoPath,
		Branch:     p.branch,
		Stage:      p.stageName,
		Agent:      p.agentName,
	}, created) {
		return "", "", false
	}

	if err := p.t.SetField("branch", p.branch); err != nil {
		p.log.Error("set field failed", "field", "branch", "err", err)
	}
	if err := p.t.SetField("last_log", d.stageLogPath(p.ticketID, p.stageName)); err != nil {
		p.log.Error("set field failed", "field", "last_log", "err", err)
	}
	// Clear the previous run's summary so the field always describes the run
	// that ended most recently.
	if err := p.t.SetField("summary", ""); err != nil {
		p.log.Error("set field failed", "field", "summary", "err", err)
	}
	// The ticket-level summary covers the runs recorded so far, which this run
	// is about to add to, so it is stale from here until the run ends. Only a
	// pipeline ticket can have one: nothing generates it for the simple path.
	if p.isPipeline {
		if err := p.t.SetField("final_summary", ""); err != nil {
			p.log.Error("set field failed", "field", "final_summary", "err", err)
		}
	}
	if err := d.writeTicket(p.t, p.filePath); err != nil {
		p.log.Error("write failed", "phase", "spawn_fields", "err", err)
		return "", "", false
	}
	return repoPath, wtPath, true
}

type spawnAgentParams struct {
	// cfg is the caller's config snapshot. It is threaded through rather than
	// re-read so the environment and log paths a stage runs with come from the
	// same config version as its agent and stage definitions.
	cfg *config.Config
	// ctx is the daemon's context, not the ticket's. A live ctx when the runner
	// returns means the daemon is still up, so the run ended for a reason of its
	// own and its resume record must go.
	ctx      context.Context
	log      *slog.Logger
	ticketID string
	filePath string
	// agentName is the configured name of agentCfg, and pipelineName the one on
	// the ticket. The agent config alone does not carry either: the resolved
	// agent name lives at the call site. Together they label this run's metrics,
	// and the stage's model is keyed by the agent name, so a map can name one
	// agent among several of the same kind.
	agentName    string
	pipelineName string
	stageName    string
	// run keys this run's structured activity sidecar. It must match the Run
	// stamped on the history entry the same run produces.
	run    int
	wtPath string
	// repoPath and branch describe the worktree wtPath was cut for. They reach
	// the stage hooks only, and an annotation run, which runs no stage hooks,
	// leaves them empty.
	repoPath   string
	branch     string
	rendered   string
	agentCfg   config.Agent
	stageCfg   config.Stage
	isPipeline bool
	// annotation marks the run a submitted set of Plannotator annotations
	// schedules. Such a run continues the session the stage's last run left behind
	// rather than a crash-recovery record.
	annotation bool
}

// sessionStage keys the agent session storage this run uses. An annotation run
// that opens a new conversation stores it apart from the stage's, so that a stage
// recovering from a daemon death cannot resolve to the ticket-rewriting session:
// a pi session file is picked by mtime within the directory. One that continues
// the stage's session appends to the stage's own file, so it reads its log from
// where that file is.
// It takes the record rather than a bool because both wrong answers fail
// silently: a fresh run pointed at the stage's directory is the collision this
// split exists to prevent, and a resumed run pointed at its own leaves the live
// activity view empty.
func (p spawnAgentParams) sessionStage(rec *resumeRecord) string {
	if p.annotation && rec == nil {
		return p.stageName + "-annotation"
	}
	return p.stageName
}

// agentRun is the outcome of one agent invocation: the process result and the
// agent's final assistant message from its session JSONL ("" when no session
// file was written).
type agentRun struct {
	Result       process.Result
	FinalMessage string
	// Resumed reports that the invocation continued a recorded session instead
	// of opening a new conversation.
	Resumed bool
	// Model and Effort are what the invocation passed the agent, after the
	// agent's own defaults. Empty means no flag was passed.
	Model  string
	Effort string
	// SessionKind and SessionRef locate the session JSONL this invocation
	// wrote, in the form ticket history stores. Both are empty for an agent
	// that writes no session Kontora can point at.
	SessionKind string
	SessionRef  string
}

// agentAttempt is the outcome of one runAgentOnce call.
type agentAttempt struct {
	run agentRun
	// ok reports that the run produced a result the caller can act on.
	ok bool
	// started reports that the agent ran. False means the invocation was
	// refused, or the agent died in its first seconds, while the ticket was
	// still live: nothing was done, so a fresh run repeats no work.
	started bool
	// pauseReason, when set, is why the ticket must be paused. The caller
	// pauses, so a refused resume can fall back before the ticket is touched.
	pauseReason string
}

// spawnAgentRun runs the stage's agent and returns its outcome. When a record
// names a session the run may continue, it continues in that session; if that
// invocation dies before the agent does anything, the run happens once more in a
// new session rather than pausing the ticket. On a spawn or runner failure the
// ticket is paused and ok=false is returned so the caller can return
// immediately.
func (d *Daemon) spawnAgentRun(taskCtx context.Context, t *ticket.Ticket, p spawnAgentParams) (agentRun, bool) {
	// One measurement per stage run, taken here rather than inside
	// runAgentOnce: a resumed run that did no work calls runAgentOnce a second
	// time, and a hook there would report two runs for one.
	started := time.Now()
	var attempt agentAttempt
	defer func() { d.recordStageRun(taskCtx, p, attempt, time.Since(started)) }()

	binaryPath, err := d.agentLookup(p.agentCfg.Binary)
	if err != nil {
		p.log.Error("agent binary lookup failed", "binary", p.agentCfg.Binary, "err", err)
		attempt.pauseReason = fmt.Sprintf("agent binary unavailable: %s", err)
		d.pauseTicket(t, p.filePath, attempt.pauseReason)
		return agentRun{}, false
	}

	// An annotation run goes through here but is not a stage: it rewrites the
	// ticket and does none of the stage's work.
	if !p.annotation {
		hookErr := d.runHooks(taskCtx, p.cfg, p.log, hook.Context{
			Event:      config.HookStageStart,
			TicketID:   p.ticketID,
			TicketFile: p.filePath,
			Worktree:   p.wtPath,
			RepoPath:   p.repoPath,
			Branch:     p.branch,
			Stage:      p.stageName,
			Agent:      p.agentName,
		})
		if hookErr != nil {
			if taskCtx.Err() == nil {
				attempt.pauseReason = hookErr.Error()
				d.pauseTicket(t, p.filePath, attempt.pauseReason)
			}
			return agentRun{}, false
		}
	}

	if rec := d.resumableRecord(p); rec != nil {
		attempt = d.runAgentOnce(taskCtx, t, p, binaryPath, rec)
		if attempt.started {
			return d.finishAttempt(taskCtx, t, p, attempt)
		}
		// An agent that already finished its work can exit inside the tmux
		// startup guard, which reads as a refused invocation. Give the stage its
		// normal run rather than pausing on that.
		p.log.Warn("resumed agent did no work; running fresh", "stage", p.stageName)
		// An annotation run borrows the stage's record and must not retire it: the
		// stage may still need it to recover from a daemon death.
		if !p.annotation {
			d.removeResumeRecord(p.cfg, p.ticketID, p.stageName)
		}
	}

	attempt = d.runAgentOnce(taskCtx, t, p, binaryPath, nil)
	return d.finishAttempt(taskCtx, t, p, attempt)
}

// recordStageRun reports one finished stage run. A run that ends while the
// ticket's context is cancelled is neither a success nor a failure of the
// stage, so it is reported as cancelled; anything that pauses the ticket or
// exits non-zero is a failure, including a runner that never produced an exit
// code.
//
// An annotation run comes through here under the stage's own name, so it is
// marked as one: the pipeline never evaluated that stage and it did none of the
// stage's work, and counting it as a run would mix a ticket rewrite into the
// stage's counts and durations.
func (d *Daemon) recordStageRun(taskCtx context.Context, p spawnAgentParams, attempt agentAttempt, elapsed time.Duration) {
	outcome := metrics.OutcomeFailure
	switch {
	case taskCtx.Err() != nil:
		outcome = metrics.OutcomeCancelled
	case attempt.ok && attempt.pauseReason == "" && attempt.run.Result.ExitCode == 0:
		outcome = metrics.OutcomeSuccess
	}
	d.metrics.StageRun(taskCtx, metrics.StageAttrs{
		Stage:      p.stageName,
		Agent:      p.agentName,
		Pipeline:   p.pipelineName,
		Outcome:    outcome,
		ExitCode:   attempt.run.Result.ExitCode,
		Annotation: p.annotation,
	}, elapsed)
}

// finishAttempt applies what an attempt decided. An attempt the caller cannot
// act on ends the stage here rather than at handleAgentExit, so it fires the
// stage_end hooks itself: the stage_start hooks have already run, and a pair
// left unmatched is exactly the case a teardown hook exists for.
func (d *Daemon) finishAttempt(taskCtx context.Context, t *ticket.Ticket, p spawnAgentParams, attempt agentAttempt) (agentRun, bool) {
	if !attempt.ok {
		d.runStageEndHooksAfterFailure(taskCtx, p, attempt)
	}
	if attempt.pauseReason != "" {
		d.pauseTicket(t, p.filePath, attempt.pauseReason)
	}
	return attempt.run, attempt.ok
}

// runAgentOnce builds agent args, invokes the runner, materializes session logs,
// and logs exit info. A non-nil rec continues that record's session. The resume
// prompt replaces the stage prompt, except for an annotation run, whose
// annotations are the only instruction it has.
func (d *Daemon) runAgentOnce(taskCtx context.Context, t *ticket.Ticket, p spawnAgentParams, binaryPath string, rec *resumeRecord) agentAttempt {
	rendered := p.rendered
	if rec != nil {
		if !p.annotation {
			resumePrompt, err := d.buildResumePrompt(t, p)
			if err != nil {
				p.log.Error("render resume prompt failed", "stage", p.stageName, "err", err)
				return agentAttempt{}
			}
			rendered = resumePrompt
		}
		p.log.Info("resuming agent session", "stage", p.stageName, "agent", rec.Agent,
			"session_id", rec.SessionID, "annotation", p.annotation)
	}

	model := p.stageCfg.Model.For(p.agentName, p.agentCfg)
	effort := p.stageCfg.Effort.For(p.agentName, p.agentCfg)
	// What the run is recorded as having used. The stage-resolved pair is what
	// buildAgentArgs takes; the effective pair is what actually reaches the CLI
	// once the agent's own defaults apply.
	effModel, effEffort := p.agentCfg.Effective(model, effort)
	// Only a Claude stage run is driven through phase boundaries: pi does its own
	// compaction from the extension, and an annotation run rewrites the ticket
	// rather than working through its phases.
	var ckpt checkpointSetup
	if !p.annotation && checkpointKind(p.agentCfg) == config.AgentKindClaude {
		ckpt = checkpointSetup{
			sidecar:   checkpointsPath(p.cfg, p.ticketID, p.stageName, p.run),
			threshold: p.agentCfg.CheckpointCompactionTokens,
			log:       p.log,
		}
		if ckpt.enabled() {
			// The stage and the run index scope the channel to this run, so a
			// compaction a previous one left latched is not read as this one's.
			ckpt.compactChannel = tmux.CompactChannelName(d.tmuxSession, p.ticketID,
				fmt.Sprintf("%s-%d", p.stageName, p.run))
		}
	}
	args, settingsFile, sessionID, err := buildAgentArgs(p.agentCfg, rendered, tmux.ChannelName(d.tmuxSession, p.ticketID), ckpt.compactChannel, model, effort, rec, !p.annotation)
	if err != nil {
		p.log.Error("build agent args failed", "err", err)
		return agentAttempt{pauseReason: "build agent args failed: " + err.Error()}
	}
	if settingsFile != "" {
		defer os.Remove(settingsFile)
	}

	// An annotation run runs under the stage's name, so a record of its own here
	// would take the stage's path: it would retire a record that still marks an
	// interrupted stage, or leave one behind that a later stage run would resume
	// into a conversation about rewriting the ticket.
	if kind := resumeAgentKind(p.agentCfg); kind != "" && !p.annotation {
		d.writeResumeRecord(p, kind, sessionID)
	}

	params := d.buildRunnerParams(p.cfg, p.agentCfg, p.stageCfg, binaryPath, args, p.wtPath, p.ticketID, p.stageName, p.sessionStage(rec), sessionID, ckpt)
	// An annotation run continues the session the stage already finished, and
	// Claude's --resume and pi's --session <path> both append to that same
	// JSONL, so the totals read once it returns carry the tokens the stage
	// already reported. Take what is in the file now and report only what this
	// run adds. It is read here, rather than remembered from the last run,
	// because the run that reported them can have been a previous daemon's.
	//
	// A crash-recovery resume is the opposite case: the invocation that spent
	// those tokens died before anything could report them, so continuing it
	// reports the file's totals whole.
	scope := runScope{startedAt: time.Now()}
	if rec != nil && p.annotation {
		scope.prior = sessionUsageTotals(params)
	}
	// The clear is deferred so that every way this function can end, including a
	// runner error or a cancelled ticket, leaves no entry claiming the run is
	// still going.
	d.setLiveRun(p.ticketID, liveRun{stage: p.stageName, agent: p.agentName, run: p.run, params: params, startedAt: scope.startedAt})
	defer d.clearLiveRun(p.ticketID)
	// Stage logs are appended across retries (pipe-pane, DirectRunner), so record
	// where this run's output starts. Failure-pattern detection only scans from
	// here, otherwise a previous attempt's error would keep matching on retry.
	logStart := fileSize(params.LogFile)
	eventsFile := stageEventsPath(p.cfg, p.ticketID, p.stageName, p.run)
	result, runnerErr := d.runner(taskCtx, params)
	// The record exists to mark a run the daemon never saw end. Once the runner
	// returns with the daemon still up, the run has ended for a reason of its
	// own — clean exit, failure, pause, or user cancellation — and starting it
	// over later must start it fresh. Only a shutdown, or a kill that runs no
	// code at all, leaves the record behind.
	if p.ctx.Err() == nil && !p.annotation {
		// Promoted on any exit code: a stage that failed still holds the
		// conversation about this ticket.
		d.promoteResumeRecord(p)
	}
	if runnerErr != nil && taskCtx.Err() == nil {
		usage, complete := d.materializeAgentLogs(p.log, params, eventsFile, scope)
		d.recordTokens(taskCtx, p.stageName, p.agentName, usage, complete)
		errAttrs := []any{"stage", p.stageName, "err", runnerErr}
		if tail := tailFile(params.LogFile); tail != "" {
			errAttrs = append(errAttrs, "output", tail)
		}
		p.log.Error("runner failed", errAttrs...)
		d.killTaskWindow(p.ticketID)
		// A runner can fail after a complete turn, and that turn wrote a
		// session, so the reference is recorded on this path too.
		failKind, failRef := runSessionRef(p.cfg, p.ticketID, params, scope.startedAt)
		return agentAttempt{
			run: agentRun{
				Result: result, Resumed: rec != nil, Model: effModel, Effort: effEffort,
				SessionKind: failKind, SessionRef: failRef,
			},
			started:     agentDidWork(result),
			pauseReason: fmt.Sprintf("runner failed: %s", runnerErr.Error()),
		}
	}

	usage, usageComplete := d.materializeAgentLogs(p.log, params, eventsFile, scope)
	d.recordTokens(taskCtx, p.stageName, p.agentName, usage, usageComplete)

	sessionKind, sessionRef := runSessionRef(p.cfg, p.ticketID, params, scope.startedAt)
	run := agentRun{
		Result:       result,
		FinalMessage: finalAssistantMessage(p.log, params, scope.startedAt),
		Resumed:      rec != nil,
		Model:        effModel,
		Effort:       effEffort,
		SessionKind:  sessionKind,
		SessionRef:   sessionRef,
	}

	dur := result.ExitedAt.Sub(result.StartedAt).Truncate(time.Second)
	attrs := []any{"stage", p.stageName, "exit_code", result.ExitCode, "duration", dur}
	if result.ExitCode != 0 {
		if tail := tailFile(params.LogFile); tail != "" {
			attrs = append(attrs, "output", tail)
		}
	}
	if runnerErr != nil {
		attrs = append(attrs, "err", runnerErr)
	}
	p.log.Info("agent exited", attrs...)

	// A clean exit can still hide a failed run: Claude ends the turn (firing the
	// Stop hook, exit 0) after a quota/usage limit or API error, so the pipeline
	// would read exit 0 as success and advance the ticket. Detect that and pause
	// instead. Non-zero exits already flow through the pipeline's on_failure
	// handling, so leave those alone. Skip when the run was cancelled.
	if result.ExitCode == 0 && taskCtx.Err() == nil {
		if reason, kind, detected := d.detectAgentError(p.agentCfg, params, logStart, scope.startedAt); detected {
			p.log.Warn("agent error detected despite clean exit", "stage", p.stageName, "reason", reason, "kind", kind)
			d.metrics.AgentError(taskCtx, p.stageName, p.agentName, kind)
			d.killTaskWindow(p.ticketID)
			return agentAttempt{run: run, started: true, pauseReason: "agent error: " + reason}
		}
	}

	return agentAttempt{run: run, ok: true, started: true}
}

// agentDidWork reports whether a failed invocation lasted long enough for the
// agent to have done something. The runner can fail after a complete turn: a
// tmux wait-for that breaks, or a /exit keystroke it cannot send once the Stop
// hook has already fired. Running the stage again after that repeats work the
// worktree already holds, so only a failure inside the startup window is
// eligible for a fresh run.
func agentDidWork(result process.Result) bool {
	if result.StartedAt.IsZero() {
		return false
	}
	return result.ExitedAt.Sub(result.StartedAt) >= tmux.MinInteractiveDuration
}

// buildResumePrompt renders the configured or built-in resume prompt with the
// same operational appendix a stage prompt gets.
func (d *Daemon) buildResumePrompt(t *ticket.Ticket, p spawnAgentParams) (string, error) {
	tmpl := p.cfg.ResumePrompt
	if tmpl == "" {
		tmpl = defaultResumePrompt
	}
	rendered, err := d.renderTicketPrompt(p.cfg, tmpl, t, p.filePath, p.wtPath)
	if err != nil {
		return "", err
	}
	if rendered != "" {
		rendered += buildOperationalAppendix(t.ID, p.filePath, p.wtPath, p.isPipeline, resumeCheckpointKind(p))
	}
	return rendered, nil
}

// runSummary returns the summary for a finished run: the text the agent wrote
// on the ticket during the run, else the agent's final assistant message,
// capped for frontmatter.
func runSummary(agentWritten, finalMessage string) string {
	if agentWritten == "" {
		agentWritten = finalMessage
	}
	return truncateSummary(agentWritten)
}

// summaryMaxLen caps a stored stage summary so a long final assistant message
// does not bloat ticket frontmatter.
const summaryMaxLen = 4000

// truncateSummary caps s at summaryMaxLen bytes, cutting on a rune boundary
// and marking the cut.
func truncateSummary(s string) string {
	return truncate(s, summaryMaxLen, "\n[truncated]")
}

// truncate caps s at limit bytes, cutting on a rune boundary so the result stays
// valid UTF-8, and appends marker when it cut.
func truncate(s string, limit int, marker string) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// buildOperationalAppendix returns a context block appended to every rendered prompt.
// It gives agents the ticket ID, file paths, and CLI commands they need so they
// don't have to search $HOME for context.
func buildOperationalAppendix(taskID, filePath, wtPath string, isPipeline bool, checkpointKind string) string {
	var b strings.Builder
	b.WriteString("\n\n## Operational Context\n")
	fmt.Fprintf(&b, "- Ticket ID: %s\n", taskID)
	fmt.Fprintf(&b, "- Ticket file: %s\n", filePath)
	fmt.Fprintf(&b, "- Worktree: %s\n", wtPath)
	fmt.Fprintf(&b, "- `kontora note %s \"...\"` — appends a timestamped note\n", taskID)
	fmt.Fprintf(&b, "- `kontora view %s` — prints ticket contents to stdout\n", taskID)
	b.WriteString("- Do not search $HOME for tickets or config; use the paths above.\n")
	fmt.Fprintf(&b, "\nWhen you finish, record a few-line summary of what this run did with:\n  kontora summary %s \"...\"\n", taskID)
	if isPipeline {
		fmt.Fprintf(&b, "\nIMPORTANT: When you finish your work, write your results as a note on the ticket. Include all relevant details — the next stage of the pipeline will read this note to continue the work. Use:\n  kontora note %s \"your results here\"", taskID)
	}
	if checkpointKind != "" {
		b.WriteString("\n\n## Phase checkpoints\n")
		b.WriteString("At each boundary between top-level ticket phases:\n")
		b.WriteString("1. Run the tests for the completed phase.\n")
		fmt.Fprintf(&b, "2. Write a `phase-N:` note with `kontora note %s \"...\"`.\n", taskID)
		b.WriteString("3. In the note, list changed files, decisions, test results, unresolved issues, and the next phase.\n")
		if checkpointKind == config.AgentKindClaude {
			fmt.Fprintf(&b, "4. Run `kontora phase-complete %s --completed \"<the phase that just finished>\" --next \"<the phase to begin next>\"`, then end your turn. Do not start the next phase yourself: kontora compacts the context if it needs to and then prompts you to continue.\n", taskID)
			b.WriteString("Do not run the command after the final phase.\n")
		} else {
			b.WriteString("4. Call `kontora_phase_complete` with `completed_phase` and `next_phase`.\n")
			b.WriteString("Do not call the tool after the final phase.\n")
		}
	}
	return b.String()
}

// buildAgentArgs constructs the argument list for an agent invocation.
// For Claude agents it injects --settings with a Notification hook that
// signals tmux wait-for on idle_prompt, and --session-id for session JSONL
// logging. A non-empty compactChannel adds the PostCompact hook a checkpointing
// run needs.
// For pi agents it injects -e with the Kontora extension that handles clean
// shutdown on agent_settled and, when checkpointEligible is true and the agent
// carries a positive CheckpointCompactionTokens, registers the
// kontora_phase_complete tool for phase-boundary compaction.
// A non-nil rec attaches the run to the session that record names instead of
// opening a new one.
// A non-empty model or effort replaces the one in the agent's own arguments,
// which keeps it ahead of the prompt this function appends last. A pair the
// agent cannot run is rejected here as well as in config validation, because a
// ticket's own agent field is only known at spawn time.
// Returns the args, the path to the temporary settings/extension file (empty
// for other agents), the session ID (empty for non-Claude agents), and any error.
func buildAgentArgs(agentCfg config.Agent, rendered, channelName, compactChannel, model, effort string, rec *resumeRecord, checkpointEligible bool) ([]string, string, string, error) {
	if err := agentCfg.CheckEffort(model, effort); err != nil {
		return nil, "", "", err
	}
	args := agentCfg.ArgsWith(model, effort)
	var settingsFile string
	var sessionID string
	switch {
	case agentCfg.IsClaude():
		var err error
		settingsFile, err = writeHooksSettings(channelName, compactChannel)
		if err != nil {
			return nil, "", "", fmt.Errorf("writing hooks settings: %w", err)
		}
		args = append(args, "--settings", settingsFile)
		if rec != nil {
			// --resume appends to the recorded session's JSONL, so the ID stays
			// the one log materialization and error detection already read.
			sessionID = rec.SessionID
			args = append(args, "--resume", sessionID)
		} else {
			sessionID = newSessionID()
			args = append(args, "--session-id", sessionID)
		}
	case agentCfg.IsPi():
		threshold := agentCfg.CheckpointCompactionTokens
		var err error
		settingsFile, err = writePiExtension(threshold, checkpointEligible && threshold > 0)
		if err != nil {
			return nil, "", "", fmt.Errorf("writing pi extension: %w", err)
		}
		args = append(args, "-e", settingsFile)
		if rec != nil {
			args = append(args, "--session", rec.SessionPath)
		}
	default:
		if model != "" {
			return nil, "", "", fmt.Errorf("model %q: agent %s takes no --model", model, agentCfg.Binary)
		}
		if effort != "" {
			return nil, "", "", fmt.Errorf("effort %q: agent %s takes no reasoning effort flag", effort, agentCfg.Binary)
		}
	}
	if rendered != "" {
		args = append(args, rendered)
	}
	return args, settingsFile, sessionID, nil
}

// writeHooksSettings creates a temporary JSON settings file with hooks that
// signal the given tmux wait-for channel when Claude finishes. Stop fires
// immediately when Claude finishes responding; Notification+idle_prompt is
// a fallback for when Claude goes idle without a clean Stop.
//
// A non-empty compactChannel adds a PostCompact hook on its own channel, which
// is how the runner learns that a /compact it typed has landed. It is set only
// for a checkpointing run, so every other run's settings file is unchanged.
func writeHooksSettings(channelName, compactChannel string) (string, error) {
	waitCmd := fmt.Sprintf("tmux wait-for -S %s", channelName)
	hooks := fmt.Sprintf(`"Stop":[{"matcher":"","hooks":[{"type":"command","command":"%s"}]}],"Notification":[{"matcher":"idle_prompt","hooks":[{"type":"command","command":"%s"}]}]`, waitCmd, waitCmd)
	if compactChannel != "" {
		hooks += fmt.Sprintf(`,"PostCompact":[{"matcher":"","hooks":[{"type":"command","command":"tmux wait-for -S %s"}]}]`, compactChannel)
	}
	settings := `{"hooks":{` + hooks + `}}`
	f, err := os.CreateTemp("", "kontora-settings-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(settings); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// resumeCheckpointKind is checkpointKind for a resumed run: an annotation run
// rewrites the ticket rather than working through its phases, so it gets no
// checkpoint protocol even when its agent carries a threshold.
func resumeCheckpointKind(p spawnAgentParams) string {
	if p.annotation {
		return ""
	}
	return checkpointKind(p.agentCfg)
}

// checkpointKind reports which agent's phase-checkpoint protocol a run follows:
// pi drives it from an extension, claude from the `kontora phase-complete`
// command and the daemon's idle decision point. It returns "" when the agent
// has no positive threshold, or is one that carries no protocol at all.
func checkpointKind(agentCfg config.Agent) string {
	if agentCfg.CheckpointCompactionTokens <= 0 {
		return ""
	}
	return agentCfg.Kind()
}

// sessionStage keys the agent's own session storage, which is the stage name for
// every run but a fresh annotation one (see spawnAgentParams.sessionStage).
func (d *Daemon) buildRunnerParams(cfg *config.Config, agentCfg config.Agent, stageCfg config.Stage, binaryPath string, args []string, dir, ticketID, stageName, sessionStage, sessionID string, ckpt checkpointSetup) RunnerParams {
	var sessionDir string
	if agentCfg.IsPi() {
		sessionDir = piSessionDir(cfg, ticketID, sessionStage)
		args = append(args, "--session-dir", sessionDir)
	}

	env := agentEnv(cfg, agentCfg, d.configPath)
	var onIdle func(context.Context, tmux.IdleEvent) tmux.IdleDecision
	if ckpt.enabled() {
		env[cli.CheckpointFileEnvVar] = ckpt.sidecar
		onIdle = newCheckpointController(ckpt, env, sessionID).onIdle
	}

	return RunnerParams{
		Binary:         binaryPath,
		Args:           args,
		Dir:            dir,
		Timeout:        stageCfg.Timeout.Duration,
		TicketID:       ticketID,
		SessionName:    d.tmuxSession,
		LogFile:        stageLogPath(cfg, ticketID, stageName),
		Interactive:    agentCfg.IsClaude(),
		SessionID:      sessionID,
		SessionDir:     sessionDir,
		Env:            env,
		OnIdle:         onIdle,
		CompactChannel: ckpt.compactChannel,
		OnReady: func() {
			d.broadcastTerminalReady(ticketID)
		},
	}
}

// agentEnv merges the agent's environment over the top-level one. An agent entry
// with an empty value unsets the variable rather than setting it to "".
//
// configPath is exported as KONTORA_CONFIG first, so the `kontora note` and
// `kontora summary` calls the prompt asks for reach the same config the daemon
// runs on. Without it they re-derive a path from the worktree and $HOME, which
// is the wrong file whenever the daemon was started with --config. A user who
// sets the variable in their own environment config still wins.
func agentEnv(cfg *config.Config, agentCfg config.Agent, configPath string) map[string]string {
	env := make(map[string]string, len(cfg.Environment)+len(agentCfg.Environment)+1)
	if configPath != "" {
		env[config.PathEnvVar] = configPath
	}
	maps.Copy(env, cfg.Environment)
	for k, v := range agentCfg.Environment {
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	return env
}

func (d *Daemon) applyAction(t *ticket.Ticket, action pipeline.Action) error {
	for _, f := range action.Fields {
		if err := t.SetField(f.Key, f.Value); err != nil {
			return fmt.Errorf("set %s: %w", f.Key, err)
		}
	}
	if action.History != nil {
		history := t.History
		history = append(history, *action.History)
		if err := t.SetField("history", history); err != nil {
			return fmt.Errorf("set history: %w", err)
		}
	}
	return nil
}

func (d *Daemon) resolvePath(t *ticket.Ticket) (repoName, repoPath string, err error) {
	if t.Path == "" {
		return "", "", fmt.Errorf("ticket %s has no path set", t.ID)
	}
	repoPath = expandTilde(t.Path)
	repoName = filepath.Base(repoPath)
	return repoName, repoPath, nil
}

func (d *Daemon) writeTicket(t *ticket.Ticket, path string) error {
	modTime, err := d.writeTicketFile(t, path)
	if err != nil {
		return err
	}
	// Refresh the cached modtime so UpdatedAt reflects this write. Paths that
	// reuse the ticketState in place (pickup, spawn) rely on this; reconstruct
	// paths reuse the refreshed modtime via setTicketState to avoid a second
	// stat. Caller must not hold d.mu.
	d.mu.Lock()
	d.refreshTicketModTimeLocked(t.ID, modTime)
	d.mu.Unlock()
	return nil
}

// writeTicketLocked is writeTicket for callers that already hold d.mu.
func (d *Daemon) writeTicketLocked(t *ticket.Ticket, path string) error {
	modTime, err := d.writeTicketFile(t, path)
	if err != nil {
		return err
	}
	d.refreshTicketModTimeLocked(t.ID, modTime)
	return nil
}

func (d *Daemon) writeTicketFile(t *ticket.Ticket, path string) (time.Time, error) {
	data, err := t.Marshal()
	if err != nil {
		return time.Time{}, err
	}
	d.recordSelfWrite(path, data)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return time.Time{}, err
	}
	if st, err := os.Stat(path); err == nil {
		return st.ModTime(), nil
	}
	return time.Time{}, nil
}

func (d *Daemon) refreshTicketModTimeLocked(ticketID string, modTime time.Time) {
	if modTime.IsZero() {
		return
	}
	if ts, ok := d.tickets[ticketID]; ok {
		ts.modTime = modTime
	}
}

// setTicketState replaces the cached state for id after a writeTicket call,
// carrying over the modtime writeTicket just refreshed so the swap doesn't
// stat the file a second time, and reconciles the queue against the new state.
// Every write that can close a ticket, or change what it waits on, goes through
// here, so putting the reconciliation in one place is what keeps the paths that
// close a ticket without the pipeline exit handler from stranding its
// dependents. Must be called with d.mu held, on a path that just wrote the file
// (so the cached modtime is current).
func (d *Daemon) setTicketState(id string, t *ticket.Ticket, path string) {
	var modTime time.Time
	if ts, ok := d.tickets[id]; ok {
		modTime = ts.modTime
	}
	d.tickets[id] = &ticketState{ticket: t, filePath: path, modTime: modTime}
	d.reconcileDependenciesLocked(id)
}

func (d *Daemon) stageLogPath(ticketID, stageName string) string {
	return stageLogPath(d.config(), ticketID, stageName)
}

func stageLogPath(cfg *config.Config, ticketID, stageName string) string {
	return session.LogPath(expandTilde(cfg.LogsDir), ticketID, stageName)
}

// stageEventsPath is the structured activity sidecar for one run of a stage.
// It sits beside <stage>.log; the .json suffix keeps it out of every existing
// log-directory scanner, all of which filter on .log.
func stageEventsPath(cfg *config.Config, ticketID, stageName string, run int) string {
	return session.EventsPath(expandTilde(cfg.LogsDir), ticketID, stageName, run)
}

// stageRunIndex returns the zero-based key for the next run of stageName,
// counting the runs of that stage already recorded in history. It is not
// derived from t.Attempt, which ActionBack resets to 0.
func stageRunIndex(t *ticket.Ticket, stageName string) int {
	n := 0
	for _, h := range t.History {
		if h.Stage == stageName {
			n++
		}
	}
	return n
}

func (d *Daemon) pauseTicket(t *ticket.Ticket, path, reason string) {
	log := d.ticketLog(t.ID)
	log.Warn("pausing", "reason", reason)
	if reason != "" {
		t.AppendNote(reason, time.Now())
	}
	if err := t.SetField("last_error", reason); err != nil {
		log.Error("pause: set last_error failed", "err", err)
	}
	if err := t.SetField("status", string(ticket.StatusPaused)); err != nil {
		log.Error("pause: set status failed", "err", err)
	}
	if err := d.writeTicket(t, path); err != nil {
		log.Error("pause: write failed", "err", err)
	}
	d.mu.Lock()
	d.setTicketState(t.ID, t, path)
	d.broadcastTicketUpdate(t.ID)
	d.mu.Unlock()
}

// selfWrite is what the daemon knows about its own last write to a ticket
// file. sum identifies the bytes it wrote; blind marks a write whose content
// was not known when it was announced.
type selfWrite struct {
	sum   [sha256.Size]byte
	blind bool
}

// recordSelfWrite remembers the exact bytes the daemon is writing to path.
// Watcher events whose content matches them are its own and are ignored.
func (d *Daemon) recordSelfWrite(path string, data []byte) {
	d.selfWritesMu.Lock()
	defer d.selfWritesMu.Unlock()
	d.selfWrites[path] = selfWrite{sum: sha256.Sum256(data)}
}

// recordSelfWriteBlind suppresses the next event for path whatever it holds.
// It is for the one write whose content the daemon cannot know in advance:
// ticket creation, which happens inside the CLI package.
func (d *Daemon) recordSelfWriteBlind(path string) {
	d.selfWritesMu.Lock()
	defer d.selfWritesMu.Unlock()
	d.selfWrites[path] = selfWrite{blind: true}
}

func (d *Daemon) forgetSelfWrite(path string) {
	d.selfWritesMu.Lock()
	defer d.selfWritesMu.Unlock()
	delete(d.selfWrites, path)
}

// isSelfWrite reports whether the file at path still holds the bytes the
// daemon wrote. The record survives a match, because one debounced event can
// stand for several daemon writes; the first event whose content differs
// drops it and is handled as the external edit it is.
//
// An external edit that restores the daemon's own bytes is read as a self
// write. Ignoring it changes nothing: the file already says what the daemon
// believes it says.
func (d *Daemon) isSelfWrite(path string) bool {
	d.selfWritesMu.Lock()
	rec, ok := d.selfWrites[path]
	if ok && rec.blind {
		// A blind record answers for one event and is spent by it; keeping it
		// would leave a hash no file can match.
		delete(d.selfWrites, path)
	}
	d.selfWritesMu.Unlock()
	if !ok {
		return false
	}
	if rec.blind {
		return true
	}

	// Read outside the lock. This runs on the watcher goroutine, and every
	// daemon write takes the same mutex, so a slow read holding it would stall
	// the scheduler and the web API behind a disk.
	data, err := os.ReadFile(path)
	if err != nil {
		// Unreadable now says nothing about who wrote it, so the record stays
		// for the next event to match.
		return false
	}
	if sha256.Sum256(data) == rec.sum {
		return true
	}

	d.selfWritesMu.Lock()
	// Drop the record only when it is still the one just checked: a daemon
	// write since then has bytes of its own waiting to be matched.
	if cur, ok := d.selfWrites[path]; ok && cur == rec {
		delete(d.selfWrites, path)
	}
	d.selfWritesMu.Unlock()
	return false
}

func (d *Daemon) killAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, cancel := range d.running {
		d.ticketLog(id).Info("killing agent", "reason", "shutdown")
		cancel()
	}
	for id, cancel := range d.plannotator {
		d.ticketLog(id).Info("killing plannotator", "reason", "shutdown")
		cancel()
	}
}

func (d *Daemon) killTaskWindow(ticketID string) {
	if tmux.HasWindow(d.tmuxSession, ticketID) {
		_ = tmux.KillWindow(d.tmuxSession, ticketID)
	}
}

// partitionWindows sorts tmux window names by ownership. A window is this
// daemon's when it is named after a ticket in its own tickets_dir that no other
// instance has claimed — the same boundary crash recovery uses.
func (d *Daemon) partitionWindows(windows []string) (mine, foreign []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range windows {
		ts, known := d.tickets[name]
		if known && !d.claimedElsewhere(ts.ticket) {
			mine = append(mine, name)
		} else {
			foreign = append(foreign, name)
		}
	}
	return mine, foreign
}

// cleanOrphanedWindows kills the tmux windows a previous run of this daemon
// left behind. Windows belonging to another daemon sharing the session survive:
// killing them would strand its agents mid-stage.
//
// A window named after no ticket in this tickets_dir looks the same as another
// daemon's, so it survives too. Deleting a ticket while the daemon is down
// therefore leaks its window; telling the two apart needs a per-window owner
// tag, which giving each daemon its own session makes unnecessary.
func (d *Daemon) cleanOrphanedWindows() {
	windows, err := d.windows.list(d.tmuxSession)
	if err != nil {
		d.log.Error("listing orphaned tmux windows", "err", err)
		return
	}
	mine, foreign := d.partitionWindows(windows)
	for _, name := range foreign {
		d.log.Debug("keeping tmux window this daemon does not own", "window", name, "tmux_session", d.tmuxSession)
	}
	for _, name := range mine {
		d.log.Warn("killing orphaned tmux window", "ticket", name, "tmux_session", d.tmuxSession)
		if err := d.windows.kill(d.tmuxSession, name); err != nil {
			d.log.Error("killing tmux window", "window", name, "err", err)
		}
	}
	d.log.Info("tmux window cleanup done", "killed", len(mine), "kept", len(foreign), "tmux_session", d.tmuxSession)
}

// Ticket queue implementation (FIFO by creation time).

type queueItem struct {
	ticketID string
	// created orders the queue: the oldest ticket runs first. It is the ticket's
	// own creation date, so it says nothing about how long this item has waited.
	created time.Time
	// enqueuedAt is when the item joined the queue: the wait the queue-wait
	// histogram and the Stats page report.
	enqueuedAt time.Time
}

type priorityQueue []*queueItem

func (pq priorityQueue) Len() int { return len(pq) }

// Less orders by creation time, oldest first. Ticket ID breaks a tie, so two
// tickets created in the same second always run in the same order.
func (pq priorityQueue) Less(i, j int) bool {
	if !pq[i].created.Equal(pq[j].created) {
		return pq[i].created.Before(pq[j].created)
	}
	return pq[i].ticketID < pq[j].ticketID
}

func (pq priorityQueue) Swap(i, j int) { pq[i], pq[j] = pq[j], pq[i] }

func (pq *priorityQueue) Push(x any) {
	*pq = append(*pq, x.(*queueItem))
}

func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*pq = old[:n-1]
	return item
}

// claudeSessionFiles globs the Claude config dir for the JSONL files of the
// given session.
func claudeSessionFiles(env map[string]string, sessionID string) (matches []string, pattern string, err error) {
	return session.ClaudeFiles(expandTilde(session.ClaudeConfigDir(env)), sessionID)
}

// sessionFile locates the session JSONL of a run: the Claude session file
// matching params.SessionID, or the newest file in the pi params.SessionDir
// that pi touched at or after since. A retried stage reuses its session
// directory, and pi only creates a file once the model has replied, so a run
// that died before its first reply leaves the directory holding the previous
// attempt's file alone. Without the floor that file would be read as this
// run's: its transcript, its final message and its tokens, all reported twice.
// Pass the zero time to ask what the directory held before the run started.
// Returns "" when the run has no session or no file was written.
func sessionFile(params RunnerParams, since time.Time) (path string, isPi bool) {
	switch {
	case params.SessionID != "":
		if matches, _, err := claudeSessionFiles(params.Env, params.SessionID); err == nil && len(matches) > 0 {
			return matches[0], false
		}
		return "", false
	case params.SessionDir != "":
		return newestSessionFileSince(params.SessionDir, since), true
	}
	return "", false
}

// liveSessionFile picks the JSONL the in-flight run is appending to, and
// reports whether it is a pi session. It is not sessionFile: pi reuses one
// session directory per stage, so until pi creates this attempt's file the
// newest one there belongs to the previous attempt. An agent with neither
// locator, such as one driven by DirectRunner, has no session file at all.
func liveSessionFile(lr liveRun) (path string, isPi bool) {
	switch {
	case lr.params.SessionID != "":
		if matches, _, err := claudeSessionFiles(lr.params.Env, lr.params.SessionID); err == nil && len(matches) > 0 {
			return matches[0], false
		}
		return "", false
	case lr.params.SessionDir != "":
		// The directory the run was given, not the one its stage name implies: an
		// annotation run that opened a new session writes outside the stage's.
		return newestSessionFileSince(lr.params.SessionDir, lr.startedAt), true
	}
	return "", false
}

// sessionTape parses the session JSONL at path into a tape.
func sessionTape(path string, isPi bool) (logfmt.Tape, error) {
	src, err := os.Open(path)
	if err != nil {
		return logfmt.Tape{}, fmt.Errorf("open session file: %w", err)
	}
	defer src.Close()

	var tape logfmt.Tape
	if isPi {
		tape, err = logfmt.EventsPi(src)
	} else {
		tape, err = logfmt.Events(src)
	}
	if err != nil {
		return logfmt.Tape{}, fmt.Errorf("parse session JSONL: %w", err)
	}
	return tape, nil
}

// runScope separates one agent invocation from the session file it can share
// with earlier ones. startedAt discards a file written before this run, and
// prior is what the file already held when it started, which a run resuming
// that session must not report as its own spend.
type runScope struct {
	startedAt time.Time
	prior     logfmt.Usage
}

// materializeAgentLogs materializes session logs for agents that write
// structured JSONL (Claude via SessionID, pi via SessionDir). It also writes
// the structured activity sidecar for the same session file to eventsFile.
//
// It returns the invocation's token totals, and whether they can be trusted. A
// tape that declares its usage partial counts only the records it could read,
// which is not what the run spent, so the caller must record nothing for it.
//
// A sidecar failure only warns: the plaintext log is the contract every
// existing reader depends on and must never be lost because the sidecar
// could not be written.
func (d *Daemon) materializeAgentLogs(log *slog.Logger, params RunnerParams, eventsFile string, scope runScope) (logfmt.Usage, bool) {
	if params.SessionID == "" && params.SessionDir == "" {
		return logfmt.Usage{}, false
	}
	path, isPi := sessionFile(params, scope.startedAt)
	if path == "" {
		log.Warn("session JSONL not found", "session_id", params.SessionID, "session_dir", params.SessionDir)
		return logfmt.Usage{}, false
	}
	if err := materializeSessionLog(path, isPi, params.LogFile); err != nil {
		log.Warn("session log materialization failed", "session_file", path, "err", err)
		return logfmt.Usage{}, false
	}
	log.Info("session log materialized", "session_file", path, "log_file", params.LogFile)

	if eventsFile == "" {
		return logfmt.Usage{}, false
	}
	tape, err := materializeSessionEvents(path, isPi, eventsFile, scope.prior)
	if err != nil {
		log.Warn("session events materialization failed", "session_file", path, "events_file", eventsFile, "err", err)
		return logfmt.Usage{}, false
	}
	log.Info("session events materialized", "session_file", path, "events_file", eventsFile)

	if slices.Contains(tape.Partial, logfmt.PartialUsage) {
		// One unreadable record is enough, so the hole in the dashboard belongs
		// to a run that was mostly counted. Say so here rather than leave the
		// figure to be explained from the Stats page.
		log.Warn("session usage incomplete, tokens not recorded", "session_file", path, "partial", tape.Partial)
		return tape.Totals, false
	}
	return tape.Totals, true
}

// sessionUsageTotals returns the token totals already in the session file a run
// is about to continue. It reads the directory as it stands now, with no time
// floor, because the file it is asking about is by definition the earlier run's.
// An unreadable or missing session gives zero, which is the same answer a fresh
// session gives.
func sessionUsageTotals(params RunnerParams) logfmt.Usage {
	path, isPi := sessionFile(params, time.Time{})
	if path == "" {
		return logfmt.Usage{}
	}
	tape, err := sessionTape(path, isPi)
	if err != nil {
		return logfmt.Usage{}
	}
	return tape.Totals
}

// usageSince subtracts what a resumed session already held from the totals read
// after the run, leaving what this invocation spent. Clamped at zero: the
// counter it feeds is monotonic, and a session file replaced rather than
// appended to would otherwise report a negative spend.
func usageSince(prior, total logfmt.Usage) logfmt.Usage {
	sub := func(a, b int) int {
		if a <= b {
			return 0
		}
		return a - b
	}
	return logfmt.Usage{
		Input:       sub(total.Input, prior.Input),
		Output:      sub(total.Output, prior.Output),
		CacheCreate: sub(total.CacheCreate, prior.CacheCreate),
		CacheRead:   sub(total.CacheRead, prior.CacheRead),
	}
}

// recordTokens reports one agent invocation's token spend. Per invocation, not
// per stage run: every invocation is a real spend of its own. Callers that
// resume a session must pass what that invocation added rather than the file's
// totals. Claude's --resume and pi's --session <path> both append to the
// recorded session's JSONL, so those totals carry every earlier run in the
// same file.
func (d *Daemon) recordTokens(ctx context.Context, stage, agent string, usage logfmt.Usage, complete bool) {
	if !complete {
		return
	}
	d.metrics.Tokens(ctx, stage, agent, metrics.TokenUsage{
		Input:       int64(usage.Input),
		Output:      int64(usage.Output),
		CacheCreate: int64(usage.CacheCreate),
		CacheRead:   int64(usage.CacheRead),
	})
}

// materializeSessionLog formats the session JSONL at path with logfmt and
// writes the result to logFile.
func materializeSessionLog(path string, isPi bool, logFile string) error {
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	dst, err := os.Create(logFile)
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	defer dst.Close()

	if isPi {
		err = logfmt.FmtPi(src, dst)
	} else {
		err = logfmt.Fmt(src, dst)
	}
	if err != nil {
		return fmt.Errorf("format session JSONL: %w", err)
	}
	return nil
}

// materializeSessionEvents parses the session JSONL at path into a structured
// tape, writes it to eventsFile, and returns it. The write is atomic (temp file
// plus rename) so a concurrent reader never sees half a document.
//
// The tape's totals are cut down to what this invocation added, because a
// sidecar is keyed per invocation while the session file it comes from holds
// every invocation that ever appended to it. Stats sums one sidecar per history
// row, so a resumed session written whole would count the earlier run twice.
// The events stay whole: the transcript is the conversation, and half of one
// would read as a run that started mid-sentence.
func materializeSessionEvents(path string, isPi bool, eventsFile string, prior logfmt.Usage) (logfmt.Tape, error) {
	tape, err := sessionTape(path, isPi)
	if err != nil {
		return logfmt.Tape{}, err
	}
	tape.Totals = usageSince(prior, tape.Totals)

	data, err := json.Marshal(tape)
	if err != nil {
		return logfmt.Tape{}, fmt.Errorf("encode tape: %w", err)
	}

	dir := filepath.Dir(eventsFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return logfmt.Tape{}, fmt.Errorf("create log directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(eventsFile)+".*")
	if err != nil {
		return logfmt.Tape{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return logfmt.Tape{}, fmt.Errorf("write tape: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return logfmt.Tape{}, fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, eventsFile); err != nil {
		os.Remove(tmpName)
		return logfmt.Tape{}, fmt.Errorf("rename tape: %w", err)
	}
	return tape, nil
}

// finalAssistantMessage returns the last assistant text from the run's session
// JSONL, or "" when no session file exists.
func finalAssistantMessage(log *slog.Logger, params RunnerParams, startedAt time.Time) string {
	path, isPi := sessionFile(params, startedAt)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var text string
	if isPi {
		text, err = logfmt.LastAssistantTextPi(f)
	} else {
		text, err = logfmt.LastAssistantText(f)
	}
	if err != nil {
		log.Warn("final assistant message extraction failed", "session_file", path, "err", err)
	}
	return text
}

// detectAgentError inspects a finished agent run for failures that leave a
// clean exit code: quota/usage limits and API errors. Claude runs are checked
// structurally from the session JSONL; any agent's output log is matched
// against the agent's configured failure_patterns. Returns a human-readable
// reason, the layer that matched (metrics.ErrorKindSessionAPI or
// metrics.ErrorKindFailurePattern), and true when a failure is detected. Call
// after materializeAgentLogs so the log file reflects the final session.
func (d *Daemon) detectAgentError(agentCfg config.Agent, params RunnerParams, logStart int64, startedAt time.Time) (reason, kind string, detected bool) {
	if agentCfg.IsClaude() && params.SessionID != "" {
		if path, _ := sessionFile(params, startedAt); path != "" {
			if reason, ok := scanClaudeSessionError(path); ok {
				return reason, metrics.ErrorKindSessionAPI, true
			}
		}
	}
	if len(agentCfg.FailurePatterns) > 0 && params.LogFile != "" {
		// Session-materialized logs are rewritten each run, so read the whole
		// file. Raw pipe-pane/DirectRunner logs accumulate across retries, so
		// only scan the bytes this run appended (past logStart).
		materialized := params.SessionID != "" || params.SessionDir != ""
		if content, err := currentRunLog(params.LogFile, materialized, logStart); err == nil && content != "" {
			if reason, ok := matchFailurePatterns(content, agentCfg.FailurePatterns); ok {
				return reason, metrics.ErrorKindFailurePattern, true
			}
		}
	}
	return "", "", false
}

// currentRunLog returns the portion of an agent log written by the most recent
// run. Appended logs carry earlier attempts, so only bytes at or after start
// are returned; a rewritten (materialized) log already holds just the last run.
func currentRunLog(path string, materialized bool, start int64) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !materialized && start > 0 && start <= int64(len(data)) {
		return string(data[start:]), nil
	}
	return string(data), nil
}

// fileSize returns the size of path, or 0 if it does not exist or cannot be
// stat'd (e.g. the log file has not been created yet).
func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// newestFile returns the path with the latest modification time, breaking ties
// on the name. Paths that cannot be stat'd are treated as oldest.
func newestFile(paths []string) string {
	var best string
	var bestTime time.Time
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		mt := fi.ModTime()
		if best == "" || mt.After(bestTime) || (mt.Equal(bestTime) && p > best) {
			best, bestTime = p, mt
		}
	}
	if best == "" && len(paths) > 0 {
		return paths[len(paths)-1]
	}
	return best
}

// newSessionID generates a UUID v4 string for Claude session identification.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func defaultLockPath() string {
	return filepath.Join(filepath.Dir(config.DefaultConfigPath()), "lock")
}

func expandTilde(path string) string {
	return config.ExpandTilde(path)
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func fieldValue(fields []pipeline.FieldUpdate, key string) any {
	for _, f := range fields {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// tailLogBytes is the maximum number of bytes read from the end of an agent
// log file when capturing diagnostic output.
const tailLogBytes = 4096

// tailFile reads up to tailLogBytes from the end of a file and returns it as a
// trimmed string. Returns empty string on any error.
func tailFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}

	offset := max(info.Size()-tailLogBytes, 0)
	if _, err := f.Seek(offset, 0); err != nil {
		return ""
	}

	buf := make([]byte, info.Size()-offset)
	n, _ := f.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}
