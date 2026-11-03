package orchestrator

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func ValidateGraph(s State) error {
	marks := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if marks[id] == 1 {
			return fmt.Errorf("work dependency cycle at %s", id)
		}
		if marks[id] == 2 {
			return nil
		}
		w, ok := s.Work[id]
		if !ok {
			return fmt.Errorf("missing work %s", id)
		}
		marks[id] = 1
		seen := map[string]bool{}
		for _, dep := range w.Dependencies {
			if seen[dep] {
				return fmt.Errorf("duplicate dependency %s", dep)
			}
			seen[dep] = true
			if err := visit(dep); err != nil {
				return err
			}
		}
		consumed := map[string]bool{}
		for _, input := range w.Inputs {
			if consumed[input.WorkID] {
				return fmt.Errorf("duplicate input work %s", input.WorkID)
			}
			consumed[input.WorkID] = true
			if err := checkRef(s, input); err != nil {
				return err
			}
			if err := visit(input.WorkID); err != nil {
				return err
			}
		}
		marks[id] = 2
		return nil
	}
	for id := range s.Work {
		if err := visit(id); err != nil {
			return err
		}
	}
	resultMarks := map[string]uint8{}
	var visitResult func(string) error
	visitResult = func(id string) error {
		if resultMarks[id] == 1 {
			return fmt.Errorf("result dependency cycle at %s", id)
		}
		if resultMarks[id] == 2 {
			return nil
		}
		resultMarks[id] = 1
		for _, ref := range s.Results[id].Inputs {
			if err := checkRef(s, ref); err != nil {
				return err
			}
			if err := visitResult(ref.ResultID); err != nil {
				return err
			}
		}
		resultMarks[id] = 2
		return nil
	}
	for id := range s.Results {
		if err := visitResult(id); err != nil {
			return err
		}
	}
	return nil
}

func checkRef(s State, ref ResultRef) error {
	r, ok := s.Results[ref.ResultID]
	if !ok || r.WorkID != ref.WorkID || r.Generation != ref.Generation {
		return fmt.Errorf("missing or mismatched result %s", ref.ResultID)
	}
	return nil
}

func validInputs(s State, refs []ResultRef) error {
	seen := map[string]bool{}
	for _, ref := range refs {
		if err := checkRef(s, ref); err != nil {
			return err
		}
		if seen[ref.WorkID] {
			return fmt.Errorf("duplicate input work %s", ref.WorkID)
		}
		seen[ref.WorkID] = true
		r := s.Results[ref.ResultID]
		if !r.Valid || r.SpecificationRevision != s.Work[r.WorkID].SpecificationRevision {
			return fmt.Errorf("%w: invalid input %s", ErrNotReady, r.ID)
		}
	}
	return nil
}

func Ready(s State, workID string) error {
	w, ok := s.Work[workID]
	if !ok {
		return fmt.Errorf("missing work %s", workID)
	}
	o := s.Objectives[w.ObjectiveID]
	if o.Status != ObjectiveActive || o.BlockedReason != "" || w.ActiveAttemptID != "" || w.ReplacedBy != "" || w.Divergence != "" {
		return ErrNotReady
	}
	for _, q := range s.Questions {
		if q.WorkID == w.ID && q.Answer == "" && q.SupersededReason == "" && !questionStale(s, q) {
			return ErrNotReady
		}
	}
	for _, a := range s.Actions {
		if a.ObjectiveID == w.ObjectiveID && a.Status == ActionParked && (a.Preconditions.WorkID == "" || a.Preconditions.WorkID == w.ID) {
			return ErrNotReady
		}
	}
	for _, dep := range w.Dependencies {
		d := s.Work[dep]
		a := s.Attempts[d.LastAttemptID]
		if a.Status != AttemptSucceeded || a.SpecificationRevision != d.SpecificationRevision || d.ReplacedBy != "" || d.Divergence != "" {
			return ErrNotReady
		}
	}
	return validInputs(s, w.Inputs)
}

// AffectedWork follows current inputs. Invalid historical results cannot make independent work depend on an old producer.
func AffectedWork(s State, workID string) map[string]bool {
	affected := map[string]bool{workID: true}
	for changed := true; changed; {
		changed = false
		for _, w := range s.Work {
			for _, input := range w.Inputs {
				if affected[input.WorkID] && !affected[w.ID] {
					affected[w.ID], changed = true, true
				}
			}
		}
		for _, r := range s.Results {
			if !r.Valid || r.SpecificationRevision != s.Work[r.WorkID].SpecificationRevision {
				continue
			}
			for _, input := range r.Inputs {
				if affected[input.WorkID] && !affected[r.WorkID] {
					affected[r.WorkID], changed = true, true
				}
			}
		}
	}
	return affected
}

func canInvalidate(s State, workID string) error {
	for id := range AffectedWork(s, workID) {
		if s.Work[id].ActiveAttemptID != "" {
			return fmt.Errorf("%w: stop affected attempt for %s first", ErrConflict, id)
		}
	}
	return nil
}

func invalidate(s *State, workID, reason string) {
	affected := AffectedWork(*s, workID)
	for id, r := range s.Results {
		if affected[r.WorkID] && r.Valid {
			r.Valid, r.InvalidReason = false, reason
			s.Results[id] = r
		}
	}
}

func Validate(s State) error {
	if s.BoardID == "" || s.Objectives == nil || s.Work == nil || s.Attempts == nil || s.Plans == nil || s.Results == nil || s.Actions == nil || s.Questions == nil || s.Acceptances == nil || s.Leases == nil {
		return fmt.Errorf("incomplete control state")
	}
	for _, validate := range []func(State) error{
		validateObjectives,
		validatePlans,
		validateWork,
		validateAttempts,
		validateLeases,
		validateResults,
		validateActions,
		validateQuestions,
		validateAcceptances,
		validateArtifacts,
		ValidateGraph,
	} {
		if err := validate(s); err != nil {
			return err
		}
	}
	return nil
}

func validateObjectives(s State) error {
	for id, o := range s.Objectives {
		if id == "" || o.ID != id || o.DocumentRevision == 0 || o.CriteriaRevision == 0 || o.Assignment == "" || len(o.Criteria) == 0 {
			return fmt.Errorf("invalid objective %s", id)
		}
		switch o.Status {
		case ObjectiveDraft, ObjectiveActive, ObjectivePaused, ObjectiveCancelled, ObjectiveAccepted:
		default:
			return fmt.Errorf("invalid objective status %s", id)
		}
		if o.Budget.MaxRuntime < 0 || o.Budget.MaxCostMicros < 0 {
			return fmt.Errorf("invalid budget %s", id)
		}
		if o.CurrentPlan != 0 {
			found := false
			for _, p := range s.Plans {
				if p.ObjectiveID == id && p.Revision == o.CurrentPlan {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("missing current plan %s", id)
			}
		}
	}
	return nil
}

func validatePlans(s State) error {
	planRevisions := map[string]map[PlanRevision]bool{}
	for id, p := range s.Plans {
		if p.ID != id || id == "" || s.Objectives[p.ObjectiveID].ID == "" || p.Revision == 0 || p.Revision > s.Objectives[p.ObjectiveID].CurrentPlan {
			return fmt.Errorf("invalid plan %s", id)
		}
		if planRevisions[p.ObjectiveID] == nil {
			planRevisions[p.ObjectiveID] = map[PlanRevision]bool{}
		}
		if planRevisions[p.ObjectiveID][p.Revision] {
			return fmt.Errorf("duplicate plan revision")
		}
		planRevisions[p.ObjectiveID][p.Revision] = true
		seen := map[string]bool{}
		for _, wid := range p.WorkIDs {
			if seen[wid] || s.Work[wid].ObjectiveID != p.ObjectiveID {
				return fmt.Errorf("invalid plan work %s", wid)
			}
			seen[wid] = true
		}
		for _, wid := range p.RequiredResults {
			if !seen[wid] {
				return fmt.Errorf("required result outside plan %s", wid)
			}
		}
		for _, wid := range p.ReplacedWorkIDs {
			if seen[wid] || s.Work[wid].ObjectiveID != p.ObjectiveID {
				return fmt.Errorf("invalid replaced work %s", wid)
			}
		}
	}
	return nil
}

func validateWork(s State) error {
	ownedTickets := map[string]string{}
	for id, w := range s.Work {
		if id == "" || w.ID != id || s.Objectives[w.ObjectiveID].ID == "" || w.DocumentRevision == 0 || w.SpecificationRevision == 0 || w.OwnershipGeneration == 0 || w.Specification == "" {
			return fmt.Errorf("invalid work %s", id)
		}
		switch w.Kind {
		case WorkTicket, WorkJob, WorkPlanner, WorkCheck, WorkSummary:
		default:
			return fmt.Errorf("invalid work kind %s", id)
		}
		if w.Kind == WorkTicket {
			if w.TicketID == "" || ownedTickets[w.TicketID] != "" {
				return fmt.Errorf("duplicate or empty ticket ownership %s", id)
			}
			ownedTickets[w.TicketID] = id
		} else if w.TicketID != "" {
			return fmt.Errorf("ticketless work has a ticket %s", id)
		}
		if w.ActiveAttemptID != "" {
			a := s.Attempts[w.ActiveAttemptID]
			if a.WorkID != id || !a.Reservation.Held || w.LastAttemptID != a.ID {
				return fmt.Errorf("invalid active attempt %s", id)
			}
		}
		if w.LastAttemptID != "" && s.Attempts[w.LastAttemptID].WorkID != id {
			return fmt.Errorf("missing last attempt %s", id)
		}
		if w.ReplacedBy != "" && s.Plans[w.ReplacedBy].ObjectiveID != w.ObjectiveID {
			return fmt.Errorf("missing replacement plan %s", id)
		}
	}
	return nil
}

func validateAttempt(s State, id string, a Attempt) error {
	w, ok := s.Work[a.WorkID]
	if id == "" || a.ID != id || !ok || a.ObjectiveID != w.ObjectiveID || a.SpecificationRevision == 0 || a.SpecificationRevision > w.SpecificationRevision || a.StartedAt.IsZero() || a.Recipe.Timeout <= 0 || a.Recipe.Profile == "" || a.Recipe.ConfigurationRevision == "" || a.Reservation.Slots <= 0 {
		return fmt.Errorf("invalid attempt %s", id)
	}
	for _, ref := range a.Inputs {
		if err := checkRef(s, ref); err != nil {
			return err
		}
	}
	return validateAttemptStatus(s, w, a)
}

func validateAttemptStatus(s State, w WorkItem, a Attempt) error {
	id := a.ID
	switch a.Status {
	case AttemptRunning, AttemptCancelling, AttemptParking, AttemptUncertain:
		if !a.Reservation.Held || w.ActiveAttemptID != id || a.SpecificationRevision != w.SpecificationRevision || !reflect.DeepEqual(a.Inputs, w.Inputs) || !a.StoppedAt.IsZero() || a.TerminationEvidence != "" || a.Usage != (Usage{}) {
			return fmt.Errorf("unaccounted live attempt %s", id)
		}
		// Stop intents retain reservations even after their pinned inputs become invalid.
		if a.Status == AttemptRunning || a.Status == AttemptUncertain {
			if err := validInputs(s, a.Inputs); err != nil {
				return err
			}
		}
	case AttemptSucceeded, AttemptFailed, AttemptCancelled, AttemptParked:
		if a.Reservation.Held || a.TerminationEvidence == "" || a.StoppedAt.Before(a.StartedAt) || a.Usage.Runtime < a.StoppedAt.Sub(a.StartedAt) {
			return fmt.Errorf("unconfirmed stopped attempt %s", id)
		}
	default:
		return fmt.Errorf("invalid attempt status %s", id)
	}
	return nil
}

func validateAttempts(s State) error {
	usage := map[string]Usage{}
	for id, a := range s.Attempts {
		if err := validateAttempt(s, id, a); err != nil {
			return err
		}
		w := s.Work[a.WorkID]
		if a.Usage.Runtime < 0 || a.Usage.CostMicros < 0 || a.Usage.Attempts != 0 || a.Usage.PlannerTurns != 0 {
			return fmt.Errorf("invalid usage %s", id)
		}
		u := usage[a.ObjectiveID]
		u.Attempts++
		if w.Kind == WorkPlanner {
			u.PlannerTurns++
		}
		if a.Usage.Runtime > 1<<63-1-u.Runtime || a.Usage.CostMicros > 1<<63-1-u.CostMicros {
			return fmt.Errorf("usage overflow")
		}
		u.Runtime += a.Usage.Runtime
		u.CostMicros += a.Usage.CostMicros
		u.UnknownCost = u.UnknownCost || a.Usage.UnknownCost
		usage[a.ObjectiveID] = u
		seen := map[string]bool{}
		for _, key := range a.Reservation.LeaseKeys {
			if seen[key] {
				return fmt.Errorf("duplicate attempt lease %s", key)
			}
			seen[key] = true
			if a.Reservation.Held {
				l := s.Leases[key]
				if l.AttemptID != id || l.WorkID != w.ID {
					return fmt.Errorf("missing live lease %s", key)
				}
			}
		}
	}
	for id, o := range s.Objectives {
		if o.Usage != usage[id] {
			return fmt.Errorf("accounting mismatch %s", id)
		}
	}
	return nil
}

func validateLeases(s State) error {
	for key, l := range s.Leases {
		if key == "" || l.Key != key || s.Work[l.WorkID].ID == "" {
			return fmt.Errorf("invalid lease %s", key)
		}
		if l.AttemptID == "" {
			if s.Work[l.WorkID].ActiveAttemptID != "" {
				return fmt.Errorf("active work omitted retained lease %s", key)
			}
			if !l.Retain {
				return fmt.Errorf("unowned transient lease %s", key)
			}
			continue
		}
		a := s.Attempts[l.AttemptID]
		if !a.Reservation.Held || a.WorkID != l.WorkID || !contains(a.Reservation.LeaseKeys, key) {
			return fmt.Errorf("lease does not match attempt %s", key)
		}
	}
	return nil
}

func validateResults(s State) error {
	generations := map[string]map[uint64]bool{}
	for id, r := range s.Results {
		w := s.Work[r.WorkID]
		a := s.Attempts[r.AttemptID]
		if id == "" || r.ID != id || w.ID == "" || a.WorkID != w.ID || a.Status != AttemptSucceeded || r.Generation == 0 || r.SpecificationRevision != a.SpecificationRevision || !reflect.DeepEqual(r.Inputs, a.Inputs) || (r.Commit == "" && len(r.Artifacts) == 0) {
			return fmt.Errorf("invalid result %s", id)
		}
		if generations[w.ID] == nil {
			generations[w.ID] = map[uint64]bool{}
		}
		if generations[w.ID][r.Generation] {
			return fmt.Errorf("duplicate result generation")
		}
		generations[w.ID][r.Generation] = true
		for _, ref := range r.Inputs {
			if err := checkRef(s, ref); err != nil {
				return err
			}
		}
		if err := validateResultValidity(s, w, r); err != nil {
			return err
		}
	}
	return nil
}

func validateResultValidity(s State, w WorkItem, r Result) error {
	if r.Valid {
		if r.SpecificationRevision != w.SpecificationRevision || w.ReplacedBy != "" || r.InvalidReason != "" {
			return fmt.Errorf("stale valid result %s", r.ID)
		}
		if err := validInputs(s, r.Inputs); err != nil {
			return err
		}
		for _, ch := range r.Checks {
			if ch.ExitCode != 0 || !ch.InputsUnchanged || ch.Timeout <= 0 || ch.DefinitionRevision == "" || ch.ConfigurationRevision == "" || len(ch.Command) == 0 {
				return fmt.Errorf("invalid result checks %s", r.ID)
			}
		}
	} else if r.InvalidReason == "" {
		return fmt.Errorf("invalid result lacks reason %s", r.ID)
	}
	return nil
}

func validateActions(s State) error {
	for id, a := range s.Actions {
		if id == "" || a.ID != id || s.Objectives[a.ObjectiveID].ID == "" || a.Kind == "" {
			return fmt.Errorf("invalid action %s", id)
		}
		if a.Preconditions.WorkID != "" && s.Work[a.Preconditions.WorkID].ObjectiveID != a.ObjectiveID {
			return fmt.Errorf("action outside objective")
		}
		if a.Approval != nil && !validApproval(a, a.Approval) {
			return fmt.Errorf("invalid bound approval %s", id)
		}
		switch a.Status {
		case ActionPending:
			if a.Receipt != "" {
				return fmt.Errorf("pending action has receipt")
			}
		case ActionParked:
			if a.RecoveryReason == "" {
				return fmt.Errorf("parked action lacks reason")
			}
		case ActionCompleted:
			if a.Receipt == "" {
				return fmt.Errorf("completed action lacks receipt")
			}
			if a.RequiresApproval && !validApproval(a, a.Approval) {
				return fmt.Errorf("unapproved completed action")
			}
		default:
			return fmt.Errorf("invalid action status")
		}
	}
	return nil
}

func validateQuestions(s State) error {
	for id, q := range s.Questions {
		a := s.Attempts[q.AttemptID]
		if id == "" || q.ID != id || q.Prompt == "" || a.ID == "" || a.WorkID != q.WorkID || a.ObjectiveID != q.ObjectiveID || q.SpecificationRevision != a.SpecificationRevision || (q.Answer == "") != (q.AnsweredBy == "") || (q.SupersededReason != "" && (q.Answer != "" || !questionStale(s, q))) {
			return fmt.Errorf("invalid question %s", id)
		}
	}
	return nil
}

func validateAcceptances(s State) error {
	for id, a := range s.Acceptances {
		if id == "" || a.ID != id || s.Objectives[a.ObjectiveID].ID == "" || a.OperatorID == "" || a.CriteriaRevision == 0 || a.PlanRevision == 0 || a.CheckRevision == "" || a.AcceptedAt.IsZero() || len(a.Results) == 0 {
			return fmt.Errorf("invalid acceptance %s", id)
		}
		for _, ref := range a.Results {
			if err := checkRef(s, ref); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateArtifacts(s State) error {
	for _, ref := range ReferencedArtifacts(s) {
		if len(ref.Hash) != 64 || ref.Hash != strings.ToLower(ref.Hash) || ref.Size < 0 {
			return fmt.Errorf("invalid artifact reference")
		}
		if _, err := hex.DecodeString(ref.Hash); err != nil {
			return fmt.Errorf("invalid artifact hash: %w", err)
		}
	}
	return nil
}

// Historical records remain available to reject delayed callbacks and recover retained work.
func ValidateTransition(before, after State) error {
	if before.BoardID != after.BoardID || before.Revision == ^uint64(0) || after.Revision != before.Revision+1 {
		return ErrStale
	}
	if err := Validate(after); err != nil {
		return err
	}
	for _, validate := range []func(State, State) error{
		validateObjectiveTransitions,
		validateWorkTransitions,
		validateAttemptTransitions,
		validatePlanTransitions,
		validateResultTransitions,
		validateActionTransitions,
		validateQuestionTransitions,
		validateAcceptanceTransitions,
	} {
		if err := validate(before, after); err != nil {
			return err
		}
	}
	return nil
}

func validateObjectiveTransitions(before, after State) error {
	for id, old := range before.Objectives {
		n, ok := after.Objectives[id]
		if !ok || old.ID != n.ID || old.Assignment != n.Assignment || old.CriteriaRevision != n.CriteriaRevision || !reflect.DeepEqual(old.Criteria, n.Criteria) || !reflect.DeepEqual(old.Policy, n.Policy) || old.Budget != n.Budget || n.DocumentRevision < old.DocumentRevision {
			return fmt.Errorf("objective authority changed %s", id)
		}
	}
	return nil
}

func validateWorkTransitions(before, after State) error {
	for id, old := range before.Work {
		n, ok := after.Work[id]
		specChanged := old.Specification != n.Specification || !reflect.DeepEqual(old.Dependencies, n.Dependencies) || !reflect.DeepEqual(old.Inputs, n.Inputs)
		if specChanged && (n.SpecificationRevision != old.SpecificationRevision+1 || n.DocumentRevision != old.DocumentRevision+1) {
			return fmt.Errorf("specification changed without new revisions %s", id)
		}
		if !specChanged && n.SpecificationRevision != old.SpecificationRevision {
			return fmt.Errorf("specification revision changed without new inputs %s", id)
		}
		if (old.Notes != n.Notes || old.ReplacedBy != n.ReplacedBy || old.Divergence != n.Divergence) && n.DocumentRevision != old.DocumentRevision+1 {
			return fmt.Errorf("document changed without new revision %s", id)
		}
		if !ok || old.ID != n.ID || old.ObjectiveID != n.ObjectiveID || old.Kind != n.Kind || old.TicketID != n.TicketID || old.OwnershipGeneration != n.OwnershipGeneration || n.DocumentRevision < old.DocumentRevision || n.SpecificationRevision < old.SpecificationRevision {
			return fmt.Errorf("work identity changed %s", id)
		}
	}
	return nil
}

func validateAttemptTransitions(before, after State) error {
	for id, a := range after.Attempts {
		if _, existed := before.Attempts[id]; !existed && (a.Status != AttemptRunning || !a.Reservation.Held) {
			return fmt.Errorf("attempt outcome lacks a prior reservation %s", id)
		}
	}
	for id, old := range before.Attempts {
		n, ok := after.Attempts[id]
		if !ok {
			return fmt.Errorf("attempt tombstone removed %s", id)
		}
		if old.Status != AttemptRunning && n.Status == AttemptRunning {
			return fmt.Errorf("attempt cannot resume after a stop intent %s", id)
		}
		if (old.Status == AttemptCancelling || old.Status == AttemptParking) && n.Status == AttemptUncertain {
			return fmt.Errorf("uncertainty cannot replace a stop intent %s", id)
		}
		if old.Reservation.Held && old.Status != AttemptRunning && n.Status == AttemptSucceeded {
			return fmt.Errorf("stopped intent cannot become successful %s", id)
		}
		if !old.Reservation.Held {
			if !reflect.DeepEqual(old, n) {
				return fmt.Errorf("terminal attempt changed %s", id)
			}
			continue
		}
		old.Status, old.StoppedAt, old.TerminationEvidence, old.CancellationReason, old.Usage, old.Logs, old.Reservation.Held = n.Status, n.StoppedAt, n.TerminationEvidence, n.CancellationReason, n.Usage, n.Logs, n.Reservation.Held
		if !reflect.DeepEqual(old, n) {
			return fmt.Errorf("frozen attempt changed %s", id)
		}
	}
	return nil
}

func validatePlanTransitions(before, after State) error {
	for id, old := range before.Plans {
		if !reflect.DeepEqual(old, after.Plans[id]) {
			return fmt.Errorf("historical plan changed %s", id)
		}
	}
	return nil
}

func validateResultTransitions(before, after State) error {
	for id, old := range before.Results {
		n, ok := after.Results[id]
		if !ok || (!old.Valid && n.Valid) {
			return fmt.Errorf("retained result removed or revived %s", id)
		}
		old.Valid, old.InvalidReason = n.Valid, n.InvalidReason
		if !reflect.DeepEqual(old, n) {
			return fmt.Errorf("retained output changed %s", id)
		}
	}
	return nil
}

func validateActionTransitions(before, after State) error {
	for id, a := range after.Actions {
		if _, existed := before.Actions[id]; !existed && a.Status != ActionPending {
			return fmt.Errorf("effect receipt lacks a prior intent %s", id)
		}
	}
	for id, old := range before.Actions {
		n, ok := after.Actions[id]
		if old.Approval != nil && !reflect.DeepEqual(old.Approval, n.Approval) {
			return fmt.Errorf("persisted approval changed %s", id)
		}
		if old.Approval == nil && n.Approval != nil && n.Status != ActionPending {
			return fmt.Errorf("approval was not persisted before the effect %s", id)
		}
		if !ok {
			return fmt.Errorf("action removed %s", id)
		}
		if old.Status == ActionPending {
			old.Status, old.Receipt, old.RecoveryReason, old.Approval = n.Status, n.Receipt, n.RecoveryReason, n.Approval
		} else if old.Status == ActionParked && n.Status == ActionCompleted {
			old.Status, old.Receipt = n.Status, n.Receipt
		}
		if !reflect.DeepEqual(old, n) {
			return fmt.Errorf("action intent changed %s", id)
		}
	}
	return nil
}

func validateQuestionTransitions(before, after State) error {
	for id, old := range before.Questions {
		n, ok := after.Questions[id]
		if !ok {
			return fmt.Errorf("question removed %s", id)
		}
		if old.Answer == "" && old.SupersededReason == "" {
			if n.SupersededReason != "" && !questionStale(after, n) {
				return fmt.Errorf("current question cannot be superseded %s", id)
			}
			if n.Answer != "" {
				a := after.Attempts[n.AttemptID]
				if a.Reservation.Held || questionStale(after, n) {
					return fmt.Errorf("answer does not match stopped work %s", id)
				}
			}
			old.Answer, old.AnsweredBy, old.SupersededReason = n.Answer, n.AnsweredBy, n.SupersededReason
		}
		if !reflect.DeepEqual(old, n) {
			return fmt.Errorf("question binding changed %s", id)
		}
	}
	return nil
}

func validateAcceptanceTransitions(before, after State) error {
	for id, old := range before.Acceptances {
		if !reflect.DeepEqual(old, after.Acceptances[id]) {
			return fmt.Errorf("acceptance changed %s", id)
		}
	}
	return nil
}

func ReferencedArtifacts(s State) []ArtifactRef {
	refs := map[ArtifactRef]bool{}
	add := func(ref ArtifactRef) { refs[ref] = true }
	for _, a := range s.Attempts {
		for _, ref := range a.Recipe.Artifacts {
			add(ref)
		}
		for _, ref := range a.Logs {
			add(ref)
		}
	}
	for _, r := range s.Results {
		for _, ref := range r.Artifacts {
			add(ref)
		}
		for _, ch := range r.Checks {
			if ch.Log != (ArtifactRef{}) {
				add(ch.Log)
			}
		}
	}
	for _, a := range s.Actions {
		if a.Payload != (ArtifactRef{}) {
			add(a.Payload)
		}
	}
	out := make([]ArtifactRef, 0, len(refs))
	for ref := range refs {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hash == out[j].Hash {
			return out[i].Size < out[j].Size
		}
		return out[i].Hash < out[j].Hash
	})
	return out
}
