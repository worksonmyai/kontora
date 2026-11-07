package app

import (
	"fmt"
	"maps"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

// PreparedExecutionSettings contains Git identities resolved outside a mutation
// scope. Validate does fresh repository reads but never invokes Git. A changed
// participant or execution input requires the caller to prepare again.
type PreparedExecutionSettings struct {
	cfg          *config.Config
	participants map[string]executionIdentity
	common       map[string]string
}

type executionIdentity struct {
	id, path, branch, rawBranch, base, parent, mode, pipeline, stage, agent string
	kind                                                                    ticket.Kind
}

func executionInputs(cfg *config.Config, t *ticket.Ticket) executionIdentity {
	return executionIdentity{
		id: t.ID, path: t.Path, branch: executionBranch(cfg, t), rawBranch: t.Branch, base: t.BaseBranch,
		parent: t.Parent, kind: t.Kind, mode: t.EpicExecution, pipeline: t.Pipeline, stage: t.Stage, agent: t.Agent,
	}
}

func executionChanged(id string) error {
	return fmt.Errorf("%w: execution settings changed for %s; retry the operation", ErrInvalidState, id)
}

func executionParticipants(cfg *config.Config, t *ticket.Ticket, index map[string]*ticket.Ticket) map[string]executionIdentity {
	out := map[string]executionIdentity{t.ID: executionInputs(cfg, t)}
	if parent := index[t.Parent]; parent != nil {
		out[parent.ID] = executionInputs(cfg, parent)
	}
	if t.IsAgentEpic() {
		for _, child := range ticket.EpicChildren(index, t.ID) {
			out[child.ID] = executionInputs(cfg, child)
		}
	}
	return out
}

// PrepareExecutionSettings must be called before entering WithMutation. The
// proposed ticket may include fields a mutation intends to write; pass that
// same proposal, rebuilt from fresh state, to Validate inside the callback.
func (s *Service) PrepareExecutionSettings(t *ticket.Ticket) (*PreparedExecutionSettings, error) {
	cfg := s.cfg()
	index, err := executionIndex(s.repo, cfg, t)
	if err != nil {
		return nil, err
	}
	p := &PreparedExecutionSettings{cfg: cfg, participants: executionParticipants(cfg, t, index), common: map[string]string{}}
	resolvePair := func(epic, child *ticket.Ticket) error {
		if !epic.IsAgentEpic() {
			return nil
		}
		if epic.Path == "" || child.Path == "" {
			return fmt.Errorf("%w: agent epic and children require a repository path", ErrInvalidState)
		}
		for _, item := range []struct {
			ticket *ticket.Ticket
			role   string
		}{{epic, "epic"}, {child, "child"}} {
			if _, ok := p.common[item.ticket.Path]; ok {
				continue
			}
			common, err := worktree.CommonDir(config.ExpandTilde(item.ticket.Path))
			if err != nil {
				return fmt.Errorf("%w: %s %s repository: %v", ErrInvalidState, item.role, item.ticket.ID, err)
			}
			p.common[item.ticket.Path] = common
		}
		return nil
	}
	if parent := index[t.Parent]; parent != nil {
		if err := resolvePair(parent, t); err != nil {
			return nil, err
		}
	}
	if t.IsAgentEpic() {
		if err := resolvePair(t, t); err != nil {
			return nil, err
		}
		for _, child := range ticket.EpicChildren(index, t.ID) {
			if err := resolvePair(t, child); err != nil {
				return nil, err
			}
		}
	}
	if err := p.check(t, index); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate checks fresh membership and execution inputs using the scoped
// repository. It performs no Git subprocesses and does not write anything.
func (p *PreparedExecutionSettings) Validate(repo MutationRepository, t *ticket.Ticket) error {
	if p == nil {
		return executionChanged(t.ID)
	}
	index, err := executionIndex(repo, p.cfg, t)
	if err != nil {
		return err
	}
	if !maps.Equal(p.participants, executionParticipants(p.cfg, t, index)) {
		return executionChanged(t.ID)
	}
	return p.check(t, index)
}

func (p *PreparedExecutionSettings) check(t *ticket.Ticket, index map[string]*ticket.Ticket) error {
	checkPair := func(epic, child *ticket.Ticket) error {
		if !epic.IsAgentEpic() {
			return nil
		}
		if epic.Path == "" || child.Path == "" || p.common[epic.Path] == "" || p.common[child.Path] == "" {
			return fmt.Errorf("%w: agent epic and children require a repository path", ErrInvalidState)
		}
		if p.common[epic.Path] != p.common[child.Path] {
			return fmt.Errorf("%w: child %s must use the same repository as epic %s", ErrInvalidState, child.ID, epic.ID)
		}
		if epic.ID != child.ID && executionBranch(p.cfg, epic) == executionBranch(p.cfg, child) {
			return fmt.Errorf("%w: integration branch must differ from child %s branch", ErrInvalidState, child.ID)
		}
		return nil
	}
	if parent := index[t.Parent]; parent != nil {
		if err := checkPair(parent, t); err != nil {
			return err
		}
	}
	if t.IsAgentEpic() {
		if err := checkPair(t, t); err != nil {
			return err
		}
		for _, child := range ticket.EpicChildren(index, t.ID) {
			if child.Kind == ticket.KindEpic {
				return fmt.Errorf("%w: epics do not nest", ErrInvalidState)
			}
			if err := checkPair(t, child); err != nil {
				return err
			}
		}
	}
	return nil
}
