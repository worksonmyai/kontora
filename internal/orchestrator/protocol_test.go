package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDecodeDecision(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	snapshot, err := FreezePlannerSnapshot(PlannerSnapshot{
		TurnID: "turn-1", ObjectiveID: "objective-1", Revision: 1, Repositories: []string{"repo"},
		Budget:          Budget{MaxAttempts: 10, MaxRuntime: time.Hour},
		Work:            map[string]PlannerWork{"work-1": {ID: "work-1", Repository: "repo", Specification: "implement"}},
		RequiredResults: []string{"work-1"}, Profiles: []string{"check"}, RemainingActions: 5,
	})
	require.NoError(t, err)
	decision := Decision{Version: 1, TurnID: snapshot.TurnID, SnapshotID: snapshot.ID, Actions: []Proposal{{ID: "action-1", Kind: "schedule", Target: "work-1", Profile: "check"}}}
	data, err := json.Marshal(decision)
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		mutate func(string) string
		valid  bool
	}{
		{name: "valid", mutate: func(s string) string { return s }, valid: true},
		{name: "stale turn", mutate: func(s string) string { return strings.Replace(s, "turn-1", "turn-0", 1) }},
		{name: "stale snapshot", mutate: func(s string) string { return strings.Replace(s, snapshot.ID, "old", 1) }},
		{name: "duplicate envelope field", mutate: func(s string) string { return strings.Replace(s, `"version":1`, `"version":1,"version":1`, 1) }},
		{name: "duplicate nested field", mutate: func(s string) string {
			return strings.Replace(s, `"kind":"schedule"`, `"kind":"schedule","kind":"schedule"`, 1)
		}},
		{name: "unknown field", mutate: func(s string) string { return strings.Replace(s, `"version":1`, `"version":1,"operator":true`, 1) }},
		{name: "case alias", mutate: func(s string) string { return strings.Replace(s, `"version":1`, `"Version":1`, 1) }},
		{name: "case alias nested", mutate: func(s string) string { return strings.Replace(s, `"kind":"schedule"`, `"Kind":"schedule"`, 1) }},
		{name: "deep nesting", mutate: func(string) string { return strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) }},
		{name: "oversized", mutate: func(string) string { return strings.Repeat(" ", MaxDecisionBytes+1) }},

		{name: "unauthorized target", mutate: func(s string) string { return strings.Replace(s, "work-1", "other", 1) }},
		{name: "unauthorized profile", mutate: func(s string) string { return strings.Replace(s, "check", "shell", 1) }},
		{name: "accept own work", mutate: func(s string) string { return strings.Replace(s, "schedule", "accept", 1) }},
		{name: "trailing JSON", mutate: func(s string) string { return s + `{}` }},
		{name: "truncated", mutate: func(s string) string { return s[:len(s)-1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeDecision([]byte(tc.mutate(string(data))), snapshot, now)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestValidateDecision(t *testing.T) {
	now := time.Now()
	base := PlannerWork{ID: "a", Repository: "repo", Specification: "original requirement"}
	for _, tc := range []struct {
		name    string
		actions []Proposal
		budget  int
		valid   bool
	}{
		{name: "prerequisite preserves requirements", budget: 2, valid: true, actions: []Proposal{{ID: "1", Kind: "revise_plan", Plan: &PlanProposal{Rationale: "required first", RequiredResults: []string{"a"}, Work: []PlannerWork{{ID: "a", Repository: "repo", Specification: base.Specification, Dependencies: []string{"b"}}, {ID: "b", Repository: "repo", Specification: "prerequisite"}}}}}},
		{name: "explicit required replacement", budget: 2, valid: true, actions: []Proposal{{ID: "1", Kind: "revise_plan", Plan: &PlanProposal{Rationale: "replace", Work: []PlannerWork{{ID: "b", Specification: "replacement"}}, Replaced: map[string]string{"a": "b"}, RequiredResults: []string{"b"}}}}},
		{name: "retained and replaced", budget: 2, actions: []Proposal{{ID: "1", Kind: "revise_plan", Plan: &PlanProposal{Rationale: "replace", Work: []PlannerWork{base, {ID: "b", Specification: "replacement"}}, Replaced: map[string]string{"a": "b"}, RequiredResults: []string{"a"}}}}},
		{name: "replacement loses requirement", budget: 2, actions: []Proposal{{ID: "1", Kind: "revise_plan", Plan: &PlanProposal{Rationale: "replace", Work: []PlannerWork{{ID: "b", Specification: "replacement"}}, Replaced: map[string]string{"a": "b"}}}}},
		{name: "dropped requirement", budget: 2, actions: []Proposal{{ID: "1", Kind: "revise_plan", Plan: &PlanProposal{Rationale: "drop", Work: []PlannerWork{base}}}}},
		{name: "cycle", budget: 2, actions: []Proposal{{ID: "1", Kind: "update_work", Work: &PlannerWork{ID: "a", Specification: "cycle", Dependencies: []string{"a"}}}}},
		{name: "expanded authority", budget: 2, actions: []Proposal{{ID: "1", Kind: "update_work", Work: &PlannerWork{ID: "a", Repository: "other", Specification: "escape"}}}},
		{name: "mixed payloads", budget: 2, actions: []Proposal{{ID: "1", Kind: "schedule", Target: "a", Profile: "check", Work: &base}}},
		{name: "partial valid proposal", budget: 2, actions: []Proposal{{ID: "1", Kind: "schedule", Target: "a", Profile: "check"}, {ID: "2", Kind: "schedule", Target: "other", Profile: "check"}}},
		{name: "budget", budget: 0, actions: []Proposal{{ID: "1", Kind: "schedule", Target: "a", Profile: "check"}}},
		{name: "duplicate action", budget: 2, actions: []Proposal{{ID: "1", Kind: "schedule", Target: "a", Profile: "check"}, {ID: "1", Kind: "schedule", Target: "a", Profile: "check"}}},
		{name: "wait", budget: 2, valid: true, actions: []Proposal{{ID: "1", Kind: "wait", Wait: &PlannerWait{Event: "feedback"}}}},
		{name: "past wait", budget: 2, actions: []Proposal{{ID: "1", Kind: "wait", Wait: &PlannerWait{Deadline: now.Add(-time.Second)}}}},
		{name: "empty wait", budget: 2, actions: []Proposal{{ID: "1", Kind: "wait", Wait: &PlannerWait{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := FreezePlannerSnapshot(PlannerSnapshot{TurnID: "turn", ObjectiveID: "objective", Work: map[string]PlannerWork{"a": base}, RequiredResults: []string{"a"}, Repositories: []string{"repo"}, Profiles: []string{"check"}, RemainingActions: tc.budget, Budget: Budget{MaxAttempts: 10, MaxRuntime: time.Hour}})
			require.NoError(t, err)
			err = ValidateDecision(Decision{Version: 1, TurnID: "turn", SnapshotID: snapshot.ID, Actions: tc.actions}, snapshot, now)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestPlannerSnapshotDetached(t *testing.T) {
	original := PlannerSnapshot{TurnID: "turn", ObjectiveID: "objective", Repositories: []string{"repo"}, Work: map[string]PlannerWork{"a": {ID: "a", Specification: "spec", Dependencies: []string{"b"}}}}
	frozen, err := FreezePlannerSnapshot(original)
	require.NoError(t, err)
	original.Repositories[0] = "other"
	original.Work["a"].Dependencies[0] = "other"
	require.Equal(t, "repo", frozen.Repositories[0])
	require.Equal(t, "b", frozen.Work["a"].Dependencies[0])
}
