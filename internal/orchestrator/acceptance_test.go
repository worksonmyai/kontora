package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func acceptanceState(t *testing.T) State {
	t.Helper()
	s, r := prepareCheckedResult(t)
	s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
	return applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "plan-1", ObjectiveID: "o", Revision: 1, WorkIDs: []string{"w"}, RequiredResults: []string{"w"}}})
}

func reviewCommand(t *testing.T, s State, id string) Command {
	t.Helper()
	binding, err := CurrentAcceptanceBinding(s, "o")
	require.NoError(t, err)
	return Command{Kind: RecordCombinedReview, ExpectedRevision: s.Revision, OperatorID: "reviewer-from-transport", At: testNow,
		Review: CombinedReview{ID: id, ObjectiveID: "o", Binding: binding, Evidence: evidenceArtifact()},
	}
}

func acceptCommand(t *testing.T, s State) Command {
	t.Helper()
	binding, err := CurrentAcceptanceBinding(s, "o")
	require.NoError(t, err)
	return Command{Kind: RecordAcceptance, ExpectedRevision: s.Revision, OperatorID: "operator-from-transport", At: testNow,
		Acceptance: Acceptance{ID: "accept", ObjectiveID: "o", Binding: &binding, ReviewID: "combined"},
	}
}

func TestAcceptanceFeedbackObligations(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		required, planned, checked bool
		ready                      bool
	}{
		{"optional outside plan", false, false, false, true},
		{"optional pending in plan", false, true, false, true},
		{"feedback outside plan", true, false, false, false},
		{"feedback pending in plan", true, true, false, false},
		{"checked feedback outside plan", true, false, true, false},
		{"checked feedback in plan", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := acceptanceState(t)
			s = applyTest(t, s, reviewCommand(t, s, "combined"))
			oldAcceptance := acceptCommand(t, s)
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "feedback/request", ObjectiveID: "o", Kind: WorkJob, Specification: "operator correction", AcceptanceRequired: tc.required}})
			if tc.checked {
				s = applyTest(t, s, admitCommand(s, "feedback/request", "feedback-attempt"))
				s = stopped(t, s, "feedback/request", "feedback-attempt", AttemptSucceeded)
				var r Result
				s, r = verifyCandidate(t, s, checkedCandidate(s, "feedback/request", "feedback-attempt", "feedback-result", 1))
				s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
			}
			if tc.planned {
				s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "feedback-plan", ObjectiveID: "o", Revision: 2, WorkIDs: []string{"w", "feedback/request"}, RequiredResults: []string{"w"}}})
			}
			binding, err := CurrentAcceptanceBinding(s, "o")
			if !tc.ready {
				require.Error(t, err)
				require.Error(t, AcceptanceReady(s, "o", "combined"))
				oldAcceptance.ExpectedRevision = s.Revision
				_, err = Apply(s, oldAcceptance)
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.required {
				require.Equal(t, []ResultRef{{ResultID: "feedback-result", WorkID: "feedback/request", Generation: 1}}, binding.Obligations)
			} else {
				require.Empty(t, binding.Obligations)
			}
			if tc.planned || tc.required {
				require.Error(t, AcceptanceReady(s, "o", "combined"))
			}
			s = applyTest(t, s, reviewCommand(t, s, "new-clean"))
			c := acceptCommand(t, s)
			c.Acceptance.ReviewID = "new-clean"
			s = applyTest(t, s, c)
			require.NoError(t, AcceptanceCurrent(s, "accept"))
		})
	}
}

func TestAcceptanceObligationDurability(t *testing.T) {
	s := acceptanceState(t)
	s = applyTest(t, s, reviewCommand(t, s, "combined"))
	s = applyTest(t, s, acceptCommand(t, s))
	s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "feedback", ObjectiveID: "o", Kind: WorkJob, Specification: "correction", AcceptanceRequired: true}})
	require.ErrorIs(t, AcceptanceCurrent(s, "accept"), ErrStale)
	for _, workID := range []string{"feedback", "w"} {
		n, err := Clone(s)
		require.NoError(t, err)
		n.Revision++
		w := n.Work[workID]
		w.AcceptanceRequired = !w.AcceptanceRequired
		w.DocumentRevision++
		n.Work[workID] = w
		require.Error(t, ValidateTransition(s, n))
	}
}

func TestReplacedFeedbackObligation(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "explicit successor"}[bound], func(t *testing.T) {
			s := acceptanceState(t)
			s = applyTest(t, s, reviewCommand(t, s, "old-clean"))
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "feedback", ObjectiveID: "o", Kind: WorkJob, Specification: "correction", AcceptanceRequired: true}})
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "successor", ObjectiveID: "o", Kind: WorkJob, Specification: "replacement correction"}})
			p := Plan{ID: "p2", ObjectiveID: "o", Revision: 2, WorkIDs: []string{"w", "successor"}, RequiredResults: []string{"w"}, ReplacedWorkIDs: []string{"feedback"}}
			if bound {
				p.ReplacementWorkIDs = map[string]string{"feedback": "successor"}
			}
			s = applyTest(t, s, Command{Kind: PutPlan, Plan: p})
			s = applyTest(t, s, admitCommand(s, "successor", "fix-attempt"))
			s = stopped(t, s, "successor", "fix-attempt", AttemptSucceeded)
			s, fix := verifyCandidate(t, s, checkedCandidate(s, "successor", "fix-attempt", "fix", 1))
			s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: fix})
			b, err := CurrentAcceptanceBinding(s, "o")
			if !bound {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []ResultRef{{ResultID: "fix", WorkID: "successor", Generation: 1}}, b.Obligations)
			require.Error(t, AcceptanceReady(s, "o", "old-clean"))
			s = applyTest(t, s, reviewCommand(t, s, "combined"))
			s = applyTest(t, s, acceptCommand(t, s))
			require.NoError(t, AcceptanceCurrent(s, "accept"))
		})
	}
}

func TestReviewRepairExecutionFreshness(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		producer, verification, preFinding bool
		valid                              bool
	}{
		{"copied identity", false, false, false, false},
		{"producer only", true, false, false, false},
		{"verification only", false, true, false, false},
		{"fresh repair and checks", true, true, false, true},
		{"previously admitted unbound evidence", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := acceptanceState(t)
			var r Result
			produce := func() {
				r = s.Results["result"]
				r.ID, r.Generation = "repair-result", 2
				if tc.producer {
					s = applyTest(t, s, admitCommand(s, "w", "repair-producer"))
					s = stopped(t, s, "w", "repair-producer", AttemptSucceeded)
					r.AttemptID = "repair-producer"
				}
				if tc.verification {
					s, r = verifyCandidate(t, s, r)
				}
			}
			if tc.preFinding {
				produce()
			}
			c := reviewCommand(t, s, "findings")
			c.Review.Findings = []ReviewFinding{{Message: "repair required output", RepairWorkID: "w"}}
			s = applyTest(t, s, c)
			snapshot := s.Reviews["findings"]
			if !tc.preFinding {
				produce()
			}
			s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
			_, err := Apply(s, reviewCommand(t, s, "combined"))
			if !tc.valid {
				require.ErrorContains(t, err, "fresh checked repair result")
				require.Error(t, AcceptanceReady(s, "o", "findings"))
				return
			}
			require.NoError(t, err)
			s = applyTest(t, s, reviewCommand(t, s, "combined"))
			s = applyTest(t, s, acceptCommand(t, s))
			require.NoError(t, AcceptanceCurrent(s, "accept"))
			require.Equal(t, snapshot, s.Reviews["findings"])
		})
	}
}

func TestReviewAdmissionSnapshot(t *testing.T) {
	s := acceptanceState(t)
	c := reviewCommand(t, s, "findings")
	c.Review.Findings = []ReviewFinding{{Message: "fix", RepairWorkID: "w"}}
	n := applyTest(t, s, c)
	require.Len(t, n.Reviews["findings"].AttemptIDs, 2)
	for _, ids := range [][]string{nil, {"attempt"}, {"missing"}} {
		forged, err := Clone(n)
		require.NoError(t, err)
		r := forged.Reviews["findings"]
		r.AttemptIDs = ids
		forged.Reviews[r.ID] = r
		require.Error(t, ValidateTransition(s, forged))
	}
	c.Review.AttemptIDs = []string{"attempt"}
	_, err := Apply(s, c)
	require.Error(t, err)
}

func TestReplacedRepairResolution(t *testing.T) {
	for _, mode := range []string{"unbound", "explicit", "late binding", "chain"} {
		t.Run(mode, func(t *testing.T) {
			s := acceptanceState(t)
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "repair", ObjectiveID: "o", Kind: WorkJob, Specification: "repair"}})
			s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p2", ObjectiveID: "o", Revision: 2, WorkIDs: []string{"w", "repair"}, RequiredResults: []string{"w"}}})
			c := reviewCommand(t, s, "findings")
			c.Review.Findings = []ReviewFinding{{Message: "broken", RepairWorkID: "repair"}}
			s = applyTest(t, s, c)
			prior := s.Reviews["findings"]
			target := "successor"
			if mode == "chain" {
				target = "intermediate"
			}
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: target, ObjectiveID: "o", Kind: WorkJob, Specification: "replacement"}})
			p := Plan{ID: "p3", ObjectiveID: "o", Revision: 3, WorkIDs: []string{"w", target}, RequiredResults: []string{"w"}, ReplacedWorkIDs: []string{"repair"}}
			if mode == "explicit" || mode == "chain" {
				p.ReplacementWorkIDs = map[string]string{"repair": target}
			}
			s = applyTest(t, s, Command{Kind: PutPlan, Plan: p})
			if mode == "chain" {
				s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "successor", ObjectiveID: "o", Kind: WorkJob, Specification: "second replacement"}})
				s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p4", ObjectiveID: "o", Revision: 4, WorkIDs: []string{"w", "successor"}, RequiredResults: []string{"w"}, ReplacedWorkIDs: []string{"intermediate"}, ReplacementWorkIDs: map[string]string{"intermediate": "successor"}}})
			}
			s = applyTest(t, s, admitCommand(s, "successor", "fix-attempt"))
			s = stopped(t, s, "successor", "fix-attempt", AttemptSucceeded)
			s, fix := verifyCandidate(t, s, checkedCandidate(s, "successor", "fix-attempt", "fix", 1))
			s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: fix})
			s = applyTest(t, s, Command{Kind: ReviseWork, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "include successor", Inputs: []ResultRef{{ResultID: fix.ID, WorkID: fix.WorkID, Generation: fix.Generation}}}})
			s = applyTest(t, s, admitCommand(s, "w", "required-attempt"))
			s = stopped(t, s, "w", "required-attempt", AttemptSucceeded)
			s, result := verifyCandidate(t, s, checkedCandidate(s, "w", "required-attempt", "repaired", 2))
			s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: result})
			if mode == "late binding" {
				_, err := Apply(s, reviewCommand(t, s, "before-binding"))
				require.ErrorContains(t, err, "fresh checked repair result")
				s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p4", ObjectiveID: "o", Revision: 4, WorkIDs: []string{"w", "successor"}, RequiredResults: []string{"w"}, ReplacementWorkIDs: map[string]string{"repair": "successor"}}})
			}
			if mode == "unbound" {
				_, err := Apply(s, reviewCommand(t, s, "combined"))
				require.ErrorContains(t, err, "fresh checked repair result")
				return
			}
			s = applyTest(t, s, reviewCommand(t, s, "combined"))
			s = applyTest(t, s, acceptCommand(t, s))
			require.NoError(t, AcceptanceCurrent(s, "accept"))
			require.Equal(t, prior, s.Reviews["findings"])
			require.ErrorIs(t, Ready(s, "repair"), ErrNotReady)
		})
	}
}

func TestRequiredRecheckExecutionFreshness(t *testing.T) {
	for _, mode := range []string{"unchanged", "copied", "producer only", "verification only", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			s := acceptanceState(t)
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "repair", ObjectiveID: "o", Kind: WorkJob, Specification: "repair"}})
			s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p2", ObjectiveID: "o", Revision: 2, WorkIDs: []string{"w", "repair"}, RequiredResults: []string{"w"}}})
			c := reviewCommand(t, s, "findings")
			c.Review.Findings = []ReviewFinding{{Message: "fix", RepairWorkID: "repair"}}
			s = applyTest(t, s, c)
			s = applyTest(t, s, admitCommand(s, "repair", "fix-attempt"))
			s = stopped(t, s, "repair", "fix-attempt", AttemptSucceeded)
			s, fix := verifyCandidate(t, s, checkedCandidate(s, "repair", "fix-attempt", "fix", 1))
			s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: fix})
			if mode != "unchanged" {
				r := s.Results["result"]
				r.ID, r.Generation = "rechecked", 2
				if mode == "producer only" || mode == "fresh" {
					s = applyTest(t, s, admitCommand(s, "w", "new-producer"))
					s = stopped(t, s, "w", "new-producer", AttemptSucceeded)
					r.AttemptID = "new-producer"
				}
				if mode == "verification only" || mode == "fresh" {
					s, r = verifyCandidate(t, s, r)
				}
				s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
			}
			_, err := Apply(s, reviewCommand(t, s, "combined"))
			if mode == "fresh" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "fresh checks of the required results")
			}
		})
	}
}

func TestAcceptanceCombinedRepositoryOutputs(t *testing.T) {
	for _, tc := range []struct {
		name, repository, head, aggregate string
		valid                             bool
	}{
		{"independent differing heads", "repo", strings.Repeat("d", 40), "", false},
		{"identical heads", "repo", outputHead, "", true},
		{"cross repository", "second-repo", strings.Repeat("d", 40), "", true},
		{"missing one input", "repo", strings.Repeat("d", 40), "partial", false},
		{"research is not integrated code", "repo", strings.Repeat("d", 40), "research", false},
		{"checked integrated output", "repo", strings.Repeat("d", 40), "direct", true},
		{"transitive integrated output", "repo", strings.Repeat("d", 40), "transitive", true},
		{"explicit no change after integration", "repo", strings.Repeat("d", 40), "no change", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t)
			o := s.Objectives["o"]
			o.Policy.Repositories = []string{"repo", "second-repo"}
			s.Objectives[o.ID] = o
			s = withVerificationPolicy(t, s, "policy")
			produce := func(workID, repository, head string, inputs []ResultRef, disposition OutputDisposition) ResultRef {
				if workID != "w" {
					s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: workID, ObjectiveID: "o", Kind: WorkJob, Specification: workID, Inputs: inputs}})
				}
				a := admitCommand(s, workID, workID+"-attempt")
				a.Attempt.Recipe.Repository = repository
				s = applyTest(t, s, a)
				s = stopped(t, s, workID, a.Attempt.ID, AttemptSucceeded)
				r := checkedCandidate(s, workID, a.Attempt.ID, workID+"-result", 1)
				r.Checked.Outputs[0] = QualifiedOutput{Repository: repository, Commit: head}
				r.Commit = head
				r.Checked.Ranges[0] = ReviewRange{Repository: repository, BaseCommit: outputBase, HeadCommit: head}
				if disposition == OutputResearch {
					s, r = researchCandidate(t, s, r)
				} else {
					if disposition == OutputNoChange {
						r.Checked.Disposition, r.Checked.Reason, r.Checked.Evidence = OutputNoChange, "combination already present", evidenceArtifact()
						r.Checked.Ranges[0].BaseCommit = head
					}
					s, r = verifyCandidate(t, s, r)
				}
				s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
				return ResultRef{ResultID: r.ID, WorkID: workID, Generation: 1}
			}
			left := produce("w", "repo", outputHead, nil, OutputCode)
			right := produce("other", tc.repository, tc.head, nil, OutputCode)
			workIDs := []string{"w", "other"}
			required := []string{"w", "other"}
			if tc.aggregate != "" {
				inputs := []ResultRef{left, right}
				if tc.aggregate == "partial" {
					inputs = []ResultRef{left}
				}
				if tc.aggregate == "transitive" {
					bridge := produce("bridge", "repo", strings.Repeat("e", 40), inputs, OutputCode)
					inputs = []ResultRef{bridge}
					workIDs = append(workIDs, "bridge")
				}
				disposition := OutputCode
				switch tc.aggregate {
				case "research":
					disposition = OutputResearch
				case "no change":
					disposition = OutputNoChange
				}
				produce("aggregate", "repo", strings.Repeat("f", 40), inputs, disposition)
				workIDs = append(workIDs, "aggregate")
				required = []string{"aggregate"}
			}
			s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p", ObjectiveID: "o", Revision: 1, WorkIDs: workIDs, RequiredResults: required}})
			_, err := CurrentAcceptanceBinding(s, "o")
			if !tc.valid {
				require.ErrorContains(t, err, "require a checked combined output")
				_, err = Apply(s, Command{Kind: RecordCombinedReview, ExpectedRevision: s.Revision, OperatorID: "operator", At: testNow, Review: CombinedReview{ID: "combined", ObjectiveID: "o", Evidence: evidenceArtifact()}})
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			s = applyTest(t, s, reviewCommand(t, s, "combined"))
			s = applyTest(t, s, acceptCommand(t, s))
			require.NoError(t, AcceptanceCurrent(s, "accept"))
		})
	}
}

func TestCheckedAcceptanceBindings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*State, *Command)
		valid  bool
	}{
		{"exact", func(*State, *Command) {}, true},
		{"untrusted caller", func(_ *State, c *Command) { c.OperatorID = "" }, false},
		{"self-declared operator", func(_ *State, c *Command) { c.Acceptance.OperatorID = "worker" }, false},
		{"missing criteria binding", func(_ *State, c *Command) { c.Acceptance.Binding = nil }, false},
		{"stale criteria", func(_ *State, c *Command) { c.Acceptance.Binding.CriteriaRevision++ }, false},
		{"stale plan", func(_ *State, c *Command) { c.Acceptance.Binding.PlanRevision++ }, false},
		{"stale checks", func(_ *State, c *Command) { c.Acceptance.Binding.VerificationPolicyID = "old-checks" }, false},
		{"stale result", func(_ *State, c *Command) { c.Acceptance.Binding.RequiredResults[0].Generation++ }, false},
		{"wrong required work", func(_ *State, c *Command) { c.Acceptance.Binding.RequiredResults[0].WorkID = "other" }, false},
		{"missing current vector", func(_ *State, c *Command) { c.Acceptance.Binding.Current = nil }, false},
		{"unqualified current vector", func(_ *State, c *Command) { c.Acceptance.Binding.Current[0].Outputs[0].Repository = "" }, false},
		{"moving review range", func(_ *State, c *Command) { c.Acceptance.Binding.Current[0].Ranges[0].BaseCommit = "delivery" }, false},
		{"legacy projection differs", func(_ *State, c *Command) { c.Acceptance.CriteriaRevision = 9 }, false},
		{"missing combined review", func(_ *State, c *Command) { c.Acceptance.ReviewID = "absent" }, false},
		{"paused objective", func(s *State, _ *Command) {
			*s = applyTest(t, *s, Command{Kind: SetObjectiveStatus, ObjectiveID: "o", Status: ObjectivePaused})
		}, false},
		{"cancelled objective", func(s *State, _ *Command) {
			*s = applyTest(t, *s, Command{Kind: SetObjectiveStatus, ObjectiveID: "o", Status: ObjectiveCancelled, Reason: "cancel"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := acceptanceState(t)
			require.Error(t, AcceptanceReady(s, "o", "combined"))
			s = applyTest(t, s, reviewCommand(t, s, "combined"))
			require.NoError(t, AcceptanceReady(s, "o", "combined"))
			c := acceptCommand(t, s)
			tc.mutate(&s, &c)
			c.ExpectedRevision = s.Revision
			before, err := Clone(s)
			require.NoError(t, err)
			n, err := Apply(s, c)
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, ObjectiveAccepted, n.Objectives["o"].Status)
				require.Equal(t, c.OperatorID, n.Acceptances["accept"].OperatorID)
				require.Equal(t, "reviewer-from-transport", n.Reviews["combined"].ReviewerID)
				c.ExpectedRevision = n.Revision
				_, err = Apply(n, c)
				require.Error(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, before, s)
		})
	}
}

func TestAcceptanceStalesAfterChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, State) State
	}{
		{"plan", func(t *testing.T, s State) State {
			return applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "plan-2", ObjectiveID: "o", Revision: 2, WorkIDs: []string{"w"}, RequiredResults: []string{"w"}}})
		}},
		{"check definitions", func(t *testing.T, s State) State { return withVerificationPolicy(t, s, "new-policy") }},
		{"work inputs", func(t *testing.T, s State) State {
			return applyTest(t, s, Command{Kind: ReviseWork, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "new spec"}})
		}},
		{"new result generation", func(t *testing.T, s State) State {
			s = applyTest(t, s, admitCommand(s, "w", "next"))
			s = stopped(t, s, "w", "next", AttemptSucceeded)
			r := checkedCandidate(s, "w", "next", "result-2", 2)
			r.Checked.Outputs[0].Commit, r.Commit, r.Checked.Ranges[0].HeadCommit = outputBase, outputBase, outputBase
			r.Checked.Ranges[0].BaseCommit = outputHead
			s, r = verifyCandidate(t, s, r)
			return applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
		}},
		{"running work", func(t *testing.T, s State) State { return applyTest(t, s, admitCommand(s, "w", "next")) }},
		{"pending effect", func(t *testing.T, s State) State {
			return applyTest(t, s, Command{Kind: RecordAction, Action: Action{ID: "effect", ObjectiveID: "o", Kind: "integration"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := acceptanceState(t)
			s = applyTest(t, s, reviewCommand(t, s, "combined"))
			c := acceptCommand(t, s)
			prior := s.Reviews["combined"]
			s = tc.change(t, s)
			require.Equal(t, prior, s.Reviews["combined"])
			require.Error(t, AcceptanceReady(s, "o", "combined"))
			c.ExpectedRevision = s.Revision
			_, err := Apply(s, c)
			require.Error(t, err)
		})
	}
}

func TestAcceptanceLegacyAndReviewedDispositions(t *testing.T) {
	for _, disposition := range []OutputDisposition{"legacy", OutputCode, OutputResearch, OutputNoChange} {
		t.Run(string(disposition), func(t *testing.T) {
			s, r := prepareCheckedResult(t)
			switch disposition {
			case OutputCode:
			case OutputResearch:
				s, r = researchCandidate(t, s, r)
			case OutputNoChange:
				r.Checked.Disposition, r.Checked.Reason, r.Checked.Evidence = OutputNoChange, "existing commit satisfies the assignment", evidenceArtifact()
				r.Checked.Ranges[0].BaseCommit = outputHead
			case "legacy":
				r.Checked, r.Checks = nil, nil
			}
			kind := RecordCheckedResult
			if disposition == "legacy" {
				kind = RecordResult
			}
			s = applyTest(t, s, Command{Kind: kind, Result: r})
			s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p", ObjectiveID: "o", Revision: 1, WorkIDs: []string{"w"}, RequiredResults: []string{"w"}}})
			if disposition == "legacy" {
				_, err := CurrentAcceptanceBinding(s, "o")
				require.Error(t, err)
				return
			}
			s = applyTest(t, s, reviewCommand(t, s, "combined"))
			s = applyTest(t, s, acceptCommand(t, s))
			require.Equal(t, ObjectiveAccepted, s.Objectives["o"].Status)
		})
	}
}

func TestCombinedReviewRepairAndHistory(t *testing.T) {
	s := acceptanceState(t)
	s = applyTest(t, s, reviewCommand(t, s, "initial-clean"))
	s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "repair", ObjectiveID: "o", Kind: WorkJob, Specification: "fix review finding"}})
	c := reviewCommand(t, s, "findings")
	c.Review.Findings = []ReviewFinding{{Message: "broken behavior", RepairWorkID: "missing"}}
	_, err := Apply(s, c)
	require.Error(t, err)
	c.Review.Findings[0].RepairWorkID = "repair"
	s = applyTest(t, s, c)
	require.Error(t, AcceptanceReady(s, "o", "findings"))
	require.Error(t, AcceptanceReady(s, "o", "initial-clean"), "later findings supersede clean readiness without rewriting the prior review")
	_, err = Apply(s, reviewCommand(t, s, "combined"))
	require.Error(t, err, "cannot dismiss findings without fresh checked results")
	before := s.Reviews["findings"]
	s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "repair-plan", ObjectiveID: "o", Revision: 2, WorkIDs: []string{"w", "repair"}, RequiredResults: []string{"w"}}})
	_, err = Apply(s, reviewCommand(t, s, "plan-only"))
	require.Error(t, err)
	s = applyTest(t, s, admitCommand(s, "repair", "fix-attempt"))
	s = stopped(t, s, "repair", "fix-attempt", AttemptSucceeded)
	s, fix := verifyCandidate(t, s, checkedCandidate(s, "repair", "fix-attempt", "fix", 1))
	s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: fix})
	_, err = Apply(s, reviewCommand(t, s, "repair-without-recheck"))
	require.Error(t, err)
	s = applyTest(t, s, Command{Kind: ReviseWork, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "include checked repair", Inputs: []ResultRef{{ResultID: fix.ID, WorkID: fix.WorkID, Generation: fix.Generation}}}})
	s = applyTest(t, s, admitCommand(s, "w", "repair-attempt"))
	s = stopped(t, s, "w", "repair-attempt", AttemptSucceeded)
	s, repaired := verifyCandidate(t, s, checkedCandidate(s, "w", "repair-attempt", "repaired", 2))
	s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: repaired})
	s = applyTest(t, s, reviewCommand(t, s, "combined"))
	require.Equal(t, before, s.Reviews["findings"])
	s = applyTest(t, s, acceptCommand(t, s))
	for _, mutate := range []func(*State){
		func(n *State) { delete(n.Reviews, "findings") },
		func(n *State) { a := n.Acceptances["accept"]; a.OperatorID = "other"; n.Acceptances[a.ID] = a },
		func(n *State) { a := n.Results["repaired"]; a.Checks[0].Directory = "/other"; n.Results[a.ID] = a },
	} {
		n, err := Clone(s)
		require.NoError(t, err)
		n.Revision++
		mutate(&n)
		require.Error(t, ValidateTransition(s, n))
	}
}

func TestCrossRepositoryOutputInvalidation(t *testing.T) {
	s := testState(t)
	// Scope is fixed at assignment; no operation below expands it.
	o := s.Objectives["o"]
	o.Policy.Repositories = []string{"repo", "second-repo"}
	s.Objectives[o.ID] = o
	s = withVerificationPolicy(t, s, "policy")
	s = applyTest(t, s, admitCommand(s, "w", "produce"))
	s = stopped(t, s, "w", "produce", AttemptSucceeded)
	s, producer := verifyCandidate(t, s, checkedCandidate(s, "w", "produce", "producer", 1))
	s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: producer})
	s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "consumer", ObjectiveID: "o", Kind: WorkJob, Specification: "cross repo", Inputs: []ResultRef{{ResultID: "producer", WorkID: "w", Generation: 1}}}})
	c := admitCommand(s, "consumer", "consume")
	c.Attempt.Recipe.Repository = "second-repo"
	s = applyTest(t, s, c)
	s = stopped(t, s, "consumer", "consume", AttemptSucceeded)
	r := checkedCandidate(s, "consumer", "consume", "downstream", 1)
	r.Checked.Outputs[0].Repository, r.Checked.Ranges[0].Repository = "second-repo", "second-repo"
	s, r = verifyCandidate(t, s, r)
	s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
	s = applyTest(t, s, Command{Kind: PutPlan, Plan: Plan{ID: "p", ObjectiveID: "o", Revision: 1, WorkIDs: []string{"w", "consumer"}, RequiredResults: []string{"consumer"}}})
	s = applyTest(t, s, reviewCommand(t, s, "combined"))
	binding, err := CurrentAcceptanceBinding(s, "o")
	require.NoError(t, err)
	require.Len(t, binding.Current, 2)
	// The complete vector includes a non-required producer and keeps repository identity.
	require.Equal(t, "second-repo", binding.Current[0].Outputs[0].Repository)
	require.Equal(t, "repo", binding.Current[1].Outputs[0].Repository)
	binding.Current[0].Outputs[0].Repository = "tampered"
	require.Equal(t, "second-repo", s.Results["downstream"].Checked.Outputs[0].Repository)
	prior := s.Reviews["combined"]
	s = applyTest(t, s, Command{Kind: ReviseWork, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "changed upstream"}})
	require.False(t, s.Results["producer"].Valid)
	require.False(t, s.Results["downstream"].Valid)
	require.Equal(t, prior, s.Reviews["combined"])
	require.Error(t, AcceptanceReady(s, "o", "combined"))
}

func TestCancellationPreservesCheckedWork(t *testing.T) {
	s := acceptanceState(t)
	s = applyTest(t, s, reviewCommand(t, s, "combined"))
	c := admitCommand(s, "w", "unfinished")
	c.Leases = []Lease{{Key: "workspace", WorkID: "w", Retain: true}}
	s = applyTest(t, s, c)
	priorResult, priorReview := s.Results["result"], s.Reviews["combined"]
	s = applyTest(t, s, Command{Kind: SetObjectiveStatus, ObjectiveID: "o", Status: ObjectiveCancelled, Reason: "operator cancelled"})
	require.Equal(t, AttemptCancelling, s.Attempts["unfinished"].Status)
	require.True(t, s.Attempts["unfinished"].Reservation.Held)
	require.Equal(t, priorResult, s.Results["result"])
	require.Equal(t, priorReview, s.Reviews["combined"])
	_, err := Apply(s, Command{Kind: ReleaseLease, ExpectedRevision: s.Revision, WorkID: "w", LeaseKey: "workspace", Reason: "explicit release"})
	require.Error(t, err)
	s = stopped(t, s, "w", "unfinished", AttemptCancelled)
	require.Contains(t, s.Leases, "workspace")
	require.Contains(t, s.Work, "w")
	require.Equal(t, priorResult, s.Results["result"])
	s = applyTest(t, s, Command{Kind: ReleaseLease, WorkID: "w", LeaseKey: "workspace", Reason: "explicit retained-work disposition"})
	require.Empty(t, s.Leases)
	require.Equal(t, priorResult, s.Results["result"])
	require.Contains(t, s.Attempts, "unfinished")
}

func TestReviewAuthorityAndPriorPersistence(t *testing.T) {
	for _, mutate := range []func(*Command){
		func(c *Command) { c.OperatorID = "" },
		func(c *Command) { c.Review.ReviewerID = "self-declared" },
		func(c *Command) { c.At = time.Time{} },
		func(c *Command) { c.Review.Evidence = ArtifactRef{} },
		func(c *Command) { c.Review.Binding.CriteriaRevision++ },
	} {
		s := acceptanceState(t)
		c := reviewCommand(t, s, "combined")
		mutate(&c)
		_, err := Apply(s, c)
		require.Error(t, err)
	}
	s := acceptanceState(t)
	r := reviewCommand(t, s, "combined")
	a := acceptCommand(t, s)
	_, err := ApplyBatch(s, s.Revision, []Command{r, a})
	require.Error(t, err, "combined review must be persisted before accepting")
	s = applyTest(t, s, r)
	a = acceptCommand(t, s)
	change := Command{Kind: ReviseWork, ExpectedRevision: s.Revision, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "changed in acceptance transaction"}}
	_, err = ApplyBatch(s, s.Revision, []Command{a, change})
	require.Error(t, err, "acceptance cannot commit with changed outputs in the same transaction")
	s = applyTest(t, s, a)
	require.NoError(t, AcceptanceCurrent(s, "accept"))
	prior := s.Acceptances["accept"]
	s = applyTest(t, s, change)
	require.Equal(t, prior, s.Acceptances["accept"])
	require.ErrorIs(t, AcceptanceCurrent(s, "accept"), ErrStale)
}
