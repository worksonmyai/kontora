package orchestrator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
)

const DecisionVersion = 1
const MaxDecisionBytes = 1 << 20

// PlannerSnapshot contains authority, not credentials. Its digest binds the whole proposal to one view.
type PlannerSnapshot struct {
	ID               string                 `json:"id"`
	TurnID           string                 `json:"turn_id"`
	ObjectiveID      string                 `json:"objective_id"`
	Assignment       string                 `json:"assignment"`
	Criteria         []string               `json:"criteria"`
	CriteriaRevision uint64                 `json:"criteria_revision"`
	PolicyRevision   uint64                 `json:"policy_revision"`
	Budget           Budget                 `json:"budget"`
	Usage            Usage                  `json:"usage"`
	Revision         uint64                 `json:"revision"`
	Repositories     []string               `json:"repositories"`
	RequiredResults  []string               `json:"required_results"`
	Work             map[string]PlannerWork `json:"work"`
	Profiles         []string               `json:"profiles"`
	RemainingActions int                    `json:"remaining_actions"`
	Evidence         *PlannerEvidence       `json:"evidence,omitempty"`
}

type PlannerWork struct {
	ID            string   `json:"id"`
	Repository    string   `json:"repository"`
	Specification string   `json:"specification"`
	Dependencies  []string `json:"dependencies"`
}

type Decision struct {
	Version    int        `json:"version"`
	TurnID     string     `json:"turn_id"`
	SnapshotID string     `json:"snapshot_id"`
	Actions    []Proposal `json:"actions"`
}

// Proposal has no shell, authority, criteria, approval, or acceptance mutation.
type Proposal struct {
	ID          string        `json:"id"`
	Kind        string        `json:"kind"`
	Plan        *PlanProposal `json:"plan,omitempty"`
	Work        *PlannerWork  `json:"work,omitempty"`
	Target      string        `json:"target,omitempty"`
	Replacement string        `json:"replacement,omitempty"`
	Profile     string        `json:"profile,omitempty"`
	Message     string        `json:"message,omitempty"`
	Wait        *PlannerWait  `json:"wait,omitempty"`
}

type PlanProposal struct {
	Rationale       string            `json:"rationale"`
	Work            []PlannerWork     `json:"work"`
	RequiredResults []string          `json:"required_results"`
	Replaced        map[string]string `json:"replaced,omitempty"`
}

type PlannerWait struct {
	Event    string    `json:"event,omitempty"`
	Deadline time.Time `json:"deadline,omitzero"`
}

func FreezePlannerSnapshot(snapshot PlannerSnapshot) (PlannerSnapshot, error) {
	snapshot.ID = ""
	data, err := json.Marshal(snapshot)
	if err != nil {
		return PlannerSnapshot{}, err
	}
	var detached PlannerSnapshot
	if err := json.Unmarshal(data, &detached); err != nil {
		return PlannerSnapshot{}, err
	}
	digest := sha256.Sum256(data)
	detached.ID = hex.EncodeToString(digest[:])
	return detached, nil
}

func DecodeDecision(data []byte, snapshot PlannerSnapshot, now time.Time) (Decision, error) {
	if len(data) > MaxDecisionBytes {
		return Decision{}, errors.New("planner decision exceeds size limit")
	}
	if err := rejectDuplicateFields(data); err != nil {
		return Decision{}, err
	}
	if err := exactDecisionFields(data, reflect.TypeFor[Decision]()); err != nil {
		return Decision{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decision Decision
	if err := decoder.Decode(&decision); err != nil {
		return Decision{}, fmt.Errorf("decode decision: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Decision{}, errors.New("trailing planner output")
	}
	if err := ValidateDecision(decision, snapshot, now); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func ValidateDecision(decision Decision, snapshot PlannerSnapshot, now time.Time) error {
	frozen, err := FreezePlannerSnapshot(snapshot)
	if err != nil || frozen.ID != snapshot.ID || snapshot.ObjectiveID == "" || snapshot.TurnID == "" {
		return errors.New("invalid planner snapshot")
	}
	if decision.Version != DecisionVersion || decision.TurnID != snapshot.TurnID || decision.SnapshotID != snapshot.ID {
		return errors.New("stale or unsupported planner decision")
	}
	if len(decision.Actions) == 0 || len(decision.Actions) > snapshot.RemainingActions {
		return errors.New("empty decision or action budget exceeded")
	}
	seen := map[string]bool{}
	work := make(map[string]PlannerWork, len(snapshot.Work))
	maps.Copy(work, snapshot.Work)
	required := slices.Clone(snapshot.RequiredResults)
	replaced := map[string]bool{}
	scheduled := map[string]bool{}
	var attempts uint64
	for _, action := range decision.Actions {
		if action.ID == "" || seen[action.ID] {
			return errors.New("missing or duplicate action identity")
		}
		seen[action.ID] = true
		if err := validateProposalShape(action); err != nil {
			return err
		}
		if action.Kind == "revise_plan" {
			next, err := validatePlanProposal(*action.Plan, work, required, snapshot.Repositories)
			if err != nil {
				return err
			}
			for id := range action.Plan.Replaced {
				replaced[id] = true
			}
			work, required = next, slices.Clone(action.Plan.RequiredResults)
		} else {
			if err := validatePlannerAction(action, snapshot, work, now); err != nil {
				return err
			}
			if action.Kind == "replace_work" {
				replaced[action.Target] = true
				for i, id := range required {
					if id == action.Target {
						required[i] = action.Replacement
					}
				}
				required = uniqueWorkIDs(required)
			}
			if action.Kind == "schedule" {
				attempts++
				scheduled[action.Target] = true
				if err := validateProposalBudget(snapshot, attempts); err != nil {
					return err
				}
			}
		}
		for id := range replaced {
			if _, present := work[id]; present || scheduled[id] {
				return errors.New("proposal restores or schedules replaced work")
			}
		}
	}
	return validatePlannerGraph(work, required)
}

func validateProposalBudget(snapshot PlannerSnapshot, attempts uint64) error {
	b, u := snapshot.Budget, snapshot.Usage
	if u.Attempts >= b.MaxAttempts || attempts > b.MaxAttempts-u.Attempts || b.MaxRuntime <= 0 || u.Runtime >= b.MaxRuntime {
		return ErrBudget
	}
	if b.MaxCostMicros > 0 && (u.UnknownCost || u.CostMicros >= b.MaxCostMicros) {
		return ErrBudget
	}
	return nil
}

func uniqueWorkIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func validateProposalShape(action Proposal) error {
	allowed := map[string]bool{"id": true, "kind": true}
	switch action.Kind {
	case "revise_plan":
		allowed["plan"] = true
		if action.Plan == nil {
			return errors.New("missing plan")
		}
	case "create_work", "update_work":
		allowed["work"] = true
		if action.Work == nil {
			return errors.New("missing work")
		}
	case "replace_work":
		allowed["target"], allowed["replacement"] = true, true
	case "schedule":
		allowed["target"], allowed["profile"] = true, true
	case "request_information", "request_approval":
		allowed["message"], allowed["target"] = true, true
	case "propose_acceptance":
		allowed["message"] = true
	case "wait":
		allowed["wait"] = true
		if action.Wait == nil {
			return errors.New("missing wait")
		}
	default:
		return fmt.Errorf("unauthorized planner operation %q", action.Kind)
	}
	data, _ := json.Marshal(action)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("field %q conflicts with operation %q", name, action.Kind)
		}
	}
	return nil
}

func validatePlannerWork(work PlannerWork, repositories []string) error {
	if work.ID == "" || work.Specification == "" {
		return errors.New("work requires identity and specification")
	}
	if work.Repository != "" && !slices.Contains(repositories, work.Repository) {
		return errors.New("work expands repository authority")
	}
	return nil
}

func validatePlannerGraph(work map[string]PlannerWork, required []string) error {
	colors := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		item, ok := work[id]
		if !ok {
			return fmt.Errorf("unknown work reference %q", id)
		}
		if colors[id] == 1 {
			return errors.New("work graph has a cycle")
		}
		if colors[id] == 2 {
			return nil
		}
		colors[id] = 1
		deps := map[string]bool{}
		for _, dep := range item.Dependencies {
			if deps[dep] {
				return errors.New("duplicate dependency")
			}
			deps[dep] = true
			if err := visit(dep); err != nil {
				return err
			}
		}
		colors[id] = 2
		return nil
	}
	for id, item := range work {
		if id != item.ID {
			return errors.New("work identity mismatch")
		}
		if err := visit(id); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, id := range required {
		if seen[id] {
			return errors.New("duplicate required result")
		}
		seen[id] = true
		if _, ok := work[id]; !ok {
			return errors.New("required result references unknown work")
		}
	}
	return nil
}

func exactDecisionFields(data []byte, typ reflect.Type) error {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[time.Time]() {
		return nil
	}
	switch {
	case typ.Kind() == reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			return err
		}
		fields := map[string]reflect.Type{}
		for field := range typ.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			fields[name] = field.Type
		}
		for name, raw := range object {
			fieldType, exists := fields[name]
			if !exists {
				return fmt.Errorf("unknown decision field %q", name)
			}
			if err := exactDecisionFields(raw, fieldType); err != nil {
				return err
			}
		}
	case typ.Kind() == reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		for _, raw := range items {
			if err := exactDecisionFields(raw, typ.Elem()); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	return nil
}

func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("decision JSON nesting limit exceeded")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON field")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return fmt.Errorf("invalid decision JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing planner JSON")
	}
	return nil
}
