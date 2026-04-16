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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

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

func (d *Daemon) renderTicketPrompt(tmpl string, t *ticket.Ticket, filePath, wtPath string) (string, error) {
	opts := prompt.Options{
		ReviewsDir: expandTilde(d.cfg.Plannotator.ReviewsDir),
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

// PlannotatorLookup is injected to resolve whether the plannotator binary is
// installed. It exists only so tests that inject a spawner can skip the real
// exec.LookPath check.
type PlannotatorLookup func(binary string) error

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

type Daemon struct {
	cfg                *config.Config
	worktrees          *worktree.Manager
	runner             RunnerFunc
	plannotatorSpawner PlannotatorSpawner
	plannotatorLookup  PlannotatorLookup
	skipOrphanCleanup  bool
	broker             *web.SSEBroker
	svc                *app.Service

	debounce time.Duration
	lockPath string
	log      *slog.Logger

	mu          sync.Mutex
	tickets     map[string]*ticketState
	running     map[string]context.CancelFunc
	queued      map[string]bool // dedupe: prevents same ticket being enqueued twice
	sem         chan struct{}
	plannotator map[string]context.CancelFunc // in-flight plannotator subprocesses

	selfWrites   map[string]int
	selfWritesMu sync.Mutex

	queue     priorityQueue
	queueCond *sync.Cond
}

type ticketState struct {
	ticket   *ticket.Ticket
	filePath string
}

func New(cfg *config.Config, opts ...Option) *Daemon {
	d := &Daemon{
		cfg:                cfg,
		worktrees:          worktree.New(expandTilde(cfg.WorktreesDir)),
		runner:             tmuxRunner,
		plannotatorSpawner: defaultPlannotatorSpawner,
		plannotatorLookup:  defaultPlannotatorLookup,
		broker:             web.NewSSEBroker(),
		debounce:           time.Second,
		lockPath:           defaultLockPath(),
		log: slog.New(charmlog.NewWithOptions(os.Stderr, charmlog.Options{
			ReportTimestamp: true,
		})),
		tickets:     make(map[string]*ticketState),
		running:     make(map[string]context.CancelFunc),
		queued:      make(map[string]bool),
		sem:         make(chan struct{}, cfg.MaxConcurrentAgents),
		plannotator: make(map[string]context.CancelFunc),
		selfWrites:  make(map[string]int),
	}
	for _, opt := range opts {
		opt(d)
	}
	d.queueCond = sync.NewCond(&d.mu)
	d.svc = d.buildService()
	return d
}

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
			d.tickets[id] = &ticketState{ticket: st.Ticket, filePath: st.FilePath}
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
	return app.New(d.cfg, repo, rt)
}

// ticketLog returns a logger with the ticket ID pre-set.
func (d *Daemon) ticketLog(ticketID string) *slog.Logger {
	return d.log.With("ticket", ticketID)
}

func (d *Daemon) Run(ctx context.Context) error {
	lockFile, err := d.acquireLock()
	if err != nil {
		return fmt.Errorf("acquiring lock: %w", err)
	}
	defer d.releaseLock(lockFile)

	if !d.skipOrphanCleanup {
		d.cleanOrphanedWindows()
	}

	tasksDir := expandTilde(d.cfg.TicketsDir)
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		return fmt.Errorf("creating tickets dir: %w", err)
	}
	logsDir := expandTilde(d.cfg.LogsDir)
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("creating logs dir: %w", err)
	}

	if err := d.initialScan(tasksDir); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}
	d.log.Info("daemon started", "dir", tasksDir, "tasks", len(d.tickets), "queued", d.queue.Len())

	if d.cfg.Web.Enabled != nil && *d.cfg.Web.Enabled {
		srv := web.New(d, d.broker, d.cfg.Web.Host, d.cfg.Web.Port, d.log)
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

	w, err := watcher.New(tasksDir, d.debounce)
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

		d.mu.Lock()

		// Crash recovery: reset running → todo (kontora tickets only).
		if t.Kontora && t.Status == ticket.StatusInProgress {
			d.ticketLog(t.ID).Warn("crash recovery: resetting to todo")
			_ = t.SetField("status", string(ticket.StatusTodo))
			data, merr := t.Marshal()
			if merr == nil {
				d.recordSelfWrite(path)
				_ = os.WriteFile(path, data, 0o644)
			}
		}

		d.tickets[t.ID] = &ticketState{ticket: t, filePath: path}
		if t.Kontora && t.Status == ticket.StatusTodo && *d.cfg.AutoPickUp {
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

	log := d.ticketLog(t.ID)

	d.mu.Lock()
	defer d.mu.Unlock()

	prev, known := d.tickets[t.ID]
	d.tickets[t.ID] = &ticketState{ticket: t, filePath: path}
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

			if !*d.cfg.AutoPickUp {
				log.Info("skipping auto pick-up", "pipeline", t.Pipeline)
			} else if !known {
				log.Info("new ticket", "pipeline", t.Pipeline)
				d.enqueue(t)
			} else {
				log.Info("enqueuing", "previous_status", string(prev.ticket.Status), "pipeline", t.Pipeline, "stage", t.Stage)
				d.enqueue(t)
			}
		}
	case ticket.StatusPaused, ticket.StatusCancelled, ticket.StatusOpen:
		if cancel, ok := d.running[t.ID]; ok {
			log.Info("killing agent", "reason", "user set "+string(t.Status))
			cancel()
		}
		if t.Status == ticket.StatusCancelled {
			go d.cleanupWorktree(log, t)
		}
	case ticket.StatusDone:
		if cancel, ok := d.running[t.ID]; ok {
			log.Info("killing agent", "reason", "user set "+string(t.Status))
			cancel()
		}
		go d.cleanupWorktree(log, t)
	default:
		if d.cfg.IsCustomStatus(string(t.Status)) {
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
func (d *Daemon) ticketBranch(t *ticket.Ticket) string {
	if b := strings.TrimSpace(t.Branch); b != "" {
		return b
	}
	return worktree.BranchName(d.cfg.BranchPrefix, t.ID)
}

// removeWorktree removes the git worktree for a ticket. Logs but does not
// propagate errors — a failed cleanup should not block ticket completion.
// Dirty worktrees are preserved (branch and directory kept intact).
func (d *Daemon) removeWorktree(log *slog.Logger, repoPath, repoName, ticketID, branchPrefix string) {
	if err := d.worktrees.Remove(repoPath, repoName, ticketID, branchPrefix); errors.Is(err, worktree.ErrDirtyWorktree) {
		log.Warn("worktree has uncommitted changes, keeping it")
	} else if err != nil {
		log.Warn("worktree cleanup failed", "err", err)
	} else {
		log.Info("worktree removed")
	}
}

// isUserOverride returns true if the status represents a user-initiated
// override that should prevent the exit handler from changing the status.
func (d *Daemon) isUserOverride(s ticket.Status) bool {
	return s == ticket.StatusPaused || s == ticket.StatusCancelled ||
		s == ticket.StatusOpen || s == ticket.StatusDone ||
		d.cfg.IsCustomStatus(string(s))
}

// isTerminalOverride returns true if the status is a terminal user override
// that requires worktree cleanup and task window teardown.
func isTerminalOverride(s ticket.Status) bool {
	return s == ticket.StatusCancelled || s == ticket.StatusDone
}

// cleanupWorktree resolves repo info from a ticket and removes its worktree.
// Safe to call from a goroutine (does not hold d.mu).
func (d *Daemon) cleanupWorktree(log *slog.Logger, t *ticket.Ticket) {
	repoName, repoPath, err := d.resolvePath(t)
	if err != nil {
		log.Warn("worktree cleanup: resolve path failed", "err", err)
		return
	}
	d.removeWorktree(log, repoPath, repoName, t.ID, d.cfg.BranchPrefix)
}

// enqueue adds a ticket to the queue. Must be called with d.mu held.
// Skips enqueue if the ticket is already queued.
func (d *Daemon) enqueue(t *ticket.Ticket) {
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

	// Check ticket is still in a state we should process.
	if t.Status != ticket.StatusTodo {
		d.mu.Unlock()
		return
	}

	// Register cancel func before mutating status so concurrent ops
	// can properly cancel the ticket while it's setting up worktrees.
	taskCtx, taskCancel := context.WithCancel(ctx)
	d.running[ticketID] = taskCancel

	defer func() {
		taskCancel()
		d.mu.Lock()
		delete(d.running, ticketID)
		d.mu.Unlock()
	}()

	if t.Pipeline == "" {
		d.mu.Unlock()
		d.runSimpleTicket(ctx, taskCtx, log, ticketID, t, filePath)
		return
	}

	pipelineCfg, ok := d.cfg.Pipelines[t.Pipeline]
	if !ok {
		log.Error("unknown pipeline", "pipeline", t.Pipeline)
		d.mu.Unlock()
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
	if d.cfg.ReworkIsBuiltin &&
		t.Stage == config.ReworkStageName &&
		!stageInPipeline(pipelineCfg, config.ReworkStageName) {
		d.runReworkStage(ctx, taskCtx, log, ticketID, t, filePath)
		return
	}

	// Evaluate pickup.
	action, err := pipeline.Evaluate(t, pipelineCfg, pipeline.Event{
		Kind:      pipeline.EventPickedUp,
		Timestamp: time.Now(),
	})
	if err != nil {
		log.Error("evaluate pickup failed", "err", err)
		return
	}

	// Apply fields (status=in_progress, started_at).
	if err := d.applyAction(t, action); err != nil {
		log.Error("apply action failed", "phase", "pickup", "err", err)
		return
	}
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

	// Resolve path.
	repoName, repoPath, err := d.resolvePath(t)
	if err != nil {
		log.Error("resolve path failed", "err", err)
		d.pauseTicket(t, filePath, "resolve path failed: "+err.Error())
		return
	}

	// Create worktree.
	branch := d.ticketBranch(t)
	wtPath, created, err := d.worktrees.Create(repoPath, repoName, ticketID, branch)
	if err != nil {
		log.Error("create worktree failed", "path", repoPath, "err", err)
		d.pauseTicket(t, filePath, "create worktree failed: "+err.Error())
		return
	}
	if created {
		log.Info("worktree created", "path", wtPath)
	} else {
		log.Info("worktree reused", "path", wtPath)
	}
	_ = t.SetField("branch", branch)
	if err := d.writeTicket(t, filePath); err != nil {
		log.Error("write failed", "phase", "branch", "err", err)
		return
	}

	// Render prompt.
	stageName := action.Spawn.Stage
	agentName := action.Spawn.Agent
	if t.Agent != "" {
		agentName = t.Agent
	}

	agentCfg, agentOK := d.cfg.Agents[agentName]
	if !agentOK {
		log.Error("unknown agent", "agent", agentName)
		d.pauseTicket(t, filePath, fmt.Sprintf("unknown agent %q", agentName))
		return
	}
	stageCfg := d.cfg.Stages[stageName]

	rendered, err := d.renderTicketPrompt(stageCfg.Prompt, t, filePath, wtPath)
	if err != nil {
		log.Error("render prompt failed", "stage", stageName, "err", err)
		d.pauseTicket(t, filePath, "render prompt failed: "+err.Error())
		return
	}

	if rendered != "" {
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, true)
	}

	log.Info("spawning agent", "agent", agentName, "stage", stageName, "binary", agentCfg.Binary)

	args, settingsFile, sessionID, err := buildAgentArgs(agentCfg, rendered, tmux.ChannelName(ticketID))
	if err != nil {
		log.Error("build agent args failed", "err", err)
		d.pauseTicket(t, filePath, "build agent args failed: "+err.Error())
		return
	}
	if settingsFile != "" {
		defer os.Remove(settingsFile)
	}
	params := d.buildRunnerParams(agentCfg, stageCfg, args, wtPath, ticketID, stageName, sessionID)
	result, runnerErr := d.runner(taskCtx, params)
	if runnerErr != nil && taskCtx.Err() == nil {
		log.Error("runner failed", "stage", stageName, "err", runnerErr)
		d.killTaskWindow(ticketID)
		d.pauseTicket(t, filePath, "runner failed: "+runnerErr.Error())
		return
	}

	d.materializeAgentLogs(log, params)

	dur := result.ExitedAt.Sub(result.StartedAt).Truncate(time.Second)
	attrs := []any{"stage", stageName, "exit_code", result.ExitCode, "duration", dur}
	if result.ExitCode != 0 {
		if tail := tailFile(params.LogFile, 512); tail != "" {
			attrs = append(attrs, "output", tail)
		}
	}
	if runnerErr != nil {
		attrs = append(attrs, "err", runnerErr)
	}
	log.Info("agent exited", attrs...)

	d.handleAgentExit(ctx, taskCtx, handleExitParams{
		log:          log,
		ticketID:     ticketID,
		filePath:     filePath,
		stageName:    stageName,
		result:       result,
		pipelineCfg:  pipelineCfg,
		repoPath:     repoPath,
		repoName:     repoName,
		branchPrefix: d.cfg.BranchPrefix,
	})
}

func (d *Daemon) runSimpleTicket(ctx, taskCtx context.Context, log *slog.Logger, ticketID string, t *ticket.Ticket, filePath string) {
	agentName := d.cfg.DefaultAgent
	if t.Agent != "" {
		agentName = t.Agent
	}
	agentCfg, ok := d.cfg.Agents[agentName]
	if !ok {
		log.Error("unknown agent", "agent", agentName)
		d.pauseTicket(t, filePath, fmt.Sprintf("unknown agent %q", agentName))
		return
	}

	// Set status=in_progress, started_at.
	now := time.Now()
	_ = t.SetField("status", string(ticket.StatusInProgress))
	_ = t.SetField("started_at", now.Format(time.RFC3339))
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

	// Resolve path.
	repoName, repoPath, err := d.resolvePath(t)
	if err != nil {
		log.Error("resolve path failed", "err", err)
		d.pauseTicket(t, filePath, "resolve path failed: "+err.Error())
		return
	}

	// Create worktree.
	branch := d.ticketBranch(t)
	wtPath, created, err := d.worktrees.Create(repoPath, repoName, ticketID, branch)
	if err != nil {
		log.Error("create worktree failed", "path", repoPath, "err", err)
		d.pauseTicket(t, filePath, "create worktree failed: "+err.Error())
		return
	}
	if created {
		log.Info("worktree created", "path", wtPath)
	} else {
		log.Info("worktree reused", "path", wtPath)
	}
	_ = t.SetField("branch", branch)
	if err := d.writeTicket(t, filePath); err != nil {
		log.Error("write failed", "phase", "branch", "err", err)
		return
	}

	// Render prompt.
	rendered, err := d.renderTicketPrompt(defaultPromptTemplate, t, filePath, wtPath)
	if err != nil {
		log.Error("render prompt failed", "err", err)
		d.pauseTicket(t, filePath, "render prompt failed: "+err.Error())
		return
	}

	if rendered != "" {
		rendered += buildOperationalAppendix(t.ID, filePath, wtPath, false)
	}

	log.Info("spawning agent", "agent", agentName, "binary", agentCfg.Binary)

	args, settingsFile, _, err := buildAgentArgs(agentCfg, rendered, tmux.ChannelName(ticketID))
	if err != nil {
		log.Error("build agent args failed", "err", err)
		d.pauseTicket(t, filePath, "build agent args failed: "+err.Error())
		return
	}
	if settingsFile != "" {
		defer os.Remove(settingsFile)
	}
	params := d.buildRunnerParams(agentCfg, config.Stage{}, args, wtPath, ticketID, "default", "")
	result, runnerErr := d.runner(taskCtx, params)
	if runnerErr != nil && taskCtx.Err() == nil {
		log.Error("runner failed", "err", runnerErr)
		d.killTaskWindow(ticketID)
		d.pauseTicket(t, filePath, "runner failed: "+runnerErr.Error())
		return
	}

	d.materializeAgentLogs(log, params)

	dur := result.ExitedAt.Sub(result.StartedAt).Truncate(time.Second)
	attrs := []any{"exit_code", result.ExitCode, "duration", dur}
	if result.ExitCode != 0 {
		if tail := tailFile(params.LogFile, 512); tail != "" {
			attrs = append(attrs, "output", tail)
		}
	}
	if runnerErr != nil {
		attrs = append(attrs, "err", runnerErr)
	}
	log.Info("agent exited", attrs...)

	// Handle context cancellation.
	branchPrefix := d.cfg.BranchPrefix
	if taskCtx.Err() != nil {
		if ctx.Err() != nil {
			log.Warn("interrupted by shutdown")
			return
		}
		if t2, err := ticket.ParseFile(filePath); err == nil {
			if isTerminalOverride(t2.Status) {
				d.removeWorktree(log, repoPath, repoName, ticketID, branchPrefix)
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
			d.removeWorktree(log, repoPath, repoName, ticketID, branchPrefix)
			d.killTaskWindow(ticketID)
		}
		d.mu.Lock()
		d.tickets[ticketID] = &ticketState{ticket: t2, filePath: filePath}
		d.mu.Unlock()
		return
	}

	// Simple exit handling: 0 → done, non-0 → paused.
	if result.ExitCode == 0 {
		_ = t2.SetField("status", string(ticket.StatusDone))
		_ = t2.SetField("last_error", "")
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
		d.removeWorktree(log, repoPath, repoName, ticketID, branchPrefix)
	}

	d.mu.Lock()
	d.tickets[ticketID] = &ticketState{ticket: t2, filePath: filePath}
	d.broadcastTicketUpdate(ticketID)
	d.mu.Unlock()
}

type handleExitParams struct {
	log          *slog.Logger
	ticketID     string
	filePath     string
	stageName    string
	result       process.Result
	pipelineCfg  config.Pipeline
	repoPath     string
	repoName     string
	branchPrefix string
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
				d.removeWorktree(p.log, p.repoPath, p.repoName, p.ticketID, p.branchPrefix)
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
			d.removeWorktree(p.log, p.repoPath, p.repoName, p.ticketID, p.branchPrefix)
			d.killTaskWindow(p.ticketID)
		}
		d.mu.Lock()
		d.tickets[p.ticketID] = &ticketState{ticket: t2, filePath: p.filePath}
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

	if err := d.writeTicket(t2, p.filePath); err != nil {
		p.log.Error("write failed", "phase", "exit", "err", err)
		return
	}

	// Clean up worktree on terminal states.
	if exitAction.Kind == pipeline.ActionComplete {
		d.removeWorktree(p.log, p.repoPath, p.repoName, p.ticketID, p.branchPrefix)
	}

	// Kill tmux window unless paused or parked-with-failure — keep it alive
	// so the user can attach and inspect/fix the failure in the worktree.
	keepWindow := exitAction.Kind == pipeline.ActionPause ||
		(exitAction.Kind == pipeline.ActionPark && p.result.ExitCode != 0)
	if !keepWindow {
		d.killTaskWindow(p.ticketID)
	}

	d.mu.Lock()
	d.tickets[p.ticketID] = &ticketState{ticket: t2, filePath: p.filePath}

	// Re-enqueue if advance/retry/back.
	switch exitAction.Kind { //nolint:exhaustive
	case pipeline.ActionAdvance, pipeline.ActionRetry, pipeline.ActionBack:
		d.enqueue(t2)
	}
	d.broadcastTicketUpdate(p.ticketID)
	d.mu.Unlock()
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

func (d *Daemon) buildRunnerParams(agentCfg config.Agent, stageCfg config.Stage, args []string, dir, ticketID, stageName, sessionID string) RunnerParams {
	logsDir := expandTilde(d.cfg.LogsDir)
	logDir := filepath.Join(logsDir, ticketID)

	var sessionDir string
	if agentCfg.Binary == "pi" {
		sessionDir = filepath.Join(logDir, "pi-sessions")
		args = append(args, "--session-dir", sessionDir)
	}

	env := make(map[string]string, len(d.cfg.Environment)+len(agentCfg.Environment))
	maps.Copy(env, d.cfg.Environment)
	for k, v := range agentCfg.Environment {
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}

	return RunnerParams{
		Binary:      agentCfg.Binary,
		Args:        args,
		Dir:         dir,
		Timeout:     stageCfg.Timeout.Duration,
		TicketID:    ticketID,
		LogFile:     filepath.Join(logDir, stageName+".log"),
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
	return os.WriteFile(path, data, 0o644)
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
	d.tickets[t.ID] = &ticketState{ticket: t, filePath: path}
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

// materializeSessionLog locates the Claude session JSONL file, formats it
// with logfmt.Fmt(), and writes the result to logFile. Non-fatal: logs a warning
// if the session file is not found.
func (d *Daemon) materializeSessionLog(log *slog.Logger, sessionID, logFile string, env map[string]string) error {
	configDir := "~/.claude"
	if v, ok := env["CLAUDE_CONFIG_DIR"]; ok && v != "" {
		configDir = v
	}
	configDir = expandTilde(configDir)

	pattern := filepath.Join(configDir, "projects", "*", sessionID+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob session file: %w", err)
	}
	if len(matches) == 0 {
		log.Warn("session JSONL not found", "session_id", sessionID, "pattern", pattern)
		return nil
	}

	sessionFile := matches[0]
	if len(matches) > 1 {
		log.Warn("multiple session JSONL files found, using first", "session_id", sessionID, "count", len(matches))
	}

	src, err := os.Open(sessionFile)
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

	if err := logfmt.Fmt(src, dst); err != nil {
		return fmt.Errorf("format session JSONL: %w", err)
	}

	log.Info("session log materialized", "session_id", sessionID, "log_file", logFile)
	return nil
}

// materializeAgentLogs materializes session logs for agents that write
// structured JSONL (Claude via SessionID, pi via SessionDir).
func (d *Daemon) materializeAgentLogs(log *slog.Logger, params RunnerParams) {
	if params.SessionID != "" {
		if err := d.materializeSessionLog(log, params.SessionID, params.LogFile, params.Env); err != nil {
			log.Warn("session log materialization failed", "err", err)
		}
	}
	if params.SessionDir != "" {
		if err := d.materializePiSessionLog(log, params.SessionDir, params.LogFile); err != nil {
			log.Warn("pi session log materialization failed", "err", err)
		}
	}
}

// materializePiSessionLog globs the pi session directory for JSONL files,
// formats the session with logfmt.FmtPi, and writes the result to logFile.
// Non-fatal: logs a warning if no files found.
func (d *Daemon) materializePiSessionLog(log *slog.Logger, sessionDir, logFile string) error {
	pattern := filepath.Join(sessionDir, "*.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob pi session files: %w", err)
	}
	if len(matches) == 0 {
		log.Warn("pi session JSONL not found", "dir", sessionDir)
		return nil
	}

	sessionFile := matches[0]
	if len(matches) > 1 {
		log.Warn("multiple pi session JSONL files found, using first", "dir", sessionDir, "count", len(matches))
	}

	src, err := os.Open(sessionFile)
	if err != nil {
		return fmt.Errorf("open pi session file: %w", err)
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

	if err := logfmt.FmtPi(src, dst); err != nil {
		return fmt.Errorf("format pi session JSONL: %w", err)
	}

	log.Info("pi session log materialized", "dir", sessionDir, "log_file", logFile)
	return nil
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

// tailFile reads up to maxBytes from the end of a file and returns it as a
// trimmed string. Returns empty string on any error.
func tailFile(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}

	offset := max(info.Size()-maxBytes, 0)
	if _, err := f.Seek(offset, 0); err != nil {
		return ""
	}

	buf := make([]byte, info.Size()-offset)
	n, _ := f.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}
