package orchestrator

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

// QualifiedOutput never treats a commit in one repository as another repository's base.
// Repository identities are resolved by the caller (normally the Git common directory).
type QualifiedOutput struct {
	Repository string      `json:"repository,omitempty"`
	Commit     string      `json:"commit,omitempty"`
	Artifact   ArtifactRef `json:"artifact,omitzero"`
}

type ReviewRange struct {
	Repository string `json:"repository"`
	BaseCommit string `json:"base_commit"`
	HeadCommit string `json:"head_commit"`
}

type Citation struct {
	Source   string      `json:"source"`
	Evidence ArtifactRef `json:"evidence"`
}

type OutputReview struct {
	ReviewerID string            `json:"reviewer_id"`
	ReviewedAt time.Time         `json:"reviewed_at"`
	Outputs    []QualifiedOutput `json:"outputs"`
	Inputs     []ResultRef       `json:"inputs"`
	Ranges     []ReviewRange     `json:"ranges"`
	Evidence   ArtifactRef       `json:"evidence"`
	Findings   []string          `json:"findings"`
}

type OutputDisposition string

const (
	OutputCode     OutputDisposition = "code"
	OutputResearch OutputDisposition = "research"
	OutputNoChange OutputDisposition = "no_change"
)

// CheckedOutput is opt-in. Legacy Result records retain their original contract and
// cannot support checked acceptance. Evidence is observational, not permission to commit.
type CheckedOutput struct {
	Disposition          OutputDisposition `json:"disposition"`
	VerificationPolicyID string            `json:"verification_policy_id"`
	Outputs              []QualifiedOutput `json:"outputs"`
	Ranges               []ReviewRange     `json:"ranges"`
	Reason               string            `json:"reason,omitempty"`
	Evidence             ArtifactRef       `json:"evidence,omitzero"`
	Sources              []Citation        `json:"sources,omitempty"`
	Review               *OutputReview     `json:"review,omitempty"`
}

// Verification evidence must come from an admitted WorkCheck attempt.
type VerificationExecution struct {
	VerificationPolicyID string            `json:"verification_policy_id"`
	DefinitionID         string            `json:"definition_id"`
	DefinitionRevision   string            `json:"definition_revision"`
	Command              []string          `json:"command"`
	Directory            string            `json:"directory"`
	Outputs              []QualifiedOutput `json:"outputs"`
}

type CheckDefinition struct {
	ID                    string        `json:"id"`
	Revision              string        `json:"revision"`
	ConfigurationRevision string        `json:"configuration_revision"`
	Command               []string      `json:"command"`
	Directory             string        `json:"directory"`
	Timeout               time.Duration `json:"timeout"`
}

// VerificationPolicy versions are immutable. Only a trusted operator command selects
// a new version; worker/planner proposals cannot supply operator authority.
type VerificationPolicy struct {
	ID          string            `json:"id"`
	ObjectiveID string            `json:"objective_id"`
	Revision    string            `json:"revision"`
	Definitions []CheckDefinition `json:"definitions"`
	OperatorID  string            `json:"operator_id"`
	RecordedAt  time.Time         `json:"recorded_at"`
}

func validArtifact(ref ArtifactRef) bool {
	if len(ref.Hash) != 64 || ref.Hash != strings.ToLower(ref.Hash) || ref.Size < 0 {
		return false
	}
	_, err := hex.DecodeString(ref.Hash)
	return err == nil
}

func validCommit(commit string) bool {
	if (len(commit) != 40 && len(commit) != 64) || commit != strings.ToLower(commit) || strings.Trim(commit, "0") == "" {
		return false
	}
	_, err := hex.DecodeString(commit)
	return err == nil
}

func validateQualifiedOutputs(outputs []QualifiedOutput, ranges []ReviewRange, repositories []string) error {
	if len(outputs) == 0 {
		return fmt.Errorf("checked output is empty")
	}
	seen := map[QualifiedOutput]bool{}
	code := map[string]string{}
	for _, output := range outputs {
		if seen[output] {
			return fmt.Errorf("duplicate qualified output")
		}
		seen[output] = true
		if output.Repository != "" && !contains(repositories, output.Repository) {
			return fmt.Errorf("output outside repository scope")
		}
		if output.Commit != "" {
			if output.Repository == "" || !validCommit(output.Commit) || output.Artifact != (ArtifactRef{}) || code[output.Repository] != "" {
				return fmt.Errorf("invalid qualified commit")
			}
			code[output.Repository] = output.Commit
		} else if !validArtifact(output.Artifact) {
			return fmt.Errorf("invalid qualified artifact")
		}
	}
	seenRanges := map[string]bool{}
	for _, r := range ranges {
		if seenRanges[r.Repository] || code[r.Repository] != r.HeadCommit || !validCommit(r.BaseCommit) || len(r.BaseCommit) != len(r.HeadCommit) {
			return fmt.Errorf("review range does not match retained output")
		}
		seenRanges[r.Repository] = true
	}
	if len(seenRanges) != len(code) {
		return fmt.Errorf("missing pinned review range")
	}
	return nil
}

func validateCheckedOutput(s State, r Result) error {
	x := r.Checked
	if x == nil {
		return fmt.Errorf("checked result requires explicit evidence")
	}
	w := s.Work[r.WorkID]
	for _, ref := range r.Inputs {
		if err := checkRef(s, ref); err != nil {
			return err
		}
		if s.Results[ref.ResultID].Checked == nil {
			return fmt.Errorf("checked outputs require qualified checked inputs")
		}
	}
	policy, ok := s.VerificationPolicies[x.VerificationPolicyID]
	if !ok || policy.ObjectiveID != w.ObjectiveID {
		return fmt.Errorf("missing verification policy")
	}
	if err := validateQualifiedOutputs(x.Outputs, x.Ranges, s.Objectives[w.ObjectiveID].Policy.Repositories); err != nil {
		return err
	}
	commits, artifacts, err := retainedOutputKinds(r)
	if err != nil {
		return err
	}
	if err := validateOutputDisposition(*x, commits, artifacts); err != nil {
		return err
	}
	if err := validateOutputReview(r); err != nil {
		return err
	}
	return validateCheckEvidence(s, r, policy)
}

func retainedOutputKinds(r Result) (int, int, error) {
	var artifacts []ArtifactRef
	var commits []QualifiedOutput
	for _, output := range r.Checked.Outputs {
		if output.Commit != "" {
			commits = append(commits, output)
		} else {
			artifacts = append(artifacts, output.Artifact)
		}
	}
	if !slices.Equal(artifacts, r.Artifacts) {
		return 0, 0, fmt.Errorf("retained artifacts differ from qualified outputs")
	}
	// The legacy scalar commit is only a projection of a single-repository output.
	if (len(commits) == 1 && r.Commit != commits[0].Commit) || (len(commits) != 1 && r.Commit != "") {
		return 0, 0, fmt.Errorf("retained commit differs from qualified outputs")
	}
	return len(commits), len(artifacts), nil
}

func validateOutputDisposition(x CheckedOutput, commits, artifacts int) error {
	switch x.Disposition {
	case OutputCode:
		changed := false
		for _, rg := range x.Ranges {
			changed = changed || rg.BaseCommit != rg.HeadCommit
		}
		if commits == 0 || !changed {
			return fmt.Errorf("code result requires changes or an explicit no-change disposition")
		}
	case OutputNoChange:
		if commits == 0 || artifacts != 0 || strings.TrimSpace(x.Reason) == "" || !validArtifact(x.Evidence) {
			return fmt.Errorf("no-change disposition requires at least one retained commit, no output artifacts, a nonblank reason, and valid evidence")
		}
		for _, rg := range x.Ranges {
			if rg.BaseCommit != rg.HeadCommit {
				return fmt.Errorf("no-change range changed")
			}
		}
	case OutputResearch:
		if artifacts == 0 || commits != 0 || len(x.Sources) == 0 || x.Review == nil {
			return fmt.Errorf("research requires artifacts, cited sources and review")
		}
		for _, source := range x.Sources {
			if strings.TrimSpace(source.Source) == "" || !validArtifact(source.Evidence) {
				return fmt.Errorf("citation requires retained source evidence")
			}
		}
	default:
		return fmt.Errorf("unknown output disposition")
	}
	return nil
}

func validateOutputReview(r Result) error {
	x := r.Checked
	if x.Review != nil {
		review := x.Review
		if review.ReviewerID == "" || review.ReviewedAt.IsZero() || !validArtifact(review.Evidence) || len(review.Findings) != 0 || !reflect.DeepEqual(review.Outputs, x.Outputs) || !reflect.DeepEqual(review.Inputs, r.Inputs) || !reflect.DeepEqual(review.Ranges, x.Ranges) {
			return fmt.Errorf("output review is missing, stale or has unresolved findings")
		}
	}
	return nil
}

func validateCheckEvidence(s State, r Result, policy VerificationPolicy) error {
	x := r.Checked
	if len(r.Checks) != len(policy.Definitions) {
		return fmt.Errorf("missing required checks")
	}
	checks := map[string]Check{}
	for _, check := range r.Checks {
		if _, duplicate := checks[check.ID]; duplicate {
			return fmt.Errorf("duplicate check evidence")
		}
		checks[check.ID] = check
	}
	for _, def := range policy.Definitions {
		check, ok := checks[def.ID]
		if !ok || !matchesVerificationAttempt(s, check, x.VerificationPolicyID) || check.DefinitionRevision != def.Revision || check.ConfigurationRevision != def.ConfigurationRevision || !reflect.DeepEqual(check.Command, def.Command) || check.Directory != def.Directory || check.Timeout != def.Timeout || check.ExitCode != 0 || !check.InputsUnchanged || !validArtifact(check.Log) || !reflect.DeepEqual(check.Inputs, r.Inputs) || !reflect.DeepEqual(check.Outputs, x.Outputs) {
			return fmt.Errorf("failed, stale or incomplete check %s", def.ID)
		}
	}
	return nil
}

func validateVerificationRecipe(s State, a Attempt) error {
	v := a.Recipe.Verification
	if v == nil {
		return nil
	}
	p, ok := s.VerificationPolicies[v.VerificationPolicyID]
	if !ok || p.ObjectiveID != a.ObjectiveID || s.Work[a.WorkID].Kind != WorkCheck {
		return fmt.Errorf("verification requires an owned check job")
	}
	var ranges []ReviewRange
	for _, output := range v.Outputs {
		if output.Commit != "" {
			ranges = append(ranges, ReviewRange{Repository: output.Repository, BaseCommit: output.Commit, HeadCommit: output.Commit})
		}
	}
	if err := validateQualifiedOutputs(v.Outputs, ranges, s.Objectives[a.ObjectiveID].Policy.Repositories); err != nil {
		return err
	}
	for _, def := range p.Definitions {
		if def.ID == v.DefinitionID && def.Revision == v.DefinitionRevision && def.ConfigurationRevision == a.Recipe.ConfigurationRevision && reflect.DeepEqual(def.Command, v.Command) && def.Directory == v.Directory && def.Timeout == a.Recipe.Timeout {
			return nil
		}
	}
	return fmt.Errorf("verification invocation does not match frozen check definition")
}

func matchesVerificationAttempt(s State, check Check, policyID string) bool {
	a, ok := s.Attempts[check.AttemptID]
	v := a.Recipe.Verification
	return ok && a.Status == AttemptSucceeded && !a.Reservation.Held && v != nil &&
		v.VerificationPolicyID == policyID && v.DefinitionID == check.ID && v.DefinitionRevision == check.DefinitionRevision &&
		a.Recipe.ConfigurationRevision == check.ConfigurationRevision && a.Recipe.Timeout == check.Timeout &&
		reflect.DeepEqual(v.Command, check.Command) && v.Directory == check.Directory &&
		reflect.DeepEqual(a.Inputs, check.Inputs) && reflect.DeepEqual(v.Outputs, check.Outputs) && slices.Contains(a.Logs, check.Log)
}

func hasRetainedOutput(r Result) bool {
	return r.Commit != "" || len(r.Artifacts) != 0 || (r.Checked != nil && len(r.Checked.Outputs) != 0)
}

func recordCheckedResult(s *State, c Command) error {
	r := c.Result
	w := s.Work[r.WorkID]
	if r.Checked == nil || r.Checked.VerificationPolicyID != s.Objectives[w.ObjectiveID].VerificationPolicyID {
		return ErrStale
	}
	if err := validateCheckedOutput(*s, r); err != nil {
		return err
	}
	return recordResult(s, c)
}

func recordVerificationPolicy(s *State, c Command) error {
	p := c.VerificationPolicy
	o, ok := s.Objectives[p.ObjectiveID]
	if !ok || o.Status == ObjectiveCancelled || o.Status == ObjectiveAccepted || p.ID == "" || s.VerificationPolicies[p.ID].ID != "" || c.OperatorID == "" || c.At.IsZero() || p.OperatorID != "" || !p.RecordedAt.IsZero() {
		return ErrConflict
	}
	for _, prior := range s.VerificationPolicies {
		if prior.ObjectiveID == p.ObjectiveID && prior.Revision == p.Revision {
			return ErrConflict
		}
	}
	p.OperatorID, p.RecordedAt = c.OperatorID, c.At
	if err := validateVerificationPolicy(p); err != nil {
		return err
	}
	if s.VerificationPolicies == nil {
		s.VerificationPolicies = map[string]VerificationPolicy{}
	}
	s.VerificationPolicies[p.ID] = p
	o.VerificationPolicyID, o.DocumentRevision = p.ID, o.DocumentRevision+1
	s.Objectives[o.ID] = o
	return nil
}

func validateVerificationPolicy(p VerificationPolicy) error {
	if p.ID == "" || p.ObjectiveID == "" || p.Revision == "" || p.OperatorID == "" || p.RecordedAt.IsZero() || len(p.Definitions) == 0 {
		return fmt.Errorf("incomplete verification policy")
	}
	seen := map[string]bool{}
	for _, def := range p.Definitions {
		if def.ID == "" || seen[def.ID] || def.Revision == "" || def.ConfigurationRevision == "" || len(def.Command) == 0 || strings.TrimSpace(def.Command[0]) == "" || !filepath.IsAbs(def.Directory) || def.Timeout <= 0 {
			return fmt.Errorf("incomplete check definition")
		}
		seen[def.ID] = true
	}
	return nil
}
