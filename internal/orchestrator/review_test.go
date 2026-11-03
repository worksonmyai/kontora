package orchestrator

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInvalidInputsRequireStopIntent(t *testing.T) {
	for _, status := range []AttemptStatus{AttemptRunning, AttemptUncertain, AttemptCancelling, AttemptParking} {
		t.Run(string(status), func(t *testing.T) {
			s := withResult(t, testState(t), "w", "producer", "r")
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "consumer", ObjectiveID: "o", Kind: WorkJob, Specification: "consume", Inputs: []ResultRef{{ResultID: "r", WorkID: "w", Generation: 1}}}})
			s = applyTest(t, s, admitCommand(s, "consumer", "live"))
			n, err := Clone(s)
			require.NoError(t, err)
			n.Revision++
			r := n.Results["r"]
			r.Valid, r.InvalidReason = false, "external edit"
			n.Results[r.ID] = r
			a := n.Attempts["live"]
			a.Status, a.CancellationReason = status, "external edit"
			n.Attempts[a.ID] = a
			err = ValidateTransition(s, n)
			if status == AttemptRunning || status == AttemptUncertain {
				require.ErrorIs(t, err, ErrNotReady)
				return
			}
			require.NoError(t, err)
			require.True(t, n.Attempts["live"].Reservation.Held)
			n = applyTest(t, n, Command{Kind: UncertainAttempt, WorkID: "consumer", AttemptID: "live", Reason: "inspection unavailable"})
			require.Equal(t, status, n.Attempts["live"].Status)
			require.Equal(t, "external edit", n.Attempts["live"].CancellationReason)
			_, err = Apply(n, Command{Kind: StopAttempt, ExpectedRevision: n.Revision, WorkID: "consumer", AttemptID: "live", AttemptStatus: AttemptSucceeded, Receipt: "stopped", At: testNow.Add(time.Second)})
			require.ErrorIs(t, err, ErrConflict)
			terminal := AttemptCancelled
			if status == AttemptParking {
				terminal = AttemptParked
			}
			n = stopped(t, n, "consumer", "live", terminal)
			require.False(t, n.Attempts["live"].Reservation.Held)
			require.ErrorIs(t, Ready(n, "consumer"), ErrNotReady)
		})
	}
}

func TestHistoricalInputsDoNotInvalidateIndependentWork(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained result", true: "live attempt"}[live], func(t *testing.T) {
			s := withResult(t, testState(t), "w", "producer", "r")
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "b", ObjectiveID: "o", Kind: WorkJob, Specification: "consume", Inputs: []ResultRef{{ResultID: "r", WorkID: "w", Generation: 1}}}})
			s = withResult(t, s, "b", "b1", "old-b")
			s = applyTest(t, s, Command{Kind: ReviseWork, WorkID: "b", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "independent"}})
			require.False(t, s.Results["old-b"].Valid)
			s = applyTest(t, s, admitCommand(s, "b", "b2"))
			if !live {
				s = stopped(t, s, "b", "b2", AttemptSucceeded)
				s = applyTest(t, s, Command{Kind: RecordResult, Result: Result{ID: "new-b", WorkID: "b", AttemptID: "b2", Generation: 2, SpecificationRevision: 2, Commit: "independent-commit", Valid: true}})
			}
			require.Equal(t, map[string]bool{"w": true}, AffectedWork(s, "w"))
			s = applyTest(t, s, Command{Kind: ReviseWork, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "revised producer"}})
			require.Equal(t, "specification changed", s.Results["old-b"].InvalidReason)
			if live {
				require.Equal(t, AttemptRunning, s.Attempts["b2"].Status)
			} else {
				require.True(t, s.Results["new-b"].Valid)
			}
		})
	}
}

func TestQuestionSupersession(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "revise", true: "replace"}[replace], func(t *testing.T) {
			s := testState(t)
			s = applyTest(t, s, admitCommand(s, "w", "a"))
			s = applyTest(t, s, Command{Kind: AskQuestion, Question: Question{ID: "q", ObjectiveID: "o", WorkID: "w", AttemptID: "a", SpecificationRevision: 1, Prompt: "which input?"}})
			s = stopped(t, s, "w", "a", AttemptParked)
			require.ErrorIs(t, Ready(s, "w"), ErrNotReady)
			if replace {
				s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p", ObjectiveID: "o", Revision: 1, ReplacedWorkIDs: []string{"w"}}})
			} else {
				s = applyTest(t, s, Command{Kind: ReviseWork, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "new question context"}})
				require.NoError(t, Ready(s, "w"))
				legacy, err := Clone(s)
				require.NoError(t, err)
				q := legacy.Questions["q"]
				q.SupersededReason = ""
				legacy.Questions[q.ID] = q
				require.NoError(t, Validate(legacy))
				require.NoError(t, Ready(legacy, "w"))
				s = applyTest(t, s, admitCommand(s, "w", "b"))
			}
			require.NotEmpty(t, s.Questions["q"].SupersededReason)
			require.Equal(t, "which input?", s.Questions["q"].Prompt)
			require.Empty(t, s.Questions["q"].Answer)
			_, err := Apply(s, Command{Kind: AnswerQuestion, ExpectedRevision: s.Revision, QuestionID: "q", Answer: "stale", OperatorID: "human"})
			require.ErrorIs(t, err, ErrStale)
			n, err := Clone(s)
			require.NoError(t, err)
			n.Revision++
			q := n.Questions["q"]
			q.SupersededReason = ""
			n.Questions[q.ID] = q
			require.Error(t, ValidateTransition(s, n))
		})
	}
}

func TestParkedEffectEvidenceOnlyCompletion(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		approved, receipt, stale bool
		valid                    bool
	}{
		{name: "complete", approved: true, receipt: true, valid: true},
		{name: "stale preconditions with exact evidence", approved: true, receipt: true, stale: true, valid: true},
		{name: "missing evidence", approved: true},
		{name: "missing prior approval", receipt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t)
			a := Action{ID: "effect", ObjectiveID: "o", Kind: "projection", RequiresApproval: true, Preconditions: Preconditions{WorkID: "w", DocumentRevision: 1, SpecificationRevision: 1}}
			s = applyTest(t, s, Command{Kind: RecordAction, Action: a})
			if tc.approved {
				s = applyTest(t, s, Command{Kind: ApproveAction, ActionID: a.ID, Approval: &Approval{OperatorID: "human", Binding: ActionBinding(a), ApprovedAt: testNow}})
			}
			s = applyTest(t, s, Command{Kind: ResolveAction, ActionID: a.ID, ActionStatus: ActionParked, Reason: "inspection unavailable"})
			if tc.stale {
				s = applyTest(t, s, Command{Kind: NoteWork, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Notes: "new document"})
			}
			require.Error(t, ActionReady(s, a.ID))
			require.ErrorIs(t, Ready(s, "w"), ErrNotReady)
			_, err := Apply(s, Command{Kind: ResolveAction, ExpectedRevision: s.Revision, ActionID: a.ID, ActionStatus: ActionPending})
			require.Error(t, err)
			c := Command{Kind: ResolveAction, ExpectedRevision: s.Revision, ActionID: a.ID, ActionStatus: ActionCompleted}
			if tc.receipt {
				c.Receipt = "exact inspected completion"
			}
			n, err := Apply(s, c)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.Receipt, n.Actions[a.ID].Receipt)
			require.Equal(t, "inspection unavailable", n.Actions[a.ID].RecoveryReason)
			require.NoError(t, Ready(n, "w"))
			require.Error(t, ActionReady(n, a.ID))
			c.ExpectedRevision = n.Revision
			_, err = Apply(n, c)
			require.ErrorIs(t, err, ErrStale)
		})
	}
}

func TestStopIntentSurvivesRepeatedUncertainty(t *testing.T) {
	for _, tc := range []struct {
		kind             CommandKind
		status, terminal AttemptStatus
	}{
		{CancelAttempt, AttemptCancelling, AttemptCancelled},
		{ParkAttempt, AttemptParking, AttemptParked},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			s := testState(t)
			c := admitCommand(s, "w", "a")
			c.Leases = []Lease{{Key: "workspace", WorkID: "w", Retain: true}}
			s = applyTest(t, s, c)
			s = applyTest(t, s, Command{Kind: tc.kind, WorkID: "w", AttemptID: "a", Reason: "original request"})
			lost, err := Clone(s)
			require.NoError(t, err)
			lost.Revision++
			a := lost.Attempts["a"]
			a.Status, a.CancellationReason = AttemptUncertain, "inspection failed"
			lost.Attempts[a.ID] = a
			require.Error(t, ValidateTransition(s, lost))
			for range 2 {
				s = applyTest(t, s, Command{Kind: UncertainAttempt, WorkID: "w", AttemptID: "a", Reason: "inspection failed"})
				require.Equal(t, tc.status, s.Attempts["a"].Status)
				require.Equal(t, "original request", s.Attempts["a"].CancellationReason)
				require.True(t, s.Attempts["a"].Reservation.Held)
				require.Equal(t, "a", s.Leases["workspace"].AttemptID)
			}
			s = stopped(t, s, "w", "a", tc.terminal)
			require.Equal(t, tc.terminal, s.Attempts["a"].Status)
			require.Equal(t, "original request", s.Attempts["a"].CancellationReason)
			require.False(t, s.Attempts["a"].Reservation.Held)
			require.Empty(t, s.Leases["workspace"].AttemptID)
		})
	}
}

func TestDecisionReplacementShadowGraph(t *testing.T) {
	a := PlannerWork{ID: "a", Specification: "original"}
	b := PlannerWork{ID: "b", Specification: "replacement"}
	replace := Proposal{ID: "replace", Kind: "replace_work", Target: "a", Replacement: "b"}
	for _, tc := range []struct {
		name    string
		actions []Proposal
		valid   bool
	}{
		{"replacement transfers requirement", []Proposal{replace}, true},
		{"schedule replacement", []Proposal{replace, {ID: "schedule", Kind: "schedule", Target: "b", Profile: "worker"}}, true},
		{"schedule replaced", []Proposal{replace, {ID: "schedule", Kind: "schedule", Target: "a", Profile: "worker"}}, false},
		{"replace scheduled", []Proposal{{ID: "schedule", Kind: "schedule", Target: "a", Profile: "worker"}, replace}, false},
		{"replacement cycle", []Proposal{replace, {ID: "cycle", Kind: "replace_work", Target: "b", Replacement: "a"}}, false},
		{"restore with create", []Proposal{replace, {ID: "restore", Kind: "create_work", Work: &a}}, false},
		{"restore with plan", []Proposal{replace, {ID: "restore", Kind: "revise_plan", Plan: &PlanProposal{Rationale: "restore", Work: []PlannerWork{a, b}, RequiredResults: []string{"b"}}}}, false},
		{"update replaced", []Proposal{replace, {ID: "update", Kind: "update_work", Work: &a}}, false},
		{"dependency on replaced", []Proposal{replace, {ID: "update", Kind: "update_work", Work: &PlannerWork{ID: "b", Specification: "replacement", Dependencies: []string{"a"}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := FreezePlannerSnapshot(PlannerSnapshot{TurnID: "turn", ObjectiveID: "o", Work: map[string]PlannerWork{"a": a, "b": b}, RequiredResults: []string{"a", "b"}, Profiles: []string{"worker"}, RemainingActions: 3, Budget: Budget{MaxAttempts: 5, MaxRuntime: time.Hour}})
			require.NoError(t, err)
			data, err := json.Marshal(Decision{Version: 1, TurnID: "turn", SnapshotID: snapshot.ID, Actions: tc.actions})
			require.NoError(t, err)
			_, err = DecodeDecision(data, snapshot, testNow)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Len(t, snapshot.Work, 2)
		})
	}
}

func TestDecisionExecutionBudgets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		budget    Budget
		usage     Usage
		schedules int
		valid     bool
	}{
		{"last attempt used by planner", Budget{MaxAttempts: 1, MaxRuntime: time.Hour}, Usage{Attempts: 1}, 1, false},
		{"zero attempt allowance", Budget{MaxRuntime: time.Hour}, Usage{}, 1, false},
		{"runtime exhausted", Budget{MaxAttempts: 5, MaxRuntime: time.Minute}, Usage{Runtime: time.Minute}, 1, false},
		{"zero runtime allowance", Budget{MaxAttempts: 5}, Usage{}, 1, false},
		{"cost exhausted", Budget{MaxAttempts: 5, MaxRuntime: time.Hour, MaxCostMicros: 10}, Usage{CostMicros: 10}, 1, false},
		{"cost unknown", Budget{MaxAttempts: 5, MaxRuntime: time.Hour, MaxCostMicros: 10}, Usage{UnknownCost: true}, 1, false},
		{"no monetary limit", Budget{MaxAttempts: 5, MaxRuntime: time.Hour}, Usage{UnknownCost: true}, 1, true},
		{"one remaining", Budget{MaxAttempts: 2, MaxRuntime: time.Hour}, Usage{Attempts: 1}, 1, true},
		{"combined schedules exceed remaining", Budget{MaxAttempts: 2, MaxRuntime: time.Hour}, Usage{Attempts: 1}, 2, false},
		{"exhausted permits nonexecution", Budget{MaxAttempts: 1, MaxRuntime: time.Minute, MaxCostMicros: 10}, Usage{Attempts: 1, Runtime: time.Minute, CostMicros: 10}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := PlannerWork{ID: "w", Specification: "original"}
			snapshot, err := FreezePlannerSnapshot(PlannerSnapshot{TurnID: "turn", ObjectiveID: "o", Budget: tc.budget, Usage: tc.usage, Work: map[string]PlannerWork{"w": work}, Profiles: []string{"worker"}, RemainingActions: 3})
			require.NoError(t, err)
			work.Specification = "updated"
			actions := []Proposal{{ID: "update", Kind: "update_work", Work: &work}}
			for i := range tc.schedules {
				actions = append(actions, Proposal{ID: string(rune('a' + i)), Kind: "schedule", Target: "w", Profile: "worker"})
			}
			data, err := json.Marshal(Decision{Version: 1, TurnID: "turn", SnapshotID: snapshot.ID, Actions: actions})
			require.NoError(t, err)
			_, err = DecodeDecision(data, snapshot, testNow)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrBudget)
			}
			require.Equal(t, "original", snapshot.Work["w"].Specification)
		})
	}
}

func TestNilPlannerWork(t *testing.T) {
	for _, work := range []map[string]PlannerWork{nil, {}} {
		for _, create := range []bool{false, true} {
			snapshot, err := FreezePlannerSnapshot(PlannerSnapshot{TurnID: "turn", ObjectiveID: "o", Work: work, RemainingActions: 1})
			require.NoError(t, err)
			action := Proposal{ID: "wait", Kind: "wait", Wait: &PlannerWait{Event: "feedback"}}
			if create {
				action = Proposal{ID: "create", Kind: "create_work", Work: &PlannerWork{ID: "new", Specification: "new work"}}
			}
			data, err := json.Marshal(Decision{Version: 1, TurnID: "turn", SnapshotID: snapshot.ID, Actions: []Proposal{action}})
			require.NoError(t, err)
			_, err = DecodeDecision(data, snapshot, testNow)
			require.NoError(t, err)
			require.Empty(t, snapshot.Work)
		}
	}
}
