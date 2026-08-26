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
func (d *Daemon) ListTickets(opts web.ListTicketsOptions) []web.TicketInfo {
	d.mu.Lock()
	defer d.mu.Unlock()

	cfg := d.config()
	tickets := make([]web.TicketInfo, 0, len(d.tickets))
	for _, ts := range d.tickets {
		// Only tickets whose status maps to a board column appear in the list.
		// This hides archived, plus foreign statuses (closed, tombstone, ...)
		// that have no column. All of them stay on disk and remain reachable
		// by ID via GetTicket, and a caller that asks for the complete set
		// gets them here too.
		if !opts.IncludeHidden && !cfg.IsBoardStatus(string(ts.ticket.Status)) {
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
	id, err := cli.GenerateID(cfg, req.Path)
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
	repoPath := t.Path
	if repoPath == "" {
		repoPath = "upload"
	}
	id, err := cli.GenerateID(cfg, repoPath)
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
		Author:         cfg.Author,
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
	// The reactions sidecar is invisible to the store, so nothing else would
	// ever collect it.
	if err := ticket.RemoveNoteSidecar(filepath.Dir(filePath), id); err != nil {
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
		status := ts.ticket.Status
		d.mu.Unlock()
		return fmt.Errorf("%w: cannot pause ticket %s in status %s (only a running ticket can be paused)", web.ErrInvalidState, id, status)
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
//
// The whole read-modify-write holds d.mu, because a scheduler pickup takes the
// same lock to claim a ticket. Without that, a pickup could claim the ticket
// between the read and the write, and this write would then put back a ticket
// parsed before the claim, dropping status, started_at and claimed_by.
func (d *Daemon) SetStage(id string, stage string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	ts, ok := d.tickets[id]
	if !ok {
		return web.ErrTicketNotFound
	}

	// A running agent owns the ticket file. A stage written under it is lost
	// when the run writes the file back at the end of the stage. d.running is
	// registered when the scheduler claims the ticket, before the file says
	// in_progress, so it catches a pickup the cached status has not seen yet.
	_, running := d.running[id]
	if running || ts.ticket.Status == ticket.StatusInProgress {
		return fmt.Errorf("%w: cannot change the stage of ticket %s while it is running", web.ErrInvalidState, id)
	}

	pipelineName := ts.ticket.Pipeline
	filePath := ts.filePath

	pipelineCfg, ok := d.config().Pipelines[pipelineName]
	if !ok {
		if pipelineName == "" {
			return fmt.Errorf("%w: ticket %s has no pipeline, so it has no stages", web.ErrInvalidState, id)
		}
		return fmt.Errorf("%w: unknown pipeline %q for ticket %s", web.ErrInvalidState, pipelineName, id)
	}

	stageNames := make([]string, len(pipelineCfg))
	for i, s := range pipelineCfg {
		stageNames[i] = s.Stage
	}
	if !slices.Contains(stageNames, stage) {
		return fmt.Errorf("%w: stage %q not found in pipeline %q (has %s)", web.ErrInvalidState, stage, pipelineName, strings.Join(stageNames, ", "))
	}

	// Read from disk rather than from the cache: the file is the state a pickup
	// would act on.
	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return err
	}

	if err := t2.SetField("stage", stage); err != nil {
		return fmt.Errorf("failed to set ticket stage to %q: %w", stage, err)
	}

	if err := d.writeTicketLocked(t2, filePath); err != nil {
		return err
	}

	d.setTicketState(id, t2, filePath)
	d.broadcastTicketUpdate(id)
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
				return fmt.Errorf("%w: unknown status %q", web.ErrInvalidState, newStatus)
			}
		}
		_, err := d.svc.SetStatus(id, ticket.Status(newStatus))
		return mapAppError(err)
	}
}

// ListArchivedTickets returns the Archive view's rows: every ticket in the
// store whose status is archived, in no particular order — the view sorts.
func (d *Daemon) ListArchivedTickets() []web.ArchivedTicketInfo {
	d.mu.Lock()
	defer d.mu.Unlock()

	cfg := d.config()
	rows := make([]web.ArchivedTicketInfo, 0)
	for _, ts := range d.tickets {
		t := ts.ticket
		if t.ID == "" || t.Status != ticket.StatusArchived {
			continue
		}
		project, _, _ := cfg.ProjectFor(t.Path)
		row := web.ArchivedTicketInfo{
			ID:       t.ID,
			Title:    t.Title(),
			Project:  project,
			Pipeline: t.Pipeline,
			// The agent is resolved, not read: a ticket with no agent field
			// takes its pipeline step's, or the configured default.
			Agent:       app.BuildView(cfg, t, false).Agent,
			Branch:      t.Branch,
			Path:        t.Path,
			Status:      string(t.ArchivedFrom),
			WallSeconds: historyWallSeconds(t.History),
			ArchivedAt:  t.ArchivedAt,
			ArchivedBy:  t.ArchivedBy,
		}
		// A ticket archived before the stamp existed still needs a date to sort
		// and render by; the file mtime is what the sweep's own cutoff uses.
		if row.ArchivedAt == nil && !ts.modTime.IsZero() {
			mt := ts.modTime
			row.ArchivedAt = &mt
		}
		rows = append(rows, row)
	}
	return rows
}

// historyWallSeconds is the interval from the first run that started to the
// last one that completed, so a ticket that ran three times reports the span of
// all three rather than the newest. Zero when no run has both ends.
func historyWallSeconds(history []ticket.HistoryEntry) int {
	var first, last *time.Time
	for i := range history {
		if h := history[i].StartedAt; h != nil && first == nil {
			first = h
		}
		if h := history[i].CompletedAt; h != nil {
			last = h
		}
	}
	if first == nil || last == nil {
		return 0
	}
	secs := int(last.Sub(*first).Seconds())
	if secs < 0 {
		return 0
	}
	return secs
}

// ArchiveTicket archives one closed ticket from the web UI.
func (d *Daemon) ArchiveTicket(id string, note string) error {
	_, err := d.svc.ArchiveTicket(id, note, app.ArchivedByWeb)
	return mapAppError(err)
}

// RestoreTicket returns an archived ticket to the status it was archived from.
func (d *Daemon) RestoreTicket(id string) error {
	_, err := d.svc.RestoreTicket(id)
	return mapAppError(err)
}

// AddNote appends a note to a ticket's body, using the same persistence as the
// local cli.Note path. An empty author signs as the configured one, so a note
// from the web composer carries the name of the person running the daemon.
func (d *Daemon) AddNote(id string, req web.AddNoteRequest) error {
	author := req.Author
	if author == "" {
		author = d.config().Author
	}
	return d.mutateNotes(id, func(t *ticket.Ticket) error {
		_, err := t.AddNote(ticket.AddNoteOptions{
			Text:     req.Text,
			Author:   author,
			ParentID: req.Parent,
			At:       time.Now(),
		})
		return err
	})
}

// EditNote replaces one note's text and flags its byline as edited.
func (d *Daemon) EditNote(id, noteID, text string) error {
	return d.mutateNotes(id, func(t *ticket.Ticket) error {
		_, err := t.EditNote(noteID, text)
		return err
	})
}

// DeleteNote removes one note and its replies.
func (d *Daemon) DeleteNote(id, noteID string) error {
	return d.mutateNotes(id, func(t *ticket.Ticket) error {
		return t.DeleteNote(noteID)
	})
}

// SetReaction toggles one actor's reaction on a note. The chip lives in the
// ticket's sidecar rather than its body, but the write still broadcasts, so
// every open page sees it move. A note that has no id yet is minted one first,
// because the sidecar keys on that id.
func (d *Daemon) SetReaction(id, noteID, emoji, actor string, on bool) error {
	cfg := d.config()
	if actor == "" {
		actor = cfg.Author
	}
	dir := config.ExpandTilde(cfg.TicketsDir)

	resolved := noteID
	if err := d.mutateNotes(id, func(t *ticket.Ticket) error {
		minted, changed, err := t.EnsureNoteID(noteID)
		if err != nil {
			return err
		}
		resolved = minted
		if !changed {
			return errNoteUnchanged
		}
		return nil
	}); err != nil && !errors.Is(err, errNoteUnchanged) {
		return err
	}

	if err := ticket.SetNoteReaction(dir, id, resolved, emoji, actor, on); err != nil {
		return err
	}
	d.broadcastTicketUpdateLocking(id)
	return nil
}

// errNoteUnchanged tells mutateNotes that the mutation found nothing to write.
// It never leaves the daemon.
var errNoteUnchanged = errors.New("note unchanged")

// mutateNotes reads a ticket from disk, applies one note mutation, writes it
// back and broadcasts. The ticket is re-read rather than taken from the cache
// so a note never clobbers a field the daemon wrote since the last watch event.
func (d *Daemon) mutateNotes(id string, mutate func(*ticket.Ticket) error) error {
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
	if err := mutate(t2); err != nil {
		return err
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

// AddDependency records that a ticket waits on another one. Like every relation
// mutation it goes through the application service, so the daemon and the CLI
// reject the same calls and the queue is reconciled the same way.
func (d *Daemon) AddDependency(id string, dependencyID string) error {
	_, err := d.svc.AddDependency(id, dependencyID)
	return mapAppError(err)
}

// RemoveDependency drops a dependency edge.
func (d *Daemon) RemoveDependency(id string, dependencyID string) error {
	_, err := d.svc.RemoveDependency(id, dependencyID)
	return mapAppError(err)
}

// LinkTickets relates a ticket to each of relatedIDs, on both sides.
func (d *Daemon) LinkTickets(id string, relatedIDs []string) error {
	_, err := d.svc.Link(id, relatedIDs...)
	return mapAppError(err)
}

// UnlinkTickets removes the relation between a ticket and each of relatedIDs.
func (d *Daemon) UnlinkTickets(id string, relatedIDs []string) error {
	_, err := d.svc.Unlink(id, relatedIDs...)
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

	if !d.config().StatusAllowsEdit(string(status)) {
		return fmt.Errorf("%w: cannot edit ticket %s in status %s", web.ErrInvalidState, id, status)
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
		if info, ok := d.liveActivity(q, lr); ok {
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
func (d *Daemon) liveActivity(q web.ActivityQuery, lr liveRun) (web.ActivityInfo, bool) {
	if lr.params.SessionID == "" && lr.params.SessionDir == "" {
		return web.ActivityInfo{}, false
	}
	path, isPi := liveSessionFile(lr)

	info := web.ActivityInfo{Source: "events", Stage: q.Stage, Run: q.Run, Live: true}
	if path == "" {
		// The agent has started but written nothing yet. The tape comes from the
		// parser rather than a bare literal, so it carries the version and agent
		// fields the client reads from the first poll on.
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
	info.Tape = &tape
	info.Offset = tape.SliceAt(q.After)
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

// chainNodeCap bounds the ladder the ticket page draws. The critical path is
// kept whatever the cap, so a chain too wide to render still names what is
// holding it up.
const chainNodeCap = 200

// GetChain walks the dependency closure around a ticket. The index is built
// from the whole store, not from the board list, so archived tickets and
// statuses with no column stay in the graph.
func (d *Daemon) GetChain(id string) (web.ChainInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.tickets[id]; !ok {
		return web.ChainInfo{}, web.ErrTicketNotFound
	}
	index := make(map[string]*ticket.Ticket, len(d.tickets))
	for tid, ts := range d.tickets {
		index[tid] = ts.ticket
	}

	res := ticket.Chain(index, id, chainNodeCap)
	info := web.ChainInfo{
		ID:         res.ID,
		Verdict:    res.Verdict,
		Position:   res.Position,
		PathLength: res.PathLength,
		Goal:       res.Goal,
		Total:      res.Total,
		Done:       res.Done,
		Cycle:      res.Cycle,
		Nodes:      make([]web.ChainNode, 0, len(res.Nodes)),
	}
	for _, n := range res.Nodes {
		node := web.ChainNode{
			ID:             n.ID,
			Depth:          n.Depth,
			Direction:      n.Direction,
			OnCriticalPath: n.OnCriticalPath,
			Resolved:       n.Resolved,
			HoldsChain:     n.HoldsChain,
			WaitsOn:        web.ChainWaits{Open: n.WaitsOpen, Total: n.WaitsTotal},
			Missing:        n.Missing,
		}
		if ts, ok := d.tickets[n.ID]; ok {
			node.Title = ts.ticket.Title()
			node.Status = string(ts.ticket.Status)
		}
		info.Nodes = append(info.Nodes, node)
	}
	return info, nil
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
	info.CanAnnotate = annotateRefusal(ts.ticket) == nil
	info.Project, _, _ = cfg.ProjectFor(ts.ticket.Path)
	d.decorateNotes(cfg, ts.ticket.ID, info.Notes)
	info.Blockers = d.blockersLocked(ts.ticket)
	info.Ready = len(info.Blockers) == 0
	mt := ts.modTime
	if mt.IsZero() && ts.filePath != "" {
		if st, err := os.Stat(ts.filePath); err == nil {
			mt = st.ModTime()
		}
	}
	if !mt.IsZero() {
		info.UpdatedAt = &mt
	}
	if w, ok := d.waiting[ts.ticket.ID]; ok {
		since := w.Since
		info.WaitingForInput = true
		info.WaitingSince = &since
		info.WaitingTool = w.Tool
		info.WaitingQuestion = w.Question
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
		info.Blocks, info.Children = d.relatedLocked(cfg, ts.ticket.ID)
	}
	return info
}

// decorateNotes fills in what the body alone cannot say: the author's kind,
// which only the config knows, and the reactions, which live in the sidecar.
// Both are added here rather than in web.ParseNotes so the SSE event carries
// them too — it builds its payload through this same function.
func (d *Daemon) decorateNotes(cfg *config.Config, ticketID string, notes []web.NoteInfo) {
	if len(notes) == 0 {
		return
	}
	reactions, err := ticket.LoadNoteReactions(config.ExpandTilde(cfg.TicketsDir), ticketID)
	if err != nil {
		d.log.Warn("reading note reactions", "ticket", ticketID, "err", err)
	}
	for i := range notes {
		notes[i].AuthorKind = authorKind(cfg, notes[i].Author)
		for _, chip := range reactions.For(notes[i].ID) {
			notes[i].Reactions = append(notes[i].Reactions, web.ReactionInfo{Emoji: chip.Emoji, Actors: chip.Actors})
		}
	}
}

// authorKind sorts a note's author into the three the UI draws differently. An
// author no agent is named after is a person; an empty one is a note written
// before notes carried an author at all, and stays unclassified.
func authorKind(cfg *config.Config, author string) string {
	switch author {
	case "":
		return ""
	case ticket.SystemAuthor:
		return web.AuthorKindSystem
	default:
		if _, ok := cfg.Agents[author]; ok {
			return web.AuthorKindAgent
		}
		return web.AuthorKindHuman
	}
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

// relatedLocked reads the two reverse edges off the store in one pass: the
// tickets whose deps name id (what this one holds up) and the tickets whose
// parent names it (its sub-tickets). Neither edge is stored on this ticket, so
// both have to be read off every other one, and this runs on every detail build
// and every SSE broadcast — hence one loop rather than two. Both are sorted by
// id, because a map is not.
// Must be called with d.mu held.
func (d *Daemon) relatedLocked(cfg *config.Config, id string) (blocks []web.TicketRef, children []web.TicketChild) {
	for otherID, ts := range d.tickets {
		if otherID == id {
			continue
		}
		if slices.Contains(ts.ticket.Deps, id) {
			blocks = append(blocks, web.TicketRef{
				ID:     otherID,
				Title:  ts.ticket.Title(),
				Status: string(ts.ticket.Status),
			})
		}
		if ts.ticket.Parent == id {
			children = append(children, childInfo(cfg, ts.ticket))
		}
	}
	slices.SortFunc(blocks, func(a, b web.TicketRef) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(children, func(a, b web.TicketChild) int { return strings.Compare(a.ID, b.ID) })
	return blocks, children
}

// childInfo projects one sub-ticket for the tree. The wall bounds come from the
// history when there is one: the frontmatter's started_at holds the current
// stage's pickup, so a child that ran four stages would otherwise report only
// the fourth. A running child gets no CompletedAt, and the page clocks it live.
func childInfo(cfg *config.Config, t *ticket.Ticket) web.TicketChild {
	c := web.TicketChild{
		ID:     t.ID,
		Title:  t.Title(),
		Status: string(t.Status),
		Stage:  t.Stage,
	}
	if steps, ok := cfg.Pipelines[t.Pipeline]; ok {
		c.StageCount = len(steps)
		for i, step := range steps {
			if step.Stage == t.Stage {
				c.StageIndex = i + 1
				break
			}
		}
	}
	c.StartedAt = t.StartedAt
	if len(t.History) > 0 {
		if h := t.History[0]; h.StartedAt != nil {
			c.StartedAt = h.StartedAt
		}
	}
	if t.Status != ticket.StatusInProgress {
		c.CompletedAt = app.FinishedAt(t)
	}
	return c
}

// mappedError reports a web sentinel to errors.Is while printing the app
// layer's message. Replacing the error outright would drop the only part a user
// can act on: "invalid state transition" alone does not say which ticket, or
// which status it is already in.
type mappedError struct {
	sentinel error
	cause    error
}

func (e *mappedError) Error() string { return e.cause.Error() }
func (e *mappedError) Unwrap() error { return e.cause }
func (e *mappedError) Is(target error) bool {
	return target == e.sentinel
}

// mapAppError translates app-level sentinel errors to web-level sentinel errors
// so that HTTP handlers and tests that check for web.ErrInvalidState etc.
// continue to work, while the message the app layer wrote reaches the client.
func mapAppError(err error) error {
	if err == nil {
		return nil
	}
	for _, m := range []struct{ from, to error }{
		{app.ErrNotFound, web.ErrTicketNotFound},
		{app.ErrInvalidState, web.ErrInvalidState},
		{app.ErrUnknownAgent, web.ErrUnknownAgent},
		// A refused relation is a conflict with the graph the store already
		// holds, not a malformed request, so it maps to the same 409 as every
		// other "the store will not allow this" rejection.
		{app.ErrRelationCycle, web.ErrInvalidState},
		{app.ErrSelfRelation, web.ErrInvalidState},
	} {
		if errors.Is(err, m.from) {
			return &mappedError{sentinel: m.to, cause: err}
		}
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
	d.syncQueueDepthLocked()
}
