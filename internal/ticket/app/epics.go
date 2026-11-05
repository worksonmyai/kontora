package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// Linked worktrees and symlink spellings of the same repo are valid.
// Passive epics can group tickets from different repositories until activation.
func ValidateEpicChildRepository(epic, child *ticket.Ticket) error {
	if !epic.IsAgentEpic() {
		return nil
	}
	if epic.Path == "" || child.Path == "" {
		return fmt.Errorf("%w: agent epic and children require a repository path", ErrInvalidState)
	}
	common, err := worktree.CommonDir(config.ExpandTilde(epic.Path))
	if err != nil {
		return fmt.Errorf("%w: epic %s repository: %v", ErrInvalidState, epic.ID, err)
	}
	childCommon, err := worktree.CommonDir(config.ExpandTilde(child.Path))
	if err != nil {
		return fmt.Errorf("%w: child %s repository: %v", ErrInvalidState, child.ID, err)
	}
	if common != childCommon {
		return fmt.Errorf("%w: child %s must use the same repository as epic %s", ErrInvalidState, child.ID, epic.ID)
	}
	return nil
}

// Adapters changing an epic's path must pass the full index, not just a filtered
// board listing. Membership comes from parent, not the manual order list.
func ValidateEpicRepositories(epic *ticket.Ticket, index map[string]*ticket.Ticket) error {
	if !epic.IsAgentEpic() {
		return nil
	}
	if err := ValidateEpicChildRepository(epic, epic); err != nil {
		return err
	}
	for _, child := range ticket.EpicChildren(index, epic.ID) {
		if child.Kind == ticket.KindEpic {
			return fmt.Errorf("%w: epics do not nest", ErrInvalidState)
		}
		if err := ValidateEpicChildRepository(epic, child); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) initEpic(st *StoredTicket, req InitRequest) (Result, error) {
	t := st.Ticket
	if t.Status == ticket.StatusArchived {
		return Result{}, fmt.Errorf("%w: cannot initialize archived epic %s", ErrInvalidState, t.ID)
	}
	if t.IsPassiveEpic() && req.Status != string(ticket.StatusTodo) {
		return Result{}, fmt.Errorf("%w: starting a passive epic requires explicit init status todo", ErrInvalidState)
	}
	if t.Kontora && !t.IsPassiveEpic() && t.Status != ticket.StatusOpen {
		return Result{}, fmt.Errorf("%w: ticket already initialized", ErrInvalidState)
	}
	status := req.Status
	if status == "" {
		status = string(ticket.StatusTodo)
	}
	if status != string(ticket.StatusOpen) && status != string(ticket.StatusTodo) {
		return Result{}, fmt.Errorf("%w: init status must be open or todo", ErrInvalidState)
	}
	if config.ClearNone(req.Pipeline) != "" || req.Stage != "" || t.Pipeline != "" || t.Stage != "" || t.ScheduledAt != "" || t.Parent != "" {
		return Result{}, fmt.Errorf("%w: epics cannot have a pipeline, stage, schedule or parent", ErrInvalidState)
	}
	if req.Path != "" {
		if err := t.SetField("path", req.Path); err != nil {
			return Result{}, err
		}
	}
	_, agent := s.cfg().ResolveTicketDefaults(t.Path, config.NoneSentinel, firstNonempty(req.Agent, t.Agent))
	if agent == "" {
		agent = s.cfg().DefaultAgent
	}
	if _, ok := s.cfg().Agents[agent]; !ok {
		return Result{}, fmt.Errorf("%w %q", ErrUnknownAgent, agent)
	}
	if err := t.SetField("epic_execution", ticket.EpicExecutionAgent); err != nil {
		return Result{}, err
	}
	if err := applyInitFields(t, req, "", agent); err != nil {
		return Result{}, err
	}
	if err := s.ValidateExecutionSettings(t); err != nil {
		return Result{}, err
	}
	if err := validateEpicGitSettings(t); err != nil {
		return Result{}, err
	}
	for _, field := range []struct {
		key   string
		value any
	}{
		{"status", status}, {"attempt", 0}, {"last_error", ""},
	} {
		if err := t.SetField(field.key, field.value); err != nil {
			return Result{}, err
		}
	}
	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}
	if status == string(ticket.StatusTodo) {
		s.runtime.Enqueue(t)
	}
	s.runtime.ReconcileDependencies(t.ID)
	s.runtime.BroadcastUpdated(t.ID)
	return Result{ID: t.ID, Status: status}, nil
}

func validateEpicGitSettings(t *ticket.Ticket) error {
	repo := config.ExpandTilde(t.Path)
	if t.Branch != "" {
		cmd := exec.Command("git", "check-ref-format", "--branch", t.Branch)
		cmd.Dir = repo
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: invalid integration branch %q", ErrInvalidState, t.Branch)
		}
	}
	if _, err := worktree.ResolveBase(repo, t.BaseBranch); err != nil {
		return fmt.Errorf("%w: epic base branch: %v", ErrInvalidState, err)
	}
	return nil
}

// Validation reads each file again because the daemon's listing can lag local edits.
// Missing grouping parents do not prevent ordinary work.
func (s *Service) ValidateExecutionSettings(t *ticket.Ticket) error {
	if !t.IsAgentEpic() && t.Parent == "" {
		return nil
	}
	stored, err := s.repo.List()
	if err != nil {
		return err
	}
	index := make(map[string]*ticket.Ticket, len(stored))
	dir := filepath.Clean(config.ExpandTilde(s.cfg().TicketsDir))
	diskBacked := false
	for _, st := range stored {
		if st.FilePath != "" && filepath.Clean(filepath.Dir(st.FilePath)) == dir {
			diskBacked = true
		}
		if !t.IsAgentEpic() && st.Ticket.ID != t.Parent {
			continue
		}
		fresh, err := s.repo.Get(st.Ticket.ID)
		if errors.Is(err, ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		index[fresh.Ticket.ID] = fresh.Ticket
	}
	// A local creation can precede the watcher's first listing of a child.
	if diskBacked {
		files, err := ticket.ListFiles(dir)
		if err != nil {
			return err
		}
		for _, path := range files {
			if !t.IsAgentEpic() && filepath.Base(path) != t.Parent+".md" {
				continue
			}
			fresh, err := ticket.ParseFile(path)
			if err != nil {
				continue
			}
			if ticket.IsCanonicalPath(path, fresh.ID) {
				index[fresh.ID] = fresh
			}
		}
	}
	index[t.ID] = t
	if parent := index[t.Parent]; parent != nil {
		if err := validateEpicChildSettings(s.cfg(), parent, t); err != nil {
			return err
		}
	}
	if err := ValidateEpicRepositories(t, index); err != nil {
		return err
	}
	for _, child := range ticket.EpicChildren(index, t.ID) {
		if err := validateEpicChildSettings(s.cfg(), t, child); err != nil {
			return err
		}
	}
	return nil
}

func validateEpicChildSettings(cfg *config.Config, epic, child *ticket.Ticket) error {
	if err := ValidateEpicChildRepository(epic, child); err != nil {
		return err
	}
	if epic.IsAgentEpic() && executionBranch(cfg, epic) == executionBranch(cfg, child) {
		return fmt.Errorf("%w: integration branch must differ from child %s branch", ErrInvalidState, child.ID)
	}
	return nil
}

func executionBranch(cfg *config.Config, t *ticket.Ticket) string {
	if branch := strings.TrimSpace(t.Branch); branch != "" {
		return branch
	}
	id := t.ID
	if cfg.BranchNamingFor(t.Path).Mode == config.BranchNamingModeSlug {
		if slug := worktree.Slug(t.Title()); slug != "" {
			id = slug + "-" + id
		}
	}
	return worktree.BranchName(cfg.BranchPrefixFor(t.Path), id)
}

func firstNonempty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
