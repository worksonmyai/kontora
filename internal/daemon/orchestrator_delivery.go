package daemon

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/worktree"
)

type deliveryIntent struct {
	Update      worktree.DeliveryUpdate `json:"update"`
	Result      orchestrator.ResultRef  `json:"result"`
	RetainedRef string                  `json:"retained_ref"`
}

// The owning daemon must serialize record/advance with input and ownership mutations.
// This helper does not provide admission or lifecycle dispatch.
type managedDelivery struct {
	store       *controlstore.Store
	afterUpdate func() error
}

func (d *managedDelivery) record(id string, intent deliveryIntent) error {
	state, err := d.store.State()
	if err != nil {
		return err
	}
	work, err := checkedDeliveryInput(state, intent)
	if err != nil {
		return err
	}
	if state.Objectives[work.ObjectiveID].Status != orchestrator.ObjectiveActive {
		return orchestrator.ErrNotReady
	}
	observed, err := worktree.InspectDelivery(intent.Update)
	if err != nil {
		return err
	}
	if observed.CurrentOID != intent.Update.ExpectedOID {
		return orchestrator.ErrStale
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	ref, err := d.store.PutArtifact(payload)
	if err != nil {
		return err
	}
	return d.store.Apply(orchestrator.Command{
		Kind: orchestrator.RecordAction, ExpectedRevision: state.Revision,
		Action: orchestrator.Action{ID: id, ObjectiveID: work.ObjectiveID, Kind: "integrate_result", Payload: ref,
			Preconditions: orchestrator.Preconditions{WorkID: work.ID, DocumentRevision: work.DocumentRevision, SpecificationRevision: work.SpecificationRevision},
		},
	})
}

func checkedDeliveryInput(state orchestrator.State, intent deliveryIntent) (orchestrator.WorkItem, error) {
	r, ok := state.Results[intent.Result.ResultID]
	w := state.Work[r.WorkID]
	if !ok || !r.Valid || r.Checked == nil || r.Checked.VerificationPolicyID != state.Objectives[w.ObjectiveID].VerificationPolicyID || r.WorkID != intent.Result.WorkID || r.Generation != intent.Result.Generation || r.SpecificationRevision != w.SpecificationRevision || w.ActiveAttemptID != "" || w.Divergence != "" {
		return w, orchestrator.ErrStale
	}
	if err := worktree.InspectRetainedResult(intent.Update.RepoPath, intent.Update.CandidateOID, intent.RetainedRef); err != nil {
		return w, err
	}
	common, err := worktree.CommonDir(intent.Update.RepoPath)
	if err != nil {
		return w, err
	}
	owner := intent.Update.Owner
	if owner.BoardID != state.BoardID || owner.ObjectiveID != w.ObjectiveID || owner.Worktree != "" || !state.Objectives[w.ObjectiveID].Policy.RequireCommitApproval {
		return w, orchestrator.ErrConflict
	}
	for _, output := range r.Checked.Outputs {
		if output.Repository == common && output.Commit != "" && output.Commit == intent.Update.CandidateOID {
			return w, nil
		}
	}
	return w, fmt.Errorf("delivery candidate is not the checked repository output")
}

func (d *managedDelivery) load(id string) (orchestrator.State, orchestrator.Action, deliveryIntent, error) {
	state, err := d.store.State()
	if err != nil {
		return state, orchestrator.Action{}, deliveryIntent{}, err
	}
	action, ok := state.Actions[id]
	if !ok || action.Kind != "integrate_result" {
		return state, action, deliveryIntent{}, orchestrator.ErrConflict
	}
	data, err := d.store.ReadArtifact(action.Payload)
	if err != nil {
		return state, action, deliveryIntent{}, err
	}
	var intent deliveryIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return state, action, intent, err
	}
	if intent.Update.Owner.BoardID != state.BoardID || intent.Update.Owner.ObjectiveID != action.ObjectiveID {
		return state, action, intent, orchestrator.ErrConflict
	}
	return state, action, intent, nil
}

// advance moves only an existing, checked candidate. It never creates commits.
func (d *managedDelivery) advance(id string) error {
	state, action, intent, err := d.load(id)
	if err != nil {
		return err
	}
	if action.Status == orchestrator.ActionCompleted {
		return d.reconcile(id)
	}
	if err := orchestrator.ActionReady(state, id); err != nil {
		return err
	}
	if _, err := checkedDeliveryInput(state, intent); err != nil {
		return err
	}
	if state.Objectives[action.ObjectiveID].Status != orchestrator.ObjectiveActive {
		return orchestrator.ErrNotReady
	}
	if _, err := worktree.UpdateDelivery(intent.Update); err != nil {
		return err
	}
	if d.afterUpdate != nil {
		if err := d.afterUpdate(); err != nil {
			return err
		}
	}
	return d.reconcile(id)
}

// reconcile acknowledges the recorded candidate even if inputs changed after the effect.
// At the old tip it leaves the intent unchanged.
// If the ref is missing or matches neither tip, reconcile parks unfinished intents and returns an error.
func (d *managedDelivery) reconcile(id string) error {
	for {
		state, action, intent, err := d.load(id)
		if err != nil {
			return err
		}
		observed, err := worktree.InspectDelivery(intent.Update)
		if err != nil {
			return err
		}
		if observed.State == worktree.DeliveryAtTarget {
			if action.Status == orchestrator.ActionCompleted {
				return orchestrator.ErrStale
			}
			return nil
		}
		if observed.State == worktree.DeliveryAtCandidate && action.Status == orchestrator.ActionCompleted {
			return nil
		}
		if action.Status == orchestrator.ActionCompleted {
			return worktree.ErrDeliveryMoved
		}
		err = d.store.Transact(state.Revision, func(next orchestrator.State) (orchestrator.State, error) {
			current := next.Actions[id]
			if observed.State == worktree.DeliveryAtCandidate {
				current.Status, current.Receipt = orchestrator.ActionCompleted, "delivery ref verified at "+intent.Update.CandidateOID
			} else {
				current.Status, current.RecoveryReason = orchestrator.ActionParked, "delivery reference moved unexpectedly to "+observed.CurrentOID
			}
			next.Actions[id] = current
			next.Revision++
			return next, nil
		})
		if errors.Is(err, orchestrator.ErrStale) {
			continue
		}
		if err != nil {
			return err
		}
		if observed.State != worktree.DeliveryAtCandidate {
			return worktree.ErrDeliveryMoved
		}
		return nil
	}
}
