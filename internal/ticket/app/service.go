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

var validSetStatuses = map[ticket.Status]bool{
	ticket.StatusOpen:      true,
	ticket.StatusTodo:      true,
	ticket.StatusPaused:    true,
	ticket.StatusDone:      true,
	ticket.StatusCancelled: true,
}

// Service owns ticket use-cases: mutations, projection, and listing.
type Service struct {
	cfg     *config.Config
	repo    Repository
	runtime RuntimeHooks
}

// New creates a Service.
func New(cfg *config.Config, repo Repository, runtime RuntimeHooks) *Service {
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
	return BuildView(s.cfg, st.Ticket, opts.IncludeBody), nil
}

// List returns views of all tickets, optionally including non-kontora tickets.
func (s *Service) List(opts ListOptions) ([]View, error) {
	stored, err := s.repo.List()
	if err != nil {
		return nil, err
	}
	var views []View
	for _, st := range stored {
		if !opts.IncludeNonKontora && !st.Ticket.Kontora && st.Ticket.Status != ticket.StatusOpen {
			continue
		}
		views = append(views, BuildView(s.cfg, st.Ticket, false))
	}
	return views, nil
}

// SetStatus changes a ticket's status with validation.
func (s *Service) SetStatus(id string, status ticket.Status) (Result, error) {
	if !validSetStatuses[status] {
		valid := make([]string, 0, len(validSetStatuses))
		for k := range validSetStatuses {
			valid = append(valid, string(k))
		}
		return Result{}, fmt.Errorf("invalid status %q, valid statuses: %v", status, valid)
	}

	resolved, err := s.repo.Resolve(id)
	if err != nil {
		return Result{}, err
	}
	st, err := s.repo.Get(resolved)
	if err != nil {
		return Result{}, err
	}

	if st.Ticket.Status == status {
		return Result{}, fmt.Errorf("%w: ticket %s is already %s", ErrInvalidState, resolved, status)
	}

	if err := st.Ticket.SetField("kontora", true); err != nil {
		return Result{}, fmt.Errorf("setting kontora: %w", err)
	}
	if err := st.Ticket.SetField("status", string(status)); err != nil {
		return Result{}, fmt.Errorf("setting status: %w", err)
	}
	_ = st.Ticket.SetField("last_error", "")

	if status == ticket.StatusDone {
		now := time.Now().UTC()
		if err := st.Ticket.SetField("completed_at", now); err != nil {
			return Result{}, fmt.Errorf("setting completed_at: %w", err)
		}
	}

	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

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

	if st.Ticket.Status == ticket.StatusInProgress || st.Ticket.Status == ticket.StatusTodo {
		return Result{}, fmt.Errorf("%w: cannot retry ticket in status %s", ErrInvalidState, st.Ticket.Status)
	}

	if err := st.Ticket.SetField("attempt", 0); err != nil {
		return Result{}, fmt.Errorf("setting attempt: %w", err)
	}
	if err := st.Ticket.SetField("status", string(ticket.StatusTodo)); err != nil {
		return Result{}, fmt.Errorf("setting status: %w", err)
	}
	_ = st.Ticket.SetField("last_error", "")

	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	s.runtime.Enqueue(st.Ticket)
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
	pipelineCfg, ok := s.cfg.Pipelines[t.Pipeline]
	if !ok {
		return Result{}, fmt.Errorf("unknown pipeline %q for ticket %s", t.Pipeline, resolved)
	}

	currentIdx := stageIndex(pipelineCfg, t.Role)
	if currentIdx < 0 {
		return Result{}, fmt.Errorf("role %q not found in pipeline %q", t.Role, t.Pipeline)
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
		if err := t.SetField("role", pipelineCfg[currentIdx+1].Role); err != nil {
			return Result{}, fmt.Errorf("setting role: %w", err)
		}
		if err := t.SetField("status", newStatus); err != nil {
			return Result{}, fmt.Errorf("setting status: %w", err)
		}
		if err := t.SetField("attempt", 0); err != nil {
			return Result{}, fmt.Errorf("setting attempt: %w", err)
		}
	}

	_ = t.SetField("last_error", "")

	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	if newStatus == string(ticket.StatusTodo) {
		s.runtime.Enqueue(t)
	}
	s.runtime.BroadcastUpdated(resolved)
	return Result{ID: resolved, Status: newStatus}, nil
}

// Init initializes a ticket for daemon processing: sets pipeline, path,
// kontora=true, status, and role.
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
	if t.Kontora {
		return Result{}, fmt.Errorf("%w: ticket already initialized", ErrInvalidState)
	}

	if req.Pipeline != "" {
		pipelineCfg, ok := s.cfg.Pipelines[req.Pipeline]
		if !ok || len(pipelineCfg) == 0 {
			return Result{}, fmt.Errorf("%w: unknown pipeline %q", ErrInvalidState, req.Pipeline)
		}
	}
	if req.Agent != "" {
		if _, ok := s.cfg.Agents[req.Agent]; !ok {
			return Result{}, fmt.Errorf("%w %q", ErrUnknownAgent, req.Agent)
		}
	}

	if err := t.SetField("pipeline", req.Pipeline); err != nil {
		return Result{}, fmt.Errorf("setting pipeline: %w", err)
	}
	if req.Path != "" {
		if err := t.SetField("path", req.Path); err != nil {
			return Result{}, fmt.Errorf("setting path: %w", err)
		}
	}
	if req.Agent != "" {
		if err := t.SetField("agent", req.Agent); err != nil {
			return Result{}, fmt.Errorf("setting agent: %w", err)
		}
	}
	if err := t.SetField("kontora", true); err != nil {
		return Result{}, fmt.Errorf("setting kontora: %w", err)
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

	if req.Pipeline != "" {
		role := req.Role
		if role == "" {
			role = s.cfg.Pipelines[req.Pipeline][0].Role
		}
		if err := t.SetField("role", role); err != nil {
			return Result{}, fmt.Errorf("setting role: %w", err)
		}
	}

	if err := t.SetField("attempt", 0); err != nil {
		return Result{}, fmt.Errorf("setting attempt: %w", err)
	}
	_ = t.SetField("last_error", "")

	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	if status == string(ticket.StatusTodo) {
		s.runtime.Enqueue(t)
	}
	s.runtime.BroadcastUpdated(resolved)
	return Result{ID: resolved, Status: status}, nil
}

// AgentForStage returns the agent configured for a pipeline stage.
func AgentForStage(cfg *config.Config, pipelineName, role string) string {
	pipeline, ok := cfg.Pipelines[pipelineName]
	if !ok || role == "" {
		return ""
	}
	for _, stage := range pipeline {
		if stage.Role == role {
			return stage.Agent
		}
	}
	return ""
}

func stageIndex(pipeline config.Pipeline, role string) int {
	for i, stage := range pipeline {
		if stage.Role == role {
			return i
		}
	}
	return -1
}
