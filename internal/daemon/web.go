package daemon

import (
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/web"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// RunningAgents returns the number of agents currently running.
func (d *Daemon) RunningAgents() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.running)
}

// ListTickets returns info for all tracked tickets.
func (d *Daemon) ListTickets() []web.TicketInfo {
	d.mu.Lock()
	defer d.mu.Unlock()

	cfg := d.config()
	tickets := make([]web.TicketInfo, 0, len(d.tickets))
	for _, ts := range d.tickets {
		// Only tickets whose status maps to a board column appear in the list.
		// This hides archived, plus foreign statuses (closed, tombstone, ...)
		// that have no column. All of them stay on disk and remain reachable
		// by ID via GetTicket.
		if !cfg.IsBoardStatus(string(ts.ticket.Status)) {
			continue
		}
		tickets = append(tickets, d.buildTicketInfo(cfg, ts, false))
	}
	return tickets
}

// GetTicket returns detailed info for a single ticket, including the body.
func (d *Daemon) GetTicket(id string) (web.TicketInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ts, ok := d.tickets[id]
	if !ok {
		return web.TicketInfo{}, web.ErrTicketNotFound
	}
	return d.buildTicketInfo(d.config(), ts, true), nil
}

// CreateTicket creates a new ticket file and registers it in the daemon.
func (d *Daemon) CreateTicket(req web.CreateTicketRequest) (web.TicketInfo, error) {
	cfg := d.config()
	id, err := cli.GenerateID(cfg.TicketsDir, req.Path)
	if err != nil {
		return web.TicketInfo{}, fmt.Errorf("generating ticket id: %w", err)
	}

	filePath := filepath.Join(config.ExpandTilde(cfg.TicketsDir), id+".md")

	// "none" is the opt-out sentinel, not a pipeline or agent name; cli.New
	// clears it. Checking here rather than relying on cli.New keeps the API
	// answering 400 instead of 500.
	if req.Pipeline != "" && req.Pipeline != config.NoneSentinel {
		if _, ok := cfg.Pipelines[req.Pipeline]; !ok {
			return web.TicketInfo{}, fmt.Errorf("%w %q", web.ErrUnknownPipeline, req.Pipeline)
		}
	}
	if req.Agent != "" && req.Agent != config.NoneSentinel {
		if _, ok := cfg.Agents[req.Agent]; !ok {
			return web.TicketInfo{}, fmt.Errorf("%w %q", web.ErrUnknownAgent, req.Agent)
		}
	}

	// cli.New writes the file itself, so the content is not available to record
	// up front. Suppressing the creation event loses nothing: the parse below
	// registers the ticket that the event would have registered.
	d.recordSelfWriteBlind(filePath)

	_, err = cli.New(cfg, cli.NewOpts{
		ID:         id,
		Path:       req.Path,
		Pipeline:   req.Pipeline,
		Agent:      req.Agent,
		Status:     req.Status,
		Title:      req.Title,
		Body:       req.Body,
		Branch:     req.Branch,
		BaseBranch: req.BaseBranch,
		NoEdit:     true,
	})
	if err != nil {
		// Nothing was created, so the reservation must go: the next ticket gets
		// this id back, and its creation event has to be acted on.
		d.forgetSelfWrite(filePath)
		return web.TicketInfo{}, fmt.Errorf("creating ticket: %w", err)
	}

	t, err := ticket.ParseFile(filePath)
	if err != nil {
		return web.TicketInfo{}, fmt.Errorf("parsing created ticket: %w", err)
	}

	d.mu.Lock()
	ts := newTicketState(t, filePath)
	d.tickets[id] = ts
	if t.Status == "todo" {
		d.enqueue(t)
	}
	info := d.buildTicketInfo(cfg, ts, false)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()

	return info, nil
}

// UploadTicket imports a ticket from raw .md file content.
func (d *Daemon) UploadTicket(content []byte) (web.TicketInfo, error) {
	cfg := d.config()
	t, err := ticket.ParseBytes(content)
	if err != nil {
		return web.TicketInfo{}, fmt.Errorf("invalid ticket file: %w", err)
	}

	if t.Title() == "" {
		return web.TicketInfo{}, fmt.Errorf("ticket must have a title (# heading in body)")
	}

	// Always generate a fresh ID for uploaded tickets — the original ID is an
	// internal identifier with no semantic meaning worth preserving, and
	// accepting user-controlled IDs would allow path traversal via crafted
	// values like "../../etc/cron.d/evil".
	prefix := t.Path
	if prefix == "" {
		prefix = "upload"
	}
	id, err := cli.GenerateID(cfg.TicketsDir, prefix)
	if err != nil {
		return web.TicketInfo{}, fmt.Errorf("generating ticket id: %w", err)
	}
	if err := t.SetField("id", id); err != nil {
		return web.TicketInfo{}, fmt.Errorf("setting ticket id: %w", err)
	}

	// Clamp status to open.
	if err := t.SetField("status", string(ticket.StatusOpen)); err != nil {
		return web.TicketInfo{}, fmt.Errorf("setting ticket status: %w", err)
	}

	// Ensure created timestamp exists.
	if t.Created == nil {
		now := time.Now().UTC()
		if err := t.SetField("created", now); err != nil {
			return web.TicketInfo{}, fmt.Errorf("setting ticket created: %w", err)
		}
	}

	filePath := filepath.Join(config.ExpandTilde(cfg.TicketsDir), t.ID+".md")
	if err := d.writeTicket(t, filePath); err != nil {
		return web.TicketInfo{}, fmt.Errorf("writing ticket file: %w", err)
	}

	d.mu.Lock()
	ts := newTicketState(t, filePath)
	d.tickets[t.ID] = ts
	info := d.buildTicketInfo(cfg, ts, false)
	d.broadcastTicketUpdate(t.ID)
	d.mu.Unlock()

	return info, nil
}

// GetConfig returns available pipelines and agents from the daemon config.
func (d *Daemon) GetConfig() web.ConfigInfo {
	cfg := d.config()
	pipelines := slices.Sorted(maps.Keys(cfg.Pipelines))
	infos := make([]web.PipelineInfo, len(pipelines))
	for i, name := range pipelines {
		steps := cfg.Pipelines[name]
		stageNames := make([]string, len(steps))
		maxRetries := make([]int, len(steps))
		for j, s := range steps {
			stageNames[j] = s.Stage
			maxRetries[j] = s.MaxRetries
		}
		var defaultAgent string
		if len(steps) > 0 {
			defaultAgent = steps[0].Agent
		}
		infos[i] = web.PipelineInfo{Name: name, Stages: stageNames, MaxRetries: maxRetries, DefaultAgent: defaultAgent}
	}
	agents := slices.Sorted(maps.Keys(cfg.Agents))
	projectNames := slices.Sorted(maps.Keys(cfg.Projects))
	projects := make([]web.ProjectInfo, len(projectNames))
	for i, name := range projectNames {
		p := cfg.Projects[name]
		projects[i] = web.ProjectInfo{
			Name:         name,
			Path:         p.Path,
			ResolvedPath: config.NormalizeRepoPath(p.Path),
			Pipeline:     p.Pipeline,
			Agent:        p.Agent,
			BranchPrefix: p.BranchPrefix,
		}
	}
	return web.ConfigInfo{
		Pipelines:      pipelines,
		PipelineInfos:  infos,
		Agents:         agents,
		Projects:       projects,
		DefaultAgent:   cfg.DefaultAgent,
		BranchPrefix:   cfg.BranchPrefix,
		CustomStatuses: cfg.Statuses,
	}
}

// DeleteTicket removes the ticket markdown file without triggering worktree cleanup.
func (d *Daemon) DeleteTicket(id string) error {
	d.mu.Lock()
	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}
	filePath := ts.filePath
	cancel, running := d.running[id]
	d.mu.Unlock()

	if err := d.guardDeletePath(filePath); err != nil {
		return err
	}

	if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if running {
		cancel()
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	ts, ok = d.tickets[id]
	if !ok {
		return nil
	}
	d.removeQueuedLocked(id)
	d.broadcastTicketDeleted(ts)
	delete(d.tickets, id)
	return nil
}

func (d *Daemon) guardDeletePath(filePath string) error {
	ticketsDir, err := filepath.Abs(config.ExpandTilde(d.config().TicketsDir))
	if err != nil {
		return fmt.Errorf("resolve tickets dir: %w", err)
	}
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("resolve ticket path: %w", err)
	}
	if filepath.Dir(absPath) != ticketsDir {
		return fmt.Errorf("%w: file outside tickets dir", web.ErrDeleteRejected)
	}
	if !strings.EqualFold(filepath.Ext(absPath), ".md") {
		return fmt.Errorf("%w: non-markdown ticket file", web.ErrDeleteRejected)
	}
	return nil
}

// PauseTicket cancels a running ticket's agent and sets its status to paused.
// It writes the paused status via a fresh ticket copy to avoid racing with
// the runTicket goroutine that holds a reference to the same *ticket.Ticket.
// Intentionally not using the shared app.Service: it needs to cancel the
// running agent and re-read from disk to avoid racing with runTicket.
func (d *Daemon) PauseTicket(id string) error {
	d.mu.Lock()

	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}
	if ts.ticket.Status != ticket.StatusInProgress {
		d.mu.Unlock()
		return web.ErrInvalidState
	}

	filePath := ts.filePath
	cancel, hasCancel := d.running[id]
	d.mu.Unlock()

	// Re-read ticket from disk to get a fresh copy that doesn't race with runTicket.
	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return err
	}
	if err := t2.SetField("status", "paused"); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}
	if err := t2.SetField("last_error", ""); err != nil {
		return fmt.Errorf("clearing last_error: %w", err)
	}
	if err := d.writeTicket(t2, filePath); err != nil {
		return err
	}

	// Cancel the running agent. The handleAgentExit path will see status=paused
	// on re-read and skip pipeline evaluation.
	if hasCancel {
		cancel()
	}

	d.mu.Lock()
	d.setTicketState(id, t2, filePath)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// RetryTicket resets a non-running ticket to todo and re-enqueues it.
func (d *Daemon) RetryTicket(id string) error {
	_, err := d.svc.Retry(id)
	return mapAppError(err)
}

// RunTicket enqueues a ticket in open or todo status for processing.
func (d *Daemon) RunTicket(id string) error {
	_, err := d.svc.Run(id)
	return mapAppError(err)
}

// SkipStage advances a ticket to the next pipeline stage, or completes it
// if already on the last stage.
func (d *Daemon) SkipStage(id string) error {
	_, err := d.svc.Skip(id)
	return mapAppError(err)
}

// SetStage moves a ticket to a specific pipeline stage by name.
func (d *Daemon) SetStage(id string, stage string) error {
	d.mu.Lock()

	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}

	pipelineName := ts.ticket.Pipeline
	filePath := ts.filePath

	pipelineCfg, ok := d.config().Pipelines[pipelineName]
	if !ok {
		d.mu.Unlock()
		return web.ErrInvalidState
	}

	found := false
	for _, s := range pipelineCfg {
		if s.Stage == stage {
			found = true
			break
		}
	}
	if !found {
		d.mu.Unlock()
		return web.ErrInvalidState
	}

	d.mu.Unlock()

	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return err
	}

	if err := t2.SetField("stage", stage); err != nil {
		return fmt.Errorf("failed to set ticket stage to %q: %w", stage, err)
	}

	if err := d.writeTicket(t2, filePath); err != nil {
		return err
	}

	d.mu.Lock()
	d.setTicketState(id, t2, filePath)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// MoveTicket sets a ticket's status to newStatus with transition validation.
func (d *Daemon) MoveTicket(id string, newStatus string) error {
	switch newStatus {
	case "paused":
		return d.PauseTicket(id)
	case "todo", "in_progress":
		// RunTicket handles open/todo tickets; RetryTicket handles paused/done/cancelled.
		// The daemon itself transitions todo→in_progress when it spawns the agent.
		if err := d.RunTicket(id); err == nil {
			return nil
		}
		return d.RetryTicket(id)
	default:
		switch newStatus {
		case "open", "done", "cancelled", "human_review":
			// valid
		default:
			if !d.config().IsCustomStatus(newStatus) {
				return web.ErrInvalidState
			}
		}
		_, err := d.svc.SetStatus(id, ticket.Status(newStatus))
		return mapAppError(err)
	}
}

// AddNote appends a timestamped note to a ticket's body, using the same
// AppendNote persistence as the local cli.Note path.
func (d *Daemon) AddNote(id string, text string) error {
	d.mu.Lock()
	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}
	filePath := ts.filePath
	d.mu.Unlock()

	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return err
	}
	t2.AppendNote(text, time.Now())
	if err := d.writeTicket(t2, filePath); err != nil {
		return err
	}

	d.mu.Lock()
	d.setTicketState(id, t2, filePath)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// SetSummary sets a ticket's summary frontmatter field, mirroring the local
// cli.Summary path.
func (d *Daemon) SetSummary(id string, text string) error {
	d.mu.Lock()
	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}
	filePath := ts.filePath
	d.mu.Unlock()

	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return err
	}
	if err := t2.SetField("summary", text); err != nil {
		return fmt.Errorf("setting summary: %w", err)
	}
	if err := d.writeTicket(t2, filePath); err != nil {
		return err
	}

	d.mu.Lock()
	d.setTicketState(id, t2, filePath)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// InitTicket initializes a non-kontora ticket: sets pipeline, path, kontora=true,
// status=todo, stage to the first pipeline stage, and enqueues it.
func (d *Daemon) InitTicket(id string, req web.InitTicketRequest) error {
	_, err := d.svc.Init(id, app.InitRequest{
		Pipeline: req.Pipeline,
		Path:     req.Path,
		Agent:    req.Agent,
		Branch:   req.Branch,
	})
	return mapAppError(err)
}

// UpdateTicket updates body and frontmatter fields of a ticket.
// Allowed in statuses: open, todo, paused.
func (d *Daemon) UpdateTicket(id string, req web.UpdateTicketRequest) error {
	d.mu.Lock()
	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ErrTicketNotFound
	}
	status := ts.ticket.Status
	filePath := ts.filePath
	d.mu.Unlock()

	switch status {
	case ticket.StatusOpen, ticket.StatusTodo, ticket.StatusPaused, ticket.StatusHumanReview:
		// allowed
	case ticket.StatusInProgress, ticket.StatusDone, ticket.StatusCancelled, ticket.StatusArchived:
		return web.ErrInvalidState
	default:
		if !d.config().IsCustomStatus(string(status)) {
			return web.ErrInvalidState
		}
	}

	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return err
	}

	cfg := d.config()
	if req.Pipeline != nil {
		pipeline := config.ClearNone(*req.Pipeline)
		if pipeline != "" {
			if _, ok := cfg.Pipelines[pipeline]; !ok {
				return fmt.Errorf("%w %q", web.ErrUnknownPipeline, pipeline)
			}
		}
		if err := t2.SetField("pipeline", pipeline); err != nil {
			return err
		}
	}
	if req.Path != nil {
		if err := t2.SetField("path", *req.Path); err != nil {
			return err
		}
	}
	if req.Agent != nil {
		agent := config.ClearNone(*req.Agent)
		if agent != "" {
			if _, ok := cfg.Agents[agent]; !ok {
				return fmt.Errorf("%w %q", web.ErrUnknownAgent, agent)
			}
		}
		if err := t2.SetField("agent", agent); err != nil {
			return err
		}
	}
	if req.Branch != nil {
		if err := t2.SetField("branch", *req.Branch); err != nil {
			return err
		}
	}
	if req.BaseBranch != nil {
		if err := t2.SetField("base_branch", *req.BaseBranch); err != nil {
			return err
		}
	}
	if req.Body != nil {
		t2.SetBody(*req.Body)
	}

	if err := d.writeTicket(t2, filePath); err != nil {
		return err
	}

	d.mu.Lock()
	d.setTicketState(id, t2, filePath)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// GetLogs returns the log content for a ticket stage. If stage is empty, it returns
// the most recently modified log, matching CLI behavior.
func (d *Daemon) GetLogs(id string, stage string) (string, error) {
	d.mu.Lock()
	_, ok := d.tickets[id]
	d.mu.Unlock()
	if !ok {
		return "", web.ErrTicketNotFound
	}

	cfg := d.config()
	var buf bytes.Buffer
	if err := cli.Logs(cfg.TicketsDir, cfg.LogsDir, id, stage, &buf); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", web.ErrLogNotFound
		}
		return "", err
	}
	return buf.String(), nil
}

// GetActivity returns the transcript of one run of a stage. A run still in
// flight is read from the session JSONL the agent is appending to; a finished
// run is read from its structured sidecar, and from the shared plaintext log
// when it has none.
//
// A fallback is marked stale when its bytes may describe a different run than
// the one asked for. That happens two ways, because <stage>.log holds only the
// newest run: the ticket's history records a newer run of the same stage, or
// the stage is running right now and appending to the same file.
func (d *Daemon) GetActivity(q web.ActivityQuery) (web.ActivityInfo, error) {
	d.mu.Lock()
	ts, ok := d.tickets[q.ID]
	var newestRun int
	var stageRunning bool
	var nextRun int
	var lr liveRun
	var registered bool
	if ok {
		for _, h := range ts.ticket.History {
			if h.Stage == q.Stage && h.Run > newestRun {
				newestRun = h.Run
			}
		}
		stageRunning = ts.ticket.Status == ticket.StatusInProgress && ts.ticket.Stage == q.Stage
		nextRun = stageRunIndex(ts.ticket, q.Stage)
		lr, registered = d.liveRuns[q.ID]
	}
	d.mu.Unlock()
	if !ok {
		return web.ActivityInfo{}, web.ErrTicketNotFound
	}

	// liveMatch is the run the daemon has registered as in flight. The second
	// clause covers the seconds between the ticket flipping to in_progress and
	// the invocation registering, which happens only after the worktree exists
	// and the prompt is rendered; without it a polling client gets an error per
	// tick at the start of every run. Once another run is registered this one is
	// not live: the rework stage runs while the ticket's stage field still names
	// a pipeline stage, and its transcript must not answer for that stage.
	liveMatch := registered && q.Stage == lr.stage && q.Run == lr.run
	live := q.Stage != "" && (liveMatch || (!registered && stageRunning && q.Run == nextRun))

	cfg := d.config()
	if liveMatch {
		if info, ok := d.liveActivity(cfg, q, lr); ok {
			return info, nil
		}
	}

	sidecarETag := fileETag(stageEventsPath(cfg, q.ID, q.Stage, q.Run), q.Run)
	if sidecarETag != "" && sidecarETag == q.IfNoneMatch {
		return web.ActivityInfo{NotModified: true, ETag: sidecarETag}, nil
	}

	tape, content, err := cli.StageActivity(cfg.TicketsDir, cfg.LogsDir, q.ID, q.Stage, q.Run)
	if err != nil {
		// A live run whose first bytes are still being written has nothing to
		// read yet. Answering with an error would flash one on every poll.
		if errors.Is(err, os.ErrNotExist) && live {
			return web.ActivityInfo{Source: "log", Stage: q.Stage, Run: q.Run, Live: true}, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return web.ActivityInfo{}, web.ErrLogNotFound
		}
		return web.ActivityInfo{}, err
	}

	info := web.ActivityInfo{Source: "events", Stage: q.Stage, Run: q.Run, Tape: tape, Live: live}
	if tape == nil {
		info.Source = "log"
		info.Content = content
		info.Stale = q.Run < newestRun || stageRunning
		return info, nil
	}
	info.ETag = sidecarETag
	return info, nil
}

// liveActivity reads the tape of a registered in-flight run from the session
// JSONL the agent is appending to. It reports false when this run has no
// session file to read, so the caller can fall back to the plaintext log.
func (d *Daemon) liveActivity(cfg *config.Config, q web.ActivityQuery, lr liveRun) (web.ActivityInfo, bool) {
	if lr.params.SessionID == "" && lr.params.SessionDir == "" {
		return web.ActivityInfo{}, false
	}
	path, isPi := liveSessionFile(cfg, q.ID, lr)

	info := web.ActivityInfo{Source: "events", Stage: q.Stage, Run: q.Run, Live: true}
	if path == "" {
		// The agent has started but written nothing yet. The tape comes from the
		// parser rather than a bare literal, so it carries the version, agent and
		// partial fields the client reads from the first poll on.
		tape := emptyTape(isPi)
		info.Tape = &tape
		return info, true
	}

	info.ETag = fileETag(path, q.Run)
	if info.ETag != "" && info.ETag == q.IfNoneMatch {
		return web.ActivityInfo{NotModified: true, ETag: info.ETag}, true
	}

	tape, err := sessionTape(path, isPi)
	if err != nil {
		return web.ActivityInfo{}, false
	}
	// Everything from the earliest tool still awaiting its result onward may
	// still be rewritten, so a cursor past that point is pulled back to it.
	off := max(0, min(q.After, tape.StableCount()))
	tape.Events = tape.Events[off:]
	info.Tape = &tape
	info.Offset = off
	return info, true
}

// emptyTape is the tape of a run that has produced no session file yet. Parsing
// nothing fills the fields a Tape literal would leave zero.
func emptyTape(isPi bool) logfmt.Tape {
	var tape logfmt.Tape
	if isPi {
		tape, _ = logfmt.EventsPi(bytes.NewReader(nil))
	} else {
		tape, _ = logfmt.Events(bytes.NewReader(nil))
	}
	return tape
}

// fileETag is a validator over a transcript file's metadata, so a poll that
// changes nothing costs one stat instead of a full parse. The run index is
// folded in because two runs of a stage can share a session file. An
// unstattable file yields "", which never matches a client's validator.
func fileETag(path string, run int) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return web.ContentETag(fmt.Appendf(nil, "%d-%d-%d", st.Size(), st.ModTime().UnixNano(), run), "")
}

// GetChanges reports the commits and changed files on a ticket's branch
// relative to the ticket's base branch, or the repository's default branch
// when the ticket sets none, computed at request time so the data stays
// available after the worktree is removed. An empty branch field or a branch
// that no longer exists yields an empty payload rather than an error: the
// branch may have been merged and deleted.
func (d *Daemon) GetChanges(id string) (web.ChangesInfo, error) {
	d.mu.Lock()
	ts, ok := d.tickets[id]
	if !ok {
		d.mu.Unlock()
		return web.ChangesInfo{}, web.ErrTicketNotFound
	}
	branch := ts.ticket.Branch
	ticketPath := ts.ticket.Path
	base := ticketBase(ts.ticket)
	d.mu.Unlock()

	info := web.ChangesInfo{
		Branch:  branch,
		Commits: []web.CommitInfo{},
		Files:   []web.FileChangeInfo{},
	}
	if branch == "" || ticketPath == "" {
		return info, nil
	}
	repoPath := expandTilde(ticketPath)

	baseRef, err := worktree.ResolveBase(repoPath, base)
	if err != nil {
		return web.ChangesInfo{}, err
	}
	if base == "" {
		base = baseRef
	}
	info.Base = base
	info.Remote = gitRemoteURL(repoPath)

	if !gitBranchExists(repoPath, branch) {
		return info, nil
	}

	commits, err := gitCommits(repoPath, baseRef, branch)
	if err != nil {
		return web.ChangesInfo{}, err
	}
	info.Commits = commits

	files, err := gitNumstat(repoPath, baseRef, branch)
	if err != nil {
		return web.ChangesInfo{}, err
	}
	info.Files = files
	return info, nil
}

func gitBranchExists(repoPath, branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoPath
	return cmd.Run() == nil
}

// gitRemoteURL reads the repository's origin. A repository without one, or one
// this cannot render as an address, reports no remote rather than an error: the
// changes payload is still correct without it.
func gitRemoteURL(repoPath string) string {
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return normalizeRemoteURL(string(out))
}

// normalizeRemoteURL rewrites a git remote as the https address of the same
// repository, without the .git suffix.
//
// Two forms need care. git@host:owner/repo.git is scp syntax rather than a URL,
// so it is given a scheme before parsing. And a remote may carry credentials
// (https://user:token@host/...), which url.Parse keeps apart from the host, so
// rebuilding the address from the host drops them instead of publishing a token
// to every reader of the page.
//
// A remote that does not end up as http or https yields "": the result is a
// link target in the browser, where a javascript: URL is not a broken link.
func normalizeRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		host, path, ok := strings.Cut(raw, ":")
		// A local path has no colon, and a Windows drive letter puts the colon
		// in front of a rooted path.
		if !ok || path == "" || strings.HasPrefix(path, "/") || !looksLikeHost(host) {
			return ""
		}
		raw = "ssh://" + host + "/" + path
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Host
	switch u.Scheme {
	case "http", "https":
	case "ssh", "git":
		// An ssh port says nothing about where the web UI listens.
		host = u.Hostname()
	default:
		return ""
	}
	path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	if path == "" || path == "/" {
		return ""
	}
	return "https://" + host + path
}

// looksLikeHost accepts the left half of scp syntax: an optional user, then a
// dotted name. Cutting on the colon alone would read "javascript:alert(1)" as a
// host and a path, and hand back an https URL built out of it.
func looksLikeHost(s string) bool {
	if _, after, ok := strings.Cut(s, "@"); ok {
		s = after
	}
	if !strings.Contains(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// gitCommits lists the commits on branch that are not on base, newest first.
func gitCommits(repoPath, base, branch string) ([]web.CommitInfo, error) {
	cmd := exec.Command("git", "log", "--no-merges", "--format=%h%x09%s", base+".."+branch)
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git log: %s: %w", strings.TrimSpace(string(out)), err)
	}
	commits := []web.CommitInfo{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		sha, subject, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		commits = append(commits, web.CommitInfo{SHA: sha, Subject: subject})
	}
	return commits, nil
}

// gitNumstat lists per-file added/deleted line counts for branch relative to
// its merge base with base. Binary files report zero counts.
func gitNumstat(repoPath, base, branch string) ([]web.FileChangeInfo, error) {
	cmd := exec.Command("git", "diff", "--numstat", base+"..."+branch)
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git diff: %s: %w", strings.TrimSpace(string(out)), err)
	}
	files := []web.FileChangeInfo{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		files = append(files, web.FileChangeInfo{Path: parts[2], Added: added, Deleted: deleted})
	}
	return files, nil
}

// GetRawConfig returns the daemon's on-disk config file contents.
func (d *Daemon) GetRawConfig() (string, error) {
	if d.configPath == "" {
		return "", web.ErrConfigPathNotSet
	}
	data, err := os.ReadFile(d.configPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// PutRawConfig validates content, writes it to the daemon's config file
// atomically, and reloads the running config before returning. Settings outside
// the live-reload set keep their startup values until the daemon restarts; see
// pinRestartOnly. If the write succeeds but the reload fails, the file stays on
// disk and the running config is unchanged.
func (d *Daemon) PutRawConfig(content string) error {
	if d.configPath == "" {
		return web.ErrConfigPathNotSet
	}
	if _, err := config.LoadReader(strings.NewReader(content)); err != nil {
		return fmt.Errorf("%w: %s", web.ErrInvalidConfig, err)
	}
	if err := atomicWriteFile(d.configPath, []byte(content), 0o644); err != nil {
		return err
	}
	// The write also fires the config watcher, so a second, idempotent reload
	// usually follows. Reloading here as well keeps the behaviour the same
	// whether or not the config path is a symlink, which the watcher's coverage
	// depends on.
	if err := d.reloadConfig(); err != nil {
		return fmt.Errorf("config saved but reload failed: %w", err)
	}
	return nil
}

// atomicWriteFile writes data to a temp file in the same directory and renames
// it over path, so a crash or concurrent reader never sees a half-written file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".kontora-config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Subscribe returns a channel that receives ticket events and an unsubscribe function.
func (d *Daemon) Subscribe() (<-chan web.TicketEvent, func()) {
	return d.broker.Subscribe()
}

// HasTerminalSession returns true if a tmux session exists for the given ticket.
func (d *Daemon) HasTerminalSession(id string) bool {
	return tmux.HasWindow(d.tmuxSession, id)
}

// broadcastTicketUpdate sends a ticket_updated event for the given ticket ID.
// Must be called with d.mu held.
func (d *Daemon) broadcastTicketUpdate(id string) {
	if d.broker == nil {
		return
	}
	ts, ok := d.tickets[id]
	if !ok {
		return
	}
	info := d.buildTicketInfo(d.config(), ts, true)
	// No subscriber renders the body off this event: the board never shows it
	// and both detail views fetch their own copy. It is the largest field by
	// far, and the event stream is excluded from gzip.
	info.Body = ""
	d.broker.Broadcast(web.TicketEvent{Type: "ticket_updated", Ticket: info})
}

// broadcastTicketUpdateLocking is like broadcastTicketUpdate but acquires d.mu.
func (d *Daemon) broadcastTicketUpdateLocking(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.broadcastTicketUpdate(id)
}

// broadcastTicketDeleted sends a ticket_deleted event for the given ticket.
// Must be called with d.mu held.
func (d *Daemon) broadcastTicketDeleted(ts *ticketState) {
	if d.broker == nil || ts == nil {
		return
	}
	d.broker.Broadcast(web.TicketEvent{
		Type:   "ticket_deleted",
		Ticket: d.buildTicketInfo(d.config(), ts, false),
	})
}

// broadcastTerminalReady sends a terminal_ready event for the given ticket ID.
// Called from the runner callback after the tmux window is created.
func (d *Daemon) broadcastTerminalReady(id string) {
	if d.broker == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ts, ok := d.tickets[id]
	if !ok {
		return
	}
	d.broker.Broadcast(web.TicketEvent{
		Type:   "terminal_ready",
		Ticket: d.buildTicketInfo(d.config(), ts, false),
	})
}

// buildTicketInfo converts internal ticket state to a web.TicketInfo. The
// caller passes the config snapshot so that every ticket in one response is
// rendered from the same config version, whatever a concurrent reload does.
// Must be called with d.mu held.
func (d *Daemon) buildTicketInfo(cfg *config.Config, ts *ticketState, includeBody bool) web.TicketInfo {
	v := app.BuildView(cfg, ts.ticket, includeBody)
	info := web.TicketInfoFromView(v)
	if strings.TrimSpace(info.Branch) == "" {
		info.AutoBranch = autoTicketBranch(cfg, ts.ticket)
	}
	mt := ts.modTime
	if mt.IsZero() && ts.filePath != "" {
		if st, err := os.Stat(ts.filePath); err == nil {
			mt = st.ModTime()
		}
	}
	if !mt.IsZero() {
		info.UpdatedAt = &mt
	}
	// Relations are resolved for the detail projection only. A board card shows
	// the ids it already has; the rail is what names the other ticket.
	if includeBody {
		for i := range info.Deps {
			d.resolveRefLocked(&info.Deps[i])
		}
		for i := range info.Links {
			d.resolveRefLocked(&info.Links[i])
		}
		d.resolveRefLocked(info.Parent)
		info.Blocks = d.dependentsLocked(ts.ticket.ID)
	}
	return info
}

// resolveRefLocked fills a relation ref's title and status from the store,
// which holds every ticket file on disk and not only the ones the board shows.
// An id with no ticket left keeps its bare form, and the UI renders that as
// text rather than a link.
// Must be called with d.mu held.
func (d *Daemon) resolveRefLocked(ref *web.TicketRef) {
	if ref == nil {
		return
	}
	ts, ok := d.tickets[ref.ID]
	if !ok {
		return
	}
	ref.Title = ts.ticket.Title()
	ref.Status = string(ts.ticket.Status)
}

// dependentsLocked lists the tickets whose deps name id: what this ticket holds
// up. The edge is stored on the blocked ticket alone, so the reverse direction
// has to be read off every other ticket. Sorted by id, because a map is not.
// Must be called with d.mu held.
func (d *Daemon) dependentsLocked(id string) []web.TicketRef {
	var refs []web.TicketRef
	for otherID, ts := range d.tickets {
		if otherID == id || !slices.Contains(ts.ticket.Deps, id) {
			continue
		}
		refs = append(refs, web.TicketRef{
			ID:     otherID,
			Title:  ts.ticket.Title(),
			Status: string(ts.ticket.Status),
		})
	}
	slices.SortFunc(refs, func(a, b web.TicketRef) int { return strings.Compare(a.ID, b.ID) })
	return refs
}

// mapAppError translates app-level sentinel errors to web-level sentinel errors
// so that HTTP handlers and tests that check for web.ErrInvalidState etc. continue
// to work correctly.
func mapAppError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, app.ErrNotFound) {
		return web.ErrTicketNotFound
	}
	if errors.Is(err, app.ErrInvalidState) {
		return web.ErrInvalidState
	}
	if errors.Is(err, app.ErrUnknownAgent) {
		return web.ErrUnknownAgent
	}
	return err
}

// removeQueuedLocked drops a ticket from the dedupe map and scheduler heap.
// Must be called with d.mu held.
func (d *Daemon) removeQueuedLocked(id string) {
	delete(d.queued, id)
	if d.queue.Len() == 0 {
		return
	}
	filtered := make(priorityQueue, 0, len(d.queue))
	for _, item := range d.queue {
		if item.ticketID != id {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == len(d.queue) {
		return
	}
	d.queue = filtered
	heap.Init(&d.queue)
}
