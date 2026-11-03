package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/worksonmyai/kontora/internal/orchestrator"
)

func (c *objectiveController) information(ref orchestrator.ArtifactRef, remaining int) (orchestrator.PlannerInformation, error) {
	information := orchestrator.PlannerInformation{Artifact: ref}
	if ref.Size > 256<<10 || remaining == 0 {
		information.Truncated = true
		return information, nil
	}
	data, err := c.store.ReadArtifact(ref)
	if err != nil {
		return information, err
	}
	if !utf8.Valid(data) {
		information.Binary = true
		return information, nil
	}
	limit := min(8<<10, remaining)
	if len(data) > limit {
		data = data[:limit]
		for !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
		information.Truncated = true
	}
	information.Text = string(data)
	return information, nil
}

func (c *objectiveController) evidence(state orchestrator.State, objectiveID string, now time.Time) (*orchestrator.PlannerEvidence, error) {
	evidence := &orchestrator.PlannerEvidence{}
	for _, attempt := range state.Attempts {
		if attempt.ObjectiveID == objectiveID {
			evidence.Attempts = append(evidence.Attempts, attempt)
		}
	}
	slices.SortFunc(evidence.Attempts, func(a, b orchestrator.Attempt) int { return strings.Compare(a.ID, b.ID) })
	for _, result := range state.Results {
		if state.Work[result.WorkID].ObjectiveID != objectiveID {
			continue
		}
		evidence.Results = append(evidence.Results, orchestrator.PlannerResult{Result: result})
	}
	slices.SortFunc(evidence.Results, func(a, b orchestrator.PlannerResult) int { return strings.Compare(a.ID, b.ID) })
	remaining := 64 << 10
	for i := range evidence.Results {
		entry := &evidence.Results[i]
		if !entry.Valid || state.Work[entry.WorkID].Kind != orchestrator.WorkJob {
			continue
		}
		for _, ref := range entry.Artifacts {
			information, err := c.information(ref, remaining)
			if err != nil {
				return nil, err
			}
			remaining -= len(information.Text)
			entry.Information = append(entry.Information, information)
		}
	}
	for _, question := range state.Questions {
		if question.ObjectiveID == objectiveID {
			evidence.Questions = append(evidence.Questions, question)
		}
	}
	slices.SortFunc(evidence.Questions, func(a, b orchestrator.Question) int { return strings.Compare(a.ID, b.ID) })
	for _, action := range state.Actions {
		if action.ObjectiveID != objectiveID || action.Kind != "planner_event" || action.Status != orchestrator.ActionPending {
			continue
		}
		data, err := c.store.ReadArtifact(action.Payload)
		if err != nil {
			return nil, err
		}
		var event plannerEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, err
		}
		if event.RetryAt.IsZero() || !now.Before(event.RetryAt) {
			evidence.Wakes = append(evidence.Wakes, orchestrator.PlannerWake{ID: action.ID, Name: event.Name})
		}
	}
	slices.SortFunc(evidence.Wakes, func(a, b orchestrator.PlannerWake) int { return strings.Compare(a.ID, b.ID) })
	return evidence, nil
}
