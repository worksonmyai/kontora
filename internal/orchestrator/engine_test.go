package orchestrator

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var testNow = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

func applyTest(t *testing.T, s State, c Command) State {
	t.Helper()
	c.ExpectedRevision = s.Revision
	n, err := Apply(s, c)
	if err != nil {
		t.Fatalf("%s: %v", c.Kind, err)
	}
	return n
}

func testState(t *testing.T) State {
	t.Helper()
	s := NewState("board")
	s = applyTest(t, s, Command{Kind: CreateObjective, Objective: Objective{ID: "o", Assignment: "build", Criteria: []string{"checked"}, Policy: Policy{Revision: 1, Repositories: []string{"repo"}}, Budget: Budget{MaxAttempts: 10, MaxPlannerTurns: 3, MaxRuntime: time.Hour}}})
	s = applyTest(t, s, Command{Kind: SetObjectiveStatus, ObjectiveID: "o", Status: ObjectiveActive})
	return applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "w", ObjectiveID: "o", Kind: WorkJob, Specification: "implement"}})
}

func admitCommand(s State, workID, attemptID string) Command {
	w := s.Work[workID]
	return Command{Kind: AdmitAttempt, ExpectedRevision: s.Revision, At: testNow, Capacity: 1, Attempt: Attempt{ID: attemptID, WorkID: workID, ObjectiveID: w.ObjectiveID, SpecificationRevision: w.SpecificationRevision, Inputs: w.Inputs, Recipe: Recipe{Profile: "worker", ConfigurationRevision: "config-1", PricingRevision: "prices-1", Timeout: time.Minute, Repository: "repo"}, Reservation: Reservation{Slots: 1}}}
}

func stopped(t *testing.T, s State, workID, attemptID string, status AttemptStatus) State {
	t.Helper()
	return applyTest(t, s, Command{Kind: StopAttempt, WorkID: workID, AttemptID: attemptID, AttemptStatus: status, At: testNow.Add(time.Second), Receipt: "process-group-confirmed-dead"})
}

func withResult(t *testing.T, s State, workID, attemptID, resultID string) State {
	t.Helper()
	s = applyTest(t, s, admitCommand(s, workID, attemptID))
	s = stopped(t, s, workID, attemptID, AttemptSucceeded)
	w := s.Work[workID]
	return applyTest(t, s, Command{Kind: RecordResult, Result: Result{ID: resultID, WorkID: workID, AttemptID: attemptID, Generation: 1, SpecificationRevision: w.SpecificationRevision, Inputs: w.Inputs, Commit: "retained-commit", Valid: true}})
}

func TestRevisionIdentities(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(State) Command
		doc   DocumentRevision
		spec  SpecificationRevision
		valid bool
	}{
		{"note", func(s State) Command {
			return Command{Kind: NoteWork, WorkID: "w", Notes: "new note", Preconditions: Preconditions{DocumentRevision: s.Work["w"].DocumentRevision, SpecificationRevision: 1}}
		}, 2, 1, true},
		{"specification", func(s State) Command {
			return Command{Kind: ReviseWork, WorkID: "w", Work: WorkItem{Specification: "changed"}, Preconditions: Preconditions{DocumentRevision: s.Work["w"].DocumentRevision, SpecificationRevision: 1}}
		}, 2, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := withResult(t, testState(t), "w", "attempt-1", "r")
			before, _ := Clone(s)
			n := applyTest(t, s, tc.edit(s))
			if n.Work["w"].DocumentRevision != tc.doc || n.Work["w"].SpecificationRevision != tc.spec || n.Results["r"].Valid != tc.valid {
				t.Fatalf("unexpected revisions: %+v %+v", n.Work["w"], n.Results["r"])
			}
			if !reflect.DeepEqual(s, before) || !reflect.DeepEqual(s.Attempts["attempt-1"], n.Attempts["attempt-1"]) {
				t.Fatal("transition mutated its input or attempt identity")
			}
		})
	}
}

func TestExactAttemptLifecycle(t *testing.T) {
	for _, kind := range []CommandKind{CancelAttempt, ParkAttempt, UncertainAttempt} {
		t.Run(string(kind), func(t *testing.T) {
			s := testState(t)
			c := admitCommand(s, "w", "old")
			c.Leases = []Lease{{Key: "repo:branch", WorkID: "w", Retain: true}, {Key: "slot", WorkID: "w"}}
			s = applyTest(t, s, c)
			s = applyTest(t, s, Command{Kind: kind, WorkID: "w", AttemptID: "old", Reason: "operator request"})
			if !s.Attempts["old"].Reservation.Held || len(s.Leases) != 2 {
				t.Fatal("unconfirmed termination released resources")
			}
			if _, err := Apply(s, admitCommand(s, "w", "new")); !errors.Is(err, ErrNotReady) {
				t.Fatalf("admission during termination: %v", err)
			}
			bad := Command{Kind: StopAttempt, ExpectedRevision: s.Revision, WorkID: "w", AttemptID: "old", At: testNow.Add(time.Second), AttemptStatus: AttemptCancelled}
			if _, err := Apply(s, bad); err == nil {
				t.Fatal("stop without evidence succeeded")
			}
			s = stopped(t, s, "w", "old", AttemptCancelled)
			if s.Attempts["old"].Reservation.Held || len(s.Leases) != 1 || s.Leases["repo:branch"].AttemptID != "" {
				t.Fatal("confirmed stop did not retain workspace and release capacity")
			}
			c = admitCommand(s, "w", "new")
			c.Leases = []Lease{{Key: "repo:branch", WorkID: "w", Retain: true}}
			s = applyTest(t, s, c)
			for _, staleKind := range []CommandKind{StopAttempt, CancelAttempt, ParkAttempt, UncertainAttempt} {
				_, err := Apply(s, Command{Kind: staleKind, ExpectedRevision: s.Revision, WorkID: "w", AttemptID: "old", Reason: "late", Receipt: "dead", At: testNow.Add(time.Second), AttemptStatus: AttemptSucceeded})
				if !errors.Is(err, ErrStale) {
					t.Fatalf("stale %s: %v", staleKind, err)
				}
			}
			if s.Work["w"].ActiveAttemptID != "new" || s.Objectives["o"].Usage.Attempts != 2 || s.Objectives["o"].Usage.Runtime != time.Second {
				t.Fatal("replacement or accounting changed")
			}
		})
	}
}

func TestResourceLeases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retain   bool
		release  bool
		admitted bool
	}{
		{"retained-after-stop", true, false, false},
		{"transient-after-stop", false, false, true},
		{"explicit-release", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t)
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "other", ObjectiveID: "o", Kind: WorkJob, Specification: "other work"}})
			first := admitCommand(s, "w", "a")
			first.Leases = []Lease{{Key: "resolved-common-dir:refs/heads/delivery", WorkID: "w", Retain: tc.retain}}
			s = applyTest(t, s, first)
			second := admitCommand(s, "other", "b")
			second.Capacity = 2
			second.Leases = []Lease{{Key: first.Leases[0].Key, WorkID: "other", Retain: tc.retain}}
			if _, err := Apply(s, second); !errors.Is(err, ErrConflict) {
				t.Fatalf("live lease conflict: %v", err)
			}
			s = stopped(t, s, "w", "a", AttemptFailed)
			if tc.release {
				s = applyTest(t, s, Command{Kind: ReleaseLease, WorkID: "w", LeaseKey: first.Leases[0].Key, Reason: "operator disposition"})
			}
			second.ExpectedRevision = s.Revision
			_, err := Apply(s, second)
			if (err == nil) != tc.admitted {
				t.Fatalf("lease after termination: %v", err)
			}
		})
	}
}

func TestQuestionParking(t *testing.T) {
	s := testState(t)
	s = applyTest(t, s, admitCommand(s, "w", "a"))
	s = applyTest(t, s, Command{Kind: AskQuestion, Question: Question{ID: "q", ObjectiveID: "o", WorkID: "w", AttemptID: "a", SpecificationRevision: 1, Prompt: "which input?"}})
	answer := Command{Kind: AnswerQuestion, ExpectedRevision: s.Revision, QuestionID: "q", Answer: "use x", OperatorID: "human"}
	if _, err := Apply(s, answer); !errors.Is(err, ErrStale) {
		t.Fatalf("answer before stop: %v", err)
	}
	if !s.Attempts["a"].Reservation.Held {
		t.Fatal("question freed live worker capacity")
	}
	s = stopped(t, s, "w", "a", AttemptParked)
	if _, err := Apply(s, admitCommand(s, "w", "b")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unanswered question admission: %v", err)
	}
	s = applyTest(t, s, answer)
	s = applyTest(t, s, admitCommand(s, "w", "b"))
	if s.Work["w"].ActiveAttemptID != "b" || s.Objectives["o"].Usage.Attempts != 2 {
		t.Fatal("answer did not require fresh admission")
	}
}

func TestAdmissionChecks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*State, *Command)
		want    error
	}{
		{"stale-state", func(_ *State, c *Command) { c.ExpectedRevision-- }, ErrStale},
		{"timeout", func(_ *State, c *Command) { c.Attempt.Recipe.Timeout = 0 }, nil},
		{"frozen-config", func(_ *State, c *Command) { c.Attempt.Recipe.ConfigurationRevision = "" }, nil},
		{"scope", func(_ *State, c *Command) { c.Attempt.Recipe.Repository = "unassigned" }, nil},
		{"capacity", func(_ *State, c *Command) { c.Attempt.Reservation.Slots = 2 }, ErrNotReady},
		{"specification", func(_ *State, c *Command) { c.Attempt.SpecificationRevision++ }, ErrStale},
		{"attempt-budget", func(s *State, _ *Command) { o := s.Objectives["o"]; o.Budget.MaxAttempts = 0; s.Objectives["o"] = o }, ErrBudget},
		{"runtime-budget", func(_ *State, c *Command) { c.Attempt.Recipe.Timeout = 2 * time.Hour }, ErrBudget},
		{"unknown-pricing", func(s *State, c *Command) {
			o := s.Objectives["o"]
			o.Budget.MaxCostMicros = 100
			s.Objectives["o"] = o
			c.Attempt.Recipe.PricingRevision = ""
		}, ErrBudget},
		{"paused", func(s *State, _ *Command) { o := s.Objectives["o"]; o.Status = ObjectivePaused; s.Objectives["o"] = o }, ErrNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t)
			c := admitCommand(s, "w", "a")
			tc.prepare(&s, &c)
			before, _ := Clone(s)
			_, err := Apply(s, c)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if !reflect.DeepEqual(before, s) {
				t.Fatal("rejected admission changed state")
			}
		})
	}
}

func TestBudgetAndSharedCapacity(t *testing.T) {
	for _, kind := range []WorkKind{WorkJob, WorkPlanner, WorkCheck, WorkSummary, WorkTicket} {
		t.Run(string(kind), func(t *testing.T) {
			s := testState(t)
			w := WorkItem{ID: "second", ObjectiveID: "o", Kind: kind, Specification: "work"}
			if kind == WorkTicket {
				w.TicketID = "ticket"
			}
			s = applyTest(t, s, Command{Kind: PutWork, Work: w})
			s = applyTest(t, s, admitCommand(s, "w", "a"))
			if _, err := Apply(s, admitCommand(s, "second", "b")); !errors.Is(err, ErrNotReady) {
				t.Fatalf("capacity bypass: %v", err)
			}
			s = stopped(t, s, "w", "a", AttemptFailed)
			s = applyTest(t, s, admitCommand(s, "second", "b"))
			if s.Objectives["o"].Usage.Attempts != 2 {
				t.Fatal("failed attempt was not counted")
			}
		})
	}
	s := testState(t)
	o := s.Objectives["o"]
	o.Budget.MaxCostMicros = 100
	s.Objectives["o"] = o
	s = applyTest(t, s, admitCommand(s, "w", "a"))
	s = applyTest(t, s, Command{Kind: StopAttempt, WorkID: "w", AttemptID: "a", AttemptStatus: AttemptFailed, At: testNow.Add(time.Second), Receipt: "dead", Usage: Usage{UnknownCost: true}})
	if _, err := Apply(s, admitCommand(s, "w", "b")); !errors.Is(err, ErrBudget) {
		t.Fatalf("unknown monetary usage: %v", err)
	}
}

func TestActionRecoveryAndApproval(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          ActionStatus
		receipt, reason string
		approved        bool
		succeeds        bool
	}{
		{"missing-receipt", ActionCompleted, "", "", true, false},
		{"missing-approval", ActionCompleted, "effect receipt", "", false, false},
		{"complete", ActionCompleted, "effect receipt", "", true, true},
		{"ambiguous", ActionParked, "", "cannot establish effect outcome", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t)
			a := Action{ID: "action", ObjectiveID: "o", Kind: "projection", RequiresApproval: true, Preconditions: Preconditions{WorkID: "w", DocumentRevision: 1, SpecificationRevision: 1}}
			s = applyTest(t, s, Command{Kind: RecordAction, Action: a})
			if tc.approved {
				s = applyTest(t, s, Command{Kind: ApproveAction, ActionID: a.ID, Approval: &Approval{OperatorID: "human", Binding: ActionBinding(a), ApprovedAt: testNow}})
			}
			c := Command{Kind: ResolveAction, ExpectedRevision: s.Revision, ActionID: a.ID, ActionStatus: tc.status, Receipt: tc.receipt, Reason: tc.reason}
			n, err := Apply(s, c)
			if (err == nil) != tc.succeeds {
				t.Fatalf("unexpected resolution: %v", err)
			}
			if !tc.succeeds {
				return
			}
			if tc.status == ActionParked && !errors.Is(Ready(n, "w"), ErrNotReady) {
				t.Fatal("ambiguous effect did not stop admission")
			}
			c.ExpectedRevision = n.Revision
			if _, err := Apply(n, c); !errors.Is(err, ErrStale) {
				t.Fatalf("repeated effect resolution: %v", err)
			}
		})
	}
}

func TestAtomicControlBatch(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "abort"}[bad], func(t *testing.T) {
			s := testState(t)
			before, _ := Clone(s)
			commands := []Command{
				{Kind: PutWork, ExpectedRevision: s.Revision, Work: WorkItem{ID: "second", ObjectiveID: "o", Kind: WorkJob, Specification: "new work"}},
				{Kind: RecordAction, ExpectedRevision: s.Revision, Action: Action{ID: "project", ObjectiveID: "o", Kind: "projection", Preconditions: Preconditions{WorkID: "second", DocumentRevision: 1, SpecificationRevision: 1}}},
			}
			if bad {
				commands[1].Action.Preconditions.DocumentRevision = 2
			}
			n, err := ApplyBatch(s, s.Revision, commands)
			if bad {
				if !errors.Is(err, ErrStale) {
					t.Fatalf("invalid batch: %v", err)
				}
			} else if err != nil || n.Revision != s.Revision+1 || n.Work["second"].ID == "" || n.Actions["project"].Status != ActionPending {
				t.Fatalf("atomic batch: %+v %v", n, err)
			}
			if !reflect.DeepEqual(before, s) {
				t.Fatal("batch changed input state")
			}
		})
	}
}

func TestApprovalBeforeEffect(t *testing.T) {
	s := testState(t)
	a := Action{ID: "a", ObjectiveID: "o", Kind: "commit", RequiresApproval: true}
	s = applyTest(t, s, Command{Kind: RecordAction, Action: a})
	for _, tc := range []struct {
		name    string
		binding string
		resolve bool
	}{
		{"wrong-binding", "unrelated-tree", false},
		{"approval-with-receipt", ActionBinding(a), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Command{Kind: ApproveAction, ExpectedRevision: s.Revision, ActionID: "a", Approval: &Approval{OperatorID: "human", Binding: tc.binding, ApprovedAt: testNow}}
			if tc.resolve {
				c.Kind = ResolveAction
				c.ActionStatus = ActionCompleted
				c.Receipt = "effect"
			}
			if _, err := Apply(s, c); err == nil {
				t.Fatal("unbound or late approval accepted")
			}
		})
	}
	approval := &Approval{OperatorID: "human", Binding: ActionBinding(a), ApprovedAt: testNow}
	if _, err := ApplyBatch(s, s.Revision, []Command{{Kind: ApproveAction, ExpectedRevision: s.Revision, ActionID: "a", Approval: approval}, {Kind: ResolveAction, ExpectedRevision: s.Revision, ActionID: "a", ActionStatus: ActionCompleted, Receipt: "effect"}}); err == nil {
		t.Fatal("effect and its prior approval shared a transaction")
	}
}

func TestDetachedCommandPayload(t *testing.T) {
	s := testState(t)
	work := WorkItem{ID: "other", ObjectiveID: "o", Kind: WorkJob, Specification: "work", Dependencies: []string{"w"}}
	n := applyTest(t, s, Command{Kind: PutWork, Work: work})
	work.Dependencies[0] = "corrupted"
	if n.Work["other"].Dependencies[0] != "w" {
		t.Fatal("command payload aliases returned state")
	}
}
