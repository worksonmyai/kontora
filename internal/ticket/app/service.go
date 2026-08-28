package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

var (
	ErrNotFound     = errors.New("ticket not found")
	ErrInvalidState = errors.New("invalid state transition")
	ErrUnknownAgent = errors.New("unknown agent")
)

// builtinSetStatuses lists the statuses that SetStatus accepts. StatusArchived
// is deliberately excluded: archived is terminal and may only be reached via the
// archive command or a direct file edit, not WebUI/API moves.
var builtinSetStatuses = map[ticket.Status]bool{
	ticket.StatusOpen:        true,
	ticket.StatusTodo:        true,
	ticket.StatusPaused:      true,
	ticket.StatusHumanReview: true,
	ticket.StatusDone:        true,
	ticket.StatusCancelled:   true,
}

// closedStatuses are the statuses a ticket reaches when its work is over.
var closedStatuses = map[ticket.Status]bool{
	ticket.StatusDone:      true,
	ticket.StatusCancelled: true,
	ticket.StatusArchived:  true,
}

// ConfigFunc returns the config to use for the current call. The daemon passes
// its own accessor so the service follows a config reload; callers with a fixed
// config pass Static.
type ConfigFunc func() *config.Config

// Static wraps a fixed config as a ConfigFunc.
func Static(cfg *config.Config) ConfigFunc {
	return func() *config.Config { return cfg }
}

// Service owns ticket use-cases: mutations, projection, and listing.
type Service struct {
	cfg     ConfigFunc
	repo    Repository
	runtime RuntimeHooks
}

// New creates a Service.
func New(cfg ConfigFunc, repo Repository, runtime RuntimeHooks) *Service {
	return &Service{cfg: cfg, repo: repo, runtime: runtime}
}

// Get retrieves a single ticket by ID (supports prefix matching).
func (s *Service) Get(id string, opts GetOptions) (View, error) {
	resolved, err := s.repo.Resolve(id)
	if err != nil {
		return View{}, err
	}
	st, err := s.repo.Get(resolved)
	if err != nil {
		return View{}, err
	}
	return BuildView(s.cfg(), st.Ticket, opts.IncludeBody), nil
}

// List returns views of all valid tickets. Markdown files that parse but do not
// declare an id are treated as non-ticket notes and remain hidden.
func (s *Service) List(_ ListOptions) ([]View, error) {
	stored, err := s.repo.List()
	if err != nil {
		return nil, err
	}
	cfg := s.cfg()
	var views []View
	for _, st := range stored {
		if st.Ticket.ID == "" {
			continue
		}
		views = append(views, BuildView(cfg, st.Ticket, false))
	}
	return views, nil
}

func (s *Service) isValidSetStatus(status ticket.Status) bool {
	if builtinSetStatuses[status] {
		return true
	}
	return s.cfg().IsCustomStatus(string(status))
}

// refuseEpic reports the error for an operation that would run or move an epic
// by hand. An epic's status is derived from its children, so a write here would
// be overwritten by the next derivation pass, and nothing about an epic is
// runnable.
func refuseEpic(t *ticket.Ticket, id, verb string) error {
	if t.Kind != ticket.KindEpic {
		return nil
	}
	return fmt.Errorf("%w: cannot %s epic %s, its status is derived from its children", ErrInvalidState, verb, id)
}

// SetStatus changes a ticket's status with validation.
func (s *Service) SetStatus(id string, status ticket.Status) (Result, error) {
	if !s.isValidSetStatus(status) {
		return Result{}, fmt.Errorf("invalid status %q", status)
	}

	resolved, err := s.repo.Resolve(id)
	if err != nil {
		return Result{}, err
	}
	st, err := s.repo.Get(resolved)
	if err != nil {
		return Result{}, err
	}

	if err := refuseEpic(st.Ticket, resolved, "move"); err != nil {
		return Result{}, err
	}

	if st.Ticket.Status == status {
		return Result{}, fmt.Errorf("%w: ticket %s is already %s", ErrInvalidState, resolved, status)
	}

	// Archived is terminal: the archive sweep and a direct file edit are the only
	// ways in, and there is no way back out through a move.
	if st.Ticket.Status == ticket.StatusArchived {
		return Result{}, fmt.Errorf("%w: ticket %s is archived", ErrInvalidState, resolved)
	}

	// Paused means "a run was stopped and parked". A ticket that already closed
	// has no run to stop, so the move would write a status the daemon never
	// produces; retry is the way back from done or cancelled.
	if status == ticket.StatusPaused && closedStatuses[st.Ticket.Status] {
		return Result{}, fmt.Errorf("%w: cannot pause ticket %s in status %s (use retry to reopen it)", ErrInvalidState, resolved, st.Ticket.Status)
	}

	if err := st.Ticket.SetField("kontora", true); err != nil {
		return Result{}, fmt.Errorf("setting kontora: %w", err)
	}
	if err := st.Ticket.SetField("status", string(status)); err != nil {
		return Result{}, fmt.Errorf("setting status: %w", err)
	}
	if err := st.Ticket.SetField("last_error", ""); err != nil {
		return Result{}, fmt.Errorf("clearing last_error: %w", err)
	}
	// A user who moves the ticket somewhere else decides where it belongs. Leaving
	// the marker set would let a later pickup run the pending annotations and then
	// restore the status the ticket had before the park, overriding this move.
	if err := st.Ticket.SetField("annotation_return_status", ""); err != nil {
		return Result{}, fmt.Errorf("clearing annotation_return_status: %w", err)
	}
	// A schedule only means "move this open ticket to todo later". A move
	// anywhere else answers that already, so leaving the timestamp behind would
	// have the daemon promote the ticket a second time.
	if status != ticket.StatusOpen {
		if err := st.Ticket.ClearSchedule(); err != nil {
			return Result{}, fmt.Errorf("clearing %s: %w", ticket.FieldScheduledAt, err)
		}
	}

	if status == ticket.StatusDone {
		now := time.Now().UTC()
		if err := st.Ticket.SetField("completed_at", now); err != nil {
			return Result{}, fmt.Errorf("setting completed_at: %w", err)
		}
	}

	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	// Stop any running agent (and its tmux window). SetStatus only moves a
	// ticket to non-running statuses, so an in_progress ticket being moved
	// must have its agent torn down. The status was saved as a daemon
	// self-write, so the file watcher won't fire to do this for us. No-op
	// when nothing is running.
	s.runtime.Cancel(resolved)

	// A ticket that just reached a terminal status releases whatever waited on
	// it; one that left a terminal status blocks its dependents again.
	s.runtime.ReconcileDependencies(resolved)
	s.runtime.BroadcastUpdated(resolved)
	return Result{ID: resolved, Status: string(status)}, nil
}

// Retry resets a ticket to todo with attempt=0 for re-processing.
func (s *Service) Retry(id string) (Result, error) {
	resolved, err := s.repo.Resolve(id)
	if err != nil {
		return Result{}, err
	}
	st, err := s.repo.Get(resolved)
	if err != nil {
		return Result{}, err
	}

	if !st.Ticket.Kontora {
		return Result{}, fmt.Errorf("%w: ticket is not initialized", ErrInvalidState)
	}
	if err := refuseEpic(st.Ticket, resolved, "retry"); err != nil {
		return Result{}, err
	}

	if st.Ticket.Status == ticket.StatusInProgress || st.Ticket.Status == ticket.StatusTodo || st.Ticket.Status == ticket.StatusArchived {
		return Result{}, fmt.Errorf("%w: cannot retry ticket in status %s", ErrInvalidState, st.Ticket.Status)
	}

	if err := st.Ticket.SetField("attempt", 0); err != nil {
		return Result{}, fmt.Errorf("setting attempt: %w", err)
	}
	if err := st.Ticket.SetField("status", string(ticket.StatusTodo)); err != nil {
		return Result{}, fmt.Errorf("setting status: %w", err)
	}
	if err := st.Ticket.SetField("last_error", ""); err != nil {
		return Result{}, fmt.Errorf("clearing last_error: %w", err)
	}
	if err := st.Ticket.ClearSchedule(); err != nil {
		return Result{}, fmt.Errorf("clearing %s: %w", ticket.FieldScheduledAt, err)
	}

	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	s.runtime.Enqueue(st.Ticket)
	s.runtime.ReconcileDependencies(resolved)
	s.runtime.BroadcastUpdated(resolved)
	return Result{ID: resolved, Status: string(ticket.StatusTodo)}, nil
}

// Skip advances a ticket to the next pipeline stage, or marks it done
// if it is already on the final stage.
func (s *Service) Skip(id string) (Result, error) {
	resolved, err := s.repo.Resolve(id)
	if err != nil {
		return Result{}, err
	}
	st, err := s.repo.Get(resolved)
	if err != nil {
		return Result{}, err
	}

	t := st.Ticket
	if !t.Kontora {
		return Result{}, fmt.Errorf("%w: ticket is not initialized", ErrInvalidState)
	}
	if t.Status == ticket.StatusArchived {
		return Result{}, fmt.Errorf("%w: cannot skip archived ticket %s", ErrInvalidState, resolved)
	}
	if err := refuseEpic(t, resolved, "skip"); err != nil {
		return Result{}, err
	}

	pipelineCfg, ok := s.cfg().Pipelines[t.Pipeline]
	if !ok {
		return Result{}, fmt.Errorf("unknown pipeline %q for ticket %s", t.Pipeline, resolved)
	}

	currentIdx := stageIndex(pipelineCfg, t.Stage)
	if currentIdx < 0 {
		return Result{}, fmt.Errorf("stage %q not found in pipeline %q", t.Stage, t.Pipeline)
	}

	var newStatus string
	if currentIdx+1 >= len(pipelineCfg) {
		// Last stage — mark done.
		newStatus = string(ticket.StatusDone)
		if err := t.SetField("status", newStatus); err != nil {
			return Result{}, fmt.Errorf("setting status: %w", err)
		}
		now := time.Now().UTC()
		if err := t.SetField("completed_at", now); err != nil {
			return Result{}, fmt.Errorf("setting completed_at: %w", err)
		}
	} else {
		// Advance to next stage.
		newStatus = string(ticket.StatusTodo)
		if err := t.SetField("stage", pipelineCfg[currentIdx+1].Stage); err != nil {
			return Result{}, fmt.Errorf("setting stage: %w", err)
		}
		if err := t.SetField("status", newStatus); err != nil {
			return Result{}, fmt.Errorf("setting status: %w", err)
		}
		if err := t.SetField("attempt", 0); err != nil {
			return Result{}, fmt.Errorf("setting attempt: %w", err)
		}
	}

	if err := t.SetField("last_error", ""); err != nil {
		return Result{}, fmt.Errorf("clearing last_error: %w", err)
	}
	if err := t.ClearSchedule(); err != nil {
		return Result{}, fmt.Errorf("clearing %s: %w", ticket.FieldScheduledAt, err)
	}

	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	if newStatus == string(ticket.StatusTodo) {
		s.runtime.Enqueue(t)
	}
	s.runtime.ReconcileDependencies(resolved)
	s.runtime.BroadcastUpdated(resolved)
	return Result{ID: resolved, Status: newStatus}, nil
}

// resolveInitFields decides the pipeline and agent Init writes, and rejects a
// value the config does not know.
//
// A blank request field keeps what the ticket file already declares, so a
// project default fills a field instead of replacing a value the ticket chose.
// "none" is how a caller asks for the field to be cleared.
func resolveInitFields(cfg *config.Config, t *ticket.Ticket, req InitRequest) (pipeline, agent string, err error) {
	// The project is keyed by the path the ticket ends up with, which req.Path
	// overrides when set.
	repoPath := req.Path
	if repoPath == "" {
		repoPath = t.Path
	}
	pipeline, agent = req.Pipeline, req.Agent
	if pipeline == "" {
		pipeline = t.Pipeline
	}
	if agent == "" {
		agent = t.Agent
	}
	pipeline, agent = cfg.ResolveTicketDefaults(repoPath, pipeline, agent)

	if pipeline != "" {
		if steps, ok := cfg.Pipelines[pipeline]; !ok || len(steps) == 0 {
			return "", "", fmt.Errorf("%w: unknown pipeline %q", ErrInvalidState, pipeline)
		}
	}
	// Init validates what it writes. An agent the ticket already carries is
	// left alone, and the daemon reports it at spawn if the config has since
	// dropped it.
	if agent != "" && agent != t.Agent {
		if _, ok := cfg.Agents[agent]; !ok {
			return "", "", fmt.Errorf("%w %q", ErrUnknownAgent, agent)
		}
	}
	return pipeline, agent, nil
}

// applyInitFields writes the run's fields onto the ticket. It is a function of
// its own because Init is at gocyclo's ceiling. Nothing is saved here: the
// caller writes the file once, after the status is settled too.
func applyInitFields(t *ticket.Ticket, req InitRequest, pipeline, agent string) error {
	if err := t.SetField("pipeline", pipeline); err != nil {
		return fmt.Errorf("setting pipeline: %w", err)
	}
	if req.Path != "" {
		if err := t.SetField("path", req.Path); err != nil {
			return fmt.Errorf("setting path: %w", err)
		}
	}
	// Writing only on a change keeps init from stamping an empty agent field
	// onto a ticket that never had one, while still clearing one that "none"
	// opted out of.
	if agent != t.Agent {
		if err := t.SetField("agent", agent); err != nil {
			return fmt.Errorf("setting agent: %w", err)
		}
	}
	if req.Branch != "" {
		if err := t.SetField("branch", req.Branch); err != nil {
			return fmt.Errorf("setting branch: %w", err)
		}
	}
	if err := t.SetField("kontora", true); err != nil {
		return fmt.Errorf("setting kontora: %w", err)
	}
	if err := SetNotifyFields(t, req.Notify, req.NotifyChannels); err != nil {
		return err
	}
	return nil
}

// SetNotifyFields writes the two notification fields, or removes them. A nil
// slice is a caller that said nothing about the field and leaves it as it is;
// an empty non-nil one deletes the key rather than leaving `notify: []` behind,
// so a ticket that asks for nothing reads the way one that never asked does.
func SetNotifyFields(t *ticket.Ticket, notify, channels []string) error {
	for _, f := range []struct {
		key  string
		list []string
	}{{ticket.FieldNotify, notify}, {ticket.FieldNotifyChannels, channels}} {
		if f.list == nil {
			continue
		}
		var err error
		if len(f.list) == 0 {
			err = t.DeleteField(f.key)
		} else {
			err = t.SetField(f.key, f.list)
		}
		if err != nil {
			return fmt.Errorf("setting %s: %w", f.key, err)
		}
	}
	return nil
}

// Init initializes a ticket for daemon processing: sets pipeline, path,
// kontora=true, status, and stage. A ticket that is already initialized may
// pass through it again while it sits in open, which rewrites those fields but
// keeps its stage and attempt count.
func (s *Service) Init(id string, req InitRequest) (Result, error) {
	resolved, err := s.repo.Resolve(id)
	if err != nil {
		return Result{}, err
	}
	st, err := s.repo.Get(resolved)
	if err != nil {
		return Result{}, err
	}

	t := st.Ticket
	if err := refuseEpic(t, resolved, "initialize"); err != nil {
		return Result{}, err
	}
	// An initialized ticket may go through init again while it waits in open:
	// the dashboard queues it through the same form, so the fields the form
	// shows are the fields the run gets. A later status means a run already
	// owns them.
	reinit := t.Kontora
	if reinit && t.Status != ticket.StatusOpen {
		return Result{}, fmt.Errorf("%w: ticket already initialized", ErrInvalidState)
	}
	prevPipeline, prevStage := t.Pipeline, t.Stage

	cfg := s.cfg()

	pipeline, agent, err := resolveInitFields(cfg, t, req)
	if err != nil {
		return Result{}, err
	}

	if err := applyInitFields(t, req, pipeline, agent); err != nil {
		return Result{}, err
	}

	status := req.Status
	if status == "" {
		status = string(ticket.StatusTodo)
	}
	switch ticket.Status(status) { //nolint:exhaustive // only open/todo are valid init statuses
	case ticket.StatusOpen, ticket.StatusTodo:
	default:
		return Result{}, fmt.Errorf("%w: init status must be \"open\" or \"todo\", got %q", ErrInvalidState, status)
	}
	if err := t.SetField("status", status); err != nil {
		return Result{}, fmt.Errorf("setting status: %w", err)
	}
	// Queueing a scheduled ticket through the init form is the person saying
	// "start it now", so the timestamp goes with the move. An init that leaves
	// the ticket open keeps it.
	if status != string(ticket.StatusOpen) {
		if err := t.ClearSchedule(); err != nil {
			return Result{}, fmt.Errorf("clearing %s: %w", ticket.FieldScheduledAt, err)
		}
	}

	if pipeline != "" {
		stageName := req.Stage
		if stageName == "" {
			stageName = cfg.Pipelines[pipeline][0].Stage
			// A re-init keeps the stage the ticket reached. Only a pipeline
			// change restarts it, because the old stage is not in the new one.
			if reinit && prevStage != "" && pipeline == prevPipeline {
				stageName = prevStage
			}
		}
		if err := t.SetField("stage", stageName); err != nil {
			return Result{}, fmt.Errorf("setting stage: %w", err)
		}
	}

	// A re-init leaves the retry count of the stage the ticket is still on.
	if !reinit {
		if err := t.SetField("attempt", 0); err != nil {
			return Result{}, fmt.Errorf("setting attempt: %w", err)
		}
	}
	if err := t.SetField("last_error", ""); err != nil {
		return Result{}, fmt.Errorf("clearing last_error: %w", err)
	}

	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	if status == string(ticket.StatusTodo) {
		s.runtime.Enqueue(t)
	}
	s.runtime.ReconcileDependencies(resolved)
	s.runtime.BroadcastUpdated(resolved)
	return Result{ID: resolved, Status: status}, nil
}

// AgentForStage returns the agent configured for a pipeline stage.
func AgentForStage(cfg *config.Config, pipelineName, stageName string) string {
	pipeline, ok := cfg.Pipelines[pipelineName]
	if !ok || stageName == "" {
		return ""
	}
	for _, step := range pipeline {
		if step.Stage == stageName {
			return step.Agent
		}
	}
	return ""
}

func stageIndex(pipeline config.Pipeline, stageName string) int {
	for i, step := range pipeline {
		if step.Stage == stageName {
			return i
		}
	}
	return -1
}
