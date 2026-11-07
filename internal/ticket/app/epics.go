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

// IsEpicChild resolves parent kind without requiring the daemon to have indexed
// the parent. Missing parents and ordinary tickets do not establish epic membership.
func (s *Service) IsEpicChild(parentID string) (bool, error) {
	if parentID == "" {
		return false, nil
	}
	st, err := s.repo.Get(parentID)
	if err == nil {
		if st.Ticket.ID != parentID {
			return false, fmt.Errorf("%w: parent %s file declares id %q", ErrInvalidState, parentID, st.Ticket.ID)
		}
		return st.Ticket.Kind == ticket.KindEpic, nil
	}
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("reading parent %s: %w", parentID, err)
	}
	// Watcher discovery can lag creation.
	if !ticket.IsSafeID(parentID) {
		return false, fmt.Errorf("%w: unsafe parent id %q", ErrInvalidState, parentID)
	}
	path := filepath.Join(config.ExpandTilde(s.cfg().TicketsDir), parentID+".md")
	parent, err := ticket.ParseFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading parent %s: %w", parentID, err)
	}
	if !ticket.IsCanonicalPath(path, parent.ID) {
		return false, fmt.Errorf("%w: parent %s file declares id %q", ErrInvalidState, parentID, parent.ID)
	}
	return parent.Kind == ticket.KindEpic, nil
}

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
	_, agent := s.cfg().ResolveTicketDefaults(t.Path, config.NoneSentinel, firstNonempty(req.Agent, t.Agent), false)
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
	if err := s.validateExecution(t); err != nil {
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

// ValidateExecutionSettings is the non-transactional convenience API. Mutation
// callers must prepare before locking and Validate against their scoped repo.
func (s *Service) ValidateExecutionSettings(t *ticket.Ticket) error {
	_, err := s.PrepareExecutionSettings(t)
	return err
}

// executionIndex refreshes discovery entries, including children not yet in the
// watcher's index. Missing grouping parents do not prevent ordinary work.
func executionIndex(repo MutationRepository, cfg *config.Config, t *ticket.Ticket) (map[string]*ticket.Ticket, error) {
	if !t.IsAgentEpic() && t.Parent == "" {
		return map[string]*ticket.Ticket{t.ID: t}, nil
	}
	stored, err := repo.List()
	if err != nil {
		return nil, err
	}
	index := make(map[string]*ticket.Ticket, len(stored))
	dir := filepath.Clean(config.ExpandTilde(cfg.TicketsDir))
	diskBacked := false
	for _, st := range stored {
		if st.FilePath != "" && filepath.Clean(filepath.Dir(st.FilePath)) == dir {
			diskBacked = true
		}
		if !t.IsAgentEpic() && st.Ticket.ID != t.Parent {
			continue
		}
		fresh, err := repo.Get(st.Ticket.ID)
		if errors.Is(err, ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// Discovery of an unrelated malformed file is not a requested
			// participant read. Known children and the requested parent are.
			if st.Ticket.Parent == t.ID || st.Ticket.ID == t.Parent || st.Ticket.ID == t.ID {
				return nil, err
			}
			continue
		}
		index[fresh.Ticket.ID] = fresh.Ticket
	}
	// A local creation can precede the watcher's first listing of a child.
	if diskBacked {
		files, err := ticket.ListFiles(dir)
		if err != nil {
			return nil, err
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
	return index, nil
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
