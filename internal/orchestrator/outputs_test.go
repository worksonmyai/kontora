package orchestrator

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var outputBase = strings.Repeat("a", 40)
var outputHead = strings.Repeat("b", 40)

func evidenceArtifact() ArtifactRef { return ArtifactRef{Hash: strings.Repeat("c", 64), Size: 12} }

func withVerificationPolicy(t *testing.T, s State, id string) State {
	t.Helper()
	return applyTest(t, s, Command{Kind: RecordVerificationPolicy, OperatorID: "operator-from-transport", At: testNow, VerificationPolicy: VerificationPolicy{
		ID: id, ObjectiveID: "o", Revision: id,
		Definitions: []CheckDefinition{{ID: "test", Revision: "test-1", ConfigurationRevision: "config-1", Command: []string{"go", "test", "./..."}, Directory: "/checkout", Timeout: time.Minute}},
	}})
}

func checkedCandidate(s State, workID, attemptID, resultID string, generation uint64) Result {
	w := s.Work[workID]
	policy := s.VerificationPolicies[s.Objectives[w.ObjectiveID].VerificationPolicyID]
	def := policy.Definitions[0]
	outputs := []QualifiedOutput{{Repository: "repo", Commit: outputHead}}
	return Result{ID: resultID, WorkID: workID, AttemptID: attemptID, Generation: generation, SpecificationRevision: w.SpecificationRevision, Inputs: w.Inputs, Commit: outputHead, Valid: true,
		Checked: &CheckedOutput{Disposition: OutputCode, VerificationPolicyID: policy.ID, Outputs: outputs, Ranges: []ReviewRange{{Repository: "repo", BaseCommit: outputBase, HeadCommit: outputHead}}},
		Checks:  []Check{{ID: def.ID, DefinitionRevision: def.Revision, ConfigurationRevision: def.ConfigurationRevision, Command: def.Command, Directory: def.Directory, Timeout: def.Timeout, ExitCode: 0, InputsUnchanged: true, Log: evidenceArtifact(), Inputs: w.Inputs, Outputs: outputs}},
	}
}

func verifyCandidate(t *testing.T, s State, r Result) (State, Result) {
	t.Helper()
	check := &r.Checks[0]
	id := r.ID + "-verification-" + string(r.Checked.Disposition)
	s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: id, ObjectiveID: "o", Kind: WorkCheck, Specification: "run frozen verification", Inputs: r.Inputs}})
	c := admitCommand(s, id, id)
	c.Attempt.Recipe.ConfigurationRevision, c.Attempt.Recipe.Timeout = check.ConfigurationRevision, check.Timeout
	c.Attempt.Recipe.Verification = &VerificationExecution{VerificationPolicyID: r.Checked.VerificationPolicyID, DefinitionID: check.ID, DefinitionRevision: check.DefinitionRevision, Command: check.Command, Directory: check.Directory, Outputs: check.Outputs}
	s = applyTest(t, s, c)
	s = applyTest(t, s, Command{Kind: StopAttempt, WorkID: id, AttemptID: id, AttemptStatus: AttemptSucceeded, Receipt: "exact verification process group stopped", At: testNow.Add(time.Second), Logs: []ArtifactRef{check.Log}})
	check.AttemptID = id
	return s, r
}

func prepareCheckedResult(t *testing.T) (State, Result) {
	t.Helper()
	s := withVerificationPolicy(t, testState(t), "policy-1")
	s = applyTest(t, s, admitCommand(s, "w", "attempt"))
	s = stopped(t, s, "w", "attempt", AttemptSucceeded)
	return verifyCandidate(t, s, checkedCandidate(s, "w", "attempt", "result", 1))
}

func TestCheckedOutputEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Result)
		valid  bool
	}{
		{"checked code", func(*Result) {}, true},
		{"explicit empty artifacts", func(r *Result) { r.Artifacts = []ArtifactRef{} }, true},
		{"no explicit contract", func(r *Result) { r.Checked = nil }, false},
		{"unqualified commit", func(r *Result) { r.Checked.Outputs[0].Repository = "" }, false},
		{"outside scope", func(r *Result) { r.Checked.Outputs[0].Repository = "other-repo" }, false},
		{"not exact commit", func(r *Result) { r.Checked.Outputs[0].Commit = "moving-branch" }, false},
		{"scalar differs", func(r *Result) { r.Commit = outputBase }, false},
		{"missing range", func(r *Result) { r.Checked.Ranges = nil }, false},
		{"range wrong repo", func(r *Result) { r.Checked.Ranges[0].Repository = "other" }, false},
		{"range wrong head", func(r *Result) { r.Checked.Ranges[0].HeadCommit = outputBase }, false},
		{"range moving base", func(r *Result) { r.Checked.Ranges[0].BaseCommit = "delivery" }, false},
		{"missing checks", func(r *Result) { r.Checks = nil }, false},
		{"wrong definition", func(r *Result) { r.Checks[0].DefinitionRevision = "old" }, false},
		{"wrong config", func(r *Result) { r.Checks[0].ConfigurationRevision = "old" }, false},
		{"wrong command", func(r *Result) { r.Checks[0].Command = []string{"true"} }, false},
		{"wrong directory", func(r *Result) { r.Checks[0].Directory = "/elsewhere" }, false},
		{"missing directory", func(r *Result) { r.Checks[0].Directory = "" }, false},
		{"wrong timeout", func(r *Result) { r.Checks[0].Timeout++ }, false},
		{"failed check", func(r *Result) { r.Checks[0].ExitCode = 1 }, false},
		{"changed inputs", func(r *Result) { r.Checks[0].InputsUnchanged = false }, false},
		{"different checked vector", func(r *Result) { r.Checks[0].Outputs = []QualifiedOutput{{Repository: "repo", Commit: outputBase}} }, false},
		{"missing retained log", func(r *Result) { r.Checks[0].Log = ArtifactRef{} }, false},
		{"missing verification attempt", func(r *Result) { r.Checks[0].AttemptID = "" }, false},
		{"not a verification job", func(r *Result) { r.Checks[0].AttemptID = r.AttemptID }, false},
		{"implicit unchanged code", func(r *Result) { r.Checked.Ranges[0].BaseCommit = outputHead }, false},
		{"implicit no change", func(r *Result) { r.Checked.Disposition = OutputNoChange }, false},
		{"explicit no change", func(r *Result) {
			r.Checked.Disposition = OutputNoChange
			r.Checked.Reason = "already satisfies assignment"
			r.Checked.Evidence = evidenceArtifact()
			r.Checked.Ranges[0].BaseCommit = outputHead
		}, true},
		{"no change without evidence", func(r *Result) {
			r.Checked.Disposition = OutputNoChange
			r.Checked.Reason = "already done"
			r.Checked.Ranges[0].BaseCommit = outputHead
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := prepareCheckedResult(t)
			tc.mutate(&r)
			before, err := Clone(s)
			require.NoError(t, err)
			n, err := Apply(s, Command{Kind: RecordCheckedResult, ExpectedRevision: s.Revision, Result: r})
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, r, n.Results[r.ID])
				n = applyTest(t, n, Command{Kind: ReviseWork, WorkID: "w", Preconditions: Preconditions{DocumentRevision: 1, SpecificationRevision: 1}, Work: WorkItem{Specification: "new assignment"}})
				require.False(t, n.Results[r.ID].Valid)
				require.Equal(t, r.Checked, n.Results[r.ID].Checked)
				require.Equal(t, r.Checks, n.Results[r.ID].Checks)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, before, s)
		})
	}
}

func TestNoChangeDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Result)
	}{
		{"blank reason", func(r *Result) { r.Checked.Reason = " \t" }},
		{"missing evidence", func(r *Result) { r.Checked.Evidence = ArtifactRef{} }},
		{"output artifacts", func(r *Result) {
			r.Artifacts = []ArtifactRef{evidenceArtifact()}
			r.Checked.Outputs = append(r.Checked.Outputs, QualifiedOutput{Artifact: evidenceArtifact()})
		}},
		{"missing commit", func(r *Result) {
			r.Commit = ""
			r.Artifacts = []ArtifactRef{evidenceArtifact()}
			r.Checked.Outputs = []QualifiedOutput{{Artifact: evidenceArtifact()}}
			r.Checked.Ranges = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := prepareCheckedResult(t)
			r.Checked.Disposition, r.Checked.Reason, r.Checked.Evidence = OutputNoChange, "already done", evidenceArtifact()
			r.Checked.Ranges[0].BaseCommit = outputHead
			tc.mutate(&r)
			_, err := Apply(s, Command{Kind: RecordCheckedResult, ExpectedRevision: s.Revision, Result: r})
			require.EqualError(t, err, "no-change disposition requires at least one retained commit, no output artifacts, a nonblank reason, and valid evidence")
		})
	}
}

func researchCandidate(t *testing.T, s State, r Result) (State, Result) {
	r.Commit = ""
	r.Artifacts = []ArtifactRef{evidenceArtifact()}
	r.Checked.Disposition = OutputResearch
	r.Checked.Outputs = []QualifiedOutput{{Artifact: r.Artifacts[0]}}
	r.Checked.Ranges = nil
	r.Checked.Sources = []Citation{{Source: "https://example.org/source", Evidence: evidenceArtifact()}}
	r.Checked.Review = &OutputReview{ReviewerID: "reviewer", ReviewedAt: testNow, Evidence: evidenceArtifact(), Outputs: r.Checked.Outputs, Inputs: r.Inputs}
	r.Checks[0].Outputs = r.Checked.Outputs
	return verifyCandidate(t, s, r)
}

func TestResearchOutputEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Result)
		valid  bool
	}{
		{"reviewed research", func(*Result) {}, true},
		{"missing review", func(r *Result) { r.Checked.Review = nil }, false},
		{"missing sources", func(r *Result) { r.Checked.Sources = nil }, false},
		{"uncited source", func(r *Result) { r.Checked.Sources[0].Source = "" }, false},
		{"source not retained", func(r *Result) { r.Checked.Sources[0].Evidence = ArtifactRef{} }, false},
		{"unreviewed generation", func(r *Result) { r.Checked.Review.Outputs = nil }, false},
		{"review wrong inputs", func(r *Result) { r.Checked.Review.Inputs = []ResultRef{{ResultID: "unknown"}} }, false},
		{"review findings", func(r *Result) { r.Checked.Review.Findings = []string{"unsupported claim"} }, false},
		{"review no evidence", func(r *Result) { r.Checked.Review.Evidence = ArtifactRef{} }, false},
		{"unretained artifact", func(r *Result) { r.Artifacts = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := prepareCheckedResult(t)
			s, r = researchCandidate(t, s, r)
			tc.mutate(&r)
			n, err := Apply(s, Command{Kind: RecordCheckedResult, ExpectedRevision: s.Revision, Result: r})
			if tc.valid {
				require.NoError(t, err)
				require.Contains(t, ReferencedArtifacts(n), evidenceArtifact())
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCheckedConsumedInputs(t *testing.T) {
	for _, tc := range []struct {
		name          string
		result, check bool
	}{
		{"exact", true, true}, {"missing result input", false, true}, {"missing check input", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := prepareCheckedResult(t)
			s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
			refs := []ResultRef{{ResultID: r.ID, WorkID: r.WorkID, Generation: r.Generation}}
			s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "consumer", ObjectiveID: "o", Kind: WorkCheck, Specification: "verify producer", Inputs: refs}})
			s = applyTest(t, s, admitCommand(s, "consumer", "verify"))
			s = stopped(t, s, "consumer", "verify", AttemptSucceeded)
			s, candidate := verifyCandidate(t, s, checkedCandidate(s, "consumer", "verify", "downstream", 1))
			if !tc.result {
				candidate.Inputs = nil
			}
			if !tc.check {
				candidate.Checks[0].Inputs = nil
			}
			_, err := Apply(s, Command{Kind: RecordCheckedResult, ExpectedRevision: s.Revision, Result: candidate})
			if tc.result && tc.check {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestEmptyCheckedInputVector(t *testing.T) {
	s := withVerificationPolicy(t, testState(t), "policy")
	s = applyTest(t, s, Command{Kind: PutWork, Work: WorkItem{ID: "empty", ObjectiveID: "o", Kind: WorkJob, Specification: "no consumed inputs", Inputs: []ResultRef{}}})
	s = applyTest(t, s, admitCommand(s, "empty", "empty-attempt"))
	s = stopped(t, s, "empty", "empty-attempt", AttemptSucceeded)
	s, r := verifyCandidate(t, s, checkedCandidate(s, "empty", "empty-attempt", "empty-result", 1))
	s = applyTest(t, s, Command{Kind: RecordCheckedResult, Result: r})
	require.NotNil(t, s.Results[r.ID].Inputs)
	require.NotNil(t, s.Results[r.ID].Checks[0].Inputs)
}

func TestVerificationJobAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Command)
	}{
		{"definition revision", func(c *Command) { c.Attempt.Recipe.Verification.DefinitionRevision = "old" }},
		{"configuration revision", func(c *Command) { c.Attempt.Recipe.ConfigurationRevision = "old" }},
		{"command", func(c *Command) { c.Attempt.Recipe.Verification.Command = []string{"true"} }},
		{"directory", func(c *Command) { c.Attempt.Recipe.Verification.Directory = "/other" }},
		{"timeout", func(c *Command) { c.Attempt.Recipe.Timeout++ }},
		{"unqualified subject", func(c *Command) { c.Attempt.Recipe.Verification.Outputs[0].Repository = "" }},
		{"not a check job", func(c *Command) { c.Attempt.WorkID = "w" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := prepareCheckedResult(t)
			check := r.Checks[0]
			c := admitCommand(s, check.AttemptID, "new-verification")
			c.Attempt.Recipe.Verification = &VerificationExecution{VerificationPolicyID: "policy-1", DefinitionID: check.ID, DefinitionRevision: check.DefinitionRevision, Command: check.Command, Directory: check.Directory, Outputs: r.Checked.Outputs}
			tc.mutate(&c)
			_, err := Apply(s, c)
			require.Error(t, err)
		})
	}
}

func TestVerificationPolicyAuthorityAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Command)
	}{
		{"missing trusted identity", func(c *Command) { c.OperatorID = "" }},
		{"identity in payload", func(c *Command) { c.VerificationPolicy.OperatorID = "self-declared" }},
		{"missing time", func(c *Command) { c.At = time.Time{} }},
		{"empty definitions", func(c *Command) { c.VerificationPolicy.Definitions = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := withVerificationPolicy(t, testState(t), "p1")
			p := s.VerificationPolicies["p1"]
			p.ID, p.Revision, p.OperatorID, p.RecordedAt = "p2", "p2", "", time.Time{}
			c := Command{Kind: RecordVerificationPolicy, ExpectedRevision: s.Revision, OperatorID: "operator", At: testNow, VerificationPolicy: p}
			tc.mutate(&c)
			_, err := Apply(s, c)
			require.Error(t, err)
		})
	}
	s := withVerificationPolicy(t, testState(t), "p1")
	prior := s.VerificationPolicies["p1"]
	s = withVerificationPolicy(t, s, "p2")
	require.Equal(t, prior, s.VerificationPolicies["p1"])
	n, err := Clone(s)
	require.NoError(t, err)
	n.Revision++
	p := n.VerificationPolicies["p1"]
	p.Definitions[0].Command[0] = "true"
	n.VerificationPolicies[p.ID] = p
	require.Error(t, ValidateTransition(s, n))
	require.False(t, reflect.DeepEqual(s, n))
}
