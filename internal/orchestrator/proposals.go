package orchestrator

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

func validatePlanProposal(plan PlanProposal, work map[string]PlannerWork, required, repositories []string) (map[string]PlannerWork, error) {
	if plan.Rationale == "" {
		return nil, errors.New("plan revision requires rationale")
	}
	next := map[string]PlannerWork{}
	for _, item := range plan.Work {
		if _, exists := next[item.ID]; exists {
			return nil, errors.New("duplicate work identity")
		}
		if err := validatePlannerWork(item, repositories); err != nil {
			return nil, err
		}
		next[item.ID] = item
	}
	for old := range work {
		if _, retained := next[old]; retained {
			continue
		}
		replacement := plan.Replaced[old]
		if _, exists := next[replacement]; !exists || replacement == old {
			return nil, errors.New("plan silently drops existing work")
		}
	}
	for old, replacement := range plan.Replaced {
		if _, retained := next[old]; retained {
			return nil, errors.New("plan retains replaced work")
		}
		if _, exists := work[old]; !exists {
			return nil, errors.New("replacement names unknown work")
		}
		if _, exists := next[replacement]; !exists || old == replacement {
			return nil, errors.New("invalid replacement")
		}
	}
	for _, id := range required {
		if replacement, replaced := plan.Replaced[id]; replaced {
			id = replacement
		}
		if !slices.Contains(plan.RequiredResults, id) {
			return nil, errors.New("plan removes required result")
		}
	}
	return next, nil
}

func validatePlannerAction(action Proposal, snapshot PlannerSnapshot, work map[string]PlannerWork, now time.Time) error {
	switch action.Kind {
	case "create_work", "update_work":
		_, exists := work[action.Work.ID]
		if exists != (action.Kind == "update_work") {
			return errors.New("work creation or update has invalid ownership")
		}
		if err := validatePlannerWork(*action.Work, snapshot.Repositories); err != nil {
			return err
		}
		work[action.Work.ID] = *action.Work
	case "replace_work":
		if _, exists := work[action.Target]; !exists {
			return errors.New("replacement target is not owned")
		}
		if _, exists := work[action.Replacement]; !exists || action.Target == action.Replacement {
			return errors.New("unknown replacement work")
		}
		delete(work, action.Target)
	case "schedule":
		if _, exists := work[action.Target]; !exists {
			return errors.New("schedule target is not owned")
		}
		if !slices.Contains(snapshot.Profiles, action.Profile) {
			return errors.New("unauthorized execution profile")
		}
	case "request_information", "request_approval":
		if action.Message == "" {
			return errors.New("question requires a message")
		}
		if action.Target != "" {
			if _, exists := work[action.Target]; !exists {
				return errors.New("question target is not owned")
			}
		}
	case "propose_acceptance":
		if action.Message == "" {
			return errors.New("acceptance proposal requires evidence")
		}
	case "wait":
		if action.Wait.Event == "" && action.Wait.Deadline.IsZero() {
			return errors.New("wait requires an event or deadline")
		}
		if !action.Wait.Deadline.IsZero() && !action.Wait.Deadline.After(now) {
			return errors.New("wait deadline must be in the future")
		}
	default:
		return fmt.Errorf("unauthorized planner operation %q", action.Kind)
	}
	return nil
}
