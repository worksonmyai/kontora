package daemon

import (
	"container/heap"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	charmlog "github.com/charmbracelet/log"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/prompt"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/watcher"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

const defaultPromptTemplate = "Work on this ticket: {{ .Ticket.ID }} — {{ .Ticket.Title }}\n\n{{ .Ticket.Description }}"

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
	LogFile     string            // path for agent output log (PTY capture or materialized session log)
	Interactive bool              // use interactive tmux wait-for flow (for Claude agents)
	SessionID   string            // Claude session ID; used for session JSONL materialization after agent exit
	SessionDir  string            // pi session directory; used for session JSONL materialization after agent exit
	Env         map[string]string // environment variables to set for the agent process
	OnReady     func()            // called after the agent process is running (e.g. tmux window created)
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
	env := make([]string, 0, len(p.Env))
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	return process.Run(ctx, process.RunParams{
		Binary:  p.Binary,
		Args:    p.Args,
		Dir:     p.Dir,
		Timeout: p.Timeout,
		Stdout:  logFile,
		Stderr:  logFile,
		Env:     env,
	})
}

// tmuxRunner wraps tmux.Run for use as a RunnerFunc.
func tmuxRunner(ctx context.Context, p RunnerParams) (process.Result, error) {
	return tmux.Run(ctx, tmux.RunParams{
		Binary:      p.Binary,
		Args:        p.Args,
		Dir:         p.Dir,
		Timeout:     p.Timeout,
		TicketID:    p.TicketID,
		LogFile:     p.LogFile,
		Interactive: p.Interactive,
		SessionID:   p.SessionID,
		Env:         p.Env,
		OnReady:     p.OnReady,
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

// WithSkipOrphanCleanup disables the startup cleanup of orphaned tmux
// windows in the kontora session. Used in tests to avoid killing windows
// owned by other concurrently running test packages.
func WithSkipOrphanCleanup() Option {
	return func(d *Daemon) { d.skipOrphanCleanup = true }
}

// PlannotatorSpawner runs the `plannotator review` subprocess and returns its
// captured stdout. Kept as a separate seam from the generic RunnerFunc because
// the rest of the codebase conflates "runner for an agent" with tmux lifecycle
// hooks that don't apply here.
type PlannotatorSpawner func(ctx context.Context, params PlannotatorParams) (stdout string, err error)

// PlannotatorParams carries inputs for a single plannotator invocation.
type PlannotatorParams struct {
	Binary  string
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

type Daemon struct {
	cfg                atomic.Pointer[config.Config]
	worktrees          *worktree.Manager
	runner             RunnerFunc
	plannotatorSpawner PlannotatorSpawner
	plannotatorLookup  PlannotatorLookup
	agentLookup        AgentLookup
	skipOrphanCleanup  bool
	broker             *web.SSEBroker
	svc                *app.Service

	debounce     time.Duration
	lockPath     string
	configPath   string
	instanceName string
	log          *slog.Logger

	// configOverride re-applies the caller's command-line overrides to every
	// config a reload loads. Set once at construction, read without a lock.
	configOverride func(*config.Config)

	// reloadMu serializes config reloads from the different triggers (SIGHUP,
	// the config file watcher, and the raw-config endpoint).
	reloadMu sync.Mutex

	mu              sync.Mutex
	tickets         map[string]*ticketState
	running         map[string]context.CancelFunc
	queued          map[string]bool   // dedupe: prevents same ticket being enqueued twice
	runningBranches map[string]string // repoPath\x00branch → ticketID holding the branch
	sem             chan struct{}
	plannotator     map[string]context.CancelFunc // in-flight plannotator subprocesses

	selfWrites   map[string]int
	selfWritesMu sync.Mutex

	queue     priorityQueue
	queueCond *sync.Cond
}

type ticketState struct {
	ticket   *ticket.Ticket
	filePath string
	modTime  time.Time
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
		worktrees:          worktree.New(expandTilde(cfg.WorktreesDir)),
		runner:             tmuxRunner,
		plannotatorSpawner: defaultPlannotatorSpawner,
		plannotatorLookup:  defaultPlannotatorLookup,
		agentLookup:        defaultAgentLookup,
		broker:             web.NewSSEBroker(),
		debounce:           time.Second,
		lockPath:           defaultLockPath(),
		instanceName:       cfg.InstanceName,
		log: slog.New(charmlog.NewWithOptions(os.Stderr, charmlog.Options{
			ReportTimestamp: true,
		})),
		tickets:         make(map[string]*ticketState),
		running:         make(map[string]context.CancelFunc),
		queued:          make(map[string]bool),
		runningBranches: make(map[string]string),
		sem:             make(chan struct{}, cfg.MaxConcurrentAgents),
		plannotator:     make(map[string]context.CancelFunc),
		selfWrites:      make(map[string]int),
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

	if !d.skipOrphanCleanup {
		d.cleanOrphanedWindows()
	}

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

	if err := d.initialScan(tasksDir); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}
	d.log.Info("daemon started", "dir", tasksDir, "tasks", len(d.tickets), "queued", d.queue.Len())

	if cfg.Web.Enabled != nil && *cfg.Web.Enabled {
		srv := web.New(d, d.broker, cfg.Web.Host, cfg.Web.Port, cfg.Web.Token, d.log)
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

	w, err := watcher.New(tasksDir, d.debounce, watcher.MarkdownFilter)
	if err != nil {
		return fmt.Errorf("starting watcher: %w", err)
	}
	defer w.Close()

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

func (d *Daemon) initialScan(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	cfg := d.config()
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
					d.recordSelfWrite(path)
					_ = os.WriteFile(path, data, 0o644)
				}
			} else {
				d.ticketLog(t.ID).Info("in_progress on another instance, leaving alone", "claimed_by", t.ClaimedBy)
			}
		}

		d.tickets[t.ID] = newTicketState(t, path)
		if t.Kontora && t.Status == ticket.StatusTodo && *cfg.AutoPickUp {
			d.ticketLog(t.ID).Info("enqueuing", "pipeline", t.Pipeline, "stage", t.Stage)
			d.enqueue(t)
		}
		d.mu.Unlock()
	}
	return nil
}

func (d *Daemon) handleEvent(ev watcher.Event) {
	if d.isSelfWrite(ev.Path) {
		return
	}

	switch ev.Op {
	case watcher.OpChanged:
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
				d.recordSelfWrite(path)
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

	for id, ts := range d.tickets {
		if ts.filePath == path {
			if cancel, ok := d.running[id]; ok {
				d.ticketLog(id).Info("killing agent", "reason", "file removed")
				cancel()
			}
			d.removeQueuedLocked(id)
			d.broadcastTicketDeleted(ts)
			delete(d.tickets, id)
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

// enqueue adds a ticket to the queue. Must be called with d.mu held.
// Skips enqueue if the ticket is already queued.
func (d *Daemon) enqueue(t *ticket.Ticket) {
	if t == nil || !t.Kontora {
		return
	}
	if d.queued[t.ID] {
		return
	}
	d.queued[t.ID] = true
	heap.Push(&d.queue, &queueItem{
		ticketID: t.ID,
		created:  derefTime(t.Created),
	})
	d.queueCond.Signal()
}

func (d *Daemon) scheduler(ctx context.Context, wg *sync.WaitGroup) {
	go func() {
		<-ctx.Done()
		d.mu.Lock()
		d.queueCond.Signal()
		d.mu.Unlock()
	}()

	for {
		d.mu.Lock()
		for d.queue.Len() == 0 {
			if ctx.Err() != nil {
				d.mu.Unlock()
				return
			}
			d.queueCond.Wait()
		}

		item := heap.Pop(&d.queue).(*queueItem)
		delete(d.queued, item.ticketID)
		d.mu.Unlock()

		if ctx.Err() != nil {
			return
		}

		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		wg.Add(1)
		go func(ticketID string) {
			defer wg.Done()
			defer func() { <-d.sem }()
			d.runTicket(ctx, ticketID)
		}(item.ticketID)
	}
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

	ts, ok := d.tickets[ticketID]
	if !ok {
		d.mu.Unlock()
		return
	}
	t := ts.ticket
	filePath := ts.filePath

	// Check ticket is still managed by Kontora and in a state we should process.
	if !t.Kontora || t.Status != ticket.StatusTodo {
		d.mu.Unlock()
		return
	}

	// Register cancel func before mutating status so concurrent ops
	// can properly cancel the ticket while it's setting up worktrees.
	taskCtx, taskCancel := context.WithCancel(ctx)
	d.running[ticketID] = taskCancel

	// Reserve the branch atomically so two tickets targeting the same
	// repository and branch can't both pass this guard and reuse each
	// other's worktree by surprise. Keyed by (repoPath, branch) because
	// identical branch names in different repos don't collide.
	branch := ticketBranch(cfg, t)
	branchKey := expandTilde(t.Path) + "\x00" + branch
	if holder, ok := d.runningBranches[branchKey]; ok && holder != ticketID {
		taskCancel()
		delete(d.running, ticketID)
		d.mu.Unlock()
		d.pauseTicket(t, filePath, fmt.Sprintf("branch %s in use by ticket %s", branch, holder))
		return
	}
	d.runningBranches[branchKey] = ticketID

	defer func() {
		taskCancel()
		d.mu.Lock()
		delete(d.running, ticketID)
		if d.runningBranches[branchKey] == ticketID {
			delete(d.runningBranches, branchKey)
		}
		d.mu.Unlock()
	}()

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

	repoPath, wtPath, prepOK := d.prepareWorktreeForAgent(log, t, filePath, ticketID, stageName, branch)
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
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, true)
	}

	log.Info("spawning agent", "agent", agentName, "stage", stageName, "binary", agentCfg.Binary)

	run, spawnOK := d.spawnAgentRun(taskCtx, t, spawnAgentParams{
		cfg:       cfg,
		log:       log,
		ticketID:  ticketID,
		filePath:  filePath,
		stageName: stageName,
		wtPath:    wtPath,
		rendered:  rendered,
		agentCfg:  agentCfg,
		stageCfg:  stageCfg,
	})
	if !spawnOK {
		return
	}

	d.handleAgentExit(ctx, taskCtx, handleExitParams{
		log:          log,
		ticketID:     ticketID,
		filePath:     filePath,
		stageName:    stageName,
		result:       run.Result,
		finalMessage: run.FinalMessage,
		pipelineCfg:  pipelineCfg,
		repoPath:     repoPath,
		wtPath:       wtPath,
	})
}

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

	repoPath, wtPath, prepOK := d.prepareWorktreeForAgent(log, t, filePath, ticketID, "default", ticketBranch(cfg, t))
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
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, false)
	}

	log.Info("spawning agent", "agent", agentName, "binary", agentCfg.Binary)

	run, spawnOK := d.spawnAgentRun(taskCtx, t, spawnAgentParams{
		cfg:       cfg,
		log:       log,
		ticketID:  ticketID,
		filePath:  filePath,
		stageName: "default",
		wtPath:    wtPath,
		rendered:  rendered,
		agentCfg:  agentCfg,
		stageCfg:  config.Stage{},
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

	// Re-read ticket from disk (user may have edited during execution).
	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		log.Error("re-read failed after agent exit", "err", err)
		return
	}

	// If user changed status while running, respect that.
	if d.isUserOverride(t2.Status) {
		log.Info("user override during execution", "status", t2.Status)
		if isTerminalOverride(t2.Status) {
			d.removeWorktreeAt(log, repoPath, wtPath)
			d.killTaskWindow(ticketID)
		}
		d.mu.Lock()
		d.tickets[ticketID] = newTicketState(t2, filePath)
		d.mu.Unlock()
		return
	}

	// If another instance claimed the ticket while we ran, discard the result:
	// write nothing and keep the worktree.
	if d.claimedElsewhere(t2) {
		log.Info("discarding exit result: claimed by another instance", "claimed_by", t2.ClaimedBy)
		d.killTaskWindow(ticketID)
		d.mu.Lock()
		d.tickets[ticketID] = newTicketState(t2, filePath)
		d.mu.Unlock()
		return
	}

	// Simple exit handling: 0 → done, non-0 → paused.
	if result.ExitCode == 0 {
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
	} else {
		_ = t2.SetField("status", string(ticket.StatusPaused))
		_ = t2.SetField("last_error", fmt.Sprintf("agent exited with code %d", result.ExitCode))
		log.Warn("paused", "exit_code", result.ExitCode)
	}

	if err := d.writeTicket(t2, filePath); err != nil {
		log.Error("write failed", "phase", "exit", "err", err)
		return
	}

	if result.ExitCode == 0 {
		d.removeWorktreeAt(log, repoPath, wtPath)
	}

	d.mu.Lock()
	d.setTicketState(ticketID, t2, filePath)
	d.broadcastTicketUpdate(ticketID)
	d.mu.Unlock()
}

type handleExitParams struct {
	log          *slog.Logger
	ticketID     string
	filePath     string
	stageName    string
	result       process.Result
	finalMessage string
	pipelineCfg  config.Pipeline
	repoPath     string
	wtPath       string
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

	// Re-read ticket from disk (user may have edited during execution).
	t2, err := ticket.ParseFile(p.filePath)
	if err != nil {
		p.log.Error("re-read failed after agent exit", "err", err)
		return
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
		return
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

	// Override history agent to record the actual agent used.
	if t2.Agent != "" && exitAction.History != nil {
		exitAction.History.Agent = t2.Agent
	}

	summary := runSummary(t2.Summary, p.finalMessage)
	if exitAction.History != nil {
		exitAction.History.Summary = summary
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

	if err := d.writeTicket(t2, p.filePath); err != nil {
		p.log.Error("write failed", "phase", "exit", "err", err)
		return
	}

	// Clean up worktree on terminal states.
	if exitAction.Kind == pipeline.ActionComplete {
		d.removeWorktreeAt(p.log, p.repoPath, p.wtPath)
	}

	// Kill tmux window unless paused or parked-with-failure — keep it alive
	// so the user can attach and inspect/fix the failure in the worktree.
	keepWindow := exitAction.Kind == pipeline.ActionPause ||
		(exitAction.Kind == pipeline.ActionPark && p.result.ExitCode != 0)
	if !keepWindow {
		d.killTaskWindow(p.ticketID)
	}

	d.mu.Lock()
	d.setTicketState(p.ticketID, t2, p.filePath)

	// Re-enqueue if advance/retry/back.
	switch exitAction.Kind { //nolint:exhaustive
	case pipeline.ActionAdvance, pipeline.ActionRetry, pipeline.ActionBack:
		d.enqueue(t2)
	}
	d.broadcastTicketUpdate(p.ticketID)
	d.mu.Unlock()
}

// prepareWorktreeForAgent resolves the ticket's repo path, creates (or reuses)
// a worktree for the given branch, and writes the branch/last_log fields.
// On failure the ticket is paused and ok=false is returned so the caller can
// return immediately.
//
// The branch is passed in rather than recomputed: runTicket already reserved it
// in runningBranches, and branch_prefix reloads live, so a second
// ticketBranch call could return a different name from the one under guard.
func (d *Daemon) prepareWorktreeForAgent(log *slog.Logger, t *ticket.Ticket, filePath, ticketID, stageName, branch string) (repoPath, wtPath string, ok bool) {
	repoName, repoPath, err := d.resolvePath(t)
	if err != nil {
		log.Error("resolve path failed", "err", err)
		d.pauseTicket(t, filePath, "resolve path failed: "+err.Error())
		return "", "", false
	}

	wtPath, created, err := d.worktrees.Create(repoPath, repoName, ticketID, branch)
	if err != nil {
		log.Error("create worktree failed", "path", repoPath, "err", err)
		d.pauseTicket(t, filePath, "create worktree failed: "+err.Error())
		return "", "", false
	}
	if created {
		log.Info("worktree created", "path", wtPath, "branch", branch)
	} else {
		log.Info("reusing existing worktree for branch", "path", wtPath, "branch", branch)
	}

	if err := t.SetField("branch", branch); err != nil {
		log.Error("set field failed", "field", "branch", "err", err)
	}
	if err := t.SetField("last_log", d.stageLogPath(ticketID, stageName)); err != nil {
		log.Error("set field failed", "field", "last_log", "err", err)
	}
	// Clear the previous run's summary so the field always describes the run
	// that ended most recently.
	if err := t.SetField("summary", ""); err != nil {
		log.Error("set field failed", "field", "summary", "err", err)
	}
	if err := d.writeTicket(t, filePath); err != nil {
		log.Error("write failed", "phase", "spawn_fields", "err", err)
		return "", "", false
	}
	return repoPath, wtPath, true
}

type spawnAgentParams struct {
	// cfg is the caller's config snapshot. It is threaded through rather than
	// re-read so the environment and log paths a stage runs with come from the
	// same config version as its agent and stage definitions.
	cfg       *config.Config
	log       *slog.Logger
	ticketID  string
	filePath  string
	stageName string
	wtPath    string
	rendered  string
	agentCfg  config.Agent
	stageCfg  config.Stage
}

// agentRun is the outcome of one agent invocation: the process result and the
// agent's final assistant message from its session JSONL ("" when no session
// file was written).
type agentRun struct {
	Result       process.Result
	FinalMessage string
}

// spawnAgentRun builds agent args, invokes the runner, materializes session
// logs, and logs exit info. On a spawn or runner failure the ticket is paused
// and ok=false is returned so the caller can return immediately.
func (d *Daemon) spawnAgentRun(taskCtx context.Context, t *ticket.Ticket, p spawnAgentParams) (agentRun, bool) {
	binaryPath, err := d.agentLookup(p.agentCfg.Binary)
	if err != nil {
		p.log.Error("agent binary lookup failed", "binary", p.agentCfg.Binary, "err", err)
		d.pauseTicket(t, p.filePath, fmt.Sprintf("agent binary unavailable: %s", err))
		return agentRun{}, false
	}

	args, settingsFile, sessionID, err := buildAgentArgs(p.agentCfg, p.rendered, tmux.ChannelName(p.ticketID))
	if err != nil {
		p.log.Error("build agent args failed", "err", err)
		d.pauseTicket(t, p.filePath, "build agent args failed: "+err.Error())
		return agentRun{}, false
	}
	if settingsFile != "" {
		defer os.Remove(settingsFile)
	}

	params := d.buildRunnerParams(p.cfg, p.agentCfg, p.stageCfg, binaryPath, args, p.wtPath, p.ticketID, p.stageName, sessionID)
	// Stage logs are appended across retries (pipe-pane, DirectRunner), so record
	// where this run's output starts. Failure-pattern detection only scans from
	// here, otherwise a previous attempt's error would keep matching on retry.
	logStart := fileSize(params.LogFile)
	result, runnerErr := d.runner(taskCtx, params)
	if runnerErr != nil && taskCtx.Err() == nil {
		d.materializeAgentLogs(p.log, params)
		errAttrs := []any{"stage", p.stageName, "err", runnerErr}
		if tail := tailFile(params.LogFile); tail != "" {
			errAttrs = append(errAttrs, "output", tail)
		}
		p.log.Error("runner failed", errAttrs...)
		d.killTaskWindow(p.ticketID)
		d.pauseTicket(t, p.filePath, fmt.Sprintf("runner failed: %s", runnerErr.Error()))
		return agentRun{Result: result}, false
	}

	d.materializeAgentLogs(p.log, params)

	run := agentRun{Result: result, FinalMessage: finalAssistantMessage(p.log, params)}

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
		if reason, detected := d.detectAgentError(p.agentCfg, params, logStart); detected {
			p.log.Warn("agent error detected despite clean exit", "stage", p.stageName, "reason", reason)
			d.killTaskWindow(p.ticketID)
			d.pauseTicket(t, p.filePath, "agent error: "+reason)
			return run, false
		}
	}

	return run, true
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
	if len(s) <= summaryMaxLen {
		return s
	}
	cut := summaryMaxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[truncated]"
}

// buildOperationalAppendix returns a context block appended to every rendered prompt.
// It gives agents the ticket ID, file paths, and CLI commands they need so they
// don't have to search $HOME for context.
func buildOperationalAppendix(taskID, filePath, wtPath string, isPipeline bool) string {
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
	return b.String()
}

// buildAgentArgs constructs the argument list for an agent invocation.
// For Claude agents it injects --settings with a Notification hook that
// signals tmux wait-for on idle_prompt, and --session-id for session JSONL
// logging.
// For pi agents it injects -e with a temporary TypeScript extension that
// calls ctx.shutdown() on agent_end so pi exits cleanly after ticket completion.
// Returns the args, the path to the temporary settings/extension file (empty
// for other agents), the session ID (empty for non-Claude agents), and any error.
func buildAgentArgs(agentCfg config.Agent, rendered, channelName string) ([]string, string, string, error) {
	args := make([]string, len(agentCfg.Args))
	copy(args, agentCfg.Args)
	var settingsFile string
	var sessionID string
	switch {
	case agentCfg.IsClaude():
		var err error
		settingsFile, err = writeHooksSettings(channelName)
		if err != nil {
			return nil, "", "", fmt.Errorf("writing hooks settings: %w", err)
		}
		args = append(args, "--settings", settingsFile)
		sessionID = newSessionID()
		args = append(args, "--session-id", sessionID)
	case agentCfg.IsPi():
		var err error
		settingsFile, err = writePiExitExtension()
		if err != nil {
			return nil, "", "", fmt.Errorf("writing pi exit extension: %w", err)
		}
		args = append(args, "-e", settingsFile)
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
func writeHooksSettings(channelName string) (string, error) {
	waitCmd := fmt.Sprintf("tmux wait-for -S %s", channelName)
	settings := fmt.Sprintf(`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"%s"}]}],"Notification":[{"matcher":"idle_prompt","hooks":[{"type":"command","command":"%s"}]}]}}`, waitCmd, waitCmd)
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

// writePiExitExtension creates a temporary TypeScript extension file that
// makes pi exit cleanly after completing work. The extension listens for
// agent_end and calls ctx.shutdown().
func writePiExitExtension() (string, error) {
	const ext = `import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";
export default function (pi: ExtensionAPI) {
    pi.on("agent_end", async (_event, ctx) => { ctx.shutdown(); });
}
`
	f, err := os.CreateTemp("", "kontora-pi-ext-*.ts")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(ext); err != nil {
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

func (d *Daemon) buildRunnerParams(cfg *config.Config, agentCfg config.Agent, stageCfg config.Stage, binaryPath string, args []string, dir, ticketID, stageName, sessionID string) RunnerParams {
	logsDir := expandTilde(cfg.LogsDir)
	logDir := filepath.Join(logsDir, ticketID)

	var sessionDir string
	if agentCfg.IsPi() {
		// Per-stage directory: every stage of a ticket materializes its log from
		// this directory, so a shared one would give each stage the same session.
		sessionDir = filepath.Join(logDir, "pi-sessions", stageName)
		args = append(args, "--session-dir", sessionDir)
	}

	env := make(map[string]string, len(cfg.Environment)+len(agentCfg.Environment))
	maps.Copy(env, cfg.Environment)
	for k, v := range agentCfg.Environment {
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}

	return RunnerParams{
		Binary:      binaryPath,
		Args:        args,
		Dir:         dir,
		Timeout:     stageCfg.Timeout.Duration,
		TicketID:    ticketID,
		LogFile:     stageLogPath(cfg, ticketID, stageName),
		Interactive: agentCfg.IsClaude(),
		SessionID:   sessionID,
		SessionDir:  sessionDir,
		Env:         env,
		OnReady: func() {
			d.broadcastTerminalReady(ticketID)
		},
	}
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
	data, err := t.Marshal()
	if err != nil {
		return err
	}
	d.recordSelfWrite(path)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	// Refresh the cached modtime so UpdatedAt reflects this write. Paths that
	// reuse the ticketState in place (pickup, spawn) rely on this; reconstruct
	// paths reuse the refreshed modtime via setTicketState to avoid a second
	// stat. Caller must not hold d.mu.
	if st, err := os.Stat(path); err == nil {
		d.mu.Lock()
		if ts, ok := d.tickets[t.ID]; ok {
			ts.modTime = st.ModTime()
		}
		d.mu.Unlock()
	}
	return nil
}

// setTicketState replaces the cached state for id after a writeTicket call,
// carrying over the modtime writeTicket just refreshed so the swap doesn't
// stat the file a second time. Must be called with d.mu held, on a path that
// just wrote the file (so the cached modtime is current).
func (d *Daemon) setTicketState(id string, t *ticket.Ticket, path string) {
	var modTime time.Time
	if ts, ok := d.tickets[id]; ok {
		modTime = ts.modTime
	}
	d.tickets[id] = &ticketState{ticket: t, filePath: path, modTime: modTime}
}

func (d *Daemon) stageLogPath(ticketID, stageName string) string {
	return stageLogPath(d.config(), ticketID, stageName)
}

func stageLogPath(cfg *config.Config, ticketID, stageName string) string {
	return filepath.Join(expandTilde(cfg.LogsDir), ticketID, stageName+".log")
}

func (d *Daemon) pauseTicket(t *ticket.Ticket, path, reason string) {
	log := d.ticketLog(t.ID)
	log.Warn("pausing")
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

func (d *Daemon) recordSelfWrite(path string) {
	d.selfWritesMu.Lock()
	defer d.selfWritesMu.Unlock()
	d.selfWrites[path]++
}

func (d *Daemon) isSelfWrite(path string) bool {
	d.selfWritesMu.Lock()
	defer d.selfWritesMu.Unlock()
	n, ok := d.selfWrites[path]
	if !ok {
		return false
	}
	if n <= 1 {
		delete(d.selfWrites, path)
	} else {
		d.selfWrites[path] = n - 1
	}
	return true
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
	if tmux.HasWindow(tmux.DefaultSessionName, ticketID) {
		_ = tmux.KillWindow(tmux.DefaultSessionName, ticketID)
	}
}

func (d *Daemon) cleanOrphanedWindows() {
	windows, err := tmux.ListWindows(tmux.DefaultSessionName)
	if err != nil {
		d.log.Error("listing orphaned tmux windows", "err", err)
		return
	}
	for _, name := range windows {
		d.log.Warn("killing orphaned tmux window", "window", name)
		if err := tmux.KillWindow(tmux.DefaultSessionName, name); err != nil {
			d.log.Error("killing tmux window", "window", name, "err", err)
		}
	}
}

// Ticket queue implementation (FIFO by creation time).

type queueItem struct {
	ticketID string
	created  time.Time
}

type priorityQueue []*queueItem

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	return pq[i].created.Before(pq[j].created)
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
	configDir := "~/.claude"
	if v, ok := env["CLAUDE_CONFIG_DIR"]; ok && v != "" {
		configDir = v
	}
	configDir = expandTilde(configDir)
	pattern = filepath.Join(configDir, "projects", "*", sessionID+".jsonl")
	matches, err = filepath.Glob(pattern)
	return matches, pattern, err
}

// sessionFile locates the session JSONL of a run: the Claude session file
// matching params.SessionID, or the newest file in the pi params.SessionDir
// (a retried stage reuses its session directory, so the newest file is the
// run that just finished). Returns "" when the run has no session or no file
// was written.
func sessionFile(params RunnerParams) (path string, isPi bool) {
	switch {
	case params.SessionID != "":
		if matches, _, err := claudeSessionFiles(params.Env, params.SessionID); err == nil && len(matches) > 0 {
			return matches[0], false
		}
		return "", false
	case params.SessionDir != "":
		matches, err := filepath.Glob(filepath.Join(params.SessionDir, "*.jsonl"))
		if err != nil || len(matches) == 0 {
			return "", true
		}
		return newestFile(matches), true
	}
	return "", false
}

// materializeAgentLogs materializes session logs for agents that write
// structured JSONL (Claude via SessionID, pi via SessionDir).
func (d *Daemon) materializeAgentLogs(log *slog.Logger, params RunnerParams) {
	if params.SessionID == "" && params.SessionDir == "" {
		return
	}
	path, isPi := sessionFile(params)
	if path == "" {
		log.Warn("session JSONL not found", "session_id", params.SessionID, "session_dir", params.SessionDir)
		return
	}
	if err := materializeSessionLog(path, isPi, params.LogFile); err != nil {
		log.Warn("session log materialization failed", "session_file", path, "err", err)
		return
	}
	log.Info("session log materialized", "session_file", path, "log_file", params.LogFile)
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

// finalAssistantMessage returns the last assistant text from the run's session
// JSONL, or "" when no session file exists.
func finalAssistantMessage(log *slog.Logger, params RunnerParams) string {
	path, isPi := sessionFile(params)
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
// reason and true when a failure is detected. Call after materializeAgentLogs
// so the log file reflects the final session.
func (d *Daemon) detectAgentError(agentCfg config.Agent, params RunnerParams, logStart int64) (string, bool) {
	if agentCfg.IsClaude() && params.SessionID != "" {
		if path, _ := sessionFile(params); path != "" {
			if reason, ok := scanClaudeSessionError(path); ok {
				return reason, true
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
				return reason, true
			}
		}
	}
	return "", false
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
