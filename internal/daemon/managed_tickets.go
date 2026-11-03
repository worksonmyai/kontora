package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/worktree"
)

const ticketProjectionKind = "managed_ticket_projection"

// ManagedTicketHooks must share the daemon's admission gate with ordinary pickup.
// Reserve calls commit only after all workers, schedules, reviews, and auxiliary jobs are absent.
type ManagedTicketHooks struct {
	Reserve   func(id string, commit func() error) error
	AfterSave func(*app.StoredTicket)
}

type ManagedTickets struct {
	mu        sync.Mutex
	dir       string
	authority controlstore.Authority
	Control   *controlstore.Store
	hooks     ManagedTicketHooks
}

type ticketProjection struct {
	TicketID   string                  `json:"ticket_id"`
	BeforeHash string                  `json:"before_hash"`
	After      []byte                  `json:"after"`
	Branch     *ticketBranchEnrollment `json:"branch,omitempty"`
}

type ticketBranchEnrollment struct {
	Repository string                   `json:"repository"`
	Branch     string                   `json:"branch"`
	Ownership  worktree.BranchOwnership `json:"ownership"`
}

// OpenManagedTickets acquires board authority before any recovery or ordinary admission.
// This component never starts managed execution.
func OpenManagedTickets(dir string, authority controlstore.Authority, hooks ManagedTicketHooks) (*ManagedTickets, error) {
	dir = config.ExpandTilde(dir)
	control, err := controlstore.OpenBoard(dir, authority)
	if err != nil {
		return nil, err
	}
	discovered, err := controlstore.Discover(dir)
	if err != nil {
		control.Close()
		return nil, err
	}
	return &ManagedTickets{dir: dir, authority: discovered, Control: control, hooks: hooks}, nil
}

func (m *ManagedTickets) Close() error { return m.Control.Close() }

func (m *ManagedTickets) Reserved(id string) (bool, error) {
	state, err := m.Control.State()
	if err != nil {
		return true, err
	}
	for _, w := range state.Work {
		if w.TicketID == id {
			return true, nil
		}
	}
	return false, nil
}

func (m *ManagedTickets) path(id string) (string, error) {
	if !ticket.IsSafeID(id) {
		return "", fmt.Errorf("unsafe ticket id %q", id)
	}
	return filepath.Join(m.dir, id+".md"), nil
}

func managedMetadata(a controlstore.Authority, w orchestrator.WorkItem) ticket.ManagedOwnership {
	return ticket.ManagedOwnership{BoardID: a.BoardID, OwnerID: a.OwnerID, ObjectiveID: w.ObjectiveID, Generation: w.OwnershipGeneration, DocumentRevision: uint64(w.DocumentRevision), SpecificationRevision: uint64(w.SpecificationRevision), AttemptID: w.ActiveAttemptID}
}

func managedSpecification(t *ticket.Ticket) string {
	data, _ := json.Marshal(struct{ Body, Pipeline, Agent, Path, Branch, BaseBranch string }{t.Body, t.Pipeline, t.Agent, t.Path, t.Branch, t.BaseBranch})
	return string(data)
}

func projectionHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (m *ManagedTickets) Enroll(id, workID, objectiveID string, expectedState uint64) (*app.StoredTicket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var saved *app.StoredTicket
	err := ticket.WithBoardMutation(m.dir, func() error {
		if m.hooks.Reserve == nil {
			return fmt.Errorf("enrollment requires the daemon admission gate")
		}
		path, err := m.path(id)
		if err != nil {
			return err
		}
		before, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		t, err := ticket.ParseBytes(before)
		if err != nil {
			return err
		}
		if t.ID != id {
			return fmt.Errorf("ticket id does not match path")
		}
		if err := ticket.RejectImportedOwnership(t); err != nil {
			return err
		}
		if t.Status == ticket.StatusTodo || t.Status == ticket.StatusInProgress || t.ScheduledAt != "" || t.AnnotationReturnStatus != "" {
			return fmt.Errorf("ticket has pending or active ordinary work")
		}
		state, err := m.Control.State()
		if err != nil {
			return err
		}
		if state.Revision != expectedState {
			return orchestrator.ErrStale
		}
		for _, w := range state.Work {
			if w.TicketID == id {
				return ticket.ErrManaged
			}
		}
		branch, err := m.prepareBranchEnrollment(t, objectiveID, state)
		if err != nil {
			return err
		}
		w := orchestrator.WorkItem{ID: workID, ObjectiveID: objectiveID, Kind: orchestrator.WorkTicket, TicketID: id, OwnershipGeneration: 1, DocumentRevision: 1, SpecificationRevision: 1, Specification: managedSpecification(t)}
		next, err := orchestrator.Apply(state, orchestrator.Command{Kind: orchestrator.PutWork, ExpectedRevision: expectedState, Work: w})
		if err != nil {
			return err
		}
		w = next.Work[workID]
		if err := t.SetField("managed", managedMetadata(m.authority, w)); err != nil {
			return err
		}
		after, err := t.Marshal()
		if err != nil {
			return err
		}
		action, err := m.projectionAction(w, before, after, branch)
		if err != nil {
			return err
		}
		next.Actions[action.ID] = action
		err = m.hooks.Reserve(id, func() error {
			current, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(current, before) {
				return orchestrator.ErrStale
			}
			return m.Control.Transact(expectedState, func(orchestrator.State) (orchestrator.State, error) { return next, nil })
		})
		if err != nil {
			return err
		}
		saved, err = m.project(action)
		return err
	})
	return saved, err
}

func canonicalTicketRepository(path string) (string, error) {
	abs, err := filepath.Abs(config.ExpandTilde(path))
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func branchReference(branch string) string {
	return "refs/heads/" + strings.TrimPrefix(branch, "refs/heads/")
}

func (b ticketBranchEnrollment) existing() (worktree.BranchOwnership, bool, error) {
	common, err := worktree.CommonDir(b.Repository)
	if err != nil {
		return worktree.BranchOwnership{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(common, "kontora", "branches.json"))
	if errors.Is(err, os.ErrNotExist) {
		return worktree.BranchOwnership{}, false, nil
	}
	if err != nil {
		return worktree.BranchOwnership{}, false, err
	}
	var leases map[string]worktree.BranchOwnership
	if err := json.Unmarshal(data, &leases); err != nil {
		return worktree.BranchOwnership{}, false, err
	}
	if leases == nil {
		return worktree.BranchOwnership{}, false, fmt.Errorf("invalid branch ownership")
	}
	owner, ok := leases[branchReference(b.Branch)]
	return owner, ok, nil
}

// The durable projection intent precedes the lease; exact ownership makes replay idempotent.
func (b ticketBranchEnrollment) reserve() error {
	owner, exists, err := b.existing()
	if err != nil {
		return err
	}
	if exists {
		if owner != b.Ownership {
			return worktree.ErrOwnedBranch
		}
		return nil
	}
	return worktree.ReserveBranch(b.Repository, b.Branch, b.Ownership)
}

func (m *ManagedTickets) projectionAction(w orchestrator.WorkItem, before, after []byte, branch *ticketBranchEnrollment) (orchestrator.Action, error) {
	t, err := ticket.ParseBytes(after)
	if err != nil {
		return orchestrator.Action{}, fmt.Errorf("invalid ticket projection: %w", err)
	}
	if t.ID != w.TicketID || t.Managed == nil || *t.Managed != managedMetadata(m.authority, w) {
		return orchestrator.Action{}, fmt.Errorf("ticket projection identity does not match control state")
	}
	data, err := json.Marshal(ticketProjection{TicketID: w.TicketID, BeforeHash: projectionHash(before), After: after, Branch: branch})
	if err != nil {
		return orchestrator.Action{}, err
	}
	payload, err := m.Control.PutArtifact(data)
	if err != nil {
		return orchestrator.Action{}, err
	}
	return orchestrator.Action{ID: fmt.Sprintf("ticket-projection:%s:%d", w.ID, w.DocumentRevision), ObjectiveID: w.ObjectiveID, Kind: ticketProjectionKind, Payload: payload, Preconditions: orchestrator.Preconditions{WorkID: w.ID, DocumentRevision: w.DocumentRevision, SpecificationRevision: w.SpecificationRevision, AttemptID: w.ActiveAttemptID}, Status: orchestrator.ActionPending}, nil
}

func (m *ManagedTickets) projection(action orchestrator.Action) (ticketProjection, error) {
	data, err := m.Control.ReadArtifact(action.Payload)
	if err != nil {
		return ticketProjection{}, err
	}
	var p ticketProjection
	err = json.Unmarshal(data, &p)
	return p, err
}

func (m *ManagedTickets) project(action orchestrator.Action) (*app.StoredTicket, error) {
	state, err := m.Control.State()
	if err != nil {
		return nil, err
	}
	p, err := m.projection(action)
	if err != nil {
		return nil, err
	}
	path, err := m.path(p.TicketID)
	if err != nil {
		return nil, err
	}
	t, err := ticket.ParseBytes(p.After)
	if err != nil {
		return nil, err
	}
	unexpected := errors.New("unexpected ticket bytes")
	err = m.Control.Transact(state.Revision, func(next orchestrator.State) (orchestrator.State, error) {
		w, ok := next.Work[action.Preconditions.WorkID]
		if !ok || w.Divergence != "" || w.DocumentRevision != action.Preconditions.DocumentRevision || w.SpecificationRevision != action.Preconditions.SpecificationRevision || (action.Status == orchestrator.ActionPending && w.ActiveAttemptID != action.Preconditions.AttemptID) {
			return orchestrator.State{}, orchestrator.ErrStale
		}
		if action.Status != orchestrator.ActionPending && action.Status != orchestrator.ActionCompleted {
			return orchestrator.State{}, fmt.Errorf("ticket projection is parked")
		}
		if p.Branch != nil {
			if err := p.Branch.reserve(); err != nil {
				return orchestrator.State{}, err
			}
		}
		var receipt orchestrator.State
		if action.Status == orchestrator.ActionPending {
			var err error
			receipt, err = orchestrator.Apply(next, orchestrator.Command{Kind: orchestrator.ResolveAction, ExpectedRevision: next.Revision, ActionID: action.ID, ActionStatus: orchestrator.ActionCompleted, Receipt: projectionHash(p.After)})
			if err != nil {
				return orchestrator.State{}, err
			}
		} else {
			receipt = next
			receipt.Revision++
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return orchestrator.State{}, err
		}
		if !bytes.Equal(current, p.After) {
			if action.Status != orchestrator.ActionPending || projectionHash(current) != p.BeforeHash {
				return orchestrator.State{}, unexpected
			}
			if err := writeManagedProjection(path, p.After); err != nil {
				return orchestrator.State{}, err
			}
		}
		return receipt, nil
	})
	if errors.Is(err, unexpected) {
		return nil, m.diverge(p.TicketID, "ticket changed before projection")
	}
	if err != nil {
		return nil, err
	}
	t.FilePath = path
	st := &app.StoredTicket{Ticket: t, FilePath: path}
	if m.hooks.AfterSave != nil {
		m.hooks.AfterSave(&app.StoredTicket{Ticket: t.Clone(), FilePath: path})
	}
	return st, nil
}

func writeManagedProjection(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".managed-ticket-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (m *ManagedTickets) latestProjection(state orchestrator.State, workID string) (orchestrator.Action, bool) {
	var latest orchestrator.Action
	for _, a := range state.Actions {
		if a.Kind == ticketProjectionKind && a.Preconditions.WorkID == workID && a.Preconditions.DocumentRevision > latest.Preconditions.DocumentRevision {
			latest = a
		}
	}
	return latest, latest.ID != ""
}

func (m *ManagedTickets) MutateManaged(req app.ManagedMutation) (*app.StoredTicket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var saved *app.StoredTicket
	err := ticket.WithBoardMutation(m.dir, func() error {
		state, err := m.Control.State()
		if err != nil {
			return err
		}
		var w orchestrator.WorkItem
		for _, candidate := range state.Work {
			if candidate.TicketID == req.ID {
				w = candidate
				break
			}
		}
		if w.ID == "" {
			return ticket.ErrManaged
		}
		expected := managedMetadata(m.authority, w)
		if req.Kind == "ownership" {
			expected.AttemptID = req.Expected.AttemptID
		}
		if req.Expected != expected {
			return orchestrator.ErrStale
		}
		if w.Divergence != "" {
			return fmt.Errorf("managed ticket diverged: %s", w.Divergence)
		}
		if req.Kind == "attempt" && (req.Expected.AttemptID == "" || w.ActiveAttemptID != req.Expected.AttemptID) {
			return orchestrator.ErrStale
		}
		action, ok := m.latestProjection(state, w.ID)
		if !ok || action.Status != orchestrator.ActionCompleted {
			return fmt.Errorf("ticket projection requires reconciliation")
		}
		p, err := m.projection(action)
		if err != nil {
			return err
		}
		path, err := m.path(req.ID)
		if err != nil {
			return err
		}
		before, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return m.diverge(req.ID, "ticket deleted")
			}
			return err
		}
		if !bytes.Equal(before, p.After) {
			return m.diverge(req.ID, "external ticket edit")
		}
		current, err := ticket.ParseBytes(before)
		if err != nil {
			return err
		}
		if current.Managed == nil || *current.Managed != req.Expected {
			return orchestrator.ErrStale
		}
		updated, err := app.ApplyManagedPatch(current, req)
		if err != nil {
			return err
		}
		kind := orchestrator.NoteWork
		if req.Kind == "specification" {
			kind = orchestrator.ReviseWork
		}
		command := orchestrator.Command{Kind: kind, ExpectedRevision: state.Revision, WorkID: w.ID, Notes: w.Notes, Work: orchestrator.WorkItem{Specification: managedSpecification(updated), Dependencies: w.Dependencies, Inputs: w.Inputs}, Preconditions: orchestrator.Preconditions{WorkID: w.ID, DocumentRevision: w.DocumentRevision, SpecificationRevision: w.SpecificationRevision, AttemptID: w.ActiveAttemptID}}
		next, err := orchestrator.Apply(state, command)
		if err != nil {
			return err
		}
		w = next.Work[w.ID]
		if err := updated.SetField("managed", managedMetadata(m.authority, w)); err != nil {
			return err
		}
		after, err := updated.Marshal()
		if err != nil {
			return err
		}
		action, err = m.projectionAction(w, before, after, nil)
		if err != nil {
			return err
		}
		next.Actions[action.ID] = action
		if err := m.Control.Transact(state.Revision, func(orchestrator.State) (orchestrator.State, error) { return next, nil }); err != nil {
			return err
		}
		saved, err = m.project(action)
		return err
	})
	return saved, err
}

func (m *ManagedTickets) diverge(id, reason string) error {
	state, err := m.Control.State()
	if err != nil {
		return err
	}
	err = m.Control.Transact(state.Revision, func(next orchestrator.State) (orchestrator.State, error) {
		affected := map[string]bool{}
		for key, w := range next.Work {
			if w.TicketID == id {
				for workID := range orchestrator.AffectedWork(next, key) {
					affected[workID] = true
				}
			}
		}
		if len(affected) == 0 {
			return orchestrator.State{}, fmt.Errorf("ticket is not reserved")
		}
		for key := range affected {
			w := next.Work[key]
			if w.Divergence != reason {
				w.DocumentRevision++
			}
			w.Divergence = reason
			next.Work[key] = w
			if w.ActiveAttemptID != "" {
				a := next.Attempts[w.ActiveAttemptID]
				a.Status, a.CancellationReason = orchestrator.AttemptCancelling, reason
				next.Attempts[a.ID] = a
			}
		}
		for key, r := range next.Results {
			if affected[r.WorkID] {
				r.Valid, r.InvalidReason = false, reason
				next.Results[key] = r
			}
		}
		next.Revision++
		return next, nil
	})
	if err != nil {
		return err
	}
	return fmt.Errorf("managed ticket %s diverged: %s", id, reason)
}

// Observe preserves external edits and deletions while recording durable divergence.
// The watcher must call Observe before replacing cached managed tickets.
func (m *ManagedTickets) Observe(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ticket.WithBoardMutation(m.dir, func() error {
		state, err := m.Control.State()
		if err != nil {
			return err
		}
		for _, w := range state.Work {
			if w.TicketID != id {
				continue
			}
			action, ok := m.latestProjection(state, w.ID)
			if !ok {
				return m.diverge(id, "missing ticket projection")
			}
			p, err := m.projection(action)
			if err != nil {
				return err
			}
			path, err := m.path(id)
			if err != nil {
				return err
			}
			current, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				return m.diverge(id, "ticket deleted")
			}
			if err != nil {
				return err
			}
			if bytes.Equal(current, p.After) {
				return nil
			}
			if action.Status == orchestrator.ActionPending && projectionHash(current) == p.BeforeHash {
				return fmt.Errorf("ticket projection pending reconciliation")
			}
			return m.diverge(id, "external ticket edit")
		}
		return nil
	})
}

// ReconcileProjections replays only the latest projection with matching before or after bytes.
func (m *ManagedTickets) ReconcileProjections() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ticket.WithBoardMutation(m.dir, func() error {
		state, err := m.Control.State()
		if err != nil {
			return err
		}
		for _, w := range state.Work {
			if w.TicketID == "" {
				continue
			}
			if w.Divergence != "" {
				return fmt.Errorf("managed ticket %s diverged: %s", w.TicketID, w.Divergence)
			}
			action, ok := m.latestProjection(state, w.ID)
			if !ok {
				return m.diverge(w.TicketID, "missing ticket projection")
			}
			if _, err := m.project(action); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return m.diverge(w.TicketID, "ticket deleted")
				}
				return err
			}
		}
		return nil
	})
}
