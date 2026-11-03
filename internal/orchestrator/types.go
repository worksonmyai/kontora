package orchestrator

import "time"

type DocumentRevision uint64
type SpecificationRevision uint64
type PlanRevision uint64

type ObjectiveStatus string

const (
	ObjectiveDraft     ObjectiveStatus = "draft"
	ObjectiveActive    ObjectiveStatus = "active"
	ObjectivePaused    ObjectiveStatus = "paused"
	ObjectiveCancelled ObjectiveStatus = "cancelled"
	ObjectiveAccepted  ObjectiveStatus = "accepted"
)

type Policy struct {
	Revision              uint64   `json:"revision"`
	Repositories          []string `json:"repositories"`
	RequireCommitApproval bool     `json:"require_commit_approval"`
	RequireAcceptance     bool     `json:"require_acceptance"`
}

type Budget struct {
	MaxAttempts     uint64        `json:"max_attempts"`
	MaxPlannerTurns uint64        `json:"max_planner_turns"`
	MaxRuntime      time.Duration `json:"max_runtime"`
	MaxCostMicros   int64         `json:"max_cost_micros"`
}

type Usage struct {
	Attempts     uint64        `json:"attempts"`
	PlannerTurns uint64        `json:"planner_turns"`
	Runtime      time.Duration `json:"runtime"`
	CostMicros   int64         `json:"cost_micros"`
	UnknownCost  bool          `json:"unknown_cost"`
}

type Objective struct {
	ID                   string           `json:"id"`
	DocumentRevision     DocumentRevision `json:"document_revision"`
	Assignment           string           `json:"assignment"`
	Criteria             []string         `json:"criteria"`
	CriteriaRevision     uint64           `json:"criteria_revision"`
	Policy               Policy           `json:"policy"`
	Budget               Budget           `json:"budget"`
	Usage                Usage            `json:"usage"`
	Status               ObjectiveStatus  `json:"status"`
	BlockedReason        string           `json:"blocked_reason,omitempty"`
	CurrentPlan          PlanRevision     `json:"current_plan"`
	VerificationPolicyID string           `json:"verification_policy_id,omitempty"`
}

type Plan struct {
	ID                 string            `json:"id"`
	ObjectiveID        string            `json:"objective_id"`
	Revision           PlanRevision      `json:"revision"`
	Rationale          string            `json:"rationale"`
	WorkIDs            []string          `json:"work_ids"`
	RequiredResults    []string          `json:"required_results"`
	ReplacedWorkIDs    []string          `json:"replaced_work_ids"`
	ReplacementWorkIDs map[string]string `json:"replacement_work_ids,omitempty"`
}

type WorkKind string

const (
	WorkTicket  WorkKind = "ticket"
	WorkJob     WorkKind = "job"
	WorkPlanner WorkKind = "planner"
	WorkCheck   WorkKind = "check"
	WorkSummary WorkKind = "summary"
)

type WorkItem struct {
	ID                    string                `json:"id"`
	ObjectiveID           string                `json:"objective_id"`
	Kind                  WorkKind              `json:"kind"`
	TicketID              string                `json:"ticket_id,omitempty"`
	OwnershipGeneration   uint64                `json:"ownership_generation"`
	DocumentRevision      DocumentRevision      `json:"document_revision"`
	SpecificationRevision SpecificationRevision `json:"specification_revision"`
	Specification         string                `json:"specification"`
	Notes                 string                `json:"notes,omitempty"`
	AcceptanceRequired    bool                  `json:"acceptance_required,omitempty"`
	Dependencies          []string              `json:"dependencies"`
	Inputs                []ResultRef           `json:"inputs"`
	ActiveAttemptID       string                `json:"active_attempt_id,omitempty"`
	LastAttemptID         string                `json:"last_attempt_id,omitempty"`
	ReplacedBy            string                `json:"replaced_by,omitempty"`
	Divergence            string                `json:"divergence,omitempty"`
}

type ResultRef struct {
	ResultID   string `json:"result_id"`
	WorkID     string `json:"work_id"`
	Generation uint64 `json:"generation"`
}

type ArtifactRef struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type Recipe struct {
	Profile               string                 `json:"profile"`
	ConfigurationRevision string                 `json:"configuration_revision"`
	PricingRevision       string                 `json:"pricing_revision,omitempty"`
	Timeout               time.Duration          `json:"timeout"`
	Repository            string                 `json:"repository,omitempty"`
	BaseCommit            string                 `json:"base_commit,omitempty"`
	Artifacts             []ArtifactRef          `json:"artifacts"`
	Verification          *VerificationExecution `json:"verification,omitempty"`
}

type AttemptStatus string

const (
	AttemptRunning    AttemptStatus = "running"
	AttemptCancelling AttemptStatus = "cancelling"
	AttemptParking    AttemptStatus = "parking"
	AttemptUncertain  AttemptStatus = "uncertain"
	AttemptSucceeded  AttemptStatus = "succeeded"
	AttemptFailed     AttemptStatus = "failed"
	AttemptCancelled  AttemptStatus = "cancelled"
	AttemptParked     AttemptStatus = "parked"
)

type Reservation struct {
	Slots     int      `json:"slots"`
	Held      bool     `json:"held"`
	LeaseKeys []string `json:"lease_keys"`
}

type Attempt struct {
	ID                    string                `json:"id"`
	WorkID                string                `json:"work_id"`
	ObjectiveID           string                `json:"objective_id"`
	SpecificationRevision SpecificationRevision `json:"specification_revision"`
	Inputs                []ResultRef           `json:"inputs"`
	Recipe                Recipe                `json:"recipe"`
	Reservation           Reservation           `json:"reservation"`
	Status                AttemptStatus         `json:"status"`
	StartedAt             time.Time             `json:"started_at"`
	StoppedAt             time.Time             `json:"stopped_at"`
	CancellationReason    string                `json:"cancellation_reason,omitempty"`
	TerminationEvidence   string                `json:"termination_evidence,omitempty"`
	Usage                 Usage                 `json:"usage"`
	Logs                  []ArtifactRef         `json:"logs"`
}

type Lease struct {
	Key       string `json:"key"`
	WorkID    string `json:"work_id"`
	AttemptID string `json:"attempt_id,omitempty"`
	Retain    bool   `json:"retain"`
}

type Check struct {
	DefinitionRevision    string            `json:"definition_revision"`
	ConfigurationRevision string            `json:"configuration_revision"`
	Command               []string          `json:"command"`
	Directory             string            `json:"directory"`
	Timeout               time.Duration     `json:"timeout"`
	ExitCode              int               `json:"exit_code"`
	InputsUnchanged       bool              `json:"inputs_unchanged"`
	Log                   ArtifactRef       `json:"log"`
	ID                    string            `json:"id,omitempty"`
	AttemptID             string            `json:"attempt_id,omitempty"`
	Inputs                []ResultRef       `json:"inputs,omitzero"`
	Outputs               []QualifiedOutput `json:"outputs,omitzero"`
}

type Result struct {
	ID                    string                `json:"id"`
	WorkID                string                `json:"work_id"`
	AttemptID             string                `json:"attempt_id"`
	Generation            uint64                `json:"generation"`
	SpecificationRevision SpecificationRevision `json:"specification_revision"`
	Inputs                []ResultRef           `json:"inputs"`
	Commit                string                `json:"commit,omitempty"`
	Artifacts             []ArtifactRef         `json:"artifacts"`
	Checks                []Check               `json:"checks"`
	Valid                 bool                  `json:"valid"`
	InvalidReason         string                `json:"invalid_reason,omitempty"`
	Checked               *CheckedOutput        `json:"checked,omitempty"`
}

type ActionStatus string

const (
	ActionPending   ActionStatus = "pending"
	ActionCompleted ActionStatus = "completed"
	ActionParked    ActionStatus = "parked"
)

type Preconditions struct {
	PreviousActionID      string                `json:"previous_action_id,omitempty"`
	WorkID                string                `json:"work_id,omitempty"`
	DocumentRevision      DocumentRevision      `json:"document_revision"`
	SpecificationRevision SpecificationRevision `json:"specification_revision"`
	AttemptID             string                `json:"attempt_id,omitempty"`
}

type Approval struct {
	OperatorID string    `json:"operator_id"`
	Binding    string    `json:"binding"`
	ApprovedAt time.Time `json:"approved_at"`
}

type Action struct {
	ID               string        `json:"id"`
	ObjectiveID      string        `json:"objective_id"`
	Kind             string        `json:"kind"`
	Payload          ArtifactRef   `json:"payload"`
	Preconditions    Preconditions `json:"preconditions"`
	RequiresApproval bool          `json:"requires_approval"`
	Approval         *Approval     `json:"approval,omitempty"`
	Status           ActionStatus  `json:"status"`
	Receipt          string        `json:"receipt,omitempty"`
	RecoveryReason   string        `json:"recovery_reason,omitempty"`
}

type Question struct {
	ID                    string                `json:"id"`
	ObjectiveID           string                `json:"objective_id"`
	WorkID                string                `json:"work_id"`
	AttemptID             string                `json:"attempt_id"`
	SpecificationRevision SpecificationRevision `json:"specification_revision"`
	Prompt                string                `json:"prompt"`
	Answer                string                `json:"answer,omitempty"`
	AnsweredBy            string                `json:"answered_by,omitempty"`
	SupersededReason      string                `json:"superseded_reason,omitempty"`
}

type Acceptance struct {
	ID               string             `json:"id"`
	ObjectiveID      string             `json:"objective_id"`
	OperatorID       string             `json:"operator_id"`
	CriteriaRevision uint64             `json:"criteria_revision"`
	PlanRevision     PlanRevision       `json:"plan_revision"`
	CheckRevision    string             `json:"check_revision"`
	Results          []ResultRef        `json:"results"`
	AcceptedAt       time.Time          `json:"accepted_at"`
	Binding          *AcceptanceBinding `json:"binding,omitempty"`
	ReviewID         string             `json:"review_id,omitempty"`
}

type State struct {
	BoardID              string                        `json:"board_id"`
	Revision             uint64                        `json:"revision"`
	Requests             map[string]ControlRequest     `json:"requests,omitempty"`
	Objectives           map[string]Objective          `json:"objectives"`
	Plans                map[string]Plan               `json:"plans"`
	Work                 map[string]WorkItem           `json:"work"`
	Attempts             map[string]Attempt            `json:"attempts"`
	Results              map[string]Result             `json:"results"`
	Actions              map[string]Action             `json:"actions"`
	Questions            map[string]Question           `json:"questions"`
	Acceptances          map[string]Acceptance         `json:"acceptances"`
	Leases               map[string]Lease              `json:"leases"`
	VerificationPolicies map[string]VerificationPolicy `json:"verification_policies,omitempty"`
	Reviews              map[string]CombinedReview     `json:"reviews,omitempty"`
}

func NewState(boardID string) State {
	return State{BoardID: boardID, Objectives: map[string]Objective{}, Plans: map[string]Plan{}, Work: map[string]WorkItem{}, Attempts: map[string]Attempt{}, Results: map[string]Result{}, Actions: map[string]Action{}, Questions: map[string]Question{}, Acceptances: map[string]Acceptance{}, Leases: map[string]Lease{}}
}
