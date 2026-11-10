package web

import "github.com/worksonmyai/kontora/internal/automation"

// AutomationService is optional. Implementations own durable revisions, create
// request receipts, and admission; HTTP handlers do not start agents.
type AutomationService interface {
	ListAutomations() ([]automation.Definition, error)
	GetAutomation(id string) (automation.Definition, error)
	CreateAutomation(requestID string, input automation.Definition) (automation.Definition, error)
	UpdateAutomation(input automation.Definition) (automation.Definition, error)
	AutomationOccurrences(id string) ([]automation.Occurrence, error)
	SkipAutomationOccurrence(id, occurrenceID string, revision uint64) (automation.Occurrence, error)
}

type AutomationsResponse struct {
	Available   bool                    `json:"available"`
	Error       string                  `json:"error,omitempty"`
	Definitions []automation.Definition `json:"definitions"`
}

type AutomationOccurrencesResponse struct {
	Occurrences []automation.Occurrence `json:"occurrences"`
	Total       int                     `json:"total"`
}
