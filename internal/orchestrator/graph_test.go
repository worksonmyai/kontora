package orchestrator

import (
	"errors"
	"testing"
)

func TestGraphValidation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		dependencies []string
		cycle        bool
		missing      bool
		valid        bool
	}{
		{"ordered", []string{"w"}, false, false, true},
		{"missing", []string{"absent"}, false, false, false},
		{"duplicate", []string{"w", "w"}, false, false, false},
		{"self-cycle", []string{"second"}, false, false, false},
		{"two-node-cycle", []string{"w"}, true, false, false},
		{"missing-input", nil, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t)
			w := s.Work["w"]
			other := w
			other.ID = "second"
			other.Dependencies = tc.dependencies
			if tc.missing {
				other.Inputs = []ResultRef{{ResultID: "missing", WorkID: "w", Generation: 1}}
			}
			s.Work[other.ID] = other
			if tc.cycle {
				w.Dependencies = []string{"second"}
				s.Work[w.ID] = w
			}
			if err := ValidateGraph(s); (err == nil) != tc.valid {
				t.Fatalf("graph validation: %v", err)
			}
		})
	}
}

func TestResultInvalidationAndLiveConsumers(t *testing.T) {
	s := withResult(t, testState(t), "w", "a", "r1")
	// Result consumption can cross objective and repository boundaries.
	s = applyTest(t, s, Command{Kind: CreateObjective, Objective: Objective{ID: "o2", Assignment: "consume", Criteria: []string{"checked"}, Policy: s.Objectives["o"].Policy, Budget: s.Objectives["o"].Budget}})
	s = applyTest(t, s, Command{Kind: SetObjectiveStatus, ObjectiveID: "o2", Status: ObjectiveActive})
	s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "consumer", ObjectiveID: "o2", Kind: WorkJob, Specification: "consume", Inputs: []ResultRef{{ResultID: "r1", WorkID: "w", Generation: 1}}}})
	s = withResult(t, s, "consumer", "b", "r2")
	s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "downstream", ObjectiveID: "o2", Kind: WorkCheck, Specification: "check", Inputs: []ResultRef{{ResultID: "r2", WorkID: "consumer", Generation: 1}}}})
	s = applyTest(t, s, admitCommand(s, "downstream", "c"))
	change := Command{Kind: ReviseWork, ExpectedRevision: s.Revision, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "new spec"}}
	if _, err := Apply(s, change); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed inputs under live consumer: %v", err)
	}
	s = stopped(t, s, "downstream", "c", AttemptCancelled)
	s = applyTest(t, s, change)
	if s.Results["r1"].Valid || s.Results["r2"].Valid {
		t.Fatal("input invalidation did not propagate")
	}
	if err := Ready(s, "consumer"); err == nil {
		t.Fatal("consumer ready with stale pinned input")
	}
	if err := Ready(s, "downstream"); err == nil {
		t.Fatal("transitive consumer ready with stale input")
	}
}

func TestPlanReplacement(t *testing.T) {
	for _, tc := range []struct {
		name           string
		work, replaced []string
		good           bool
	}{
		{"retain", []string{"w"}, nil, true},
		{"silent-drop", nil, nil, false},
		{"explicit-replace", nil, []string{"w"}, true},
		{"keep-and-replace", []string{"w"}, []string{"w"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t)
			s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p1", ObjectiveID: "o", Revision: 1, WorkIDs: []string{"w"}, RequiredResults: []string{"w"}}})
			_, err := Apply(s, Command{Kind: PutPlan, ExpectedRevision: s.Revision, Plan: Plan{ID: "p2", ObjectiveID: "o", Revision: 2, WorkIDs: tc.work, RequiredResults: tc.work, ReplacedWorkIDs: tc.replaced}})
			if (err == nil) != tc.good {
				t.Fatalf("plan revision: %v", err)
			}
		})
	}
}

func TestRetainedHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*State)
	}{
		{"delete-attempt", func(s *State) {
			delete(s.Attempts, "a")
			w := s.Work["w"]
			w.LastAttemptID = ""
			s.Work["w"] = w
			o := s.Objectives["o"]
			o.Usage = Usage{}
			s.Objectives["o"] = o
		}},
		{"rewrite-recipe", func(s *State) { a := s.Attempts["a"]; a.Recipe.Profile = "other"; s.Attempts[a.ID] = a }},
		{"alter-budget", func(s *State) { o := s.Objectives["o"]; o.Budget.MaxAttempts++; s.Objectives[o.ID] = o }},
		{"change-output", func(s *State) { r := s.Results["r"]; r.Commit = "other"; s.Results[r.ID] = r }},
		{"delete-ownership", func(s *State) { delete(s.Work, "w") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := withResult(t, testState(t), "w", "a", "r")
			n, _ := Clone(s)
			n.Revision++
			tc.mutate(&n)
			if err := ValidateTransition(s, n); err == nil {
				t.Fatal("historical identity mutation accepted")
			}
		})
	}
}

func TestArtifactReferences(t *testing.T) {
	s := testState(t)
	a := ArtifactRef{Hash: "a", Size: 10}
	b := ArtifactRef{Hash: "b", Size: 20}
	s.Attempts["test"] = Attempt{Recipe: Recipe{Artifacts: []ArtifactRef{a}}, Logs: []ArtifactRef{b, a}}
	s.Results["test"] = Result{Artifacts: []ArtifactRef{a}, Checks: []Check{{Log: b}}}
	s.Actions["test"] = Action{Payload: a}
	refs := ReferencedArtifacts(s)
	if len(refs) != 2 || refs[0] != a || refs[1] != b {
		t.Fatalf("artifact references: %+v", refs)
	}
}
