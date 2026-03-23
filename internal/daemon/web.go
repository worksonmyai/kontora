package daemon

import (
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/tmux"
	"github.com/worksonmyai/kontora/internal/web"
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

	tickets := make([]web.TicketInfo, 0, len(d.tickets))
	for _, ts := range d.tickets {
		tickets = append(tickets, d.buildTicketInfo(ts, false))
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
	return d.buildTicketInfo(ts, true), nil
}

// CreateTicket creates a new ticket file and registers it in the daemon.
func (d *Daemon) CreateTicket(req web.CreateTicketRequest) (web.TicketInfo, error) {
	id, err := cli.GenerateID(d.cfg.TicketsDir, req.Path)
	if err != nil {
		return web.TicketInfo{}, fmt.Errorf("generating ticket id: %w", err)
	}

	filePath := filepath.Join(config.ExpandTilde(d.cfg.TicketsDir), id+".md")

	if req.Agent != "" {
		if _, ok := d.cfg.Agents[req.Agent]; !ok {
			return web.TicketInfo{}, fmt.Errorf("%w %q", web.ErrUnknownAgent, req.Agent)
		}
	}

	d.recordSelfWrite(filePath)

	_, err = cli.New(d.cfg, cli.NewOpts{
		ID:       id,
		Path:     req.Path,
		Pipeline: req.Pipeline,
		Agent:    req.Agent,
		Status:   req.Status,
		Title:    req.Title,
		Body:     req.Body,
		Branch:   req.Branch,
		NoEdit:   true,
	})
	if err != nil {
		return web.TicketInfo{}, fmt.Errorf("creating ticket: %w", err)
	}

	t, err := ticket.ParseFile(filePath)
	if err != nil {
		return web.TicketInfo{}, fmt.Errorf("parsing created ticket: %w", err)
	}

	d.mu.Lock()
	ts := &ticketState{ticket: t, filePath: filePath}
	d.tickets[id] = ts
	if t.Status == "todo" {
		d.enqueue(t)
	}
	info := d.buildTicketInfo(ts, false)
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()

	return info, nil
}

// UploadTicket imports a ticket from raw .md file content.
func (d *Daemon) UploadTicket(content []byte) (web.TicketInfo, error) {
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
	id, err := cli.GenerateID(d.cfg.TicketsDir, prefix)
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

	filePath := filepath.Join(config.ExpandTilde(d.cfg.TicketsDir), t.ID+".md")
	if err := d.writeTicket(t, filePath); err != nil {
		return web.TicketInfo{}, fmt.Errorf("writing ticket file: %w", err)
	}

	d.mu.Lock()
	ts := &ticketState{ticket: t, filePath: filePath}
	d.tickets[t.ID] = ts
	info := d.buildTicketInfo(ts, false)
	d.broadcastTicketUpdate(t.ID)
	d.mu.Unlock()

	return info, nil
}

// GetConfig returns available pipelines and agents from the daemon config.
func (d *Daemon) GetConfig() web.ConfigInfo {
	pipelines := slices.Sorted(maps.Keys(d.cfg.Pipelines))
	infos := make([]web.PipelineInfo, len(pipelines))
	for i, name := range pipelines {
		stages := d.cfg.Pipelines[name]
		stageNames := make([]string, len(stages))
		for j, s := range stages {
			stageNames[j] = s.Stage
		}
		infos[i] = web.PipelineInfo{Name: name, Stages: stageNames}
	}
	agents := slices.Sorted(maps.Keys(d.cfg.Agents))
	return web.ConfigInfo{
		Pipelines:     pipelines,
		PipelineInfos: infos,
		Agents:        agents,
		BranchPrefix:  d.cfg.BranchPrefix,
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
	ticketsDir, err := filepath.Abs(config.ExpandTilde(d.cfg.TicketsDir))
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
	d.tickets[id] = &ticketState{ticket: t2, filePath: filePath}
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// RetryTicket resets a non-running ticket to todo and re-enqueues it.
func (d *Daemon) RetryTicket(id string) error {
	_, err := d.svc.Retry(id)
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

	pipelineCfg, ok := d.cfg.Pipelines[pipelineName]
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
	d.tickets[id] = &ticketState{ticket: t2, filePath: filePath}
	d.broadcastTicketUpdate(id)
	d.mu.Unlock()
	return nil
}

// MoveTicket sets a ticket's status to newStatus with transition validation.
func (d *Daemon) MoveTicket(id string, newStatus string) error {
	switch newStatus {
	case "paused":
		return d.PauseTicket(id)
	case "todo":
		return d.RetryTicket(id)
	default:
		switch newStatus {
		case "open", "done", "cancelled":
			// valid
		default:
			return web.ErrInvalidState
		}
		_, err := d.svc.SetStatus(id, ticket.Status(newStatus))
		return mapAppError(err)
	}
}

// InitTicket initializes a non-kontora ticket: sets pipeline, path, kontora=true,
// status=todo, stage to the first pipeline stage, and enqueues it.
func (d *Daemon) InitTicket(id string, req web.InitTicketRequest) error {
	_, err := d.svc.Init(id, app.InitRequest{
		Pipeline: req.Pipeline,
		Path:     req.Path,
		Agent:    req.Agent,
	})
	return mapAppError(err)
}

// UpdateTicket updates body and frontmatter fields of a ticket.
// Allowed in statuses: open, todo, paused.
func (d *Daemon) UpdateTicket(id string, req web.UpdateTicketRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	ts, ok := d.tickets[id]
	if !ok {
		return web.ErrTicketNotFound
	}
	switch ts.ticket.Status {
	case ticket.StatusOpen, ticket.StatusTodo, ticket.StatusPaused:
		// allowed
	case ticket.StatusInProgress, ticket.StatusDone, ticket.StatusCancelled:
		return web.ErrInvalidState
	}

	filePath := ts.filePath

	t2, err := ticket.ParseFile(filePath)
	if err != nil {
		return err
	}

	if req.Pipeline != nil {
		if *req.Pipeline != "" {
			if _, ok := d.cfg.Pipelines[*req.Pipeline]; !ok {
				return fmt.Errorf("unknown pipeline %q", *req.Pipeline)
			}
		}
		if err := t2.SetField("pipeline", *req.Pipeline); err != nil {
			return err
		}
	}
	if req.Path != nil {
		if err := t2.SetField("path", *req.Path); err != nil {
			return err
		}
	}
	if req.Agent != nil {
		if *req.Agent != "" {
			if _, ok := d.cfg.Agents[*req.Agent]; !ok {
				return fmt.Errorf("%w %q", web.ErrUnknownAgent, *req.Agent)
			}
		}
		if err := t2.SetField("agent", *req.Agent); err != nil {
			return err
		}
	}
	if req.Branch != nil {
		if err := t2.SetField("branch", *req.Branch); err != nil {
			return err
		}
	}
	if req.Body != nil {
		t2.SetBody(*req.Body)
	}

	if err := d.writeTicket(t2, filePath); err != nil {
		return err
	}

	d.tickets[id] = &ticketState{ticket: t2, filePath: filePath}
	d.broadcastTicketUpdate(id)
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

	var buf bytes.Buffer
	if err := cli.Logs(d.cfg.TicketsDir, d.cfg.LogsDir, id, stage, &buf); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", web.ErrLogNotFound
		}
		return "", err
	}
	return buf.String(), nil
}

// Subscribe returns a channel that receives ticket events and an unsubscribe function.
func (d *Daemon) Subscribe() (<-chan web.TicketEvent, func()) {
	return d.broker.Subscribe()
}

// HasTerminalSession returns true if a tmux session exists for the given ticket.
func (d *Daemon) HasTerminalSession(id string) bool {
	return tmux.HasWindow(tmux.DefaultSessionName, id)
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
	d.broker.Broadcast(web.TicketEvent{
		Type:   "ticket_updated",
		Ticket: d.buildTicketInfo(ts, true),
	})
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
		Ticket: d.buildTicketInfo(ts, false),
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
		Ticket: d.buildTicketInfo(ts, false),
	})
}

// buildTicketInfo converts internal ticket state to a web.TicketInfo.
// Must be called with d.mu held.
func (d *Daemon) buildTicketInfo(ts *ticketState, includeBody bool) web.TicketInfo {
	v := app.BuildView(d.cfg, ts.ticket, includeBody)
	return web.TicketInfoFromView(v)
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
