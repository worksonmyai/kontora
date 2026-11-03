package daemon

import (
	"fmt"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/worktree"
)

func (m *ManagedTickets) prepareBranchEnrollment(t *ticket.Ticket, objectiveID string, state orchestrator.State) (*ticketBranchEnrollment, error) {
	if t.Kind == ticket.KindEpic {
		return nil, nil
	}
	if t.Branch == "" || t.Path == "" {
		return nil, fmt.Errorf("enrollment requires an explicit repository and branch")
	}
	repository, err := canonicalTicketRepository(t.Path)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, scope := range state.Objectives[objectiveID].Policy.Repositories {
		path, err := canonicalTicketRepository(scope)
		if err == nil && path == repository {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("ticket repository is outside objective policy")
	}
	branch := &ticketBranchEnrollment{Repository: repository, Branch: t.Branch, Ownership: worktree.BranchOwnership{BoardID: m.authority.BoardID, ObjectiveID: objectiveID, Generation: 1}}
	if _, exists, err := branch.existing(); err != nil {
		return nil, err
	} else if exists {
		return nil, worktree.ErrOwnedBranch
	}
	for _, action := range state.Actions {
		if action.Kind != ticketProjectionKind {
			continue
		}
		p, err := m.projection(action)
		if err != nil {
			return nil, err
		}
		if p.Branch != nil && branchReference(p.Branch.Branch) == branchReference(branch.Branch) {
			previous, err := worktree.CommonDir(p.Branch.Repository)
			if err != nil {
				return nil, err
			}
			current, err := worktree.CommonDir(branch.Repository)
			if err != nil {
				return nil, err
			}
			if previous == current {
				return nil, worktree.ErrOwnedBranch
			}
		}
	}
	return branch, nil
}
