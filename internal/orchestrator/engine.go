package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"
)

var (
	ErrStale    = errors.New("stale control state")
	ErrConflict = errors.New("control state conflict")
	ErrNotReady = errors.New("work is not ready")
	ErrBudget   = errors.New("objective budget exhausted")
)

type CommandKind string

const (
	CreateObjective          CommandKind = "create_objective"
	SetObjectiveStatus       CommandKind = "set_objective_status"
	PutWork                  CommandKind = "put_work"
	NoteWork                 CommandKind = "note_work"
	ReviseWork               CommandKind = "revise_work"
	PutPlan                  CommandKind = "put_plan"
	AdmitAttempt             CommandKind = "admit_attempt"
	CancelAttempt            CommandKind = "cancel_attempt"
	ParkAttempt              CommandKind = "park_attempt"
	UncertainAttempt         CommandKind = "uncertain_attempt"
	StopAttempt              CommandKind = "stop_attempt"
	RecordResult             CommandKind = "record_result"
	RecordCheckedResult      CommandKind = "record_checked_result"
	RecordVerificationPolicy CommandKind = "record_verification_policy"
	RecordCombinedReview     CommandKind = "record_combined_review"
	RecordAcceptance         CommandKind = "record_acceptance"
	RecordAction             CommandKind = "record_action"
	ApproveAction            CommandKind = "approve_action"
	ResolveAction            CommandKind = "resolve_action"
	AskQuestion              CommandKind = "ask_question"
	AnswerQuestion           CommandKind = "answer_question"
	ReleaseLease             CommandKind = "release_lease"
)

type Command struct {
	Kind               CommandKind
	ExpectedRevision   uint64
	Objective          Objective
	Work               WorkItem
	Plan               Plan
	Attempt            Attempt
	Result             Result
	VerificationPolicy VerificationPolicy
	Review             CombinedReview
	Acceptance         Acceptance
	Action             Action
	Question           Question
	ObjectiveID        string
	WorkID             string
	AttemptID          string
	QuestionID         string
	ActionID           string
	LeaseKey           string
	Preconditions      Preconditions
	Status             ObjectiveStatus
	AttemptStatus      AttemptStatus
	ActionStatus       ActionStatus
	Reason             string
	Notes              string
	Answer             string
	OperatorID         string
	Receipt            string
	Approval           *Approval
	At                 time.Time
	Usage              Usage
	CostKnown          bool
	Logs               []ArtifactRef
	Leases             []Lease
	Capacity           int
}

func Clone(s State) (State, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return State{}, err
	}
	var out State
	err = json.Unmarshal(data, &out)
	return out, err
}

// Apply does not perform effects. Callers must commit the returned state before executing an action.
func Apply(s State, c Command) (State, error) {
	return ApplyBatch(s, c.ExpectedRevision, []Command{c})
}

// ApplyBatch records related state changes and effect intents under one journal revision.
func ApplyBatch(s State, expectedRevision uint64, commands []Command) (State, error) {
	if s.Revision != expectedRevision {
		return State{}, ErrStale
	}
	if len(commands) == 0 {
		return State{}, fmt.Errorf("empty control transaction")
	}
	if err := Validate(s); err != nil {
		return State{}, err
	}
	n, err := Clone(s)
	if err != nil {
		return State{}, err
	}
	for _, c := range commands {
		if c.ExpectedRevision != expectedRevision {
			return State{}, ErrStale
		}
		if err = apply(&n, c); err != nil {
			return State{}, err
		}
	}
	if n.Revision == ^uint64(0) {
		return State{}, fmt.Errorf("revision overflow")
	}
	n.Revision++
	if err = ValidateTransition(s, n); err != nil {
		return State{}, err
	}
	return Clone(n)
}

func apply(s *State, c Command) error {
	switch c.Kind {
	case CreateObjective:
		return createObjective(s, c)
	case SetObjectiveStatus:
		return setObjectiveStatus(s, c)
	case PutWork:
		return putWork(s, c)
	case NoteWork, ReviseWork:
		return updateWork(s, c)
	case PutPlan:
		return putPlan(s, c)
	case AdmitAttempt:
		return admit(s, c)
	case CancelAttempt:
		return transitionAttempt(s, c, AttemptCancelling)
	case ParkAttempt:
		return transitionAttempt(s, c, AttemptParking)
	case UncertainAttempt:
		return transitionAttempt(s, c, AttemptUncertain)
	case StopAttempt:
		return stop(s, c)
	case RecordResult:
		if c.Result.Checked != nil {
			return fmt.Errorf("use record_checked_result for checked evidence")
		}
		return recordResult(s, c)
	case RecordCheckedResult:
		return recordCheckedResult(s, c)
	case RecordVerificationPolicy:
		return recordVerificationPolicy(s, c)
	case RecordCombinedReview:
		return recordCombinedReview(s, c)
	case RecordAcceptance:
		return recordAcceptance(s, c)
	case RecordAction:
		return recordAction(s, c)
	case ApproveAction:
		return approveAction(s, c)
	case ResolveAction:
		return resolveAction(s, c)
	case AskQuestion:
		return askQuestion(s, c)
	case AnswerQuestion:
		return answerQuestion(s, c)
	case ReleaseLease:
		return releaseLease(s, c)
	default:
		return fmt.Errorf("unknown command %q", c.Kind)
	}
}

func createObjective(s *State, c Command) error {
	o := c.Objective
	if o.ID == "" || s.Objectives[o.ID].ID != "" {
		return ErrConflict
	}
	if o.Status != "" && o.Status != ObjectiveDraft {
		return fmt.Errorf("new objectives must be drafts")
	}
	if o.Usage != (Usage{}) || o.CurrentPlan != 0 {
		return fmt.Errorf("new objective contains runtime state")
	}
	o.Status, o.DocumentRevision = ObjectiveDraft, 1
	if o.CriteriaRevision == 0 {
		o.CriteriaRevision = 1
	}
	s.Objectives[o.ID] = o
	return nil
}

func setObjectiveStatus(s *State, c Command) error {
	o, ok := s.Objectives[c.ObjectiveID]
	if !ok {
		return ErrConflict
	}
	if o.Status == ObjectiveCancelled || o.Status == ObjectiveAccepted {
		return ErrConflict
	}
	if c.Status != ObjectiveActive && c.Status != ObjectivePaused && c.Status != ObjectiveCancelled {
		return fmt.Errorf("unsupported objective status")
	}
	if c.Status == ObjectiveActive && c.Reason != "" {
		return fmt.Errorf("active objective cannot have a blocked reason")
	}
	o.Status, o.BlockedReason = c.Status, c.Reason
	o.DocumentRevision++
	s.Objectives[o.ID] = o
	if c.Status == ObjectiveCancelled {
		for id, a := range s.Attempts {
			if a.ObjectiveID == o.ID && a.Reservation.Held {
				a.Status, a.CancellationReason = AttemptCancelling, c.Reason
				s.Attempts[id] = a
			}
		}
	}
	return nil
}

func putWork(s *State, c Command) error {
	w := c.Work
	if w.ID == "" || s.Work[w.ID].ID != "" || s.Objectives[w.ObjectiveID].ID == "" {
		return ErrConflict
	}
	if w.ActiveAttemptID != "" || w.LastAttemptID != "" || w.ReplacedBy != "" {
		return fmt.Errorf("new work contains execution state")
	}
	w.DocumentRevision, w.SpecificationRevision = 1, 1
	if w.OwnershipGeneration == 0 {
		w.OwnershipGeneration = 1
	}
	s.Work[w.ID] = w
	return nil
}

func updateWork(s *State, c Command) error {
	w, err := checkedWork(*s, c.WorkID, c.Preconditions)
	if err != nil {
		return err
	}
	if c.Kind == NoteWork {
		w.Notes = c.Notes
	} else {
		if err := canInvalidate(*s, w.ID); err != nil {
			return err
		}
		w.Specification = c.Work.Specification
		w.Dependencies, w.Inputs = c.Work.Dependencies, c.Work.Inputs
		w.SpecificationRevision++
		invalidate(s, w.ID, "specification changed")
	}
	w.DocumentRevision++
	s.Work[w.ID] = w
	if c.Kind == ReviseWork {
		supersedeQuestions(s, w.ID, "specification changed")
	}
	return nil
}

func putPlan(s *State, c Command) error {
	p := c.Plan
	o, ok := s.Objectives[p.ObjectiveID]
	if !ok || p.ID == "" || s.Plans[p.ID].ID != "" || p.Revision != o.CurrentPlan+1 {
		return ErrConflict
	}
	for _, id := range p.ReplacedWorkIDs {
		w, ok := s.Work[id]
		if !ok || w.ObjectiveID != o.ID || w.ActiveAttemptID != "" || w.ReplacedBy != "" {
			return ErrConflict
		}
		if err := canInvalidate(*s, id); err != nil {
			return err
		}
		w.ReplacedBy = p.ID
		w.DocumentRevision++
		s.Work[id] = w
		supersedeQuestions(s, id, "work replaced")
		invalidate(s, id, "work replaced")
	}
	for _, old := range s.Plans {
		if old.ObjectiveID != o.ID || old.Revision != o.CurrentPlan {
			continue
		}
		for _, id := range old.WorkIDs {
			if !contains(p.WorkIDs, id) && !contains(p.ReplacedWorkIDs, id) {
				return fmt.Errorf("plan silently drops work %s", id)
			}
		}
		for _, id := range old.RequiredResults {
			if !contains(p.RequiredResults, id) && !contains(p.ReplacedWorkIDs, id) {
				return fmt.Errorf("plan silently drops required result %s", id)
			}
		}
	}
	s.Plans[p.ID] = p
	o.CurrentPlan, o.DocumentRevision = p.Revision, o.DocumentRevision+1
	s.Objectives[o.ID] = o
	return nil
}

// Replacement edges are explicit; legacy replacement lists do not choose successors.
func replacementWork(s State, workID string, revision PlanRevision) (string, error) {
	seen := map[string]bool{}
	for {
		if seen[workID] {
			return "", fmt.Errorf("work replacement cycle at %s", workID)
		}
		seen[workID] = true
		w, ok := s.Work[workID]
		if !ok {
			return "", ErrConflict
		}
		if w.ReplacedBy == "" || s.Plans[w.ReplacedBy].Revision > revision {
			return workID, nil
		}
		next := ""
		for _, p := range s.Plans {
			if p.ObjectiveID == w.ObjectiveID && p.Revision <= revision && p.ReplacementWorkIDs[workID] != "" {
				next = p.ReplacementWorkIDs[workID]
			}
		}
		if next == "" {
			return "", nil
		}
		workID = next
	}
}

func transitionAttempt(s *State, c Command, status AttemptStatus) error {
	a, err := exactAttempt(*s, c.WorkID, c.AttemptID)
	if err != nil {
		return err
	}
	if !a.Reservation.Held {
		return ErrStale
	}
	if c.Reason == "" {
		return fmt.Errorf("attempt transition requires a reason")
	}
	// Uncertainty does not replace a persisted cancellation or parking request.
	if status == AttemptUncertain && (a.Status == AttemptCancelling || a.Status == AttemptParking) {
		return nil
	}
	a.Status = status
	a.CancellationReason = c.Reason
	s.Attempts[a.ID] = a
	return nil
}

func recordResult(s *State, c Command) error {
	r := c.Result
	w, ok := s.Work[r.WorkID]
	a := s.Attempts[r.AttemptID]
	if !ok || w.LastAttemptID != r.AttemptID || w.ActiveAttemptID != "" || a.Status != AttemptSucceeded {
		return ErrStale
	}
	if r.ID == "" || s.Results[r.ID].ID != "" || r.SpecificationRevision != w.SpecificationRevision || !reflect.DeepEqual(r.Inputs, a.Inputs) {
		return ErrConflict
	}
	var generation uint64
	for _, old := range s.Results {
		if old.WorkID == w.ID && old.Generation > generation {
			generation = old.Generation
		}
	}
	if r.Generation != generation+1 || !r.Valid {
		return fmt.Errorf("invalid result generation")
	}
	if !hasRetainedOutput(r) {
		return fmt.Errorf("result requires retained output")
	}
	for _, check := range r.Checks {
		if check.ExitCode != 0 || !check.InputsUnchanged {
			return fmt.Errorf("result has failed or stale checks")
		}
	}
	if err := validInputs(*s, r.Inputs); err != nil {
		return err
	}
	if err := canInvalidate(*s, w.ID); err != nil {
		return err
	}
	invalidate(s, w.ID, "new result generation")
	s.Results[r.ID] = r
	return nil
}

func recordAction(s *State, c Command) error {
	a := c.Action
	if a.ID == "" || s.Actions[a.ID].ID != "" || s.Objectives[a.ObjectiveID].ID == "" {
		return ErrConflict
	}
	if err := checkAction(*s, a); err != nil {
		return err
	}
	if a.Status != "" && a.Status != ActionPending {
		return ErrConflict
	}
	if a.Receipt != "" || a.RecoveryReason != "" {
		return ErrConflict
	}
	a.Status = ActionPending
	s.Actions[a.ID] = a
	return nil
}

func approveAction(s *State, c Command) error {
	a, ok := s.Actions[c.ActionID]
	if !ok || a.Status != ActionPending || a.Approval != nil || !a.RequiresApproval {
		return ErrConflict
	}
	if err := checkAction(*s, a); err != nil {
		return err
	}
	if !validApproval(a, c.Approval) {
		return fmt.Errorf("approval must bind an operator to the exact action")
	}
	a.Approval = c.Approval
	s.Actions[a.ID] = a
	return nil
}

func resolveAction(s *State, c Command) error {
	a, ok := s.Actions[c.ActionID]
	if !ok || (a.Status != ActionPending && (a.Status != ActionParked || c.ActionStatus != ActionCompleted)) {
		return ErrStale
	}
	switch c.ActionStatus {
	case ActionParked:
		if c.Reason == "" {
			return fmt.Errorf("ambiguous effect requires a recovery reason")
		}
		a.Status, a.RecoveryReason = ActionParked, c.Reason
	case ActionCompleted:
		if a.Status == ActionPending {
			if err := ActionReady(*s, a.ID); err != nil {
				return err
			}
		} else if a.RequiresApproval && !validApproval(a, a.Approval) {
			return fmt.Errorf("completed effect requires a prior bound approval")
		}
		// A parked effect accepts only inspected completion evidence, never permission to repeat the effect.
		if c.Receipt == "" {
			return fmt.Errorf("completed action requires a receipt")
		}
		if c.Approval != nil {
			return fmt.Errorf("approval must be persisted before executing an effect")
		}
		a.Status, a.Receipt = ActionCompleted, c.Receipt
	case ActionPending:
		return fmt.Errorf("invalid action resolution")
	default:
		return fmt.Errorf("invalid action resolution")
	}
	s.Actions[a.ID] = a
	return nil
}

func askQuestion(s *State, c Command) error {
	q := c.Question
	a, err := exactAttempt(*s, q.WorkID, q.AttemptID)
	if err != nil {
		return err
	}
	if q.ID == "" || s.Questions[q.ID].ID != "" || !a.Reservation.Held || q.Answer != "" || q.AnsweredBy != "" || q.SupersededReason != "" {
		return ErrConflict
	}
	if q.SpecificationRevision != a.SpecificationRevision || q.ObjectiveID != a.ObjectiveID || q.Prompt == "" {
		return ErrStale
	}
	if a.Status != AttemptRunning {
		return ErrConflict
	}
	s.Questions[q.ID] = q
	a.Status, a.CancellationReason = AttemptParking, "waiting for question "+q.ID
	s.Attempts[a.ID] = a
	return nil
}

func answerQuestion(s *State, c Command) error {
	q, ok := s.Questions[c.QuestionID]
	if !ok || q.Answer != "" || c.Answer == "" || c.OperatorID == "" {
		return ErrConflict
	}
	a := s.Attempts[q.AttemptID]
	if a.Reservation.Held || q.SupersededReason != "" || questionStale(*s, q) {
		return ErrStale
	}
	q.Answer, q.AnsweredBy = c.Answer, c.OperatorID
	s.Questions[q.ID] = q
	return nil
}

func questionStale(s State, q Question) bool {
	w := s.Work[q.WorkID]
	return w.SpecificationRevision != q.SpecificationRevision || w.LastAttemptID != q.AttemptID || w.ReplacedBy != ""
}

func supersedeQuestions(s *State, workID, reason string) {
	for id, q := range s.Questions {
		if q.WorkID == workID && q.Answer == "" && q.SupersededReason == "" && questionStale(*s, q) {
			q.SupersededReason = reason
			s.Questions[id] = q
		}
	}
}

func releaseLease(s *State, c Command) error {
	l, ok := s.Leases[c.LeaseKey]
	if !ok || l.WorkID != c.WorkID || l.AttemptID != "" || s.Work[l.WorkID].ActiveAttemptID != "" || c.Reason == "" {
		return ErrConflict
	}
	delete(s.Leases, c.LeaseKey)
	return nil
}

func checkedWork(s State, id string, p Preconditions) (WorkItem, error) {
	w, ok := s.Work[id]
	if !ok {
		return WorkItem{}, ErrConflict
	}
	if p.WorkID != "" && p.WorkID != id {
		return WorkItem{}, ErrStale
	}
	if p.DocumentRevision != w.DocumentRevision || p.SpecificationRevision != w.SpecificationRevision {
		return WorkItem{}, ErrStale
	}
	if p.AttemptID != "" && p.AttemptID != w.ActiveAttemptID {
		return WorkItem{}, ErrStale
	}
	return w, nil
}

func exactAttempt(s State, workID, attemptID string) (Attempt, error) {
	w, ok := s.Work[workID]
	a, found := s.Attempts[attemptID]
	if !ok || !found || w.ActiveAttemptID != attemptID || a.WorkID != workID {
		return Attempt{}, ErrStale
	}
	return a, nil
}

func admit(s *State, c Command) error {
	a := c.Attempt
	if a.ID == "" || s.Attempts[a.ID].ID != "" {
		return ErrConflict
	}
	w, ok := s.Work[a.WorkID]
	if !ok {
		return ErrConflict
	}
	if err := Ready(*s, w.ID); err != nil {
		return err
	}
	if a.ObjectiveID != w.ObjectiveID || a.SpecificationRevision != w.SpecificationRevision || !reflect.DeepEqual(a.Inputs, w.Inputs) {
		return ErrStale
	}
	if err := validateNewAttempt(*s, w, c); err != nil {
		return err
	}
	runtimeReserved, err := checkAttemptCapacity(*s, w, c)
	if err != nil {
		return err
	}
	if err := checkAttemptBudget(s.Objectives[w.ObjectiveID], w.Kind, a, runtimeReserved); err != nil {
		return err
	}
	if err := reserveAttemptLeases(s, &a, c.Leases); err != nil {
		return err
	}
	a.Status, a.StartedAt, a.Reservation.Held = AttemptRunning, c.At, true
	s.Attempts[a.ID] = a
	w.ActiveAttemptID, w.LastAttemptID = a.ID, a.ID
	s.Work[w.ID] = w
	o := s.Objectives[w.ObjectiveID]
	o.Usage.Attempts++
	if w.Kind == WorkPlanner {
		o.Usage.PlannerTurns++
	}
	s.Objectives[o.ID] = o
	return nil
}

func validateNewAttempt(s State, w WorkItem, c Command) error {
	a := c.Attempt
	if a.Recipe.Timeout <= 0 || a.Recipe.Profile == "" || a.Recipe.ConfigurationRevision == "" || c.At.IsZero() {
		return fmt.Errorf("attempt requires a frozen recipe and start time")
	}
	if a.Status != "" || a.Reservation.Held || len(a.Reservation.LeaseKeys) != 0 || a.Usage != (Usage{}) || !a.StoppedAt.IsZero() || a.TerminationEvidence != "" || a.CancellationReason != "" || len(a.Logs) != 0 {
		return fmt.Errorf("new attempt contains runtime state")
	}
	if a.Recipe.Repository != "" && !contains(s.Objectives[w.ObjectiveID].Policy.Repositories, a.Recipe.Repository) {
		return fmt.Errorf("repository outside objective scope")
	}
	if a.Recipe.Verification != nil {
		if a.Recipe.Verification.VerificationPolicyID != s.Objectives[w.ObjectiveID].VerificationPolicyID {
			return ErrStale
		}
		if err := validateVerificationRecipe(s, a); err != nil {
			return err
		}
	}
	if a.Reservation.Slots <= 0 || c.Capacity <= 0 {
		return fmt.Errorf("positive execution capacity required")
	}
	return nil
}

func checkAttemptCapacity(s State, w WorkItem, c Command) (time.Duration, error) {
	a := c.Attempt
	used := 0
	plannerActive := false
	var runtimeReserved time.Duration
	for _, old := range s.Attempts {
		if !old.Reservation.Held {
			continue
		}
		if old.Reservation.Slots > c.Capacity || used > c.Capacity-old.Reservation.Slots {
			return 0, ErrNotReady
		}
		used += old.Reservation.Slots
		if s.Work[old.WorkID].Kind == WorkPlanner {
			plannerActive = true
		}
		if old.ObjectiveID == w.ObjectiveID {
			if old.Recipe.Timeout > time.Duration(1<<63-1)-runtimeReserved {
				return 0, ErrBudget
			}
			runtimeReserved += old.Recipe.Timeout
		}
	}
	if a.Reservation.Slots > c.Capacity || used > c.Capacity-a.Reservation.Slots {
		return 0, ErrNotReady
	}
	if w.Kind == WorkPlanner && plannerActive {
		return 0, ErrNotReady
	}
	return runtimeReserved, nil
}

func checkAttemptBudget(o Objective, kind WorkKind, a Attempt, runtimeReserved time.Duration) error {
	b, u := o.Budget, o.Usage
	if b.MaxAttempts == 0 || u.Attempts >= b.MaxAttempts || (kind == WorkPlanner && (b.MaxPlannerTurns == 0 || u.PlannerTurns >= b.MaxPlannerTurns)) {
		return ErrBudget
	}
	if b.MaxRuntime <= 0 || a.Recipe.Timeout > b.MaxRuntime || runtimeReserved > b.MaxRuntime-a.Recipe.Timeout || u.Runtime > b.MaxRuntime-a.Recipe.Timeout-runtimeReserved {
		return ErrBudget
	}
	if b.MaxCostMicros > 0 && (u.UnknownCost || u.CostMicros >= b.MaxCostMicros || a.Recipe.PricingRevision == "") {
		return ErrBudget
	}
	return nil
}

func reserveAttemptLeases(s *State, a *Attempt, leases []Lease) error {
	keys := map[string]bool{}
	for _, l := range leases {
		if l.Key == "" || keys[l.Key] || l.WorkID != a.WorkID || (l.AttemptID != "" && l.AttemptID != a.ID) {
			return ErrConflict
		}
		keys[l.Key] = true
		if old, exists := s.Leases[l.Key]; exists && (old.WorkID != a.WorkID || old.AttemptID != "" || old.Retain != l.Retain) {
			return ErrConflict
		}
		l.AttemptID = a.ID
		s.Leases[l.Key] = l
		a.Reservation.LeaseKeys = append(a.Reservation.LeaseKeys, l.Key)
	}
	for key, l := range s.Leases {
		if l.WorkID == a.WorkID && l.Retain && !keys[key] {
			return fmt.Errorf("attempt must acquire retained lease %s", key)
		}
	}
	return nil
}

func stop(s *State, c Command) error {
	a, err := exactAttempt(*s, c.WorkID, c.AttemptID)
	if err != nil {
		return err
	}
	if !a.Reservation.Held || c.Receipt == "" || c.At.Before(a.StartedAt) {
		return fmt.Errorf("stopping requires evidence for the exact live attempt")
	}
	if c.AttemptStatus != AttemptSucceeded && c.AttemptStatus != AttemptFailed && c.AttemptStatus != AttemptCancelled && c.AttemptStatus != AttemptParked {
		return fmt.Errorf("invalid terminal attempt status")
	}
	if a.Status != AttemptRunning && c.AttemptStatus == AttemptSucceeded {
		return ErrConflict
	}
	if c.Usage.Runtime < 0 || c.Usage.CostMicros < 0 || c.Usage.Attempts != 0 || c.Usage.PlannerTurns != 0 {
		return fmt.Errorf("invalid attempt accounting")
	}
	// Account elapsed execution even when a provider omits usage. Human waits start only after termination.
	u := c.Usage
	if elapsed := c.At.Sub(a.StartedAt); u.Runtime < elapsed {
		u.Runtime = elapsed
	}
	o := s.Objectives[a.ObjectiveID]
	if !c.CostKnown {
		u.UnknownCost = true
	}
	if o.Usage.Runtime > time.Duration(1<<63-1)-u.Runtime || o.Usage.CostMicros > int64(1<<63-1)-u.CostMicros {
		return fmt.Errorf("usage overflow")
	}
	o.Usage.Runtime += u.Runtime
	o.Usage.CostMicros += u.CostMicros
	o.Usage.UnknownCost = o.Usage.UnknownCost || u.UnknownCost
	s.Objectives[o.ID] = o
	a.Status, a.StoppedAt, a.TerminationEvidence, a.Usage, a.Logs = c.AttemptStatus, c.At, c.Receipt, u, c.Logs
	a.Reservation.Held = false
	s.Attempts[a.ID] = a
	w := s.Work[a.WorkID]
	w.ActiveAttemptID = ""
	s.Work[w.ID] = w
	for _, key := range a.Reservation.LeaseKeys {
		l, ok := s.Leases[key]
		if !ok || l.AttemptID != a.ID {
			return ErrConflict
		}
		if l.Retain {
			l.AttemptID = ""
			s.Leases[key] = l
		} else {
			delete(s.Leases, key)
		}
	}
	return nil
}

func validApproval(a Action, approval *Approval) bool {
	return approval != nil && approval.OperatorID != "" && approval.Binding == ActionBinding(a) && !approval.ApprovedAt.IsZero()
}

// ActionReady checks control preconditions only. The caller must establish operator identity and inspect external effects.
func ActionReady(s State, actionID string) error {
	a, ok := s.Actions[actionID]
	if !ok || a.Status != ActionPending {
		return ErrConflict
	}
	if previous := a.Preconditions.PreviousActionID; previous != "" {
		prior, ok := s.Actions[previous]
		if !ok || prior.ObjectiveID != a.ObjectiveID || prior.Status != ActionCompleted {
			return ErrConflict
		}
	}
	if err := checkAction(s, a); err != nil {
		return err
	}
	if a.RequiresApproval && !validApproval(a, a.Approval) {
		return fmt.Errorf("action requires a persisted operator approval")
	}
	return nil
}

// ActionBinding binds approval to the frozen intent, not to its later receipt.
func ActionBinding(a Action) string {
	data, _ := json.Marshal(struct {
		ID            string
		ObjectiveID   string
		Kind          string
		Payload       ArtifactRef
		Preconditions Preconditions
	}{a.ID, a.ObjectiveID, a.Kind, a.Payload, a.Preconditions})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func checkAction(s State, a Action) error {
	if a.Preconditions.WorkID == "" && (a.Preconditions.AttemptID != "" || a.Preconditions.DocumentRevision != 0 || a.Preconditions.SpecificationRevision != 0) {
		return fmt.Errorf("action revisions require a work identity")
	}
	if a.Kind == "" {
		return fmt.Errorf("action requires a kind")
	}
	if a.Preconditions.WorkID != "" {
		w, err := checkedWork(s, a.Preconditions.WorkID, a.Preconditions)
		if err != nil {
			return err
		}
		if w.ObjectiveID != a.ObjectiveID {
			return ErrConflict
		}
	}
	return nil
}

func contains(values []string, value string) bool {
	return slices.Contains(values, value)
}
