package orchestrator

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDomainRecordRoundTrip(t *testing.T) {
	for _, value := range []any{
		Objective{ID: "o", DocumentRevision: 7, CriteriaRevision: 3, CurrentPlan: 2},
		Plan{ID: "p", ReplacedWorkIDs: []string{"old"}, ReplacementWorkIDs: map[string]string{"old": "new"}},
		WorkItem{ID: "w", DocumentRevision: 9, SpecificationRevision: 4, ActiveAttemptID: "attempt-unique", LastAttemptID: "attempt-unique", AcceptanceRequired: true},
		AcceptanceBinding{Obligations: []ResultRef{{ResultID: "r", WorkID: "feedback", Generation: 1}}},
		Attempt{ID: "attempt-unique", WorkID: "w", SpecificationRevision: 4},
		Result{ID: "r", WorkID: "w", AttemptID: "attempt-unique", Generation: 2, SpecificationRevision: 4},
		Question{ID: "q", AttemptID: "attempt-unique", SpecificationRevision: 4},
		Acceptance{ID: "accept", CriteriaRevision: 3, PlanRevision: 2, Results: []ResultRef{{ResultID: "r", WorkID: "w", Generation: 2}}, Binding: &AcceptanceBinding{CriteriaRevision: 3, PlanRevision: 2, VerificationPolicyID: "checks"}, ReviewID: "review"},
		CheckedOutput{Disposition: OutputResearch, VerificationPolicyID: "checks", Outputs: []QualifiedOutput{{Artifact: evidenceArtifact()}}, Sources: []Citation{{Source: "source", Evidence: evidenceArtifact()}}, Review: &OutputReview{ReviewerID: "reviewer", Evidence: evidenceArtifact()}},
		VerificationPolicy{ID: "checks", ObjectiveID: "o", Revision: "v1", Definitions: []CheckDefinition{{ID: "test", Command: []string{"go", "test"}}}, OperatorID: "operator", RecordedAt: testNow},
		VerificationExecution{VerificationPolicyID: "checks", DefinitionID: "test", Command: []string{"go", "test"}, Outputs: []QualifiedOutput{{Repository: "repo", Commit: outputHead}}},
		CombinedReview{ID: "review", ObjectiveID: "o", ReviewerID: "reviewer", Evidence: evidenceArtifact(), AttemptIDs: []string{"producer", "check"}, Binding: AcceptanceBinding{Current: []QualifiedResult{{Result: ResultRef{ResultID: "r", WorkID: "w", Generation: 1}, Outputs: []QualifiedOutput{{Repository: "repo", Commit: outputHead}}, Ranges: []ReviewRange{{Repository: "repo", BaseCommit: outputBase, HeadCommit: outputHead}}}}}},
	} {
		t.Run(reflect.TypeOf(value).Name(), func(t *testing.T) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			out := reflect.New(reflect.TypeOf(value))
			if err = json.Unmarshal(data, out.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(value, out.Elem().Interface()) {
				t.Fatalf("record identities changed: %s", data)
			}
		})
	}
}

func TestLegacyPlanEncoding(t *testing.T) {
	const legacy = `{"id":"p","objective_id":"o","revision":1,"rationale":"replace","work_ids":[],"required_results":[],"replaced_work_ids":["old"]}`
	var p Plan
	if err := json.Unmarshal([]byte(legacy), &p); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != legacy {
		t.Fatalf("legacy plan encoding changed: %s", data)
	}
}

func TestAttemptZeroStoppedAtEncoding(t *testing.T) {
	// Keep zero timestamps explicit: existing journal checksums include this field.
	data, err := json.Marshal(Attempt{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if got := string(fields["stopped_at"]); got != `"0001-01-01T00:00:00Z"` {
		t.Fatalf("zero stopped_at encoding changed: %s", got)
	}
}

func TestStateClone(t *testing.T) {
	s := testState(t)
	n, err := Clone(s)
	if err != nil {
		t.Fatal(err)
	}
	o := n.Objectives["o"]
	o.Criteria[0] = "changed"
	o.Policy.Repositories[0] = "changed"
	n.Objectives[o.ID] = o
	delete(n.Work, "w")
	if s.Objectives["o"].Criteria[0] != "checked" || s.Objectives["o"].Policy.Repositories[0] != "repo" || s.Work["w"].ID == "" {
		t.Fatal("cloned state aliases original state")
	}
}
