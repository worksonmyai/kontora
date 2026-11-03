package orchestrator

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// Review ranges are pinned; never derive them from a moving branch.
type QualifiedResult struct {
	Result  ResultRef         `json:"result"`
	Outputs []QualifiedOutput `json:"outputs"`
	Ranges  []ReviewRange     `json:"ranges"`
}

type AcceptanceBinding struct {
	CriteriaRevision     uint64            `json:"criteria_revision"`
	PlanRevision         PlanRevision      `json:"plan_revision"`
	VerificationPolicyID string            `json:"verification_policy_id"`
	RequiredResults      []ResultRef       `json:"required_results"`
	Obligations          []ResultRef       `json:"obligations,omitempty"`
	Current              []QualifiedResult `json:"current"`
}

type ReviewFinding struct {
	Message      string `json:"message"`
	RepairWorkID string `json:"repair_work_id"`
}

type CombinedReview struct {
	ID          string            `json:"id"`
	ObjectiveID string            `json:"objective_id"`
	ReviewerID  string            `json:"reviewer_id"`
	ReviewedAt  time.Time         `json:"reviewed_at"`
	Binding     AcceptanceBinding `json:"binding"`
	Evidence    ArtifactRef       `json:"evidence"`
	Findings    []ReviewFinding   `json:"findings"`
	AttemptIDs  []string          `json:"attempt_ids,omitempty"`
}

// Only checked results support acceptance. The returned binding is detached.
func CurrentAcceptanceBinding(s State, objectiveID string) (AcceptanceBinding, error) {
	o, ok := s.Objectives[objectiveID]
	if !ok || o.CurrentPlan == 0 || o.VerificationPolicyID == "" {
		return AcceptanceBinding{}, ErrNotReady
	}
	policy, ok := s.VerificationPolicies[o.VerificationPolicyID]
	if !ok || policy.ObjectiveID != objectiveID {
		return AcceptanceBinding{}, ErrNotReady
	}
	var plan Plan
	for _, p := range s.Plans {
		if p.ObjectiveID == objectiveID && p.Revision == o.CurrentPlan {
			plan = p
		}
	}
	if len(plan.RequiredResults) == 0 {
		return AcceptanceBinding{}, ErrNotReady
	}
	binding := AcceptanceBinding{CriteriaRevision: o.CriteriaRevision, PlanRevision: o.CurrentPlan, VerificationPolicyID: policy.ID}
	current := map[string]ResultRef{}
	for _, workID := range plan.WorkIDs {
		w := s.Work[workID]
		if w.Divergence != "" || w.ReplacedBy != "" {
			return AcceptanceBinding{}, ErrNotReady
		}
		for _, r := range s.Results {
			if r.WorkID != workID || !r.Valid {
				continue
			}
			if current[workID].ResultID != "" || r.SpecificationRevision != w.SpecificationRevision || r.Checked == nil || r.Checked.VerificationPolicyID != policy.ID {
				return AcceptanceBinding{}, ErrNotReady
			}
			if err := validInputs(s, r.Inputs); err != nil {
				return AcceptanceBinding{}, err
			}
			if err := validateCheckedOutput(s, r); err != nil {
				return AcceptanceBinding{}, err
			}
			ref := ResultRef{ResultID: r.ID, WorkID: r.WorkID, Generation: r.Generation}
			current[workID] = ref
			binding.Current = append(binding.Current, QualifiedResult{Result: ref, Outputs: r.Checked.Outputs, Ranges: r.Checked.Ranges})
		}
	}
	for _, workID := range plan.RequiredResults {
		ref, ok := current[workID]
		if !ok {
			return AcceptanceBinding{}, ErrNotReady
		}
		binding.RequiredResults = append(binding.RequiredResults, ref)
	}
	obligations, err := currentObligations(s, objectiveID, plan.Revision, current)
	if err != nil {
		return AcceptanceBinding{}, err
	}
	binding.Obligations = obligations
	if err := combinedOutputsChecked(s, binding); err != nil {
		return AcceptanceBinding{}, err
	}
	sort.Slice(binding.Obligations, func(i, j int) bool { return binding.Obligations[i].WorkID < binding.Obligations[j].WorkID })
	sort.Slice(binding.RequiredResults, func(i, j int) bool { return binding.RequiredResults[i].WorkID < binding.RequiredResults[j].WorkID })
	sort.Slice(binding.Current, func(i, j int) bool { return binding.Current[i].Result.WorkID < binding.Current[j].Result.WorkID })
	// Do not expose slices in retained result evidence.
	data, err := json.Marshal(binding)
	if err != nil {
		return AcceptanceBinding{}, err
	}
	var detached AcceptanceBinding
	if err := json.Unmarshal(data, &detached); err != nil {
		return AcceptanceBinding{}, err
	}
	return detached, nil
}

func currentObligations(s State, objectiveID string, revision PlanRevision, current map[string]ResultRef) ([]ResultRef, error) {
	seen := map[string]bool{}
	var obligations []ResultRef
	for _, w := range s.Work {
		if w.ObjectiveID != objectiveID || !w.AcceptanceRequired {
			continue
		}
		workID, err := replacementWork(s, w.ID, revision)
		if err != nil || workID == "" || current[workID].ResultID == "" {
			return nil, ErrNotReady
		}
		if !seen[workID] {
			obligations = append(obligations, current[workID])
			seen[workID] = true
		}
	}
	return obligations, nil
}

func acceptanceIdle(s State, objectiveID string) error {
	o := s.Objectives[objectiveID]
	if o.Status != ObjectiveActive || o.BlockedReason != "" {
		return ErrNotReady
	}
	for _, a := range s.Attempts {
		if a.ObjectiveID == objectiveID && a.Reservation.Held {
			return ErrNotReady
		}
	}
	for _, a := range s.Actions {
		if a.ObjectiveID == objectiveID && a.Status != ActionCompleted {
			return ErrNotReady
		}
	}
	for _, q := range s.Questions {
		if q.ObjectiveID == objectiveID && q.Answer == "" && q.SupersededReason == "" && !questionStale(s, q) {
			return ErrNotReady
		}
	}
	return nil
}

// Readiness grants no authority; the transport must authenticate the operator.
func AcceptanceReady(s State, objectiveID, reviewID string) error {
	if err := acceptanceIdle(s, objectiveID); err != nil {
		return err
	}
	binding, err := CurrentAcceptanceBinding(s, objectiveID)
	if err != nil {
		return err
	}
	review, ok := s.Reviews[reviewID]
	if !ok || review.ObjectiveID != objectiveID || len(review.Findings) != 0 || !reflect.DeepEqual(review.Binding, binding) {
		return ErrStale
	}
	return reviewRepairsChecked(s, objectiveID, binding)
}

// Upstream changes can make stored human decisions stale without removing them.
func AcceptanceCurrent(s State, acceptanceID string) error {
	a, ok := s.Acceptances[acceptanceID]
	if !ok || a.Binding == nil {
		return ErrStale
	}
	binding, err := CurrentAcceptanceBinding(s, a.ObjectiveID)
	if err != nil || !reflect.DeepEqual(binding, *a.Binding) {
		return ErrStale
	}
	return reviewRepairsChecked(s, a.ObjectiveID, binding)
}

func combinedOutputsChecked(s State, binding AcceptanceBinding) error {
	byRepository := map[string][]ResultRef{}
	heads := map[string]map[string]bool{}
	for _, q := range binding.Current {
		for _, output := range q.Outputs {
			if output.Commit == "" {
				continue
			}
			if heads[output.Repository] == nil {
				heads[output.Repository] = map[string]bool{}
			}
			heads[output.Repository][output.Commit] = true
			byRepository[output.Repository] = append(byRepository[output.Repository], q.Result)
		}
	}
	for repository, refs := range byRepository {
		if len(heads[repository]) <= 1 {
			continue
		}
		combined := false
		for _, candidate := range refs {
			consumed := resultClosure(s, candidate)
			all := true
			for _, ref := range refs {
				all = all && consumed[ref]
			}
			combined = combined || all
		}
		if !combined {
			return fmt.Errorf("differing heads in repository %s require a checked combined output", repository)
		}
	}
	return nil
}

func resultClosure(s State, ref ResultRef) map[ResultRef]bool {
	seen := map[ResultRef]bool{}
	var visit func(ResultRef)
	visit = func(ref ResultRef) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		for _, input := range s.Results[ref.ResultID].Inputs {
			visit(input)
		}
	}
	visit(ref)
	return seen
}

func reviewAttemptIDs(s State, objectiveID string) []string {
	var ids []string
	for id, a := range s.Attempts {
		if a.ObjectiveID == objectiveID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Older reviews without an admission snapshot use a strict timestamp fallback.
func freshReviewResult(s State, r Result, review CombinedReview) bool {
	fresh := func(id string) bool {
		a, ok := s.Attempts[id]
		if !ok {
			return false
		}
		if len(review.AttemptIDs) == 0 {
			return a.StartedAt.After(review.ReviewedAt)
		}
		return !contains(review.AttemptIDs, id)
	}
	if !fresh(r.AttemptID) || len(r.Checks) == 0 {
		return false
	}
	for _, check := range r.Checks {
		if !fresh(check.AttemptID) {
			return false
		}
	}
	return true
}

func reviewRepairsChecked(s State, objectiveID string, binding AcceptanceBinding) error {
	for _, prior := range s.Reviews {
		if prior.ObjectiveID != objectiveID || len(prior.Findings) == 0 {
			continue
		}
		current := map[string]ResultRef{}
		for _, q := range binding.Current {
			current[q.Result.WorkID] = q.Result
		}
		for _, finding := range prior.Findings {
			workID, err := replacementWork(s, finding.RepairWorkID, binding.PlanRevision)
			if err != nil {
				return err
			}
			ref := current[workID]
			if ref.ResultID == "" || !freshReviewResult(s, s.Results[ref.ResultID], prior) {
				return fmt.Errorf("review finding requires a fresh checked repair result")
			}
		}
		changed := false
		for _, ref := range prior.Binding.RequiredResults {
			workID, err := replacementWork(s, ref.WorkID, binding.PlanRevision)
			if err != nil {
				return err
			}
			result := s.Results[current[workID].ResultID]
			changed = changed || (result.ID != "" && freshReviewResult(s, result, prior))
		}
		if !changed {
			return fmt.Errorf("review findings require fresh checks of the required results")
		}
	}
	return nil
}

func recordCombinedReview(s *State, c Command) error {
	r := c.Review
	if r.ID == "" || s.Reviews[r.ID].ID != "" || c.OperatorID == "" || c.At.IsZero() || r.ReviewerID != "" || !r.ReviewedAt.IsZero() || len(r.AttemptIDs) != 0 || !validArtifact(r.Evidence) {
		return ErrConflict
	}
	if err := acceptanceIdle(*s, r.ObjectiveID); err != nil {
		return err
	}
	binding, err := CurrentAcceptanceBinding(*s, r.ObjectiveID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(r.Binding, binding) {
		return ErrStale
	}
	if len(r.Findings) == 0 {
		if err := reviewRepairsChecked(*s, r.ObjectiveID, binding); err != nil {
			return err
		}
	}
	for _, finding := range r.Findings {
		if finding.Message == "" || s.Work[finding.RepairWorkID].ObjectiveID != r.ObjectiveID {
			return fmt.Errorf("review findings require tracked repair work")
		}
	}
	r.ReviewerID, r.ReviewedAt = c.OperatorID, c.At
	r.AttemptIDs = reviewAttemptIDs(*s, r.ObjectiveID)
	if s.Reviews == nil {
		s.Reviews = map[string]CombinedReview{}
	}
	s.Reviews[r.ID] = r
	return nil
}

// Command.OperatorID must be supplied by trusted transport, not copied from request
// JSON or a worker capability. This pure domain package cannot authenticate callers.
func recordAcceptance(s *State, c Command) error {
	a := c.Acceptance
	if a.ID == "" || s.Acceptances[a.ID].ID != "" || c.OperatorID == "" || c.At.IsZero() || a.Binding == nil || a.OperatorID != "" || !a.AcceptedAt.IsZero() {
		return ErrConflict
	}
	if err := AcceptanceReady(*s, a.ObjectiveID, a.ReviewID); err != nil {
		return err
	}
	binding, err := CurrentAcceptanceBinding(*s, a.ObjectiveID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(*a.Binding, binding) {
		return ErrStale
	}
	if (a.CriteriaRevision != 0 && a.CriteriaRevision != binding.CriteriaRevision) || (a.PlanRevision != 0 && a.PlanRevision != binding.PlanRevision) || (a.CheckRevision != "" && a.CheckRevision != s.VerificationPolicies[binding.VerificationPolicyID].Revision) || (len(a.Results) != 0 && !reflect.DeepEqual(a.Results, binding.RequiredResults)) {
		return ErrStale
	}
	a.OperatorID, a.AcceptedAt = c.OperatorID, c.At
	a.CriteriaRevision, a.PlanRevision = binding.CriteriaRevision, binding.PlanRevision
	a.CheckRevision = s.VerificationPolicies[binding.VerificationPolicyID].Revision
	a.Results = binding.RequiredResults
	s.Acceptances[a.ID] = a
	o := s.Objectives[a.ObjectiveID]
	o.Status, o.DocumentRevision = ObjectiveAccepted, o.DocumentRevision+1
	s.Objectives[o.ID] = o
	return nil
}

func validateVerificationPolicies(s State) error {
	for id, p := range s.VerificationPolicies {
		if id != p.ID || s.Objectives[p.ObjectiveID].ID == "" {
			return fmt.Errorf("invalid verification policy identity")
		}
		if err := validateVerificationPolicy(p); err != nil {
			return err
		}
	}
	for _, o := range s.Objectives {
		if o.VerificationPolicyID != "" && s.VerificationPolicies[o.VerificationPolicyID].ObjectiveID != o.ID {
			return fmt.Errorf("missing current verification policy")
		}
	}
	return nil
}

func validateCheckedRecords(s State) error {
	if err := validateVerificationPolicies(s); err != nil {
		return err
	}
	for id, review := range s.Reviews {
		if id == "" || id != review.ID || s.Objectives[review.ObjectiveID].ID == "" || review.ReviewerID == "" || review.ReviewedAt.IsZero() || !validArtifact(review.Evidence) {
			return fmt.Errorf("invalid combined review")
		}
		if err := validateReviewAttempts(s, review); err != nil {
			return err
		}
		if err := validateHistoricalBinding(s, review.ObjectiveID, review.Binding); err != nil {
			return err
		}
		for _, finding := range review.Findings {
			if finding.Message == "" || s.Work[finding.RepairWorkID].ObjectiveID != review.ObjectiveID {
				return fmt.Errorf("untracked review finding")
			}
		}
	}
	for _, a := range s.Acceptances {
		// Legacy acceptance records are not evidence for checked outputs.
		if a.Binding == nil {
			continue
		}
		review, ok := s.Reviews[a.ReviewID]
		if !ok || review.ObjectiveID != a.ObjectiveID || len(review.Findings) != 0 || !reflect.DeepEqual(review.Binding, *a.Binding) || a.CriteriaRevision != a.Binding.CriteriaRevision || a.PlanRevision != a.Binding.PlanRevision || a.CheckRevision != s.VerificationPolicies[a.Binding.VerificationPolicyID].Revision || !reflect.DeepEqual(a.Results, a.Binding.RequiredResults) {
			return fmt.Errorf("invalid checked acceptance")
		}
		if err := validateHistoricalBinding(s, a.ObjectiveID, *a.Binding); err != nil {
			return err
		}
	}
	return nil
}

func validateReviewAttempts(s State, review CombinedReview) error {
	seen := map[string]bool{}
	for _, id := range review.AttemptIDs {
		if seen[id] || s.Attempts[id].ObjectiveID != review.ObjectiveID {
			return fmt.Errorf("invalid review admission snapshot")
		}
		seen[id] = true
	}
	return nil
}

// Historical evidence may refer to invalidated generations; freshness is checked separately.
func validateHistoricalBinding(s State, objectiveID string, b AcceptanceBinding) error {
	o := s.Objectives[objectiveID]
	if b.CriteriaRevision != o.CriteriaRevision || b.PlanRevision == 0 || s.VerificationPolicies[b.VerificationPolicyID].ObjectiveID != objectiveID {
		return fmt.Errorf("invalid evidence binding")
	}
	var plan Plan
	for _, p := range s.Plans {
		if p.ObjectiveID == objectiveID && p.Revision == b.PlanRevision {
			plan = p
		}
	}
	if plan.ID == "" || len(b.RequiredResults) != len(plan.RequiredResults) || len(b.RequiredResults) == 0 {
		return fmt.Errorf("invalid required evidence")
	}
	current := map[string]ResultRef{}
	for _, qualified := range b.Current {
		r := s.Results[qualified.Result.ResultID]
		if err := checkRef(s, qualified.Result); err != nil {
			return err
		}
		if !contains(plan.WorkIDs, r.WorkID) || current[r.WorkID].ResultID != "" || r.Checked == nil || r.Checked.VerificationPolicyID != b.VerificationPolicyID || !reflect.DeepEqual(qualified.Outputs, r.Checked.Outputs) || !reflect.DeepEqual(qualified.Ranges, r.Checked.Ranges) {
			return fmt.Errorf("mismatched qualified vector")
		}
		current[r.WorkID] = qualified.Result
	}
	seen := map[string]bool{}
	for _, ref := range b.RequiredResults {
		if seen[ref.WorkID] || !contains(plan.RequiredResults, ref.WorkID) || current[ref.WorkID] != ref {
			return fmt.Errorf("mismatched required vector")
		}
		seen[ref.WorkID] = true
	}
	return validateHistoricalObligations(s, objectiveID, b, current)
}

func validateHistoricalObligations(s State, objectiveID string, b AcceptanceBinding, current map[string]ResultRef) error {
	seenObligations := map[string]bool{}
	for _, ref := range b.Obligations {
		if seenObligations[ref.WorkID] || current[ref.WorkID] != ref {
			return fmt.Errorf("mismatched obligation vector")
		}
		tracked := false
		for _, w := range s.Work {
			if w.ObjectiveID == objectiveID && w.AcceptanceRequired {
				workID, err := replacementWork(s, w.ID, b.PlanRevision)
				if err != nil {
					return err
				}
				tracked = tracked || workID == ref.WorkID
			}
		}
		if !tracked {
			return fmt.Errorf("untracked acceptance obligation")
		}
		seenObligations[ref.WorkID] = true
	}
	return nil
}

func validateCheckedTransitions(before, after State) error {
	if err := validateNewCheckedEvidence(before, after); err != nil {
		return err
	}
	for id, old := range before.VerificationPolicies {
		if !reflect.DeepEqual(old, after.VerificationPolicies[id]) {
			return fmt.Errorf("verification policy changed %s", id)
		}
	}
	for id, old := range before.Reviews {
		if !reflect.DeepEqual(old, after.Reviews[id]) {
			return fmt.Errorf("combined review changed %s", id)
		}
	}
	if err := validateNewCheckedAcceptances(before, after); err != nil {
		return err
	}
	return validateCheckedObjectiveTransitions(before, after)
}

func validateNewCheckedEvidence(before, after State) error {
	for id, r := range after.Results {
		if before.Results[id].ID == "" && r.Checked != nil && r.Checked.VerificationPolicyID != after.Objectives[after.Work[r.WorkID].ObjectiveID].VerificationPolicyID {
			return ErrStale
		}
	}
	for id, review := range after.Reviews {
		if before.Reviews[id].ID != "" {
			continue
		}
		if err := acceptanceIdle(after, review.ObjectiveID); err != nil {
			return err
		}
		binding, err := CurrentAcceptanceBinding(after, review.ObjectiveID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(review.AttemptIDs, reviewAttemptIDs(after, review.ObjectiveID)) {
			return fmt.Errorf("combined review requires the current admission snapshot")
		}
		if !reflect.DeepEqual(binding, review.Binding) {
			return ErrStale
		}
		if len(review.Findings) == 0 {
			if err := reviewRepairsChecked(before, review.ObjectiveID, binding); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateNewCheckedAcceptances(before, after State) error {
	for id, a := range after.Acceptances {
		if _, existed := before.Acceptances[id]; existed || a.Binding == nil {
			continue
		}
		if err := AcceptanceReady(before, a.ObjectiveID, a.ReviewID); err != nil {
			return err
		}
		binding, err := CurrentAcceptanceBinding(before, a.ObjectiveID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(binding, *a.Binding) || after.Objectives[a.ObjectiveID].Status != ObjectiveAccepted {
			return ErrStale
		}
		if err := AcceptanceCurrent(after, a.ID); err != nil {
			return err
		}
	}
	return nil
}

func validateCheckedObjectiveTransitions(before, after State) error {
	for id, o := range after.Objectives {
		old := before.Objectives[id]
		if old.VerificationPolicyID != o.VerificationPolicyID {
			p, ok := after.VerificationPolicies[o.VerificationPolicyID]
			if !ok || before.VerificationPolicies[p.ID].ID != "" || p.ObjectiveID != id || o.DocumentRevision != old.DocumentRevision+1 {
				return fmt.Errorf("verification policy selection lacks a new operator record")
			}
		}
		if old.Status != ObjectiveAccepted && o.Status == ObjectiveAccepted {
			found := false
			for aid, a := range after.Acceptances {
				if before.Acceptances[aid].ID == "" && a.ObjectiveID == id && a.Binding != nil {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("acceptance requires checked operator evidence")
			}
		}
		if old.Status == ObjectiveAccepted && o.Status != ObjectiveAccepted {
			return ErrConflict
		}
	}
	return nil
}
